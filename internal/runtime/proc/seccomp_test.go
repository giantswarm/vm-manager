package proc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// ioUringHelperEnv makes TestIOUringHelper the child: it calls io_uring_setup
// and prints the error number, "0" when a ring was created.
const ioUringHelperEnv = "VM_MANAGER_TEST_IO_URING_HELPER"

func TestIOUringHelper(t *testing.T) {
	if os.Getenv(ioUringHelperEnv) != "1" {
		t.Skip("helper process for TestOSExecNoIOUring")
	}
	status, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Seccomp:") || strings.HasPrefix(line, "NoNewPrivs:") {
			fmt.Println(strings.Join(strings.Fields(line), " "))
		}
	}
	fmt.Printf("io_uring_setup errno=%d\n", int(ioUringSetup()))
	os.Exit(0)
}

// ioUringSetup creates and closes a one-entry ring and returns the error.
func ioUringSetup() unix.Errno {
	var params [120]byte                                                                             // struct io_uring_params
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, 1, uintptr(unsafe.Pointer(&params[0])), 0) // #nosec G103 -- test helper
	if errno == 0 {
		_ = unix.Close(int(fd))
	}
	return errno
}

func TestOSExecNoIOUring(t *testing.T) {
	before := ioUringSetup()
	out := NewTail(0)
	p, err := OSExec{}.Start(context.Background(), Cmd{
		Path:      os.Args[0],
		Args:      []string{"-test.run=^TestIOUringHelper$"},
		Env:       append(os.Environ(), ioUringHelperEnv+"=1"),
		Stdout:    out,
		Stderr:    out,
		NoIOUring: true,
	})
	require.NoError(t, err)
	require.Equal(t, 0, exitOf(t, p).Code, out.String())
	assert.Contains(t, out.String(), "Seccomp: 2", "the child runs under a filter")
	assert.Contains(t, out.String(), "NoNewPrivs: 1")
	assert.Contains(t, out.String(), fmt.Sprintf("io_uring_setup errno=%d", int(unix.EPERM)))
	assert.Equal(t, before, ioUringSetup(), "the filter stays with the child")
}
