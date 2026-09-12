package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

const mib = 1 << 20

func TestFileProviderAcquire(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		existing map[string]int64 // volumes created beforehand with their size
		spec     AcquireSpec
		wantErr  error
		wantSize int64
		wantRO   bool
	}{
		{name: "new sparse", spec: AcquireSpec{Name: "a", SizeBytes: 64 * mib, Create: CreateNew}, wantSize: 64 * mib},
		{name: "new allocated", spec: AcquireSpec{Name: "a", SizeBytes: mib, Create: CreateNew, Template: TemplateAllocatedFile}, wantSize: mib},
		{name: "new exists", existing: map[string]int64{"a": mib}, spec: AcquireSpec{Name: "a", SizeBytes: mib, Create: CreateNew}, wantErr: apierr.ErrConflict},
		{name: "new without size", spec: AcquireSpec{Name: "a", Create: CreateNew}, wantErr: apierr.ErrInvalid},
		{name: "new bad template", spec: AcquireSpec{Name: "a", SizeBytes: mib, Create: CreateNew, Template: "directory"}, wantErr: apierr.ErrInvalid},
		{name: "open missing", spec: AcquireSpec{Name: "a"}, wantErr: apierr.ErrNotFound},
		{name: "open keeps size", existing: map[string]int64{"a": 2 * mib}, spec: AcquireSpec{Name: "a", SizeBytes: mib}, wantSize: 2 * mib},
		{name: "open read-only", existing: map[string]int64{"a": mib}, spec: AcquireSpec{Name: "a", ReadOnly: true}, wantSize: mib, wantRO: true},
		{name: "any creates", spec: AcquireSpec{Name: "a", SizeBytes: mib, Create: CreateAny}, wantSize: mib},
		{name: "any opens", existing: map[string]int64{"a": 3 * mib}, spec: AcquireSpec{Name: "a", SizeBytes: mib, Create: CreateAny}, wantSize: 3 * mib},
		{name: "new read-only", spec: AcquireSpec{Name: "a", SizeBytes: mib, Create: CreateNew, ReadOnly: true}, wantSize: mib, wantRO: true},
		{name: "empty name", spec: AcquireSpec{Name: "", SizeBytes: mib, Create: CreateNew}, wantErr: apierr.ErrInvalid},
		{name: "dot name", spec: AcquireSpec{Name: "..", SizeBytes: mib, Create: CreateNew}, wantErr: apierr.ErrInvalid},
		{name: "slash name", spec: AcquireSpec{Name: "a/b", SizeBytes: mib, Create: CreateNew}, wantErr: apierr.ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "volumes")
			p := NewFileProvider(dir)
			for name, size := range tc.existing {
				v, err := p.Acquire(ctx, AcquireSpec{Name: name, SizeBytes: size, Create: CreateNew})
				require.NoError(t, err)
				require.NoError(t, p.Release(ctx, v))
			}

			v, err := p.Acquire(ctx, tc.spec)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			defer func() { assert.NoError(t, v.Close()) }()

			assert.Equal(t, tc.spec.Name, v.Name)
			assert.Equal(t, filepath.Join(dir, tc.spec.Name+VolumeSuffix), v.Path)
			assert.Equal(t, KindFile, v.Kind)
			assert.Equal(t, tc.wantSize, v.SizeBytes)
			assert.Equal(t, tc.wantRO, v.ReadOnly)

			fi, err := os.Stat(v.Path)
			require.NoError(t, err)
			assert.Equal(t, tc.wantSize, fi.Size())
			assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

			// The held descriptor is the same inode as Path and is usable.
			ffi, err := v.File().Stat()
			require.NoError(t, err)
			assert.True(t, os.SameFile(fi, ffi))
			_, err = v.File().Write([]byte("x"))
			if tc.wantRO {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestFileProviderAllocatedTemplateAllocatesBlocks(t *testing.T) {
	p := NewFileProvider(t.TempDir())
	ctx := context.Background()

	sparse, err := p.Acquire(ctx, AcquireSpec{Name: "sparse", SizeBytes: 4 * mib, Create: CreateNew})
	require.NoError(t, err)
	defer func() { _ = sparse.Close() }()
	full, err := p.Acquire(ctx, AcquireSpec{Name: "full", SizeBytes: 4 * mib, Create: CreateNew, Template: TemplateAllocatedFile})
	require.NoError(t, err)
	defer func() { _ = full.Close() }()

	assert.Less(t, blocks(t, sparse.Path), blocks(t, full.Path))
}

// blocks is the number of 512-byte blocks actually allocated for path.
func blocks(t *testing.T, path string) int64 {
	t.Helper()
	var st unix.Stat_t
	require.NoError(t, unix.Stat(path, &st))
	return st.Blocks
}

func TestFileProviderDeleteAndList(t *testing.T) {
	p := NewFileProvider(t.TempDir())
	ctx := context.Background()

	for _, name := range []string{"vm-a", "vm-b", "other"} {
		v, err := p.Acquire(ctx, AcquireSpec{Name: name, SizeBytes: mib, Create: CreateNew})
		require.NoError(t, err)
		require.NoError(t, v.Close())
		assert.NoError(t, v.Close(), "Close is idempotent")
	}
	// A stray directory entry is not a volume vm-manager lists or deletes.
	require.NoError(t, os.Mkdir(volumePath(p.VolumeDir(), "tree"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(volumePath(p.VolumeDir(), "tree"), "f"), nil, 0o600))

	all, err := p.List(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, []Info{{Name: "other", SizeBytes: mib}, {Name: "vm-a", SizeBytes: mib}, {Name: "vm-b", SizeBytes: mib}}, all)

	vms, err := p.List(ctx, "vm-*")
	require.NoError(t, err)
	assert.Len(t, vms, 2)

	require.NoError(t, p.Delete(ctx, "vm-a"))
	require.ErrorIs(t, p.Delete(ctx, "vm-a"), apierr.ErrNotFound)
	require.ErrorIs(t, p.Delete(ctx, "tree"), apierr.ErrUnsupported)
	require.ErrorIs(t, p.Delete(ctx, "../x"), apierr.ErrInvalid)
	_, err = os.Stat(volumePath(p.VolumeDir(), "vm-a"))
	require.ErrorIs(t, err, os.ErrNotExist)

	// An open volume can still be read after its entry is deleted: the
	// descriptor is the lease.
	v, err := p.Acquire(ctx, AcquireSpec{Name: "vm-b"})
	require.NoError(t, err)
	defer func() { _ = v.Close() }()
	require.NoError(t, p.Delete(ctx, "vm-b"))
	_, err = io.ReadAll(io.LimitReader(v.File(), 16))
	require.NoError(t, err)
}

func TestDetectFallsBackToFiles(t *testing.T) {
	if _, err := os.Stat(SystemSocket); err == nil {
		t.Skipf("%s exists on this host; Detect would pick it", SystemSocket)
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := filepath.Join(t.TempDir(), "fallback")

	p := Detect(context.Background(), testLogger(t), dir)
	require.IsType(t, &FileProvider{}, p)
	assert.Equal(t, "file", p.Name())
	assert.Equal(t, dir, p.(*FileProvider).VolumeDir())
}
