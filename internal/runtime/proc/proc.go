// Package proc starts and supervises the long-running host processes the VM
// runtime is made of: QEMU (internal/runtime/qemu) and swtpm (internal/tpm).
// Exec is the injectable seam, the long-running counterpart of host.Runner:
// OSExec runs real processes through os/exec, FakeExec (fake.go) records the
// commands and lets tests drive exit codes and signals without a VM stack.
package proc

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

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
}

// Exec starts processes; tests inject a fake.
type Exec interface {
	// Start launches cmd. ctx bounds the start only: the process outlives
	// it, a VM must survive the request that created it.
	Start(ctx context.Context, cmd Cmd) (Process, error)
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
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Path, err)
	}
	p := &osProcess{cmd: cmd, done: make(chan struct{})}
	go p.wait()
	return p, nil
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
