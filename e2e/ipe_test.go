//go:build e2e

package e2e

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
)

// The IPE policy of images/ipe/giantswarm-vm.policy as the kernel shows it.
const (
	ipePolicy    = "giantswarm-vm"
	ipeFS        = "/sys/kernel/security/ipe"
	ipePolicyDir = ipeFS + "/policies/" + ipePolicy
	// capMACAdmin is CAP_MAC_ADMIN's bit in /proc/<pid>/status CapBnd.
	capMACAdmin = 33
)

// TestIPE boots the installed image with its certificate in the VM's db, as
// vm-manager does, and proves the integrity policy (images/README.md, IPE):
// the kernel trusts the certificate (platform keyring), opened the root's
// verity device with its signature, and PID 1 loaded and activated the
// signed policy in enforce mode; a copy of an image binary outside the
// verity root does not execute, from the var partition, the /etc overlay,
// /run or /tmp alike; nobody holds CAP_MAC_ADMIN, so root can neither switch
// enforcement off nor load a policy. Then it plants what would undo IPE on
// the next boot (the policy hidden, a manager drop-in restoring
// CAP_MAC_ADMIN), reboots and finds both dropped by the initrd, the policy
// active and the bounding set unchanged; the clean shutdown in between runs
// systemd-shutdown from the exitrd, which the policy must allow.
func TestIPE(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	h := New(t)
	if h.Image.Certificate == "" {
		t.Skipf("e2e: %s has no %s_%s.crt; the image predates IPE", h.Image.Dir, imageID, h.Image.Version)
	}

	runInstallPhase(ctx, t, h, directKernelInstaller)

	inst := bootInstalled(ctx, t, h, "boot")
	sh := func(cmd string) string {
		t.Helper()
		out, err := h.SSH(ctx, cmd)
		require.NoError(t, err, "%s: %s", cmd, out)
		return strings.TrimSpace(out)
	}

	assert.Contains(t, strings.Split(sh("cat /sys/kernel/security/lsm"), ","), "ipe", "LSMs")
	assert.Equal(t, "1", sh("cat "+ipePolicyDir+"/active"), "policy %s active\n%s", ipePolicy, h.ConsoleTail(inst))
	assert.Equal(t, h.Image.Version, sh("cat "+ipePolicyDir+"/version"), "policy version")
	assert.Equal(t, "1", sh("cat "+ipeFS+"/enforce"), "IPE enforces")
	t.Logf("platform keyring:\n%s", sh("keyctl show %:.platform"))
	assert.Contains(t, sh("keyctl show %:.platform"), "asymmetric", "the image certificate on the platform keyring")
	assert.Contains(t, sh("dmsetup table root"), "root_hash_sig_key_desc", "the root's verity device carries its signature, verified by the kernel")

	// Nothing outside the signed dm-verity root runs, from any writable file
	// system the guest has.
	for _, dir := range []string{"/var/tmp", "/etc", "/run", "/tmp", "/root"} {
		out := sh("cp /usr/bin/true " + dir + "/ipe-true && { " + dir + "/ipe-true; echo rc=$?; } 2>&1; rm -f " + dir + "/ipe-true")
		assert.Contains(t, out, "Permission denied", "executing a copy in %s", dir)
		assert.Contains(t, out, "rc=126", "executing a copy in %s", dir)
	}
	t.Logf("IPE audit records:\n%s", sh("journalctl -b -o cat --no-pager | grep -E 'ipe_op=|IPE_ACCESS|type=1420' | tail -n 5 || true"))

	// A script: executing it is denied like any file outside the root, but
	// the image's bash reading it as data is not IPE's to judge (bash does
	// not ask with AT_EXECVE_CHECK). Pinned, so a change is noticed.
	sh("printf '#!/usr/bin/bash\\necho script ran\\n' > /var/tmp/ipe.sh && chmod 0755 /var/tmp/ipe.sh")
	assert.Contains(t, sh("{ /var/tmp/ipe.sh; } 2>&1 || true"), "Permission denied", "executing a script on var")
	assert.Equal(t, "script ran", sh("bash /var/tmp/ipe.sh"), "the image's bash reading a script")

	// Locked: no CAP_MAC_ADMIN anywhere, so the securityfs writes fail even
	// for root.
	assertNoMACAdmin(t, sh("grep ^CapBnd: /proc/1/status /proc/self/status"))
	assert.Contains(t, sh("{ echo 0 > "+ipeFS+"/enforce; } 2>&1 || true"), "Operation not permitted", "switching enforcement off")
	assert.Contains(t, sh("{ cat /etc/ipe/"+ipePolicy+".p7b > "+ipeFS+"/new_policy; } 2>&1 || true"), "Operation not permitted", "loading a policy")
	assert.Equal(t, "1", sh("cat "+ipeFS+"/enforce"), "IPE still enforces")

	// The processes the kernel starts itself do not hold it either: a
	// core_pattern pipe runs the image's bash on a script from var, as root
	// and outside PID 1's bounding set, and its write is refused.
	assertNoMACAdmin(t, "CapBnd: "+capHex(t, sh("cat /proc/sys/kernel/usermodehelper/bset")))
	sh("printf '#!/usr/bin/bash\\n{ echo 0 > " + ipeFS + "/enforce; } 2>> /var/tmp/umh.log; echo helper ran >> /var/tmp/umh.log\\n' > /var/tmp/umh.sh && " +
		"echo '|/usr/bin/bash /var/tmp/umh.sh' > /proc/sys/kernel/core_pattern && " +
		"{ bash -c 'ulimit -c unlimited; kill -SEGV $$' || true; }")
	umh := ""
	require.Eventually(t, func() bool {
		umh = sh("cat /var/tmp/umh.log 2>/dev/null || true")
		return strings.Contains(umh, "helper ran")
	}, 15*time.Second, 500*time.Millisecond, "the core_pattern helper did not run")
	assert.Contains(t, umh, "Operation not permitted", "a usermode helper switching IPE off")
	assert.Equal(t, "1", sh("cat "+ipeFS+"/enforce"), "IPE still enforces after the helper")
	assert.Empty(t, unexpectedFailures(sh("systemctl --failed --no-legend --plain")), "failed units\n%s", h.ConsoleTail(inst))

	// What root could leave in /etc for the next boot.
	sh("rm /etc/ipe/" + ipePolicy + ".p7b && mkdir -p /etc/systemd/system.conf.d && " +
		"printf '[Manager]\\nCapabilityBoundingSet=\\nCapabilityBoundingSet=~\\n' > /etc/systemd/system.conf.d/99-ipe-e2e.conf")
	stopInstalled(ctx, t, h, inst)

	inst = bootInstalled(ctx, t, h, "reboot")
	assert.Equal(t, "1", sh("cat "+ipePolicyDir+"/active"), "policy active after a boot that hid it\n%s", h.ConsoleTail(inst))
	assert.Equal(t, "present", sh("test -s /etc/ipe/"+ipePolicy+".p7b && echo present"), "the image's policy is back in /etc/ipe")
	assert.Equal(t, "dropped", sh("test -e /etc/systemd/system.conf.d/99-ipe-e2e.conf || echo dropped"), "the planted manager drop-in")
	assertNoMACAdmin(t, sh("grep ^CapBnd: /proc/1/status"))
	assert.Contains(t, sh("journalctl -b -o cat --no-pager -u persistent-etc.service"), "persistent-etc: /etc/ipe is the image's", "the initrd reported the dropped override")
	assert.Empty(t, sh("journalctl -b -o cat --no-pager -t systemd-fsck | grep -i 'recovering journal' || true"),
		"var was not cleanly unmounted at the previous shutdown: the exitrd's systemd-shutdown did not run")
	stopInstalled(ctx, t, h, inst)
}

// bootInstalled starts the installed system and waits for READY=1 and ssh.
func bootInstalled(ctx context.Context, t *testing.T, h *Harness, name string) *qemu.Instance {
	t.Helper()
	tp := h.StartTPM(ctx)
	spec := h.Spec(qemu.PhaseBoot, tp, name)
	spec.Credentials[qemu.CredentialSSHAuthorizedKeysRoot] = h.AuthorizedKey
	inst := h.Start(ctx, spec)
	h.WaitReady(inst, bootCeiling)
	h.SSHRetry(ctx, sshCeiling, "true")
	return inst
}

// stopInstalled powers the guest down and requires a clean exit.
func stopInstalled(ctx context.Context, t *testing.T, h *Harness, inst *qemu.Instance) {
	t.Helper()
	sctx, scancel := context.WithTimeout(ctx, shutdownCeiling)
	defer scancel()
	require.NoError(t, inst.Stop(sctx))
	st := <-inst.Wait()
	require.Equal(t, 0, st.Code, "guest did not power down cleanly: %s\n%s", st, h.ConsoleTail(inst))
}

// capHex turns the two 32-bit words of /proc/sys/kernel/usermodehelper/bset
// (low, high) into the hex form of /proc/<pid>/status.
func capHex(t *testing.T, words string) string {
	t.Helper()
	fields := strings.Fields(words)
	require.Len(t, fields, 2, "usermodehelper bset %q", words)
	lo, err := strconv.ParseUint(fields[0], 10, 32)
	require.NoError(t, err)
	hi, err := strconv.ParseUint(fields[1], 10, 32)
	require.NoError(t, err)
	return strconv.FormatUint(hi<<32|lo, 16)
}

// assertNoMACAdmin checks that no CapBnd line of the grep output has
// CAP_MAC_ADMIN.
func assertNoMACAdmin(t *testing.T, capBnd string) {
	t.Helper()
	lines := strings.Split(capBnd, "\n")
	require.NotEmpty(t, lines)
	for _, line := range lines {
		fields := strings.Fields(line)
		require.NotEmpty(t, fields, "CapBnd line %q", line)
		v, err := strconv.ParseUint(fields[len(fields)-1], 16, 64)
		require.NoError(t, err, "CapBnd line %q", line)
		assert.Zero(t, v&(1<<capMACAdmin), "CAP_MAC_ADMIN in the bounding set: %s", line)
	}
}
