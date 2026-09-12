package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/varlink"
	"github.com/giantswarm/vm-manager/internal/varlink/varlinktest"
)

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

// fakeAcquireParams mirrors the wire request with the optionality of the
// IDL, so the fake can tell unset from false.
type fakeAcquireParams struct {
	Name            string `json:"name"`
	CreateMode      string `json:"createMode"`
	Template        string `json:"template"`
	ReadOnly        *bool  `json:"readOnly"`
	RequestAs       string `json:"requestAs"`
	CreateSizeBytes int64  `json:"createSizeBytes"`
}

// fakeStorage serves io.systemd.StorageProvider from dir with the
// semantics of systemd-storage-fs: NAME.volume entries, regular files
// from the sparse-file template, directories exposed as "dir".
func fakeStorage(t *testing.T, dir string) *varlinktest.Server {
	t.Helper()
	idl, err := os.ReadFile(filepath.Join("testdata", "io.systemd.StorageProvider.varlink"))
	require.NoError(t, err)

	srv := varlinktest.New(t, func(c varlinktest.Call) []varlinktest.Reply {
		switch c.Method {
		case methodAcquire:
			var p fakeAcquireParams
			require.NoError(t, json.Unmarshal(c.Parameters, &p))
			return []varlinktest.Reply{fakeAcquire(dir, p)}
		case methodListVolumes:
			var p struct {
				MatchName string `json:"matchName"`
			}
			require.NoError(t, json.Unmarshal(c.Parameters, &p))
			return fakeList(dir, p.MatchName)
		}
		return []varlinktest.Reply{{Error: "org.varlink.service.MethodNotFound"}}
	})
	srv.Interfaces[interfaceName] = string(idl)
	return srv
}

func fakeAcquire(dir string, p fakeAcquireParams) varlinktest.Reply {
	fail := func(name string) varlinktest.Reply { return varlinktest.Reply{Error: name} }
	path := volumePath(dir, p.Name)
	fi, err := os.Stat(path)
	exists := err == nil
	switch {
	case p.CreateMode == "open" && !exists, p.CreateMode == "" && !exists:
		return fail(errNoSuchVolume)
	case p.CreateMode == "new" && exists:
		return fail(errVolumeExists)
	case !exists && p.Template != "" && p.Template != TemplateSparseFile:
		return fail(errNoSuchTemplate)
	case !exists && p.CreateSizeBytes == 0:
		return fail(errCreateSizeRequired)
	case exists && fi.IsDir() && p.RequestAs != "" && p.RequestAs != "dir":
		return fail(errWrongType)
	}
	if !exists {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return fail("io.systemd.System")
		}
		if err := os.Truncate(path, p.CreateSizeBytes); err != nil {
			return fail("io.systemd.System")
		}
		fi, _ = os.Stat(path)
	}
	readOnly := p.ReadOnly != nil && *p.ReadOnly
	flags := os.O_RDWR
	if readOnly || fi.IsDir() {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flags, 0) // #nosec G304 -- test fixture below t.TempDir()
	if err != nil {
		return fail(errReadOnlyVolume)
	}
	typ := typeReg
	if fi.IsDir() {
		typ = "dir"
	}
	return varlinktest.Reply{
		Parameters: map[string]any{"fileDescriptorIndex": 0, "type": typ, "readOnly": readOnly},
		Files:      []*os.File{f},
	}
}

func fakeList(dir, glob string) []varlinktest.Reply {
	if glob == "" {
		glob = "*"
	}
	matches, _ := filepath.Glob(volumePath(dir, glob))
	var out []varlinktest.Reply
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		typ := typeReg
		if fi.IsDir() {
			typ = "dir"
		}
		name := filepath.Base(m)
		out = append(out, varlinktest.Reply{Parameters: map[string]any{
			"name": name[:len(name)-len(VolumeSuffix)], "type": typ, "readOnly": false, "sizeBytes": fi.Size(),
		}})
	}
	return out
}

func TestSystemdProviderAcquireCreatesRegularFileVolume(t *testing.T) {
	dir := t.TempDir()
	srv := fakeStorage(t, dir)
	p := NewSystemdProvider(srv.Path, dir)
	ctx := context.Background()

	v, err := p.Acquire(ctx, AcquireSpec{Name: "vm-1", SizeBytes: 64 * mib, Create: CreateNew})
	require.NoError(t, err)
	defer func() { _ = v.Close() }()

	assert.Equal(t, "vm-1", v.Name)
	assert.Equal(t, volumePath(dir, "vm-1"), v.Path, "path resolved from the descriptor")
	assert.Equal(t, KindFile, v.Kind)
	assert.Equal(t, int64(64*mib), v.SizeBytes)
	assert.False(t, v.ReadOnly)

	// The request carried what systemd-storage-fs needs to create a raw
	// disk rather than its default directory volume.
	calls := srv.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, methodAcquire, calls[0].Method)
	assert.JSONEq(t, `{"name":"vm-1","createMode":"new","readOnly":false,"requestAs":"reg","createSizeBytes":67108864}`, string(calls[0].Parameters))

	// Path and descriptor are the same inode and both usable.
	_, err = v.File().Write([]byte("boot"))
	require.NoError(t, err)
	data, err := os.ReadFile(v.Path) // #nosec G304 -- test fixture below t.TempDir()
	require.NoError(t, err)
	assert.Equal(t, "boot", string(data[:4]))

	// Opening sends no requestAs so block volumes stay acquirable.
	ro, err := p.Acquire(ctx, AcquireSpec{Name: "vm-1", ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = ro.Close() }()
	assert.True(t, ro.ReadOnly)
	assert.JSONEq(t, `{"name":"vm-1","createMode":"open","readOnly":true}`, string(srv.Calls()[1].Parameters))
	_, err = ro.File().Write([]byte("x"))
	assert.Error(t, err)

	require.NoError(t, p.Release(ctx, v))
	assert.Nil(t, v.File())
}

func TestSystemdProviderMapsErrors(t *testing.T) {
	dir := t.TempDir()
	srv := fakeStorage(t, dir)
	p := NewSystemdProvider(srv.Path, dir)
	ctx := context.Background()
	mkdirVolume(t, dir, "tree")

	seed, err := p.Acquire(ctx, AcquireSpec{Name: "have", SizeBytes: mib, Create: CreateNew})
	require.NoError(t, err)
	require.NoError(t, seed.Close())

	tests := []struct {
		name    string
		spec    AcquireSpec
		wantErr error
		varlink string
	}{
		{name: "open missing", spec: AcquireSpec{Name: "nope"}, wantErr: apierr.ErrNotFound, varlink: errNoSuchVolume},
		{name: "new exists", spec: AcquireSpec{Name: "have", SizeBytes: mib, Create: CreateNew}, wantErr: apierr.ErrConflict, varlink: errVolumeExists},
		{name: "new without size", spec: AcquireSpec{Name: "sizeless", Create: CreateNew}, wantErr: apierr.ErrInvalid, varlink: errCreateSizeRequired},
		{name: "unknown template", spec: AcquireSpec{Name: "tpl", SizeBytes: mib, Create: CreateNew, Template: "nvme"}, wantErr: apierr.ErrInvalid, varlink: errNoSuchTemplate},
		{name: "any on directory volume", spec: AcquireSpec{Name: "tree", SizeBytes: mib, Create: CreateAny}, wantErr: apierr.ErrConflict, varlink: errWrongType},
		{name: "open directory volume", spec: AcquireSpec{Name: "tree"}, wantErr: apierr.ErrUnsupported},
		{name: "bad name", spec: AcquireSpec{Name: "a/b"}, wantErr: apierr.ErrInvalid},
		{name: "negative size", spec: AcquireSpec{Name: "neg", SizeBytes: -1, Create: CreateNew}, wantErr: apierr.ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Acquire(ctx, tc.spec)
			require.ErrorIs(t, err, tc.wantErr)
			var verr *varlink.Error
			if tc.varlink == "" {
				assert.False(t, errors.As(err, &verr), "no Varlink error expected: %v", err)
				return
			}
			require.ErrorAs(t, err, &verr, "Varlink error kept in the chain")
			assert.Equal(t, tc.varlink, verr.Name)
		})
	}
}

// mkdirVolume creates a directory volume the way systemd-storage-fs lays
// it out: NAME.volume holding the root/ tree.
func mkdirVolume(t *testing.T, dir, name string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(volumePath(dir, name), "root"), 0o750))
}

func TestSystemdProviderListAndDelete(t *testing.T) {
	dir := t.TempDir()
	srv := fakeStorage(t, dir)
	p := NewSystemdProvider(srv.Path, dir)
	ctx := context.Background()

	for _, name := range []string{"vm-a", "vm-b", "other"} {
		v, err := p.Acquire(ctx, AcquireSpec{Name: name, SizeBytes: mib, Create: CreateNew})
		require.NoError(t, err)
		require.NoError(t, v.Close())
	}
	mkdirVolume(t, dir, "tree")

	all, err := p.List(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, []Info{{Name: "other", SizeBytes: mib}, {Name: "vm-a", SizeBytes: mib}, {Name: "vm-b", SizeBytes: mib}}, all, "directory volumes are skipped")
	assert.True(t, srv.Calls()[len(srv.Calls())-1].More, "ListVolumes is a more stream")

	vms, err := p.List(ctx, "vm-*")
	require.NoError(t, err)
	assert.Len(t, vms, 2)

	require.NoError(t, p.Delete(ctx, "vm-a"))
	require.ErrorIs(t, p.Delete(ctx, "vm-a"), apierr.ErrNotFound)
	require.ErrorIs(t, p.Delete(ctx, "tree"), apierr.ErrUnsupported)
	rest, err := p.List(ctx, "")
	require.NoError(t, err)
	assert.Len(t, rest, 2)
}

func TestSystemdProviderInterfaceMatchesTestdata(t *testing.T) {
	srv := fakeStorage(t, t.TempDir())
	conn, err := varlink.Dial(context.Background(), srv.Path)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	desc, err := conn.InterfaceDescription(context.Background(), interfaceName)
	require.NoError(t, err)
	for _, want := range []string{"method Acquire(", "fileDescriptorIndex: int", "method ListVolumes(", "error NoSuchVolume()", "error VolumeExists()"} {
		assert.Contains(t, desc, want)
	}
}

func TestDetectPrefersUserSocket(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root Detect tries the system socket first")
	}
	dir := t.TempDir()
	srv := fakeStorage(t, dir)
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("XDG_STATE_HOME", dir)
	sockDir := filepath.Dir(UserSocket())
	require.NoError(t, os.MkdirAll(sockDir, 0o700))
	require.NoError(t, os.Symlink(srv.Path, UserSocket()))

	p := Detect(context.Background(), testLogger(t), filepath.Join(dir, "fallback"))
	require.IsType(t, &SystemdProvider{}, p)
	sp := p.(*SystemdProvider)
	assert.Equal(t, "systemd", p.Name())
	assert.Equal(t, UserSocket(), sp.Socket())
	assert.Equal(t, filepath.Join(dir, "storage"), sp.VolumeDir())

	v, err := p.Acquire(context.Background(), AcquireSpec{Name: "vm", SizeBytes: mib, Create: CreateNew})
	require.NoError(t, err)
	_, err = io.ReadFull(v.File(), make([]byte, 1))
	require.NoError(t, err)
	require.NoError(t, v.Close())
}
