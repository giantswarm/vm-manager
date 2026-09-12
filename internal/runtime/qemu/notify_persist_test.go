package qemu

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBind stands in for the vsock bind: port 0 gets kernelPick, busy
// ports fail with EADDRINUSE, and every listener is a unix socket.
type fakeBind struct {
	t          *testing.T
	kernelPick uint32
	busy       map[uint32]bool
	bound      []uint32
}

func (f *fakeBind) listen(port uint32) (*NotifyListener, error) {
	if port == 0 {
		port = f.kernelPick
	}
	if f.busy[port] {
		return nil, &net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	}
	f.bound = append(f.bound, port)
	ln, err := net.Listen("unix", filepath.Join(f.t.TempDir(), "n.sock"))
	require.NoError(f.t, err)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newNotifyListener(ln, port, func(net.Conn) (uint32, uint32, bool) { return 0, 0, false }, quiet), nil
}

func TestListenNotifyPersistent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "notify-port")
	bind := &fakeBind{t: t, kernelPick: 1234567, busy: map[uint32]bool{}}

	// First run: the kernel picks, the pick is recorded.
	ln, err := listenNotifyPersistent(file, 0, bind.listen)
	require.NoError(t, err)
	assert.Equal(t, uint32(1234567), ln.Port())
	data, err := os.ReadFile(file) // #nosec G304 -- the test's temp file
	require.NoError(t, err)
	assert.Equal(t, "1234567\n", string(data))
	require.NoError(t, ln.Close())

	// Next run: the recorded port is reused, whatever the kernel would pick.
	bind.kernelPick = 7654321
	ln, err = listenNotifyPersistent(file, 0, bind.listen)
	require.NoError(t, err)
	assert.Equal(t, uint32(1234567), ln.Port())
	assert.Equal(t, []uint32{1234567, 1234567}, bind.bound)
	require.NoError(t, ln.Close())

	// The recorded port bound by someone else is a hard error.
	bind.busy[1234567] = true
	_, err = listenNotifyPersistent(file, 0, bind.listen)
	require.ErrorIs(t, err, ErrNotifyPortInUse)
	assert.ErrorContains(t, err, "another vm-manager")

	// An explicit port wins over the record and replaces it; a busy explicit
	// port is the plain bind error.
	ln, err = listenNotifyPersistent(file, 42, bind.listen)
	require.NoError(t, err)
	assert.Equal(t, uint32(42), ln.Port())
	data, _ = os.ReadFile(file) // #nosec G304 -- the test's temp file
	assert.Equal(t, "42\n", string(data))
	require.NoError(t, ln.Close())
	bind.busy[42] = true
	_, err = listenNotifyPersistent(file, 42, bind.listen)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNotifyPortInUse))

	// A corrupt record is reported, not ignored.
	require.NoError(t, os.WriteFile(file, []byte("nope\n"), 0o600))
	_, err = listenNotifyPersistent(file, 0, bind.listen)
	require.ErrorContains(t, err, "is not a vsock port")
}

// TestListenNotifyPersistentVsock checks the error mapping against the
// real transport: a second vm-manager on the recorded port.
func TestListenNotifyPersistentVsock(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	file := filepath.Join(t.TempDir(), "notify-port")
	first, err := ListenNotifyPersistent(file, 0, quiet)
	if err != nil {
		t.Skipf("no AF_VSOCK on this host: %v", err)
	}
	defer func() { _ = first.Close() }()
	_, err = ListenNotifyPersistent(file, 0, quiet)
	require.ErrorIs(t, err, ErrNotifyPortInUse)
}
