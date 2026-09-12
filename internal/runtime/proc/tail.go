package proc

import (
	"io"
	"os"
	"strings"
	"sync"
)

// Tail is an io.Writer that keeps the last Limit bytes written to it. The
// runtime hands it to a process as stderr so an early exit can be reported
// with the reason the process printed, without keeping unbounded output.
type Tail struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

// DefaultTailBytes is the Tail size the runtime packages use for stderr.
const DefaultTailBytes = 8 * 1024

// NewTail keeps the last limit bytes; limit <= 0 uses DefaultTailBytes.
func NewTail(limit int) *Tail {
	if limit <= 0 {
		limit = DefaultTailBytes
	}
	return &Tail{limit: limit}
}

// Write implements io.Writer; it never fails.
func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= t.limit {
		t.buf = append(t.buf[:0], p[len(p)-t.limit:]...)
		return len(p), nil
	}
	if over := len(t.buf) + len(p) - t.limit; over > 0 {
		t.buf = t.buf[over:]
	}
	t.buf = append(t.buf, p...)
	return len(p), nil
}

// String is the retained output, trimmed of surrounding whitespace.
func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// TailFile is the last limit bytes of the file at path (a Cmd.Log), trimmed
// like Tail.String; limit <= 0 uses DefaultTailBytes. A missing or
// unreadable file yields "".
func TailFile(path string, limit int) string {
	if limit <= 0 {
		limit = DefaultTailBytes
	}
	f, err := os.Open(path) // #nosec G304 -- the caller's own log path
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := max(st.Size()-int64(limit), 0)
	data, err := io.ReadAll(io.NewSectionReader(f, offset, st.Size()-offset))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
