//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// TestRestart proves a VM outlives its vm-manager: with the systemd
// launcher QEMU and swtpm are transient units of the user manager, so a
// SIGTERM to the server leaves them running, and the next server on the
// same state dir reattaches (same PID, same IP, ready state, exec_vm and the
// console continue) and can stop and delete the VM, taking the units down.
func TestRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), apiTestTimeout)
	defer cancel()
	requireHost(t)
	requireUserManager(t)
	image := locateImage(t)
	dir := stateDir(t)
	_, testKey := generateSSHKey(t, dir)

	srv := startServer(ctx, t, dir, image.Dir, flagLearnGolden)
	m := newMCPClient(ctx, t, srv.URL)
	var n vm.NetworkInfo
	m.call(ctx, api.ToolCreateNetwork, map[string]any{"name": restartNetwork, "cidr": restartCIDR}, &n)
	var v vm.VM
	m.call(ctx, api.ToolCreateVM, map[string]any{
		"name":                restartVMName,
		"network":             restartNetwork,
		"ssh_authorized_keys": []string{testKey},
		"require_attestation": false,
		"wait_for":            string(vm.WaitReady),
	}, &v)
	require.Equal(t, vm.StateReady, v.State, "create_vm with wait_for ready: lastError %q\n%s", v.LastError, srv.logTail())
	procs := vmProcesses(t, v)
	qemuPID, unit := v.Processes.QEMU.PID, v.Processes.QEMU.Unit
	t.Logf("vm %s ip %s qemu pid %d unit %s swtpm pid %d unit %s", v.ID, v.IP, qemuPID, unit, v.Processes.TPM.PID, v.Processes.TPM.Unit)
	require.Equal(t, "active", unitProperty(t, unit, "ActiveState"), "qemu unit before the restart")
	assert.NotContains(t, childrenOf(t, srv.pid()), qemuPID, "qemu is systemd's child, not the server's")

	g := &guest{m: m, id: v.ID}
	g.waitSSH(ctx)
	bootID := g.sh(ctx, "cat /proc/sys/kernel/random/boot_id")
	g.sh(ctx, "echo "+restartMarker+" > /run/restart-marker")
	var before api.ConsoleResponse
	m.call(ctx, api.ToolGetVMConsole, map[string]any{"id": v.ID, "lines": 10000}, &before)
	require.Contains(t, before.Console, consoleMultiUser)

	// SIGTERM: the server exits within its stop timeout and leaves the VM.
	srv.stop()
	require.True(t, alive(qemuPID), "qemu died with the server")
	require.Equal(t, "active", unitProperty(t, unit, "ActiveState"), "qemu unit after the server exited")
	for pid, comm := range procs {
		assert.True(t, alive(pid), "%s (pid %d) died with the server", comm, pid)
	}
	assert.Contains(t, srv.logTail(), "vm left running", "the server logged the handover")

	// The next server on the same state dir reattaches before it is ready.
	srv2 := startServer(ctx, t, dir, image.Dir, flagLearnGolden)
	reattachSeconds := srv2.startup.Seconds()
	m2 := newMCPClient(ctx, t, srv2.URL)
	var after vm.VM
	m2.call(ctx, api.ToolGetVM, map[string]any{"id": v.ID}, &after)
	require.Equal(t, vm.StateReady, after.State, "get_vm after the restart: lastError %q\n%s", after.LastError, srv2.logTail())
	assert.Empty(t, after.LastError)
	assert.Equal(t, v.IP, after.IP)
	assert.Equal(t, v.MAC, after.MAC)
	assert.Equal(t, v.ReadyAt, after.ReadyAt, "the boot is the same one")
	require.NotNil(t, after.Processes)
	assert.Equal(t, qemuPID, after.Processes.QEMU.PID)
	assert.Equal(t, unit, after.Processes.QEMU.Unit)
	assert.Contains(t, srv2.logTail(), "vm reattached")

	// The guest is the same instance, reachable as before, and the console
	// file keeps growing where it left off.
	g2 := &guest{m: m2, id: v.ID}
	g2.waitSSH(ctx)
	assert.Equal(t, bootID, g2.sh(ctx, "cat /proc/sys/kernel/random/boot_id"), "the guest was not rebooted")
	assert.Equal(t, restartMarker, g2.sh(ctx, "cat /run/restart-marker"))
	g2.sh(ctx, "echo "+restartMarker+"-after > /dev/console")
	require.Eventually(t, func() bool {
		var console api.ConsoleResponse
		m2.call(ctx, api.ToolGetVMConsole, map[string]any{"id": v.ID, "lines": 10000}, &console)
		return strings.HasPrefix(console.Console, before.Console[:min(len(before.Console), 4096)]) &&
			strings.Contains(console.Console, restartMarker+"-after")
	}, 30*time.Second, pollEvery, "the console continues in the same file")
	var fwd api.ForwardResponse
	m2.call(ctx, api.ToolForwardPort, map[string]any{"id": v.ID, "port": 22}, &fwd)
	assertSSHBanner(t, fwd.Address)

	// Stop through the reattached instance: units gone, no processes left.
	var stopped vm.VM
	m2.call(ctx, api.ToolStopVM, map[string]any{"id": v.ID}, &stopped)
	require.Eventually(t, func() bool {
		m2.call(ctx, api.ToolGetVM, map[string]any{"id": v.ID}, &stopped)
		return stopped.State == vm.StateStopped
	}, stopTimeout+30*time.Second, pollEvery, "stop_vm: state %s lastError %q", stopped.State, stopped.LastError)
	assert.Empty(t, stopped.LastError, "a requested stop is not an error")
	assert.Nil(t, stopped.Processes)
	assertGone(t, procs, childrenGoneWithin, "stop_vm")
	assert.Equal(t, "not-found", unitProperty(t, unit, "LoadState"), "qemu unit released")
	assert.Equal(t, "not-found", unitProperty(t, v.Processes.TPM.Unit, "LoadState"), "swtpm unit released")

	// Start again under the second server, then delete: everything goes.
	var restarted vm.VM
	m2.call(ctx, api.ToolStartVM, map[string]any{"id": v.ID}, &restarted)
	require.Eventually(t, func() bool {
		m2.call(ctx, api.ToolGetVM, map[string]any{"id": v.ID}, &restarted)
		return restarted.State == vm.StateReady
	}, createVMWithin, pollEvery, "start_vm after reattach: state %s lastError %q", restarted.State, restarted.LastError)
	secondRun := vmProcesses(t, restarted)
	var deleted api.DeletedResponse
	m2.call(ctx, api.ToolDeleteVM, map[string]any{"id": v.ID}, &deleted)
	assert.True(t, deleted.Deleted)
	assertGone(t, secondRun, childrenGoneWithin, "delete_vm")
	assert.Equal(t, "not-found", unitProperty(t, unit, "LoadState"))
	m2.call(ctx, api.ToolDeleteNetwork, map[string]any{"name": restartNetwork}, &deleted)
	assert.True(t, deleted.Deleted)
	srv2.stop()

	t.Logf("reattach_seconds=%.2f", reattachSeconds)
	fmt.Printf("reattach_seconds=%.2f\n", reattachSeconds)
}

const (
	restartNetwork = "e2e-restart"
	restartCIDR    = "192.168.141.0/24"
	restartVMName  = "restart-e2e"
	restartMarker  = "survived-vm-manager-restart"
)

// requireUserManager skips the test where the systemd launcher would not be
// chosen: the tests run unprivileged, so they need the user's own manager.
func requireUserManager(t *testing.T) {
	t.Helper()
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		t.Skip("XDG_RUNTIME_DIR unset: no user service manager for the systemd launcher")
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "systemd", "private")); err != nil {
		t.Skipf("no user service manager: %v", err)
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skipf("systemd-run: %v", err)
	}
}

// unitProperty asks the user manager for one property of a unit.
func unitProperty(t *testing.T, unit, property string) string {
	t.Helper()
	out, err := exec.Command("systemctl", "--user", "show", "--property="+property, "--value", unit).CombinedOutput() // #nosec G204 -- fixed binary
	require.NoError(t, err, "systemctl show %s: %s", unit, out)
	return strings.TrimSpace(string(out))
}
