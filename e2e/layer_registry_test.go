//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasremote "oras.land/oras-go/v2/registry/remote"

	"github.com/giantswarm/vm-manager/internal/registry"
	"github.com/giantswarm/vm-manager/pkg/layersign"
)

// The registry production nodes pull from, and what TestKubernetesCluster
// pulls from it: kubeadm's images (ClusterConfiguration.imageRepository), the
// sandbox the sysext's containerd.toml pins, and Cilium from the chart
// Giant Swarm deploys (giantswarm/cilium-app).
const (
	gsoci              = "gsoci.azurecr.io"
	gsociRepository    = gsoci + "/giantswarm"
	sandboxImage       = gsociRepository + "/pause:3.10.2"
	ciliumChart        = "oci://" + gsoci + "/charts/giantswarm/cilium"
	ciliumChartVersion = "1.9.0"
)

// kubeadmImages are the images `kubeadm config images list` names besides
// the control plane's own (kube-apiserver, kube-controller-manager,
// kube-scheduler and kube-proxy at the Kubernetes version), per Kubernetes
// minor the image ships (images/Makefile, KUBERNETES_VERSIONS).
var kubeadmImages = map[string][]string{
	"1.36": {"coredns:v1.14.2", "etcd:3.6.8-0", "pause:3.10.2"},
	"1.35": {"coredns:v1.13.1", "etcd:3.6.6-0", "pause:3.10.1"},
}

// ciliumValues are the cilium-app values of these nodes (images/README.md,
// Cilium): the sysext ships cilium-cni, cgroup2 and the sysctl drop-in, and
// kube-proxy stays. The pod CIDR is the test's; tags instead of the chart's
// index digests, because the e2e host's registry holds the linux/amd64
// manifest under the tag; no Hubble and no Envoy, two images fewer.
var ciliumValues = []string{
	"kubeProxyReplacement=false",
	"cgroup.autoMount.enabled=false",
	"cgroup.hostRoot=/sys/fs/cgroup",
	"sysctlfix.enabled=false",
	"ipam.mode=cluster-pool",
	"ipam.operator.clusterPoolIPv4PodCIDRList={" + podCIDR + "}",
	"operator.replicas=1",
	"image.useDigest=false",
	"operator.image.useDigest=false",
	"hubble.enabled=false",
	"hubble.relay.enabled=false",
	"hubble.ui.enabled=false",
	"envoy.enabled=false",
	"l7Proxy=false",
}

// manifestImage matches an image reference in a rendered manifest.
var manifestImage = regexp.MustCompile(`(?m)^\s*image:\s*"?([^"\s]+)"?\s*$`)

// The test's workload images: the sandbox image with one layer more, which
// holds workloadFirmware and a file named after the image.
const (
	workloadRepository = "giantswarm/vm-manager-e2e"
	workloadDir        = "vm-manager-e2e"
	// workloadFirmware is what cfg80211 requests from the firmware loader
	// when it loads; the base root does not carry it.
	workloadFirmware = "regulatory.db"
	// workloadUser is the user of the giant-swarm image's config, a name
	// containerd resolves through the image's /etc/passwd: workloadUID.
	workloadUser = "vm-manager-e2e"
	workloadUID  = "4242"
)

// layerKey signs layers: a key with the certificate the nodes trust.
type layerKey struct {
	signer crypto.Signer
	cert   *x509.Certificate
}

func (k layerKey) String() string { return "CN=" + k.cert.Subject.CommonName }

// kms is k signing the way a KMS key does: pkg/layersign signs with a
// crypto/ecdsa key deterministically (RFC 6979), so a layer signed again
// has the same signature, but an ECDSA key in a KMS (Azure Key Vault) draws
// a fresh nonce on every call, so every run gives the layer new signature
// bytes.
func (k layerKey) kms() layerKey { return layerKey{signer: kmsSigner{k.signer}, cert: k.cert} }

type kmsSigner struct{ crypto.Signer }

func (s kmsSigner) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	return s.Signer.Sign(rand.Reader, digest, opts)
}

// newLayerKey parses a PEM key and the PEM certificate of it.
func newLayerKey(t *testing.T, keyPEM, certPEM []byte) layerKey {
	t.Helper()
	signer, err := layersign.ParsePrivateKey(keyPEM)
	require.NoError(t, err)
	certs, err := layersign.ParseCertificates(certPEM)
	require.NoError(t, err)
	cert := layersign.CertificateFor(signer, certs)
	require.NotNil(t, cert, "the certificate is not the key's")
	return layerKey{signer: signer, cert: cert}
}

// giantSwarmLayerKey is the build's test Giant Swarm layer key, whose
// certificate the initrd enrolls into the .dm-verity keyring (images/README.md,
// Layer certificates). It stays in the build's keys/ directory next to build/
// (scripts/gen-keys; e2e.yml downloads it there).
func giantSwarmLayerKey(t *testing.T, image Image) layerKey {
	t.Helper()
	keys := filepath.Join(image.Dir, "..", "keys")
	keyPEM, err := os.ReadFile(filepath.Join(keys, "layer.key")) // #nosec G304 -- build key next to the configured image dir
	require.NoError(t, err, "the build's layer key; build with `make -C images`")
	certPEM, err := os.ReadFile(filepath.Join(keys, "layer.crt")) // #nosec G304 -- build certificate next to the configured image dir
	require.NoError(t, err)
	return newLayerKey(t, keyPEM, certPEM)
}

// layerRegistry is the e2e host's plain-HTTP OCI registry. The guests reach
// it through their network's NAT at the host's address and use it as a
// mirror of gsoci (hostsTOML); it holds linux/amd64 copies of the images
// under their gsoci paths, each signed with `vm-manager layers sign`'s
// library. Nodes find the signatures through its referrers API, as they do
// on gsoci (referrersAPI).
type layerRegistry struct {
	host string
	tmp  string
}

// startLayerRegistry serves a registry with its blobs under dir for the rest
// of the test.
func startLayerRegistry(t *testing.T, dir string) *layerRegistry {
	t.Helper()
	r := &layerRegistry{tmp: filepath.Join(dir, "layers")}
	blobs := filepath.Join(dir, "registry")
	for _, d := range []string{blobs, r.tmp} {
		require.NoError(t, os.MkdirAll(d, 0o750))
	}
	ln, err := net.Listen("tcp", ":0") // #nosec G102 -- the guests reach it through their NAT at the host's address
	require.NoError(t, err)
	srv := &http.Server{
		Handler: referrersAPI(ggcrregistry.New(
			ggcrregistry.WithBlobHandler(ggcrregistry.NewDiskBlobHandler(blobs)),
			ggcrregistry.WithReferrersSupport(true),
			ggcrregistry.Logger(log.New(io.Discard, "", 0)))),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	r.host = net.JoinHostPort(outboundAddress(t), strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	return r
}

// outboundAddress is the host's address on its default route: a VM's
// connection is dialed from the host process, and loopback is not reachable
// from the guest (no host alias on the cluster network). A UDP socket's
// connect sends nothing.
func outboundAddress(t *testing.T) string {
	t.Helper()
	conn, err := net.Dial("udp", "192.0.2.1:9")
	require.NoError(t, err, "no default route on the host")
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// hostsTOML points containerd's gsoci host at the registry, the only host
// with the referrers capability: gsoci's referrers API would answer for the
// mirrored manifests without their signatures.
func (r *layerRegistry) hostsTOML() map[string]any {
	return ignitionFile("/etc/containerd/certs.d/"+gsoci+"/hosts.toml", 0o644,
		`server = "https://`+gsoci+`"
capabilities = ["pull", "resolve"]

[host."http://`+r.host+`"]
  capabilities = ["pull", "resolve", "referrers"]
`)
}

// referrersPath matches the OCI referrers API.
var referrersPath = regexp.MustCompile(`^/v2/(.+)/referrers/(sha256:[0-9a-f]{64})$`)

// referrersAPI completes go-containerregistry's referrers API as the OCI
// distribution spec has it: a descriptor's artifactType is the referrer
// manifest's artifactType (go-containerregistry gives its config media
// type, the empty descriptor's for an artifact), and the artifactType query
// filters the list (OCI-Filters-Applied).
func referrersAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		m := referrersPath.FindStringSubmatch(req.URL.Path)
		if req.Method != http.MethodGet || m == nil {
			next.ServeHTTP(w, req)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, req)
		var index ocispec.Index
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &index) != nil {
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		filter := req.URL.Query()["artifactType"]
		manifests := []ocispec.Descriptor{}
		for _, d := range index.Manifests {
			get := httptest.NewRequestWithContext(req.Context(), http.MethodGet, "/v2/"+m[1]+"/manifests/"+d.Digest.String(), nil)
			get.Header.Set("Accept", d.MediaType)
			body := httptest.NewRecorder()
			next.ServeHTTP(body, get)
			var referrer ocispec.Manifest
			if json.Unmarshal(body.Body.Bytes(), &referrer) == nil && referrer.ArtifactType != "" {
				d.ArtifactType = referrer.ArtifactType
			}
			if len(filter) == 0 || slices.Contains(filter, d.ArtifactType) {
				manifests = append(manifests, d)
			}
		}
		index.Manifests = manifests
		out, err := json.Marshal(index)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", ocispec.MediaTypeImageIndex)
		if len(filter) > 0 {
			w.Header().Set("OCI-Filters-Applied", "artifactType")
		}
		_, _ = w.Write(out)
	})
}

// repository opens path (<repository>[:<tag>]) in the registry.
func (r *layerRegistry) repository(t *testing.T, path string) *orasremote.Repository {
	t.Helper()
	repo, err := registry.Repository(r.host+"/"+path, true)
	require.NoError(t, err)
	return repo
}

// mirrorSigned copies each gsoci image's linux/amd64 manifest under the same
// path and signs it with key. It returns what it signed, by reference.
func (r *layerRegistry) mirrorSigned(ctx context.Context, t *testing.T, refs []string, key layerKey) map[string]layersign.Signed {
	t.Helper()
	all := make(map[string]layersign.Signed, len(refs))
	started := time.Now()
	opts := oras.DefaultCopyOptions
	opts.WithTargetPlatform(&ocispec.Platform{OS: "linux", Architecture: "amd64"})
	for _, ref := range refs {
		path, ok := strings.CutPrefix(ref, gsoci+"/")
		require.True(t, ok, "%s is not on %s", ref, gsoci)
		src, err := registry.Repository(ref, false)
		require.NoError(t, err)
		tag := src.Reference.Reference
		dst := r.repository(t, path)
		desc, err := oras.Copy(ctx, src, tag, dst, tag, opts)
		require.NoError(t, err, "copy %s", ref)
		signed := r.sign(ctx, t, path, key)
		all[ref] = signed
		t.Logf("layer registry: %s linux/amd64 %s, %d layers signed by %s", ref, desc.Digest, len(signed.Layers), key)
	}
	t.Logf("layer registry: %d images mirrored and signed in %.1fs; layer_registry_seconds=%.1f", len(refs), time.Since(started).Seconds(), time.Since(started).Seconds())
	return all
}

// sign signs path's single platform manifest with key.
func (r *layerRegistry) sign(ctx context.Context, t *testing.T, path string, key layerKey) layersign.Signed {
	t.Helper()
	repo := r.repository(t, path)
	signed, err := layersign.Sign(ctx, repo, repo.Reference.ReferenceOrDefault(), layersign.SignOptions{
		Signer:      key.signer,
		Certificate: key.cert,
		TempDir:     r.tmp,
		Logger:      slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err, "sign %s", path)
	require.Len(t, signed, 1, "%s is one platform manifest", path)
	return signed[0]
}

// workloads are the test's workload images, as gsoci references.
type workloads struct {
	// signed are the images with every layer signed, by pod: each is the
	// sandbox image's layer (or that tar) with layers of its own.
	signed map[string]string
	// shared are the layers the signed images share with each other or with
	// the sandbox image, each signed in more than one way.
	shared []sharedLayer
	// unsigned is the giant-swarm image with one layer more, which its
	// referrer (the giant-swarm image's blobs) does not cover: unsignedLayer.
	unsigned      string
	unsignedLayer string
	// untrustedFirst is an image signed by a key the nodes do not trust and
	// one of signed that shares its layer.
	untrustedFirst untrustedFirst
}

// untrustedFirst is an image signed by a key the nodes do not trust, with a
// layer of its own, and an image signed with the Giant Swarm key that is that
// image with a layer more, so it shares the layer. Pulled in that order on a
// node that has neither, the kernel refuses the first image's signature of
// the layer before it becomes a snapshot, and the second image unpacks the
// layer with its own signature.
type untrustedFirst struct {
	// untrusted is the image signed by the untrusted key; untrustedText
	// describes its signature of layer, untrustedSignature.
	untrusted          string
	untrustedSignature digest.Digest
	untrustedText      string
	// layer is the untrusted image's own layer, chainID its snapshot.
	layer   string
	chainID string
	// trusted is the image signed with the Giant Swarm key, a pod of
	// workloads.signed; trustedSignature is its signature of layer.
	trusted          string
	trustedSignature digest.Digest
	trustedText      string
}

// sharedLayer is a layer two images bring with different signatures: one
// snapshot on the node, the chain ID's, serves both.
type sharedLayer struct {
	// name says how the signatures differ.
	name string
	// chainID names the layer's snapshot.
	chainID string
	// signatures describes each image's signature of the layer, by the
	// signature blob's digest.
	signatures map[digest.Digest]string
}

// buildWorkloads pushes and signs the workload images on top of the mirrored
// sandbox image, which sandboxSigned is the Giant Swarm key's signature of.
// They share the sandbox's layer as production images share base layers:
//
//   - giant-swarm: signed with the Giant Swarm key, like the sandbox;
//   - installation: signed with the installation key, so the sandbox's layer
//     has another key's signature;
//   - installation-again: installation with a layer more, signed with the
//     installation key in a run of its own, so the layers it shares with
//     installation have the same key's other signatures (installation signs
//     like a KMS key, layerKey.kms);
//   - recompressed: the sandbox's tar compressed again, signed with the
//     Giant Swarm key: the same tar under another layer digest has another
//     index and root hash, as an index built by another erofs-utils has;
//   - after-refusal: the image of untrustedFirst signed with the Giant Swarm
//     key, on top of untrusted-key, signed with the untrusted key.
func (r *layerRegistry) buildWorkloads(ctx context.Context, t *testing.T, sandboxSigned layersign.Signed, giantSwarm, installation, untrusted layerKey) workloads {
	t.Helper()
	sandbox, err := name.ParseReference(r.host+"/"+strings.TrimPrefix(sandboxImage, gsoci+"/"), name.Insecure)
	require.NoError(t, err)
	base, err := remote.Image(sandbox, remote.WithContext(ctx))
	require.NoError(t, err, "the mirrored sandbox image")
	w := workloads{
		signed:   map[string]string{},
		unsigned: gsociRepository + "/vm-manager-e2e:unsigned-layer",
	}
	repo := r.repository(t, workloadRepository)
	sandboxRepo := r.repository(t, strings.TrimPrefix(sandboxImage, gsoci+"/"))
	push := func(pod string, img v1.Image, user string, key layerKey) (v1.Image, layersign.Signed) {
		img = r.pushWorkload(ctx, t, img, pod, user)
		w.signed[pod] = gsociRepository + "/vm-manager-e2e:" + pod
		return img, r.sign(ctx, t, workloadRepository+":"+pod, key)
	}
	gsImage, gsSigned := push(podGiantSwarm, base, workloadUser, giantSwarm)
	installationImage, installationSigned := push(podInstallation, base, "", installation)
	_, againSigned := push(podInstallationAgain, installationImage, "", installation)
	recompressedImage, recompressedSigned := push(podRecompressed, recompressed(t, base), "", giantSwarm)

	diffIDs := func(img v1.Image) []digest.Digest {
		cfg, err := img.ConfigFile()
		require.NoError(t, err)
		var ids []digest.Digest
		for _, id := range cfg.RootFS.DiffIDs {
			ids = append(ids, digest.Digest(id.String()))
		}
		return ids
	}
	layerDigests := func(img v1.Image) []digest.Digest {
		layers, err := img.Layers()
		require.NoError(t, err)
		var ds []digest.Digest
		for _, l := range layers {
			d, err := l.Digest()
			require.NoError(t, err)
			ds = append(ds, digest.Digest(d.String()))
		}
		return ds
	}
	// signature is how signed signs layer: its root hash and signature blob.
	signature := func(from *orasremote.Repository, image string, signed layersign.Signed, layer digest.Digest, key layerKey) (digest.Digest, string, string) {
		raw, err := content.FetchAll(ctx, from, signed.Referrer)
		require.NoError(t, err)
		var referrer ocispec.Manifest
		require.NoError(t, json.Unmarshal(raw, &referrer))
		for _, l := range referrer.Layers {
			if l.MediaType == layersign.SignatureMediaType && l.Annotations[layersign.AnnotationSourceLayerDigest] == layer.String() {
				root := l.Annotations[layersign.AnnotationRootHash]
				return l.Digest, root, fmt.Sprintf("%s by %s as layer %s, root hash %s, signature %s", image, key, layer, root, l.Digest)
			}
		}
		require.Failf(t, "no signature", "%s's referrer %s signs no layer %s", image, signed.Referrer.Digest, layer)
		return "", "", ""
	}

	sandboxLayer := layerDigests(base)[0]
	sandboxChain := identity.ChainID(diffIDs(base))
	sandboxSig, sandboxRoot, sandboxText := signature(sandboxRepo, sandboxImage, sandboxSigned, sandboxLayer, giantSwarm)
	installationSig, installationRoot, installationText := signature(repo, w.signed[podInstallation], installationSigned, sandboxLayer, installation)
	require.NotEqual(t, sandboxSig, installationSig, "the installation key signs the sandbox's layer differently")
	require.Equal(t, sandboxRoot, installationRoot, "the same layer has the same root hash")
	w.shared = append(w.shared, sharedLayer{name: "different keys", chainID: sandboxChain.String(),
		signatures: map[digest.Digest]string{sandboxSig: sandboxText, installationSig: installationText}})

	ownLayer := layerDigests(installationImage)[1]
	ownChain := identity.ChainID(diffIDs(installationImage))
	firstSig, firstRoot, firstText := signature(repo, w.signed[podInstallation], installationSigned, ownLayer, installation)
	againSig, againRoot, againText := signature(repo, w.signed[podInstallationAgain], againSigned, ownLayer, installation)
	require.NotEqual(t, firstSig, againSig, "the installation key signs like a KMS key, differently in every run")
	require.Equal(t, firstRoot, againRoot, "the same layer has the same root hash")
	w.shared = append(w.shared, sharedLayer{name: "same key, separate runs", chainID: ownChain.String(),
		signatures: map[digest.Digest]string{firstSig: firstText, againSig: againText}})

	recompressedLayer := layerDigests(recompressedImage)[0]
	require.NotEqual(t, sandboxLayer, recompressedLayer, "the recompressed layer has a digest of its own")
	require.Equal(t, sandboxChain, identity.ChainID(diffIDs(recompressedImage)[:1]), "the recompressed layer is the sandbox's tar")
	recompressedSig, recompressedRoot, recompressedText := signature(repo, w.signed[podRecompressed], recompressedSigned, recompressedLayer, giantSwarm)
	require.NotEqual(t, sandboxRoot, recompressedRoot, "the recompressed layer's index, and so its root hash, is another")
	w.shared = append(w.shared, sharedLayer{name: "another index, another root hash", chainID: sandboxChain.String(),
		signatures: map[digest.Digest]string{sandboxSig: sandboxText, recompressedSig: recompressedText}})

	untrustedImage := r.pushWorkload(ctx, t, base, podUntrusted, "")
	untrustedRef := gsociRepository + "/vm-manager-e2e:" + podUntrusted
	untrustedSigned := r.sign(ctx, t, workloadRepository+":"+podUntrusted, untrusted)
	_, afterSigned := push(podAfterRefusal, untrustedImage, "", giantSwarm)
	untrustedLayer := layerDigests(untrustedImage)[1]
	untrustedSig, untrustedRoot, untrustedText := signature(repo, untrustedRef, untrustedSigned, untrustedLayer, untrusted)
	afterSig, afterRoot, afterText := signature(repo, w.signed[podAfterRefusal], afterSigned, untrustedLayer, giantSwarm)
	require.NotEqual(t, untrustedSig, afterSig, "the untrusted key signs the layer differently")
	require.Equal(t, untrustedRoot, afterRoot, "the same layer has the same root hash")
	w.untrustedFirst = untrustedFirst{
		untrusted: untrustedRef, untrustedSignature: untrustedSig, untrustedText: untrustedText,
		layer: untrustedLayer.String(), chainID: identity.ChainID(diffIDs(untrustedImage)).String(),
		trusted: w.signed[podAfterRefusal], trustedSignature: afterSig, trustedText: afterText,
	}

	unsigned := r.pushWorkload(ctx, t, gsImage, "unsigned-layer", "")
	w.unsignedLayer = layerDigests(unsigned)[len(layerDigests(unsigned))-1].String()
	raw, err := content.FetchAll(ctx, repo, gsSigned.Referrer)
	require.NoError(t, err)
	var referrer ocispec.Manifest
	require.NoError(t, json.Unmarshal(raw, &referrer))
	subject := describe(t, unsigned)
	_, err = oras.PackManifest(ctx, repo, oras.PackManifestVersion1_1, layersign.ArtifactType, oras.PackManifestOptions{
		Subject:             &subject,
		Layers:              referrer.Layers,
		ManifestAnnotations: referrer.Annotations,
	})
	require.NoError(t, err, "the unsigned-layer image's referrer")
	for _, pod := range slices.Sorted(maps.Keys(w.signed)) {
		t.Logf("workload %s: %s", pod, w.signed[pod])
	}
	for _, sl := range w.shared {
		t.Logf("shared layer, %s: snapshot %s", sl.name, sl.chainID)
		for _, d := range slices.Sorted(maps.Keys(sl.signatures)) {
			t.Logf("  %s", sl.signatures[d])
		}
	}
	t.Logf("workload %s with layer %s outside its referrer", w.unsigned, w.unsignedLayer)
	t.Logf("untrusted key first: snapshot %s\n  %s\n  %s", w.untrustedFirst.chainID, untrustedText, afterText)
	return w
}

// pushWorkload pushes base with a layer of its own as workloadRepository:tag,
// running as user when it is not empty.
func (r *layerRegistry) pushWorkload(ctx context.Context, t *testing.T, base v1.Image, tag, user string) v1.Image {
	t.Helper()
	layerTar := workloadLayer(t, tag)
	layer, err := tarballLayer(layerTar)
	require.NoError(t, err)
	img, err := mutate.AppendLayers(base, layer)
	require.NoError(t, err)
	if user != "" {
		cfg, err := img.ConfigFile()
		require.NoError(t, err)
		c := cfg.Config.DeepCopy()
		c.User = user
		img, err = mutate.Config(img, *c)
		require.NoError(t, err)
	}
	ref, err := name.ParseReference(r.host+"/"+workloadRepository+":"+tag, name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img, remote.WithContext(ctx)), "push %s", ref)
	return img
}

// recompressed is base with its one layer compressed again, with base's
// config: the same tar, so the same diff ID and chain ID, under another
// layer digest.
func recompressed(t *testing.T, base v1.Image) v1.Image {
	t.Helper()
	layers, err := base.Layers()
	require.NoError(t, err)
	require.Len(t, layers, 1, "the sandbox image has one layer")
	rc, err := layers[0].Uncompressed()
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	tarBytes, err := io.ReadAll(rc)
	require.NoError(t, err)
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(tarBytes)), nil },
		tarball.WithCompressionLevel(gzip.BestCompression))
	require.NoError(t, err)
	cfg, err := base.ConfigFile()
	require.NoError(t, err)
	cfg = cfg.DeepCopy()
	cfg.RootFS.DiffIDs, cfg.History = nil, nil
	img, err := mutate.ConfigFile(empty.Image, cfg)
	require.NoError(t, err)
	img, err = mutate.AppendLayers(img, layer)
	require.NoError(t, err)
	return img
}

// workloadLayer is a tar of workloadDir with workloadFirmware and a file
// named after the image, so that every workload image has a layer of its own,
// and the /etc/passwd and /etc/group that name workloadUser.
func workloadLayer(t *testing.T, tag string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	modTime := time.Unix(0, 0)
	for _, dir := range []string{"etc/", workloadDir + "/"} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o755, ModTime: modTime}))
	}
	for _, f := range []struct{ name, body string }{
		{"etc/passwd", "root:x:0:0:root:/:/pause\n" + workloadUser + ":x:" + workloadUID + ":" + workloadUID + ":vm-manager e2e:/:/pause\n"},
		{"etc/group", "root:x:0:\n" + workloadUser + ":x:" + workloadUID + ":\n"},
		{workloadDir + "/" + workloadFirmware, "vm-manager e2e: a file of a signed container layer, not a regulatory database\n"},
		{workloadDir + "/" + tag, "vm-manager e2e workload " + tag + "\n"},
	} {
		file, body := f.name, f.body
		require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: file, Mode: 0o644, Size: int64(len(body)), ModTime: modTime}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// tarballLayer is an uncompressed tar as a gzip layer.
func tarballLayer(b []byte) (v1.Layer, error) {
	return tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
}

// describe is img's OCI descriptor.
func describe(t *testing.T, img v1.Image) ocispec.Descriptor {
	t.Helper()
	mediaType, err := img.MediaType()
	require.NoError(t, err)
	d, err := img.Digest()
	require.NoError(t, err)
	size, err := img.Size()
	require.NoError(t, err)
	return ocispec.Descriptor{MediaType: string(mediaType), Digest: digest.Digest(d.String()), Size: size}
}

// clusterImages are the gsoci images a cluster of version needs: kubeadm's
// with ClusterConfiguration.imageRepository gsociRepository, the pinned
// sandbox and what the rendered Cilium manifest runs.
func clusterImages(t *testing.T, version, ciliumManifest string) []string {
	t.Helper()
	parts := strings.SplitN(version, ".", 3)
	require.Len(t, parts, 3, "Kubernetes version %q", version)
	extra, ok := kubeadmImages[parts[0]+"."+parts[1]]
	require.True(t, ok, "no kubeadmImages for Kubernetes %s: add `kubeadm config images list --kubernetes-version v%s` to it", version, version)
	refs := []string{sandboxImage}
	for _, component := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-proxy"} {
		refs = append(refs, gsociRepository+"/"+component+":v"+version)
	}
	for _, image := range extra {
		refs = append(refs, gsociRepository+"/"+image)
	}
	for _, m := range manifestImage.FindAllStringSubmatch(ciliumManifest, -1) {
		refs = append(refs, m[1])
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

// renderCilium renders the pinned cilium-app chart for version with
// ciliumValues.
func renderCilium(ctx context.Context, t *testing.T, helm, version string) string {
	t.Helper()
	args := []string{"template", "cilium", ciliumChart, "--version", ciliumChartVersion, "--namespace", "kube-system", "--kube-version", version}
	for _, v := range ciliumValues {
		args = append(args, "--set", v)
	}
	out, err := exec.CommandContext(ctx, helm, args...).Output() // #nosec G204 -- helm from PATH, arguments built here
	var stderr string
	if ee, ok := err.(*exec.ExitError); ok {
		stderr = string(ee.Stderr)
	}
	require.NoError(t, err, "helm template %s %s: %s", ciliumChart, ciliumChartVersion, stderr)
	return string(out)
}

// requireTool returns a host tool from PATH or skips.
func requireTool(t *testing.T, tool string) string {
	t.Helper()
	path, err := exec.LookPath(tool)
	if err != nil {
		t.Skipf("e2e: %v", err)
	}
	return path
}
