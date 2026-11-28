//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The workload pods: one per workload image, on the worker.
const (
	workloadsManifestPath = "/run/vm-manager-e2e/workloads.yaml"
	workloadPod           = `apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: default
spec:
  nodeName: %[2]s
  containers:
  - name: workload
    image: %[3]s
`
	podGiantSwarm   = "giant-swarm"
	podInstallation = "installation"
	podUnsigned     = "unsigned-layer"
)

// assertWorkloads runs the workload images on the worker: the pods of the
// images signed with the Giant Swarm and the installation key run, the pod of
// the image with a layer outside its signatures fails at pull with an event
// naming that layer.
func assertWorkloads(ctx context.Context, t *testing.T, cp, worker *guest, w workloads) {
	t.Helper()
	var manifest []string
	for _, p := range [][2]string{{podGiantSwarm, w.giantSwarm}, {podInstallation, w.installation}, {podUnsigned, w.unsigned}} {
		manifest = append(manifest, fmt.Sprintf(workloadPod, p[0], workerName, p[1]))
	}
	cp.put(ctx, workloadsManifestPath, []byte(strings.Join(manifest, "---\n")))
	cp.sh(ctx, kubectlInGuest+" apply -f "+workloadsManifestPath)

	started := time.Now()
	running := map[string]bool{}
	var refusal string
	for len(running) < 2 || refusal == "" {
		for _, pod := range []string{podGiantSwarm, podInstallation} {
			if !running[pod] && cp.sh(ctx, kubectlInGuest+" get pod "+pod+" -o 'jsonpath={.status.phase} {.status.containerStatuses[0].ready}'") == "Running true" {
				running[pod] = true
				t.Logf("workload %s Running after %.1fs", pod, time.Since(started).Seconds())
			}
		}
		if refusal == "" {
			events := cp.sh(ctx, kubectlInGuest+" get events --field-selector involvedObject.name="+podUnsigned+" -o 'jsonpath={range .items[*]}{.reason}: {.message}{\"\\n\"}{end}'")
			for _, line := range strings.Split(events, "\n") {
				if strings.Contains(line, w.unsignedLayer) {
					refusal = line
				}
			}
		}
		if time.Since(started) > workloadsWithin {
			t.Fatalf("workloads within %s: running %v, refusal of %s %q\n%s", workloadsWithin, running, w.unsignedLayer, refusal,
				diagnostics(ctx, cp, []string{
					kubectlInGuest + " get pods -o wide",
					kubectlInGuest + " get events --sort-by=.lastTimestamp | tail -n 20",
				})+diagnostics(ctx, worker, []string{"journalctl -b -o cat -u containerd.service --no-pager | grep -iE 'error|fail|verity' | tail -n 20"}))
		}
		time.Sleep(clusterPollEvery)
	}
	t.Logf("unsigned layer refused at pull: %s", refusal)
	assert.Contains(t, refusal, "dm-verity", "the pull of %s fails for the layer's missing signature", w.unsigned)
	assert.Equal(t, "Pending", cp.sh(ctx, kubectlInGuest+" get pod "+podUnsigned+" -o 'jsonpath={.status.phase}'"), "the pod of %s does not start", w.unsigned)
	t.Logf("workload images on %s:\n%s", workerName, worker.sh(ctx, "crictl images | grep -E 'IMAGE|vm-manager-e2e'"))

	// The giant-swarm image's config names its user; containerd resolved it
	// through the image's /etc/passwd, read from the layers' dm-verity
	// devices.
	uid := worker.sh(ctx, "cid=$(crictl ps -q --label io.kubernetes.pod.name="+podGiantSwarm+") && "+
		"pid=$(crictl inspect -o go-template --template '{{.info.pid}}' \"$cid\") && grep '^Uid:' /proc/$pid/status | cut -f2")
	assert.Equal(t, workloadUID, uid, "the %s container runs as %s from the image's /etc/passwd", podGiantSwarm, workloadUser)
	t.Logf("workload %s runs as %s: uid %s", podGiantSwarm, workloadUser, uid)
	assertLayerDevices(ctx, t, worker)
}

// assertLayerDevices checks that every containerd-erofs-* dm-verity device
// on the node backs a running container: its EROFS mount is a lower layer of
// a container's overlay. Creating a container opens each layer's device to
// read the image's /etc/passwd and /etc/group, and must close it again.
func assertLayerDevices(ctx context.Context, t *testing.T, g *guest) {
	t.Helper()
	// The image has no awk: shell, grep and coreutils only.
	out := g.sh(ctx, `dmsetup ls --target verity | while read -r name _; do case "$name" in containerd-erofs-*) `+
		`m=$(findmnt -rn -t erofs -o SOURCE,TARGET | grep "^/dev/mapper/$name " | head -n 1 | cut -d' ' -f2); `+
		`if [ -z "$m" ]; then echo "leftover $name: not mounted"; `+
		`elif findmnt -rn -t overlay -o OPTIONS | grep -qF "$m"; then echo "device $name: $m"; `+
		`else echo "leftover $name: $m is in no container's overlay"; fi;; esac; done`)
	var devices, leftover []string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "device "):
			devices = append(devices, line)
		case line != "":
			leftover = append(leftover, line)
		}
	}
	assert.Empty(t, leftover, "containerd-erofs dm-verity devices that back no running container")
	assert.NotEmpty(t, devices, "the running containers' layers are containerd-erofs dm-verity devices")
	t.Logf("layer devices on the node: %d, each the lower layer of a running container", len(devices))
}

// assertLayerOnlyExecutes takes a file of the running giant-swarm pod's own
// layer from the node's root (its EROFS mount on the layer's signed dm-verity
// device) and shows IPE refusing it as a kernel module and as firmware, and
// the kernel or IPE refusing it as a kexec image: the IPE policy pins those
// operations to the base root, and kernel.kexec_load_disabled, where the image
// sets it, refuses kexec before IPE sees the file. The audit record names the
// path the kernel opened: the layer's own for the module and the firmware, a
// bind mount of it on the ESP for kexec.
func assertLayerOnlyExecutes(ctx context.Context, t *testing.T, g *guest) {
	t.Helper()
	marker := workloadDir + "/" + podGiantSwarm
	mounts := g.sh(ctx, `for m in $(findmnt -rn -t erofs -o TARGET); do test -e "$m/`+marker+`" && echo "$m"; done; true`)
	require.NotEmpty(t, mounts, "the giant-swarm pod's layer among the node's EROFS mounts\n%s",
		g.sh(ctx, "findmnt -rn -t erofs,overlay -o TARGET,SOURCE,FSTYPE | tail -n 20; dmsetup ls --target verity"))
	dir := strings.Fields(mounts)[0] + "/" + workloadDir
	file := dir + "/" + workloadFirmware
	source := g.sh(ctx, "findmnt -n -o SOURCE,FSTYPE --target "+dir)
	device := strings.Fields(source)[0]
	t.Logf("the giant-swarm pod's layer on the node: %s on %s, %s", dir, source, g.sh(ctx, "dmsetup table "+device+" | cut -c1-160"))
	assert.Contains(t, source, "erofs", "the layer is an EROFS mount")
	assert.Contains(t, g.sh(ctx, "dmsetup table "+device), "verity", "the layer's device is dm-verity")
	denials := func(op string) string {
		return g.sh(ctx, "journalctl -b -o cat --no-pager | grep -F 'ipe_op="+op+"' | grep -F 'action=DENY' | grep -F '"+workloadDir+"' || true")
	}

	assert.Contains(t, g.sh(ctx, "insmod "+file+" 2>&1 || true"), "Permission denied", "a layer file as a kernel module")
	assert.Contains(t, denials("KMODULE"), file, "IPE refused the layer file as a kernel module")

	// Firmware: the custom firmware path, searched first, is the layer's
	// directory; cfg80211 requests workloadFirmware when it loads.
	const fwPath = "/sys/module/firmware_class/parameters/path"
	previous := g.sh(ctx, "cat "+fwPath)
	g.sh(ctx, "echo "+dir+" > "+fwPath)
	t.Logf("cfg80211 before the reload: %s", g.sh(ctx, "{ grep '^cfg80211 ' /proc/modules || echo 'not loaded'; } && "+
		"{ ! grep -q '^cfg80211 ' /proc/modules || modprobe -r cfg80211; } && modprobe cfg80211"))
	assert.Eventually(t, func() bool { return strings.Contains(denials("FIRMWARE"), file) }, 30*time.Second, time.Second,
		"IPE refused the layer file as firmware\n%s", g.sh(ctx, "journalctl -b -k -o short-monotonic --no-pager | grep -iE 'regulatory|firmware|cfg80211|ipe' | tail -n 20 || true"))
	g.sh(ctx, "echo '"+previous+"' > "+fwPath)

	// kexec: systemctl kexec loads the default boot entry's kernel with
	// kexec_file_load(); a one-shot type 1 entry whose kernel is the layer
	// file. systemctl prints the syscall's error at debug level only.
	out := g.sh(ctx, `esp=$(bootctl --print-esp-path) && mkdir -p "$esp/loader/entries" && `+
		`printf 'title vm-manager-e2e
linux /vm-manager-e2e-kernel
' > "$esp/loader/entries/vm-manager-e2e.conf" && touch "$esp/vm-manager-e2e-kernel" && `+
		`mount --bind `+file+` "$esp/vm-manager-e2e-kernel" && bootctl set-oneshot vm-manager-e2e.conf; `+
		`{ SYSTEMD_LOG_LEVEL=debug systemctl kexec 2>&1 || true; }; bootctl set-oneshot ''; umount "$esp/vm-manager-e2e-kernel"; rm -f "$esp/vm-manager-e2e-kernel" "$esp/loader/entries/vm-manager-e2e.conf"`)
	kexecDenial := g.sh(ctx, "journalctl -b -o cat --no-pager | grep -F 'ipe_op=KEXEC_IMAGE' | grep -F 'action=DENY' | grep -F vm-manager-e2e-kernel || true")
	switch {
	case kexecDenial != "":
		t.Logf("kexec of the layer file: refused by IPE: %s", kexecDenial)
	case g.sh(ctx, "cat /proc/sys/kernel/kexec_load_disabled") == "1" && kexecEPERM.MatchString(out):
		t.Logf("kexec of the layer file: refused by the kernel with EPERM (kexec_load_disabled=1), before IPE: %s", kexecEPERM.FindString(out))
	default:
		t.Errorf("kexec of the layer file (bind-mounted on the ESP) was refused neither by IPE nor by kexec_load_disabled\nsystemctl kexec: %s", out)
	}
	assert.Equal(t, "0", g.sh(ctx, "cat /sys/kernel/kexec_loaded"), "no kexec kernel loaded")
	t.Logf("IPE denials of the container layer:\n%s\n%s", denials("KMODULE"), denials("FIRMWARE"))
}
