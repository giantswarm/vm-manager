package vm_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/runtime/proc"
	"github.com/giantswarm/vm-manager/internal/vm"
	"github.com/giantswarm/vm-manager/internal/vm/vmtest"
)

func onDisk(t *testing.T, v *vm.VM) vm.VM {
	t.Helper()
	var rec vm.VM
	data, err := os.ReadFile(filepath.Join(v.Paths.Dir, vm.RecordFile))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &rec))
	return rec
}

func TestDetachOnCloseAndReattach(t *testing.T) {
	h := newHarness(t)
	h.detach = true
	h.restart()

	v := h.create("node")
	boot := h.install(v.ID)
	v = h.ready(v.ID, v.CID)
	require.NotNil(t, v.Processes, "a running VM records its processes")
	assert.Equal(t, proc.UnitName(v.ID, "qemu"), v.Processes.QEMU.Unit)
	assert.Equal(t, proc.UnitName(v.ID, "swtpm"), v.Processes.TPM.Unit)
	assert.Equal(t, boot.Handle(), v.Processes.QEMU)

	// Close leaves the VM alone: no stop, no kill, swtpm still up, the
	// record on disk still live with its handles.
	h.ev.Reset()
	require.NoError(t, h.svc.Close(h.ctx))
	events := h.ev.List()
	assert.Contains(t, events, "qemu.detach")
	assert.NotContains(t, events, "qemu.stop")
	assert.NotContains(t, events, "qemu.kill")
	assert.NotContains(t, events, "tpm.stop")
	assert.Equal(t, 1, h.tpm.Running())
	rec := onDisk(t, v)
	assert.Equal(t, vm.StateReady, rec.State)
	assert.Equal(t, v.Processes, rec.Processes)

	// The next vm-manager finds the processes and picks them up: volume,
	// lease, swtpm, QEMU, supervisor; the state is untouched.
	h.rt.SetAttachable(v.Processes.QEMU.PID)
	h.nets = vmtest.NewNetworks(h.ev)
	h.ev.Reset()
	h.start()
	for _, want := range []string{"storage.acquire:open", "tpm.attach", "qemu.attach:boot"} {
		assert.Contains(t, h.ev.List(), want)
	}
	after, err := h.svc.Get(v.ID)
	require.NoError(t, err)
	assert.Equal(t, vm.StateReady, after.State)
	assert.Empty(t, after.LastError)
	assert.Equal(t, v.IP, after.IP)
	assert.Equal(t, v.Processes, after.Processes)
	attached := h.waitInstances(3)
	assert.Equal(t, v.ID, attached.Spec().ID)

	// A notification still reaches the reattached VM, and an exit after
	// the reattach is handled like any other: swtpm stopped, state settled.
	require.Eventually(t, func() bool { return h.notify.Notify(v.CID, map[string]string{"STATUS": "after restart"}) }, waitAtMost, waitEvery)
	require.Eventually(t, func() bool { g, _ := h.svc.Get(v.ID); return g.Status == "after restart" }, waitAtMost, waitEvery)
	attached.Exit(0)
	after = h.waitState(v.ID, vm.StateStopped)
	assert.Equal(t, "guest shut down", after.LastError)
	assert.Nil(t, after.Processes)
	assert.Equal(t, 0, h.tpm.Running())

	// Start, detach again, reattach with the swtpm gone, then Stop and
	// Delete through the reattached instance.
	_, err = h.svc.Start(h.ctx, v.ID)
	require.NoError(t, err)
	h.waitInstances(4)
	v = h.ready(v.ID, v.CID)
	h.rt.SetAttachable(v.Processes.QEMU.PID)
	h.tpm.AttachErr = proc.ErrGone
	h.restart()
	after, err = h.svc.Get(v.ID)
	require.NoError(t, err)
	assert.Equal(t, vm.StateReady, after.State, "QEMU runs on without its swtpm")
	_, err = h.svc.Stop(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Contains(t, h.ev.List(), "qemu.stop")
	h.waitState(v.ID, vm.StateStopped)
	require.NoError(t, h.svc.Delete(h.ctx, v.ID))
	assert.NoDirExists(t, v.Paths.Dir)
}

func TestLoadWithoutReattachableProcesses(t *testing.T) {
	h := newHarness(t)
	h.detach = true
	h.restart()
	v := h.create("node")
	h.install(v.ID)
	v = h.ready(v.ID, v.CID)

	// The launcher's processes did not survive (or were plain children):
	// the VM is stopped with the reason, the volume closed, Start works.
	h.ev.Reset()
	h.restart()
	after, err := h.svc.Get(v.ID)
	require.NoError(t, err)
	assert.Equal(t, vm.StateStopped, after.State)
	assert.Contains(t, after.LastError, "vm-manager restarted while the VM was ready")
	assert.Contains(t, after.LastError, "process is gone")
	assert.Nil(t, after.Processes)
	assert.Contains(t, h.ev.List(), "tpm.stop", "a swtpm whose QEMU is gone is stopped, not orphaned")
	assert.Equal(t, 0, h.tpm.Running())
	h.ev.Reset()
	_, err = h.svc.Start(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Contains(t, h.ev.List(), "storage.acquire:open", "the volume was released with the failed reattach")
}

func TestLoadReattachResumesStop(t *testing.T) {
	h := newHarness(t)
	h.detach = true
	h.restart()
	v := h.create("node")
	h.install(v.ID)
	v = h.ready(v.ID, v.CID)

	// A stop interrupted by the restart: the record says stopping. The
	// resumed stop may settle the VM before Load is done with it, so what
	// Load logs is what the reattach found.
	const pid = 4242
	restartStopping := func(v *vm.VM) *logBuffer {
		t.Helper()
		rec := onDisk(t, v)
		require.NoError(t, h.svc.Close(h.ctx))
		rec.State = vm.StateStopping
		rec.Processes.QEMU.PID = pid
		require.NoError(t, vm.WriteJSON(filepath.Join(v.Paths.Dir, vm.RecordFile), rec))
		h.rt.SetAttachable(pid)
		h.nets = vmtest.NewNetworks(h.ev)
		h.logs = &logBuffer{}
		h.ev.Reset()
		h.start()
		return h.logs
	}
	reattached := fmt.Sprintf(`msg="vm reattached" id=%s state=stopping pid=%d`, v.ID, pid)

	logs := restartStopping(v)
	after := h.waitState(v.ID, vm.StateStopped)
	assert.Contains(t, h.ev.List(), "qemu.stop", "the stop is resumed")
	assert.Nil(t, after.Processes)
	assert.Contains(t, logs.String(), reattached)

	// The same with a QEMU that exits while it is reattached: its
	// supervisor settles the record right away, concurrently with Load.
	_, err := h.svc.Start(h.ctx, v.ID)
	require.NoError(t, err)
	h.waitInstances(4)
	v = h.ready(v.ID, v.CID)
	h.rt.SetOnAttach(func(i *vmtest.Instance) { i.Exit(0) })
	logs = restartStopping(v)
	after = h.waitState(v.ID, vm.StateStopped)
	assert.Nil(t, after.Processes)
	assert.Equal(t, 0, h.tpm.Running())
	assert.Contains(t, logs.String(), reattached)
}
