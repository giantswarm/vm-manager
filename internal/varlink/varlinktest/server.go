// Package varlinktest is an in-process fake Varlink service for tests, in
// the spirit of net/http/httptest. It speaks the wire protocol of package
// varlink (NUL-terminated JSON over AF_UNIX, SCM_RIGHTS for files) so the
// client and the packages built on it are tested end to end without
// systemd. The server answers org.varlink.service.GetInfo and
// GetInterfaceDescription itself from Interfaces; every other method goes
// to the Handler.
package varlinktest

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// Call is one request as the server received it.
type Call struct {
	Method     string          `json:"method"`
	Parameters json.RawMessage `json:"parameters"`
	More       bool            `json:"more"`
	Oneway     bool            `json:"oneway"`
}

// Reply is one message the server sends back. Parameters is marshalled as
// the reply's parameters (nil becomes {}). Error, if set, makes this an
// error reply. Files are sent as SCM_RIGHTS with the message and closed
// afterwards; the reply parameters are expected to refer to them by index.
type Reply struct {
	Parameters any
	Error      string
	Files      []*os.File
}

// Handler answers one call with zero or more replies. For a "more" call
// every reply but the last is sent with continues=true; a call without
// "more" gets only the first reply. No replies means an empty success.
type Handler func(Call) []Reply

// Server is a fake Varlink service listening on a unix socket.
type Server struct {
	// Path is the socket path to Dial.
	Path string
	// Interfaces maps interface names to their Varlink IDL, served by
	// GetInterfaceDescription and listed by GetInfo.
	Interfaces map[string]string

	t       testing.TB
	handler Handler
	ln      *net.UnixListener
	mu      sync.Mutex
	calls   []Call
	wg      sync.WaitGroup
}

// New starts a server in t.TempDir() and stops it when the test ends.
func New(t testing.TB, handler Handler) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "varlink.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("varlinktest: listen %s: %v", path, err)
	}
	s := &Server{Path: path, Interfaces: map[string]string{}, t: t, handler: handler, ln: ln}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

// Calls returns every request received so far, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.AcceptUnix()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.serve(conn)
	}
}

func (s *Server) serve(conn *net.UnixConn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		msg, err := r.ReadBytes(0)
		if err != nil {
			return
		}
		var call Call
		if err := json.Unmarshal(msg[:len(msg)-1], &call); err != nil {
			s.t.Errorf("varlinktest: malformed request %q: %v", msg, err)
			return
		}
		s.mu.Lock()
		s.calls = append(s.calls, call)
		s.mu.Unlock()
		if call.Oneway {
			continue
		}
		// A write error means the client hung up (cancelled context, closed
		// connection), which is a normal end of the conversation.
		if err := s.respond(conn, call); err != nil {
			return
		}
	}
}

// respond writes the replies for call and returns the first write error.
func (s *Server) respond(conn *net.UnixConn, call Call) error {
	replies := s.replies(call)
	if len(replies) == 0 {
		replies = []Reply{{}}
	}
	if !call.More {
		replies = replies[:1]
	}
	for i, rep := range replies {
		wire := map[string]any{"parameters": rep.Parameters}
		if rep.Parameters == nil {
			wire["parameters"] = struct{}{}
		}
		switch {
		case rep.Error != "":
			wire["error"] = rep.Error
		case call.More && i < len(replies)-1:
			wire["continues"] = true
		}
		b, err := json.Marshal(wire)
		if err != nil {
			s.t.Errorf("varlinktest: %s: encode reply: %v", call.Method, err)
			return nil
		}
		b = append(b, 0)
		if len(rep.Files) == 0 {
			_, err = conn.Write(b)
		} else {
			fds := make([]int, len(rep.Files))
			for j, f := range rep.Files {
				fds[j] = int(f.Fd())
			}
			_, _, err = conn.WriteMsgUnix(b, unix.UnixRights(fds...), nil)
			for _, f := range rep.Files {
				_ = f.Close()
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) replies(call Call) []Reply {
	switch call.Method {
	case "org.varlink.service.GetInfo":
		names := slices.Sorted(func(yield func(string) bool) {
			for name := range s.Interfaces {
				if !yield(name) {
					return
				}
			}
		})
		return []Reply{{Parameters: map[string]any{
			"vendor": "vm-manager", "product": "varlinktest", "version": "0", "url": "",
			"interfaces": append([]string{"org.varlink.service"}, names...),
		}}}
	case "org.varlink.service.GetInterfaceDescription":
		var p struct {
			Interface string `json:"interface"`
		}
		_ = json.Unmarshal(call.Parameters, &p)
		desc, ok := s.Interfaces[p.Interface]
		if !ok {
			return []Reply{{Error: "org.varlink.service.InterfaceNotFound", Parameters: map[string]string{"interface": p.Interface}}}
		}
		return []Reply{{Parameters: map[string]string{"description": desc}}}
	}
	if s.handler == nil {
		return []Reply{{Error: "org.varlink.service.MethodNotFound", Parameters: map[string]string{"method": call.Method}}}
	}
	return s.handler(call)
}
