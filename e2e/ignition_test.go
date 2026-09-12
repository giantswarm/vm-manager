//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// The test's network and VMs; the CIDR stays clear of TestNetworkIMDS and of
// the server's default network.
const (
	ignitionNetwork = "e2e-ignition"
	ignitionCIDR    = "192.168.141.0/24"
	ignitionVMName  = "ignition-e2e"
	ignitionGatedVM = "ignition-gated-e2e"
)

// What the user-data asks Ignition to do, and where the proof shows up.
const (
	ignitionEtcFile    = "/etc/vm-manager-ignition"
	ignitionEtcContent = "written by ignition-files.service on the first installed boot\n"
	ignitionUnit       = "ignition-e2e.service"
	ignitionRunMarker  = "/run/ignition-e2e-ran"
	// ignitionConfigCache is where the fetch stage caches the parsed config
	// for the later stages. The initrd's /run is the system's /run, so the
	// file outlives the switch root and its presence tells whether Ignition
	// fetched anything in this boot.
	ignitionConfigCache = "/run/ignition.json"
	// Ignition's own log lines (internal/exec/engine.go, internal/main.go,
	// internal/resource/http.go of v2.27.0), as journald records them from
	// the units' stdout.
	ignitionFetchPassed = "fetch passed"
	ignitionFilesPassed = "files passed"
	ignitionFinished    = "Ignition finished successfully"
	ignitionFetchRetry  = "GET http://169.254.169.254/giantswarm/v1/user-data: attempt #2"
	ignitionGatedResult = "GET result: Service Unavailable"
)

// Ceilings on top of the ones in network_imds_test.go: a reboot is one
// installed boot; the gated VM only has to reach the initrd's fetch loop.
const (
	ignitionTestTimeout = 15 * time.Minute
	rebootReadyWithin   = 3 * time.Minute
	gatedBootWithin     = 2 * time.Minute
	gatedRetriesWithin  = 60 * time.Second
)

// ignitionConfig is the Ignition v3.4.0 user-data: one file with mode 0644 and
// one enabled oneshot unit, the shape of what CAPI's kubeadm bootstrap
// provider emits with format ignition (files plus units), minus kubeadm.
func ignitionConfig(t *testing.T) string {
	t.Helper()
	unit := "[Unit]\nDescription=vm-manager e2e: written and enabled by Ignition\n\n" +
		"[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/usr/bin/touch " + ignitionRunMarker + "\n\n" +
		"[Install]\nWantedBy=multi-user.target\n"
	cfg := map[string]any{
		"ignition": map[string]any{"version": "3.4.0"},
		"storage": map[string]any{
			"files": []map[string]any{{
				"path":      ignitionEtcFile,
				"mode":      0o644,
				"overwrite": true,
				"contents":  map[string]any{"source": "data:;base64," + base64.StdEncoding.EncodeToString([]byte(ignitionEtcContent))},
			}},
		},
		"systemd": map[string]any{
			"units": []map[string]any{{"name": ignitionUnit, "enabled": true, "contents": unit}},
		},
	}
	b, err := json.Marshal(cfg)
	require.NoError(t, err)
	return string(b)
}

// TestIgnition proves the user-data path end to end through the MCP API:
// create_vm with an Ignition config and require_attestation false, and the
// first installed boot has the file, the unit (enabled, ran) and a clean
// Ignition journal; a reboot_vm later shows Ignition idle, because
// ignition.firstboot is on the kernel command line of the first installed
// boot only. The gated subtest documents what a VM with require_attestation
// true does today, without an initrd attestation agent: the IMDS answers 503
// and Ignition's fetch stage retries until its own timeout.
func TestIgnition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), ignitionTestTimeout)
	defer cancel()
	requireHost(t)
	image := locateImage(t)
	dir := stateDir(t)
	_, testKey := generateSSHKey(t, dir)

	srv := startServer(ctx, t, dir, image.Dir)
	m := newMCPClient(ctx, t, srv.URL)
	m.call(ctx, api.ToolCreateNetwork, map[string]any{"name": ignitionNetwork, "cidr": ignitionCIDR}, nil)
	userData := ignitionConfig(t)

	t.Run("first boot applies user-data, reboot leaves Ignition idle", func(t *testing.T) {
		started := time.Now()
		var v vm.VM
		m.call(ctx, api.ToolCreateVM, map[string]any{
			"name":                ignitionVMName,
			"hostname":            ignitionVMName,
			"network":             ignitionNetwork,
			"ssh_authorized_keys": []string{testKey},
			"user_data":           userData,
			"require_attestation": false,
			"wait_for":            string(vm.WaitReady),
		}, &v)
		t.Logf("create_vm returned after %.1fs: id %s state %s ip %s", time.Since(started).Seconds(), v.ID, v.State, v.IP)
		require.Equal(t, vm.StateReady, v.State, "create_vm with wait_for ready: lastError %q\n%s", v.LastError, srv.logTail())
		assert.True(t, v.Attestation.UserDataReleased, "require_attestation false releases user-data at boot")
		require.NotNil(t, v.BootedAt)
		firstBoot := *v.BootedAt

		g := &guest{m: m, id: v.ID}
		g.waitSSH(ctx)
		assertIgnitionApplied(ctx, t, g)

		m.call(ctx, api.ToolRebootVM, map[string]any{"id": v.ID}, &v)
		v = waitRebooted(ctx, t, m, v.ID, firstBoot)
		assertIgnitionIdle(ctx, t, m, v.ID)

		var deleted api.DeletedResponse
		m.call(ctx, api.ToolDeleteVM, map[string]any{"id": v.ID}, &deleted)
		assert.True(t, deleted.Deleted)
	})

	t.Run("gated user-data keeps the fetch stage retrying (current behaviour)", func(t *testing.T) {
		var v vm.VM
		m.call(ctx, api.ToolCreateVM, map[string]any{
			"name":                ignitionGatedVM,
			"hostname":            ignitionGatedVM,
			"network":             ignitionNetwork,
			"user_data":           userData,
			"require_attestation": true,
			"wait_for":            string(vm.WaitInstalled),
		}, &v)
		require.NotNil(t, v.InstalledAt, "create_vm with wait_for installed: state %s lastError %q", v.State, v.LastError)
		t.Cleanup(func() {
			var deleted api.DeletedResponse
			m.call(ctx, api.ToolDeleteVM, map[string]any{"id": v.ID}, &deleted)
			assert.True(t, deleted.Deleted)
		})

		v = waitFor(ctx, t, m, v.ID, gatedBootWithin, "installed boot started", func(v vm.VM) bool { return v.BootedAt != nil })
		assert.True(t, v.Attestation.Required)
		assert.False(t, v.Attestation.UserDataReleased, "get_vm must report user-data as not released")

		// No initrd attestation agent posts a quote yet, so the IMDS keeps
		// answering 503 and Ignition keeps retrying (internal/resource/http.go
		// of v2.27.0 retries every status >= 500 with 200 ms..5 s backoff
		// until --fetch-timeout, 2 minutes). The VM is deleted while still
		// in that loop.
		var console api.ConsoleResponse
		deadline := time.Now().Add(gatedRetriesWithin)
		for {
			m.call(ctx, api.ToolGetVMConsole, map[string]any{"id": v.ID, "lines": 10000}, &console)
			if strings.Contains(console.Console, ignitionFetchRetry) && strings.Contains(console.Console, ignitionGatedResult) {
				break
			}
			require.False(t, time.Now().After(deadline), "no Ignition fetch retries on the console within %s; last lines:\n%s", gatedRetriesWithin, tail(console.Console, 30))
			time.Sleep(pollEvery)
		}
		t.Logf("gated fetch loop on the console:\n%s", tail(console.Console, 12))
		m.call(ctx, api.ToolGetVM, map[string]any{"id": v.ID}, &v)
		assert.False(t, v.Attestation.UserDataReleased, "user-data stays gated without a verified initrd quote")
		assert.Nil(t, v.ReadyAt, "the guest cannot reach READY=1 while its initrd waits for user-data")
	})

	var deleted api.DeletedResponse
	m.call(ctx, api.ToolDeleteNetwork, map[string]any{"name": ignitionNetwork}, &deleted)
	assert.True(t, deleted.Deleted)
	srv.stop()
}

// assertIgnitionApplied checks the first installed boot: the kernel command
// line carried ignition.firstboot, the fetch stage cached the config, the
// file and the unit are there and the four stages logged success without
// leaving anything failed.
func assertIgnitionApplied(ctx context.Context, t *testing.T, g *guest) {
	t.Helper()
	cmdline := g.sh(ctx, "cat /proc/cmdline")
	assert.Contains(t, cmdline, "ignition.firstboot", "first installed boot cmdline")
	assert.Contains(t, cmdline, "ignition.config.url=http://169.254.169.254/giantswarm/v1/user-data", "UKI cmdline")
	assert.Equal(t, "cached", g.sh(ctx, fmt.Sprintf("test -s %s && echo cached", ignitionConfigCache)), "%s from the fetch stage", ignitionConfigCache)

	assert.Equal(t, strings.TrimSpace(ignitionEtcContent), g.sh(ctx, "cat "+ignitionEtcFile), ignitionEtcFile)
	assert.Equal(t, "644 root root", g.sh(ctx, "stat -c '%a %U %G' "+ignitionEtcFile), ignitionEtcFile+" mode and owner")

	show := g.sh(ctx, "systemctl show -p UnitFileState -p ActiveState -p Result "+ignitionUnit)
	assert.Contains(t, show, "UnitFileState=enabled", ignitionUnit)
	assert.Contains(t, show, "ActiveState=active", ignitionUnit)
	assert.Contains(t, show, "Result=success", ignitionUnit)
	assert.Equal(t, "ran", g.sh(ctx, fmt.Sprintf("test -f %s && echo ran", ignitionRunMarker)), "%s from %s", ignitionRunMarker, ignitionUnit)

	journal := g.sh(ctx, "journalctl -b -o cat -u 'ignition-*' --no-pager")
	t.Logf("ignition journal of the first installed boot:\n%s", journal)
	for _, want := range []string{ignitionFetchPassed, ignitionFilesPassed, ignitionFinished} {
		assert.Contains(t, journal, want, "ignition journal")
	}
	assert.NotContains(t, journal, "Ignition failed", "ignition journal")
	assert.Equal(t, 5, strings.Count(journal, ignitionFinished), "one success per stage: fetch, disks, mount, files, and umount (ExecStop= of ignition-mount.service at the switch root)")

	assert.Empty(t, g.sh(ctx, "systemctl --failed --no-legend --plain"), "failed units after Ignition ran")
	assert.Equal(t, "running", g.sh(ctx, "systemctl is-system-running || true"), "system state")
}

// assertIgnitionIdle checks a later boot through its serial console (the
// console log is the current QEMU process's, so it holds this boot only):
// ignition-complete.target was reached, but no stage started and Ignition
// logged nothing, because ignition.firstboot is missing from the command
// line; the guest still reached multi-user. It reads the console rather than
// exec_vm: sshd regenerates its host key on the volatile /etc, so exec_vm
// refuses the rebooted guest ("host key changed") until /etc is persistent,
// which is a separate change, as is asserting that the file and the enabled
// unit survived the reboot.
func assertIgnitionIdle(ctx context.Context, t *testing.T, m *mcpClient, id string) {
	t.Helper()
	var console api.ConsoleResponse
	m.call(ctx, api.ToolGetVMConsole, map[string]any{"id": id, "lines": 10000}, &console)
	assert.Contains(t, console.Console, "Ignition Complete", "a later boot still reaches ignition-complete.target; last lines:\n%s", tail(console.Console, 20))
	assert.Contains(t, console.Console, consoleMultiUser, "multi-user after the reboot; last lines:\n%s", tail(console.Console, 20))
	for _, unwanted := range []string{"Starting Ignition (", "ignition[", "Ignition failed", "Emergency Mode"} {
		assert.NotContains(t, console.Console, unwanted, "console of a later boot")
	}
}

// waitRebooted polls get_vm until the VM is ready again in a boot that
// started after firstBoot.
func waitRebooted(ctx context.Context, t *testing.T, m *mcpClient, id string, firstBoot time.Time) vm.VM {
	t.Helper()
	return waitFor(ctx, t, m, id, rebootReadyWithin, "ready after reboot_vm", func(v vm.VM) bool {
		return v.State == vm.StateReady && v.BootedAt != nil && v.BootedAt.After(firstBoot) && v.ReadyAt != nil
	})
}

// waitFor polls get_vm until done reports true, failing the test on a failed
// VM or once within has elapsed.
func waitFor(ctx context.Context, t *testing.T, m *mcpClient, id string, within time.Duration, what string, done func(vm.VM) bool) vm.VM {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var v vm.VM
		m.call(ctx, api.ToolGetVM, map[string]any{"id": id}, &v)
		require.NotEqual(t, vm.StateFailed, v.State, "waiting for %s: lastError %q", what, v.LastError)
		if done(v) {
			return v
		}
		require.False(t, time.Now().After(deadline), "%s not reached within %s: state %s lastError %q", what, within, v.State, v.LastError)
		time.Sleep(pollEvery)
	}
}
