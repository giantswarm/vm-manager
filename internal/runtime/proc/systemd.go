package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Manager is the systemd instance a SystemdExec talks to.
type Manager string

const (
	// ManagerSystem is the system service manager; vm-manager must be root.
	ManagerSystem Manager = "system"
	// ManagerUser is the calling user's service manager (systemctl --user).
	ManagerUser Manager = "user"
)

// DefaultSlice groups the transient units below one slice. Slice names are
// hierarchical, so it sits under vm.slice.
const DefaultSlice = "vm-manager.slice"

// Timeouts of the launcher's own systemctl calls.
const (
	// ctlTimeout bounds one systemd-run or systemctl invocation.
	ctlTimeout = 15 * time.Second
	// statusTimeout bounds the wait for systemd to record the exit status
	// after the process is gone; statusPoll is the retry interval.
	statusTimeout = 5 * time.Second
	statusPoll    = 50 * time.Millisecond
)

// Values of ExecMainCode, siginfo si_code of the main process' end.
const (
	cldExited = 1
	cldKilled = 2
	cldDumped = 3
)

// Runner runs a short host command and returns its standard output; it is
// the shape of host.Runner. Tests inject a fake that stands in for
// systemd-run and systemctl.
type Runner interface {
	Output(ctx context.Context, name string, args ...string) (string, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, name string, args ...string) (string, error)

// Output implements Runner.
func (f RunnerFunc) Output(ctx context.Context, name string, args ...string) (string, error) {
	return f(ctx, name, args...)
}

// execRunner runs the command through os/exec; a failure carries stderr.
type execRunner struct{}

func (execRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- systemd-run and systemctl with arguments built here
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		label := name
		if len(args) > 0 {
			label += " " + args[0]
		}
		return "", fmt.Errorf("%s: %w: %s", label, err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// SystemdExec starts each command as a transient systemd service:
//
//	systemd-run [--user] --unit=<Cmd.Unit> --service-type=exec --slice=<Slice>
//	  -p RemainAfterExit=yes [-p StandardOutput=append:<Cmd.Log> ...] -- <cmd>
//
// systemd is the process' parent, so it lives on when the caller exits, and
// RemainAfterExit keeps the unit loaded after the process ended until the
// launcher releases it, so ExecMainCode/ExecMainStatus still tell how it
// ended when a later vm-manager attaches. The launcher learns of the exit
// through a pidfd on the main process and then reads the status from the
// unit. Signal and Kill go through systemctl kill, which targets the unit's
// cgroup and cannot hit a reused PID.
type SystemdExec struct {
	// Manager selects the service manager; ManagerSystem when empty.
	Manager Manager
	// Slice is the slice the units are placed in; DefaultSlice when empty.
	Slice string
	// Runner runs systemd-run and systemctl; nil uses os/exec.
	Runner Runner
	// Logger for the launcher's own events; nil uses slog.Default().
	Logger *slog.Logger
}

// Start implements Exec. Cmd.Unit is required; a unit of that name left
// over from an earlier run is released when its process is gone and a
// conflict when it still runs.
func (x *SystemdExec) Start(ctx context.Context, c Cmd) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.Unit == "" {
		return nil, fmt.Errorf("start %s: systemd launcher needs a unit name", c.Path)
	}
	path, err := exec.LookPath(c.Path)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Path, err)
	}
	unit := serviceName(c.Unit)
	st, err := x.show(ctx, unit)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Path, err)
	}
	if st.loaded() {
		if st.MainPID > 0 {
			return nil, fmt.Errorf("start %s: unit %s is already running (pid %d); is another vm-manager using this state directory?", c.Path, unit, st.MainPID)
		}
		x.log().Info("releasing stale unit", "unit", unit, "state", st.ActiveState+"/"+st.SubState)
		x.release(ctx, unit)
	}
	if c.Log != "" {
		// Pre-create it so the mode is ours rather than systemd's umask.
		f, err := openLog(c.Log)
		if err != nil {
			return nil, fmt.Errorf("start %s: %w", c.Path, err)
		}
		_ = f.Close()
	}
	if _, err := x.run(ctx, "systemd-run", x.runArgs(unit, path, c)...); err != nil {
		return nil, fmt.Errorf("start %s as %s: %w", c.Path, unit, err)
	}
	st, err = x.show(ctx, unit)
	if err != nil {
		return nil, fmt.Errorf("start %s as %s: %w", c.Path, unit, err)
	}
	return x.process(unit, st), nil
}

// runArgs is the systemd-run argument list for c.
func (x *SystemdExec) runArgs(unit, path string, c Cmd) []string {
	slice := x.Slice
	if slice == "" {
		slice = DefaultSlice
	}
	args := []string{
		"--unit=" + unit,
		"--service-type=exec",
		"--slice=" + slice,
		"--quiet",
		"--expand-environment=no",
		"--property=RemainAfterExit=yes",
	}
	if c.Log != "" {
		args = append(args, "--property=StandardOutput=append:"+c.Log, "--property=StandardError=append:"+c.Log)
	}
	if c.Dir != "" {
		args = append(args, "--working-directory="+c.Dir)
	}
	for _, kv := range c.Env {
		args = append(args, "--setenv="+kv)
	}
	args = append(args, "--", path)
	return append(args, c.Args...)
}

// Attach implements Attacher: the unit of h is looked up and, when loaded,
// watched like a unit this launcher started.
func (x *SystemdExec) Attach(ctx context.Context, h Handle) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.Unit == "" {
		return nil, fmt.Errorf("%w: pid %d was not started as a unit", ErrGone, h.PID)
	}
	unit := serviceName(h.Unit)
	st, err := x.show(ctx, unit)
	if err != nil {
		return nil, err
	}
	if !st.loaded() {
		return nil, fmt.Errorf("%w: unit %s", ErrGone, unit)
	}
	if h.PID != 0 && st.MainPID != 0 && st.MainPID != h.PID {
		x.log().Warn("unit main pid differs from the recorded one", "unit", unit, "recorded", h.PID, "mainPID", st.MainPID)
	}
	return x.process(unit, st), nil
}

// process wraps a loaded unit. A main process still running is watched
// through a pidfd; one that already ended is settled at once.
func (x *SystemdExec) process(unit string, st unitState) *unitProcess {
	p := &unitProcess{x: x, unit: unit, pid: st.MainPID, done: make(chan struct{})}
	if st.MainPID > 0 {
		fd, err := unix.PidfdOpen(st.MainPID, unix.PIDFD_NONBLOCK)
		if err == nil {
			go p.watch(os.NewFile(uintptr(fd), unit))
			return p
		}
		if !errors.Is(err, unix.ESRCH) {
			x.log().Warn("pidfd_open failed, polling the unit instead", "unit", unit, "pid", st.MainPID, "err", err)
			go p.poll()
			return p
		}
	}
	go p.settle()
	return p
}

// unitProcess is one transient service.
type unitProcess struct {
	x    *SystemdExec
	unit string
	pid  int

	once   sync.Once
	done   chan struct{}
	status ExitStatus
}

func (p *unitProcess) PID() int { return p.pid }

func (p *unitProcess) Handle() Handle { return Handle{Unit: p.unit, PID: p.pid} }

func (p *unitProcess) Wait() <-chan ExitStatus { return waitOn(p.done, &p.status) }

// Signal implements Process through systemctl kill on the main process.
func (p *unitProcess) Signal(sig os.Signal) error {
	if p.exited() {
		return os.ErrProcessDone
	}
	s, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("signal %v is not a unix signal", sig)
	}
	return p.kill("--kill-whom=main", "--signal="+unix.SignalName(s))
}

// Kill implements Process: SIGKILL to every process in the unit's cgroup.
func (p *unitProcess) Kill() error {
	if p.exited() {
		return os.ErrProcessDone
	}
	return p.kill("--signal=SIGKILL")
}

func (p *unitProcess) kill(flags ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), ctlTimeout)
	defer cancel()
	args := append([]string{"kill"}, flags...)
	_, err := p.x.run(ctx, "systemctl", append(args, p.unit)...)
	return err
}

func (p *unitProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// watch waits on the pidfd until the main process ended. The descriptor is
// non-blocking, so the runtime poller carries the wait; should it decline
// the descriptor, poll(2) on a thread does.
func (p *unitProcess) watch(pidfd *os.File) {
	defer func() { _ = pidfd.Close() }()
	rc, err := pidfd.SyscallConn()
	if err == nil {
		err = rc.Read(func(fd uintptr) bool { return pidfdReady(int(fd), 0) })
	}
	if err != nil {
		for !pidfdReady(int(pidfd.Fd()), -1) { //nolint:revive // retry on EINTR
		}
	}
	p.settle()
}

// pidfdReady polls fd for the exit of its process with the given timeout
// (poll(2) semantics: -1 blocks).
func pidfdReady(fd int, timeout int) bool {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} // #nosec G115 -- descriptors fit
	n, err := unix.Poll(fds, timeout)
	return err == nil && n > 0 && fds[0].Revents&unix.POLLIN != 0
}

// poll is the fallback without a pidfd: the unit is asked until its main
// process is gone.
func (p *unitProcess) poll() {
	for {
		time.Sleep(time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), ctlTimeout)
		st, err := p.x.show(ctx, p.unit)
		cancel()
		if err != nil || !st.loaded() || st.MainPID == 0 {
			p.settle()
			return
		}
	}
}

// settle reads the exit status once systemd recorded it, releases the unit
// and completes Wait.
func (p *unitProcess) settle() {
	p.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
		defer cancel()
		p.status = p.x.exitStatus(ctx, p.unit)
		p.x.release(ctx, p.unit)
		close(p.done)
	})
}

// exitStatus is how the unit's main process ended. systemd reaps it a
// moment after the pidfd fires, so the unit is asked until ExecMainCode is
// set; a unit that is gone, or still shows the process after the timeout,
// yields an unknown status (Code -1).
func (x *SystemdExec) exitStatus(ctx context.Context, unit string) ExitStatus {
	for {
		st, err := x.show(ctx, unit)
		switch {
		case err != nil:
			return ExitStatus{Code: -1, Err: fmt.Errorf("exit status unknown: %w", err)}
		case !st.loaded():
			return ExitStatus{Code: -1, Err: fmt.Errorf("exit status unknown: unit %s is gone", unit)}
		case st.MainPID == 0 && st.ExecMainCode != 0:
			return st.exit()
		}
		select {
		case <-ctx.Done():
			return ExitStatus{Code: -1, Err: fmt.Errorf("exit status unknown: unit %s still reports pid %d", unit, st.MainPID)}
		case <-time.After(statusPoll):
		}
	}
}

// release unloads the unit after its process ended: stop drops a unit that
// is active (exited), reset-failed one that failed. Errors are of no
// consequence, the unit is gone either way.
func (x *SystemdExec) release(ctx context.Context, unit string) {
	if _, err := x.run(ctx, "systemctl", "stop", unit); err != nil {
		x.log().Debug("stop released unit", "unit", unit, "err", err)
	}
	if _, err := x.run(ctx, "systemctl", "reset-failed", unit); err != nil {
		x.log().Debug("reset-failed released unit", "unit", unit, "err", err)
	}
}

// unitState is what systemctl show reports about a unit.
type unitState struct {
	LoadState      string
	ActiveState    string
	SubState       string
	MainPID        int
	ExecMainCode   int
	ExecMainStatus int
}

// showProperties are the properties show asks for.
const showProperties = "LoadState,ActiveState,SubState,MainPID,ExecMainCode,ExecMainStatus"

// loaded reports whether the manager knows the unit.
func (s unitState) loaded() bool { return s.LoadState != "" && s.LoadState != "not-found" }

// exit maps ExecMainCode/ExecMainStatus to an ExitStatus the way os/exec
// renders a *ExitError.
func (s unitState) exit() ExitStatus {
	switch s.ExecMainCode {
	case cldExited:
		if s.ExecMainStatus == 0 {
			return ExitStatus{}
		}
		return ExitStatus{Code: s.ExecMainStatus, Err: fmt.Errorf("exit status %d", s.ExecMainStatus)}
	case cldKilled, cldDumped:
		msg := "signal: " + syscall.Signal(s.ExecMainStatus).String()
		if s.ExecMainCode == cldDumped {
			msg += " (core dumped)"
		}
		return ExitStatus{Code: -1, Err: errors.New(msg)}
	default:
		return ExitStatus{Code: -1, Err: fmt.Errorf("exit status unknown: ExecMainCode %d", s.ExecMainCode)}
	}
}

// show queries the unit; a unit systemd does not know comes back with
// LoadState not-found rather than an error.
func (x *SystemdExec) show(ctx context.Context, unit string) (unitState, error) {
	out, err := x.run(ctx, "systemctl", "show", "--property="+showProperties, unit)
	if err != nil {
		return unitState{}, err
	}
	var st unitState
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			st.LoadState = value
		case "ActiveState":
			st.ActiveState = value
		case "SubState":
			st.SubState = value
		case "MainPID":
			st.MainPID, _ = strconv.Atoi(value)
		case "ExecMainCode":
			st.ExecMainCode, _ = strconv.Atoi(value)
		case "ExecMainStatus":
			st.ExecMainStatus, _ = strconv.Atoi(value)
		}
	}
	return st, nil
}

// run invokes systemd-run or systemctl against the configured manager.
func (x *SystemdExec) run(ctx context.Context, name string, args ...string) (string, error) {
	if x.Manager == ManagerUser {
		args = append([]string{"--user"}, args...)
	}
	r := x.Runner
	if r == nil {
		r = execRunner{}
	}
	return r.Output(ctx, name, args...)
}

func (x *SystemdExec) log() *slog.Logger {
	if x.Logger != nil {
		return x.Logger
	}
	return slog.Default()
}

// serviceName completes a unit name with the .service suffix.
func serviceName(unit string) string {
	if strings.HasSuffix(unit, ".service") {
		return unit
	}
	return unit + ".service"
}
