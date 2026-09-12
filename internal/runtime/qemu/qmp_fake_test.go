package qemu

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const fakeGreeting = `{"QMP": {"version": {"qemu": {"micro": 1, "minor": 1, "major": 11}, "package": "fake"}, "capabilities": ["oob"]}}`

// fakeQMP is an in-process QMP server on a unix socket. It answers the
// commands the runtime uses and lets tests override any of them, emit
// events and drop connections.
type fakeQMP struct {
	ln       net.Listener
	mu       sync.Mutex
	conns    []net.Conn
	commands []string
	// handler is consulted first; (nil, nil) falls through to the defaults.
	handler func(cmd string, args json.RawMessage) (any, *Error)
	// onPowerdown and onQuit run after the respective default replies.
	onPowerdown func()
	onQuit      func()
	wg          sync.WaitGroup
}

func startFakeQMP(t *testing.T, path string) *fakeQMP {
	t.Helper()
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	f := &fakeQMP{ln: ln}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(f.close)
	return f
}

func (f *fakeQMP) close() {
	_ = f.ln.Close()
	f.closeConns()
	f.wg.Wait()
}

func (f *fakeQMP) closeConns() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		_ = c.Close()
	}
	f.conns = nil
}

func (f *fakeQMP) serve() {
	defer f.wg.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, c)
		f.mu.Unlock()
		f.wg.Add(1)
		go f.handle(c)
	}
}

type fakeRequest struct {
	Execute   string          `json:"execute"`
	Arguments json.RawMessage `json:"arguments"`
	ID        json.RawMessage `json:"id"`
}

func (f *fakeQMP) handle(c net.Conn) {
	defer f.wg.Done()
	f.write(c, json.RawMessage(fakeGreeting))
	dec := json.NewDecoder(c)
	for {
		var req fakeRequest
		if err := dec.Decode(&req); err != nil {
			return
		}
		f.mu.Lock()
		f.commands = append(f.commands, req.Execute)
		h := f.handler
		f.mu.Unlock()
		var ret any
		var qerr *Error
		if h != nil {
			ret, qerr = h(req.Execute, req.Arguments)
		}
		if ret == nil && qerr == nil {
			ret, qerr = f.defaults(req.Execute)
		}
		resp := map[string]any{"id": req.ID}
		if qerr != nil {
			resp["error"] = qerr
		} else {
			resp["return"] = ret
		}
		f.write(c, resp)
		switch {
		case req.Execute == "system_powerdown" && qerr == nil && f.onPowerdown != nil:
			f.onPowerdown()
		case req.Execute == "quit" && qerr == nil:
			_ = c.Close()
			if f.onQuit != nil {
				f.onQuit()
			}
			return
		}
	}
}

func (f *fakeQMP) defaults(cmd string) (any, *Error) {
	switch cmd {
	case "qmp_capabilities", "system_powerdown", "quit":
		return map[string]any{}, nil
	case "query-status":
		return Status{Status: "running", Running: true}, nil
	default:
		return nil, &Error{Class: "CommandNotFound", Desc: "The command " + cmd + " has not been found"}
	}
}

func (f *fakeQMP) write(c net.Conn, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = c.Write(append(b, '\n'))
}

// emit sends ev to every connected client.
func (f *fakeQMP) emit(ev Event) {
	f.mu.Lock()
	conns := append([]net.Conn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		f.write(c, ev)
	}
}

// received are the command names in arrival order.
func (f *fakeQMP) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *fakeQMP) setHandler(h func(cmd string, args json.RawMessage) (any, *Error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
}
