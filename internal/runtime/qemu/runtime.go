package qemu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/giantswarm/vm-manager/internal/runtime/proc"
)

// DefaultBinary is the QEMU executable looked up on PATH.
const DefaultBinary = "qemu-system-x86_64"

// UnitRole names the QEMU process in its transient unit (proc.UnitName).
const UnitRole = "qemu"

// DefaultOVMFVarsTemplate is the pristine variable store each VM's OVMFVars
// is copied from (Arch edk2-ovmf; host.Info reports the code image).
const DefaultOVMFVarsTemplate = "/usr/share/edk2/x64/OVMF_VARS.4m.fd"

// Defaults for Options.
const (
	DefaultStartTimeout     = 10 * time.Second
	DefaultPowerdownTimeout = 30 * time.Second
	DefaultStopGrace        = 5 * time.Second
	qmpPoll                 = 20 * time.Millisecond
	qmpDialTimeout          = 2 * time.Second
)

// Options configure the Runtime.
type Options struct {
	// Exec starts QEMU; nil uses proc.OSExec.
	Exec proc.Exec
	// Binary is the QEMU executable; empty uses DefaultBinary.
	Binary string
	// OVMFVarsTemplate is copied to Spec.OVMFVars when that file is missing.
	OVMFVarsTemplate string
	// Logger for lifecycle events; nil uses slog.Default().
	Logger *slog.Logger
	// StartTimeout bounds the wait for QMP to come up after the process
	// started.
	StartTimeout time.Duration
	// PowerdownTimeout is how long Stop waits for a guest to shut down when
	// its ctx carries no deadline.
	PowerdownTimeout time.Duration
	// StopGrace is how long Stop waits after SIGTERM before SIGKILL.
	StopGrace time.Duration
}

// Runtime starts VMs.
type Runtime struct {
	exec             proc.Exec
	binary           string
	varsTemplate     string
	log              *slog.Logger
	startTimeout     time.Duration
	powerdownTimeout time.Duration
	stopGrace        time.Duration
}

// New builds a Runtime.
func New(opts Options) *Runtime {
	r := &Runtime{
		exec:             opts.Exec,
		binary:           opts.Binary,
		varsTemplate:     opts.OVMFVarsTemplate,
		log:              opts.Logger,
		startTimeout:     opts.StartTimeout,
		powerdownTimeout: opts.PowerdownTimeout,
		stopGrace:        opts.StopGrace,
	}
	if r.exec == nil {
		r.exec = proc.OSExec{}
	}
	if r.binary == "" {
		r.binary = DefaultBinary
	}
	if r.varsTemplate == "" {
		r.varsTemplate = DefaultOVMFVarsTemplate
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.startTimeout <= 0 {
		r.startTimeout = DefaultStartTimeout
	}
	if r.powerdownTimeout <= 0 {
		r.powerdownTimeout = DefaultPowerdownTimeout
	}
	if r.stopGrace <= 0 {
		r.stopGrace = DefaultStopGrace
	}
	return r
}

// Instance is a running VM.
type Instance struct {
	spec   Spec
	proc   proc.Process
	stderr *proc.Tail
	exited chan struct{}
	// qmp is nil only on an Instance attached to a QEMU that had already
	// exited; every method tolerates that.
	qmp              *QMP
	log              *slog.Logger
	powerdownTimeout time.Duration
	stopGrace        time.Duration
}

// Start validates spec, prepares the per-VM files (OVMF vars copy, parent
// directories, stale QMP socket), launches QEMU and returns once QMP
// answers. A QEMU that exits first fails Start with its stderr.
func (r *Runtime) Start(ctx context.Context, spec Spec) (*Instance, error) {
	args, err := Command(spec)
	if err != nil {
		return nil, err
	}
	if err := r.prepare(spec); err != nil {
		return nil, err
	}
	stderr := proc.NewTail(0)
	cmd := proc.Cmd{Path: r.binary, Args: args, Stdout: stderr, Stderr: stderr, Log: spec.ProcessLog, Unit: proc.UnitName(spec.ID, UnitRole)}
	p, err := r.exec.Start(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("qemu: %w", err)
	}
	inst := r.instance(spec, p, stderr)
	qmp, err := inst.connectQMP(ctx, r.startTimeout)
	if err != nil {
		_ = inst.Kill()
		return nil, err
	}
	inst.setQMP(qmp)
	inst.log.Info("qemu started", "phase", spec.Phase, "pid", p.PID(), "unit", p.Handle().Unit)
	return inst, nil
}

// Attach picks up the QEMU of an earlier vm-manager: the process behind h
// (the launcher must be a proc.Attacher, else proc.ErrNoReattach; a process
// that is gone is proc.ErrGone) and its QMP socket. A QEMU that has already
// exited comes back as an exited Instance so the caller settles it the way
// it settles any exit; one that runs but does not answer QMP is an error,
// and left running for the caller to decide.
func (r *Runtime) Attach(ctx context.Context, spec Spec, h proc.Handle) (*Instance, error) {
	a, ok := r.exec.(proc.Attacher)
	if !ok {
		return nil, fmt.Errorf("qemu: %w", proc.ErrNoReattach)
	}
	p, err := a.Attach(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("qemu: %w", err)
	}
	inst := r.instance(spec, p, proc.NewTail(0))
	qmp, err := inst.connectQMP(ctx, r.startTimeout)
	switch {
	case err == nil:
		inst.setQMP(qmp)
	case inst.Exited():
		inst.log.Info("qemu had exited before reattach", "exit", inst.status().String())
	default:
		return nil, fmt.Errorf("reattach qemu pid %d: %w", p.PID(), err)
	}
	inst.log.Info("qemu reattached", "phase", spec.Phase, "pid", p.PID(), "unit", h.Unit)
	return inst, nil
}

// instance wraps a started or attached process.
func (r *Runtime) instance(spec Spec, p proc.Process, stderr *proc.Tail) *Instance {
	inst := &Instance{
		spec:             spec,
		proc:             p,
		stderr:           stderr,
		exited:           make(chan struct{}),
		log:              r.log.With("vm", spec.ID),
		powerdownTimeout: r.powerdownTimeout,
		stopGrace:        r.stopGrace,
	}
	go func() {
		<-p.Wait()
		close(inst.exited)
	}()
	return inst
}

// setQMP installs the control channel and closes it with the process.
func (i *Instance) setQMP(qmp *QMP) {
	i.qmp = qmp
	go func() {
		<-i.exited
		_ = qmp.Close()
	}()
}

// prepare creates the directories the VM writes into, seeds the OVMF
// variable store and removes a QMP socket left by an earlier process.
func (r *Runtime) prepare(spec Spec) error {
	for _, p := range []string{spec.OVMFVars, spec.SerialLog, spec.QMPSocket} {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return fmt.Errorf("vm directory: %w", err)
		}
	}
	if err := copyIfMissing(r.varsTemplate, spec.OVMFVars); err != nil {
		return fmt.Errorf("OVMF vars: %w", err)
	}
	if err := os.Remove(spec.QMPSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stale QMP socket: %w", err)
	}
	return nil
}

// copyIfMissing copies src to dst (0600) unless dst exists.
func copyIfMissing(src, dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- dst is the VM's own vars path
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	in, err := os.Open(src) // #nosec G304 -- src is the configured firmware template
	if err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	defer func() { _ = in.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// connectQMP dials the QMP socket until it answers, the process exits, ctx
// ends or timeout passes.
func (i *Instance) connectQMP(ctx context.Context, timeout time.Duration) (*QMP, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		dctx, cancel := context.WithTimeout(ctx, qmpDialTimeout)
		q, err := dialQMP(dctx, i.spec.QMPSocket, i.log)
		cancel()
		if err == nil {
			return q, nil
		}
		select {
		case <-i.exited:
			return nil, fmt.Errorf("qemu exited during start (%s): %s", i.status(), i.stderr)
		case <-ctx.Done():
			return nil, fmt.Errorf("qemu start: %w", ctx.Err())
		case <-deadline.C:
			return nil, fmt.Errorf("qemu QMP at %s did not answer within %s: %w (%s)", i.spec.QMPSocket, timeout, err, i.stderr)
		case <-time.After(qmpPoll):
		}
	}
}

// status is the exit status; call after exited is closed.
func (i *Instance) status() proc.ExitStatus { return <-i.proc.Wait() }

// Spec is what the VM was started with.
func (i *Instance) Spec() Spec { return i.spec }

// PID is the QEMU process id.
func (i *Instance) PID() int { return i.proc.PID() }

// Handle identifies the process for Runtime.Attach after a restart.
func (i *Instance) Handle() proc.Handle { return i.proc.Handle() }

// QMP is the control channel; nil only for an Instance attached after QEMU
// had exited.
func (i *Instance) QMP() *QMP { return i.qmp }

// Detach lets go of a running QEMU without stopping it: the QMP connection
// is closed so the next vm-manager can take it (QEMU serves one client),
// the process keeps running under its launcher. Wait no longer reports its
// exit to this Instance in any useful way.
func (i *Instance) Detach() error {
	if i.qmp == nil {
		return nil
	}
	i.log.Info("qemu detached", "pid", i.PID(), "unit", i.Handle().Unit)
	return i.qmp.Close()
}

// Wait delivers the exit status; see proc.Process.Wait. In PhaseInstall
// with NoReboot the guest's reboot ends the process with status 0.
func (i *Instance) Wait() <-chan proc.ExitStatus { return i.proc.Wait() }

// Exited reports whether QEMU has ended.
func (i *Instance) Exited() bool {
	select {
	case <-i.exited:
		return true
	default:
		return false
	}
}

// Stderr is the tail of what QEMU printed: the end of Spec.ProcessLog when
// the output goes to a file, else what was kept in memory.
func (i *Instance) Stderr() string {
	if i.spec.ProcessLog != "" {
		return proc.TailFile(i.spec.ProcessLog, 0)
	}
	return i.stderr.String()
}

// Stop shuts the VM down in escalating steps: system_powerdown over QMP
// and wait for the guest (until ctx's deadline, or PowerdownTimeout when it
// has none), then SIGTERM, then SIGKILL after StopGrace. It returns nil once
// the process is gone.
func (i *Instance) Stop(ctx context.Context) error {
	if i.Exited() {
		return nil
	}
	pctx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		pctx, cancel = context.WithTimeout(ctx, i.powerdownTimeout)
		defer cancel()
	}
	if i.qmp == nil {
		i.log.Debug("no QMP channel, terminating")
	} else if err := i.qmp.SystemPowerdown(pctx); err != nil {
		i.log.Debug("powerdown not delivered, terminating", "error", err)
	} else {
		select {
		case <-i.exited:
			return nil
		case <-pctx.Done():
			i.log.Warn("guest did not power down, terminating")
		}
	}
	if err := i.proc.Signal(syscall.SIGTERM); errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	grace := time.NewTimer(i.stopGrace)
	defer grace.Stop()
	select {
	case <-i.exited:
		return nil
	case <-grace.C:
		i.log.Warn("qemu did not exit on SIGTERM, killing")
	}
	return i.Kill()
}

// Kill ends QEMU with SIGKILL and waits for it to be reaped.
func (i *Instance) Kill() error {
	if err := i.proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill qemu: %w", err)
	}
	<-i.exited
	return nil
}
