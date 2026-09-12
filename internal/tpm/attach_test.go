package tpm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/runtime/proc"
)

func TestAttach(t *testing.T) {
	ctx := context.Background()
	fake := &proc.FakeExec{Hook: createSocket}
	dir := filepath.Join(t.TempDir(), "tpm")
	cfg := Config{ID: "ab12", StateDir: dir, Log: filepath.Join(dir, LogName)}
	inst, err := New(Options{Exec: fake, Logger: quiet}).Start(ctx, cfg)
	require.NoError(t, err)
	cmd := fake.Started()[0]
	assert.Equal(t, "vm-manager-ab12-swtpm", cmd.Unit)
	assert.Equal(t, cfg.Log, cmd.Log)
	assert.Equal(t, proc.Handle{Unit: cmd.Unit, PID: inst.PID()}, inst.Handle())
	require.NoError(t, os.WriteFile(cfg.Log, []byte("swtpm: note\n"), 0o600))
	assert.Equal(t, "swtpm: note", inst.Stderr(), "stderr comes from the log file")

	// The next vm-manager: a new manager on the same launcher.
	again, err := New(Options{Exec: fake, Logger: quiet}).Attach(ctx, cfg, inst.Handle())
	require.NoError(t, err)
	assert.Equal(t, inst.PID(), again.PID())
	assert.Equal(t, inst.SocketPath(), again.SocketPath())
	require.NoError(t, again.Kill())
	st := <-inst.Wait()
	assert.Equal(t, -1, st.Code)

	_, err = New(Options{Exec: fake, Logger: quiet}).Attach(ctx, cfg, proc.Handle{Unit: "vm-manager-x-swtpm", PID: 999})
	assert.ErrorIs(t, err, proc.ErrGone)
	_, err = New(Options{Exec: proc.OSExec{}, Logger: quiet}).Attach(ctx, cfg, inst.Handle())
	assert.ErrorIs(t, err, proc.ErrNoReattach)
}
