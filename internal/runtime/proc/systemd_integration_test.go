//go:build integration

package proc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegrationSystemdExec drives the real service manager of this host:
// a sleep is started as a transient unit, a second launcher (the next
// vm-manager) attaches to it, ends it, and both observe the same status.
func TestIntegrationSystemdExec(t *testing.T) {
	l, err := SelectLauncher(LauncherAuto, quietLogger())
	require.NoError(t, err)
	if !l.Persistent() {
		t.Skipf("no service manager: %s", l.Reason)
	}
	x := l.Exec.(*SystemdExec)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var b [4]byte
	_, _ = rand.Read(b[:])
	id := "itest" + hex.EncodeToString(b[:])
	log := filepath.Join(t.TempDir(), "sleep.log")

	p, err := x.Start(ctx, Cmd{Path: "sh", Args: []string{"-c", "echo started; exec sleep 300"}, Unit: UnitName(id, "sleep"), Log: log})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Kill() })
	unit := UnitName(id, "sleep") + ".service"
	assert.Equal(t, Handle{Unit: unit, PID: p.PID()}, p.Handle())

	out := systemctlShow(t, x.Manager, unit, "ActiveState,MainPID,ControlGroup")
	assert.Contains(t, out, "ActiveState=active")
	assert.Contains(t, out, "MainPID="+strconv.Itoa(p.PID()))
	assert.Contains(t, out, "/vm-manager.slice/"+unit, "the unit lives in its own cgroup below the slice")
	require.Eventually(t, func() bool {
		data, _ := os.ReadFile(log) // #nosec G304 -- the test's log
		return strings.Contains(string(data), "started")
	}, 5*time.Second, 50*time.Millisecond, "output reaches the log file")

	// The next vm-manager: a fresh launcher attaches by handle.
	second := &SystemdExec{Manager: x.Manager, Logger: quietLogger()}
	p2, err := second.Attach(ctx, p.Handle())
	require.NoError(t, err)
	assert.Equal(t, p.PID(), p2.PID())
	require.NoError(t, p2.Signal(syscall.SIGTERM))
	// The first launcher's watcher fires too; whichever settles first
	// releases the unit, and the other may only learn that it is gone.
	assertWatchersSettled(t, exitOf(t, p), exitOf(t, p2), "signal: terminated", unit)
	assert.Contains(t, systemctlShow(t, x.Manager, unit, "LoadState"), "LoadState=not-found", "unit released")
	_, err = second.Attach(ctx, p.Handle())
	assert.ErrorIs(t, err, ErrGone)

	// An exit code survives the round trip through the unit.
	p3, err := x.Start(ctx, Cmd{Path: "sh", Args: []string{"-c", "exit 3"}, Unit: UnitName(id, "exit")})
	require.NoError(t, err)
	st := exitOf(t, p3)
	assert.Equal(t, 3, st.Code)
	assert.EqualError(t, st.Err, "exit status 3")
}

func systemctlShow(t *testing.T, m Manager, unit, props string) string {
	t.Helper()
	args := []string{"show", "--property=" + props, unit}
	if m == ManagerUser {
		args = append([]string{"--user"}, args...)
	}
	out, err := exec.Command("systemctl", args...).CombinedOutput() // #nosec G204 -- fixed binary
	require.NoError(t, err, "%s", out)
	return string(out)
}
