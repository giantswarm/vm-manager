//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/mdlayher/vsock"
)

// Credentials that make the guest's journal reach the host
// (systemd.system-credentials(7)): journald forwards every entry to the
// address in journal.forward_to_socket, and systemd-debug-generator adds a
// systemd.unit-dropin.<unit> credential as a drop-in of <unit>, in the
// initrd as in the installed system.
//
//nolint:gosec // G101: credential names, not secrets
const (
	credentialJournalForward = "journal.forward_to_socket"
	credentialUnitDropin     = "systemd.unit-dropin."
)

// journalDebugUnits log at debug level in every guest the harness boots:
// systemd-networkd and systemd-networkd-wait-online say nothing at the
// default level about why a link is not online, which is what a boot that
// waits in the initrd for network-online.target needs explained.
var journalDebugUnits = []string{
	"systemd-networkd.service",
	"systemd-networkd-wait-online.service",
}

// journalDebugDropin is the drop-in journalDebugUnits get.
const journalDebugDropin = "[Service]\nEnvironment=SYSTEMD_LOG_LEVEL=debug\n"

// journalMaxField bounds one binary field of the export format; journald's
// own limit for an entry is far below it.
const journalMaxField = 64 << 20

// GuestJournal receives what the guest's systemd-journald forwards
// (ForwardToSocket=, journald.conf(5)) over AF_VSOCK in the Journal Export
// Format and writes it as text, like journalctl -o short-monotonic, to the
// file of the phase that is booting: <name>-journal.log next to the serial
// console <name>.log. journald runs from the first second of the initrd,
// where ssh is not up yet, so a boot that never gets as far as READY=1
// still leaves its journal behind.
type GuestJournal struct {
	ln  net.Listener
	cid uint32
	log *slog.Logger

	mu    sync.Mutex
	path  string
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// listenJournal binds a vsock stream listener on every host context id for
// the guest with context id cid; connections from other guests are refused.
func listenJournal(cid uint32, log *slog.Logger) (*GuestJournal, error) {
	ln, err := vsock.ListenContextID(math.MaxUint32, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	j := &GuestJournal{ln: ln, cid: cid, log: log, conns: map[net.Conn]struct{}{}}
	j.wg.Add(1)
	go j.accept()
	return j, nil
}

// Credential is the journal.forward_to_socket value: the listener's port on
// the host's context id.
func (j *GuestJournal) Credential() string {
	return fmt.Sprintf("vsock:%d:%d", vsock.Host, j.ln.Addr().(*vsock.Addr).Port)
}

// SetPath directs the connections the guest opens from now on to path; each
// QEMU start runs a new journald, which connects anew.
func (j *GuestJournal) SetPath(path string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.path = path
}

// Path is the file the current phase's journal goes to.
func (j *GuestJournal) Path() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.path
}

// Close stops accepting, ends open connections and waits for their entries
// to be written.
func (j *GuestJournal) Close() error {
	err := j.ln.Close()
	j.mu.Lock()
	for c := range j.conns {
		_ = c.Close()
	}
	j.mu.Unlock()
	j.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (j *GuestJournal) accept() {
	defer j.wg.Done()
	for {
		conn, err := j.ln.Accept()
		if err != nil {
			return
		}
		if peer, ok := conn.RemoteAddr().(*vsock.Addr); !ok || peer.ContextID != j.cid {
			j.log.Warn("journal connection from another guest refused", "remote", conn.RemoteAddr())
			_ = conn.Close()
			continue
		}
		j.mu.Lock()
		j.conns[conn] = struct{}{}
		path := j.path
		j.mu.Unlock()
		j.wg.Add(1)
		go j.handle(conn, path)
	}
}

// handle appends the entries of one journald connection to path.
func (j *GuestJournal) handle(conn net.Conn, path string) {
	defer j.wg.Done()
	defer func() {
		j.mu.Lock()
		delete(j.conns, conn)
		j.mu.Unlock()
		_ = conn.Close()
	}()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) // #nosec G304 -- a file in the harness's state dir
	if err != nil {
		j.log.Warn("guest journal file", "error", err)
		return
	}
	defer func() { _ = f.Close() }()
	if err := copyJournal(f, conn); err != nil && !errors.Is(err, net.ErrClosed) {
		_, _ = fmt.Fprintf(f, "-- journal stream ended: %v --\n", err)
	}
}

// copyJournal renders the export format stream r to w until r ends. Output
// is flushed whenever the stream has nothing more buffered, so the file is
// current while the guest runs.
func copyJournal(w io.Writer, r io.Reader) error {
	br := bufio.NewReader(r)
	bw := bufio.NewWriter(w)
	defer func() { _ = bw.Flush() }()
	var boot string
	for {
		entry, err := readExportEntry(br)
		if len(entry) > 0 {
			if id := entry["_BOOT_ID"]; id != "" && id != boot {
				boot = id
				_, _ = fmt.Fprintf(bw, "-- Boot %s --\n", id)
			}
			_, _ = bw.WriteString(formatEntry(entry))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if br.Buffered() == 0 {
			if err := bw.Flush(); err != nil {
				return err
			}
		}
	}
}

// readExportEntry reads one entry of the Journal Export Format
// (https://systemd.io/JOURNAL_EXPORT_FORMATS/): FIELD=value lines, or a
// field name line followed by a little-endian 64-bit length, that many
// bytes and a newline for values that are not plain text, up to an empty
// line. It returns the fields read so far with io.EOF when the stream ends.
func readExportEntry(r *bufio.Reader) (map[string]string, error) {
	entry := map[string]string{}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return entry, err
			}
			if len(line) == 0 {
				return entry, io.EOF
			}
			// The guest died mid-entry: keep what arrived of the field.
			if name, value, ok := bytes.Cut(line, []byte("=")); ok {
				entry[string(name)] = string(value)
			}
			return entry, io.ErrUnexpectedEOF
		}
		line = line[:len(line)-1]
		if len(line) == 0 {
			if len(entry) == 0 {
				continue
			}
			return entry, nil
		}
		if name, value, ok := bytes.Cut(line, []byte("=")); ok {
			entry[string(name)] = string(value)
			continue
		}
		var size uint64
		if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
			return entry, fmt.Errorf("field %s: length: %w", line, err)
		}
		if size > journalMaxField {
			return entry, fmt.Errorf("field %s: %d bytes", line, size)
		}
		value := make([]byte, size+1)
		if _, err := io.ReadFull(r, value); err != nil {
			return entry, fmt.Errorf("field %s: %w", line, err)
		}
		if value[size] != '\n' {
			return entry, fmt.Errorf("field %s: no newline after %d bytes", line, size)
		}
		entry[string(line)] = string(value[:size])
	}
}

// formatEntry is one entry as journalctl -o short-monotonic prints it,
// without the hostname: [seconds since boot] identifier[pid]: message, with
// the continuation lines of a multi-line message indented.
func formatEntry(e map[string]string) string {
	ts := "[    ?.??????]"
	if us, err := strconv.ParseUint(e["__MONOTONIC_TIMESTAMP"], 10, 64); err == nil {
		ts = fmt.Sprintf("[%5d.%06d]", us/1_000_000, us%1_000_000)
	}
	ident := firstNonEmpty(e["SYSLOG_IDENTIFIER"], e["_COMM"], "?")
	if pid := firstNonEmpty(e["SYSLOG_PID"], e["_PID"]); pid != "" {
		ident += "[" + pid + "]"
	}
	msg := strings.ReplaceAll(strings.TrimRight(e["MESSAGE"], "\n"), "\n", "\n    ")
	return ts + " " + ident + ": " + msg + "\n"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
