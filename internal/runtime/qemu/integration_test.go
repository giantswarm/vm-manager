//go:build integration

package qemu_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/tpm"
)

const (
	ovmfCode = "/usr/share/edk2/x64/OVMF_CODE.4m.fd"
	ovmfVars = qemu.DefaultOVMFVarsTemplate
)

// firmwareVM starts a real QEMU with OVMF, swtpm, user networking and a
// blank target disk: no OS, so the firmware ends up in its shell, which is
// enough to prove the process, QMP and serial plumbing. It skips when the
// host cannot run it.
func firmwareVM(t *testing.T, ctx context.Context) (*qemu.Instance, *tpm.Instance) {
	t.Helper()
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Skipf("/dev/kvm: %v", err)
	} else {
		_ = f.Close()
	}
	for _, bin := range []string{qemu.DefaultBinary, tpm.DefaultBinary} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	for _, fw := range []string{ovmfCode, ovmfVars} {
		if _, err := os.Stat(fw); err != nil {
			t.Skipf("firmware: %v", err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	dir := t.TempDir()

	tp, err := tpm.New(tpm.Options{Logger: log}).Start(ctx, tpm.Config{StateDir: filepath.Join(dir, "tpm")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tp.Kill() })

	target := filepath.Join(dir, "target.raw")
	require.NoError(t, os.WriteFile(target, nil, 0o600))
	require.NoError(t, os.Truncate(target, 64<<20))

	creds := map[string]string{qemu.CredentialMachineID: "0123456789abcdef0123456789abcdef"}
	if nl, err := qemu.ListenNotify(0, log); err == nil {
		t.Cleanup(func() { _ = nl.Close() })
		creds[qemu.CredentialNotifySocket] = nl.Credential()
	} else {
		t.Logf("vsock notify listener unavailable, credential omitted: %v", err)
	}
	spec := qemu.Spec{
		ID:          "integration",
		Phase:       qemu.PhaseBoot,
		CPUs:        1,
		MemoryMiB:   512,
		Target:      target,
		Netdevs:     []qemu.Netdev{{ID: "net0", Backend: "user", MAC: "52:54:00:12:34:56"}},
		Credentials: creds,
		VsockCID:    uint32(0x10000 + os.Getpid()%0x10000), // #nosec G115 -- bounded well inside MinCID..MaxCID
		TPMSocket:   tp.SocketPath(),
		OVMFCode:    ovmfCode,
		OVMFVars:    filepath.Join(dir, "OVMF_VARS.fd"),
		SerialLog:   filepath.Join(dir, "serial.log"),
		QMPSocket:   filepath.Join(dir, "qmp.sock"),
	}
	inst, err := qemu.New(qemu.Options{Logger: log, OVMFVarsTemplate: ovmfVars}).Start(ctx, spec)
	require.NoError(t, err)
	t.Cleanup(func() { _ = inst.Kill() })
	return inst, tp
}

func waitExit(t *testing.T, inst *qemu.Instance, within time.Duration) {
	t.Helper()
	select {
	case st := <-inst.Wait():
		t.Logf("qemu exited: %s; stderr: %q", st, inst.Stderr())
	case <-time.After(within):
		t.Fatalf("qemu still running; stderr: %q", inst.Stderr())
	}
}

// TestIntegrationFirmwareBoot boots the firmware, waits for QMP to report
// the machine running, quits it and checks the serial log and that swtpm
// followed QEMU out.
func TestIntegrationFirmwareBoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	inst, tp := firmwareVM(t, ctx)
	assert.Positive(t, inst.PID())

	require.Eventually(t, func() bool {
		st, err := inst.QMP().QueryStatus(ctx)
		return err == nil && st.Running && st.Status == "running"
	}, 30*time.Second, 200*time.Millisecond, "stderr: %q", inst.Stderr())

	assert.FileExists(t, inst.Spec().SerialLog)
	assert.FileExists(t, inst.Spec().OVMFVars)
	require.NoError(t, inst.QMP().Quit(ctx))
	waitExit(t, inst, 15*time.Second)
	assert.True(t, inst.Exited())

	select {
	case st := <-tp.Wait():
		t.Logf("swtpm exited with qemu: %s", st)
	case <-time.After(10 * time.Second):
		t.Fatal("swtpm did not terminate after qemu closed the control channel")
	}
}

// TestIntegrationStopEscalates stops a guest that cannot handle ACPI
// powerdown (there is no OS): Stop must fall through to SIGTERM, which QEMU
// answers with a clean exit and a SHUTDOWN event whose reason is
// host-signal, and the process must be gone when Stop returns.
func TestIntegrationStopEscalates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	inst, _ := firmwareVM(t, ctx)

	sctx, scancel := context.WithTimeout(ctx, 2*time.Second)
	defer scancel()
	start := time.Now()
	require.NoError(t, inst.Stop(sctx))
	assert.True(t, inst.Exited())
	assert.Less(t, time.Since(start), 10*time.Second)
	st := <-inst.Wait()
	assert.Equal(t, 0, st.Code, "qemu exits cleanly on SIGTERM: %s; stderr: %q", st, inst.Stderr())

	var shutdown struct {
		Guest  bool   `json:"guest"`
		Reason string `json:"reason"`
	}
	for ev := range inst.QMP().Events() {
		if ev.Name == qemu.EventShutdown {
			require.NoError(t, json.Unmarshal(ev.Data, &shutdown))
		}
	}
	assert.Equal(t, "host-signal", shutdown.Reason)
	assert.False(t, shutdown.Guest)
}
