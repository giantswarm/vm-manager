package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Defaults of the /proc conversions on Linux.
const (
	DefaultProcRoot   = "/proc"
	DefaultClockTicks = 100
)

// procStats is what the host collector reads about a QEMU process.
type procStats struct {
	CPUSeconds float64
	RSSBytes   int64
}

// procReader reads /proc/<pid>/stat and statm below an injectable root.
type procReader struct {
	root     string
	ticks    float64
	pageSize int64
}

// read returns the process's CPU time and resident set; an error means
// the process is gone or the tree is not procfs.
func (p procReader) read(pid int) (procStats, error) {
	dir := filepath.Join(p.root, strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat")) // #nosec G304 -- procfs below the configured root.
	if err != nil {
		return procStats{}, err
	}
	// The command name in parentheses may contain spaces; the fixed-format
	// fields start after the last ')': state, ppid, ..., utime (14th field
	// of the line) and stime (15th).
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return procStats{}, fmt.Errorf("%s/stat: no command name", dir)
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 13 {
		return procStats{}, fmt.Errorf("%s/stat: %d fields after the command name", dir, len(fields))
	}
	utime, err1 := strconv.ParseFloat(fields[11], 64)
	stime, err2 := strconv.ParseFloat(fields[12], 64)
	if err1 != nil || err2 != nil {
		return procStats{}, fmt.Errorf("%s/stat: utime %q stime %q", dir, fields[11], fields[12])
	}
	statm, err := os.ReadFile(filepath.Join(dir, "statm")) // #nosec G304 -- procfs below the configured root.
	if err != nil {
		return procStats{}, err
	}
	mf := strings.Fields(string(statm))
	if len(mf) < 2 {
		return procStats{}, fmt.Errorf("%s/statm: %d fields", dir, len(mf))
	}
	resident, err := strconv.ParseInt(mf[1], 10, 64)
	if err != nil {
		return procStats{}, fmt.Errorf("%s/statm: resident %q", dir, mf[1])
	}
	return procStats{CPUSeconds: (utime + stime) / p.ticks, RSSBytes: resident * p.pageSize}, nil
}
