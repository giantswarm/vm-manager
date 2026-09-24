// Package proc starts and supervises the long-running host processes the VM
// runtime is made of: QEMU (internal/runtime/qemu) and swtpm (internal/tpm).
// Exec is the injectable seam, the long-running counterpart of host.Runner.
// There are three launchers:
//
//   - SystemdExec (systemd.go) starts every command as a transient systemd
//     service (systemd-run --service-type=exec) under the user or the system
//     service manager. The process is a child of systemd, not of vm-manager,
//     in a cgroup of its own, so it survives a vm-manager restart; the next
//     vm-manager reattaches with Attach and still learns how it exited
//     (RemainAfterExit=yes keeps the unit and its ExecMainStatus until the
//     watcher releases it). DetectLauncher picks it whenever a service
//     manager is reachable.
//   - OSExec runs plain child processes through os/exec; they end with
//     vm-manager. It is the fallback without systemd.
//   - FakeExec (fake.go) records the commands and lets tests drive exit
//     codes and signals without a VM stack.
//
// A launched process is identified by its Handle (unit name and PID), which
// the caller persists; an Attacher turns a Handle back into a Process. With
// Cmd.Log set the output goes to a file rather than a pipe, since a pipe to
// the caller would break when the caller exits while the process lives on.
package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// ErrGone is returned by Attach when the process (or its unit) no longer
// exists and nothing recorded how it ended.
var ErrGone = errors.New("process is gone")

// ErrNoReattach is returned by launchers that cannot pick a process up
// again, for example OSExec whose children die with the caller.
var ErrNoReattach = errors.New("launcher cannot reattach processes")

// Cmd describes a process to start.
type Cmd struct {
	// Path is the binary, looked up on PATH when it has no directory part.
	Path string
	// Args are the arguments after argv[0].
	Args []string
	// Dir is the working directory; empty inherits the caller's.
	Dir string
	// Env is the environment; nil inherits the caller's.
	Env []string
	// Stdout and Stderr receive the process output; nil discards it. Stdin
	// is always /dev/null.
	Stdout io.Writer
	Stderr io.Writer
	// Log, when set, is a file both streams are appended to instead, created
	// 0600 if missing. Use it for a process that must outlive the caller:
	// the launchers that keep it running when the caller exits cannot hand
	// the caller a pipe. TailFile reads the end of it back.
	Log string
	// Unit names the transient unit a systemd launcher runs the command as
	// (UnitName builds it); launchers without units ignore it.
	Unit string
	// NoIOUring makes io_uring_setup fail with EPERM in the process, through
	// a seccomp filter, as kernel.io_uring_disabled=2 does host-wide. QEMU
	// 10.2+ then monitors its main loop with epoll instead of io_uring, whose
	// fd monitoring loses TPM emulator commands (vm-manager#84).
	NoIOUring bool
}

// Handle identifies a launched process for Attacher.Attach. The caller
// persists it next to whatever the process serves.
type Handle struct {
	// Unit is the transient unit name; empty for a plain child process.
	Unit string `json:"unit,omitempty"`
	// PID is the process id at launch, for the reader and as a fallback
	// check; the unit is authoritative where there is one.
	PID int `json:"pid"`
}

// UnitName is the transient unit for one role (qemu, swtpm) of one VM:
// vm-manager-<id>-<role>. Service names are flat, unlike slice names, so the
// dashes carry no hierarchy.
func UnitName(vmID, role string) string {
	return "vm-manager-" + vmID + "-" + role
}

// ExitStatus is how a process ended.
type ExitStatus struct {
	// Code is the exit code, -1 when the process was terminated by a signal
	// or its status is unknown.
	Code int
	// Err is nil for a clean exit (code 0); otherwise the *exec.ExitError or
	// the error waiting for the process.
	Err error
}

// String renders the status the way a log line wants it.
func (s ExitStatus) String() string {
	if s.Err == nil {
		return "exit status 0"
	}
	return s.Err.Error()
}

// Process is a started process.
type Process interface {
	// PID is the operating system process id.
	PID() int
	// Wait returns a channel that delivers the exit status once the process
	// has ended. Every call gets its own channel, so any number of goroutines
	// may wait.
	Wait() <-chan ExitStatus
	// Signal sends sig; os.ErrProcessDone once the process has ended.
	Signal(sig os.Signal) error
	// Kill sends SIGKILL; os.ErrProcessDone once the process has ended.
	Kill() error
	// Handle identifies the process for a later Attach.
	Handle() Handle
}

// Exec starts processes; tests inject a fake.
type Exec interface {
	// Start launches cmd. ctx bounds the start only: the process outlives
	// it, a VM must survive the request that created it.
	Start(ctx context.Context, cmd Cmd) (Process, error)
}

// Attacher is an Exec that can pick up a process an earlier Exec (in an
// earlier vm-manager) started.
type Attacher interface {
	Exec
	// Attach returns the process behind h. A process that already ended but
	// whose exit is still recorded comes back as a Process whose Wait
	// delivers at once; ErrGone means nothing is left of it.
	Attach(ctx context.Context, h Handle) (Process, error)
}

// OSExec starts processes on the host through os/exec.
type OSExec struct{}

// Start implements Exec.
func (OSExec) Start(ctx context.Context, c Cmd) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(c.Path, c.Args...) // #nosec G204 -- argv is assembled by the runtime packages from validated specs
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	if c.Log != "" {
		f, err := openLog(c.Log)
		if err != nil {
			return nil, fmt.Errorf("start %s: %w", c.Path, err)
		}
		defer func() { _ = f.Close() }() // the child holds its own descriptor
		cmd.Stdout, cmd.Stderr = f, f
	}
	start := cmd.Start
	if c.NoIOUring {
		start = func() error { return startDenyingIOUring(cmd) }
	}
	if err := start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Path, err)
	}
	p := &osProcess{cmd: cmd, done: make(chan struct{})}
	go p.wait()
	return p, nil
}

// openLog opens a Cmd.Log for appending, creating its directory.
func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) // #nosec G304 -- the caller's own log path
	if err != nil {
		return nil, fmt.Errorf("log file: %w", err)
	}
	return f, nil
}

type osProcess struct {
	cmd    *exec.Cmd
	done   chan struct{}
	status ExitStatus
}

func (p *osProcess) wait() {
	err := p.cmd.Wait()
	p.status = ExitStatus{Code: p.cmd.ProcessState.ExitCode(), Err: err}
	close(p.done)
}

func (p *osProcess) PID() int { return p.cmd.Process.Pid }

func (p *osProcess) Wait() <-chan ExitStatus { return waitOn(p.done, &p.status) }

func (p *osProcess) Signal(sig os.Signal) error {
	return p.cmd.Process.Signal(sig)
}

func (p *osProcess) Kill() error {
	return p.cmd.Process.Kill()
}

func (p *osProcess) Handle() Handle { return Handle{PID: p.cmd.Process.Pid} }

// waitOn is the shared Wait implementation: a fresh channel that yields
// *status once done is closed. status must not change after done closes.
func waitOn(done <-chan struct{}, status *ExitStatus) <-chan ExitStatus {
	ch := make(chan ExitStatus, 1)
	go func() {
		<-done
		ch <- *status
	}()
	return ch
}
