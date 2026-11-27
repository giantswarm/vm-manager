//go:build !linux

package proc

import (
	"errors"
	"os/exec"
)

// errNoIOUringDeny reports that NoIOUring needs the Linux seccomp filter.
var errNoIOUringDeny = errors.New("no_io_uring: unsupported on this platform, needs Linux seccomp")

// startDenyingIOUring is unsupported outside Linux.
func startDenyingIOUring(_ *exec.Cmd) error {
	return errNoIOUringDeny
}
