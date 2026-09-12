package qemu

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/runtime/proc"
)

func TestRuntimeAttach(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, Options{})
	h.spec.ProcessLog = filepath.Join(t.TempDir(), "qemu.log")
	inst, p := h.start(t)
	cmd := h.exec.Started()[0]
	assert.Equal(t, proc.UnitName(h.spec.ID, UnitRole), cmd.Unit, "the unit is named after the VM")
	assert.Equal(t, h.spec.ProcessLog, cmd.Log)
	assert.Equal(t, proc.Handle{Unit: cmd.Unit, PID: p.PID()}, inst.Handle())

	// The next vm-manager: the same launcher after a restart.
	require.NoError(t, inst.Detach(), "the first instance lets go of QMP")
	again, err := h.rt.Attach(ctx, h.spec, inst.Handle())
	require.NoError(t, err)
	assert.Equal(t, p.PID(), again.PID())
	assert.False(t, again.Exited())
	require.NotNil(t, again.QMP(), "QMP reconnected")
	require.NoError(t, os.WriteFile(h.spec.ProcessLog, []byte("qemu: warning\n"), 0o600))
	assert.Equal(t, "qemu: warning", again.Stderr(), "stderr comes from the process log")

	// One that exited meanwhile is settled, not failed. The fake QMP server
	// would still answer; a real QEMU takes its socket with it.
	p.Exit(3)
	st := exited(t, again)
	assert.Equal(t, 3, st.Code)
	require.NoError(t, os.Remove(h.spec.QMPSocket))
	gone, err := h.rt.Attach(ctx, h.spec, inst.Handle())
	require.NoError(t, err)
	assert.True(t, gone.Exited())
	assert.Nil(t, gone.QMP())
	assert.Equal(t, 3, exited(t, gone).Code)
	require.NoError(t, gone.Stop(ctx), "stop of an exited instance is a no-op")

	_, err = h.rt.Attach(ctx, h.spec, proc.Handle{Unit: "vm-manager-nope-qemu", PID: 1})
	assert.ErrorIs(t, err, proc.ErrGone)

	_, err = New(Options{Exec: proc.OSExec{}}).Attach(ctx, h.spec, inst.Handle())
	assert.ErrorIs(t, err, proc.ErrNoReattach)
}
