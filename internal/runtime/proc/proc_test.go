package proc

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const waitTimeout = 5 * time.Second

// exitOf receives the status or fails the test after waitTimeout.
func exitOf(t *testing.T, p Process) ExitStatus {
	t.Helper()
	select {
	case st := <-p.Wait():
		return st
	case <-time.After(waitTimeout):
		t.Fatal("process did not exit")
		return ExitStatus{}
	}
}

func TestOSExec(t *testing.T) {
	ctx := context.Background()
	t.Run("clean exit with captured output", func(t *testing.T) {
		out := NewTail(0)
		p, err := OSExec{}.Start(ctx, Cmd{Path: "sh", Args: []string{"-c", "echo out; echo err >&2"}, Stdout: out, Stderr: out})
		require.NoError(t, err)
		assert.Positive(t, p.PID())
		st := exitOf(t, p)
		assert.Equal(t, 0, st.Code)
		assert.NoError(t, st.Err)
		assert.Equal(t, "exit status 0", st.String())
		assert.Contains(t, out.String(), "out")
		assert.Contains(t, out.String(), "err")
	})
	t.Run("non-zero exit", func(t *testing.T) {
		p, err := OSExec{}.Start(ctx, Cmd{Path: "sh", Args: []string{"-c", "exit 3"}})
		require.NoError(t, err)
		st := exitOf(t, p)
		assert.Equal(t, 3, st.Code)
		assert.EqualError(t, st.Err, "exit status 3")
	})
	t.Run("signal and kill", func(t *testing.T) {
		p, err := OSExec{}.Start(ctx, Cmd{Path: "sleep", Args: []string{"30"}})
		require.NoError(t, err)
		require.NoError(t, p.Kill())
		st := exitOf(t, p)
		assert.Equal(t, -1, st.Code)
		assert.Contains(t, st.String(), "killed")
		assert.ErrorIs(t, p.Signal(syscall.SIGTERM), os.ErrProcessDone)
		assert.ErrorIs(t, p.Kill(), os.ErrProcessDone)
	})
	t.Run("missing binary", func(t *testing.T) {
		_, err := OSExec{}.Start(ctx, Cmd{Path: "definitely-not-a-binary-vm-manager"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "definitely-not-a-binary-vm-manager")
	})
	t.Run("cancelled context", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := OSExec{}.Start(cctx, Cmd{Path: "true"})
		assert.ErrorIs(t, err, context.Canceled)
	})
	t.Run("every waiter gets the status", func(t *testing.T) {
		p, err := OSExec{}.Start(ctx, Cmd{Path: "true"})
		require.NoError(t, err)
		a, b := p.Wait(), p.Wait()
		assert.Equal(t, 0, (<-a).Code)
		assert.Equal(t, 0, (<-b).Code)
	})
}

func TestFakeExec(t *testing.T) {
	ctx := context.Background()
	t.Run("records commands and drives exit", func(t *testing.T) {
		var hooked []string
		f := &FakeExec{Hook: func(cmd Cmd, p *FakeProcess) error {
			hooked = append(hooked, cmd.Path)
			_, _ = p.Cmd().Stderr.Write([]byte("boom\n"))
			return nil
		}}
		tail := NewTail(0)
		p, err := f.Start(ctx, Cmd{Path: "qemu", Args: []string{"-x"}, Stderr: tail})
		require.NoError(t, err)
		assert.Equal(t, []string{"qemu"}, hooked)
		assert.Equal(t, "boom", tail.String())
		assert.Equal(t, 4001, p.PID())
		assert.Len(t, f.Started(), 1)
		assert.Equal(t, []string{"-x"}, f.Started()[0].Args)
		fp := f.Processes()[0]
		assert.False(t, fp.Exited())
		require.NoError(t, p.Signal(syscall.SIGTERM))
		assert.False(t, fp.Exited(), "SIGTERM alone does not end the fake")
		fp.Exit(7)
		fp.Exit(0)
		st := exitOf(t, p)
		assert.Equal(t, 7, st.Code)
		assert.EqualError(t, st.Err, "exit status 7")
		assert.Equal(t, []os.Signal{syscall.SIGTERM}, fp.Signals())
		assert.ErrorIs(t, p.Signal(syscall.SIGTERM), os.ErrProcessDone)
	})
	t.Run("hook error fails start", func(t *testing.T) {
		f := &FakeExec{Hook: func(Cmd, *FakeProcess) error { return errors.New("no such file") }}
		_, err := f.Start(ctx, Cmd{Path: "swtpm"})
		assert.EqualError(t, err, "start swtpm: no such file")
	})
	t.Run("exit on term and kill", func(t *testing.T) {
		f := &FakeExec{Hook: func(_ Cmd, p *FakeProcess) error { p.ExitOnTerm = true; return nil }}
		p, err := f.Start(ctx, Cmd{Path: "swtpm"})
		require.NoError(t, err)
		require.NoError(t, p.Signal(syscall.SIGTERM))
		assert.Equal(t, 143, exitOf(t, p).Code)

		q, err := f.Start(ctx, Cmd{Path: "qemu"})
		require.NoError(t, err)
		require.NoError(t, q.Kill())
		st := exitOf(t, q)
		assert.Equal(t, -1, st.Code)
		assert.Equal(t, "signal: killed", st.String())
		assert.ErrorIs(t, q.Kill(), os.ErrProcessDone)
	})
	t.Run("cancelled context", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := (&FakeExec{}).Start(cctx, Cmd{Path: "qemu"})
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestTail(t *testing.T) {
	tests := []struct {
		name   string
		limit  int
		writes []string
		want   string
	}{
		{name: "under limit", limit: 8, writes: []string{"ab", "cd"}, want: "abcd"},
		{name: "rolls over", limit: 4, writes: []string{"abc", "def"}, want: "cdef"},
		{name: "single write over limit", limit: 4, writes: []string{"abcdefgh"}, want: "efgh"},
		{name: "trims whitespace", limit: 16, writes: []string{"x\n\n"}, want: "x"},
		{name: "default limit", limit: 0, writes: []string{strings.Repeat("y", DefaultTailBytes+5)}, want: strings.Repeat("y", DefaultTailBytes)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tail := NewTail(tc.limit)
			for _, w := range tc.writes {
				n, err := tail.Write([]byte(w))
				require.NoError(t, err)
				assert.Equal(t, len(w), n)
			}
			assert.Equal(t, tc.want, tail.String())
		})
	}
}
