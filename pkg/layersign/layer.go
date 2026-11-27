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
	// The tar layer types containerd unpacks; other blobs a manifest lists
	// (in-toto statements) are not unpacked and not signed.
	ociLayerPrefix    = "application/vnd.oci.image.layer."
	dockerLayerPrefix = "application/vnd.docker.image.rootfs."
	// dockerReferenceType on an index entry marks a build attestation.
	dockerReferenceType = "vnd.docker.reference.type"
)

// platformManifest is one image manifest a node pulls: of the reference
// itself, or of one platform of its index.
type platformManifest struct {
	desc     ocispec.Descriptor
	manifest ocispec.Manifest
	diffIDs  []digest.Digest
}

// platformManifests resolves desc to the image manifests under it. Manifests
// of an index that are no container images (build attestations, artifacts)
// are skipped.
func platformManifests(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) ([]platformManifest, error) {
	switch desc.MediaType {
	case ocispec.MediaTypeImageIndex, dockerManifestList:
		var index ocispec.Index
		if err := fetchJSON(ctx, fetcher, desc, &index); err != nil {
			return nil, err
		}
		var out []platformManifest
		for _, m := range index.Manifests {
			if m.Annotations[dockerReferenceType] == "attestation-manifest" {
				continue
			}
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
			return nil, nil
		}
		var config ocispec.Image
		if err := fetchJSON(ctx, fetcher, m.Config, &config); err != nil {
			return nil, err
		}
		return []platformManifest{{
			desc:     ocispec.Descriptor{MediaType: desc.MediaType, Digest: desc.Digest, Size: desc.Size},
			manifest: m,
			diffIDs:  config.RootFS.DiffIDs,
		}}, nil
	default:
		return nil, nil
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

// layers returns the distinct tar layers of the manifest with their
// diff_ids (empty when the config does not list them one per layer). A
// native EROFS layer is an error: its signing is not this format.
func (pm platformManifest) layers() ([]ocispec.Descriptor, []digest.Digest, error) {
	seen := map[digest.Digest]bool{}
	var layers []ocispec.Descriptor
	var diffIDs []digest.Digest
	for i, l := range pm.manifest.Layers {
		if strings.HasPrefix(l.MediaType, erofsLayerPrefix) {
			return nil, nil, fmt.Errorf("layer %s is a native EROFS layer (%s): only tar layers are signed", l.Digest, l.MediaType)
		}
		if seen[l.Digest] || !strings.HasPrefix(l.MediaType, ociLayerPrefix) && !strings.HasPrefix(l.MediaType, dockerLayerPrefix) {
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
