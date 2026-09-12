package tpm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/runtime/proc"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// exitOf receives the status or fails the test.
func exitOf(t *testing.T, ch <-chan proc.ExitStatus) proc.ExitStatus {
	t.Helper()
	select {
	case st := <-ch:
		return st
	case <-time.After(5 * time.Second):
		t.Fatal("swtpm did not exit")
		return proc.ExitStatus{}
	}
}

// createSocket is a Hook that creates the control socket file the way
// swtpm would, at the path the --ctrl argument names.
func createSocket(cmd proc.Cmd, _ *proc.FakeProcess) error {
	i := slices.Index(cmd.Args, "--ctrl")
	if i < 0 || i+1 >= len(cmd.Args) {
		return errors.New("no --ctrl argument")
	}
	path := strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "type=unixio,path="), ",terminate")
	return os.WriteFile(path, nil, 0o600)
}

func TestArgs(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "socket in state dir",
			cfg:  Config{StateDir: "/var/lib/vm-manager/vms/vm-1/tpm"},
			want: []string{
				"socket", "--tpm2",
				"--tpmstate", "dir=/var/lib/vm-manager/vms/vm-1/tpm",
				"--ctrl", "type=unixio,path=/var/lib/vm-manager/vms/vm-1/tpm/swtpm.sock,terminate",
				"--log", "level=1",
			},
		},
		{
			name: "explicit socket",
			cfg:  Config{StateDir: "/state", SocketPath: "/run/vm-manager/vm-1/swtpm.sock"},
			want: []string{
				"socket", "--tpm2",
				"--tpmstate", "dir=/state",
				"--ctrl", "type=unixio,path=/run/vm-manager/vm-1/swtpm.sock,terminate",
				"--log", "level=3",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			level := 1
			if tc.cfg.SocketPath != "" {
				level = 3
			}
			assert.Equal(t, tc.want, Args(tc.cfg, level))
		})
	}
}

func TestStart(t *testing.T) {
	ctx := context.Background()
	t.Run("creates state dir, waits for socket", func(t *testing.T) {
		fake := &proc.FakeExec{Hook: createSocket}
		m := New(Options{Exec: fake, Logger: quiet})
		dir := filepath.Join(t.TempDir(), "vm", "tpm")
		inst, err := m.Start(ctx, Config{StateDir: dir})
		require.NoError(t, err)
		st, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), st.Mode().Perm())
		assert.Equal(t, filepath.Join(dir, DefaultSocketName), inst.SocketPath())
		assert.FileExists(t, inst.SocketPath())
		assert.Equal(t, 4001, inst.PID())
		require.Len(t, fake.Started(), 1)
		assert.Equal(t, DefaultBinary, fake.Started()[0].Path)
		assert.Equal(t, Args(Config{StateDir: dir}, DefaultLogLevel), fake.Started()[0].Args)
	})
	t.Run("removes a stale socket first", func(t *testing.T) {
		dir := t.TempDir()
		sock := filepath.Join(dir, "ctrl.sock")
		require.NoError(t, os.WriteFile(sock, []byte("stale"), 0o600))
		fake := &proc.FakeExec{Hook: func(cmd proc.Cmd, p *proc.FakeProcess) error {
			_, err := os.Stat(sock)
			assert.ErrorIs(t, err, os.ErrNotExist, "stale socket must be gone before swtpm starts")
			return createSocket(cmd, p)
		}}
		inst, err := New(Options{Exec: fake, Logger: quiet}).Start(ctx, Config{StateDir: dir, SocketPath: sock})
		require.NoError(t, err)
		assert.Equal(t, sock, inst.SocketPath())
	})
	t.Run("custom binary and log level", func(t *testing.T) {
		fake := &proc.FakeExec{Hook: createSocket}
		_, err := New(Options{Exec: fake, Logger: quiet, Binary: "/opt/swtpm", LogLevel: 4}).Start(ctx, Config{StateDir: t.TempDir()})
		require.NoError(t, err)
		assert.Equal(t, "/opt/swtpm", fake.Started()[0].Path)
		assert.Contains(t, fake.Started()[0].Args, "level=4")
	})
	t.Run("missing state dir", func(t *testing.T) {
		_, err := New(Options{Exec: &proc.FakeExec{}, Logger: quiet}).Start(ctx, Config{})
		assert.ErrorIs(t, err, apierr.ErrInvalid)
	})
	t.Run("exec failure", func(t *testing.T) {
		fake := &proc.FakeExec{Hook: func(proc.Cmd, *proc.FakeProcess) error { return exec.ErrNotFound }}
		_, err := New(Options{Exec: fake, Logger: quiet}).Start(ctx, Config{StateDir: t.TempDir()})
		assert.ErrorIs(t, err, exec.ErrNotFound)
	})
	t.Run("exits before the socket appears", func(t *testing.T) {
		fake := &proc.FakeExec{Hook: func(cmd proc.Cmd, p *proc.FakeProcess) error {
			_, _ = cmd.Stderr.Write([]byte("swtpm: Could not open tpmstate\n"))
			p.Exit(1)
			return nil
		}}
		_, err := New(Options{Exec: fake, Logger: quiet}).Start(ctx, Config{StateDir: t.TempDir()})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exited before its socket came up (exit status 1)")
		assert.Contains(t, err.Error(), "Could not open tpmstate")
	})
	t.Run("socket never appears", func(t *testing.T) {
		fake := &proc.FakeExec{}
		_, err := New(Options{Exec: fake, Logger: quiet, StartTimeout: 50 * time.Millisecond}).Start(ctx, Config{StateDir: t.TempDir()})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not appear within 50ms")
		p := fake.Processes()[0]
		assert.True(t, p.Exited(), "a swtpm that never came up is killed")
		assert.Equal(t, []os.Signal{syscall.SIGKILL}, p.Signals())
	})
	t.Run("context cancelled while waiting", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		fake := &proc.FakeExec{Hook: func(proc.Cmd, *proc.FakeProcess) error { cancel(); return nil }}
		_, err := New(Options{Exec: fake, Logger: quiet}).Start(cctx, Config{StateDir: t.TempDir()})
		assert.ErrorIs(t, err, context.Canceled)
		assert.True(t, fake.Processes()[0].Exited())
	})
}

func TestStop(t *testing.T) {
	ctx := context.Background()
	start := func(t *testing.T, term bool, grace time.Duration) (*Instance, *proc.FakeProcess) {
		t.Helper()
		fake := &proc.FakeExec{Hook: func(cmd proc.Cmd, p *proc.FakeProcess) error {
			p.ExitOnTerm = term
			return createSocket(cmd, p)
		}}
		inst, err := New(Options{Exec: fake, Logger: quiet, StopGrace: grace}).Start(ctx, Config{StateDir: t.TempDir()})
		require.NoError(t, err)
		return inst, fake.Processes()[0]
	}
	t.Run("SIGTERM suffices", func(t *testing.T) {
		inst, p := start(t, true, time.Second)
		require.NoError(t, inst.Stop(ctx))
		assert.Equal(t, []os.Signal{syscall.SIGTERM}, p.Signals())
		assert.Equal(t, 143, exitOf(t, inst.Wait()).Code)
		require.NoError(t, inst.Stop(ctx), "stopping a stopped instance is a no-op")
	})
	t.Run("escalates to SIGKILL after the grace period", func(t *testing.T) {
		inst, p := start(t, false, 20*time.Millisecond)
		require.NoError(t, inst.Stop(ctx))
		assert.Equal(t, []os.Signal{syscall.SIGTERM, syscall.SIGKILL}, p.Signals())
		assert.Equal(t, -1, exitOf(t, inst.Wait()).Code)
	})
	t.Run("escalates when the context ends", func(t *testing.T) {
		inst, p := start(t, false, time.Minute)
		cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		require.NoError(t, inst.Stop(cctx))
		assert.Equal(t, []os.Signal{syscall.SIGTERM, syscall.SIGKILL}, p.Signals())
	})
	t.Run("kill", func(t *testing.T) {
		inst, p := start(t, false, time.Minute)
		require.NoError(t, inst.Kill())
		assert.True(t, p.Exited())
		assert.Empty(t, inst.Stderr())
	})
}

// TestRealSwtpm runs the swtpm on PATH: the control socket must be a
// listening unix socket, and losing the control connection (what happens
// when QEMU exits) must end the process because of the terminate flag.
func TestRealSwtpm(t *testing.T) {
	if _, err := exec.LookPath(DefaultBinary); err != nil {
		t.Skip("swtpm not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inst, err := New(Options{Logger: quiet}).Start(ctx, Config{StateDir: filepath.Join(t.TempDir(), "tpm")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = inst.Kill() })

	st, err := os.Stat(inst.SocketPath())
	require.NoError(t, err)
	assert.Equal(t, os.ModeSocket, st.Mode().Type())
	assert.Positive(t, inst.PID())
	select {
	case s := <-inst.Wait():
		t.Fatalf("swtpm exited early: %s: %s", s, inst.Stderr())
	case <-time.After(200 * time.Millisecond):
	}

	conn, err := net.Dial("unix", inst.SocketPath())
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	exited := exitOf(t, inst.Wait())
	assert.Equal(t, 0, exited.Code, "stderr: %s", inst.Stderr())
}
