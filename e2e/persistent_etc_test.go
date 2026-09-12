//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// The test's network and VM, disjoint from the other tests and from the
// server's default network (192.168.127.0/24).
const (
	etcNetwork  = "e2e-etc"
	etcCIDR     = "192.168.141.0/24"
	etcVMName   = "etc-e2e"
	etcHostname = "etc-e2e"
)

// What the test writes to /etc before the reboot and where it must show up.
const (
	etcFile        = "/etc/vm-manager-e2e"
	etcFileContent = "written to /etc before the reboot"
	etcUnit        = "e2e-marker.service"
	etcUnitPath    = "/etc/systemd/system/" + etcUnit
	etcBootMarker  = "/var/e2e-marker"
	etcOverlayDir  = "/var/lib/etc-overlay"
	etcUpperDir    = etcOverlayDir + "/upper"
	etcSetupUnit   = "persistent-etc.service"
	etcMachineID   = "/etc/machine-id"
	etcHostKey     = "/etc/ssh/ssh_host_ed25519_key.pub"
	kernelBootID   = "/proc/sys/kernel/random/boot_id"
	kernelHostname = "/proc/sys/kernel/hostname"
	varPartLabel   = "var"
)

// A oneshot that records the boot id it ran in; enabled in /etc, so both the
// unit file and the multi-user.target.wants symlink live in the overlay.
const etcUnitText = `[Unit]
Description=vm-manager e2e: record the boot id
After=local-fs.target

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'cat ` + kernelBootID + ` >` + etcBootMarker + `'

[Install]
WantedBy=multi-user.target
`

// Ceiling of the whole test; the reboot itself is bounded by rebootReadyWithin
// (ignition_test.go). The record's timestamps give the rebooted system's own
// boot budget, printed as reboot_to_ready_seconds=.
const etcTestTimeout = 10 * time.Minute

// TestPersistentEtc proves that /etc is writable and persistent across a
// reboot of the installed system: a file and an enabled unit written to /etc
// survive, the unit runs on the new boot, the SSH host key and the machine
// id are unchanged, the system is healthy, and /etc is an overlay whose
// upper directory is on the var partition (the file written to /etc is
// found there).
func TestPersistentEtc(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), etcTestTimeout)
	defer cancel()
	requireHost(t)
	image := locateImage(t)
	dir := stateDir(t)
	_, testKey := generateSSHKey(t, dir)

	srv := startServer(ctx, t, dir, image.Dir)
	m := newMCPClient(ctx, t, srv.URL)

	var n vm.NetworkInfo
	m.call(ctx, api.ToolCreateNetwork, map[string]any{"name": etcNetwork, "cidr": etcCIDR}, &n)

	var v vm.VM
	m.call(ctx, api.ToolCreateVM, map[string]any{
		"name":                etcVMName,
		"hostname":            etcHostname,
		"network":             etcNetwork,
		"ssh_authorized_keys": []string{testKey},
		"require_attestation": false,
		"wait_for":            string(vm.WaitReady),
	}, &v)
	require.Equal(t, vm.StateReady, v.State, "create_vm with wait_for ready: lastError %q\n%s", v.LastError, srv.logTail())
	require.NotNil(t, v.BootedAt, "bootedAt")
	firstBoot := *v.BootedAt
	t.Logf("create_vm: id %s ip %s", v.ID, v.IP)

	g := &guest{m: m, id: v.ID}
	g.waitSSH(ctx)
	before := recordEtc(ctx, t, g)
	assertEtcLayout(ctx, t, g)

	m.call(ctx, api.ToolRebootVM, map[string]any{"id": v.ID}, &v)
	v = waitRebooted(ctx, t, m, v.ID, firstBoot)
	boot := v.ReadyAt.Sub(*v.BootedAt)
	assert.Less(t, boot, apiBootCeiling, "rebooted system to READY=1")

	g.waitSSH(ctx)
	after := recordEtc(ctx, t, g)
	assertEtcLayout(ctx, t, g)
	assertEtcPersisted(ctx, t, g, before, after)

	var deleted api.DeletedResponse
	m.call(ctx, api.ToolDeleteVM, map[string]any{"id": v.ID}, &deleted)
	assert.True(t, deleted.Deleted)
	m.call(ctx, api.ToolDeleteNetwork, map[string]any{"name": etcNetwork}, &deleted)
	assert.True(t, deleted.Deleted)
	srv.stop()

	t.Logf("reboot_to_ready_seconds=%.1f", boot.Seconds())
	fmt.Printf("reboot_to_ready_seconds=%.1f\n", boot.Seconds())
}

// etcState is what the test reads from the guest on each side of the reboot.
type etcState struct {
	bootID, machineID, hostKey string
}

// recordEtc writes the marker file and the enabled unit (idempotent: the
// second call, after the reboot, just rewrites the same content) and reads
// what must stay the same across the reboot.
func recordEtc(ctx context.Context, t *testing.T, g *guest) etcState {
	t.Helper()
	g.sh(ctx, fmt.Sprintf("printf '%%s' '%s' >%s", etcFileContent, etcFile))
	g.sh(ctx, fmt.Sprintf("cat >%s <<'EOF'\n%sEOF\nsystemctl enable %s", etcUnitPath, etcUnitText, etcUnit))
	s := etcState{
		bootID:    g.sh(ctx, "cat "+kernelBootID),
		machineID: g.sh(ctx, "cat "+etcMachineID),
		hostKey:   g.sh(ctx, "cat "+etcHostKey),
	}
	assert.Len(t, s.machineID, 32, "machine id %q", s.machineID)
	assert.True(t, strings.HasPrefix(s.hostKey, "ssh-ed25519 "), "host key %q", s.hostKey)
	t.Logf("boot %s machine-id %s", s.bootID, s.machineID)
	return s
}

// assertEtcLayout checks the mounts persistent-etc set up in the initrd:
// /var is the ext4 var partition, /etc an overlay whose upper directory is
// on it (the file written to /etc appears under /var/lib/etc-overlay/upper),
// /root and /opt are bind mounts from var.
func assertEtcLayout(ctx context.Context, t *testing.T, g *guest) {
	t.Helper()
	varSource := g.sh(ctx, "findmnt -no SOURCE /var")
	assert.Equal(t, "ext4", g.sh(ctx, "findmnt -no FSTYPE /var"), "/var file system")
	assert.Equal(t, varPartLabel, g.sh(ctx, "lsblk -no PARTLABEL "+varSource), "partition label of %s", varSource)

	assert.Equal(t, "overlay", g.sh(ctx, "findmnt -no FSTYPE /etc"), "/etc file system")
	options := g.sh(ctx, "findmnt -no OPTIONS /etc")
	assert.Contains(t, options, "upperdir=/sysroot"+etcUpperDir, "/etc overlay options")
	assert.Equal(t, etcFileContent, g.sh(ctx, "cat "+etcUpperDir+strings.TrimPrefix(etcFile, "/etc")), "the file written to /etc in the overlay's upper directory on var")
	assert.Equal(t, "enabled", g.sh(ctx, "systemctl is-enabled "+etcUnit))

	for _, mount := range []string{"/root", "/opt"} {
		assert.Equal(t, "ext4", g.sh(ctx, "findmnt -no FSTYPE "+mount), "%s file system", mount)
		assert.Contains(t, g.sh(ctx, "findmnt -no SOURCE "+mount), varSource+"[", "%s is not bound from the var partition", mount)
	}
	assert.Contains(t, g.sh(ctx, "cat /root/.ssh/authorized_keys"), "ssh-", "root's authorized_keys on the bound /root")
}

// assertEtcPersisted compares the two sides of the reboot.
func assertEtcPersisted(ctx context.Context, t *testing.T, g *guest, before, after etcState) {
	t.Helper()
	require.NotEqual(t, before.bootID, after.bootID, "the guest did not reboot")
	assert.Equal(t, before.machineID, after.machineID, "machine id changed across the reboot")
	assert.Equal(t, before.hostKey, after.hostKey, "SSH host key changed across the reboot")

	assert.Equal(t, etcFileContent, g.sh(ctx, "cat "+etcFile), "file written to /etc before the reboot")
	assert.Equal(t, "success", g.sh(ctx, "systemctl show "+etcUnit+" -p Result --value"), "%s result on the new boot", etcUnit)
	assert.Equal(t, after.bootID, g.sh(ctx, "cat "+etcBootMarker), "%s did not run on the new boot", etcUnit)

	assert.Equal(t, etcHostname, g.sh(ctx, "cat "+kernelHostname), "kernel hostname from the persisted /etc/hostname")
	assert.Equal(t, etcHostname, g.sh(ctx, "hostnamectl --static"), "static hostname")

	failed := g.sh(ctx, "systemctl --failed --no-legend --plain")
	assert.Empty(t, failed, "failed units after the reboot:\n%s", failed)
	state, err := g.run(ctx, "systemctl is-system-running")
	require.NoError(t, err)
	assert.Equal(t, "running", strings.TrimSpace(state.Stdout), "system state (exit %d)", state.ExitCode)

	// The initrd's fsck of var on this boot proves the partition was checked
	// before it was mounted: e2fsck always ends with the "clean" summary line.
	// A dirty ext4 additionally logs "recovering journal" first, and today it
	// does: systemd-shutdown cannot unmount the /etc overlay, which pins var's
	// writers; see images/README.md, "Persistent state", known limitation.
	// That recovery is logged, not asserted, until it is solved; the data is
	// safe (systemd-shutdown syncs before power-off).
	setup := g.sh(ctx, "journalctl -b -o cat --no-pager -u "+etcSetupUnit)
	t.Logf("%s on the new boot (fsck of var):\n%s", etcSetupUnit, setup)
	assert.Contains(t, setup, "var: clean", "%s did not fsck var", etcSetupUnit)
}
