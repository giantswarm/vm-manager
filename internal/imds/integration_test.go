//go:build integration

package imds

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The real client. It sets SO_MARK on its HTTP socket, which needs
// CAP_NET_ADMIN, and skips the loopback interface unless told otherwise, so
// the test re-executes itself in an unprivileged user+network namespace
// (unshare -rn) where it owns lo and the capability.
const (
	imdsdBinary = "/usr/lib/systemd/systemd-imdsd"
	childEnv    = "IMDS_INTEGRATION_CHILD"
)

// TestSystemdImdsd proves systemd-imdsd resolves keys, well-known keys and
// user-data from Handler exactly as the hwdb record configures it in the
// guest, and that a locked /user-data and an unknown key fail for it.
func TestSystemdImdsd(t *testing.T) {
	if _, err := os.Stat(imdsdBinary); err != nil {
		t.Skipf("%s not installed: %v", imdsdBinary, err)
	}
	if os.Getenv(childEnv) == "" {
		runInNamespace(t)
		return
	}

	addr := netip.MustParseAddr("127.0.0.1")
	resolver := &fakeResolver{instances: map[netip.Addr]Instance{addr: testInstance()}}
	srv := httptest.NewServer(Handler(Deps{
		Resolver:  resolver,
		Attestor:  &NoopAttestor{},
		Reports:   &fakeSink{},
		Artifacts: FSArtifacts{FS: fstest.MapFS{}},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	defer srv.Close()
	dataURL := srv.URL + BasePath

	out, stderr, err := imdsd(t, dataURL, "/hostname")
	require.NoError(t, err, stderr)
	assert.Equal(t, "worker-1.example", out)

	out, stderr, err = imdsd(t, dataURL, "--well-known-key=hostname:/hostname", "-K", "hostname")
	require.NoError(t, err, stderr)
	assert.Equal(t, "worker-1.example", out, "well-known hostname via the hwdb property path")

	out, stderr, err = imdsd(t, dataURL, "--well-known-key=ssh-key:/public-keys/0", "-K", "ssh-key")
	require.NoError(t, err, stderr)
	assert.Equal(t, "ssh-ed25519 AAAA first", out)

	out, stderr, err = imdsd(t, dataURL, "--well-known-key=userdata:/user-data", "-K", "userdata")
	require.Error(t, err, "user-data must stay locked before the initrd attestation")
	assert.NotContains(t, out, "ignition")
	t.Logf("locked user-data: %s", strings.TrimSpace(stderr))

	resolver.release(addr)
	out, stderr, err = imdsd(t, dataURL, "--well-known-key=userdata:/user-data", "-K", "userdata")
	require.NoError(t, err, stderr)
	assert.Equal(t, ignition, out, "released user-data is delivered verbatim")

	_, stderr, err = imdsd(t, dataURL, "/nope")
	require.Error(t, err, "unknown key must be KeyNotFound")
	t.Logf("unknown key: %s", strings.TrimSpace(stderr))
}

// imdsd runs the client with manual endpoint configuration, the way the hwdb
// record would configure it, and returns stdout and stderr.
func imdsd(t *testing.T, dataURL string, args ...string) (string, string, error) {
	t.Helper()
	base := []string{"-i", "lo", "--vendor=" + Vendor, "--data-url=" + dataURL, "--cache=no", "--wait=no"}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(context.Background(), imdsdBinary, append(base, args...)...) //nolint:gosec // fixed binary, test arguments
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// runInNamespace re-executes this test under unshare -rn with lo up and
// skips when the host does not allow unprivileged user namespaces.
func runInNamespace(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"unshare", "ip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH: %v", tool, err)
		}
	}
	exe, err := os.Executable()
	require.NoError(t, err)

	cmd := exec.CommandContext(context.Background(), "unshare", "-rn", "/bin/sh", "-c", //nolint:gosec // re-exec of this test binary
		`ip link set lo up && exec "$0" -test.run '^TestSystemdImdsd$' -test.v`, exe)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("child output:\n%s", out)
	if !bytes.Contains(out, []byte("=== RUN   TestSystemdImdsd")) {
		t.Skipf("cannot enter an unprivileged user+network namespace: %v", err)
	}
	require.NoError(t, err, "systemd-imdsd against Handler failed in the namespace")
}
