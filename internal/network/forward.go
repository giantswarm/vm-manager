package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

// PortForward exposes one guest TCP port on a host address. It is an
// io.Closer; Addr reports the bound host address, useful with port 0.
type PortForward struct {
	ln     net.Listener
	target string
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Forward listens on hostAddr ("ip:port", port 0 picks one) and proxies every
// accepted connection to vmIP:port inside the network. The forward lives
// until Close or until the network closes; ctx only bounds the listen call.
func (n *Network) Forward(ctx context.Context, hostAddr, vmIP string, port int) (*PortForward, error) {
	ip, err := netip.ParseAddr(vmIP)
	if err != nil || !n.layout.prefix.Contains(ip) {
		return nil, fmt.Errorf("%w: %q is not an address of %s", apierr.ErrInvalid, vmIP, n.layout.prefix)
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("%w: port %d out of range", apierr.ErrInvalid, port)
	}
	if n.isClosed() {
		return nil, fmt.Errorf("%w: network %s is closed", apierr.ErrConflict, n.spec.Name)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", hostAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", hostAddr, err)
	}
	fwdCtx, cancel := context.WithCancel(n.ctx)
	f := &PortForward{ln: ln, target: net.JoinHostPort(ip.String(), strconv.Itoa(port)), cancel: cancel}
	f.wg.Add(1)
	go f.serve(fwdCtx, n)
	n.log.Info("forward up", "host", ln.Addr(), "target", f.target)
	return f, nil
}

// Addr is the host address the forward listens on.
func (f *PortForward) Addr() net.Addr { return f.ln.Addr() }

// Close stops the listener and every proxied connection.
func (f *PortForward) Close() error {
	f.cancel()
	err := f.ln.Close()
	f.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (f *PortForward) serve(ctx context.Context, n *Network) {
	defer f.wg.Done()
	// Stop the listener when the network goes away, not only on Close.
	stop := context.AfterFunc(ctx, func() { _ = f.ln.Close() })
	defer stop()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.proxy(ctx, n, conn)
		}()
	}
}

func (f *PortForward) proxy(ctx context.Context, n *Network, host net.Conn) {
	defer func() { _ = host.Close() }()
	guest, err := n.vn.DialContextTCP(ctx, f.target)
	if err != nil {
		n.log.Debug("forward dial", "target", f.target, "err", err)
		return
	}
	defer func() { _ = guest.Close() }()
	stop := context.AfterFunc(ctx, func() {
		_ = host.Close()
		_ = guest.Close()
	})
	defer stop()

	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(guest, host)
	go pipe(host, guest)
	<-done
	<-done
}
