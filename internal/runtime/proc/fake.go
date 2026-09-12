package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
)

// FakeExec is an Exec for tests: it records every Cmd and returns a
// FakeProcess the test drives. It lives here rather than in a _test file so
// the qemu and tpm packages share one fake. It is also an Attacher: Attach
// finds a process by PID among those the fake started (the same launcher
// after a restart) and fails with ErrGone otherwise.
type FakeExec struct {
	// Hook runs on every Start with the command and the process about to
	// be returned. It creates the side effects a real process would (a
	// listening socket, a state file), preconfigures the process, or
	// returns an error to make Start fail. nil starts silently.
	Hook func(cmd Cmd, p *FakeProcess) error

	mu      sync.Mutex
	started []Cmd
	procs   []*FakeProcess
}

// Start implements Exec.
func (f *FakeExec) Start(ctx context.Context, cmd Cmd) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	p := &FakeProcess{pid: 4000 + len(f.procs) + 1, cmd: cmd, done: make(chan struct{})}
	f.started = append(f.started, cmd)
	f.procs = append(f.procs, p)
	f.mu.Unlock()
	if f.Hook != nil {
		if err := f.Hook(cmd, p); err != nil {
			return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
		}
	}
	return p, nil
}

// Attach implements Attacher.
func (f *FakeExec) Attach(ctx context.Context, h Handle) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.procs {
		if p.pid == h.PID {
			return p, nil
		}
	}
	return nil, fmt.Errorf("%w: pid %d", ErrGone, h.PID)
}

// Started are the commands passed to Start, in order.
func (f *FakeExec) Started() []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Cmd(nil), f.started...)
}

// Processes are the processes returned by Start, in order.
func (f *FakeExec) Processes() []*FakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*FakeProcess(nil), f.procs...)
}

// FakeProcess is the Process a FakeExec returns. It ends only when the test
// calls Exit, Kill is called, or ExitOnTerm is set and SIGTERM arrives.
type FakeProcess struct {
	// ExitOnTerm makes SIGTERM end the process with code 143, the way a
	// cooperative daemon does.
	ExitOnTerm bool

	pid     int
	cmd     Cmd
	mu      sync.Mutex
	signals []os.Signal
	once    sync.Once
	done    chan struct{}
	status  ExitStatus
}

// Cmd is the command this process was started with.
func (p *FakeProcess) Cmd() Cmd { return p.cmd }

// PID implements Process.
func (p *FakeProcess) PID() int { return p.pid }

// Handle implements Process; Unit is the Cmd's Unit, as a unit launcher
// would report it.
func (p *FakeProcess) Handle() Handle { return Handle{Unit: p.cmd.Unit, PID: p.pid} }

// Wait implements Process.
func (p *FakeProcess) Wait() <-chan ExitStatus { return waitOn(p.done, &p.status) }

// Signal implements Process.
func (p *FakeProcess) Signal(sig os.Signal) error {
	if p.Exited() {
		return os.ErrProcessDone
	}
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	term := p.ExitOnTerm && sig == syscall.SIGTERM
	p.mu.Unlock()
	if term {
		p.Exit(143)
	}
	return nil
}

// Kill implements Process: it records SIGKILL and ends the process the way
// the kernel would report it.
func (p *FakeProcess) Kill() error {
	if p.Exited() {
		return os.ErrProcessDone
	}
	p.mu.Lock()
	p.signals = append(p.signals, syscall.SIGKILL)
	p.mu.Unlock()
	p.end(ExitStatus{Code: -1, Err: errors.New("signal: killed")})
	return nil
}

// Exit ends the process with code; later calls are no-ops.
func (p *FakeProcess) Exit(code int) {
	st := ExitStatus{Code: code}
	if code != 0 {
		st.Err = fmt.Errorf("exit status %d", code)
	}
	p.end(st)
}

func (p *FakeProcess) end(st ExitStatus) {
	p.once.Do(func() {
		p.status = st
		close(p.done)
	})
}

// Exited reports whether the process has ended.
func (p *FakeProcess) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Signals are the signals received so far, in order.
func (p *FakeProcess) Signals() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}
