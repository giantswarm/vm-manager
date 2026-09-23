// Package tpm runs the software TPM behind each VM. Every VM gets its own
// swtpm process with its own state directory, started before QEMU and
// attached through a unix control socket (qemu: -chardev socket + -tpmdev
// emulator + -device tpm-crb). The state persists across the two boot
// phases (docs/design.md "Boot flow"): the credentials systemd-sysinstall
// seals during the installer boot are unsealed by the same vTPM on every
// installed boot, and the PCR quotes internal/attest verifies come from it.
//
// Start returns only once swtpm has answered CMD_GET_CAPABILITY on its
// control channel, the first command QEMU sends, so QEMU never starts
// against a vTPM that is not serving yet; a swtpm that does not answer in
// time is ErrUnresponsive. The probe is why the control channel does not
// carry swtpm's terminate option (it would end swtpm when the probe hangs
// up): --terminate ends swtpm once QEMU closes the data channel instead, so
// a VM that exits still takes its TPM with it; Stop covers the cases where
// QEMU never connected.
//
// Like QEMU, swtpm is launched through a proc.Exec; with the systemd
// launcher it is the transient unit vm-manager-<id>-swtpm and survives a
// vm-manager restart, after which Manager.Attach picks it up again from
// the Handle the caller persisted. Its own output goes to swtpm.log in the
// state directory so it is not lost with the vm-manager that started it,
// at a level that logs every control and TPM command and its response, so
// a command swtpm never answered is the last one in the log. Start moves
// the previous run's log to PrevLogName, which bounds the logs to the
// current and the previous run (the installer's, once the VM has booted).
package tpm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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

// LogName is the file in the state directory swtpm's own output goes to.
const LogName = "swtpm.log"

// PrevLogName is the previous run's LogName, moved aside by Start.
const PrevLogName = "swtpm.prev.log"

// ErrUnresponsive is a swtpm that did not answer on its control channel.
var ErrUnresponsive = errors.New("vtpm not responding")

// cmdGetCapability is swtpm's CMD_GET_CAPABILITY: a big-endian command
// code on the control channel, answered with the 8-byte capability mask.
const (
	cmdGetCapability = 1
	capabilityLen    = 8
)

// UnitRole names the swtpm process in its transient unit (proc.UnitName).
const UnitRole = "swtpm"

// Defaults for Options.
const (
	DefaultStartTimeout = 5 * time.Second
	DefaultStopGrace    = 5 * time.Second
	// DefaultLogLevel is the lowest swtpm level that logs each command.
	DefaultLogLevel = 2
	socketPoll      = 20 * time.Millisecond
)

// Options configure the Manager.
type Options struct {
	// Exec starts swtpm; nil uses proc.OSExec.
	Exec proc.Exec
	// Binary is the swtpm executable; empty uses DefaultBinary.
	Binary string
	// Logger for lifecycle events; nil uses slog.Default().
	Logger *slog.Logger
	// StartTimeout bounds the wait for swtpm to answer on its control
	// socket.
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
	// ID is the owning VM's id; it names the transient unit
	// (vm-manager-<id>-swtpm) under a launcher that uses units.
	ID string
	// StateDir holds the TPM state (created 0700 if missing); it must
	// outlive the VM.
	StateDir string
	// SocketPath is the control socket; empty puts DefaultSocketName in
	// StateDir. Unix socket paths are limited to 107 bytes.
	SocketPath string
	// Log, when set, is the file swtpm's own output is appended to
	// (proc.Cmd.Log; LogName in StateDir is the convention). Empty keeps
	// the output in memory, lost with the vm-manager that started it.
	Log string
}

// socket is the effective control socket path.
func (c Config) socket() string {
	if c.SocketPath != "" {
		return c.SocketPath
	}
	return filepath.Join(c.StateDir, DefaultSocketName)
}

// Args are the swtpm arguments for cfg: a TPM 2.0 in socket mode with a
// unixio control socket, ending once its client (QEMU) closes the data
// channel it passed over that socket.
func Args(cfg Config, logLevel int) []string {
	return []string{
		"socket",
		"--tpm2",
		"--tpmstate", "dir=" + cfg.StateDir,
		"--ctrl", "type=unixio,path=" + cfg.socket(),
		"--terminate",
		"--log", "level=" + strconv.Itoa(logLevel),
	}
}

// Instance is a running swtpm.
type Instance struct {
	proc   proc.Process
	cfg    Config
	socket string
	stderr *proc.Tail
	grace  time.Duration
	log    *slog.Logger
}

// Start creates the state directory, launches swtpm and returns once it
// has answered on its control socket, or fails with what swtpm printed
// (ErrUnresponsive when it did not answer within Options.StartTimeout).
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
	if err := rotateLog(cfg.Log); err != nil {
		return nil, err
	}
	stderr := proc.NewTail(0)
	cmd := proc.Cmd{Path: m.binary, Args: Args(cfg, m.logLevel), Stdout: stderr, Stderr: stderr, Log: cfg.Log, Unit: proc.UnitName(cfg.ID, UnitRole)}
	p, err := m.exec.Start(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("swtpm: %w", err)
	}
	inst := m.instance(cfg, p, stderr)
	if err := inst.waitReady(ctx, m.startTimeout); err != nil {
		_ = p.Kill()
		return nil, err
	}
	m.log.Info("swtpm started", "pid", p.PID(), "socket", socket, "unit", p.Handle().Unit)
	return inst, nil
}

// Attach picks up the swtpm of an earlier vm-manager behind h (the launcher
// must be a proc.Attacher, else proc.ErrNoReattach; a process that is gone
// is proc.ErrGone). A swtpm that has already exited is returned as is: its
// Wait delivers at once and Stop is a no-op.
func (m *Manager) Attach(ctx context.Context, cfg Config, h proc.Handle) (*Instance, error) {
	a, ok := m.exec.(proc.Attacher)
	if !ok {
		return nil, fmt.Errorf("swtpm: %w", proc.ErrNoReattach)
	}
	p, err := a.Attach(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("swtpm: %w", err)
	}
	inst := m.instance(cfg, p, proc.NewTail(0))
	m.log.Info("swtpm reattached", "pid", p.PID(), "socket", inst.socket, "unit", h.Unit)
	return inst, nil
}

func (m *Manager) instance(cfg Config, p proc.Process, stderr *proc.Tail) *Instance {
	return &Instance{proc: p, cfg: cfg, socket: cfg.socket(), stderr: stderr, grace: m.stopGrace, log: m.log}
}

// rotateLog moves the previous run's log to PrevLogName next to it.
func rotateLog(path string) error {
	if path == "" {
		return nil
	}
	err := os.Rename(path, filepath.Join(filepath.Dir(path), PrevLogName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate swtpm log: %w", err)
	}
	return nil
}

// waitReady probes the control socket until swtpm answers, swtpm exits,
// ctx ends or timeout passes.
func (i *Instance) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	exited := i.proc.Wait()
	for {
		err := probe(ctx, i.socket, deadline)
		if err == nil {
			return nil
		}
		select {
		case st := <-exited:
			return fmt.Errorf("swtpm exited before its control socket answered (%s): %s", st, i.Stderr())
		case <-ctx.Done():
			return fmt.Errorf("swtpm: %w", ctx.Err())
		case <-timer.C:
			return fmt.Errorf("%w: swtpm control socket %s did not answer within %s (%v): %s", ErrUnresponsive, i.socket, timeout, err, i.Stderr())
		case <-time.After(socketPoll):
		}
	}
}

// probe sends CMD_GET_CAPABILITY over the control socket and reads the
// answer, giving up at deadline or when ctx ends.
func probe(ctx context.Context, socket string, deadline time.Time) error {
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(dctx, func() { _ = conn.Close() })
	defer stop()
	req := binary.BigEndian.AppendUint32(nil, cmdGetCapability)
	if _, err := conn.Write(req); err != nil {
		return err
	}
	var caps [capabilityLen]byte
	if _, err := io.ReadFull(conn, caps[:]); err != nil {
		if dctx.Err() != nil {
			return fmt.Errorf("no answer to CMD_GET_CAPABILITY: %w", dctx.Err())
		}
		return err
	}
	return nil
}

// SocketPath is the control socket QEMU's tpm chardev connects to.
func (i *Instance) SocketPath() string { return i.socket }

// PID is the swtpm process id.
func (i *Instance) PID() int { return i.proc.PID() }

// Handle identifies the process for Manager.Attach after a restart.
func (i *Instance) Handle() proc.Handle { return i.proc.Handle() }

// Wait delivers the exit status; see proc.Process.Wait.
func (i *Instance) Wait() <-chan proc.ExitStatus { return i.proc.Wait() }

// Stderr is the tail of what swtpm printed: the end of Config.Log when the
// output goes to a file, else what was kept in memory.
func (i *Instance) Stderr() string {
	if i.cfg.Log != "" {
		return proc.TailFile(i.cfg.Log, 0)
	}
	return i.stderr.String()
}

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
