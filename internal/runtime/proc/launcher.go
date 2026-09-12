package proc

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// LauncherMode is how a Launcher was chosen or is requested.
type LauncherMode string

const (
	// LauncherAuto picks the systemd launcher when a service manager is
	// reachable and falls back to plain processes.
	LauncherAuto LauncherMode = "auto"
	// LauncherSystemd requires a service manager and fails without one.
	LauncherSystemd LauncherMode = "systemd"
	// LauncherProcess runs plain child processes that end with the caller.
	LauncherProcess LauncherMode = "process"
)

// LauncherModes are the valid values of a --launcher flag.
var LauncherModes = []LauncherMode{LauncherAuto, LauncherSystemd, LauncherProcess}

// Launcher is the Exec the runtime packages start their processes with and
// how it was chosen.
type Launcher struct {
	Exec Exec
	// Manager is the service manager behind a systemd launcher; empty for
	// plain processes.
	Manager Manager
	// Reason says why this launcher was chosen, for the startup log.
	Reason string
}

// Persistent reports whether processes outlive the caller, which is what
// makes reattaching after a restart possible.
func (l Launcher) Persistent() bool { return l.Manager != "" }

// Mode is the launcher in use: systemd or process.
func (l Launcher) Mode() LauncherMode {
	if l.Persistent() {
		return LauncherSystemd
	}
	return LauncherProcess
}

// systemManagerDir exists while the system instance of systemd runs.
const systemManagerDir = "/run/systemd/system"

// SelectLauncher chooses the launcher for mode. The systemd launcher uses
// the system manager when the caller is root and the user's own manager
// (systemctl --user) otherwise, reached through $XDG_RUNTIME_DIR; without
// either, or without systemd-run on PATH, LauncherAuto falls back to plain
// processes and LauncherSystemd fails.
func SelectLauncher(mode LauncherMode, log *slog.Logger) (Launcher, error) {
	if log == nil {
		log = slog.Default()
	}
	switch mode {
	case LauncherProcess:
		return Launcher{Exec: OSExec{}, Reason: "requested"}, nil
	case LauncherAuto, LauncherSystemd:
	default:
		return Launcher{}, fmt.Errorf("launcher %q is not one of %v", mode, LauncherModes)
	}
	manager, runtimeDir, reason := detectManager()
	if manager == "" {
		if mode == LauncherSystemd {
			return Launcher{}, fmt.Errorf("systemd launcher unavailable: %s", reason)
		}
		return Launcher{Exec: OSExec{}, Reason: reason}, nil
	}
	return Launcher{Exec: &SystemdExec{Manager: manager, RuntimeDir: runtimeDir, Logger: log}, Manager: manager, Reason: reason}, nil
}

// userRuntimeDir is where the user manager of uid lives when
// $XDG_RUNTIME_DIR is not set, as in a system service with User= whose
// user has lingering enabled (loginctl enable-linger).
func userRuntimeDir(uid int) string { return filepath.Join("/run/user", strconv.Itoa(uid)) }

// detectManager finds a reachable service manager and, for the user
// manager, its runtime dir; the empty Manager says why there is none.
func detectManager() (manager Manager, runtimeDir, reason string) {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return "", "", "systemd-run is not on PATH"
	}
	if os.Geteuid() == 0 {
		if _, err := os.Stat(systemManagerDir); err == nil {
			return ManagerSystem, "", "running as root with the system manager at " + systemManagerDir
		}
		return "", "", "running as root but " + systemManagerDir + " does not exist"
	}
	runtimeDir = os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = userRuntimeDir(os.Geteuid())
	}
	private := filepath.Join(runtimeDir, "systemd", "private")
	if _, err := os.Stat(private); err != nil { // #nosec G703 -- the login session's runtime dir, only probed
		return "", "", "no user service manager at " + private + " (for a service user: loginctl enable-linger)"
	}
	return ManagerUser, runtimeDir, "user service manager at " + private
}
