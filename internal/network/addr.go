package network

import (
	"fmt"
	"hash/fnv"
	"net"
	"net/netip"
	"regexp"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

// IMDS addressing. The guest reaches the metadata service on IMDSAddress; the
// gateway answers ARP for IMDSIP so the guest's on-link route works.
const (
	IMDSIP      = "169.254.169.254"
	IMDSPort    = 80
	IMDSAddress = "169.254.169.254:80"
)

// Prefix length bounds: a /29 leaves four pool addresses, a /16 is the
// largest static lease table worth pre-registering (65k entries).
const (
	minPrefixBits = 16
	maxPrefixBits = 29
)

// nameRE is a DNS label: the name doubles as a directory name and may become
// part of a search domain.
var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// linkLocal is never a valid network CIDR: 169.254.0.0/16 is reserved for the
// IMDS virtual IP and the gateway drops traffic to it.
var linkLocal = netip.MustParsePrefix("169.254.0.0/16")

// Spec describes a network. It is plain data and serializes as-is.
type Spec struct {
	// Name identifies the network; a DNS label of at most 63 characters.
	Name string `json:"name"`
	// CIDR is the IPv4 subnet, /16 to /29, outside 169.254.0.0/16.
	CIDR string `json:"cidr"`
	// GatewayIP overrides the gateway address (default: first host address).
	GatewayIP string `json:"gatewayIP,omitempty"`
	// DNSSearchDomain is handed to guests in the DHCP reply.
	DNSSearchDomain string `json:"dnsSearchDomain,omitempty"`
	// EnableIMDS makes 169.254.169.254 reachable from guests and allows
	// ListenIMDS; without it, connections to the IMDS address are refused.
	EnableIMDS bool `json:"enableIMDS"`
}

// layout is the resolved addressing of a Spec.
type layout struct {
	prefix    netip.Prefix
	network   netip.Addr
	gateway   netip.Addr
	host      netip.Addr
	broadcast netip.Addr
}

// resolve validates spec and computes its addressing.
func resolve(spec Spec) (layout, error) {
	if !nameRE.MatchString(spec.Name) {
		return layout{}, fmt.Errorf("%w: name %q must be a DNS label", apierr.ErrInvalid, spec.Name)
	}
	prefix, err := netip.ParsePrefix(spec.CIDR)
	if err != nil {
		return layout{}, fmt.Errorf("%w: cidr %q: %v", apierr.ErrInvalid, spec.CIDR, err)
	}
	if !prefix.Addr().Is4() {
		return layout{}, fmt.Errorf("%w: cidr %q must be IPv4", apierr.ErrInvalid, spec.CIDR)
	}
	if prefix.Bits() < minPrefixBits || prefix.Bits() > maxPrefixBits {
		return layout{}, fmt.Errorf("%w: cidr %q must be between /%d and /%d", apierr.ErrInvalid, spec.CIDR, minPrefixBits, maxPrefixBits)
	}
	prefix = prefix.Masked()
	if prefix.Overlaps(linkLocal) {
		return layout{}, fmt.Errorf("%w: cidr %q overlaps the link-local range reserved for IMDS", apierr.ErrInvalid, spec.CIDR)
	}
	l := layout{
		prefix:    prefix,
		network:   prefix.Addr(),
		broadcast: lastAddr(prefix),
	}
	l.host = l.broadcast.Prev()
	l.gateway = l.network.Next()
	if spec.GatewayIP != "" {
		gw, err := netip.ParseAddr(spec.GatewayIP)
		if err != nil {
			return layout{}, fmt.Errorf("%w: gateway %q: %v", apierr.ErrInvalid, spec.GatewayIP, err)
		}
		if !prefix.Contains(gw) || gw == l.network || gw == l.broadcast || gw == l.host {
			return layout{}, fmt.Errorf("%w: gateway %q must be a host address of %s other than %s", apierr.ErrInvalid, spec.GatewayIP, prefix, l.host)
		}
		l.gateway = gw
	}
	return l, nil
}

// poolContains reports whether ip is an address Attach may hand out.
func (l layout) poolContains(ip netip.Addr) bool {
	return l.prefix.Contains(ip) && ip != l.network && ip != l.gateway && ip != l.host && ip != l.broadcast
}

// poolSize is the number of addresses Attach may hand out.
func (l layout) poolSize() int {
	return 1<<(32-l.prefix.Bits()) - 4
}

// eachPool calls fn for every pool address in ascending order until fn
// returns false.
func (l layout) eachPool(fn func(ip netip.Addr) bool) {
	for ip := l.network.Next(); ip.Compare(l.broadcast) < 0; ip = ip.Next() {
		if !l.poolContains(ip) {
			continue
		}
		if !fn(ip) {
			return
		}
	}
}

// lastAddr is the broadcast address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	mask := net.CIDRMask(p.Bits(), 32)
	for i := range a {
		a[i] |= ^mask[i]
	}
	return netip.AddrFrom4(a)
}

// macFor derives the MAC of an address on a network: locally administered
// unicast (02), a byte hashed from the network name so networks on the same
// host differ, then the four IPv4 bytes. Deterministic, so a VM keeps its MAC
// as long as it keeps its IP.
func macFor(networkName string, ip netip.Addr) net.HardwareAddr {
	h := fnv.New32a()
	_, _ = h.Write([]byte(networkName))
	sum := h.Sum(nil)
	a := ip.As4()
	return net.HardwareAddr{0x02, sum[3], a[0], a[1], a[2], a[3]}
}
