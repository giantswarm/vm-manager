package layersign

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
)

const testTag = "v1"

var (
	testTime = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	quiet    = slog.New(slog.DiscardHandler)
)

func requireMkfsErofs(t *testing.T) {
	t.Helper()
	if _, err := newMkfsErofs(context.Background(), ""); err != nil {
		t.Skipf("no usable mkfs.erofs: %v", err)
	}
}

func tarOf(t *testing.T, files map[string]string, padding int) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"etc/", "etc/os-release", "bin/app", ".wh.gone"} {
		body, ok := files[name]
		if !ok {
			continue
		}
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(1700000000, 0), Typeflag: tar.TypeReg}
		if name[len(name)-1] == '/' {
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	// Bytes after the end-of-archive marker belong to the diff_id too.
	buf.Write(make([]byte, padding))
	return buf.Bytes()
}

func compress(t *testing.T, kind string, b []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	switch kind {
	case "gzip":
		w := gzip.NewWriter(&buf)
		_, err := w.Write(b)
		require.NoError(t, err)
		require.NoError(t, w.Close())
		return buf.Bytes(), ocispec.MediaTypeImageLayerGzip
	case "zstd":
		w, err := zstd.NewWriter(&buf)
		require.NoError(t, err)
		_, err = w.Write(b)
		require.NoError(t, err)
		require.NoError(t, w.Close())
		return buf.Bytes(), ocispec.MediaTypeImageLayerZstd
	default:
		return b, ocispec.MediaTypeImageLayer
	}
}

type testLayer struct {
	tar         []byte
	compression string
	mediaType   string // overrides the compression's
}

func pushBlob(t *testing.T, store content.Pusher, mediaType string, b []byte) ocispec.Descriptor {
	t.Helper()
	desc := content.NewDescriptorFromBytes(mediaType, b)
	if err := store.Push(context.Background(), desc, bytes.NewReader(b)); !errors.Is(err, errdef.ErrAlreadyExists) {
		require.NoError(t, err)
	}
	return desc
}

func pushJSON(t *testing.T, store content.Pusher, mediaType string, v any) ocispec.Descriptor {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return pushBlob(t, store, mediaType, b)
}

func pushManifest(t *testing.T, store content.Pusher, arch string, layers []testLayer) ocispec.Descriptor {
	t.Helper()
	m := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest}
	config := ocispec.Image{Platform: ocispec.Platform{OS: "linux", Architecture: arch}, RootFS: ocispec.RootFS{Type: "layers"}}
	for _, l := range layers {
		blob, mediaType := compress(t, l.compression, l.tar)
		if l.mediaType != "" {
			mediaType = l.mediaType
		}
		m.Layers = append(m.Layers, pushBlob(t, store, mediaType, blob))
		config.RootFS.DiffIDs = append(config.RootFS.DiffIDs, digest.FromBytes(l.tar))
	}
	m.Config = pushJSON(t, store, ocispec.MediaTypeImageConfig, config)
	desc := pushJSON(t, store, ocispec.MediaTypeImageManifest, m)
	desc.Platform = &ocispec.Platform{OS: "linux", Architecture: arch}
	return desc
}

// testImage is an index of two platforms that share a layer, with gzip,
// zstd and uncompressed layers, and a build attestation.
func testImage(t *testing.T) (*memory.Store, map[string][]byte) {
	t.Helper()
	store := memory.New()
	tars := map[string][]byte{
		"base":  tarOf(t, map[string]string{"etc/": "", "etc/os-release": strings.Repeat("ID=test\n", 4096)}, 0),
		"amd64": tarOf(t, map[string]string{"bin/app": "amd64 binary"}, 4096),
		"zstd":  tarOf(t, map[string]string{"bin/app": "zstd layer", ".wh.gone": ""}, 0),
		"arm64": tarOf(t, map[string]string{"bin/app": "arm64 binary"}, 0),
	}
	amd64 := pushManifest(t, store, "amd64", []testLayer{{tar: tars["base"], compression: "gzip"}, {tar: tars["amd64"]}, {tar: tars["zstd"], compression: "zstd"}})
	arm64 := pushManifest(t, store, "arm64", []testLayer{{tar: tars["base"], compression: "gzip"}, {tar: tars["arm64"], compression: "gzip"}})
	attestation := pushManifest(t, store, "unknown", []testLayer{{tar: []byte(`{"_type":"https://in-toto.io/Statement/v0.1"}`), mediaType: "application/vnd.in-toto+json"}})
	attestation.Annotations = map[string]string{dockerReferenceType: "attestation-manifest"}
	index := pushJSON(t, store, ocispec.MediaTypeImageIndex, ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amd64, arm64, attestation},
	})
	require.NoError(t, store.Tag(context.Background(), index, testTag))
	return store, tars
}

func signOpts(k testKey, at time.Time) SignOptions {
	return SignOptions{Signer: k.signer, Certificate: k.cert, Now: func() time.Time { return at }, Logger: quiet}
}

func referrerManifest(t *testing.T, store content.Fetcher, desc ocispec.Descriptor) ocispec.Manifest {
	t.Helper()
	var m ocispec.Manifest
	require.NoError(t, fetchJSON(context.Background(), store, desc, &m))
	return m
}

func TestSignVerify(t *testing.T) {
	requireMkfsErofs(t)
	ctx := context.Background()
	store, _ := testImage(t)
	k := newTestKey(t, "rsa")

	signed, err := Sign(ctx, store, testTag, signOpts(k, testTime))
	require.NoError(t, err)
	require.Len(t, signed, 2, "one referrer per platform manifest; the attestation is skipped")
	require.Len(t, signed[0].Layers, 3)
	require.Len(t, signed[1].Layers, 2)
	require.Equal(t, signed[0].Layers[0], signed[1].Layers[0], "a shared layer has one root hash")

	for _, s := range signed {
		refs, err := registry.Referrers(ctx, store, s.Manifest, ArtifactType)
		require.NoError(t, err)
		require.Len(t, refs, 1)
		m := referrerManifest(t, store, s.Referrer)
		require.Equal(t, ArtifactType, m.ArtifactType)
		require.Equal(t, ocispec.DescriptorEmptyJSON.Digest, m.Config.Digest)
		require.Equal(t, s.Manifest.Digest, m.Subject.Digest)
		require.Equal(t, testTime.Format(time.RFC3339), m.Annotations[ocispec.AnnotationCreated])
		require.Regexp(t, `^\d+\.\d+`, m.Annotations[AnnotationErofsUtilsVersion])
		require.Len(t, m.Layers, 3*len(s.Layers))
		for i, l := range s.Layers {
			blobs := m.Layers[3*i : 3*i+3]
			for j, mt := range []string{SignatureMediaType, IndexMediaType, TreeMediaType} {
				require.Equal(t, mt, blobs[j].MediaType)
				require.Equal(t, l.Digest.String(), blobs[j].Annotations[AnnotationSourceLayerDigest])
			}
			require.Equal(t, l.RootHash, blobs[0].Annotations[AnnotationRootHash])
			require.Regexp(t, `^[0-9a-f]{64}$`, l.RootHash)
		}
	}

	verified, err := Verify(ctx, store, testTag, VerifyOptions{Certificates: []*x509.Certificate{k.cert}, Logger: quiet})
	require.NoError(t, err)
	require.Len(t, verified, 2)
	require.Equal(t, k.cert, verified[1].Layers[1].Signer)

	// Re-signing gives the same blobs; the newer referrer is the one used.
	again, err := Sign(ctx, store, testTag, signOpts(k, testTime.Add(time.Hour)))
	require.NoError(t, err)
	for i := range signed {
		require.NotEqual(t, signed[i].Referrer.Digest, again[i].Referrer.Digest)
		require.Equal(t, referrerManifest(t, store, signed[i].Referrer).Layers, referrerManifest(t, store, again[i].Referrer).Layers)
	}
	verified, err = Verify(ctx, store, testTag, VerifyOptions{Certificates: []*x509.Certificate{k.cert}, Logger: quiet})
	require.NoError(t, err)
	require.Equal(t, again[0].Referrer.Digest, verified[0].Referrer.Digest)

	// A rotation: the new key's newer referrer verifies with both
	// certificates trusted, not with the old one alone.
	next := newTestKey(t, "p256")
	_, err = Sign(ctx, store, testTag, signOpts(next, testTime.Add(2*time.Hour)))
	require.NoError(t, err)
	_, err = Verify(ctx, store, testTag, VerifyOptions{Certificates: []*x509.Certificate{k.cert, next.cert}, Logger: quiet})
	require.NoError(t, err)
	_, err = Verify(ctx, store, testTag, VerifyOptions{Certificates: []*x509.Certificate{k.cert}, Logger: quiet})
	require.ErrorContains(t, err, "none of the given certificates")
}

// TestRootHashCrossCheck rebuilds a device from the published index and the
// tar and compares veritysetup's root hash with the signed one.
func TestRootHashCrossCheck(t *testing.T) {
	requireMkfsErofs(t)
	veritysetup, err := exec.LookPath("veritysetup")
	if err != nil {
		t.Skip("veritysetup not installed")
	}
	ctx := context.Background()
	store, tars := testImage(t)
	k := newTestKey(t, "p384")
	signed, err := Sign(ctx, store, testTag, signOpts(k, testTime))
	require.NoError(t, err)

	m := referrerManifest(t, store, signed[0].Referrer)
	dir := t.TempDir()
	index, tarPath, device := filepath.Join(dir, "index"), filepath.Join(dir, "tar"), filepath.Join(dir, "device")
	b, err := content.FetchAll(ctx, store, m.Layers[1])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(index, b, 0o600))
	require.NoError(t, os.WriteFile(tarPath, tars["base"], 0o600))
	_, err = writeDevice(index, tarPath, device)
	require.NoError(t, err)
	//nolint:gosec // G204: veritysetup from PATH, test files
	out, err := exec.Command(veritysetup, "format", "--data-block-size=512", "--hash-block-size=512",
		"--salt="+fmt.Sprintf("%064d", 0), device, filepath.Join(dir, "hash")).CombinedOutput()
	require.NoError(t, err, string(out))
	require.Regexp(t, `Root hash:\s+`+signed[0].Layers[0].RootHash, string(out))
}

func TestSignRefusesNativeEROFS(t *testing.T) {
	requireMkfsErofs(t)
	store := memory.New()
	desc := pushManifest(t, store, "amd64", []testLayer{{tar: []byte("erofs image"), mediaType: "application/vnd.erofs.layer.v1"}})
	require.NoError(t, store.Tag(context.Background(), desc, testTag))

	_, err := Sign(context.Background(), store, testTag, signOpts(newTestKey(t, "rsa"), testTime))
	require.ErrorContains(t, err, "native EROFS layer")
}

func TestSignRefusesMismatchedKey(t *testing.T) {
	store, _ := testImage(t)
	k, other := newTestKey(t, "rsa"), newTestKey(t, "rsa")
	_, err := Sign(context.Background(), store, testTag, SignOptions{Signer: k.signer, Certificate: other.cert})
	require.ErrorContains(t, err, "not the certificate's")
}

func TestMkfsErofsChecks(t *testing.T) {
	_, err := newMkfsErofs(context.Background(), filepath.Join(t.TempDir(), "mkfs.erofs"))
	require.ErrorContains(t, err, "not found")

	old := filepath.Join(t.TempDir(), "mkfs.erofs")
	require.NoError(t, os.WriteFile(old, []byte("#!/bin/sh\necho 'mkfs.erofs (erofs-utils) 1.7.1'\necho ' --tar=X  tarball'\n"), 0o700)) //nolint:gosec // G306: an executable script
	_, err = newMkfsErofs(context.Background(), old)
	require.ErrorContains(t, err, "erofs-utils 1.7.1) is too old")
}

// tamper pushes a newer referrer for the first platform manifest, built from
// the signed one with change applied to its blobs.
func tamper(t *testing.T, store *memory.Store, s Signed, change func(blobs []ocispec.Descriptor) []ocispec.Descriptor) {
	t.Helper()
	m := referrerManifest(t, store, s.Referrer)
	blobs := change(append([]ocispec.Descriptor(nil), m.Layers...))
	_, err := oras.PackManifest(context.Background(), store, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		Subject:             m.Subject,
		Layers:              blobs,
		ManifestAnnotations: map[string]string{ocispec.AnnotationCreated: testTime.Add(time.Hour).Format(time.RFC3339)},
	})
	require.NoError(t, err)
}

// flipByte replaces blob with a copy whose byte at offset is flipped.
func flipByte(t *testing.T, store *memory.Store, blob ocispec.Descriptor, offset int64) ocispec.Descriptor {
	t.Helper()
	b, err := content.FetchAll(context.Background(), store, blob)
	require.NoError(t, err)
	b[offset] ^= 0xff
	desc := pushBlob(t, store, blob.MediaType, b)
	desc.Annotations = blob.Annotations
	return desc
}

func TestVerifyCatchesTampering(t *testing.T) {
	requireMkfsErofs(t)
	for name, tc := range map[string]struct {
		change  func(t *testing.T, store *memory.Store, blobs []ocispec.Descriptor) []ocispec.Descriptor
		wantErr string
	}{
		"index": {
			change: func(t *testing.T, store *memory.Store, blobs []ocispec.Descriptor) []ocispec.Descriptor {
				blobs[1] = flipByte(t, store, blobs[1], 1024)
				return blobs
			},
			wantErr: "does not verify against the published tree",
		},
		"tree leaf": {
			// Superblock, the top block, then the first leaf block.
			change: func(t *testing.T, store *memory.Store, blobs []ocispec.Descriptor) []ocispec.Descriptor {
				blobs[2] = flipByte(t, store, blobs[2], 1024)
				return blobs
			},
			wantErr: "does not verify against the published tree",
		},
		"tree root": {
			change: func(t *testing.T, store *memory.Store, blobs []ocispec.Descriptor) []ocispec.Descriptor {
				blobs[2] = flipByte(t, store, blobs[2], 512)
				return blobs
			},
			wantErr: "is not the signed root hash",
		},
		"tar": {
			// The first layer's index and tree with the second layer's tar.
			change: func(t *testing.T, store *memory.Store, blobs []ocispec.Descriptor) []ocispec.Descriptor {
				first, second := blobs[0].Annotations[AnnotationSourceLayerDigest], blobs[3].Annotations[AnnotationSourceLayerDigest]
				for i, src := range map[int]string{1: second, 2: second, 4: first, 5: first} {
					blobs[i].Annotations = map[string]string{AnnotationSourceLayerDigest: src}
				}
				return blobs
			},
			wantErr: "the device has",
		},
		"root-hash annotation": {
			change: func(t *testing.T, store *memory.Store, blobs []ocispec.Descriptor) []ocispec.Descriptor {
				blobs[0].Annotations = map[string]string{
					AnnotationSourceLayerDigest: blobs[0].Annotations[AnnotationSourceLayerDigest],
					AnnotationRootHash:          blobs[3].Annotations[AnnotationRootHash],
				}
				return blobs
			},
			wantErr: "signature:",
		},
		"missing tree": {
			change: func(t *testing.T, store *memory.Store, blobs []ocispec.Descriptor) []ocispec.Descriptor {
				return append(blobs[:2], blobs[3:]...)
			},
			wantErr: "lacks its tree blob",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, _ := testImage(t)
			k := newTestKey(t, "p256")
			signed, err := Sign(context.Background(), store, testTag, signOpts(k, testTime))
			require.NoError(t, err)
			tamper(t, store, signed[0], func(blobs []ocispec.Descriptor) []ocispec.Descriptor { return tc.change(t, store, blobs) })

			_, err = Verify(context.Background(), store, testTag, VerifyOptions{Certificates: []*x509.Certificate{k.cert}, Logger: quiet})
			var le *LayerError
			require.ErrorAs(t, err, &le)
			require.Equal(t, signed[0].Layers[0].Digest, le.Layer, "names the layer")
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
