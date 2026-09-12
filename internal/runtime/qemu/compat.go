package qemu

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// QEMU 9.2 renamed the -netdev stream/dgram re-dial option from
// reconnect=<seconds> to reconnect-ms=<milliseconds> (the old spelling is
// deprecated there and removed later). Spec.Netdev backends are written for
// current QEMU; on an older binary, such as Ubuntu 24.04's 8.2, the Runtime
// rewrites the option before launching. Nothing else on the command line
// depends on the release.

// Version is a QEMU release, major and minor.
type Version struct {
	Major int
	Minor int
}

// reconnectMSSince is the first release that knows reconnect-ms=.
var reconnectMSSince = Version{Major: 9, Minor: 2}

// versionProbeTimeout bounds `qemu-system-x86_64 --version`.
const versionProbeTimeout = 5 * time.Second

var (
	// versionPattern matches the first line of --version:
	// "QEMU emulator version 8.2.2 (Debian 1:8.2.2+ds-0ubuntu1.4)".
	versionPattern = regexp.MustCompile(`QEMU emulator version (\d+)\.(\d+)`)
	// reconnectMSPattern matches the option inside a -netdev value.
	reconnectMSPattern = regexp.MustCompile(`(^|,)reconnect-ms=(\d+)(,|$)`)
)

// String formats the release as major.minor.
func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// Less reports whether v is an older release than o.
func (v Version) Less(o Version) bool {
	return v.Major < o.Major || (v.Major == o.Major && v.Minor < o.Minor)
}

// ParseVersion reads the release from the output of `qemu --version`.
func ParseVersion(out string) (Version, error) {
	m := versionPattern.FindStringSubmatch(out)
	if m == nil {
		return Version{}, fmt.Errorf("qemu: no version in %q", strings.TrimSpace(firstLine(out)))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return Version{Major: major, Minor: minor}, nil
}

// probeVersion runs the binary once with --version.
func probeVersion(ctx context.Context, binary string) (Version, error) {
	ctx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").Output() // #nosec G204 -- the configured QEMU binary
	if err != nil {
		return Version{}, fmt.Errorf("qemu --version: %w", err)
	}
	return ParseVersion(string(out))
}

// compatArgs adapts the arguments Command rendered to the release that will
// run them: before reconnectMSSince, reconnect-ms=<ms> in -netdev values
// becomes reconnect=<s>, rounded up to at least one second (reconnect=0
// would mean "do not reconnect"). Other releases get args back unchanged.
func compatArgs(args []string, v Version) []string {
	if !v.Less(reconnectMSSince) {
		return args
	}
	out := slices.Clone(args)
	for i := 0; i+1 < len(out); i++ {
		if out[i] != "-netdev" {
			continue
		}
		out[i+1] = reconnectMSPattern.ReplaceAllStringFunc(out[i+1], func(opt string) string {
			m := reconnectMSPattern.FindStringSubmatch(opt)
			ms, _ := strconv.Atoi(m[2])
			return m[1] + "reconnect=" + strconv.Itoa(max(1, (ms+999)/1000)) + m[3]
		})
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
