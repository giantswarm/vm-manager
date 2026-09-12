package quote

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/imds"
)

// startSwtpm runs the vTPM the guests really get (swtpm --tpm2) on a unix
// data socket below dir, keeping its state in dir so a restart is a reboot
// of the same TPM. It skips the test when swtpm is not installed.
func startSwtpm(t *testing.T, dir string) (sock string, stop func()) {
	t.Helper()
	bin, err := exec.LookPath("swtpm")
	if err != nil {
		t.Skip("swtpm not installed")
	}
	sock = filepath.Join(dir, "tpm.sock")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "socket", "--tpm2", // #nosec G204 -- constant arguments below a test directory
		"--tpmstate", "dir="+dir,
		"--server", "type=unixio,path="+sock,
		"--ctrl", "type=unixio,path="+filepath.Join(dir, "ctrl.sock"),
		"--flags", "not-need-init,startup-clear")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	stop = func() {
		cancel()
		_ = cmd.Wait()
	}
	t.Cleanup(stop)
	require.Eventually(t, func() bool {
		fi, err := os.Stat(sock)
		return err == nil && fi.Mode()&os.ModeSocket != 0
	}, 5*time.Second, 20*time.Millisecond, "swtpm socket did not appear")
	return sock, stop
}

// TestBuildAgainstSwtpm is the same protocol as the simulator tests, but
// over the socket transport and the real swtpm, including a restart of the
// process to show the AK survives a reboot of the vTPM.
func TestBuildAgainstSwtpm(t *testing.T) {
	// Unix socket paths are length-limited; keep the directory short.
	dir, err := os.MkdirTemp("", "swtpm")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock, stop := startSwtpm(t, dir)
	// Point the logs at a readable file: with a socket-backed TPM they must
	// be left out anyway, as the host's logs describe another TPM.
	hostLog := filepath.Join(dir, "host.log")
	require.NoError(t, os.WriteFile(hostLog, []byte("not this tpm"), 0o600))
	opts := func(sock string) Options {
		return Options{Device: sock, FirmwareLog: hostLog, UserspaceLog: hostLog}
	}

	first, err := Build(opts(sock), imds.StageInitrd, nonce)
	require.NoError(t, err)
	want, _ := Selection(imds.StageInitrd)
	verifyQuote(t, first, want)
	assert.Nil(t, first.EventLog, "host logs are not attached for a socket-backed tpm")
	assert.Nil(t, first.UserspaceLog)

	stop()
	require.NoError(t, os.Remove(sock))
	sock, _ = startSwtpm(t, dir)

	second, err := Build(opts(sock), imds.StageReady, nonce)
	require.NoError(t, err)
	want, _ = Selection(imds.StageReady)
	verifyQuote(t, second, want)
	assert.Equal(t, first.AKPub, second.AKPub, "the persistent AK survives a vTPM restart")
	assert.Equal(t, first.EKPub, second.EKPub)
}
