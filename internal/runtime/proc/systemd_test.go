package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSystemd stands in for systemd-run and systemctl of a user manager. A
// started unit runs a real `sleep` (a child of the test) so the pidfd path
// is exercised; show reports it the way systemd would, kill signals it,
// stop and reset-failed unload it.
type fakeSystemd struct {
	t     *testing.T
	mu    sync.Mutex
	calls [][]string
	units map[string]*fakeUnit
}

type fakeUnit struct {
	cmd    *exec.Cmd
	exited chan struct{}
	// scripted units (Attach tests) have no process.
	state unitState
}

func newFakeSystemd(t *testing.T) *fakeSystemd {
	return &fakeSystemd{t: t, units: map[string]*fakeUnit{}}
}

func (f *fakeSystemd) Output(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	require.Equal(f.t, "--user", args[0], "user manager calls carry --user first")
	args = args[1:]
	switch name {
	case "systemd-run":
		return "", f.systemdRun(args)
	case "systemctl":
		return f.systemctl(args)
	}
	return "", fmt.Errorf("unexpected command %s", name)
}

func (f *fakeSystemd) systemdRun(args []string) error {
	var unit string
	sep := -1
	for i, a := range args {
		if strings.HasPrefix(a, "--unit=") {
			unit = strings.TrimPrefix(a, "--unit=")
		}
		if a == "--" {
			sep = i
			break
		}
	}
	require.NotEmpty(f.t, unit)
	require.Positive(f.t, sep, "systemd-run separates the command with --")
	if _, dup := f.units[unit]; dup {
		return fmt.Errorf("Unit %s was already loaded or has a fragment file", unit)
	}
	f.spawn(unit, args[sep+1:]...)
	return nil
}

// spawn starts argv as the process of unit the way systemd-run would and
// returns its PID. The caller holds f.mu or is alone with the fake.
func (f *fakeSystemd) spawn(unit string, argv ...string) int {
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- the test's own command
	require.NoError(f.t, cmd.Start())
	f.t.Cleanup(func() { _ = cmd.Process.Kill() })
	u := &fakeUnit{cmd: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(u.exited)
	}()
	f.units[unit] = u
	return cmd.Process.Pid
}

func (f *fakeSystemd) systemctl(args []string) (string, error) {
	verb, unit := args[0], args[len(args)-1]
	u := f.units[unit]
	switch verb {
	case "show":
		if u == nil {
			return "LoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\nExecMainCode=0\nExecMainStatus=0\n", nil
		}
		st := u.status()
		return fmt.Sprintf("LoadState=%s\nActiveState=%s\nSubState=%s\nMainPID=%d\nExecMainCode=%d\nExecMainStatus=%d\n",
			st.LoadState, st.ActiveState, st.SubState, st.MainPID, st.ExecMainCode, st.ExecMainStatus), nil
	case "kill":
		if u == nil {
			return "", fmt.Errorf("Unit %s not loaded", unit)
		}
		sig := syscall.SIGKILL
		for _, a := range args[1 : len(args)-1] {
			if a == "--signal=SIGTERM" {
				sig = syscall.SIGTERM
			}
		}
		if u.cmd != nil && !u.done() {
			_ = u.cmd.Process.Signal(sig)
		}
		return "", nil
	case "stop":
		if u == nil {
			return "", fmt.Errorf("Unit %s not loaded", unit)
		}
		if u.cmd != nil && !u.done() {
			_ = u.cmd.Process.Kill()
			<-u.exited
		}
		delete(f.units, unit)
		return "", nil
	case "reset-failed":
		if u == nil {
			return "", fmt.Errorf("Unit %s not loaded", unit)
		}
		delete(f.units, unit)
		return "", nil
	}
	return "", fmt.Errorf("unexpected systemctl %s", verb)
}

func (u *fakeUnit) done() bool {
	select {
	case <-u.exited:
		return true
	default:
		return false
	}
}

// status is what systemd would show: the process while it runs, its wait
// status after.
func (u *fakeUnit) status() unitState {
	if u.cmd == nil {
		return u.state
	}
	if !u.done() {
		return unitState{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: u.cmd.Process.Pid}
	}
	ws := u.cmd.ProcessState.Sys().(syscall.WaitStatus)
	st := unitState{LoadState: "loaded", ActiveState: "active", SubState: "exited"}
	switch {
	case ws.Signaled():
		st.ExecMainCode, st.ExecMainStatus = cldKilled, int(ws.Signal())
	default:
		st.ExecMainCode, st.ExecMainStatus = cldExited, ws.ExitStatus()
		if ws.ExitStatus() != 0 {
			st.ActiveState, st.SubState = "failed", "failed"
		}
	}
	return st
}

// calledWith are the recorded calls whose name and verb match.
func (f *fakeSystemd) calledWith(name, verb string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == name && (verb == "" || c[2] == verb) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSystemd) loaded(unit string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.units[unit]
	return ok
}

func newSystemdExec(f *fakeSystemd) *SystemdExec {
	return &SystemdExec{Manager: ManagerUser, Runner: f, Logger: quietLogger()}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSystemdExecStart(t *testing.T) {
	f := newFakeSystemd(t)
	x := newSystemdExec(f)
	ctx := context.Background()
	log := filepath.Join(t.TempDir(), "vm", "qemu.log")

	_, err := x.Start(ctx, Cmd{Path: "sleep", Args: []string{"60"}})
	require.ErrorContains(t, err, "needs a unit name")

	p, err := x.Start(ctx, Cmd{Path: "sleep", Args: []string{"60"}, Unit: UnitName("abcd1234", "qemu"), Log: log, Dir: "/", Env: []string{"A=1"}})
	require.NoError(t, err)

	runs := f.calledWith("systemd-run", "")
	require.Len(t, runs, 1)
	args := runs[0][1:]
	sleep, err := exec.LookPath("sleep")
	require.NoError(t, err)
	for _, want := range []string{"--user", "--unit=vm-manager-abcd1234-qemu.service", "--service-type=exec", "--slice=" + DefaultSlice,
		"--quiet", "--expand-environment=no", "--property=RemainAfterExit=yes",
		"--property=StandardOutput=append:" + log, "--property=StandardError=append:" + log,
		"--working-directory=/", "--setenv=A=1", "--", sleep, "60"} {
		assert.Contains(t, args, want)
	}
	assert.Equal(t, []string{"--", sleep, "60"}, args[len(args)-3:], "command last")
	st, err := os.Stat(log)
	require.NoError(t, err, "log pre-created")
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	assert.Positive(t, p.PID())
	assert.Equal(t, Handle{Unit: "vm-manager-abcd1234-qemu.service", PID: p.PID()}, p.Handle())
	_, err = os.Stat(fmt.Sprintf("/proc/%d", p.PID()))
	require.NoError(t, err, "the unit's process runs")
	select {
	case st := <-p.Wait():
		t.Fatalf("exited early: %s", st)
	case <-time.After(50 * time.Millisecond):
	}

	// A second start of the same unit while it runs is a conflict.
	_, err = x.Start(ctx, Cmd{Path: "sleep", Args: []string{"60"}, Unit: UnitName("abcd1234", "qemu")})
	require.ErrorContains(t, err, "already running")

	require.NoError(t, p.Signal(syscall.SIGTERM))
	kills := f.calledWith("systemctl", "kill")
	require.Len(t, kills, 1)
	assert.Equal(t, []string{"systemctl", "--user", "kill", "--kill-whom=main", "--signal=SIGTERM", "vm-manager-abcd1234-qemu.service"}, kills[0])

	st2 := exitOf(t, p)
	assert.Equal(t, -1, st2.Code)
	assert.EqualError(t, st2.Err, "signal: terminated")
	assert.ErrorIs(t, p.Signal(syscall.SIGTERM), os.ErrProcessDone)
	assert.ErrorIs(t, p.Kill(), os.ErrProcessDone)
	assert.False(t, f.loaded("vm-manager-abcd1234-qemu.service"), "unit released after the exit")
	assert.Len(t, f.calledWith("systemctl", "stop"), 1)

	// The name is free again, and a clean exit maps to status 0.
	p, err = x.Start(ctx, Cmd{Path: "true", Unit: UnitName("abcd1234", "qemu")})
	require.NoError(t, err)
	st3 := exitOf(t, p)
	assert.Equal(t, 0, st3.Code)
	assert.NoError(t, st3.Err)
}

func TestSystemdExecStartReleasesStaleUnit(t *testing.T) {
	f := newFakeSystemd(t)
	f.units["vm-manager-aa-swtpm.service"] = &fakeUnit{state: unitState{LoadState: "loaded", ActiveState: "active", SubState: "exited", ExecMainCode: cldExited}}
	x := newSystemdExec(f)
	p, err := x.Start(context.Background(), Cmd{Path: "sleep", Args: []string{"60"}, Unit: "vm-manager-aa-swtpm"})
	require.NoError(t, err)
	assert.Len(t, f.calledWith("systemctl", "stop"), 1, "the leftover unit was stopped before the start")
	assert.Positive(t, p.PID())
}

func TestSystemdExecAttach(t *testing.T) {
	f := newFakeSystemd(t)
	ctx := context.Background()
	x := newSystemdExec(f)

	// The unit of a predecessor whose process still runs and which, that
	// vm-manager being gone, nobody watches.
	pid := f.spawn(beefUnit, "sleep", "60")
	p, err := x.Attach(ctx, Handle{Unit: UnitName("beef", "qemu"), PID: pid})
	require.NoError(t, err)
	assert.Equal(t, pid, p.PID())
	assert.Equal(t, Handle{Unit: beefUnit, PID: pid}, p.Handle())

	require.NoError(t, p.Kill())
	kills := f.calledWith("systemctl", "kill")
	require.Len(t, kills, 1)
	assert.Equal(t, []string{"systemctl", "--user", "kill", "--signal=SIGKILL", beefUnit}, kills[0])
	st := exitOf(t, p)
	assert.Equal(t, -1, st.Code)
	assert.EqualError(t, st.Err, "signal: killed")
	assert.False(t, f.loaded(beefUnit), "unit released after the exit")

	// Gone: nothing to attach to. A handle without a unit never had one.
	_, err = x.Attach(ctx, p.Handle())
	assert.ErrorIs(t, err, ErrGone)
	_, err = x.Attach(ctx, Handle{PID: 4711})
	assert.ErrorIs(t, err, ErrGone)

	// A unit whose process ended while nobody watched still tells how.
	f.units["vm-manager-dead-qemu.service"] = &fakeUnit{state: unitState{LoadState: "loaded", ActiveState: "failed", SubState: "failed", ExecMainCode: cldExited, ExecMainStatus: 3}}
	p3, err := x.Attach(ctx, Handle{Unit: "vm-manager-dead-qemu", PID: 99999})
	require.NoError(t, err)
	st = exitOf(t, p3)
	assert.Equal(t, 3, st.Code)
	assert.EqualError(t, st.Err, "exit status 3")
	assert.False(t, f.loaded("vm-manager-dead-qemu.service"))
}

// beefUnit is the unit the Attach tests share.
const beefUnit = "vm-manager-beef-qemu.service"

// TestSystemdExecAttachWhileWatched has the launcher that started a unit
// still watching it when a second one attaches, as when a test keeps the
// predecessor alive. Both watchers see the exit; which of them settles
// first, and so reads the status and releases the unit, is up to the
// scheduler.
func TestSystemdExecAttachWhileWatched(t *testing.T) {
	f := newFakeSystemd(t)
	ctx := context.Background()
	first := newSystemdExec(f)
	p1, err := first.Start(ctx, Cmd{Path: "sleep", Args: []string{"60"}, Unit: UnitName("beef", "qemu")})
	require.NoError(t, err)

	second := newSystemdExec(f)
	p2, err := second.Attach(ctx, p1.Handle())
	require.NoError(t, err)
	assert.Equal(t, p1.PID(), p2.PID())
	assert.Equal(t, p1.Handle(), p2.Handle())

	require.NoError(t, p2.Kill())
	assertWatchersSettled(t, exitOf(t, p1), exitOf(t, p2), "signal: killed", beefUnit)
	assert.False(t, f.loaded(beefUnit), "unit released after the exit")
	assert.ErrorIs(t, p1.Kill(), os.ErrProcessDone)
}

// assertWatchersSettled checks what two watchers of one unit report once
// its process died of signal: both codes are -1, the watcher that released
// the unit read the signal, and the other read the same or, coming after
// the release, only that the unit is gone.
func assertWatchersSettled(t *testing.T, a, b ExitStatus, signal, unit string) {
	t.Helper()
	assert.Equal(t, -1, a.Code)
	assert.Equal(t, -1, b.Code)
	require.Error(t, a.Err)
	require.Error(t, b.Err)
	got := []string{a.Err.Error(), b.Err.Error()}
	assert.Contains(t, got, signal, "the watcher that released the unit read its status first")
	gone := fmt.Sprintf("exit status unknown: unit %s is gone", unit)
	for _, e := range got {
		assert.Contains(t, []string{signal, gone}, e)
	}
}

func TestUnitStateExit(t *testing.T) {
	for name, tc := range map[string]struct {
		st   unitState
		code int
		err  string
	}{
		"clean":   {unitState{ExecMainCode: cldExited}, 0, ""},
		"code":    {unitState{ExecMainCode: cldExited, ExecMainStatus: 2}, 2, "exit status 2"},
		"killed":  {unitState{ExecMainCode: cldKilled, ExecMainStatus: 9}, -1, "signal: killed"},
		"term":    {unitState{ExecMainCode: cldKilled, ExecMainStatus: 15}, -1, "signal: terminated"},
		"dumped":  {unitState{ExecMainCode: cldDumped, ExecMainStatus: 11}, -1, "signal: segmentation fault (core dumped)"},
		"unknown": {unitState{ExecMainCode: 7}, -1, "exit status unknown: ExecMainCode 7"},
	} {
		t.Run(name, func(t *testing.T) {
			got := tc.st.exit()
			assert.Equal(t, tc.code, got.Code)
			if tc.err == "" {
				assert.NoError(t, got.Err)
			} else {
				assert.EqualError(t, got.Err, tc.err)
			}
		})
	}
}

func TestSystemdExecShowError(t *testing.T) {
	boom := errors.New("dbus down")
	x := &SystemdExec{Manager: ManagerUser, Logger: quietLogger(), Runner: RunnerFunc(func(context.Context, string, ...string) (string, error) {
		return "", boom
	})}
	_, err := x.Start(context.Background(), Cmd{Path: "true", Unit: "u"})
	assert.ErrorIs(t, err, boom)
	_, err = x.Attach(context.Background(), Handle{Unit: "u"})
	assert.ErrorIs(t, err, boom)
}

func TestSelectLauncher(t *testing.T) {
	l, err := SelectLauncher(LauncherProcess, nil)
	require.NoError(t, err)
	assert.IsType(t, OSExec{}, l.Exec)
	assert.False(t, l.Persistent())
	assert.Equal(t, LauncherProcess, l.Mode())

	_, err = SelectLauncher("scope", nil)
	require.ErrorContains(t, err, `launcher "scope"`)

	if os.Geteuid() == 0 {
		t.Skip("the user manager cases need an unprivileged caller")
	}
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	_, err = SelectLauncher(LauncherSystemd, nil)
	require.ErrorContains(t, err, "systemd launcher unavailable")
	l, err = SelectLauncher(LauncherAuto, nil)
	require.NoError(t, err)
	assert.IsType(t, OSExec{}, l.Exec)
	assert.Contains(t, l.Reason, "no user service manager")

	// With a manager socket in place the user manager is chosen.
	require.NoError(t, os.MkdirAll(filepath.Join(runtimeDir, "systemd"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "systemd", "private"), nil, 0o600))
	l, err = SelectLauncher(LauncherAuto, nil)
	if err != nil {
		t.Skipf("systemd-run missing: %v", err)
	}
	if !l.Persistent() {
		t.Skipf("no systemd launcher: %s", l.Reason)
	}
	assert.Equal(t, ManagerUser, l.Manager)
	assert.Equal(t, LauncherSystemd, l.Mode())
	assert.Equal(t, ManagerUser, l.Exec.(*SystemdExec).Manager)
}
