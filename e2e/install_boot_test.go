//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/tpm"
)

// Ceilings. docs/design.md budgets the installer phase at < 60 s and the
// installed boot at < 10 s to READY; these are the generous first
// assertions, the measured install_seconds= and boot_to_ready_seconds=
// lines in the output are what to tighten them from.
const (
	installCeiling  = 120 * time.Second
	bootCeiling     = 30 * time.Second
	sshCeiling      = 30 * time.Second
	shutdownCeiling = 30 * time.Second
	testTimeout     = 5 * time.Minute
)

// GPT partition type GUIDs (x86-64 where the type is per-architecture), as
// systemd-repart writes and sfdisk --json prints them.
const (
	typeESP           = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	typeRoot          = "4f68bce3-e8cd-4db1-96e7-fbcaf984b709"
	typeRootVerity    = "2c7357ed-ebd2-46d9-aec1-23d437ec2bf5"
	typeRootVeritySig = "41092b05-9fc8-4523-994f-2def0408b176"
	typeVar           = "4d21b016-b534-45c2-a9fb-5c16e091fd2d"
)

// hostname is passed as firstboot.hostname in the installer phase only:
// systemd-sysinstall seals it for the vTPM next to the UKI on the target
// ESP, and the installed system must come up with it without being told
// again.
const hostname = "e2e-install-boot"

// TestInstallBoot is the two-phase boot flow of docs/design.md "Boot flow"
// against the real image: the installer boot runs systemd-sysinstall onto a
// blank disk and QEMU exits on the guest's reboot (-no-reboot); the target
// then carries the expected A/B layout; the installed boot reports READY=1
// over vsock, answers ssh over vsock with the sealed hostname, no failed
// units and the imported credentials, and powers down on request.
func TestInstallBoot(t *testing.T) {
	testInstallBoot(t, directKernelInstaller)
}

// TestInstallBootSystemdBoot is the same flow with the installer booted
// through systemd-boot from the base DDI's ESP instead of -kernel: what
// OVMF falls back to when it does not take the direct kernel boot, for
// example after the vTPM stalled in the firmware.
func TestInstallBootSystemdBoot(t *testing.T) {
	testInstallBoot(t, systemdBootInstaller)
}

func testInstallBoot(t *testing.T, installer installerBoot) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	h := New(t)

	afterInstall, afterFirstBoot := expectedLayout(imageID + "_" + h.Image.Version)

	install := runInstallPhase(ctx, t, h, installer)
	parts := PartitionTable(t, h.Target)
	assertLayout(t, "after the installer", parts, afterInstall)
	assertSlotUUIDs(t, h, parts)

	boot := runBootPhase(ctx, t, h)
	assertLayout(t, "after the first boot", PartitionTable(t, h.Target), afterFirstBoot)

	t.Logf("install_seconds=%.1f", install.Seconds())
	t.Logf("boot_to_ready_seconds=%.1f", boot.Seconds())
	fmt.Printf("install_seconds=%.1f\nboot_to_ready_seconds=%.1f\n", install.Seconds(), boot.Seconds())
}

// installerBoot is one way to boot the installer onto h.Target: the QEMU
// spec, and the console line vm-sysinstall prints for how it found the UKI,
// which proves the boot took that path.
type installerBoot func(t *testing.T, h *Harness, tp *tpm.Instance) (spec qemu.Spec, ukiFound string)

// directKernelInstaller is vm-manager's installer boot: the UKI via
// -kernel, so no ESP is automounted and vm-sysinstall searches the base
// DDI's ESP itself.
func directKernelInstaller(_ *testing.T, h *Harness, tp *tpm.Instance) (qemu.Spec, string) {
	return h.Spec(qemu.PhaseInstall, tp, "install"), "vm-sysinstall: searching ESP"
}

// sysinstallTargetSerial is the blank disk's serial in the systemd-boot
// installer boot, where "target" is the boot disk.
const sysinstallTargetSerial = "sysinstall"

// systemdBootInstaller boots systemd-boot from a scratch copy of the base
// DDI: PhaseBoot passes no -kernel and gives the spec's Target bootindex 0,
// so the copy goes there and the blank disk is an extra drive that
// vm.install-target names. systemd-gpt-auto-generator then automounts the
// booted ESP at /boot, which vm-sysinstall covers with a tmpfs.
func systemdBootInstaller(t *testing.T, h *Harness, tp *tpm.Instance) (qemu.Spec, string) {
	t.Helper()
	ddi := filepath.Join(h.Dir, "installer.raw")
	out, err := exec.Command("cp", "--sparse=always", "--reflink=auto", h.Image.DDI, ddi).CombinedOutput()
	require.NoError(t, err, "copy the base DDI: %s", out)

	spec := h.Spec(qemu.PhaseBoot, tp, "install")
	spec.Target = ddi
	spec.ExtraDrives = []qemu.Drive{{Path: h.Target, Serial: sysinstallTargetSerial}}
	spec.NoReboot = true
	spec.Credentials[qemu.CredentialInstallTarget] = "/dev/disk/by-id/virtio-" + sysinstallTargetSerial
	return spec, "vm-sysinstall: using the booted ESP mounted at /"
}

// runInstallPhase boots the installer with the blank target and returns how
// long the guest took from QEMU start to its reboot.
func runInstallPhase(ctx context.Context, t *testing.T, h *Harness, installer installerBoot) time.Duration {
	t.Helper()
	tp := h.StartTPM(ctx)
	spec, ukiFound := installer(t, h, tp)
	spec.Credentials[qemu.CredentialHostname] = hostname
	spec.Credentials[qemu.CredentialSSHAuthorizedKeysRoot] = h.AuthorizedKey

	started := time.Now()
	inst := h.Start(ctx, spec)
	st := h.WaitExit(inst, installCeiling)
	elapsed := time.Since(started)
	console := h.Console(inst)
	require.Equal(t, 0, st.Code, "installer qemu exit: %s; stderr: %q\n%s", st, inst.Stderr(), h.ConsoleTail(inst))

	// The installer unit ran systemd-sysinstall to completion and the guest
	// rebooted (which -no-reboot turned into the exit we just saw). The
	// unit logs to the console (SYSTEMD_LOG_TARGET=console) as sysinstall[PID].
	for _, want := range []string{
		ukiFound,
		"vm-sysinstall: installing",
		"Installation succeeded.",
		"vm-sysinstall: installation complete, rebooting",
		"reboot: Restarting system",
	} {
		assert.Contains(t, console, want, "installer console lacks %q\n%s", want, h.ConsoleTail(inst))
	}
	assert.NotContains(t, console, "vm-sysinstall.service: Failed", "installer unit failed\n%s", h.ConsoleTail(inst))
	assert.NotContains(t, console, "emergency mode", "installer boot dropped to emergency mode\n%s", h.ConsoleTail(inst))

	select {
	case st := <-tp.Wait():
		t.Logf("swtpm followed the installer qemu out: %s", st)
	case <-time.After(10 * time.Second):
		t.Fatal("swtpm did not terminate after the installer qemu closed its control channel")
	}
	assert.Less(t, elapsed, installCeiling)
	return elapsed
}

// expectedPartition is one entry of the layout images/.../repart.sysinstall.d
// defines: ESP, the A slots as block copies of the installer's partitions,
// empty B slots of the same fixed sizes, var with the rest of the disk.
type expectedPartition struct {
	number  int
	typ     string
	name    string
	sizeKiB int64 // 0: at least minVarKiB, var takes the rest of the disk
}

const (
	minVarKiB = 1024 * 1024
	// veritySigKiB is what systemd-repart makes every verity signature
	// partition (VERITY_SIG_SIZE), whatever the definition says.
	veritySigKiB = 16
)

// expectedLayout is the target after the installer (systemd-sysinstall
// defers Label=_empty partitions: their space is reserved, var is already
// number 8) and after the installed system's first boot, whose
// systemd-repart creates the B slots from /usr/lib/repart.d.
func expectedLayout(slot string) (afterInstall, afterFirstBoot []expectedPartition) {
	a := []expectedPartition{
		{1, typeESP, "esp", 512 * 1024},
		{2, typeRoot, slot, 2048 * 1024},
		{3, typeRootVerity, slot + "_verity", 64 * 1024},
		{4, typeRootVeritySig, slot + "_verity_sig", veritySigKiB},
	}
	b := []expectedPartition{
		{5, typeRoot, "_empty", 2048 * 1024},
		{6, typeRootVerity, "_empty", 64 * 1024},
		{7, typeRootVeritySig, "_empty", veritySigKiB},
	}
	v := expectedPartition{8, typeVar, "var", 0}
	afterInstall = append(append([]expectedPartition{}, a...), v)
	afterFirstBoot = append(append(a, b...), v)
	return afterInstall, afterFirstBoot
}

func assertLayout(t *testing.T, what string, parts []Partition, expected []expectedPartition) {
	t.Helper()
	t.Logf("partition table %s:\n%s", what, Describe(parts))
	require.Len(t, parts, len(expected), "partition count %s\n%s", what, Describe(parts))
	for i, want := range expected {
		got := parts[i]
		assert.Equal(t, want.number, got.Number(), "partition %d number %s", i+1, what)
		assert.True(t, strings.EqualFold(want.typ, got.Type), "partition %d type %s: want %s got %s", want.number, what, want.typ, got.Type)
		assert.Equal(t, want.name, got.Name, "partition %d label %s", want.number, what)
		if want.sizeKiB > 0 {
			assert.Equal(t, want.sizeKiB, got.SizeKiB(), "partition %d size %s", want.number, what)
		} else {
			assert.GreaterOrEqual(t, got.SizeKiB(), int64(minVarKiB), "partition %d (var) size %s", want.number, what)
		}
	}
}

// assertSlotUUIDs checks that CopyBlocks=auto carried the source partition
// UUIDs over: the installed UKI's roothash= locates root and verity by them
// (the two halves of the hash).
func assertSlotUUIDs(t *testing.T, h *Harness, parts []Partition) {
	t.Helper()
	if len(h.Image.RootHash) != 64 {
		t.Logf("no roothash artifact next to the image, skipping partition UUID check")
		return
	}
	assert.True(t, strings.EqualFold(uuidFromHex(h.Image.RootHash[:32]), parts[1].UUID), "root A partition UUID is not the first half of the roothash %s: %s", h.Image.RootHash, parts[1].UUID)
	assert.True(t, strings.EqualFold(uuidFromHex(h.Image.RootHash[32:]), parts[2].UUID), "verity A partition UUID is not the second half of the roothash %s: %s", h.Image.RootHash, parts[2].UUID)
}

// toleratedFailures are units that cannot succeed without the IMDS this test
// does not provide (user networking, systemd.imds=no): the metrics upload
// timer fires every 15 s against 169.254.169.254. TestNetworkIMDS, which
// serves a real IMDS, tolerates no failed unit.
var toleratedFailures = map[string]bool{
	"vm-report-upload.service": true,
}

// unexpectedFailures filters `systemctl --failed --no-legend --plain`
// output down to the units this test does not tolerate.
func unexpectedFailures(failed string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(failed), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || toleratedFailures[fields[0]] {
			continue
		}
		out = append(out, line)
	}
	return out
}

// uuidFromHex formats 32 hex digits as 8-4-4-4-12.
func uuidFromHex(h string) string {
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// runBootPhase boots the installed target with the same TPM state, firmware
// variables and machine ID, waits for READY=1, verifies the guest over ssh
// and powers it down. It returns the time from QEMU start to READY=1.
func runBootPhase(ctx context.Context, t *testing.T, h *Harness) time.Duration {
	t.Helper()
	tp := h.StartTPM(ctx)
	spec := h.Spec(qemu.PhaseBoot, tp, "boot")
	spec.Credentials[qemu.CredentialSSHAuthorizedKeysRoot] = h.AuthorizedKey

	started := time.Now()
	inst := h.Start(ctx, spec)
	ready := h.WaitReady(inst, bootCeiling)
	elapsed := time.Since(started)
	assert.True(t, ready.Privileged(), "READY=1 came from port %d, not from PID 1", ready.Port)
	assert.Less(t, elapsed, bootCeiling)

	// The static hostname can only come from the firstboot.hostname the
	// installer sealed for this vTPM (phase B passes none), the running
	// hostname from the image's drop-in that applies it.
	static := strings.TrimSpace(h.SSHRetry(ctx, sshCeiling, "hostnamectl --static"))
	assert.Equal(t, hostname, static, "static hostname: the sealed firstboot.hostname was not applied\n%s", h.ConsoleTail(inst))
	running, err := h.SSH(ctx, "hostnamectl hostname")
	require.NoError(t, err)
	assert.Equal(t, hostname, strings.TrimSpace(running), "running hostname")

	failed, err := h.SSH(ctx, "systemctl --failed --no-legend --plain")
	require.NoError(t, err)
	assert.Empty(t, unexpectedFailures(failed), "failed units in the installed system:\n%s\n%s", failed, h.ConsoleTail(inst))

	state, err := h.SSH(ctx, "systemctl is-system-running || true")
	require.NoError(t, err)
	t.Logf("system state: %s", strings.TrimSpace(state))

	// The first boot's systemd-repart creates the B slots the installer
	// deferred; its result is asserted on the host after the shutdown.
	repart, err := h.SSH(ctx, "systemctl show systemd-repart.service -p ActiveState,Result,ExecMainStatus --value | paste -sd' '; lsblk -o NAME,SIZE,PARTLABEL,MOUNTPOINTS; journalctl -b -o cat --no-pager 2>/dev/null | grep -iE 'gpt-auto|var\\.mount|/var|root file system' | head -20")
	require.NoError(t, err)
	t.Logf("systemd-repart (active result status), block devices, gpt-auto journal:\n%s", repart)

	creds, err := h.SSH(ctx, "systemd-creds --system list --no-legend 2>/dev/null || ls /run/credentials/@system")
	require.NoError(t, err)
	t.Logf("system credentials:\n%s", creds)
	for _, want := range []string{
		qemu.CredentialHostname,
		qemu.CredentialSSHAuthorizedKeysRoot,
		qemu.CredentialMachineID,
		qemu.CredentialNotifySocket,
	} {
		assert.Contains(t, creds, want, "credential %s not imported", want)
	}
	machineID, err := h.SSH(ctx, "cat /etc/machine-id")
	require.NoError(t, err)
	assert.Equal(t, h.MachineID, strings.TrimSpace(machineID), "machine id")
	varMount, err := h.SSH(ctx, "findmnt -no SOURCE,FSTYPE /var")
	require.NoError(t, err)
	t.Logf("/var: %s", strings.TrimSpace(varMount))
	assert.Contains(t, varMount, "ext4", "var is not mounted from the target's ext4 partition")

	sctx, scancel := context.WithTimeout(ctx, shutdownCeiling)
	defer scancel()
	require.NoError(t, inst.Stop(sctx))
	st := <-inst.Wait()
	assert.Equal(t, 0, st.Code, "installed system did not power down cleanly: %s\n%s", st, h.ConsoleTail(inst))
	return elapsed
}
