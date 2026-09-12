package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dialFake(t *testing.T) (*QMP, *fakeQMP) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qmp.sock")
	f := startFakeQMP(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q, err := dialQMP(ctx, path, quiet)
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Close() })
	return q, f
}

func TestQMP(t *testing.T) {
	ctx := context.Background()
	t.Run("handshake and commands", func(t *testing.T) {
		q, f := dialFake(t)
		assert.Equal(t, []string{"qmp_capabilities"}, f.received())
		st, err := q.QueryStatus(ctx)
		require.NoError(t, err)
		assert.Equal(t, Status{Status: "running", Running: true}, st)
		require.NoError(t, q.SystemPowerdown(ctx))
		var raw json.RawMessage
		require.NoError(t, q.Execute(ctx, "query-status", map[string]any{"x": 1}, &raw))
		assert.JSONEq(t, `{"status":"running","running":true}`, string(raw))
		assert.Equal(t, []string{"qmp_capabilities", "query-status", "system_powerdown", "query-status"}, f.received())
		assert.Zero(t, q.Dropped())
	})
	t.Run("command errors", func(t *testing.T) {
		q, f := dialFake(t)
		f.setHandler(func(cmd string, _ json.RawMessage) (any, *Error) {
			if cmd == "query-status" {
				return nil, &Error{Class: "GenericError", Desc: "boom"}
			}
			return nil, nil
		})
		_, err := q.QueryStatus(ctx)
		require.EqualError(t, err, "qmp query-status: GenericError: boom")
		var qerr *Error
		require.ErrorAs(t, err, &qerr)
		assert.Equal(t, "GenericError", qerr.Class)
		err = q.Execute(ctx, "no-such-command", nil, nil)
		require.ErrorAs(t, err, &qerr)
		assert.Equal(t, "CommandNotFound", qerr.Class)
		err = q.Execute(ctx, "query-status", make(chan int), nil)
		assert.ErrorContains(t, err, "encode")
	})
	t.Run("events", func(t *testing.T) {
		q, f := dialFake(t)
		f.emit(Event{Name: EventShutdown, Data: json.RawMessage(`{"guest":true,"reason":"guest-shutdown"}`), Timestamp: Timestamp{Seconds: 7, Microseconds: 8}})
		select {
		case ev := <-q.Events():
			assert.Equal(t, EventShutdown, ev.Name)
			assert.JSONEq(t, `{"guest":true,"reason":"guest-shutdown"}`, string(ev.Data))
			assert.Equal(t, Timestamp{Seconds: 7, Microseconds: 8}, ev.Timestamp)
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
		}
		for range eventBuffer + 3 {
			f.emit(Event{Name: EventReset})
		}
		assert.Eventually(t, func() bool { return q.Dropped() == 3 }, 5*time.Second, 10*time.Millisecond)
		assert.Len(t, q.Events(), eventBuffer)
	})
	t.Run("context deadline while waiting", func(t *testing.T) {
		q, f := dialFake(t)
		release := make(chan struct{})
		f.setHandler(func(cmd string, _ json.RawMessage) (any, *Error) {
			if cmd == "system_powerdown" {
				<-release
			}
			return nil, nil
		})
		tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		err := q.SystemPowerdown(tctx)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		close(release)
		st, err := q.QueryStatus(ctx)
		require.NoError(t, err, "the late reply is discarded, later commands work")
		assert.True(t, st.Running)
	})
	t.Run("quit closes the connection", func(t *testing.T) {
		q, _ := dialFake(t)
		require.NoError(t, q.Quit(ctx))
		select {
		case <-q.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("connection stayed open")
		}
		_, ok := <-q.Events()
		assert.False(t, ok, "events channel closes with the connection")
		err := q.SystemPowerdown(ctx)
		assert.ErrorContains(t, err, "connection closed")
		require.NoError(t, q.Close())
	})
	t.Run("server drops mid-command", func(t *testing.T) {
		q, f := dialFake(t)
		f.setHandler(func(cmd string, _ json.RawMessage) (any, *Error) {
			if cmd == "query-status" {
				f.closeConns()
			}
			return nil, nil
		})
		_, err := q.QueryStatus(ctx)
		assert.ErrorContains(t, err, "connection closed")
	})
	t.Run("close then execute", func(t *testing.T) {
		q, _ := dialFake(t)
		require.NoError(t, q.Close())
		err := q.Execute(ctx, "query-status", nil, nil)
		assert.ErrorIs(t, err, net.ErrClosed)
	})
	t.Run("concurrent commands", func(t *testing.T) {
		q, f := dialFake(t)
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				st, err := q.QueryStatus(ctx)
				assert.NoError(t, err)
				assert.True(t, st.Running)
			}()
		}
		wg.Wait()
		assert.Len(t, f.received(), 21)
	})
}

func TestDialQMPErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Run("no socket", func(t *testing.T) {
		_, err := DialQMP(ctx, filepath.Join(t.TempDir(), "missing.sock"))
		require.Error(t, err)
	})
	serveOnce := func(t *testing.T, payload string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "s.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(payload))
			_ = c.Close()
		}()
		return path
	}
	t.Run("not a QMP greeting", func(t *testing.T) {
		_, err := DialQMP(ctx, serveOnce(t, `{"hello":1}`))
		assert.EqualError(t, err, "qmp greeting: not a QMP server")
	})
	t.Run("garbage greeting", func(t *testing.T) {
		_, err := DialQMP(ctx, serveOnce(t, "not json"))
		assert.ErrorContains(t, err, "qmp greeting")
	})
	t.Run("connection dropped after greeting", func(t *testing.T) {
		_, err := DialQMP(ctx, serveOnce(t, fakeGreeting))
		require.Error(t, err)
		assert.ErrorContains(t, err, "qmp qmp_capabilities")
	})
	t.Run("greeting never arrives", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		tctx, tcancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer tcancel()
		_, err = DialQMP(tctx, path)
		require.Error(t, err)
		var nerr net.Error
		assert.True(t, errors.As(err, &nerr) && nerr.Timeout(), "expected a timeout, got %v", err)
	})
}
