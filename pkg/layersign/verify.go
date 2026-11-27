package layersign

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
)

// VerifyOptions configure Verify.
type VerifyOptions struct {
	// Certificates a signature may be made with: the nodes' .dm-verity
	// keyring. During a rotation, the old and the new one.
	Certificates []*x509.Certificate
	// TempDir holds a layer's blobs and device while it is checked; empty
	// means the system default.
	TempDir string
	// Logger receives a line per layer; nil means slog.Default().
	Logger *slog.Logger
}

// Verified is the result of Verify for one platform manifest.
type Verified struct {
	Manifest ocispec.Descriptor
	Referrer ocispec.Descriptor
	Layers   []VerifiedLayer
}

// VerifiedLayer is one layer whose signature, tree and data check out.
type VerifiedLayer struct {
	Digest   digest.Digest
	RootHash string
	// Signer is the certificate of Certificates that made the signature.
	Signer *x509.Certificate
}

// LayerError names the layer that failed.
type LayerError struct {
	Manifest digest.Digest
	Layer    digest.Digest
	Err      error
}

func (e *LayerError) Error() string {
	return fmt.Sprintf("manifest %s, layer %s: %v", e.Manifest, e.Layer, e.Err)
}

func (e *LayerError) Unwrap() error { return e.Err }

var rootHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Verify checks, for every platform manifest reference resolves to, what a
// node checks before it opens a layer: its newest referrer of ArtifactType
// holds a signature, an index and a tree for every layer; the signature over
// the root-hash annotation verifies against one of the certificates by the
// kernel's rules; the tree's root is that root hash; and the device built
// from the published index and the layer's tar verifies against the tree.
// The first failure is returned as a *LayerError when a layer is to blame.
func Verify(ctx context.Context, target Target, reference string, opts VerifyOptions) ([]Verified, error) {
	if len(opts.Certificates) == 0 {
		return nil, errors.New("at least one certificate is required")
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
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	var out []Verified
	for _, pm := range manifests {
		v, err := verifyManifest(ctx, target, pm, opts.Certificates, work, logger)
		if err != nil {
			return out, err
		}
		if len(v.Layers) > 0 {
			out = append(out, v)
		}
	}
	return out, nil
}

func verifyManifest(ctx context.Context, target Target, pm platformManifest, certs []*x509.Certificate, work string, logger *slog.Logger) (Verified, error) {
	v := Verified{Manifest: pm.desc}
	layers, diffIDs, err := pm.layers()
	if err != nil || len(layers) == 0 {
		return v, err
	}
	referrer, err := newestReferrer(ctx, target, pm.desc)
	if err != nil {
		return v, fmt.Errorf("manifest %s: %w", pm.desc.Digest, err)
	}
	v.Referrer = referrer
	var m ocispec.Manifest
	if err := fetchJSON(ctx, target, referrer, &m); err != nil {
		return v, fmt.Errorf("manifest %s: referrer: %w", pm.desc.Digest, err)
	}
	if m.Subject == nil || m.Subject.Digest != pm.desc.Digest {
		return v, fmt.Errorf("manifest %s: referrer %s has another subject", pm.desc.Digest, referrer.Digest)
	}
	bundles, err := groupBlobs(m.Layers)
	if err != nil {
		return v, fmt.Errorf("manifest %s: referrer %s: %w", pm.desc.Digest, referrer.Digest, err)
	}
	for i, layer := range layers {
		vl, err := verifyLayer(ctx, target, layer, diffIDs[i], bundles[layer.Digest], certs, work)
		if err != nil {
			return v, &LayerError{Manifest: pm.desc.Digest, Layer: layer.Digest, Err: err}
		}
		logger.Info("verified layer", "manifest", pm.desc.Digest, "layer", layer.Digest, "rootHash", vl.RootHash, "signer", vl.Signer.Subject.String())
		v.Layers = append(v.Layers, vl)
	}
	return v, nil
}

// newestReferrer picks the referrer a node uses: the newest by
// org.opencontainers.image.created, ties to the lowest digest.
func newestReferrer(ctx context.Context, target Target, subject ocispec.Descriptor) (ocispec.Descriptor, error) {
	refs, err := registry.Referrers(ctx, target, subject, ArtifactType)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("list referrers: %w", err)
	}
	if len(refs) == 0 {
		return ocispec.Descriptor{}, errors.New("no layer signatures: no referrer of type " + ArtifactType)
	}
	created := func(d ocispec.Descriptor) time.Time {
		t, _ := time.Parse(time.RFC3339, d.Annotations[ocispec.AnnotationCreated])
		return t
	}
	sort.Slice(refs, func(i, j int) bool {
		ti, tj := created(refs[i]), created(refs[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return refs[i].Digest < refs[j].Digest
	})
	return refs[0], nil
}

type blobs struct {
	signature, index, tree *ocispec.Descriptor
}

// groupBlobs groups a referrer's blobs by source layer; a layer with two
// blobs of one kind makes the referrer malformed.
func groupBlobs(descs []ocispec.Descriptor) (map[digest.Digest]*blobs, error) {
	out := map[digest.Digest]*blobs{}
	for i := range descs {
		d := &descs[i]
		src := digest.Digest(d.Annotations[AnnotationSourceLayerDigest])
		if err := src.Validate(); err != nil {
			return nil, fmt.Errorf("blob %s: annotation %s: %w", d.Digest, AnnotationSourceLayerDigest, err)
		}
		b := out[src]
		if b == nil {
			b = &blobs{}
			out[src] = b
		}
		var slot **ocispec.Descriptor
		switch d.MediaType {
		case SignatureMediaType:
			slot = &b.signature
		case IndexMediaType:
			slot = &b.index
		case TreeMediaType:
			slot = &b.tree
		default:
			continue
		}
		if *slot != nil {
			return nil, fmt.Errorf("layer %s has two %s blobs", src, d.MediaType)
		}
		*slot = d
	}
	return out, nil
}

func verifyLayer(ctx context.Context, target Target, layer ocispec.Descriptor, diffID digest.Digest, b *blobs, certs []*x509.Certificate, work string) (VerifiedLayer, error) {
	vl := VerifiedLayer{Digest: layer.Digest}
	switch {
	case b == nil:
		return vl, errors.New("not signed: the referrer has no blobs for it")
	case b.signature == nil:
		return vl, errors.New("the referrer lacks its signature blob")
	case b.index == nil:
		return vl, errors.New("the referrer lacks its index blob")
	case b.tree == nil:
		return vl, errors.New("the referrer lacks its tree blob")
	}
	root := b.signature.Annotations[AnnotationRootHash]
	if !rootHashRE.MatchString(root) {
		return vl, fmt.Errorf("root-hash annotation %q is not 64 lowercase hex digits", root)
	}
	vl.RootHash = root
	sig, err := content.FetchAll(ctx, target, *b.signature)
	if err != nil {
		return vl, fmt.Errorf("fetch signature: %w", err)
	}
	if vl.Signer, err = verifyContent(sig, []byte(root), certs); err != nil {
		return vl, fmt.Errorf("signature: %w", err)
	}

	dir, err := os.MkdirTemp(work, "verify-")
	if err != nil {
		return vl, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	p := func(name string) string { return filepath.Join(dir, name) }
	for _, f := range []struct {
		desc *ocispec.Descriptor
		path string
	}{{b.index, p("index")}, {b.tree, p("tree")}} {
		if err := fetchFile(ctx, target, *f.desc, f.path); err != nil {
			return vl, err
		}
	}
	if err := fetchTar(ctx, target, layer, diffID, p("tar")); err != nil {
		return vl, err
	}
	size, err := writeDevice(p("index"), p("tar"), p("device"))
	if err != nil {
		return vl, err
	}
	treeRoot, err := checkTree(p("tree"), size)
	if err != nil {
		return vl, fmt.Errorf("tree: %w", err)
	}
	if treeRoot != root {
		return vl, fmt.Errorf("the tree's root hash %s is not the signed root hash %s", treeRoot, root)
	}
	if err := verifyDevice(p("device"), size, p("tree"), root); err != nil {
		return vl, err
	}
	return vl, nil
}

func fetchFile(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor, out string) error {
	rc, err := fetcher.Fetch(ctx, desc)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", desc.MediaType, err)
	}
	defer func() { _ = rc.Close() }()
	f, err := os.Create(out) //nolint:gosec // G304: a file of the work directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	vr := content.NewVerifyReader(rc, desc)
	if _, err := io.Copy(f, vr); err != nil {
		return fmt.Errorf("fetch %s: %w", desc.MediaType, err)
	}
	if err := vr.Verify(); err != nil {
		return fmt.Errorf("fetch %s: %w", desc.MediaType, err)
	}
	return f.Close()
}
