//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/attest"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// The test's network and VM; the CIDR stays clear of the other e2e tests and
// of the server's default network.
const (
	kubernetesNetwork = "e2e-kubernetes"
	kubernetesCIDR    = "192.168.142.0/24"
	kubernetesVMName  = "kubernetes-e2e"
)

// The guest side of the Kubernetes sysext (images/README.md, "Kubernetes
// sysext"): the unit that pulls, merges and measures it, where sysupdate
// puts the file, and where the TPM exposes PCR 13.
const (
	kubernetesUnit    = "vm-kubernetes.service"
	extensionsDir     = "/var/lib/extensions"
	containerdSocket  = "unix:///run/containerd/containerd.sock"
	pcr13Sysfs        = "/sys/class/tpm/tpm0/pcr-sha256/13"
	tpmUserspaceLog   = "/run/log/systemd/tpm2-measure.log"
	kubernetesTimeout = 15 * time.Minute
	// containerdWithin: the merge's daemon-reload makes multi-user.target's
	// Upholds= start containerd; READY=1 does not wait for that.
	containerdWithin = 60 * time.Second
)

// pulledLine is what /usr/lib/vm-manager/kubernetes logs after the download.
var pulledLine = regexp.MustCompile(`pulled (kubernetes_\S+\.raw) \((\d+) bytes\) in ([0-9.]+) s`)

// A VM created with the newest kubernetes_version of the image pulls that
// sysext from the IMDS at boot, merges exactly that one version, starts
// containerd (kubelet restarts until kubeadm configures it), and measures
// the sysext into PCR 13 so that the value equals what the host computes
// from the published artifact alone (policy.json pcr13.<version>, the
// attest.SysextPCR rule).
func TestKubernetesSysext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), kubernetesTimeout)
	defer cancel()
	requireHost(t)
	image := locateImage(t)
	dir := stateDir(t)
	_, testKey := generateSSHKey(t, dir)

	sums := readSHA256SUMS(t, filepath.Join(image.Dir, "sysupdate", "kubernetes", "SHA256SUMS"))
	policy := readPolicy(t, filepath.Join(image.Dir, "policy.json"))

	srv := startServer(ctx, t, dir, image.Dir)
	m := newMCPClient(ctx, t, srv.URL)

	var img images.Image
	m.call(ctx, api.ToolGetImage, map[string]any{"ref": imageID}, &img)
	require.NotEmpty(t, img.KubernetesVersions, "get_image lists no Kubernetes versions; run `make -C images kubernetes verify-kubernetes`")
	version := img.KubernetesVersions[0]
	file := "kubernetes_" + version + ".raw"
	require.Contains(t, sums, file, "SHA256SUMS of the kubernetes component")

	// The host side of the PCR 13 rule: verify-kubernetes (Python) and
	// attest.SysextPCR (Go) agree on the artifact.
	line := attest.SysextMeasurement(sums[file], file)
	wantPCR13 := attest.SysextPCR(line)
	require.Equal(t, wantPCR13, policy.PCR13[version], "policy.json pcr13[%s] disagrees with attest.SysextPCR(%q); run `make -C images verify-kubernetes`", version, line)
	t.Logf("kubernetes %s: measurement %q, expected pcr 13 %s", version, line, wantPCR13)

	m.call(ctx, api.ToolCreateNetwork, map[string]any{"name": kubernetesNetwork, "cidr": kubernetesCIDR}, nil)
	started := time.Now()
	var v vm.VM
	m.call(ctx, api.ToolCreateVM, map[string]any{
		"name":                kubernetesVMName,
		"hostname":            kubernetesVMName,
		"network":             kubernetesNetwork,
		"ssh_authorized_keys": []string{testKey},
		"kubernetes_version":  version,
		"require_attestation": false,
		"wait_for":            string(vm.WaitReady),
	}, &v)
	t.Logf("create_vm returned after %.1fs: id %s state %s ip %s", time.Since(started).Seconds(), v.ID, v.State, v.IP)
	require.Equal(t, vm.StateReady, v.State, "create_vm with wait_for ready: lastError %q\n%s", v.LastError, srv.logTail())
	assert.Equal(t, version, v.KubernetesVersion, "the record pins the version served as /kubernetes-version")
	require.NotNil(t, v.BootedAt)
	require.NotNil(t, v.ReadyAt)
	t.Logf("boot_to_ready_seconds=%.1f", v.ReadyAt.Sub(*v.BootedAt).Seconds())

	g := &guest{m: m, id: v.ID}
	g.waitSSH(ctx)
	assertKubernetesPulled(ctx, t, g, version, file, line)
	assertKubernetesRunning(ctx, t, g, version)
	assertPCR13(ctx, t, g, wantPCR13)

	var deleted api.DeletedResponse
	m.call(ctx, api.ToolDeleteVM, map[string]any{"id": v.ID}, &deleted)
	assert.True(t, deleted.Deleted)
	m.call(ctx, api.ToolDeleteNetwork, map[string]any{"name": kubernetesNetwork}, &deleted)
	assert.True(t, deleted.Deleted)
	srv.stop()
}

// assertKubernetesPulled checks the pull unit and the file lifecycle: the
// unit succeeded in this boot, downloaded the file, logged the measurement,
// and exactly one kubernetes extension is merged from exactly one file.
func assertKubernetesPulled(ctx context.Context, t *testing.T, g *guest, version, file, line string) {
	t.Helper()
	show := g.sh(ctx, "systemctl show -p ActiveState -p Result -p ExecMainStartTimestampMonotonic -p ExecMainExitTimestampMonotonic "+kubernetesUnit)
	assert.Contains(t, show, "ActiveState=active", kubernetesUnit)
	assert.Contains(t, show, "Result=success", kubernetesUnit)
	if secs, ok := unitRunSeconds(show); ok {
		t.Logf("kubernetes_unit_seconds=%.1f (%s from start to exit)", secs, kubernetesUnit)
	}

	journal := g.sh(ctx, "journalctl -b -o cat -u "+kubernetesUnit+" --no-pager")
	t.Logf("%s journal:\n%s", kubernetesUnit, journal)
	pulled := pulledLine.FindStringSubmatch(journal)
	if assert.NotNil(t, pulled, "the unit logs the download") {
		assert.Equal(t, file, pulled[1])
		t.Logf("kubernetes_pull_seconds=%s (%s bytes)", pulled[3], pulled[2])
	}
	assert.Contains(t, journal, "measured '"+line+"' into PCR 13", "the unit measures the SHA256SUMS line")
	assert.NotContains(t, journal, "removing superseded", "a fresh VM has nothing to supersede")

	status := g.sh(ctx, "systemd-sysext status --no-legend --no-pager")
	t.Logf("systemd-sysext status:\n%s", status)
	merged := ""
	for _, row := range strings.Split(status, "\n") {
		if fields := strings.Fields(row); len(fields) >= 2 && fields[0] == "/usr" {
			merged = fields[1]
		}
	}
	assert.Equal(t, "kubernetes_"+version, merged, "exactly this extension merged on /usr")
	assert.Equal(t, file, g.sh(ctx, "ls "+extensionsDir), "exactly one file in %s", extensionsDir)
	assert.Equal(t, "1", g.sh(ctx, "ls /usr/lib/extension-release.d/ | grep -c ^extension-release.kubernetes_"), "one extension-release")
}

// assertKubernetesRunning checks that the node stack of the extension is
// usable: the binaries report the version, containerd serves its socket,
// and nothing failed. kubelet is the documented exception: until kubeadm
// init/join writes its configuration it exits and Restart= brings it back
// every 10 s, which systemd reports as activating (auto-restart), not
// failed; the assertion below pins that distinction.
func assertKubernetesRunning(ctx context.Context, t *testing.T, g *guest, version string) {
	t.Helper()
	assert.Equal(t, "v"+version, g.sh(ctx, "kubeadm version -o short"))
	assert.Equal(t, "Kubernetes v"+version, g.sh(ctx, "kubelet --version"))

	deadline := time.Now().Add(containerdWithin)
	for {
		res, err := g.run(ctx, "systemctl is-active containerd.service")
		require.NoError(t, err)
		if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) == "active" {
			break
		}
		require.False(t, time.Now().After(deadline), "containerd not active within %s: %q\n%s", containerdWithin, strings.TrimSpace(res.Stdout), g.sh(ctx, "systemctl status containerd.service --no-pager || true"))
		time.Sleep(pollEvery)
	}
	crictl := g.sh(ctx, "crictl --runtime-endpoint "+containerdSocket+" version")
	t.Logf("crictl version:\n%s", crictl)
	assert.Contains(t, crictl, "RuntimeName:  containerd")

	kubelet := g.sh(ctx, "systemctl show -p ActiveState -p SubState -p Result kubelet.service")
	t.Logf("kubelet: %s", strings.ReplaceAll(kubelet, "\n", " "))
	assert.NotContains(t, kubelet, "ActiveState=failed", "kubelet must restart, not fail, until kubeadm configures it")
	assert.Regexp(t, `ActiveState=(active|activating)`, kubelet)

	assert.Empty(t, g.sh(ctx, "systemctl --failed --no-legend --plain"), "failed units after the pull")
	assert.Equal(t, "running", g.sh(ctx, "systemctl is-system-running || true"), "system state")
}

// assertPCR13 compares the TPM's PCR 13 with the host-computed value and
// logs the events behind it: the initrd's os-separator and the unit's
// measurement, from systemd's userspace event log.
func assertPCR13(ctx context.Context, t *testing.T, g *guest, want string) {
	t.Helper()
	got := strings.ToLower(g.sh(ctx, "cat "+pcr13Sysfs))
	t.Logf("pcr 13: %s\n%s", got, g.sh(ctx, "systemd-analyze pcrs 13 --no-pager 2>&1 || true"))
	events := g.sh(ctx, "grep -E '\"pcr\" *: *13' "+tpmUserspaceLog+" || true")
	t.Logf("pcr 13 events in %s:\n%s", tpmUserspaceLog, events)
	assert.Equal(t, want, got, "PCR 13 must equal attest.SysextPCR of the artifact")
	assert.Equal(t, 2, strings.Count(events, "\n")+1, "two PCR 13 events: os-separator and the sysext measurement")
}

// unitRunSeconds derives the run time of a oneshot unit from the monotonic
// start and exit timestamps of `systemctl show`.
func unitRunSeconds(show string) (float64, bool) {
	var start, exit float64
	for _, row := range strings.Split(show, "\n") {
		k, val, ok := strings.Cut(row, "=")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			continue
		}
		switch k {
		case "ExecMainStartTimestampMonotonic":
			start = f
		case "ExecMainExitTimestampMonotonic":
			exit = f
		}
	}
	if start == 0 || exit < start {
		return 0, false
	}
	return (exit - start) / 1e6, true
}

// readSHA256SUMS parses a sha256sum manifest into file -> hex digest.
func readSHA256SUMS(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- build artifact under the configured image dir
	if err != nil {
		t.Skipf("e2e: %v; run `make -C images kubernetes`", err)
	}
	sums := map[string]string{}
	for _, row := range strings.Split(string(raw), "\n") {
		if fields := strings.Fields(row); len(fields) == 2 {
			sums[fields[1]] = fields[0]
		}
	}
	return sums
}

// readPolicy loads the image's policy.json as the server does.
func readPolicy(t *testing.T, path string) attest.Policy {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- build artifact under the configured image dir
	if err != nil {
		t.Skipf("e2e: %v; run `make -C images verify`", err)
	}
	p, err := attest.ParsePolicy(raw)
	require.NoError(t, err, "%s", path)
	require.NotEmpty(t, p.PCR13, fmt.Sprintf("%s has no pcr13 values; run `make -C images verify-kubernetes`", path))
	return p
}
