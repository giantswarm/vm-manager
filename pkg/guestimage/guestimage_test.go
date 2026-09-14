package guestimage

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ggcr "github.com/google/go-containerregistry/pkg/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
)

// buildDir lays out what `make -C images` leaves behind, the published files
// and the build-only leftovers alike.
func buildDir(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	stem := "giantswarm-vm-base_" + version
	files := map[string]string{
		stem + ".efi":                                "uki " + version,
		stem + ".raw":                                "disk " + version,
		stem + ".roothash":                           strings.Repeat("a", 64),
		stem + ".repart.d/10-esp.conf":               "[Partition]\nType=esp\n",
		PolicyFile:                                   `{"image_id":"giantswarm-vm-base","image_version":"` + version + `","pcr11":{}}`,
		"sysupdate/base/SHA256SUMS":                  "sums",
		"sysupdate/base/" + stem + ".efi":            "uki " + version,
		"sysupdate/kubernetes/SHA256SUMS":            "sums",
		"sysupdate/kubernetes/kubernetes_1.36.4.raw": "sysext",
		// Build leftovers that must not travel.
		stem + ".initrd":              "initrd",
		stem + ".root-x86-64.raw":     "partition",
		"kubernetes_1.36.4.raw":       "sysext",
		"base/usr/lib/os-release":     "leftover tree",
		"pkgs/1.36.4/kubeadm.pkg.zst": "package",
		"initrd.cpio.zst":             "initrd",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	require.NoError(t, os.Symlink(stem+".raw", filepath.Join(dir, stem)))
	return dir
}

func TestContents(t *testing.T) {
	dir := buildDir(t, "0.1.0")
	got, err := Contents(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"giantswarm-vm-base_0.1.0.efi",
		"giantswarm-vm-base_0.1.0.raw",
		"giantswarm-vm-base_0.1.0.roothash",
		"giantswarm-vm-base_0.1.0.repart.d/10-esp.conf",
		"policy.json",
		"sysupdate/base/SHA256SUMS",
		"sysupdate/base/giantswarm-vm-base_0.1.0.efi",
		"sysupdate/kubernetes/SHA256SUMS",
		"sysupdate/kubernetes/kubernetes_1.36.4.raw",
	}, got)

	empty := t.TempDir()
	_, err = Contents(empty)
	assert.ErrorContains(t, err, "holds no image")

	require.NoError(t, os.Remove(filepath.Join(dir, PolicyFile)))
	_, err = Contents(dir)
	assert.ErrorContains(t, err, "policy.json")
}

// readFile reads a test fixture.
func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- test fixtures.
	require.NoError(t, err)
	return string(raw)
}

func testRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(ggcr.New(ggcr.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestPushPull(t *testing.T) {
	ctx := context.Background()
	host := testRegistry(t)
	opts := Options{PlainHTTP: true}
	ref := host + "/giantswarm/vm-manager-guest-image:0.1.0"

	src := buildDir(t, "0.1.0")
	desc, err := Push(ctx, src, ref, opts)
	require.NoError(t, err)
	assert.Equal(t, ocispec.MediaTypeImageManifest, desc.MediaType)

	// First pull: everything lands, nothing else, marker written.
	dst := filepath.Join(t.TempDir(), "images")
	res, err := Pull(ctx, ref, dst, opts)
	require.NoError(t, err)
	assert.True(t, res.Fetched)
	assert.Equal(t, 9, res.Files)
	assert.Equal(t, desc.Digest, res.Descriptor.Digest)
	for _, name := range []string{"giantswarm-vm-base_0.1.0.efi", "giantswarm-vm-base_0.1.0.raw", "giantswarm-vm-base_0.1.0.repart.d/10-esp.conf", "sysupdate/kubernetes/kubernetes_1.36.4.raw"} {
		assert.Equal(t, readFile(t, filepath.Join(src, name)), readFile(t, filepath.Join(dst, name)), name)
	}
	for _, leftover := range []string{"giantswarm-vm-base_0.1.0.initrd", "kubernetes_1.36.4.raw", "base", "pkgs"} {
		_, err := os.Stat(filepath.Join(dst, leftover))
		assert.ErrorIs(t, err, os.ErrNotExist, leftover)
	}
	marker, err := ReadMarker(dst)
	require.NoError(t, err)
	assert.Equal(t, ref, marker.Reference)
	assert.Equal(t, desc.Digest.String(), marker.Digest)
	assert.Equal(t, 9, marker.Files)

	// The same digest again: a no-op that keeps a policy.json amended by
	// `image golden`.
	policy := filepath.Join(dst, PolicyFile)
	require.NoError(t, os.WriteFile(policy, []byte(`{"golden":{"sha256":{"0":"aa"}}}`), 0o600))
	res, err = Pull(ctx, ref, dst, opts)
	require.NoError(t, err)
	assert.False(t, res.Fetched)
	assert.Contains(t, readFile(t, policy), "golden")

	// A new build under the same tag replaces the directory: the old image
	// and the amended policy are gone.
	desc2, err := Push(ctx, buildDir(t, "0.2.0"), ref, opts)
	require.NoError(t, err)
	require.NotEqual(t, desc.Digest, desc2.Digest)
	res, err = Pull(ctx, ref, dst, opts)
	require.NoError(t, err)
	assert.True(t, res.Fetched)
	_, err = os.Stat(filepath.Join(dst, "giantswarm-vm-base_0.1.0.efi"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(dst, "giantswarm-vm-base_0.2.0.efi"))
	assert.NoError(t, err)
	assert.NotContains(t, readFile(t, policy), "golden")

	// A digest reference pulls too.
	byDigest := host + "/giantswarm/vm-manager-guest-image@" + desc.Digest.String()
	other := filepath.Join(t.TempDir(), "images")
	res, err = Pull(ctx, byDigest, other, opts)
	require.NoError(t, err)
	assert.True(t, res.Fetched)
	_, err = os.Stat(filepath.Join(other, "giantswarm-vm-base_0.1.0.efi"))
	assert.NoError(t, err)
}

func TestPushNeedsATag(t *testing.T) {
	_, err := Push(context.Background(), buildDir(t, "0.1.0"), testRegistry(t)+"/giantswarm/vm-manager-guest-image", Options{PlainHTTP: true})
	assert.ErrorContains(t, err, "tag")
}

// TestPullRefusesOtherArtifacts: an artifact of another type, or one naming a
// file outside the directory, is refused before the directory is touched.
func TestPullRefusesOtherArtifacts(t *testing.T) {
	ctx := context.Background()
	host := testRegistry(t)
	opts := Options{PlainHTTP: true}
	dst := t.TempDir()
	keep := filepath.Join(dst, "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("mine"), 0o600))

	push := func(t *testing.T, ref, artifactType string, layers []ocispec.Descriptor, store *memory.Store) {
		t.Helper()
		manifest, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, artifactType, oras.PackManifestOptions{Layers: layers})
		require.NoError(t, err)
		require.NoError(t, store.Tag(ctx, manifest, "x"))
		repo, err := remote.NewRepository(ref)
		require.NoError(t, err)
		repo.PlainHTTP = true
		_, err = oras.Copy(ctx, store, "x", repo, repo.Reference.Reference, oras.DefaultCopyOptions)
		require.NoError(t, err)
	}

	store := memory.New()
	blob, err := oras.PushBytes(ctx, store, FileMediaType, []byte("hello"))
	require.NoError(t, err)

	other := host + "/other:1"
	push(t, other, "application/vnd.example.other.v1", []ocispec.Descriptor{blob}, store)
	_, err = Pull(ctx, other, dst, opts)
	assert.ErrorContains(t, err, "not a guest image artifact")

	blob.Annotations = map[string]string{ocispec.AnnotationTitle: "../escape"}
	traversal := host + "/traversal:1"
	push(t, traversal, ArtifactType, []ocispec.Descriptor{blob}, memory.New())
	_, err = Pull(ctx, traversal, dst, opts)
	assert.ErrorContains(t, err, "not a path inside")

	assert.Equal(t, "mine", readFile(t, keep), "a refused pull must not touch the directory")
}

func TestMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Marker{Reference: "r", Digest: "sha256:abc", Files: 3}
	require.NoError(t, writeMarker(dir, want))
	var onDisk map[string]any
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(dir, MarkerFile))), &onDisk))
	assert.Equal(t, "sha256:abc", onDisk["digest"])
	got, err := ReadMarker(dir)
	require.NoError(t, err)
	assert.Equal(t, want.Digest, got.Digest)
	assert.Equal(t, want.Files, got.Files)
}
