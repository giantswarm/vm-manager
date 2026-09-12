package qemu

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdlayher/vsock"
)

// HostCID is the vsock context id guests reach the host on.
const HostCID = vsock.Host

// cidAny is VMADDR_CID_ANY: accept connections addressed to any of the
// host's context ids (2 from guests, 1 over loopback in tests).
const cidAny = math.MaxUint32

const (
	// notifyMaxBytes bounds one notification message.
	notifyMaxBytes = 64 * 1024
	// notifyReadTimeout bounds how long a guest may take to send and close.
	notifyReadTimeout = 5 * time.Second
	// notifyBuffer is how many notifications queue per CID before drops.
	notifyBuffer = 16
	// privilegedPorts is the first unprivileged vsock port: systemd's PID 1
	// sends from below it, so a notification from a higher port did not
	// come from the service manager.
	privilegedPorts = 1024
	// acceptRetry is the pause after a transient accept failure.
	acceptRetry = 100 * time.Millisecond
)

// Notification is one sd_notify message a guest sent to the
// vmm.notify_socket address.
type Notification struct {
	// CID is the sending guest's context id, Port its source port.
	CID  uint32
	Port uint32
	// Fields are the KEY=VALUE lines of the message, last value per key.
	Fields map[string]string
}

// Ready is READY=1: the guest's service manager finished booting.
func (n Notification) Ready() bool { return n.Fields["READY"] == "1" }

// Status is the STATUS= text, if any.
func (n Notification) Status() string { return n.Fields["STATUS"] }

// Privileged is true when the message came from a port below 1024, which
// only the guest's PID 1 uses.
func (n Notification) Privileged() bool { return n.Port < privilegedPorts }

// ParseNotify parses the sd_notify wire format: newline-separated KEY=VALUE
// assignments. Lines without "=" or with an empty key are ignored; a key
// repeated later wins.
func ParseNotify(data []byte) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			continue
		}
		fields[key] = value
	}
	return fields
}

// NotifyListener receives sd_notify messages from guests over AF_VSOCK and
// routes them by sending CID. One listener serves every VM: each guest gets
// Credential as its vmm.notify_socket and the caller reads Subscribe(cid).
//
// The address is vsock-stream: virtio-vsock transports offer SOCK_STREAM
// everywhere while SOCK_DGRAM depends on the guest kernel, systemd-vmspawn
// uses the same, and PID 1 then opens one connection per message, writes
// it and closes, so a message is complete at EOF.
type NotifyListener struct {
	ln      net.Listener
	port    uint32
	peer    func(net.Conn) (cid, port uint32, ok bool)
	log     *slog.Logger
	mu      sync.Mutex
	subs    map[uint32]chan Notification
	closed  bool
	dropped atomic.Uint64
	wg      sync.WaitGroup
}

// ListenNotify binds a vsock stream listener on every host context id; port
// 0 lets the kernel pick one. Logger nil uses slog.Default().
func ListenNotify(port uint32, log *slog.Logger) (*NotifyListener, error) {
	ln, err := vsock.ListenContextID(cidAny, port, nil)
	if err != nil {
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	addr, ok := ln.Addr().(*vsock.Addr)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("vsock listen: unexpected address %v", ln.Addr())
	}
	return newNotifyListener(ln, addr.Port, vsockPeer, log), nil
}

// vsockPeer reads the guest's CID and port from a vsock connection.
func vsockPeer(c net.Conn) (uint32, uint32, bool) {
	a, ok := c.RemoteAddr().(*vsock.Addr)
	if !ok {
		return 0, 0, false
	}
	return a.ContextID, a.Port, true
}

// newNotifyListener serves ln, identifying peers with peer; tests pass a
// unix listener and a fake peer.
func newNotifyListener(ln net.Listener, port uint32, peer func(net.Conn) (uint32, uint32, bool), log *slog.Logger) *NotifyListener {
	if log == nil {
		log = slog.Default()
	}
	l := &NotifyListener{ln: ln, port: port, peer: peer, log: log, subs: map[uint32]chan Notification{}}
	l.wg.Add(1)
	go l.accept()
	return l
}

// Port is the bound vsock port.
func (l *NotifyListener) Port() uint32 { return l.port }

// Credential is the vmm.notify_socket value for guests: vsock-stream:2:PORT.
func (l *NotifyListener) Credential() string {
	return fmt.Sprintf("vsock-stream:%d:%d", HostCID, l.port)
}

// Subscribe returns the channel notifications from cid are delivered on;
// repeated calls return the same channel. Subscribe before the guest boots
// so nothing is missed; the channel is closed by Unsubscribe or Close.
func (l *NotifyListener) Subscribe(cid uint32) <-chan Notification {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch, ok := l.subs[cid]; ok {
		return ch
	}
	ch := make(chan Notification, notifyBuffer)
	l.subs[cid] = ch
	return ch
}

// Unsubscribe stops delivery for cid and closes its channel.
func (l *NotifyListener) Unsubscribe(cid uint32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch, ok := l.subs[cid]; ok {
		delete(l.subs, cid)
		close(ch)
	}
}

// Dropped counts notifications discarded because a subscriber's channel
// was full.
func (l *NotifyListener) Dropped() uint64 { return l.dropped.Load() }

// Close stops accepting, waits for in-flight messages and closes every
// subscription channel.
func (l *NotifyListener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	err := l.ln.Close()
	l.wg.Wait()
	l.mu.Lock()
	for cid, ch := range l.subs {
		delete(l.subs, cid)
		close(ch)
	}
	l.mu.Unlock()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (l *NotifyListener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func (l *NotifyListener) accept() {
	defer l.wg.Done()
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if l.isClosed() || errors.Is(err, net.ErrClosed) {
				return
			}
			l.log.Warn("notify accept failed", "error", err)
			time.Sleep(acceptRetry)
			continue
		}
		l.wg.Add(1)
		go l.handle(conn)
	}
}

// handle reads one message (until the guest closes) and routes it.
func (l *NotifyListener) handle(conn net.Conn) {
	defer l.wg.Done()
	defer func() { _ = conn.Close() }()
	cid, port, ok := l.peer(conn)
	if !ok {
		l.log.Warn("notify connection without a vsock peer address dropped", "remote", conn.RemoteAddr())
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(notifyReadTimeout))
	data, err := io.ReadAll(io.LimitReader(conn, notifyMaxBytes))
	if err != nil {
		l.log.Warn("notify message from guest incomplete", "cid", cid, "error", err)
		return
	}
	l.deliver(Notification{CID: cid, Port: port, Fields: ParseNotify(data)})
}

func (l *NotifyListener) deliver(n Notification) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ch, ok := l.subs[n.CID]
	if !ok {
		l.log.Debug("notification from unsubscribed cid dropped", "cid", n.CID, "fields", n.Fields)
		return
	}
	select {
	case ch <- n:
	default:
		l.dropped.Add(1)
		l.log.Warn("notification dropped, subscriber not reading", "cid", n.CID)
	}
}
