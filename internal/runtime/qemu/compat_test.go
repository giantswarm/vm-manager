package qemu

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want Version
	}{
		{"QEMU emulator version 8.2.2 (Debian 1:8.2.2+ds-0ubuntu1.4)\nCopyright (c) 2003-2023 Fabrice Bellard\n", Version{8, 2}},
		{"QEMU emulator version 11.1.1\n", Version{11, 1}},
		{"QEMU emulator version 10.0.50 (v10.0.0-1234-gabcdef)", Version{10, 0}},
	} {
		v, err := ParseVersion(tc.out)
		require.NoError(t, err, tc.out)
		assert.Equal(t, tc.want, v, tc.out)
	}
	_, err := ParseVersion("qemu: command not found")
	require.Error(t, err)
}

func TestVersionLess(t *testing.T) {
	assert.True(t, Version{8, 2}.Less(Version{9, 2}))
	assert.True(t, Version{9, 1}.Less(Version{9, 2}))
	assert.False(t, Version{9, 2}.Less(Version{9, 2}))
	assert.False(t, Version{10, 0}.Less(Version{9, 2}))
	assert.Equal(t, "9.2", Version{9, 2}.String())
}

func TestCompatArgsReconnect(t *testing.T) {
	args := []string{
		"-name", "guest=x",
		"-netdev", "stream,addr.type=unix,addr.path=/run/net.sock,reconnect-ms=1000,id=net0",
		"-device", "virtio-net-pci,netdev=net0,mac=52:54:00:00:00:01",
		"-netdev", "user,id=net1",
		"-netdev", "stream,addr.type=unix,addr.path=/run/x,reconnect-ms=1500",
		"-netdev", "stream,reconnect-ms=0,addr.path=/run/y",
	}

	// Current QEMU: untouched, and the same slice.
	same := compatArgs(args, Version{9, 2})
	assert.Equal(t, args, same)
	same = compatArgs(args, Version{11, 1})
	assert.Equal(t, args, same)

	// QEMU 8.2: reconnect-ms becomes whole seconds, rounded up, never 0;
	// the input is not modified.
	old := compatArgs(args, Version{8, 2})
	assert.Equal(t, []string{
		"-name", "guest=x",
		"-netdev", "stream,addr.type=unix,addr.path=/run/net.sock,reconnect=1,id=net0",
		"-device", "virtio-net-pci,netdev=net0,mac=52:54:00:00:00:01",
		"-netdev", "user,id=net1",
		"-netdev", "stream,addr.type=unix,addr.path=/run/x,reconnect=2",
		"-netdev", "stream,reconnect=1,addr.path=/run/y",
	}, old)
	assert.Contains(t, args[3], "reconnect-ms=1000", "input slice must stay as rendered")
}
