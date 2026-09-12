//go:build integration

package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

// storagectlVolume is one entry of `storagectl --user volumes --json=short`.
type storagectlVolume struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	RO       bool   `json:"ro"`
	Size     int64  `json:"size"`
}

func storagectlVolumes(t *testing.T, ctx context.Context) map[string]storagectlVolume {
	t.Helper()
	out, err := exec.CommandContext(ctx, "storagectl", "--user", "volumes", "--json=short").Output()
	require.NoError(t, err, "storagectl --user volumes")
	var list []storagectlVolume
	if len(out) > 0 {
		require.NoError(t, json.Unmarshal(out, &list), "parse %s", out)
	}
	byName := make(map[string]storagectlVolume, len(list))
	for _, v := range list {
		if v.Provider == "fs" {
			byName[v.Name] = v
		}
	}
	return byName
}

// TestIntegrationSystemdUserProvider runs the full volume lifecycle against
// the user-level systemd-storage-fs on this host: create, use through the
// path and the descriptor, observe with storagectl, release, delete.
func TestIntegrationSystemdUserProvider(t *testing.T) {
	socket := UserSocket()
	if _, err := os.Stat(socket); err != nil {
		t.Skipf("no user storage provider at %s: %v", socket, err)
	}
	if _, err := exec.LookPath("storagectl"); err != nil {
		t.Skip("storagectl not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := NewSystemdProvider(socket, UserVolumeDir())
	name := fmt.Sprintf("vmm-it-%d", os.Getpid())
	t.Cleanup(func() { _ = p.Delete(context.Background(), name) })

	const size = 64 * mib
	v, err := p.Acquire(ctx, AcquireSpec{Name: name, SizeBytes: size, Create: CreateNew})
	require.NoError(t, err)
	t.Logf("acquired %s: path=%s kind=%s size=%d ro=%v", v.Name, v.Path, v.Kind, v.SizeBytes, v.ReadOnly)

	assert.Equal(t, KindFile, v.Kind)
	assert.Equal(t, int64(size), v.SizeBytes)
	assert.False(t, v.ReadOnly)
	assert.Equal(t, volumePath(UserVolumeDir(), name), v.Path)

	// Write through the path, as QEMU will, and read back through the
	// descriptor the provider handed out.
	f, err := os.OpenFile(v.Path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("vm-manager"), 1*mib)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	buf := make([]byte, 10)
	_, err = v.File().ReadAt(buf, 1*mib)
	require.NoError(t, err)
	assert.Equal(t, "vm-manager", string(buf))
	fi, err := os.Stat(v.Path)
	require.NoError(t, err)
	assert.Equal(t, int64(size), fi.Size())

	listed := storagectlVolumes(t, ctx)
	require.Contains(t, listed, name, "storagectl --user volumes lists the volume")
	assert.Equal(t, storagectlVolume{Provider: "fs", Name: name, Type: "reg", Size: size}, listed[name])

	ours, err := p.List(ctx, name)
	require.NoError(t, err)
	assert.Equal(t, []Info{{Name: name, Kind: KindFile, SizeBytes: size}}, ours)

	_, err = p.Acquire(ctx, AcquireSpec{Name: name, SizeBytes: size, Create: CreateNew})
	require.ErrorIs(t, err, apierr.ErrConflict)

	again, err := p.Acquire(ctx, AcquireSpec{Name: name, ReadOnly: true})
	require.NoError(t, err)
	assert.True(t, again.ReadOnly)
	_, err = io.ReadFull(again.File(), buf[:1])
	require.NoError(t, err)
	require.NoError(t, p.Release(ctx, again))

	require.NoError(t, p.Release(ctx, v))
	require.NoError(t, p.Delete(ctx, name))

	_, err = os.Stat(v.Path)
	require.ErrorIs(t, err, os.ErrNotExist, "entry removed")
	require.NotContains(t, storagectlVolumes(t, ctx), name, "storagectl no longer lists the volume")
	require.ErrorIs(t, p.Delete(ctx, name), apierr.ErrNotFound)
	_, err = p.Acquire(ctx, AcquireSpec{Name: name})
	require.ErrorIs(t, err, apierr.ErrNotFound)

	leftovers, err := filepath.Glob(volumePath(UserVolumeDir(), "vmm-it-*"))
	require.NoError(t, err)
	assert.Empty(t, leftovers, "no leftovers from this or earlier runs")
}

// TestIntegrationDetectPrefersSystemd checks Detect picks a systemd
// provider when one answers on this host.
func TestIntegrationDetectPrefersSystemd(t *testing.T) {
	if _, err := os.Stat(UserSocket()); err != nil {
		t.Skipf("no user storage provider: %v", err)
	}
	p := Detect(context.Background(), testLogger(t), t.TempDir())
	require.IsType(t, &SystemdProvider{}, p)
	sp := p.(*SystemdProvider)
	t.Logf("detected %s at %s (volumes in %s)", p.Name(), sp.Socket(), sp.VolumeDir())
	if os.Geteuid() != 0 {
		assert.Equal(t, UserSocket(), sp.Socket())
	}
}
