package qemu

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/runtime/proc"
)

// qmpPathOf extracts the QMP socket path from a QEMU argument list, undoing
// the comma escaping Command applies.
func qmpPathOf(t *testing.T, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "-qmp" && i+1 < len(args) {
			p := strings.TrimSuffix(strings.TrimPrefix(args[i+1], "unix:"), ",server=on,wait=off")
			return strings.ReplaceAll(p, ",,", ",")
		}
	}
	t.Fatal("no -qmp argument")
	return ""
}

// harness is a Runtime on a FakeExec whose QEMU is a fakeQMP server.
type harness struct {
	rt       *Runtime
	exec     *proc.FakeExec
	spec     Spec
	template string
	qmp      *fakeQMP
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{template: filepath.Join(dir, "OVMF_VARS.fd")}
	require.NoError(t, os.WriteFile(h.template, []byte("VARS"), 0o600))
	h.spec = bootSpec()
	h.spec.OVMFVars = filepath.Join(dir, "vm", "vars.fd")
	h.spec.SerialLog = filepath.Join(dir, "vm", "log", "serial.log")
	h.spec.QMPSocket = filepath.Join(dir, "run", "qmp.sock")
	h.exec = &proc.FakeExec{Hook: func(cmd proc.Cmd, _ *proc.FakeProcess) error {
		h.qmp = startFakeQMP(t, qmpPathOf(t, cmd.Args))
		return nil
	}}
	opts.Exec = h.exec
	opts.Logger = quiet
	opts.OVMFVarsTemplate = h.template
	h.rt = New(opts)
	return h
}

func (h *harness) start(t *testing.T) (*Instance, *proc.FakeProcess) {
	t.Helper()
	inst, err := h.rt.Start(context.Background(), h.spec)
	require.NoError(t, err)
	t.Cleanup(func() { _ = inst.Kill() })
	return inst, h.exec.Processes()[0]
}

func exited(t *testing.T, inst *Instance) proc.ExitStatus {
	t.Helper()
	select {
	case st := <-inst.Wait():
		return st
	case <-time.After(5 * time.Second):
		t.Fatal("qemu did not exit")
		return proc.ExitStatus{}
	}
}

func TestRuntimeStart(t *testing.T) {
	ctx := context.Background()
	t.Run("starts qemu and connects QMP", func(t *testing.T) {
		h := newHarness(t, Options{Binary: "/opt/qemu"})
		inst, p := h.start(t)
		want, err := Command(h.spec)
		require.NoError(t, err)
		require.Len(t, h.exec.Started(), 1)
		assert.Equal(t, "/opt/qemu", h.exec.Started()[0].Path)
		assert.Equal(t, want, h.exec.Started()[0].Args)
		assert.Equal(t, p.PID(), inst.PID())
		assert.Equal(t, h.spec, inst.Spec())
		assert.False(t, inst.Exited())
		assert.Empty(t, inst.Stderr())
		vars, err := os.ReadFile(h.spec.OVMFVars)
		require.NoError(t, err)
		assert.Equal(t, "VARS", string(vars), "vars store copied from the template")
		assert.DirExists(t, filepath.Dir(h.spec.SerialLog))
		st, err := inst.QMP().QueryStatus(ctx)
		require.NoError(t, err)
		assert.True(t, st.Running)
		assert.Equal(t, []string{"qmp_capabilities", "query-status"}, h.qmp.received())
	})
	t.Run("keeps an existing vars store and removes a stale QMP socket", func(t *testing.T) {
		h := newHarness(t, Options{})
		require.NoError(t, os.MkdirAll(filepath.Dir(h.spec.OVMFVars), 0o750))
		require.NoError(t, os.WriteFile(h.spec.OVMFVars, []byte("OLD"), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Dir(h.spec.QMPSocket), 0o750))
		require.NoError(t, os.WriteFile(h.spec.QMPSocket, []byte("stale"), 0o600))
		h.start(t)
		vars, err := os.ReadFile(h.spec.OVMFVars)
		require.NoError(t, err)
		assert.Equal(t, "OLD", string(vars))
		assert.Equal(t, DefaultBinary, h.exec.Started()[0].Path)
	})
	t.Run("invalid spec starts nothing", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.spec.VsockCID = 1
		_, err := h.rt.Start(ctx, h.spec)
		assert.ErrorIs(t, err, apierr.ErrInvalid)
		assert.Empty(t, h.exec.Started())
	})
	t.Run("missing vars template", func(t *testing.T) {
		h := newHarness(t, Options{})
		require.NoError(t, os.Remove(h.template))
		_, err := h.rt.Start(ctx, h.spec)
		assert.ErrorContains(t, err, "OVMF vars")
		assert.Empty(t, h.exec.Started())
		assert.NoFileExists(t, h.spec.OVMFVars, "a failed copy leaves no partial store")
	})
	t.Run("exec failure", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.exec.Hook = func(proc.Cmd, *proc.FakeProcess) error { return exec.ErrNotFound }
		_, err := h.rt.Start(ctx, h.spec)
		assert.ErrorIs(t, err, exec.ErrNotFound)
	})
	t.Run("qemu exits before QMP answers", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.exec.Hook = func(cmd proc.Cmd, p *proc.FakeProcess) error {
			_, _ = cmd.Stderr.Write([]byte("qemu-system-x86_64: -device vhost-vsock-pci,id=vsock0,guest-cid=4001: vhost-vsock: unable to set guest cid: Address already in use\n"))
			p.Exit(1)
			return nil
		}
		_, err := h.rt.Start(ctx, h.spec)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exited during start (exit status 1)")
		assert.Contains(t, err.Error(), "Address already in use")
	})
	t.Run("QMP never answers", func(t *testing.T) {
		h := newHarness(t, Options{StartTimeout: 100 * time.Millisecond})
		h.exec.Hook = nil
		_, err := h.rt.Start(ctx, h.spec)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not answer within 100ms")
		p := h.exec.Processes()[0]
		assert.True(t, p.Exited())
		assert.Equal(t, []os.Signal{syscall.SIGKILL}, p.Signals())
	})
	t.Run("context cancelled while waiting for QMP", func(t *testing.T) {
		h := newHarness(t, Options{})
		cctx, cancel := context.WithCancel(ctx)
		h.exec.Hook = func(proc.Cmd, *proc.FakeProcess) error { cancel(); return nil }
		_, err := h.rt.Start(cctx, h.spec)
		assert.ErrorIs(t, err, context.Canceled)
		assert.True(t, h.exec.Processes()[0].Exited())
	})
}

func TestInstanceStop(t *testing.T) {
	ctx := context.Background()
	t.Run("guest powers down", func(t *testing.T) {
		h := newHarness(t, Options{})
		inst, p := h.start(t)
		h.qmp.onPowerdown = func() { p.Exit(0) }
		require.NoError(t, inst.Stop(ctx))
		assert.Empty(t, p.Signals())
		assert.Equal(t, 0, exited(t, inst).Code)
		assert.True(t, inst.Exited())
		select {
		case <-inst.QMP().Done():
		case <-time.After(5 * time.Second):
			t.Fatal("QMP not closed after exit")
		}
		require.NoError(t, inst.Stop(ctx), "stopping again is a no-op")
	})
	t.Run("guest ignores powerdown, SIGTERM works", func(t *testing.T) {
		h := newHarness(t, Options{})
		inst, p := h.start(t)
		p.ExitOnTerm = true
		sctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		require.NoError(t, inst.Stop(sctx))
		assert.Equal(t, []os.Signal{syscall.SIGTERM}, p.Signals())
		assert.Equal(t, 143, exited(t, inst).Code)
		assert.Contains(t, h.qmp.received(), "system_powerdown")
	})
	t.Run("default powerdown timeout then escalation to SIGKILL", func(t *testing.T) {
		h := newHarness(t, Options{PowerdownTimeout: 30 * time.Millisecond, StopGrace: 30 * time.Millisecond})
		inst, p := h.start(t)
		require.NoError(t, inst.Stop(ctx))
		assert.Equal(t, []os.Signal{syscall.SIGTERM, syscall.SIGKILL}, p.Signals())
		assert.Equal(t, -1, exited(t, inst).Code)
	})
	t.Run("QMP dead goes straight to SIGTERM", func(t *testing.T) {
		h := newHarness(t, Options{})
		inst, p := h.start(t)
		p.ExitOnTerm = true
		h.qmp.closeConns()
		select {
		case <-inst.QMP().Done():
		case <-time.After(5 * time.Second):
			t.Fatal("QMP did not notice the drop")
		}
		require.NoError(t, inst.Stop(ctx))
		assert.Equal(t, []os.Signal{syscall.SIGTERM}, p.Signals())
	})
	t.Run("kill", func(t *testing.T) {
		h := newHarness(t, Options{})
		inst, p := h.start(t)
		require.NoError(t, inst.Kill())
		assert.Equal(t, []os.Signal{syscall.SIGKILL}, p.Signals())
		assert.Equal(t, -1, exited(t, inst).Code)
		require.NoError(t, inst.Kill(), "killing again is a no-op")
	})
	t.Run("events reach the caller", func(t *testing.T) {
		h := newHarness(t, Options{})
		inst, _ := h.start(t)
		h.qmp.emit(Event{Name: EventShutdown, Data: json.RawMessage(`{"guest":true,"reason":"guest-reset"}`)})
		select {
		case ev := <-inst.QMP().Events():
			assert.Equal(t, EventShutdown, ev.Name)
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
		}
	})
}
