//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
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

// pulledLine is what /usr/lib/vm-manager/kubernetes logs after the download;
// the Kubernetes sysext is pulled first.
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

	policy := readPolicy(t, filepath.Join(image.Dir, "policy.json"))

	srv := startServer(ctx, t, dir, image.Dir, flagLearnGolden)
	m := newMCPClient(ctx, t, srv.URL)

	var img images.Image
	m.call(ctx, api.ToolGetImage, map[string]any{"ref": imageID}, &img)
	require.NotEmpty(t, img.KubernetesVersions, "get_image lists no Kubernetes versions; run `make -C images kubernetes verify-kubernetes`")
	require.NotEmpty(t, img.CNIVersions, "get_image lists no CNI versions; run `make -C images cni verify-cni`")
	version := img.KubernetesVersions[0]
	// The host side of the PCR 13 rule: the sha256 verify-sysext records per
	// sysext is the published file's, and attest.SysextPCR computes the value
	// of the pair from them.
	sx := publishedSysexts(t, image.Dir, policy, version, img.DefaultCNIVersion())
	t.Logf("kubernetes %s, cni %s: measurements %q, expected pcr 13 %s", version, sx.cni.version, sx.lines(), sx.pcr13())

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
	assert.Equal(t, sx.cni.version, v.CNIVersion, "create_vm's default CNI, served as /cni-version")
	require.NotNil(t, v.BootedAt)
	require.NotNil(t, v.ReadyAt)
	t.Logf("boot_to_ready_seconds=%.1f", v.ReadyAt.Sub(*v.BootedAt).Seconds())

	g := &guest{m: m, id: v.ID}
	g.waitSSH(ctx)
	assertKubernetesPulled(ctx, t, g, sx)
	assertKubernetesRunning(ctx, t, g, version)
	assertCNIPlugins(ctx, t, g, sx, image.Certificate != "")
	assertPCR13(ctx, t, g, sx)

	var deleted api.DeletedResponse
	m.call(ctx, api.ToolDeleteVM, map[string]any{"id": v.ID}, &deleted)
	assert.True(t, deleted.Deleted)
	m.call(ctx, api.ToolDeleteNetwork, map[string]any{"name": kubernetesNetwork}, &deleted)
	assert.True(t, deleted.Deleted)
	srv.stop()
}

// sysext is one published sysext file and the SHA256SUMS line
// vm-kubernetes.service measures into PCR 13 for it.
type sysext struct{ component, version, file, line string }

// sysexts is what a VM merges: its Kubernetes sysext and its CNI sysext, in
// the order of their PCR 13 events.
type sysexts struct{ kubernetes, cni sysext }

func (s sysexts) all() []sysext { return []sysext{s.kubernetes, s.cni} }

func (s sysexts) lines() []string {
	var out []string
	for _, x := range s.all() {
		out = append(out, x.line)
	}
	return out
}

// pcr13 is the value the verifier computes for the pair (attest.SysextPCR).
func (s sysexts) pcr13() string { return attest.SysextPCR(s.lines()...) }

// publishedSysexts reads the published files of a Kubernetes version and a
// CNI release from their SHA256SUMS and checks that policy.json records
// their sha256 (sysexts.<component>), what the verifier computes PCR 13
// from.
func publishedSysexts(t *testing.T, imageDir string, policy attest.Policy, kubernetes, cni string) sysexts {
	t.Helper()
	get := func(component, version string) sysext {
		sums := readSHA256SUMS(t, filepath.Join(imageDir, "sysupdate", component, "SHA256SUMS"))
		file := component + "_" + version + ".raw"
		require.Contains(t, sums, file, "SHA256SUMS of the %s component", component)
		require.Equal(t, sums[file], policy.Sysexts[component][version], "policy.json sysexts.%s[%s] is not the published sha256; run `make -C images verify`", component, version)
		return sysext{component: component, version: version, file: file, line: attest.SysextMeasurement(sums[file], file)}
	}
	return sysexts{kubernetes: get(attest.SysextKubernetes, kubernetes), cni: get(attest.SysextCNI, cni)}
}

// assertKubernetesPulled checks the pull unit and the file lifecycle: the
// unit succeeded in this boot, downloaded the files, logged the measurements
// in order, and exactly the Kubernetes and the CNI extension are merged,
// each from exactly one file.
func assertKubernetesPulled(ctx context.Context, t *testing.T, g *guest, sx sysexts) {
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
		assert.Equal(t, sx.kubernetes.file, pulled[1])
		t.Logf("kubernetes_pull_seconds=%s (%s bytes)", pulled[3], pulled[2])
	}
	assert.Contains(t, journal, "pulled "+sx.cni.file+" (", "the unit downloads the CNI sysext")
	k := strings.Index(journal, "measured '"+sx.kubernetes.line+"' into PCR 13")
	c := strings.Index(journal, "measured '"+sx.cni.line+"' into PCR 13")
	assert.True(t, k >= 0 && c > k, "the unit measures the SHA256SUMS line of kubernetes, then of cni")
	assert.NotContains(t, journal, "removing superseded", "a fresh VM has nothing to supersede")

	status := g.sh(ctx, "systemd-sysext status --no-legend --no-pager")
	t.Logf("systemd-sysext status:\n%s", status)
	merged := ""
	for _, row := range strings.Split(status, "\n") {
		if fields := strings.Fields(row); len(fields) >= 2 && fields[0] == "/usr" {
			merged = fields[1]
		}
	}
	assert.ElementsMatch(t, []string{"kubernetes_" + sx.kubernetes.version, "cni_" + sx.cni.version}, strings.Split(merged, ","), "exactly these extensions merged on /usr")
	assert.ElementsMatch(t, []string{sx.kubernetes.file, sx.cni.file}, strings.Fields(g.sh(ctx, "ls "+extensionsDir)), "exactly one file per component in %s", extensionsDir)
	assert.Equal(t, "1", g.sh(ctx, "ls /usr/lib/extension-release.d/ | grep -c ^extension-release.kubernetes_"), "one kubernetes extension-release")
	assert.Equal(t, "1", g.sh(ctx, "ls /usr/lib/extension-release.d/ | grep -c ^extension-release.cni_"), "one cni extension-release")
	// The extension's verity device, like the root's, was opened with its
	// signature and the kernel verified it: what lets the image's IPE
	// policy run the binaries it brings (images/README.md, IPE).
	tables := g.sh(ctx, "dmsetup table --target verity")
	t.Logf("verity devices:\n%s", tables)
	assert.Equal(t, "3", g.sh(ctx, "dmsetup table --target verity | grep -c root_hash_sig_key_desc || true"), "the root and both extensions verified by the kernel")
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

// assertCNIPlugins checks where containerd finds CNI plugins: its effective
// plugin directories are /usr/lib/cni alone, the CNI sysext's plugins are
// there and execute (under IPE: from the extension's signed verity device),
// and on an IPE image a copy of the plugin in /opt/cni/bin, where a CNI
// DaemonSet still installs it, is refused.
func assertCNIPlugins(ctx context.Context, t *testing.T, g *guest, sx sysexts, ipe bool) {
	t.Helper()
	var info struct {
		CNIConfig struct{ PluginDirs []string } `json:"cniconfig"`
	}
	raw := g.sh(ctx, "crictl --runtime-endpoint "+containerdSocket+" info -o json")
	require.NoError(t, json.Unmarshal([]byte(raw), &info), "crictl info: %s", raw)
	assert.Equal(t, []string{"/usr/lib/cni"}, info.CNIConfig.PluginDirs, "containerd's effective bin_dirs: only the sysexts' /usr/lib/cni")

	plugins := strings.Fields(g.sh(ctx, "ls /usr/lib/cni"))
	t.Logf("/usr/lib/cni (kubernetes %s + cni %s): %v", sx.kubernetes.version, sx.cni.version, plugins)
	assert.Contains(t, plugins, "bridge", "reference plugins of the kubernetes sysext")
	name := strings.SplitN(sx.cni.version, "-", 2)[0]
	require.Contains(t, plugins, name, "the plugin of the cni sysext")
	out := g.sh(ctx, "CNI_COMMAND=VERSION /usr/lib/cni/"+name+" </dev/null")
	t.Logf("CNI_COMMAND=VERSION /usr/lib/cni/%s: %s", name, out)
	assert.Contains(t, out, "supportedVersions", "the cni sysext's plugin executes")
	if !ipe {
		return
	}
	res, err := g.run(ctx, "install -D -m0755 /usr/lib/cni/"+name+" /opt/cni/bin/"+name+" && CNI_COMMAND=VERSION /opt/cni/bin/"+name+" </dev/null")
	require.NoError(t, err)
	t.Logf("copy in /opt/cni/bin: exit %d %s%s", res.ExitCode, res.Stdout, res.Stderr)
	assert.NotZero(t, res.ExitCode, "IPE refuses the plugin copied into /opt/cni/bin")
	assert.Contains(t, res.Stderr, "Permission denied")
	g.sh(ctx, "rm -f /opt/cni/bin/"+name)
}

// assertPCR13 compares the TPM's PCR 13 with the host-computed value and
// logs the events behind it: the initrd's os-separator and the unit's
// measurements, from systemd's userspace event log.
func assertPCR13(ctx context.Context, t *testing.T, g *guest, sx sysexts) {
	want := sx.pcr13()
	t.Helper()
	got := strings.ToLower(g.sh(ctx, "cat "+pcr13Sysfs))
	t.Logf("pcr 13: %s\n%s", got, g.sh(ctx, "systemd-analyze pcrs 13 --no-pager 2>&1 || true"))
	events := g.sh(ctx, "grep -E '\"pcr\" *: *13' "+tpmUserspaceLog+" || true")
	t.Logf("pcr 13 events in %s:\n%s", tpmUserspaceLog, events)
	assert.Equal(t, want, got, "PCR 13 must equal attest.SysextPCR of the artifact")
	assert.Equal(t, 1+len(sx.all()), strings.Count(events, "\n")+1, "PCR 13 events: os-separator and one measurement per sysext")
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
	require.NotEmpty(t, p.Sysexts[attest.SysextKubernetes], fmt.Sprintf("%s has no sysexts.kubernetes values; run `make -C images verify`", path))
	require.NotEmpty(t, p.Sysexts[attest.SysextCNI], fmt.Sprintf("%s has no sysexts.cni values; run `make -C images verify`", path))
	return p
}
