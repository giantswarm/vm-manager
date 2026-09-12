package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

// QEMU attachment parameters.
const (
	// SocketName is the unix socket QEMU connects to, inside the network dir.
	SocketName = "qemu.sock"
	// netdevID is the QEMU netdev/device pairing id; one NIC per VM.
	netdevID = "net0"
	// reconnectMS is how fast QEMU re-dials the socket after vm-manager
	// restarts.
	reconnectMS = 1000
	// mtu is the guest MTU announced over DHCP.
	mtu = 1500
	// loopback is what the host alias address translates to.
	loopback = "127.0.0.1"
	// maxSocketPath is the unix socket path limit minus the terminator.
	maxSocketPath = 107
	// acceptRetry is the pause after an unexpected Accept error.
	acceptRetry = 100 * time.Millisecond
)

// Attachment is a VM's seat on a network: the MAC the runtime gives the NIC,
// the IP DHCP will lease to it and the QEMU arguments that connect them.
type Attachment struct {
	VMID string     `json:"vmID"`
	MAC  string     `json:"mac"`
	IP   netip.Addr `json:"ip"`
	// SocketPath is the network's QEMU unix socket.
	SocketPath string `json:"socketPath"`
}

// QEMUArgs returns the -netdev/-device pair the runtime appends to the QEMU
// command line. QEMU dials the socket as a client and re-dials it every
// reconnect-ms while it is gone, so a vm-manager restart does not unplug the
// NIC.
func (a Attachment) QEMUArgs() []string {
	return []string{
		"-netdev", fmt.Sprintf("stream,id=%s,addr.type=unix,addr.path=%s,reconnect-ms=%d", netdevID, qemuEscape(a.SocketPath), reconnectMS),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=%s,mac=%s", netdevID, a.MAC),
	}
}

// qemuEscape doubles commas, QEMU's option separator.
func qemuEscape(s string) string {
	return strings.ReplaceAll(s, ",", ",,")
}

// Lease is one VM's address on a network.
type Lease struct {
	VMID string     `json:"vmID"`
	MAC  string     `json:"mac"`
	IP   netip.Addr `json:"ip"`
}

// Stats are the network's traffic counters.
type Stats struct {
	// BytesSent is the byte count delivered to VMs over their QEMU sockets.
	BytesSent uint64 `json:"bytesSent"`
	// BytesReceived is the byte count received from VMs over their QEMU sockets.
	BytesReceived uint64 `json:"bytesReceived"`
	// Attachments is the number of VMs holding a lease.
	Attachments int `json:"attachments"`
}

// State is everything needed to rebuild a network after a restart.
type State struct {
	Spec Spec `json:"spec"`
	// Leases maps VM id to IP address.
	Leases map[string]string `json:"leases,omitempty"`
}

// Network is one L2 segment with its gateway stack.
type Network struct {
	spec   Spec
	layout layout
	log    *slog.Logger
	dir    string
	socket string
	vn     *virtualnetwork.VirtualNetwork

	// ctx ends when the network closes; QEMU connections and forwards hang
	// off it.
	ctx    context.Context
	cancel context.CancelFunc
	ln     net.Listener
	wg     sync.WaitGroup

	// sent and received count frame bytes to and from VMs, including the
	// four-byte length prefix of every frame.
	sent, received atomic.Uint64

	mu     sync.Mutex
	leases map[string]netip.Addr
	conns  map[net.Conn]struct{}
	closed bool
}

// countingConn tallies the bytes flowing through a QEMU connection.
type countingConn struct {
	net.Conn
	read, written *atomic.Uint64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(uint64(n)) // #nosec G115 -- n is never negative
	return n, err
}

func (c countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(uint64(n)) // #nosec G115 -- n is never negative
	return n, err
}

// newNetwork validates spec and leases, builds the gateway stack and starts
// accepting QEMU connections on the network's socket under dir.
func newNetwork(ctx context.Context, dir string, spec Spec, leases map[string]string, log *slog.Logger) (*Network, error) {
	l, err := resolve(spec)
	if err != nil {
		return nil, err
	}
	restored, err := parseLeases(l, leases)
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(dir, SocketName)
	if len(socket) > maxSocketPath {
		return nil, fmt.Errorf("%w: socket path %q exceeds %d bytes", apierr.ErrInvalid, socket, maxSocketPath)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create network dir: %w", err)
	}
	vn, err := virtualnetwork.New(configuration(spec, l))
	if err != nil {
		return nil, fmt.Errorf("create virtual network: %w", err)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", socket, err)
	}
	serveCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	n := &Network{
		spec:   spec,
		layout: l,
		log:    log.With("network", spec.Name),
		dir:    dir,
		socket: socket,
		vn:     vn,
		ctx:    serveCtx,
		cancel: cancel,
		ln:     ln,
		leases: restored,
		conns:  make(map[net.Conn]struct{}),
	}
	n.wg.Add(1)
	go n.serve(serveCtx)
	n.log.Info("network up", "cidr", l.prefix, "gateway", l.gateway, "host", l.host, "socket", socket, "imds", spec.EnableIMDS)
	return n, nil
}

// configuration builds the gvisor-tap-vsock configuration: the gateway with
// its virtual IPs, NAT of the host alias to loopback and the complete static
// lease table.
func configuration(spec Spec, l layout) *types.Configuration {
	static := make(map[string]string, l.poolSize()+2)
	l.eachPool(func(ip netip.Addr) bool {
		static[ip.String()] = macFor(spec.Name, ip).String()
		return true
	})
	// The library reserves the gateway itself but would lease the host alias
	// and even the broadcast address to a MAC it does not know; block both.
	static[l.host.String()] = macFor(spec.Name, l.host).String()
	static[l.broadcast.String()] = macFor(spec.Name, l.broadcast).String()
	virtualIPs := []string{l.host.String()}
	if spec.EnableIMDS {
		virtualIPs = append(virtualIPs, IMDSIP)
	}
	cfg := &types.Configuration{
		MTU:               mtu,
		Subnet:            l.prefix.String(),
		GatewayIP:         l.gateway.String(),
		GatewayMacAddress: macFor(spec.Name, l.gateway).String(),
		NAT:               map[string]string{l.host.String(): loopback},
		GatewayVirtualIPs: virtualIPs,
		DHCPStaticLeases:  static,
		Protocol:          types.QemuProtocol,
	}
	if spec.DNSSearchDomain != "" {
		cfg.DNSSearchDomains = []string{spec.DNSSearchDomain}
	}
	return cfg
}

// parseLeases checks restored leases against the pool: every IP must be a
// pool address and held by one VM only.
func parseLeases(l layout, leases map[string]string) (map[string]netip.Addr, error) {
	out := make(map[string]netip.Addr, len(leases))
	used := make(map[netip.Addr]string, len(leases))
	for vmID, s := range leases {
		if vmID == "" {
			return nil, fmt.Errorf("%w: lease for %s has an empty vm id", apierr.ErrInvalid, s)
		}
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("%w: lease for %s: %v", apierr.ErrInvalid, vmID, err)
		}
		if !l.poolContains(ip) {
			return nil, fmt.Errorf("%w: lease %s for %s is outside the pool of %s", apierr.ErrInvalid, ip, vmID, l.prefix)
		}
		if other, dup := used[ip]; dup {
			return nil, fmt.Errorf("%w: lease %s held by both %s and %s", apierr.ErrInvalid, ip, other, vmID)
		}
		used[ip] = vmID
		out[vmID] = ip
	}
	return out, nil
}

// serve accepts QEMU connections until the network closes and plugs each one
// into the switch.
func (n *Network) serve(ctx context.Context) {
	defer n.wg.Done()
	for {
		raw, err := n.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			n.log.Error("accept qemu connection", "err", err)
			time.Sleep(acceptRetry)
			continue
		}
		conn := countingConn{Conn: raw, read: &n.received, written: &n.sent}
		if !n.track(conn) {
			_ = conn.Close()
			return
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			defer n.untrack(conn)
			n.log.Debug("qemu connected")
			if err := n.vn.AcceptQemu(ctx, conn); err != nil && ctx.Err() == nil {
				n.log.Debug("qemu disconnected", "err", err)
			}
		}()
	}
}

func (n *Network) track(conn net.Conn) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return false
	}
	n.conns[conn] = struct{}{}
	return true
}

func (n *Network) untrack(conn net.Conn) {
	n.mu.Lock()
	delete(n.conns, conn)
	n.mu.Unlock()
	_ = conn.Close()
}

// Name is the network name.
func (n *Network) Name() string { return n.spec.Name }

// Spec is the network's specification.
func (n *Network) Spec() Spec { return n.spec }

// SocketPath is the unix socket QEMU connects to.
func (n *Network) SocketPath() string { return n.socket }

// GatewayIP is the gateway, DHCP and DNS address of the network.
func (n *Network) GatewayIP() netip.Addr { return n.layout.gateway }

// HostIP is the address on which guests reach the host's loopback services.
func (n *Network) HostIP() netip.Addr { return n.layout.host }

// Prefix is the network's CIDR.
func (n *Network) Prefix() netip.Prefix { return n.layout.prefix }

// State is a snapshot for persistence.
func (n *Network) State() State {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := State{Spec: n.spec, Leases: make(map[string]string, len(n.leases))}
	for vmID, ip := range n.leases {
		s.Leases[vmID] = ip.String()
	}
	return s
}

// Attach gives vmID the lowest free pool address; a VM that already holds a
// lease gets it back. It fails with apierr.ErrConflict when the pool is
// exhausted or the network is closed.
func (n *Network) Attach(ctx context.Context, vmID string) (*Attachment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if vmID == "" {
		return nil, fmt.Errorf("%w: vm id is empty", apierr.ErrInvalid)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, fmt.Errorf("%w: network %s is closed", apierr.ErrConflict, n.spec.Name)
	}
	ip, ok := n.leases[vmID]
	if !ok {
		ip, ok = n.freeAddrLocked()
		if !ok {
			return nil, fmt.Errorf("%w: network %s has no free address", apierr.ErrConflict, n.spec.Name)
		}
		n.leases[vmID] = ip
		n.log.Info("attached", "vm", vmID, "ip", ip, "mac", macFor(n.spec.Name, ip))
	}
	return n.attachment(vmID, ip), nil
}

func (n *Network) freeAddrLocked() (netip.Addr, bool) {
	used := make(map[netip.Addr]struct{}, len(n.leases))
	for _, ip := range n.leases {
		used[ip] = struct{}{}
	}
	var free netip.Addr
	n.layout.eachPool(func(ip netip.Addr) bool {
		if _, taken := used[ip]; taken {
			return true
		}
		free = ip
		return false
	})
	return free, free.IsValid()
}

func (n *Network) attachment(vmID string, ip netip.Addr) *Attachment {
	return &Attachment{VMID: vmID, MAC: macFor(n.spec.Name, ip).String(), IP: ip, SocketPath: n.socket}
}

// Detach releases vmID's address. The QEMU connection, if any, is unaffected;
// stop the VM first.
func (n *Network) Detach(vmID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	ip, ok := n.leases[vmID]
	if !ok {
		return fmt.Errorf("%w: vm %s is not attached to %s", apierr.ErrNotFound, vmID, n.spec.Name)
	}
	delete(n.leases, vmID)
	n.log.Info("detached", "vm", vmID, "ip", ip)
	return nil
}

// Leases lists the attached VMs ordered by address.
func (n *Network) Leases() []Lease {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Lease, 0, len(n.leases))
	for vmID, ip := range n.leases {
		out = append(out, Lease{VMID: vmID, MAC: macFor(n.spec.Name, ip).String(), IP: ip})
	}
	slices.SortFunc(out, func(a, b Lease) int { return a.IP.Compare(b.IP) })
	return out
}

// Stats returns the traffic counters.
func (n *Network) Stats() Stats {
	n.mu.Lock()
	attached := len(n.leases)
	n.mu.Unlock()
	return Stats{BytesSent: n.sent.Load(), BytesReceived: n.received.Load(), Attachments: attached}
}

// Dial opens a TCP connection from the host into the network; addr is
// "ip:port" of a guest.
func (n *Network) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if n.isClosed() {
		return nil, fmt.Errorf("%w: network %s is closed", apierr.ErrConflict, n.spec.Name)
	}
	return n.vn.DialContextTCP(ctx, addr)
}

// ListenIMDS returns a TCP listener on 169.254.169.254:80 inside the network.
// Accepted connections report the guest's IP as RemoteAddr, which is how the
// metadata service identifies the VM. The listener is bound to the IMDS IP
// itself (not the unspecified address), so it never shadows other services
// on the gateway; the gateway stack accepts it because it runs with address
// spoofing enabled and answers ARP for the IMDS IP as a virtual address.
func (n *Network) ListenIMDS() (net.Listener, error) {
	if !n.spec.EnableIMDS {
		return nil, fmt.Errorf("%w: network %s has IMDS disabled", apierr.ErrUnsupported, n.spec.Name)
	}
	if n.isClosed() {
		return nil, fmt.Errorf("%w: network %s is closed", apierr.ErrConflict, n.spec.Name)
	}
	return n.vn.Listen("tcp", IMDSAddress)
}

func (n *Network) isClosed() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.closed
}

// Close stops accepting QEMU connections, drops the existing ones and removes
// the socket. Leases are kept so State stays valid for persistence.
func (n *Network) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	conns := make([]net.Conn, 0, len(n.conns))
	for c := range n.conns {
		conns = append(conns, c)
	}
	n.mu.Unlock()

	n.cancel()
	err := n.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	n.wg.Wait()
	if rmErr := os.Remove(n.socket); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		err = errors.Join(err, rmErr)
	}
	n.log.Info("network down")
	return err
}
