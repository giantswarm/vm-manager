//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
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

// The test-only sysext images/scripts/build-e2e-sysext builds next to the
// image: firmware the base root lacks, in a DDI signed with the image's verity
// key, merged from /run/extensions.
const (
	e2eSysextFile  = "e2e/ipe-e2e.raw"
	e2eSysextGuest = "/run/extensions/ipe-e2e.raw"
	e2eFirmware    = "regulatory.db"
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
	if image.Certificate != "" {
		// Before the stray extension: it merges and drops the test's own
		// sysext, which neither that check nor vm-kubernetes may see.
		assertSysextOnlyExecutes(ctx, t, g, image.Dir)
	}
	// Last: every run of the unit extends PCR 13 again.
	assertStrayExtensionRefused(ctx, t, g, sx)

	var deleted api.DeletedResponse
	m.call(ctx, api.ToolDeleteVM, map[string]any{"id": v.ID}, &deleted)
	assert.True(t, deleted.Deleted)
	m.call(ctx, api.ToolDeleteNetwork, map[string]any{"name": kubernetesNetwork}, &deleted)
	assert.True(t, deleted.Deleted)
	srv.stop()
}

// assertSysextOnlyExecutes proves the IPE policy's split (images/README.md,
// IPE) with signed dm-verity devices that are not the base root: a file of the
// Kubernetes sysext executes, but the kernel refuses it as a kernel module and
// as a kexec image, and the firmware a second signed sysext of the test brings
// (e2eSysextFile) as firmware (IPE_ACCESS records with ipe_op=KMODULE,
// KEXEC_IMAGE, FIRMWARE and action=DENY), while the base root's own modules
// still load. IPE judges the file before the module loader, the firmware
// loader or kexec look at its content, so a sysext binary stands in for a .ko
// and a kernel, and a placeholder for the firmware: the same call with a file
// of the base root passes IPE and fails only at the format check. The audit
// record names the path the kernel opened: the sysext's own for the module
// and the firmware, a bind mount of it on the ESP for kexec.
func assertSysextOnlyExecutes(ctx context.Context, t *testing.T, g *guest, imageDir string) {
	t.Helper()
	const sysextFile = "/usr/bin/kubelet"
	require.Contains(t, g.sh(ctx, "findmnt -n -o FSTYPE /usr"), "overlay", "the sysext is merged into /usr")
	assert.Contains(t, g.sh(ctx, sysextFile+" --version"), "Kubernetes v", "a sysext binary executes")
	denials := func(op string) string {
		return g.sh(ctx, "journalctl -b -o cat --no-pager | grep -F 'ipe_op="+op+"' | grep -F 'action=DENY' || true")
	}

	// Kernel modules: the base root's load, the sysext's file is refused by
	// IPE (EACCES) where a base root file fails as a module (ENOEXEC).
	assert.Equal(t, "loaded", g.sh(ctx, "modprobe dummy && grep -q '^dummy ' /proc/modules && echo loaded"), "a base root module loads")
	g.sh(ctx, "modprobe -r dummy")
	assert.NotContains(t, g.sh(ctx, "insmod /usr/bin/true 2>&1 || true"), "Permission denied", "a base root file passes IPE as a module, failing only as one")
	assert.Contains(t, g.sh(ctx, "insmod "+sysextFile+" 2>&1 || true"), "Permission denied", "a sysext file as a kernel module")
	assert.Contains(t, denials("KMODULE"), "kubelet", "IPE refused the sysext file as a kernel module")

	// Firmware: the test's sysext carries /usr/lib/firmware/regulatory.db,
	// which the base root lacks; merged next to the Kubernetes sysext, it is
	// what the firmware loader's default search finds when cfg80211 asks for
	// it on load. cfg80211 asks once per load, and systemd-networkd's nl80211
	// lookup loaded it at boot, before the merge: load it again.
	raw, err := os.ReadFile(filepath.Join(imageDir, e2eSysextFile)) // #nosec G304 -- build artifact under the configured image dir
	require.NoError(t, err, "the e2e sysext; run `make -C images e2e-sysext`")
	g.put(ctx, e2eSysextGuest, raw)
	g.sh(ctx, "systemd-sysext refresh")
	require.Contains(t, g.sh(ctx, "systemd-sysext status --no-legend --no-pager"), "ipe-e2e", "the e2e sysext merged next to the Kubernetes and CNI sysexts")
	t.Logf("cfg80211 before the reload: %s", g.sh(ctx, "{ grep '^cfg80211 ' /proc/modules || echo 'not loaded'; } && "+
		"{ ! grep -q '^cfg80211 ' /proc/modules || modprobe -r cfg80211; } && modprobe cfg80211"))
	assert.Eventually(t, func() bool { return strings.Contains(denials("FIRMWARE"), e2eFirmware) }, 30*time.Second, time.Second,
		"IPE refused the e2e sysext's firmware\n%s", g.sh(ctx, "journalctl -b -k -o short-monotonic --no-pager | grep -iE 'regulatory|firmware|cfg80211|ipe|VFS' | tail -n 20 || true"))
	// Unmerge the test's sysext again: the merged set is the Kubernetes and
	// the CNI sysext alone for what follows (vm-kubernetes checks it).
	g.sh(ctx, "rm -f "+e2eSysextGuest+" && systemd-sysext refresh")
	require.NotContains(t, g.sh(ctx, "systemd-sysext status --no-legend --no-pager"), "ipe-e2e", "the e2e sysext unmerged")

	// kexec: systemctl kexec loads the default boot entry's kernel with
	// kexec_file_load(); a one-shot type 1 entry whose kernel is the sysext's
	// file. The refused load leaves systemctl without a kernel to boot, so it
	// does not reboot.
	out := g.sh(ctx, `esp=$(bootctl --print-esp-path) && mkdir -p "$esp/loader/entries" && `+
		`printf 'title ipe-e2e
linux /ipe-e2e-kernel
' > "$esp/loader/entries/ipe-e2e.conf" && touch "$esp/ipe-e2e-kernel" && `+
		`mount --bind `+sysextFile+` "$esp/ipe-e2e-kernel" && bootctl set-oneshot ipe-e2e.conf; `+
		`{ systemctl kexec 2>&1 || true; }; bootctl set-oneshot ''; umount "$esp/ipe-e2e-kernel"; rm -f "$esp/ipe-e2e-kernel" "$esp/loader/entries/ipe-e2e.conf"`)
	assert.Contains(t, denials("KEXEC_IMAGE"), "ipe-e2e-kernel", "IPE refused the sysext file (bind-mounted on the ESP) as a kexec image\nsystemctl kexec: %s", out)
	t.Logf("IPE denials of the sysexts:\n%s\n%s\n%s", denials("KMODULE"), denials("FIRMWARE"), denials("KEXEC_IMAGE"))
}

// put writes data to path in the guest in one exec_vm call: gzip, base64 on
// the command line (exec_vm runs `sh -c <command>`, whose one argument the
// kernel caps at 128 KiB), and the image's zstd, which reads gzip too.
func (g *guest) put(ctx context.Context, path string, data []byte) {
	g.m.t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(data)
	require.NoError(g.m.t, err)
	require.NoError(g.m.t, zw.Close())
	enc := base64.StdEncoding.EncodeToString(buf.Bytes())
	require.Less(g.m.t, len(enc), 96<<10, "%s compresses to %d bytes of base64, too long for one command", path, len(enc))
	g.sh(ctx, "mkdir -p "+filepath.Dir(path)+" && printf %s "+enc+" | base64 -d | zstd -dcq > "+path)
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
	// The extensions of a hierarchy are one per line in that table; its
	// JSON form names them as a list.
	merged := strings.Join(strings.Fields(g.sh(ctx, `systemd-sysext status --json=short --no-pager | jq -r '.[] | select(.hierarchy == "/usr") | .extensions | if type == "array" then .[] else empty end'`)), ",")
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

// assertCNIPlugins checks where containerd finds CNI plugins and what the
// Cilium CNI sysext provides: containerd's effective plugin directories are
// /usr/lib/cni alone, cilium-cni is there and executes (under IPE: from the
// extension's signed verity device), cgroup2 is at /sys/fs/cgroup for
// cgroup.hostRoot, the sysext's sysctl drop-in is applied, to links Cilium
// creates later too, and on an IPE image a copy of the plugin in /opt/cni/bin,
// where the chart's install-cni-binaries still installs it, is refused
// (images/README.md, Cilium).
func assertCNIPlugins(ctx context.Context, t *testing.T, g *guest, sx sysexts, ipe bool) {
	t.Helper()
	var info struct {
		CNIConfig struct{ PluginDirs []string } `json:"cniconfig"`
	}
	raw := g.sh(ctx, "crictl --runtime-endpoint "+containerdSocket+" info -o json")
	require.NoError(t, json.Unmarshal([]byte(raw), &info), "crictl info: %s", raw)
	assert.Equal(t, []string{"/usr/lib/cni"}, info.CNIConfig.PluginDirs, "containerd's effective bin_dirs: only the sysexts' /usr/lib/cni")

	require.True(t, strings.HasPrefix(sx.cni.version, "cilium-"), "the default cni sysext is Cilium's, got %s", sx.cni.version)
	plugins := strings.Fields(g.sh(ctx, "ls /usr/lib/cni"))
	t.Logf("/usr/lib/cni (kubernetes %s + cni %s): %v", sx.kubernetes.version, sx.cni.version, plugins)
	assert.Contains(t, plugins, "bridge", "reference plugins of the kubernetes sysext")
	const plugin = "cilium-cni"
	require.Contains(t, plugins, plugin, "the plugin of the cni sysext")
	out := g.sh(ctx, "CNI_COMMAND=VERSION /usr/lib/cni/"+plugin+" </dev/null")
	t.Logf("CNI_COMMAND=VERSION /usr/lib/cni/%s: %s", plugin, out)
	assert.Contains(t, out, "supportedVersions", "the cni sysext's plugin executes")

	assert.Equal(t, "cgroup2fs", g.sh(ctx, "stat -fc %T /sys/fs/cgroup"), "cgroup2 at /sys/fs/cgroup, Cilium's cgroup.hostRoot")
	assert.Equal(t, "0", g.sh(ctx, "sysctl -n net.ipv4.conf.all.rp_filter"), "the cni sysext's sysctl drop-in")
	// A link created after the merge, named like Cilium's endpoint veths,
	// gets the drop-in's per-link key from systemd's udev rule; its peer
	// keeps the default.
	link := g.sh(ctx, "ip link add lxce2e type veth peer name e2epeer && udevadm settle && "+
		"sysctl -n net.ipv4.conf.lxce2e.rp_filter net.ipv4.conf.e2epeer.rp_filter; ip link del lxce2e")
	t.Logf("rp_filter of lxce2e and e2epeer: %q\n%s", link, g.sh(ctx, "systemd-sysctl --cat-config --no-pager | grep -B1 -A1 -F lxc || true"))
	assert.True(t, strings.HasPrefix(link, "0\n"), "rp_filter 0 on a new lxc* link: %q", link)

	if !ipe {
		return
	}
	res, err := g.run(ctx, "install -D -m0755 /usr/lib/cni/"+plugin+" /opt/cni/bin/"+plugin+" && CNI_COMMAND=VERSION /opt/cni/bin/"+plugin+" </dev/null")
	require.NoError(t, err)
	t.Logf("copy in /opt/cni/bin: exit %d %s%s", res.ExitCode, res.Stdout, res.Stderr)
	assert.NotZero(t, res.ExitCode, "IPE refuses the plugin copied into /opt/cni/bin")
	assert.Contains(t, res.Stderr, "Permission denied")
	g.sh(ctx, "rm -f /opt/cni/bin/"+plugin)
}

// assertStrayExtensionRefused adds an extension the VM was not created with
// (a directory extension in /run/extensions, which systemd-sysext merges like
// any other) and runs the unit again: it must fail, naming the stray image
// in the set systemd-sysext merged, and succeed again once the image is gone.
func assertStrayExtensionRefused(ctx context.Context, t *testing.T, g *guest, sx sysexts) {
	t.Helper()
	const stray = "/run/extensions/stray"
	g.sh(ctx, "mkdir -p "+stray+"/usr/lib/extension-release.d && echo ID=_any > "+stray+"/usr/lib/extension-release.d/extension-release.stray")
	res, err := g.run(ctx, "systemctl restart "+kubernetesUnit)
	require.NoError(t, err)
	journal := g.sh(ctx, "journalctl -b -o cat -u "+kubernetesUnit+" --no-pager | tail -n 5")
	t.Logf("%s with a stray extension: exit %d\n%s", kubernetesUnit, res.ExitCode, journal)
	assert.NotZero(t, res.ExitCode, "the unit refuses a merged extension the VM was not created with")
	assert.Contains(t, journal, "systemd-sysext merged '"+"cni_"+sx.cni.version+",kubernetes_"+sx.kubernetes.version+",stray' on /usr")

	g.sh(ctx, "rm -r "+stray+" && systemctl reset-failed "+kubernetesUnit)
	res, err = g.run(ctx, "systemctl restart "+kubernetesUnit)
	require.NoError(t, err)
	assert.Zero(t, res.ExitCode, "the unit passes again without the stray extension: %s%s", res.Stdout, res.Stderr)
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
