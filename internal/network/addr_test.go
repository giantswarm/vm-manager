package network

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name    string
		spec    Spec
		want    layout
		wantErr error
	}{
		{
			name: "slash 24 defaults",
			spec: Spec{Name: "dev", CIDR: "10.42.0.0/24"},
			want: layout{
				prefix:    netip.MustParsePrefix("10.42.0.0/24"),
				network:   netip.MustParseAddr("10.42.0.0"),
				gateway:   netip.MustParseAddr("10.42.0.1"),
				host:      netip.MustParseAddr("10.42.0.254"),
				broadcast: netip.MustParseAddr("10.42.0.255"),
			},
		},
		{
			name: "unmasked cidr and custom gateway",
			spec: Spec{Name: "dev", CIDR: "192.168.5.7/29", GatewayIP: "192.168.5.3"},
			want: layout{
				prefix:    netip.MustParsePrefix("192.168.5.0/29"),
				network:   netip.MustParseAddr("192.168.5.0"),
				gateway:   netip.MustParseAddr("192.168.5.3"),
				host:      netip.MustParseAddr("192.168.5.6"),
				broadcast: netip.MustParseAddr("192.168.5.7"),
			},
		},
		{
			name: "slash 16",
			spec: Spec{Name: "big", CIDR: "172.20.0.0/16"},
			want: layout{
				prefix:    netip.MustParsePrefix("172.20.0.0/16"),
				network:   netip.MustParseAddr("172.20.0.0"),
				gateway:   netip.MustParseAddr("172.20.0.1"),
				host:      netip.MustParseAddr("172.20.255.254"),
				broadcast: netip.MustParseAddr("172.20.255.255"),
			},
		},
		{name: "bad name", spec: Spec{Name: "Dev_1", CIDR: "10.0.0.0/24"}, wantErr: apierr.ErrInvalid},
		{name: "empty name", spec: Spec{CIDR: "10.0.0.0/24"}, wantErr: apierr.ErrInvalid},
		{name: "bad cidr", spec: Spec{Name: "dev", CIDR: "10.0.0.0"}, wantErr: apierr.ErrInvalid},
		{name: "ipv6", spec: Spec{Name: "dev", CIDR: "fd00::/64"}, wantErr: apierr.ErrInvalid},
		{name: "too small", spec: Spec{Name: "dev", CIDR: "10.0.0.0/30"}, wantErr: apierr.ErrInvalid},
		{name: "too big", spec: Spec{Name: "dev", CIDR: "10.0.0.0/8"}, wantErr: apierr.ErrInvalid},
		{name: "link local", spec: Spec{Name: "dev", CIDR: "169.254.0.0/24"}, wantErr: apierr.ErrInvalid},
		{name: "gateway outside", spec: Spec{Name: "dev", CIDR: "10.0.0.0/24", GatewayIP: "10.0.1.1"}, wantErr: apierr.ErrInvalid},
		{name: "gateway is host alias", spec: Spec{Name: "dev", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.254"}, wantErr: apierr.ErrInvalid},
		{name: "gateway is broadcast", spec: Spec{Name: "dev", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.255"}, wantErr: apierr.ErrInvalid},
		{name: "gateway unparsable", spec: Spec{Name: "dev", CIDR: "10.0.0.0/24", GatewayIP: "nope"}, wantErr: apierr.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolve(tt.spec)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLayoutPool(t *testing.T) {
	l, err := resolve(Spec{Name: "dev", CIDR: "10.0.0.0/29", GatewayIP: "10.0.0.2"})
	require.NoError(t, err)

	var pool []string
	l.eachPool(func(ip netip.Addr) bool {
		pool = append(pool, ip.String())
		return true
	})
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.3", "10.0.0.4", "10.0.0.5"}, pool)
	assert.Equal(t, 4, l.poolSize())

	for _, reserved := range []string{"10.0.0.0", "10.0.0.2", "10.0.0.6", "10.0.0.7", "10.0.1.1"} {
		assert.False(t, l.poolContains(netip.MustParseAddr(reserved)), reserved)
	}

	var first netip.Addr
	l.eachPool(func(ip netip.Addr) bool {
		first = ip
		return false
	})
	assert.Equal(t, "10.0.0.1", first.String(), "eachPool stops when fn returns false")
}

func TestMacFor(t *testing.T) {
	ip := netip.MustParseAddr("10.42.0.7")
	mac := macFor("dev", ip)

	assert.Len(t, mac, 6)
	assert.Equal(t, byte(0x02), mac[0]&0x03, "locally administered, unicast")
	assert.Equal(t, []byte{10, 42, 0, 7}, []byte(mac[2:]), "low four bytes are the IP")
	assert.Equal(t, mac, macFor("dev", ip), "deterministic")
	assert.NotEqual(t, mac, macFor("dev", netip.MustParseAddr("10.42.0.8")))
	assert.NotEqual(t, mac, macFor("prod", ip), "differs per network")
}

func TestMacForFormat(t *testing.T) {
	mac := macFor("dev", netip.MustParseAddr("192.168.127.2"))
	assert.Regexp(t, `^02:[0-9a-f]{2}:c0:a8:7f:02$`, mac.String())
}

func TestParseLeases(t *testing.T) {
	l, err := resolve(Spec{Name: "dev", CIDR: "10.0.0.0/24"})
	require.NoError(t, err)

	tests := []struct {
		name   string
		leases map[string]string
		ok     bool
	}{
		{name: "empty", ok: true},
		{name: "valid", leases: map[string]string{"a": "10.0.0.2", "b": "10.0.0.9"}, ok: true},
		{name: "duplicate ip", leases: map[string]string{"a": "10.0.0.2", "b": "10.0.0.2"}},
		{name: "gateway", leases: map[string]string{"a": "10.0.0.1"}},
		{name: "host alias", leases: map[string]string{"a": "10.0.0.254"}},
		{name: "outside", leases: map[string]string{"a": "10.0.1.2"}},
		{name: "garbage", leases: map[string]string{"a": "ten"}},
		{name: "empty vm id", leases: map[string]string{"": "10.0.0.2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLeases(l, tt.leases)
			if !tt.ok {
				require.True(t, errors.Is(err, apierr.ErrInvalid), "got %v", err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, got, len(tt.leases))
		})
	}
}
