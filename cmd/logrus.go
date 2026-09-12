package cmd

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// gvisor-tap-vsock, the user-space network stack behind internal/network,
// logs through logrus' global logger. bridgeLogrus forwards those entries to
// slog at the matching level, so one handler and one --verbose flag govern
// all output. Its per-connection teardown errors (a guest or a client closed
// a socket) are demoted to debug: they are routine, not failures.

var bridgeOnce sync.Once

func bridgeLogrus(log *slog.Logger) {
	bridgeOnce.Do(func() {
		logrus.SetOutput(io.Discard)
		// Forward everything; the slog handler applies the configured level.
		logrus.SetLevel(logrus.TraceLevel)
		logrus.AddHook(logrusHook{log: log.With("component", "gvproxy")})
	})
}

type logrusHook struct {
	log *slog.Logger
}

func (logrusHook) Levels() []logrus.Level { return logrus.AllLevels }

func (h logrusHook) Fire(e *logrus.Entry) error {
	ctx := e.Context
	if ctx == nil {
		ctx = context.Background()
	}
	attrs := make([]any, 0, 2*len(e.Data))
	for k, v := range e.Data {
		attrs = append(attrs, k, v)
	}
	h.log.Log(ctx, slogLevel(e), e.Message, attrs...)
	return nil
}

// connectionNoise are the messages gvisor emits when one connection ends.
var connectionNoise = []string{
	"use of closed network connection",
	"connection reset by peer",
	"broken pipe",
	"endpoint is closed for send",
	"connection was aborted",
}

func slogLevel(e *logrus.Entry) slog.Level {
	for _, noise := range connectionNoise {
		if strings.Contains(e.Message, noise) {
			return slog.LevelDebug
		}
	}
	switch e.Level {
	case logrus.PanicLevel, logrus.FatalLevel, logrus.ErrorLevel:
		return slog.LevelError
	case logrus.WarnLevel:
		return slog.LevelWarn
	case logrus.InfoLevel:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}
