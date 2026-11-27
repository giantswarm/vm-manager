package layersign

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

const (
	dockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	dockerConfig       = "application/vnd.docker.container.image.v1+json"
	// erofsLayerPrefix: native EROFS layers, which containerd mounts without a
	// tar index; their signing is not this format.
	erofsLayerPrefix = "application/vnd.erofs.layer"
)

// tarLayerTypes are the layer media types a layer is signed for: the tar
// layers containerd unpacks, compressed with gzip, zstd or not at all. Any
// other layer type in an image manifest fails the image, so nothing
// containerd unpacks goes unsigned or unchecked.
var tarLayerTypes = map[string]bool{
	ocispec.MediaTypeImageLayer:                                 true,
	ocispec.MediaTypeImageLayerGzip:                             true,
	ocispec.MediaTypeImageLayerZstd:                             true,
	ocispec.MediaTypeImageLayerNonDistributable:                 true, //nolint:staticcheck // still unpacked by containerd
	ocispec.MediaTypeImageLayerNonDistributableGzip:             true, //nolint:staticcheck // still unpacked by containerd
	ocispec.MediaTypeImageLayerNonDistributableZstd:             true, //nolint:staticcheck // still unpacked by containerd
	"application/vnd.docker.image.rootfs.diff.tar":              true,
	"application/vnd.docker.image.rootfs.diff.tar.gzip":         true,
	"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip": true,
	"application/vnd.docker.image.rootfs.foreign.diff.tar":      true,
}

// platformManifest is one image manifest a node pulls, of the reference
// itself or of one platform of its index; or a manifest of the index that
// is skipped, with the reason.
type platformManifest struct {
	desc     ocispec.Descriptor
	manifest ocispec.Manifest
	diffIDs  []digest.Digest
	skipped  string
}

// checkDigest refuses a descriptor digest that is not a well-formed SHA-256
// or SHA-512 digest: digests come from the registry and name files and
// blobs.
func checkDigest(d digest.Digest) error {
	if err := d.Validate(); err != nil {
		return fmt.Errorf("digest %q: %w", d, err)
	}
	if a := d.Algorithm(); a != digest.SHA256 && a != digest.SHA512 {
		return fmt.Errorf("digest %q: algorithm %s is neither sha256 nor sha512", d, a)
	}
	return nil
}

// isUnknownPlatform: BuildKit marks its attestation manifests in an index
// with the platform unknown/unknown; nothing pulls them as images.
func isUnknownPlatform(p *ocispec.Platform) bool {
	return p != nil && p.OS == "unknown" && p.Architecture == "unknown"
}

// platformManifests resolves desc to the image manifests under it. Only an
// index entry for the platform unknown/unknown is skipped (and returned as
// skipped); any other manifest that is no container image fails, as does
// a digest that is not sha256 or sha512.
func platformManifests(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) ([]platformManifest, error) {
	if err := checkDigest(desc.Digest); err != nil {
		return nil, err
	}
	ref := ocispec.Descriptor{MediaType: desc.MediaType, Digest: desc.Digest, Size: desc.Size}
	if isUnknownPlatform(desc.Platform) {
		return []platformManifest{{desc: ref, skipped: "platform unknown/unknown (a build attestation)"}}, nil
	}
	switch desc.MediaType {
	case ocispec.MediaTypeImageIndex, dockerManifestList:
		var index ocispec.Index
		if err := fetchJSON(ctx, fetcher, desc, &index); err != nil {
			return nil, err
		}
		var out []platformManifest
		for _, m := range index.Manifests {
			pms, err := platformManifests(ctx, fetcher, m)
			if err != nil {
				return nil, err
			}
			out = append(out, pms...)
		}
		return out, nil
	case ocispec.MediaTypeImageManifest, dockerManifest:
		var m ocispec.Manifest
		if err := fetchJSON(ctx, fetcher, desc, &m); err != nil {
			return nil, err
		}
		if m.Config.MediaType != ocispec.MediaTypeImageConfig && m.Config.MediaType != dockerConfig {
			return nil, fmt.Errorf("manifest %s: config media type %q is not a container image config", desc.Digest, m.Config.MediaType)
		}
		for _, d := range append([]ocispec.Descriptor{m.Config}, m.Layers...) {
			if err := checkDigest(d.Digest); err != nil {
				return nil, fmt.Errorf("manifest %s: %w", desc.Digest, err)
			}
		}
		var config ocispec.Image
		if err := fetchJSON(ctx, fetcher, m.Config, &config); err != nil {
			return nil, err
		}
		return []platformManifest{{desc: ref, manifest: m, diffIDs: config.RootFS.DiffIDs}}, nil
	default:
		return nil, fmt.Errorf("manifest %s: media type %q is neither an image index nor an image manifest", desc.Digest, desc.MediaType)
	}
}

func fetchJSON(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor, v any) error {
	b, err := content.FetchAll(ctx, fetcher, desc)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", desc.Digest, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("parse %s: %w", desc.Digest, err)
	}
	return nil
}

// layers returns the distinct layers of the manifest with their diff_ids
// (empty when the config does not list them one per layer). A layer of any
// type but a tar layer fails: a native EROFS layer is not signed in this
// format, and anything else is not a layer this signer can process.
func (pm platformManifest) layers() ([]ocispec.Descriptor, []digest.Digest, error) {
	seen := map[digest.Digest]bool{}
	var layers []ocispec.Descriptor
	var diffIDs []digest.Digest
	for i, l := range pm.manifest.Layers {
		switch {
		case strings.HasPrefix(l.MediaType, erofsLayerPrefix):
			return nil, nil, fmt.Errorf("layer %s is a native EROFS layer (%s): only tar layers are signed", l.Digest, l.MediaType)
		case !tarLayerTypes[l.MediaType]:
			return nil, nil, fmt.Errorf("layer %s has media type %q, which is no tar layer this signer processes", l.Digest, l.MediaType)
		case seen[l.Digest]:
			continue
		}
		seen[l.Digest] = true
		layers = append(layers, l)
		var id digest.Digest
		if len(pm.diffIDs) == len(pm.manifest.Layers) {
			id = pm.diffIDs[i]
		}
		diffIDs = append(diffIDs, id)
	}
	return layers, diffIDs, nil
}

// fetchTar writes the uncompressed tar of layer (the diff_id bytes) to out.
// Gzip and zstd are recognized by their magic bytes, as containerd does;
// anything else is taken as an uncompressed tar.
func fetchTar(ctx context.Context, fetcher content.Fetcher, layer ocispec.Descriptor, diffID digest.Digest, out string) error {
	rc, err := fetcher.Fetch(ctx, layer)
	if err != nil {
		return fmt.Errorf("fetch layer %s: %w", layer.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	vr := content.NewVerifyReader(rc, layer)
	br := bufio.NewReader(vr)
	magic, _ := br.Peek(4)
	var r io.Reader = br
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("layer %s: %w", layer.Digest, err)
		}
		defer func() { _ = gz.Close() }()
		r = gz
	case bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		zr, err := zstd.NewReader(br)
		if err != nil {
			return fmt.Errorf("layer %s: %w", layer.Digest, err)
		}
		defer zr.Close()
		r = zr
	}
	f, err := os.Create(out) //nolint:gosec // G304: a file of the work directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	d := digest.Canonical.Digester()
	if _, err := io.Copy(io.MultiWriter(f, d.Hash()), r); err != nil {
		return fmt.Errorf("layer %s: %w", layer.Digest, err)
	}
	// Drain what the decompressor left (gzip's trailer), so the digest of
	// the compressed blob is checked.
	if _, err := io.Copy(io.Discard, br); err != nil {
		return fmt.Errorf("layer %s: %w", layer.Digest, err)
	}
	if err := vr.Verify(); err != nil {
		return fmt.Errorf("layer %s: %w", layer.Digest, err)
	}
	if diffID != "" && d.Digest() != diffID {
		return fmt.Errorf("layer %s: uncompressed digest %s is not the config's diff_id %s", layer.Digest, d.Digest(), diffID)
	}
	return f.Close()
}
