package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Event names the runtime reacts to. SHUTDOWN carries data.guest (true when
// the guest asked for it) and data.reason (guest-shutdown, guest-reset, ...).
const (
	EventShutdown  = "SHUTDOWN"
	EventReset     = "RESET"
	EventStop      = "STOP"
	EventPowerdown = "POWERDOWN"
)

// eventBuffer is how many events queue before new ones are dropped.
const eventBuffer = 64

// QMP is a minimal client for QEMU's machine protocol over the unix socket
// named by Spec.QMPSocket: capability negotiation, commands with JSON
// arguments, and the asynchronous events. QEMU serves one QMP client per
// -qmp socket, so an Instance owns exactly one QMP and hands it out.
type QMP struct {
	conn    net.Conn
	log     *slog.Logger
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[uint64]chan qmpResponse
	nextID  uint64
	events  chan Event
	dropped atomic.Uint64
	done    chan struct{}
	err     error
	closer  sync.Once
}

// Event is an asynchronous QMP message.
type Event struct {
	Name      string          `json:"event"`
	Data      json.RawMessage `json:"data,omitempty"`
	Timestamp Timestamp       `json:"timestamp"`
}

// Timestamp is when QEMU emitted an event.
type Timestamp struct {
	Seconds      int64 `json:"seconds"`
	Microseconds int64 `json:"microseconds"`
}

// Status is the query-status result.
type Status struct {
	// Status is the run state name: running, paused, shutdown, ...
	Status string `json:"status"`
	// Running is true while vCPUs execute.
	Running bool `json:"running"`
}

// Error is what QEMU returns for a failed command.
type Error struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *Error) Error() string { return e.Class + ": " + e.Desc }

// qmpMessage is the union of everything the server sends.
type qmpMessage struct {
	QMP       json.RawMessage `json:"QMP"`
	Event     string          `json:"event"`
	Data      json.RawMessage `json:"data"`
	Timestamp Timestamp       `json:"timestamp"`
	Return    json.RawMessage `json:"return"`
	Error     *Error          `json:"error"`
	ID        *uint64         `json:"id"`
}

type qmpResponse struct {
	ret json.RawMessage
	err *Error
}

type qmpRequest struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
	ID        uint64 `json:"id"`
}

// DialQMP connects to the QMP socket at path, reads the greeting and
// negotiates capabilities. ctx bounds the handshake.
func DialQMP(ctx context.Context, path string) (*QMP, error) {
	return dialQMP(ctx, path, slog.Default())
}

func dialQMP(ctx context.Context, path string, log *slog.Logger) (*QMP, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	q := &QMP{
		conn:    conn,
		log:     log,
		pending: map[uint64]chan qmpResponse{},
		events:  make(chan Event, eventBuffer),
		done:    make(chan struct{}),
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
	}
	dec := json.NewDecoder(conn)
	var greeting qmpMessage
	if err := dec.Decode(&greeting); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("qmp greeting: %w", err)
	}
	if greeting.QMP == nil {
		_ = conn.Close()
		return nil, errors.New("qmp greeting: not a QMP server")
	}
	_ = conn.SetReadDeadline(time.Time{})
	go q.read(dec)
	if err := q.Execute(ctx, "qmp_capabilities", nil, nil); err != nil {
		_ = q.Close()
		return nil, err
	}
	return q, nil
}

// read dispatches server messages until the connection ends, then wakes
// every waiter and closes the event channel.
func (q *QMP) read(dec *json.Decoder) {
	var err error
	for {
		var m qmpMessage
		if err = dec.Decode(&m); err != nil {
			break
		}
		switch {
		case m.Event != "":
			q.deliver(Event{Name: m.Event, Data: m.Data, Timestamp: m.Timestamp})
		case m.ID != nil:
			q.mu.Lock()
			ch := q.pending[*m.ID]
			delete(q.pending, *m.ID)
			q.mu.Unlock()
			if ch != nil {
				ch <- qmpResponse{ret: m.Return, err: m.Error}
			}
		default:
			q.log.Debug("qmp: message without id or event ignored")
		}
	}
	q.err = err
	close(q.done)
	close(q.events)
}

func (q *QMP) deliver(ev Event) {
	select {
	case q.events <- ev:
	default:
		q.dropped.Add(1)
		q.log.Warn("qmp: event dropped, consumer too slow", "event", ev.Name)
	}
}

// Execute runs command with args (marshalled as the arguments object, nil
// for none) and decodes the return value into result when it is not nil.
func (q *QMP) Execute(ctx context.Context, command string, args, result any) error {
	select {
	case <-q.done:
		return fmt.Errorf("qmp %s: %w", command, q.closedErr())
	default:
	}
	q.mu.Lock()
	q.nextID++
	id := q.nextID
	ch := make(chan qmpResponse, 1)
	q.pending[id] = ch
	q.mu.Unlock()

	b, err := json.Marshal(qmpRequest{Execute: command, Arguments: args, ID: id})
	if err != nil {
		q.forget(id)
		return fmt.Errorf("qmp %s: encode: %w", command, err)
	}
	q.writeMu.Lock()
	dl, _ := ctx.Deadline()
	_ = q.conn.SetWriteDeadline(dl) // zero clears a deadline left by an earlier command
	_, err = q.conn.Write(b)
	q.writeMu.Unlock()
	if err != nil {
		q.forget(id)
		return fmt.Errorf("qmp %s: %w", command, err)
	}
	select {
	case resp := <-ch:
		if resp.err != nil {
			return fmt.Errorf("qmp %s: %w", command, resp.err)
		}
		if result != nil && len(resp.ret) > 0 {
			if err := json.Unmarshal(resp.ret, result); err != nil {
				return fmt.Errorf("qmp %s: decode: %w", command, err)
			}
		}
		return nil
	case <-ctx.Done():
		q.forget(id)
		return fmt.Errorf("qmp %s: %w", command, ctx.Err())
	case <-q.done:
		q.forget(id)
		return fmt.Errorf("qmp %s: %w", command, q.closedErr())
	}
}

func (q *QMP) forget(id uint64) {
	q.mu.Lock()
	delete(q.pending, id)
	q.mu.Unlock()
}

// closedErr is the reason the connection ended; call after done is closed.
func (q *QMP) closedErr() error {
	if q.err == nil {
		return net.ErrClosed
	}
	return fmt.Errorf("connection closed: %w", q.err)
}

// QueryStatus is query-status.
func (q *QMP) QueryStatus(ctx context.Context) (Status, error) {
	var st Status
	err := q.Execute(ctx, "query-status", nil, &st)
	return st, err
}

// SystemPowerdown presses the virtual power button; a guest with ACPI
// handling shuts down and QEMU emits SHUTDOWN.
func (q *QMP) SystemPowerdown(ctx context.Context) error {
	return q.Execute(ctx, "system_powerdown", nil, nil)
}

// Quit ends QEMU immediately, like SIGTERM but acknowledged.
func (q *QMP) Quit(ctx context.Context) error {
	return q.Execute(ctx, "quit", nil, nil)
}

// Events delivers asynchronous events; it is closed when the connection
// ends. A consumer that falls eventBuffer behind loses events (Dropped).
func (q *QMP) Events() <-chan Event { return q.events }

// Dropped is how many events were discarded because Events was not read.
func (q *QMP) Dropped() uint64 { return q.dropped.Load() }

// Done is closed when the connection has ended.
func (q *QMP) Done() <-chan struct{} { return q.done }

// Close ends the connection and waits for the reader to finish.
func (q *QMP) Close() error {
	var err error
	q.closer.Do(func() {
		err = q.conn.Close()
		<-q.done
	})
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
