package proc

import (
	"fmt"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioUringDenyFilter is a seccomp program that fails io_uring_setup with
// EPERM, what a host with kernel.io_uring_disabled=2 answers, and allows
// every other system call. The processes it guards are native binaries, and
// io_uring_setup has the same number (425) on amd64, arm64 and their 32-bit
// compat ABIs, so the program needs no architecture check.
var ioUringDenyFilter = [...]unix.SockFilter{
	{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}, // seccomp_data.nr
	{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: unix.SYS_IO_URING_SETUP},
	{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
	{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
}

// startDenyingIOUring starts cmd with ioUringDenyFilter installed. Seccomp
// filters and no_new_privs belong to a thread and pass to the children it
// forks, so they go on a locked thread that forks cmd and then ends with its
// goroutine (no UnlockOSThread): no other goroutine ever runs under them, and
// the runtime creates new threads from its template thread, not this one.
func startDenyingIOUring(cmd *exec.Cmd) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := denyIOUring(); err != nil {
			errc <- err
			return
		}
		errc <- cmd.Start()
	}()
	return <-errc
}

func denyIOUring() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	prog := unix.SockFprog{Len: uint16(len(ioUringDenyFilter)), Filter: &ioUringDenyFilter[0]}
	// #nosec G103 -- the kernel reads the program during the call
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&prog)), 0, 0); err != nil {
		return fmt.Errorf("install seccomp filter: %w", err)
	}
	runtime.KeepAlive(&prog)
	return nil
}
