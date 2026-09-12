// Package tpm runs the software TPM behind each VM. Every VM gets its own
// swtpm process with its own state directory, started before QEMU and
// attached through a unix control socket (qemu: -chardev socket + -tpmdev
// emulator + -device tpm-crb). The state persists across the two boot
// phases (docs/design.md "Boot flow"): the credentials systemd-sysinstall
// seals during the installer boot are unsealed by the same vTPM on every
// installed boot, and the PCR quotes internal/attest verifies come from it.
//
// swtpm is told to terminate when QEMU drops the control connection, so a
// VM that exits takes its TPM with it; Stop covers the cases where QEMU
// never connected.
package tpm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/runtime/proc"
)

// DefaultBinary is the swtpm executable looked up on PATH.
const DefaultBinary = "swtpm"

// DefaultSocketName is the control socket created in the state directory
// when Config.SocketPath is empty.
const DefaultSocketName = "swtpm.sock"

// Defaults for Options.
const (
	DefaultStartTimeout = 5 * time.Second
	DefaultStopGrace    = 5 * time.Second
	DefaultLogLevel     = 1
	socketPoll          = 20 * time.Millisecond
)

// Options configure the Manager.
type Options struct {
	// Exec starts swtpm; nil uses proc.OSExec.
	Exec proc.Exec
	// Binary is the swtpm executable; empty uses DefaultBinary.
	Binary string
	// Logger for lifecycle events; nil uses slog.Default().
	Logger *slog.Logger
	// StartTimeout bounds the wait for the control socket to appear.
	StartTimeout time.Duration
	// StopGrace is how long Stop waits after SIGTERM before SIGKILL.
	StopGrace time.Duration
	// LogLevel is swtpm's --log level; 0 uses DefaultLogLevel.
	LogLevel int
}

// Manager starts swtpm instances.
type Manager struct {
	exec         proc.Exec
	binary       string
	log          *slog.Logger
	startTimeout time.Duration
	stopGrace    time.Duration
	logLevel     int
}

// New builds a Manager.
func New(opts Options) *Manager {
	m := &Manager{
		exec:         opts.Exec,
		binary:       opts.Binary,
		log:          opts.Logger,
		startTimeout: opts.StartTimeout,
		stopGrace:    opts.StopGrace,
		logLevel:     opts.LogLevel,
	}
	if m.exec == nil {
		m.exec = proc.OSExec{}
	}
	if m.binary == "" {
		m.binary = DefaultBinary
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.startTimeout <= 0 {
		m.startTimeout = DefaultStartTimeout
	}
	if m.stopGrace <= 0 {
		m.stopGrace = DefaultStopGrace
	}
	if m.logLevel <= 0 {
		m.logLevel = DefaultLogLevel
	}
	return m
}

// Config is one VM's TPM.
type Config struct {
	// StateDir holds the TPM state (created 0700 if missing); it must
	// outlive the VM.
	StateDir string
	// SocketPath is the control socket; empty puts DefaultSocketName in
	// StateDir. Unix socket paths are limited to 107 bytes.
	SocketPath string
}

// socket is the effective control socket path.
func (c Config) socket() string {
	if c.SocketPath != "" {
		return c.SocketPath
	}
	return filepath.Join(c.StateDir, DefaultSocketName)
}

// Args are the swtpm arguments for cfg: a TPM 2.0 in socket mode whose
// control channel is a unixio socket that ends the process when its peer
// (QEMU) disconnects.
func Args(cfg Config, logLevel int) []string {
	return []string{
		"socket",
		"--tpm2",
		"--tpmstate", "dir=" + cfg.StateDir,
		"--ctrl", "type=unixio,path=" + cfg.socket() + ",terminate",
		"--log", "level=" + strconv.Itoa(logLevel),
	}
}

// Instance is a running swtpm.
type Instance struct {
	proc   proc.Process
	socket string
	stderr *proc.Tail
	grace  time.Duration
	log    *slog.Logger
}

// Start creates the state directory, launches swtpm and returns once the
// control socket accepts QEMU, or fails with what swtpm printed.
func (m *Manager) Start(ctx context.Context, cfg Config) (*Instance, error) {
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("%w: tpm state dir is required", apierr.ErrInvalid)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tpm state dir: %w", err)
	}
	socket := cfg.socket()
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stale tpm socket: %w", err)
	}
	stderr := proc.NewTail(0)
	p, err := m.exec.Start(ctx, proc.Cmd{Path: m.binary, Args: Args(cfg, m.logLevel), Stdout: stderr, Stderr: stderr})
	if err != nil {
		return nil, fmt.Errorf("swtpm: %w", err)
	}
	inst := &Instance{proc: p, socket: socket, stderr: stderr, grace: m.stopGrace, log: m.log}
	if err := inst.waitSocket(ctx, m.startTimeout); err != nil {
		_ = p.Kill()
		return nil, err
	}
	m.log.Info("swtpm started", "pid", p.PID(), "socket", socket)
	return inst, nil
}

// waitSocket polls for the control socket until it exists, swtpm exits, ctx
// ends or timeout passes.
func (i *Instance) waitSocket(ctx context.Context, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	exited := i.proc.Wait()
	for {
		if _, err := os.Stat(i.socket); err == nil {
			return nil
		}
		select {
		case st := <-exited:
			return fmt.Errorf("swtpm exited before its socket came up (%s): %s", st, i.stderr)
		case <-ctx.Done():
			return fmt.Errorf("swtpm: %w", ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("swtpm socket %s did not appear within %s: %s", i.socket, timeout, i.stderr)
		case <-time.After(socketPoll):
		}
	}
}

// SocketPath is the control socket QEMU's tpm chardev connects to.
func (i *Instance) SocketPath() string { return i.socket }

// PID is the swtpm process id.
func (i *Instance) PID() int { return i.proc.PID() }

// Wait delivers the exit status; see proc.Process.Wait.
func (i *Instance) Wait() <-chan proc.ExitStatus { return i.proc.Wait() }

// Stderr is the tail of what swtpm printed.
func (i *Instance) Stderr() string { return i.stderr.String() }

// Stop ends swtpm: SIGTERM, then SIGKILL once the grace period or ctx runs
// out. It returns nil when the process is gone.
func (i *Instance) Stop(ctx context.Context) error {
	exited := i.proc.Wait()
	if err := i.proc.Signal(syscall.SIGTERM); errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	grace := time.NewTimer(i.grace)
	defer grace.Stop()
	select {
	case <-exited:
		return nil
	case <-ctx.Done():
	case <-grace.C:
	}
	i.log.Warn("swtpm did not stop on SIGTERM, killing", "pid", i.PID())
	return i.Kill()
}

// Kill ends swtpm with SIGKILL and waits for it to be reaped.
func (i *Instance) Kill() error {
	exited := i.proc.Wait()
	if err := i.proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill swtpm: %w", err)
	}
	<-exited
	return nil
}
