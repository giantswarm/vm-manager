package images

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func imageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, v := range []string{"0.1.0", "0.10.0", "0.9.0"} {
		writeFile(t, filepath.Join(dir, "giantswarm-vm-base_"+v+".efi"), "uki")
		writeFile(t, filepath.Join(dir, "giantswarm-vm-base_"+v+".raw"), "ddi")
	}
	writeFile(t, filepath.Join(dir, "other-image_1.0.0.efi"), "uki")
	writeFile(t, filepath.Join(dir, "other-image_1.0.0.raw"), "ddi")
	// Orphans and noise that must be skipped.
	writeFile(t, filepath.Join(dir, "giantswarm-vm-base_2.0.0.efi"), "uki without disk")
	writeFile(t, filepath.Join(dir, "giantswarm-vm-base_3.0.0.raw"), "disk without uki")
	writeFile(t, filepath.Join(dir, "noversion.efi"), "uki")
	writeFile(t, filepath.Join(dir, "noversion.raw"), "ddi")
	writeFile(t, filepath.Join(dir, SysupdateDirName, KubernetesComponent, sumsFile),
		"aaaa  kubernetes_1.31.2.raw\nbbbb  kubernetes_1.32.0.raw\ncccc  SHA256SUMS.gpg\nmalformed\n")
	writeFile(t, filepath.Join(dir, "giantswarm-vm-base_0.9.0"+policySuffix), `{"image_version":"0.9.0","pcr11":{}}`)
	writeFile(t, filepath.Join(dir, policyFile), `{"image_id":"giantswarm-vm-base","image_version":"0.10.0","pcr11":{"enter-initrd":"ab"}}`)
	return dir
}

func TestCatalogScan(t *testing.T) {
	dir := imageDir(t)
	c, err := Load(dir, quiet())
	require.NoError(t, err)

	list := c.List()
	refs := make([]string, 0, len(list))
	for _, img := range list {
		refs = append(refs, img.Ref())
	}
	assert.Equal(t, []string{
		"giantswarm-vm-base_0.10.0", "giantswarm-vm-base_0.9.0", "giantswarm-vm-base_0.1.0", "other-image_1.0.0",
	}, refs)

	img := list[0]
	assert.Equal(t, "giantswarm-vm-base", img.ID)
	assert.Equal(t, "0.10.0", img.Version)
	assert.Equal(t, filepath.Join(dir, "giantswarm-vm-base_0.10.0.efi"), img.UKI)
	assert.Equal(t, filepath.Join(dir, "giantswarm-vm-base_0.10.0.raw"), img.Disk)
	assert.Equal(t, filepath.Join(dir, SysupdateDirName), img.SysupdateDir)
	assert.Equal(t, []string{"1.32.0", "1.31.2"}, img.KubernetesVersions)
	assert.True(t, img.HasKubernetesVersion("1.31.2"))
	assert.False(t, img.HasKubernetesVersion("1.30.0"))
	assert.JSONEq(t, `{"image_id":"giantswarm-vm-base","image_version":"0.10.0","pcr11":{"enter-initrd":"ab"}}`, string(img.Policy))
	assert.JSONEq(t, `{"image_version":"0.9.0","pcr11":{}}`, string(list[1].Policy), "per-image policy file wins")
	assert.Nil(t, list[2].Policy, "policy.json names another version")
	assert.Nil(t, list[3].Policy)
}

func TestCatalogGetAndDefault(t *testing.T) {
	c, err := Load(imageDir(t), quiet())
	require.NoError(t, err)

	tests := []struct {
		name string
		ref  string
		want string
		err  error
	}{
		{name: "exact ref", ref: "giantswarm-vm-base_0.1.0", want: "giantswarm-vm-base_0.1.0"},
		{name: "bare id picks highest", ref: "giantswarm-vm-base", want: "giantswarm-vm-base_0.10.0"},
		{name: "other id", ref: "other-image", want: "other-image_1.0.0"},
		{name: "orphan uki", ref: "giantswarm-vm-base_2.0.0", err: apierr.ErrNotFound},
		{name: "unknown", ref: "nope", err: apierr.ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			img, err := c.Get(tc.ref)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, img.Ref())
		})
	}

	def, err := c.Default()
	require.NoError(t, err)
	assert.Equal(t, "other-image_1.0.0", def.Ref(), "highest version across ids")
}

func TestCatalogEmptyAndMissing(t *testing.T) {
	c, err := Load(t.TempDir(), quiet())
	require.NoError(t, err)
	assert.Empty(t, c.List())
	_, err = c.Default()
	require.ErrorIs(t, err, apierr.ErrNotFound)
	_, err = c.Get("x")
	require.ErrorIs(t, err, apierr.ErrNotFound)

	_, err = Load(filepath.Join(t.TempDir(), "missing"), quiet())
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func TestCatalogRefresh(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir, quiet())
	require.NoError(t, err)
	assert.Empty(t, c.List())

	writeFile(t, filepath.Join(dir, "giantswarm-vm-base_0.1.0.efi"), "uki")
	writeFile(t, filepath.Join(dir, "giantswarm-vm-base_0.1.0.raw"), "ddi")
	require.NoError(t, c.Refresh())
	require.Len(t, c.List(), 1)
	assert.Empty(t, c.List()[0].SysupdateDir)
	assert.Empty(t, c.List()[0].KubernetesVersions)
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"0.10.0", "0.9.0", 1},
		{"0.9.0", "0.10.0", -1},
		{"1.0", "1.0.0", 0},
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc2", "1.0.0-rc1", 1},
		{"abc", "abd", -1},
		{"1.32.0", "1.31.2", 1},
		{"1.36.4", "1.35.4", 1},
		{"1.35.10", "1.35.4", 1},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, CompareVersions(tc.a, tc.b), "%s vs %s", tc.a, tc.b)
	}
}
