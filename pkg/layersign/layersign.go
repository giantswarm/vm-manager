// Package layersign signs the layers of an OCI image for nodes whose
// containerd unpacks them as EROFS tar indexes under dm-verity with
// dm_verity.require_signatures=1 (IPE-enforcing nodes), and verifies such
// signatures.
//
// For every layer of every platform manifest, Sign builds what the node
// opens: the EROFS tar index of the uncompressed tar (mkfs.erofs --tar=i,
// reproducible), the device index ‖ tar zero-padded to 512 bytes, its
// dm-verity hash tree (format 1, SHA-256, 512-byte blocks, 32 zero-byte salt)
// and the detached CMS signature of the root hash. It publishes them as one
// referrer per platform manifest (ArtifactType), three blobs per layer. The
// node builds nothing: it checks the published tree against the layer and
// the signed root hash, and hands the signature to the kernel.
package layersign

import (
	"context"
	"crypto"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
)

const (
	// ArtifactType of the referrer that carries a platform manifest's layer
	// signatures.
	ArtifactType = "application/vnd.containerd.erofs.dmverity.v1"
	// SignatureMediaType: the detached CMS signature (DER) of a layer's root
	// hash, annotated with AnnotationRootHash.
	SignatureMediaType = "application/vnd.containerd.erofs.dmverity.layer-signature.v1+pkcs7"
	// IndexMediaType: the layer's EROFS tar index.
	IndexMediaType = "application/vnd.containerd.erofs.metadata.v1"
	// TreeMediaType: the dm-verity superblock followed by the hash tree.
	TreeMediaType = "application/vnd.containerd.erofs.dmverity.merkle-tree.v1"

	// AnnotationSourceLayerDigest on each blob names the manifest layer it
	// belongs to.
	AnnotationSourceLayerDigest = "io.containerd.erofs.v1/dmverity.source-layer-digest"
	// AnnotationRootHash on the signature: the signed root hash, lowercase hex.
	AnnotationRootHash = "io.containerd.erofs.v1/dmverity.root-hash"
	// AnnotationErofsUtilsVersion on the referrer: the erofs-utils that built
	// the indexes. Nodes do not need it to match theirs.
	AnnotationErofsUtilsVersion = "io.giantswarm.vm-manager.layers/erofs-utils-version"
)

// Target is what Sign and Verify work on: a registry repository
// (oras-go's remote.Repository) or a local store.
type Target = oras.GraphTarget

// SignOptions configure Sign.
type SignOptions struct {
	// Signer signs the root hashes: a private key of ParsePrivateKey, or a
	// KMS key's crypto.Signer. RSA keys sign with PKCS#1 v1.5, ECDSA keys
	// (P-256, P-384) with SHA-256.
	Signer crypto.Signer
	// Certificate of Signer's public key, embedded in every signature; the
	// nodes trust it (the .dm-verity keyring).
	Certificate *x509.Certificate
	// MkfsErofs is the mkfs.erofs to run; empty means the one on PATH.
	MkfsErofs string
	// TempDir holds a layer's tar, index, device and tree while it is signed;
	// empty means the system default.
	TempDir string
	// Now is the time of the referrer's org.opencontainers.image.created; nil
	// means time.Now.
	Now func() time.Time
	// Logger receives a line per layer; nil means slog.Default().
	Logger *slog.Logger
}

// Signed is the result of Sign for one platform manifest.
type Signed struct {
	// Manifest is the signed platform manifest.
	Manifest ocispec.Descriptor
	// Referrer is the pushed referrer manifest.
	Referrer ocispec.Descriptor
	// Layers are the signed layers in manifest order, with their root hashes.
	Layers []SignedLayer
	// Skipped says why the manifest was not signed (an index entry for the
	// platform unknown/unknown); empty for a signed one.
	Skipped string
}

// SignedLayer is one signed layer.
type SignedLayer struct {
	Digest   digest.Digest
	RootHash string
}

// layerBundle is a signed layer's three blobs, as files in the work
// directory.
type layerBundle struct {
	rootHash  string
	signature string
	index     string
	tree      string
}

// Sign signs every layer of every platform manifest reference (a tag or
// digest in target) resolves to and pushes one referrer per platform
// manifest. Signing the same image with the same key again gives the same
// blobs; the newer referrer differs only in its creation time.
func Sign(ctx context.Context, target Target, reference string, opts SignOptions) ([]Signed, error) {
	if opts.Signer == nil || opts.Certificate == nil {
		return nil, fmt.Errorf("a signer and its certificate are required")
	}
	if err := checkPublicKey(opts.Certificate.PublicKey); err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	if CertificateFor(opts.Signer, []*x509.Certificate{opts.Certificate}) == nil {
		return nil, fmt.Errorf("the key is not the certificate's (%s)", opts.Certificate.Subject)
	}
	mkfs, err := newMkfsErofs(ctx, opts.MkfsErofs)
	if err != nil {
		return nil, err
	}
	manifests, err := resolve(ctx, target, reference)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(opts.TempDir, "vm-manager-layers-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(work) }()

	s := &signer{opts: opts, target: target, mkfs: mkfs, work: work, bundles: map[digest.Digest]*layerBundle{}}
	var out []Signed
	for _, pm := range manifests {
		if pm.skipped != "" {
			s.logger().Warn("skipped manifest", "manifest", pm.desc.Digest, "reason", pm.skipped)
			out = append(out, Signed{Manifest: pm.desc, Skipped: pm.skipped})
			continue
		}
		signed, err := s.manifest(ctx, pm)
		if err != nil {
			return out, fmt.Errorf("manifest %s: %w", pm.desc.Digest, err)
		}
		out = append(out, signed)
	}
	return out, nil
}

func resolve(ctx context.Context, target Target, reference string) ([]platformManifest, error) {
	desc, err := target.Resolve(ctx, reference)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", reference, err)
	}
	manifests, err := platformManifests(ctx, target, desc)
	if err != nil {
		return nil, err
	}
	// Every manifest's layers are checked before any work, so an image with
	// a layer that cannot be signed fails as a whole.
	images := 0
	for _, pm := range manifests {
		if pm.skipped != "" {
			continue
		}
		if _, _, err := pm.layers(); err != nil {
			return nil, fmt.Errorf("manifest %s: %w", pm.desc.Digest, err)
		}
		images++
	}
	if images == 0 {
		return nil, fmt.Errorf("%s (%s, %s) holds no container image manifest", reference, desc.MediaType, desc.Digest)
	}
	return manifests, nil
}

type signer struct {
	opts    SignOptions
	target  Target
	mkfs    *mkfsErofs
	work    string
	bundles map[digest.Digest]*layerBundle
}

func (s *signer) manifest(ctx context.Context, pm platformManifest) (Signed, error) {
	signed := Signed{Manifest: pm.desc}
	layers, diffIDs, err := pm.layers()
	if err != nil || len(layers) == 0 {
		return signed, err
	}
	var blobs []ocispec.Descriptor
	for i, layer := range layers {
		b, err := s.layer(ctx, layer, diffIDs[i])
		if err != nil {
			return signed, err
		}
		for _, f := range []struct {
			mediaType, path string
			annotations     map[string]string
		}{
			{SignatureMediaType, b.signature, map[string]string{AnnotationRootHash: b.rootHash}},
			{IndexMediaType, b.index, nil},
			{TreeMediaType, b.tree, nil},
		} {
			desc, err := s.push(ctx, f.mediaType, f.path)
			if err != nil {
				return signed, fmt.Errorf("layer %s: %w", layer.Digest, err)
			}
			desc.Annotations = map[string]string{AnnotationSourceLayerDigest: layer.Digest.String()}
			for k, v := range f.annotations {
				desc.Annotations[k] = v
			}
			blobs = append(blobs, desc)
		}
		signed.Layers = append(signed.Layers, SignedLayer{Digest: layer.Digest, RootHash: b.rootHash})
	}
	now := time.Now
	if s.opts.Now != nil {
		now = s.opts.Now
	}
	subject := pm.desc
	referrer, err := oras.PackManifest(ctx, s.target, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		Subject: &subject,
		Layers:  blobs,
		ManifestAnnotations: map[string]string{
			ocispec.AnnotationCreated:   now().UTC().Format(time.RFC3339),
			AnnotationErofsUtilsVersion: s.mkfs.version,
		},
	})
	if err != nil {
		return signed, fmt.Errorf("push referrer: %w", err)
	}
	signed.Referrer = referrer
	return signed, nil
}

// layer builds and signs one layer, once per run.
func (s *signer) layer(ctx context.Context, layer ocispec.Descriptor, diffID digest.Digest) (*layerBundle, error) {
	if b, ok := s.bundles[layer.Digest]; ok {
		return b, nil
	}
	dir, err := os.MkdirTemp(s.work, "layer-")
	if err != nil {
		return nil, err
	}
	p := func(name string) string { return filepath.Join(dir, name) }
	if err := fetchTar(ctx, s.target, layer, diffID, p("tar")); err != nil {
		return nil, err
	}
	if err := s.mkfs.index(ctx, p("tar"), p("index"), layer.Digest); err != nil {
		return nil, fmt.Errorf("layer %s: %w", layer.Digest, err)
	}
	size, err := writeDevice(p("index"), p("tar"), p("device"))
	if err != nil {
		return nil, fmt.Errorf("layer %s: %w", layer.Digest, err)
	}
	root, err := writeTree(p("device"), size, p("tree"), layer.Digest)
	if err != nil {
		return nil, fmt.Errorf("layer %s: %w", layer.Digest, err)
	}
	sig, err := signContent([]byte(root), s.opts.Signer, s.opts.Certificate)
	if err != nil {
		return nil, fmt.Errorf("layer %s: %w", layer.Digest, err)
	}
	if err := os.WriteFile(p("signature"), sig, 0o600); err != nil {
		return nil, err
	}
	// The tar and the device are the bulk; only the three blobs stay.
	_ = os.Remove(p("tar"))
	_ = os.Remove(p("device"))
	b := &layerBundle{rootHash: root, signature: p("signature"), index: p("index"), tree: p("tree")}
	s.bundles[layer.Digest] = b
	s.logger().Info("signed layer", "layer", layer.Digest, "rootHash", root)
	return b, nil
}

// push pushes the file at path as a blob unless the target has it.
func (s *signer) push(ctx context.Context, mediaType, path string) (ocispec.Descriptor, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a file of the work directory
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer func() { _ = f.Close() }()
	d, err := digest.Canonical.FromReader(f)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: fi.Size()}
	exists, err := s.target.Exists(ctx, desc)
	if err != nil {
		return desc, err
	}
	if exists {
		return desc, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return desc, err
	}
	if err := s.target.Push(ctx, desc, f); err != nil {
		return desc, fmt.Errorf("push %s: %w", mediaType, err)
	}
	return desc, nil
}

func (s *signer) logger() *slog.Logger {
	if s.opts.Logger != nil {
		return s.opts.Logger
	}
	return slog.Default()
}
