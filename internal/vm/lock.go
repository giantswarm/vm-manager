package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// lockFile is the flock(2)ed file below the state dir that keeps two
// vm-managers from serving the same state: both would start the same
// units, bind the same sockets and rewrite the same records.
const lockFile = "lock"

// ErrStateDirInUse is New's error when another vm-manager holds the state
// dir's lock.
var ErrStateDirInUse = errors.New("state dir is in use by another vm-manager")

// lockStateDir takes the exclusive lock on <dir>/lock without waiting and
// writes the holder's PID into it for the next one's error message. The
// lock is held as long as the file is open; the kernel drops it when the
// process ends, so a crash leaves nothing stale behind.
func lockStateDir(dir string) (*os.File, error) {
	path := filepath.Join(dir, lockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- the state dir's own lock file
	if err != nil {
		return nil, fmt.Errorf("state dir lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(path) // #nosec G304 -- the state dir's own lock file
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is held by pid %s", ErrStateDirInUse, path, strings.TrimSpace(string(holder)))
		}
		return nil, fmt.Errorf("state dir lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return f, nil
}
