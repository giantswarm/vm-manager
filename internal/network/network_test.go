package network

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), testLogger(t))
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	return m
}

func TestManagerLifecycle(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	_, err := m.Get("dev")
	require.ErrorIs(t, err, apierr.ErrNotFound)
	assert.Empty(t, m.List())

	_, err = m.Create(ctx, Spec{Name: "dev", CIDR: "10.0.0.0/8"})
	require.ErrorIs(t, err, apierr.ErrInvalid)

	dev, err := m.Create(ctx, Spec{Name: "dev", CIDR: "10.42.0.0/24", EnableIMDS: true})
	require.NoError(t, err)
	assert.Equal(t, "dev", dev.Name())
	assert.Equal(t, "10.42.0.1", dev.GatewayIP().String())
	assert.Equal(t, "10.42.0.254", dev.HostIP().String())
	assert.Equal(t, "10.42.0.0/24", dev.Prefix().String())
	assert.Equal(t, filepath.Join(m.Dir("dev"), SocketName), dev.SocketPath())
	assert.FileExists(t, dev.SocketPath())

	_, err = m.Create(ctx, Spec{Name: "dev", CIDR: "10.43.0.0/24"})
	require.ErrorIs(t, err, apierr.ErrConflict)

	prod, err := m.Create(ctx, Spec{Name: "prod", CIDR: "10.43.0.0/24"})
	require.NoError(t, err)

	got, err := m.Get("dev")
	require.NoError(t, err)
	assert.Same(t, dev, got)
	assert.Equal(t, []*Network{dev, prod}, m.List(), "sorted by name")

	att, err := dev.Attach(ctx, "vm-1")
	require.NoError(t, err)
	require.ErrorIs(t, m.Delete(ctx, "dev"), apierr.ErrConflict, "attached VMs block deletion")
	require.NoError(t, dev.Detach(att.VMID))

	require.NoError(t, m.Delete(ctx, "dev"))
	require.ErrorIs(t, m.Delete(ctx, "dev"), apierr.ErrNotFound)
	assert.NoFileExists(t, dev.SocketPath())
	assert.NoDirExists(t, m.Dir("dev"))
	assert.Equal(t, []*Network{prod}, m.List())

	_, err = dev.Attach(ctx, "vm-2")
	require.ErrorIs(t, err, apierr.ErrConflict, "closed network refuses attachments")
	_, err = dev.Dial(ctx, "10.42.0.2:22")
	require.ErrorIs(t, err, apierr.ErrConflict)
	_, err = dev.ListenIMDS()
	require.ErrorIs(t, err, apierr.ErrConflict)
	require.NoError(t, dev.Close(), "Close is idempotent")
}

func TestManagerContextCancelled(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := m.Create(ctx, Spec{Name: "dev", CIDR: "10.42.0.0/24"})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, m.Delete(ctx, "dev"), context.Canceled)
}

func TestManagerSocketPathTooLong(t *testing.T) {
	long := t.TempDir()
	for len(long) < maxSocketPath {
		long = filepath.Join(long, "subdir")
	}
	m := NewManager(long, testLogger(t))
	_, err := m.Create(context.Background(), Spec{Name: "dev", CIDR: "10.42.0.0/24"})
	require.ErrorIs(t, err, apierr.ErrInvalid)
}

func TestAttach(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	n, err := m.Create(ctx, Spec{Name: "tiny", CIDR: "10.9.9.0/29", GatewayIP: "10.9.9.2"})
	require.NoError(t, err)

	_, err = n.Attach(ctx, "")
	require.ErrorIs(t, err, apierr.ErrInvalid)

	a, err := n.Attach(ctx, "vm-a")
	require.NoError(t, err)
	assert.Equal(t, "10.9.9.1", a.IP.String(), "lowest free pool address, the gateway moved out of the way")
	assert.Equal(t, macFor("tiny", a.IP).String(), a.MAC)
	assert.Equal(t, n.SocketPath(), a.SocketPath)

	again, err := n.Attach(ctx, "vm-a")
	require.NoError(t, err)
	assert.Equal(t, a, again, "Attach is idempotent per VM")

	b, err := n.Attach(ctx, "vm-b")
	require.NoError(t, err)
	assert.Equal(t, "10.9.9.3", b.IP.String())
	c, err := n.Attach(ctx, "vm-c")
	require.NoError(t, err)
	assert.Equal(t, "10.9.9.4", c.IP.String())
	d, err := n.Attach(ctx, "vm-d")
	require.NoError(t, err)
	assert.Equal(t, "10.9.9.5", d.IP.String())

	_, err = n.Attach(ctx, "vm-e")
	require.ErrorIs(t, err, apierr.ErrConflict, "pool exhausted")

	assert.Equal(t, Stats{Attachments: 4}, n.Stats())

	require.NoError(t, n.Detach("vm-b"))
	require.ErrorIs(t, n.Detach("vm-b"), apierr.ErrNotFound)
	e, err := n.Attach(ctx, "vm-e")
	require.NoError(t, err)
	assert.Equal(t, "10.9.9.3", e.IP.String(), "released address is reused first")

	leases := n.Leases()
	require.Len(t, leases, 4)
	assert.Equal(t, []string{"vm-a", "vm-e", "vm-c", "vm-d"}, []string{leases[0].VMID, leases[1].VMID, leases[2].VMID, leases[3].VMID}, "ordered by IP")
	assert.Equal(t, macFor("tiny", e.IP).String(), leases[1].MAC)
}

func TestAttachmentQEMUArgs(t *testing.T) {
	a := Attachment{VMID: "vm-1", MAC: "02:6b:0a:2a:00:02", SocketPath: "/var/lib/vm-manager/networks/dev/qemu.sock"}
	assert.Equal(t, []string{
		"-netdev", "stream,id=net0,addr.type=unix,addr.path=/var/lib/vm-manager/networks/dev/qemu.sock,reconnect-ms=1000",
		"-device", "virtio-net-pci,netdev=net0,mac=02:6b:0a:2a:00:02",
	}, a.QEMUArgs())

	a.SocketPath = "/tmp/a,b/qemu.sock"
	assert.Contains(t, a.QEMUArgs()[1], "addr.path=/tmp/a,,b/qemu.sock", "commas are doubled for QEMU's option parser")
}

func TestRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	log := testLogger(t)

	m1 := NewManager(dir, log)
	n, err := m1.Create(ctx, Spec{Name: "dev", CIDR: "10.42.0.0/24", DNSSearchDomain: "dev.local", EnableIMDS: true})
	require.NoError(t, err)
	first, err := n.Attach(ctx, "vm-1")
	require.NoError(t, err)
	second, err := n.Attach(ctx, "vm-2")
	require.NoError(t, err)
	require.NoError(t, n.Detach("vm-1"))
	states := m1.States()
	require.NoError(t, m1.Close())
	assert.NoFileExists(t, n.SocketPath(), "Close removes the socket")

	require.Equal(t, []State{{
		Spec:   Spec{Name: "dev", CIDR: "10.42.0.0/24", DNSSearchDomain: "dev.local", EnableIMDS: true},
		Leases: map[string]string{"vm-2": "10.42.0.3"},
	}}, states, "State is plain data")

	m2 := NewManager(dir, log)
	t.Cleanup(func() { require.NoError(t, m2.Close()) })
	require.NoError(t, m2.Restore(ctx, states))

	restored, err := m2.Get("dev")
	require.NoError(t, err)
	assert.Equal(t, states[0].Spec, restored.Spec())
	assert.FileExists(t, restored.SocketPath())

	att, err := restored.Attach(ctx, "vm-2")
	require.NoError(t, err)
	assert.Equal(t, second, att, "vm-2 keeps IP and MAC across the restart")
	att, err = restored.Attach(ctx, "vm-3")
	require.NoError(t, err)
	assert.Equal(t, first.IP, att.IP, "the freed address is available again")
	assert.Equal(t, states, []State{{Spec: states[0].Spec, Leases: map[string]string{"vm-2": "10.42.0.3"}}}, "snapshot is not aliased")
}

func TestRestoreRejectsBadState(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	err := m.Restore(ctx, []State{
		{Spec: Spec{Name: "ok", CIDR: "10.1.0.0/24"}},
		{Spec: Spec{Name: "bad", CIDR: "10.2.0.0/24"}, Leases: map[string]string{"vm": "10.2.0.1"}},
	})
	require.ErrorIs(t, err, apierr.ErrInvalid)
	assert.ErrorContains(t, err, `restore network "bad"`)

	_, err = m.Get("ok")
	require.NoError(t, err, "networks before the failure stay up")
	_, err = m.Get("bad")
	require.ErrorIs(t, err, apierr.ErrNotFound)
	assert.NoDirExists(t, m.Dir("bad"), "a network that failed to come up leaves nothing behind")
}

func TestListenIMDSDisabled(t *testing.T) {
	m := newTestManager(t)
	n, err := m.Create(context.Background(), Spec{Name: "dev", CIDR: "10.42.0.0/24"})
	require.NoError(t, err)
	_, err = n.ListenIMDS()
	require.ErrorIs(t, err, apierr.ErrUnsupported)
}

func TestForwardValidation(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	n, err := m.Create(ctx, Spec{Name: "dev", CIDR: "10.42.0.0/24"})
	require.NoError(t, err)

	_, err = n.Forward(ctx, "127.0.0.1:0", "10.43.0.2", 22)
	require.ErrorIs(t, err, apierr.ErrInvalid)
	_, err = n.Forward(ctx, "127.0.0.1:0", "10.42.0.2", 0)
	require.ErrorIs(t, err, apierr.ErrInvalid)
	_, err = n.Forward(ctx, "127.0.0.1:0", "10.42.0.2", 70000)
	require.ErrorIs(t, err, apierr.ErrInvalid)
	_, err = n.Forward(ctx, "256.0.0.1:0", "10.42.0.2", 22)
	require.Error(t, err)

	fw, err := n.Forward(ctx, "127.0.0.1:0", "10.42.0.2", 22)
	require.NoError(t, err)
	assert.NotZero(t, fw.Addr().(*net.TCPAddr).Port)
	require.NoError(t, fw.Close())
	require.NoError(t, fw.Close(), "Close is idempotent")

	orphan, err := n.Forward(ctx, "127.0.0.1:0", "10.42.0.2", 22)
	require.NoError(t, err)
	require.NoError(t, n.Close())
	require.NoError(t, orphan.Close(), "closing the network took the forward down")
	_, err = net.Dial("tcp", orphan.Addr().String())
	require.Error(t, err)
	_, err = n.Forward(ctx, "127.0.0.1:0", "10.42.0.2", 22)
	require.ErrorIs(t, err, apierr.ErrConflict)
}

func TestNewManagerDefaultsLogger(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	require.NotNil(t, m.log)
}

func TestNetworkDirPermissions(t *testing.T) {
	m := newTestManager(t)
	n, err := m.Create(context.Background(), Spec{Name: "dev", CIDR: "10.42.0.0/24"})
	require.NoError(t, err)
	info, err := os.Stat(filepath.Dir(n.SocketPath()))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o750), info.Mode().Perm()&0o750)
}
