package network

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// fakeVM is a guest without KVM: a gVisor stack whose Ethernet frames travel
// over the network's QEMU socket with the -netdev stream framing (32-bit
// big-endian length, then the frame), exactly what QEMU sends. It is
// configured the way the real guest ends up after DHCP: the leased IP and
// MAC, a default route through the gateway and an on-link route to the IMDS
// address as systemd-imdsd installs it.
type fakeVM struct {
	stack  *stack.Stack
	link   *channel.Endpoint
	conn   net.Conn
	ip     netip.Addr
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// hangup closes when the network side closed the socket.
	hangup chan struct{}
}

const (
	fakeNIC     = tcpip.NICID(1)
	frameQueue  = 128
	dhcpTimeout = 300 * time.Millisecond
)

func newFakeVM(t *testing.T, n *Network, att *Attachment) *fakeVM {
	t.Helper()
	mac, err := net.ParseMAC(att.MAC)
	require.NoError(t, err)

	link := channel.New(frameQueue, mtu, tcpip.LinkAddress(mac))
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	require.Nil(t, s.CreateNIC(fakeNIC, ethernet.New(link)))
	require.Nil(t, s.AddProtocolAddress(fakeNIC, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addrOf(att.IP), PrefixLen: n.Prefix().Bits()},
	}, stack.AddressProperties{}))
	subnet, err := tcpip.NewSubnet(addrOf(n.Prefix().Addr()), tcpip.MaskFromBytes(net.CIDRMask(n.Prefix().Bits(), 32)))
	require.NoError(t, err)
	s.SetRouteTable([]tcpip.Route{
		{Destination: subnet, NIC: fakeNIC},
		{Destination: addrOf(netip.MustParseAddr(IMDSIP)).WithPrefix().Subnet(), NIC: fakeNIC},
		{Destination: header.IPv4EmptySubnet, Gateway: addrOf(n.GatewayIP()), NIC: fakeNIC},
	})

	conn, err := net.Dial("unix", att.SocketPath)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	vm := &fakeVM{stack: s, link: link, conn: conn, ip: att.IP, cancel: cancel, hangup: make(chan struct{})}
	vm.wg.Add(2)
	go vm.pumpOut(ctx)
	go vm.pumpIn()
	t.Cleanup(vm.close)
	return vm
}

func (vm *fakeVM) close() {
	vm.cancel()
	_ = vm.conn.Close()
	vm.wg.Wait()
	vm.stack.Destroy()
}

// pumpOut sends frames the stack emits to the network socket.
func (vm *fakeVM) pumpOut(ctx context.Context) {
	defer vm.wg.Done()
	var size [4]byte
	for {
		pkt := vm.link.ReadContext(ctx)
		if pkt == nil {
			return
		}
		frame := pkt.ToView().AsSlice()
		binary.BigEndian.PutUint32(size[:], uint32(len(frame))) // #nosec G115 -- frames are MTU sized
		_, err := vm.conn.Write(append(size[:], frame...))
		pkt.DecRef()
		if err != nil {
			return
		}
	}
}

// pumpIn injects frames from the network socket into the stack.
func (vm *fakeVM) pumpIn() {
	defer vm.wg.Done()
	defer close(vm.hangup)
	r := bufio.NewReader(vm.conn)
	var size [4]byte
	for {
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return
		}
		frame := make([]byte, binary.BigEndian.Uint32(size[:]))
		if _, err := io.ReadFull(r, frame); err != nil {
			return
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
		vm.link.InjectInbound(header.IPv4ProtocolNumber, pkt)
		pkt.DecRef()
	}
}

// dial opens a TCP connection from the guest.
func (vm *fakeVM) dial(ctx context.Context, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, err
	}
	return gonet.DialContextTCP(ctx, vm.stack, tcpip.FullAddress{NIC: fakeNIC, Addr: addrOf(netip.MustParseAddr(host)), Port: uint16(port)}, ipv4.ProtocolNumber)
}

// httpClient talks HTTP from inside the guest.
func (vm *fakeVM) httpClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext:       func(ctx context.Context, _, addr string) (net.Conn, error) { return vm.dial(ctx, addr) },
			DisableKeepAlives: true,
		},
	}
}

// echoListener serves a TCP echo on the guest's port.
func (vm *fakeVM) echoListener(t *testing.T, port uint16) net.Listener {
	t.Helper()
	ln, err := gonet.ListenTCP(vm.stack, tcpip.FullAddress{NIC: fakeNIC, Addr: addrOf(vm.ip), Port: port}, ipv4.ProtocolNumber)
	require.NoError(t, err)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// dhcpConn is a UDP socket on the DHCP client port that may broadcast.
func (vm *fakeVM) dhcpConn(t *testing.T) *gonet.UDPConn {
	t.Helper()
	var wq waiter.Queue
	ep, tcpErr := vm.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	require.Nil(t, tcpErr)
	ep.SocketOptions().SetBroadcast(true)
	require.Nil(t, ep.Bind(tcpip.FullAddress{NIC: fakeNIC, Addr: addrOf(vm.ip), Port: dhcpv4.ClientPort}))
	c := gonet.NewUDPConn(&wq, ep)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// dhcpExchange broadcasts msg and returns the server's reply, or nil when
// none arrives within dhcpTimeout.
func dhcpExchange(t *testing.T, c *gonet.UDPConn, msg *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
	t.Helper()
	_, err := c.WriteTo(msg.ToBytes(), &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpv4.ServerPort})
	require.NoError(t, err)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(dhcpTimeout)))
	buf := make([]byte, mtu)
	n, _, err := c.ReadFrom(buf)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return nil
	}
	require.NoError(t, err)
	reply, err := dhcpv4.FromBytes(buf[:n])
	require.NoError(t, err)
	return reply
}

func addrOf(ip netip.Addr) tcpip.Address {
	return tcpip.AddrFrom4(ip.As4())
}

func echo(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
	_, err := io.WriteString(c, msg)
	require.NoError(t, err)
	buf := make([]byte, len(msg))
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, msg, string(buf))
}

func get(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url) // #nosec G107 -- test URLs
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	return string(body)
}

// TestFakeVM drives a complete network with two in-process guests: IMDS
// inside the stack, NAT to the host, host-to-guest dialing, port forwards,
// guest-to-guest switching and the DHCP lease table.
func TestFakeVM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m := newTestManager(t)
	n, err := m.Create(ctx, Spec{Name: "lab", CIDR: "10.77.0.0/24", DNSSearchDomain: "lab.internal", EnableIMDS: true, EnableHostAlias: true})
	require.NoError(t, err)

	att1, err := n.Attach(ctx, "vm-1")
	require.NoError(t, err)
	att2, err := n.Attach(ctx, "vm-2")
	require.NoError(t, err)

	// IMDS: an in-stack listener on 169.254.169.254:80 sees the guest's IP.
	var (
		mu     sync.Mutex
		remote []string
	)
	imds, err := n.ListenIMDS()
	require.NoError(t, err)
	imdsSrv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		mu.Lock()
		remote = append(remote, host)
		mu.Unlock()
		_, _ = io.WriteString(w, "vm-hostname")
	})}
	go func() { _ = imdsSrv.Serve(imds) }()
	t.Cleanup(func() { _ = imdsSrv.Close() })

	vm1 := newFakeVM(t, n, att1)
	vm2 := newFakeVM(t, n, att2)

	t.Run("imds", func(t *testing.T) {
		body := get(t, vm1.httpClient(), "http://"+IMDSAddress+"/giantswarm/v1/hostname")
		assert.Equal(t, "vm-hostname", body)
		body = get(t, vm2.httpClient(), "http://"+IMDSAddress+"/giantswarm/v1/hostname")
		assert.Equal(t, "vm-hostname", body)
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, []string{att1.IP.String(), att2.IP.String()}, remote, "the metadata service identifies VMs by source IP")
	})

	t.Run("nat to host loopback", func(t *testing.T) {
		hostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello from the host")
		}))
		t.Cleanup(hostSrv.Close)
		_, port, err := net.SplitHostPort(hostSrv.Listener.Addr().String())
		require.NoError(t, err)
		body := get(t, vm1.httpClient(), "http://"+net.JoinHostPort(n.HostIP().String(), port)+"/")
		assert.Equal(t, "hello from the host", body)
	})

	const echoPort = 2222
	vm1.echoListener(t, echoPort)

	t.Run("host dials guest", func(t *testing.T) {
		c, err := n.Dial(ctx, net.JoinHostPort(att1.IP.String(), strconv.Itoa(echoPort)))
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		echo(t, c, "ping from the host")
	})

	t.Run("guest dials guest", func(t *testing.T) {
		c, err := vm2.dial(ctx, net.JoinHostPort(att1.IP.String(), strconv.Itoa(echoPort)))
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		echo(t, c, "ping across the switch")
	})

	t.Run("forward", func(t *testing.T) {
		fw, err := n.Forward(ctx, "127.0.0.1:0", att1.IP.String(), echoPort)
		require.NoError(t, err)
		defer func() { require.NoError(t, fw.Close()) }()
		c, err := net.Dial("tcp", fw.Addr().String())
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		echo(t, c, "ping through the forward")
	})

	t.Run("dhcp", func(t *testing.T) {
		mac, err := net.ParseMAC(att1.MAC)
		require.NoError(t, err)
		c := vm1.dhcpConn(t)

		discover, err := dhcpv4.NewDiscovery(mac)
		require.NoError(t, err)
		offer := dhcpExchange(t, c, discover)
		require.NotNil(t, offer, "no OFFER")
		assert.Equal(t, dhcpv4.MessageTypeOffer, offer.MessageType())
		assert.Equal(t, att1.IP.String(), offer.YourIPAddr.String(), "the static lease wins")
		assert.Equal(t, []net.IP{net.ParseIP(n.GatewayIP().String()).To4()}, offer.Router())
		assert.Equal(t, []net.IP{net.ParseIP(n.GatewayIP().String()).To4()}, offer.DNS())
		assert.Equal(t, net.IPMask(net.CIDRMask(24, 32)), offer.SubnetMask())
		assert.Equal(t, []string{"lab.internal"}, offer.DomainSearch().Labels)

		request, err := dhcpv4.NewRequestFromOffer(offer)
		require.NoError(t, err)
		ack := dhcpExchange(t, c, request)
		require.NotNil(t, ack, "no ACK")
		assert.Equal(t, dhcpv4.MessageTypeAck, ack.MessageType())
		assert.Equal(t, att1.IP.String(), ack.YourIPAddr.String())

		stranger, err := dhcpv4.NewDiscovery(net.HardwareAddr{0x02, 0xff, 0xde, 0xad, 0xbe, 0xef})
		require.NoError(t, err)
		assert.Nil(t, dhcpExchange(t, c, stranger), "a MAC without a lease gets no offer")
	})

	stats := n.Stats()
	assert.Equal(t, 2, stats.Attachments)
	assert.Positive(t, stats.BytesSent)
	assert.Positive(t, stats.BytesReceived)

	// Closing the network hangs up on the guests; a real QEMU would re-dial.
	require.NoError(t, n.Detach("vm-1"))
	require.NoError(t, n.Detach("vm-2"))
	require.NoError(t, m.Delete(ctx, "lab"))
	select {
	case <-vm1.hangup:
	case <-ctx.Done():
		t.Fatal("the QEMU side did not see the socket close")
	}
}
