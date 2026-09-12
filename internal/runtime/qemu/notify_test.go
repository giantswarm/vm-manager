package qemu

import (
	"net"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNotify(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{name: "empty", in: "", want: map[string]string{}},
		{name: "ready", in: "READY=1\n", want: map[string]string{"READY": "1"}},
		{name: "ready with status and hostname", in: "READY=1\nSTATUS=Ready.\nX_SYSTEMD_HOSTNAME=node-1\n",
			want: map[string]string{"READY": "1", "STATUS": "Ready.", "X_SYSTEMD_HOSTNAME": "node-1"}},
		{name: "value with equals", in: "STATUS=a=b\n", want: map[string]string{"STATUS": "a=b"}},
		{name: "empty value", in: "STATUS=\n", want: map[string]string{"STATUS": ""}},
		{name: "last wins", in: "STATUS=one\nSTATUS=two\n", want: map[string]string{"STATUS": "two"}},
		{name: "garbage skipped", in: "noequals\n=novalue\n\nREADY=1", want: map[string]string{"READY": "1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ParseNotify([]byte(tc.in)))
		})
	}
	n := Notification{Port: 42, Fields: ParseNotify([]byte("READY=1\nSTATUS=up\n"))}
	assert.True(t, n.Ready())
	assert.Equal(t, "up", n.Status())
	assert.True(t, n.Privileged())
	assert.False(t, Notification{Port: 1024}.Privileged())
	assert.False(t, Notification{Fields: map[string]string{"READY": "0"}}.Ready())
}

// unixNotify is a NotifyListener over a unix socket whose peer identity the
// test sets through cid and port before each connection.
type unixNotify struct {
	*NotifyListener
	path string
	cid  atomic.Uint32
	port atomic.Uint32
}

func startUnixNotify(t *testing.T) *unixNotify {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	u := &unixNotify{path: path}
	u.NotifyListener = newNotifyListener(ln, 40001, func(net.Conn) (uint32, uint32, bool) {
		return u.cid.Load(), u.port.Load(), true
	}, quiet)
	t.Cleanup(func() { _ = u.Close() })
	return u
}

// send connects as cid:port, writes msg and closes, like sd_notify does.
func (u *unixNotify) send(t *testing.T, cid, port uint32, msg string) {
	t.Helper()
	u.cid.Store(cid)
	u.port.Store(port)
	c, err := net.Dial("unix", u.path)
	require.NoError(t, err)
	_, err = c.Write([]byte(msg))
	require.NoError(t, err)
	require.NoError(t, c.Close())
}

func receive(t *testing.T, ch <-chan Notification) Notification {
	t.Helper()
	select {
	case n, ok := <-ch:
		require.True(t, ok, "channel closed")
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
		return Notification{}
	}
}

func TestNotifyListener(t *testing.T) {
	t.Run("routes by cid", func(t *testing.T) {
		u := startUnixNotify(t)
		assert.Equal(t, uint32(40001), u.Port())
		assert.Equal(t, "vsock-stream:2:40001", u.Credential())
		a, b := u.Subscribe(4001), u.Subscribe(4002)
		assert.Equal(t, a, u.Subscribe(4001), "subscribing twice returns the same channel")

		u.send(t, 4001, 500, "READY=1\nSTATUS=Ready.\n")
		n := receive(t, a)
		assert.Equal(t, uint32(4001), n.CID)
		assert.True(t, n.Ready())
		assert.True(t, n.Privileged())
		assert.Equal(t, "Ready.", n.Status())

		u.send(t, 4002, 3000, "STATUS=Starting...\n")
		n = receive(t, b)
		assert.Equal(t, uint32(4002), n.CID)
		assert.False(t, n.Ready())
		assert.False(t, n.Privileged())
		assert.Len(t, a, 0)
	})
	t.Run("unsubscribed cid is dropped without blocking", func(t *testing.T) {
		u := startUnixNotify(t)
		a := u.Subscribe(4001)
		u.send(t, 9, 500, "READY=1\n")
		u.send(t, 4001, 500, "READY=1\n")
		assert.True(t, receive(t, a).Ready())
		assert.Zero(t, u.Dropped())
	})
	t.Run("slow subscriber loses excess", func(t *testing.T) {
		u := startUnixNotify(t)
		a := u.Subscribe(4001)
		for range notifyBuffer + 2 {
			u.send(t, 4001, 500, "STATUS=x\n")
		}
		assert.Eventually(t, func() bool { return u.Dropped() == 2 }, 5*time.Second, 10*time.Millisecond)
		assert.Len(t, a, notifyBuffer)
	})
	t.Run("unsubscribe and close end the channels", func(t *testing.T) {
		u := startUnixNotify(t)
		a, b := u.Subscribe(1), u.Subscribe(2)
		u.Unsubscribe(1)
		u.Unsubscribe(1)
		_, ok := <-a
		assert.False(t, ok)
		require.NoError(t, u.Close())
		_, ok = <-b
		assert.False(t, ok)
		require.NoError(t, u.Close(), "closing twice is fine")
	})
	t.Run("peer without vsock address is ignored", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "n.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		l := newNotifyListener(ln, 1, func(net.Conn) (uint32, uint32, bool) { return 0, 0, false }, quiet)
		t.Cleanup(func() { _ = l.Close() })
		a := l.Subscribe(0)
		c, err := net.Dial("unix", path)
		require.NoError(t, err)
		_, _ = c.Write([]byte("READY=1\n"))
		require.NoError(t, c.Close())
		require.NoError(t, l.Close())
		_, ok := <-a
		assert.False(t, ok)
	})
}

// TestNotifyListenerVsock binds a real AF_VSOCK port and, where the kernel
// has the loopback transport (vsock_loopback), sends itself a message.
func TestNotifyListenerVsock(t *testing.T) {
	l, err := ListenNotify(0, quiet)
	if err != nil {
		t.Skipf("AF_VSOCK unavailable: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	assert.NotZero(t, l.Port())
	assert.Equal(t, "vsock-stream:2:"+strconv.FormatUint(uint64(l.Port()), 10), l.Credential())

	ch := l.Subscribe(vsock.Local)
	c, err := vsock.Dial(vsock.Local, l.Port(), nil)
	if err != nil {
		t.Skipf("vsock loopback unavailable (vsock_loopback not loaded): %v", err)
	}
	_, err = c.Write([]byte("READY=1\nSTATUS=loopback\n"))
	require.NoError(t, err)
	require.NoError(t, c.Close())
	n := receive(t, ch)
	assert.Equal(t, uint32(vsock.Local), n.CID)
	assert.True(t, n.Ready())
	assert.Equal(t, "loopback", n.Status())
	assert.False(t, n.Privileged(), "an unprivileged test process gets an ephemeral port")
}
