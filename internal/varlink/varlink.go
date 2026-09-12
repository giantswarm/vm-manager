// Package varlink is a minimal Varlink client for the systemd services
// vm-manager talks to (io.systemd.StorageProvider today, io.systemd.Metrics
// later). It deliberately implements only what those need; there is no code
// generation and no third-party dependency beyond golang.org/x/sys/unix.
//
// # Protocol
//
// Varlink is JSON over a stream socket, here AF_UNIX. Every message is one
// JSON object terminated by a NUL byte. A request carries "method" and
// optional "parameters"; the flags "more" (expect a stream of replies) and
// "oneway" (expect no reply) are set per call. A reply carries either
// "parameters" plus "continues" (true while a "more" stream is still
// running) or "error" (a qualified error name such as
// io.systemd.StorageProvider.NoSuchVolume) with optional parameters.
// Error replies surface as *Error so callers can switch on Name.
//
// # File descriptors
//
// systemd services return open file descriptors by sending them as
// SCM_RIGHTS ancillary data on the same sendmsg(2) as the reply; the reply's
// JSON refers to them by index (StorageProvider.Acquire returns
// fileDescriptorIndex). Conn reads with recvmsg(2), collects any rights
// received before a message completed and hands them over as []*os.File on
// that message. The caller owns the files.
//
// # Connections
//
// A Conn is a single connection that serialises calls; it is safe for
// concurrent use, one call at a time. Transport and protocol failures, and
// a "more" stream abandoned before its last reply, leave the wire in an
// undefined state, so the connection is closed and every later call fails.
// Connections are cheap: callers open one per operation.
package varlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// maxMessageSize bounds a single message, matching systemd's own limit.
	maxMessageSize = 16 << 20
	// maxFDs is SCM_MAX_FD, the most file descriptors one message can carry.
	maxFDs = 253
	// readChunk is the buffer size of one recvmsg(2).
	readChunk = 64 << 10

	methodGetInfo              = "org.varlink.service.GetInfo"
	methodGetInterfaceDescript = "org.varlink.service.GetInterfaceDescription"
)

// ErrClosed is returned by calls on a connection that was closed, or that
// broke after a transport or protocol failure.
var ErrClosed = errors.New("varlink: connection closed")

// Error is a Varlink error reply. Name is the qualified error name, for
// example "io.systemd.StorageProvider.NoSuchVolume"; Parameters is the raw
// JSON object the service attached, if any.
type Error struct {
	Name       string
	Parameters json.RawMessage
}

func (e *Error) Error() string {
	if len(e.Parameters) > 0 && !bytes.Equal(e.Parameters, []byte("{}")) {
		return fmt.Sprintf("varlink: %s %s", e.Name, e.Parameters)
	}
	return "varlink: " + e.Name
}

// ServiceInfo is the reply of org.varlink.service.GetInfo.
type ServiceInfo struct {
	Vendor     string   `json:"vendor"`
	Product    string   `json:"product"`
	Version    string   `json:"version"`
	URL        string   `json:"url"`
	Interfaces []string `json:"interfaces"`
}

type request struct {
	Method     string `json:"method"`
	Parameters any    `json:"parameters"`
	More       bool   `json:"more,omitempty"`
	Oneway     bool   `json:"oneway,omitempty"`
}

type reply struct {
	Parameters json.RawMessage `json:"parameters"`
	Continues  bool            `json:"continues"`
	Error      string          `json:"error"`
}

// callbackError wraps an error returned by a CallMore callback on the final
// reply, where the wire is still consistent and the connection stays usable.
type callbackError struct{ err error }

func (e *callbackError) Error() string { return e.err.Error() }
func (e *callbackError) Unwrap() error { return e.err }

// Conn is one Varlink connection.
type Conn struct {
	mu   sync.Mutex
	conn *net.UnixConn
	err  error // set once the connection is closed or broken

	rbuf   []byte        // received bytes not yet consumed
	pos    int64         // stream offset of rbuf[0]
	oob    []byte        // ancillary data buffer, reused across reads
	rights []rightsBatch // rights received but not yet attached to a message
}

// rightsBatch is one SCM_RIGHTS payload together with the stream offset of
// the first data byte it arrived with. Services send a message and its file
// descriptors in one sendmsg(2), and the kernel never merges bytes carrying
// different ancillary data into one recvmsg(2), so that offset is the first
// byte of the message the descriptors belong to.
type rightsBatch struct {
	at  int64
	fds []int
}

// Dial connects to the Varlink service listening on the AF_UNIX socket at
// path.
func Dial(ctx context.Context, path string) (*Conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("varlink: dial %s: %w", path, err)
	}
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		_ = nc.Close()
		return nil, fmt.Errorf("varlink: dial %s: not a unix connection (%T)", path, nc)
	}
	return &Conn{conn: uc, oob: make([]byte, unix.CmsgSpace(maxFDs*4))}, nil
}

// Close closes the connection and any received file descriptors that were
// not attached to a message. It is safe to call more than once.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shutdown(ErrClosed)
}

// shutdown closes the socket once and records why; the caller holds c.mu.
func (c *Conn) shutdown(cause error) error {
	if c.err != nil {
		return nil
	}
	c.err = cause
	for _, b := range c.rights {
		for _, fd := range b.fds {
			_ = unix.Close(fd)
		}
	}
	c.rights = nil
	return c.conn.Close()
}

// Call invokes method with params and decodes the reply parameters into out
// (nil to discard them). A Varlink error reply is returned as *Error. Any
// file descriptors on the reply are closed; use CallWithFiles to keep them.
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	files, err := c.CallWithFiles(ctx, method, params, out)
	closeFiles(files)
	return err
}

// CallWithFiles is Call for methods whose reply carries file descriptors.
// The returned files, in the order the service sent them, belong to the
// caller; the reply parameters index into them.
func (c *Conn) CallWithFiles(ctx context.Context, method string, params, out any) ([]*os.File, error) {
	var (
		raw   json.RawMessage
		files []*os.File
	)
	err := c.do(ctx, func() error {
		if err := c.send(request{Method: method, Parameters: orEmpty(params)}); err != nil {
			return err
		}
		r, f, err := c.receive()
		if err != nil {
			return err
		}
		if r.Error != "" {
			closeFiles(f)
			return &Error{Name: r.Error, Parameters: r.Parameters}
		}
		if r.Continues {
			closeFiles(f)
			return fmt.Errorf("varlink: %s: reply to a single call has continues=true", method)
		}
		raw, files = r.Parameters, f
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := decode(raw, out); err != nil {
		closeFiles(files)
		return nil, fmt.Errorf("varlink: %s: decode reply: %w", method, err)
	}
	return files, nil
}

// CallMore invokes method with the "more" flag and calls fn with the
// parameters of every reply until the service sends the last one. An error
// from fn stops the stream and is returned; if replies were still pending
// the connection is closed. Files on stream replies are closed.
func (c *Conn) CallMore(ctx context.Context, method string, params any, fn func(json.RawMessage) error) error {
	return c.CallMoreWithFiles(ctx, method, params, func(raw json.RawMessage, files []*os.File) error {
		closeFiles(files)
		return fn(raw)
	})
}

// CallMoreWithFiles is CallMore for streams whose replies carry file
// descriptors: fn receives the files sent with each reply and owns them.
func (c *Conn) CallMoreWithFiles(ctx context.Context, method string, params any, fn func(json.RawMessage, []*os.File) error) error {
	return c.do(ctx, func() error {
		if err := c.send(request{Method: method, Parameters: orEmpty(params), More: true}); err != nil {
			return err
		}
		for {
			r, files, err := c.receive()
			if err != nil {
				closeFiles(files)
				return err
			}
			if r.Error != "" {
				closeFiles(files)
				return &Error{Name: r.Error, Parameters: r.Parameters}
			}
			if err := fn(r.Parameters, files); err != nil {
				if r.Continues {
					return err
				}
				return &callbackError{err: err}
			}
			if !r.Continues {
				return nil
			}
		}
	})
}

// Oneway invokes method with the "oneway" flag; the service sends no reply.
func (c *Conn) Oneway(ctx context.Context, method string, params any) error {
	return c.do(ctx, func() error {
		return c.send(request{Method: method, Parameters: orEmpty(params), Oneway: true})
	})
}

// Info calls org.varlink.service.GetInfo.
func (c *Conn) Info(ctx context.Context) (*ServiceInfo, error) {
	var info ServiceInfo
	if err := c.Call(ctx, methodGetInfo, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// InterfaceDescription calls org.varlink.service.GetInterfaceDescription
// and returns the interface definition in Varlink IDL.
func (c *Conn) InterfaceDescription(ctx context.Context, iface string) (string, error) {
	var out struct {
		Description string `json:"description"`
	}
	params := map[string]string{"interface": iface}
	if err := c.Call(ctx, methodGetInterfaceDescript, params, &out); err != nil {
		return "", err
	}
	return out.Description, nil
}

// do runs one exchange under the lock with ctx applied as socket deadline.
// Errors other than Varlink error replies and final-reply callback errors
// break the connection.
func (c *Conn) do(ctx context.Context, fn func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	deadline, _ := ctx.Deadline()
	_ = c.conn.SetDeadline(deadline)
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		_ = c.conn.SetDeadline(time.Unix(1, 0))
	})

	err := fn()

	if !stop() {
		<-interrupted
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		// The only deadline ever set on the socket mirrors the context's, and
		// the poller can fire a hair before the context's own timer. Wait for
		// the context so the caller sees its error, not an I/O timeout.
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	var (
		verr *Error
		cerr *callbackError
	)
	switch {
	case errors.As(err, &verr):
		return err
	case errors.As(err, &cerr):
		return cerr.err
	}
	_ = c.shutdown(fmt.Errorf("%w: %w", ErrClosed, err))
	return err
}

func (c *Conn) send(req request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("varlink: encode %s: %w", req.Method, err)
	}
	b = append(b, 0)
	if _, err := c.conn.Write(b); err != nil {
		return fmt.Errorf("varlink: send %s: %w", req.Method, err)
	}
	return nil
}

// receive reads the next NUL-terminated message together with the file
// descriptors received while waiting for it.
func (c *Conn) receive() (*reply, []*os.File, error) {
	for {
		if i := bytes.IndexByte(c.rbuf, 0); i >= 0 {
			msg := c.rbuf[:i]
			var r reply
			if err := json.Unmarshal(msg, &r); err != nil {
				return nil, nil, fmt.Errorf("varlink: malformed reply: %w", err)
			}
			end := c.pos + int64(i)
			c.rbuf = append(c.rbuf[:0], c.rbuf[i+1:]...)
			c.pos = end + 1
			return &r, c.takeFiles(end), nil
		}
		if len(c.rbuf) > maxMessageSize {
			return nil, nil, fmt.Errorf("varlink: reply exceeds %d bytes", maxMessageSize)
		}
		if err := c.read(); err != nil {
			return nil, nil, err
		}
	}
}

// read performs one recvmsg(2), appending data to rbuf and rights to fds.
func (c *Conn) read() error {
	buf := make([]byte, readChunk)
	at := c.pos + int64(len(c.rbuf))
	n, oobn, flags, _, err := c.conn.ReadMsgUnix(buf, c.oob)
	if oobn > 0 {
		if cerr := c.collectRights(c.oob[:oobn], at); cerr != nil {
			return cerr
		}
	}
	if flags&unix.MSG_CTRUNC != 0 {
		return errors.New("varlink: ancillary data truncated, file descriptors lost")
	}
	if n > 0 {
		c.rbuf = append(c.rbuf, buf[:n]...)
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("varlink: %w", io.ErrUnexpectedEOF)
		}
		return fmt.Errorf("varlink: receive: %w", err)
	}
	return nil
}

func (c *Conn) collectRights(oob []byte, at int64) error {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return fmt.Errorf("varlink: parse ancillary data: %w", err)
	}
	for i := range msgs {
		if msgs[i].Header.Level != unix.SOL_SOCKET || msgs[i].Header.Type != unix.SCM_RIGHTS {
			continue
		}
		fds, err := unix.ParseUnixRights(&msgs[i])
		if err != nil {
			return fmt.Errorf("varlink: parse SCM_RIGHTS: %w", err)
		}
		c.rights = append(c.rights, rightsBatch{at: at, fds: fds})
	}
	return nil
}

// takeFiles wraps the descriptors that arrived at or before stream offset
// end (the NUL of the message just consumed) as *os.File and keeps later
// batches for the messages they belong to. ReadMsgUnix receives with
// MSG_CMSG_CLOEXEC, so the fds are already close-on-exec.
func (c *Conn) takeFiles(end int64) []*os.File {
	var files []*os.File
	kept := c.rights[:0]
	for _, b := range c.rights {
		if b.at > end {
			kept = append(kept, b)
			continue
		}
		for _, fd := range b.fds {
			if fd < 0 {
				continue
			}
			files = append(files, os.NewFile(uintptr(fd), "varlink-fd")) // #nosec G115 -- fd is a non-negative kernel-supplied descriptor
		}
	}
	c.rights = kept
	return files
}

func decode(raw json.RawMessage, out any) error {
	if out == nil || len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func orEmpty(params any) any {
	if params == nil {
		return struct{}{}
	}
	return params
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}
