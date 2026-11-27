//go:build e2e

package e2e

import (
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/layercert/layercerttest"
)

// The layer keyring of images/README.md, Layer certificates.
const (
	// gsLayerCertificateCN is the subject of scripts/gen-keys' test Giant
	// Swarm layer certificate, which the initrd of every e2e image enrolls.
	gsLayerCertificateCN = "vm-manager dev layer signing key"
	// layerKeyringUnit fills and seals the keyring in the initrd.
	layerKeyringUnit = "vm-layer-keyring.service"
)

// layerKeyringCmdline are the UKI command line flags the keyring rests on
// (images/mkosi.conf).
var layerKeyringCmdline = []string{
	"dm_verity.require_signatures=1",
	"dm_verity.keyring_unsealed=1",
	"module.sig_enforce=1",
	"sysctl.kernel.kexec_load_disabled=1",
}

// assertLayerKeyring checks a booted guest through sh (a root shell that
// returns trimmed stdout): the kernel command line carries the flags, kexec
// is off, the .dm-verity keyring holds exactly the certificates whose
// subjects are cns, the initrd's unit sealed it, and a key added from the
// running root is refused.
func assertLayerKeyring(t *testing.T, sh func(string) string, cns ...string) {
	t.Helper()
	cmdline := strings.Fields(sh("cat /proc/cmdline"))
	for _, flag := range layerKeyringCmdline {
		assert.Contains(t, cmdline, flag, "/proc/cmdline")
	}
	assert.Equal(t, "1", sh("cat /proc/sys/kernel/kexec_load_disabled"), "kexec_load_disabled")

	keyring := sh("keyctl show %:.dm-verity")
	journal := sh("journalctl -b -o cat --no-pager -u " + layerKeyringUnit)
	t.Logf(".dm-verity keyring:\n%s\n%s (initrd):\n%s", keyring, layerKeyringUnit, journal)
	for _, cn := range cns {
		assert.Contains(t, keyring, "asymmetric: "+cn+":", "the keyring holds %q", cn)
	}
	assert.Equal(t, len(cns), strings.Count(keyring, "asymmetric: "), "the keyring holds exactly %q", cns)
	assert.Contains(t, journal, "layer-keyring: sealed .dm-verity", "the initrd sealed the keyring")

	probe := layercerttest.New(t, "e2e key from the running root")
	block, _ := pem.Decode([]byte(probe.PEM))
	require.NotNil(t, block)
	out := sh("echo " + base64.StdEncoding.EncodeToString(block.Bytes) +
		` | base64 -d | { keyctl padd asymmetric "" %:.dm-verity 2>&1; echo rc=$?; }`)
	t.Logf("keyctl padd asymmetric \"\" %%:.dm-verity from the running root: %s", out)
	assert.Contains(t, out, "Operation not permitted", "the sealed keyring refuses a key from the running root")
	assert.NotContains(t, out, "rc=0")
	assert.Equal(t, len(cns), strings.Count(sh("keyctl show %:.dm-verity"), "asymmetric: "), "no key was added")
}
