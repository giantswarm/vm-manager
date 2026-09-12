package qemu

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrNotifyPortInUse is ListenNotifyPersistent's error when the port an
// earlier vm-manager recorded for this state dir is bound by someone else:
// most likely another vm-manager serving the same state dir.
var ErrNotifyPortInUse = errors.New("notify port in use")

// ListenNotifyPersistent is ListenNotify for a vm-manager that restarts:
// the guests' vmm.notify_socket credential names the port at boot, so a
// later vm-manager must listen on the same one or a reattached VM's
// READY=1 and STATUS= go nowhere. With port 0 the port recorded in file by
// an earlier run is reused (the kernel picks one on the first run); a
// non-zero port wins over the file. The bound port is recorded either way.
// A recorded port that is already bound fails with ErrNotifyPortInUse
// rather than silently moving.
func ListenNotifyPersistent(file string, port uint32, log *slog.Logger) (*NotifyListener, error) {
	return listenNotifyPersistent(file, port, func(p uint32) (*NotifyListener, error) { return ListenNotify(p, log) })
}

// listenNotifyPersistent is ListenNotifyPersistent with the bind
// injectable for tests.
func listenNotifyPersistent(file string, port uint32, listen func(uint32) (*NotifyListener, error)) (*NotifyListener, error) {
	recorded := false
	if port == 0 {
		p, err := readNotifyPort(file)
		if err != nil {
			return nil, err
		}
		port, recorded = p, p != 0
	}
	ln, err := listen(port)
	if err != nil {
		if recorded && errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("%w: vsock port %d recorded in %s is bound by another process; is another vm-manager serving this state directory? (%v)", ErrNotifyPortInUse, port, file, err)
		}
		return nil, err
	}
	if err := writeNotifyPort(file, ln.Port()); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// readNotifyPort is the port recorded in file, 0 when there is none.
func readNotifyPort(file string) (uint32, error) {
	data, err := os.ReadFile(file) // #nosec G304 -- the state dir's own file
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("notify port file: %w", err)
	}
	port, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
	if err != nil || port == 0 || port > math.MaxUint32 {
		return 0, fmt.Errorf("notify port file %s: %q is not a vsock port", file, strings.TrimSpace(string(data)))
	}
	return uint32(port), nil
}

// writeNotifyPort records port in file, atomically.
func writeNotifyPort(file string, port uint32) error {
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(uint64(port), 10)+"\n"), 0o600); err != nil {
		return fmt.Errorf("record notify port: %w", err)
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("record notify port: %w", err)
	}
	return nil
}

// NotifyPortFile is the conventional name of the port record below a
// state dir.
func NotifyPortFile(stateDir string) string { return filepath.Join(stateDir, "notify-port") }
