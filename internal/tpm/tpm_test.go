package tpm

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
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

// ctrlPath is the control socket the --ctrl argument of cmd names.
func ctrlPath(cmd proc.Cmd) (string, error) {
	i := slices.Index(cmd.Args, "--ctrl")
	if i < 0 || i+1 >= len(cmd.Args) {
		return "", errors.New("no --ctrl argument")
	}
	return strings.TrimPrefix(cmd.Args[i+1], "type=unixio,path="), nil
}

// fakeSwtpm is a Hook that serves the control socket like swtpm: it
// listens at once and, after delay (never when negative), answers each
// CMD_GET_CAPABILITY with a capability mask. answered counts the answers.
func fakeSwtpm(t *testing.T, delay time.Duration, answered *atomic.Int32) func(proc.Cmd, *proc.FakeProcess) error {
	return func(cmd proc.Cmd, _ *proc.FakeProcess) error {
		path, err := ctrlPath(cmd)
		if err != nil {
			return err
		}
		l, err := net.Listen("unix", path)
		if err != nil {
			return err
		}
		t.Cleanup(func() { _ = l.Close() })
		ready := time.Now().Add(delay)
		go func() {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				go func() {
					defer func() { _ = conn.Close() }()
					req := make([]byte, 4)
					if _, err := io.ReadFull(conn, req); err != nil || binary.BigEndian.Uint32(req) != cmdGetCapability {
						return
					}
					if delay < 0 {
						_, _ = io.Copy(io.Discard, conn)
						return
					}
					time.Sleep(time.Until(ready))
					if _, err := conn.Write(make([]byte, capabilityLen)); err == nil && answered != nil {
						answered.Add(1)
					}
				}()
			}
		}()
		return nil
	}
}

// createSocket serves a control socket that answers at once.
func createSocket(t *testing.T) func(proc.Cmd, *proc.FakeProcess) error {
	return fakeSwtpm(t, 0, nil)
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
				"--ctrl", "type=unixio,path=/var/lib/vm-manager/vms/vm-1/tpm/swtpm.sock",
				"--terminate",
				"--log", "level=2",
			},
		},
		{
			name: "explicit socket",
			cfg:  Config{StateDir: "/state", SocketPath: "/run/vm-manager/vm-1/swtpm.sock"},
			want: []string{
				"socket", "--tpm2",
				"--tpmstate", "dir=/state",
				"--ctrl", "type=unixio,path=/run/vm-manager/vm-1/swtpm.sock",
				"--terminate",
				"--log", "level=3",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			level := 2
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
		fake := &proc.FakeExec{Hook: createSocket(t)}
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
			return createSocket(t)(cmd, p)
		}}
		inst, err := New(Options{Exec: fake, Logger: quiet}).Start(ctx, Config{StateDir: dir, SocketPath: sock})
		require.NoError(t, err)
		assert.Equal(t, sock, inst.SocketPath())
	})
	t.Run("custom binary and log level", func(t *testing.T) {
		fake := &proc.FakeExec{Hook: createSocket(t)}
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
		assert.Contains(t, err.Error(), "exited before its control socket answered (exit status 1)")
		assert.Contains(t, err.Error(), "Could not open tpmstate")
	})
	t.Run("waits for a late answer", func(t *testing.T) {
		var answered atomic.Int32
		fake := &proc.FakeExec{Hook: fakeSwtpm(t, 300*time.Millisecond, &answered)}
		begin := time.Now()
		_, err := New(Options{Exec: fake, Logger: quiet}).Start(ctx, Config{StateDir: t.TempDir()})
		require.NoError(t, err)
		assert.GreaterOrEqual(t, time.Since(begin), 300*time.Millisecond, "Start returns only after the answer")
		assert.Equal(t, int32(1), answered.Load())
	})
	unresponsive := map[string]func(*testing.T) func(proc.Cmd, *proc.FakeProcess) error{
		"socket never appears": func(*testing.T) func(proc.Cmd, *proc.FakeProcess) error { return nil },
		"socket file, no listener": func(*testing.T) func(proc.Cmd, *proc.FakeProcess) error {
			return func(cmd proc.Cmd, _ *proc.FakeProcess) error {
				path, err := ctrlPath(cmd)
				if err != nil {
					return err
				}
				return os.WriteFile(path, nil, 0o600)
			}
		},
		"accepts, never answers": func(t *testing.T) func(proc.Cmd, *proc.FakeProcess) error { return fakeSwtpm(t, -1, nil) },
		"answers too late":       func(t *testing.T) func(proc.Cmd, *proc.FakeProcess) error { return fakeSwtpm(t, time.Second, nil) },
	}
	for name, hook := range unresponsive {
		t.Run(name, func(t *testing.T) {
			fake := &proc.FakeExec{Hook: hook(t)}
			begin := time.Now()
			_, err := New(Options{Exec: fake, Logger: quiet, StartTimeout: 100 * time.Millisecond}).Start(ctx, Config{StateDir: t.TempDir()})
			require.ErrorIs(t, err, ErrUnresponsive)
			assert.Contains(t, err.Error(), "did not answer within 100ms")
			assert.Less(t, time.Since(begin), time.Second, "the wait is bounded by StartTimeout")
			p := fake.Processes()[0]
			assert.True(t, p.Exited(), "a swtpm that never answered is killed")
			assert.Equal(t, []os.Signal{syscall.SIGKILL}, p.Signals())
		})
	}
	t.Run("moves the previous log aside", func(t *testing.T) {
		dir := t.TempDir()
		log := filepath.Join(dir, LogName)
		require.NoError(t, os.WriteFile(log, []byte("installer run\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, PrevLogName), []byte("older run\n"), 0o600))
		fake := &proc.FakeExec{Hook: createSocket(t)}
		_, err := New(Options{Exec: fake, Logger: quiet}).Start(ctx, Config{StateDir: dir, Log: log})
		require.NoError(t, err)
		prev, err := os.ReadFile(filepath.Join(dir, PrevLogName)) // #nosec G304 -- the test's temp dir
		require.NoError(t, err)
		assert.Equal(t, "installer run\n", string(prev), "only the previous run is kept")
		assert.NoFileExists(t, log, "the new run starts a new log")
		assert.Equal(t, log, fake.Started()[0].Log)
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
			return createSocket(t)(cmd, p)
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

// TestRealSwtpm runs the swtpm on PATH: Start returns once swtpm has
// answered on its control socket, and the probe hanging up does not end
// swtpm (the control socket carries no terminate option), so QEMU can
// connect after it; that swtpm follows QEMU out is
// qemu.TestIntegrationFirmwareBoot.
func TestRealSwtpm(t *testing.T) {
	if _, err := exec.LookPath(DefaultBinary); err != nil {
		t.Skip("swtpm not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "tpm")
	inst, err := New(Options{Logger: quiet}).Start(ctx, Config{StateDir: dir, Log: filepath.Join(dir, LogName)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = inst.Kill() })

	st, err := os.Stat(inst.SocketPath())
	require.NoError(t, err)
	assert.Equal(t, os.ModeSocket, st.Mode().Type())
	assert.Positive(t, inst.PID())
	require.NoError(t, probe(ctx, inst.SocketPath(), time.Now().Add(5*time.Second)), "a second client is served too: %s", inst.Stderr())
	select {
	case s := <-inst.Wait():
		t.Fatalf("swtpm exited after the probe hung up: %s: %s", s, inst.Stderr())
	case <-time.After(200 * time.Millisecond):
	}
	assert.Contains(t, inst.Stderr(), "Ctrl Cmd:", "the log level shows each command")
}
