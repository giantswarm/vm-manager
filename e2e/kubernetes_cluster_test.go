//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// The test's network and VMs. The CIDR stays clear of the other e2e tests,
// the server's default network and the cluster's pod and service ranges.
const (
	clusterNetwork   = "k8s"
	clusterCIDR      = "192.168.150.0/24"
	controlPlaneName = "cp-1"
	workerName       = "w-1"
	podCIDR          = "10.244.0.0/16"
	apiServerPort    = 6443
	// controlPlaneMiB fits etcd, the API server and the controllers next
	// to the CNI; the worker keeps the create_vm default.
	controlPlaneMiB = 3072
)

// What CAPI's kubeadm bootstrap provider writes with format ignition
// (cluster-api, bootstrap/kubeadm/internal/ignition/clc/templates): the
// kubeadm config, a script that runs kubeadm and marks success, and a
// oneshot unit that runs the script once (the config is moved away
// afterwards, so a later boot does not run kubeadm again). The providerID is
// docs/design.md "How CAPI fits", keyed by the VM name: the caller knows the
// name before create_vm returns the id, and user-data is fixed at creation.
const (
	kubeadmConfigPath = "/etc/kubeadm.yml"
	kubeadmScriptPath = "/etc/kubeadm.sh"
	kubeadmUnit       = "kubeadm.service"
	bootstrapMarker   = "/run/cluster-api/bootstrap-success.complete"
	adminKubeconfig   = "/etc/kubernetes/admin.conf"
	kubectlInGuest    = "kubectl --kubeconfig " + adminKubeconfig
	providerIDPrefix  = "giantswarm-vm://"
	// flannelManifest is the CNI: its default pod network is podCIDR, and
	// its DaemonSet installs the plugin binary into /opt/cni/bin, the bind
	// mount from var of images/README.md "Persistent state".
	flannelManifest = "https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml"
	// corednsReadyPort is CoreDNS's ready plugin, reachable on the pod IP:
	// the cross-node proof of the pod network needs no extra image.
	corednsReadyPort = 8181
)

// Ceilings. The kubeadm unit is part of the boot transaction, so PID 1 sends
// READY=1 only once kubeadm init or join finished; the server's boot timeout
// (VM_MANAGER_BOOT_TIMEOUT, read by serve) must cover kubeadm init with its
// image pulls from registry.k8s.io, or the VM is reported running instead of
// ready. create_vm therefore waits for installed and the test follows the
// bootstrap itself, with kubeadm's journal on a timeout.
const (
	clusterTestTimeout  = 15 * time.Minute
	clusterBootTimeout  = 6 * time.Minute
	installedBootWithin = 2 * time.Minute
	bootstrapWithin     = 5 * time.Minute
	cniReadyWithin      = 4 * time.Minute
	joinWithin          = 4 * time.Minute
	readyAfterBootstrap = time.Minute
	clusterPollEvery    = 3 * time.Second
	teardownWithin      = 2 * time.Minute
	journalTailLines    = 60
)

// kubeadmInitConfig is the control plane's kubeadm config in the shape a
// KubeadmControlPlane renders, on kubeadm.k8s.io/v1beta4 (the API of kubeadm
// 1.36, where kubeletExtraArgs is a list of name/value pairs): no
// controlPlaneEndpoint (single control plane, the API server advertises the
// VM's IP), the pod subnet flannel expects, SANs so the host can use the
// admin kubeconfig through forward_port, the CRI socket of the sysext's
// containerd, the providerID as a kubelet flag and the systemd cgroup driver
// containerd.toml is configured for.
const kubeadmInitConfig = `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  criSocket: %[1]s
  kubeletExtraArgs:
  - name: provider-id
    value: %[2]s
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v%[3]s
networking:
  podSubnet: %[4]s
apiServer:
  certSANs:
  - 127.0.0.1
  - localhost
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
`

// kubeadmJoinConfig is a worker's kubeadm config as a KubeadmConfig renders
// it: bootstrap-token discovery against the control plane's IP with the CA
// hash, and the same node registration as the control plane.
const kubeadmJoinConfig = `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: %[1]s
    token: %[2]s
    caCertHashes:
    - %[3]s
nodeRegistration:
  criSocket: %[4]s
  kubeletExtraArgs:
  - name: provider-id
    value: %[5]s
`

// serverLine matches the cluster entry of a kubeconfig.
var serverLine = regexp.MustCompile(`(?m)^(\s*server:\s*)https://\S+`)

// TestKubernetesCluster bootstraps a Kubernetes cluster through the MCP API
// from CAPI-shaped Ignition user-data: a control plane VM runs kubeadm init
// at boot and reaches Ready with flannel, a worker VM joins with a token the
// control plane issued, the host reaches the API server through forward_port
// with the admin kubeconfig and sees both nodes Ready with their providerIDs,
// and the pod network carries traffic between the nodes. It prints
// cp_ready_seconds and worker_join_seconds, each from its create_vm.
func TestKubernetesCluster(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), clusterTestTimeout)
	defer cancel()
	requireHost(t)
	kubectl := requireKubectl(t)
	image := locateImage(t)
	dir := stateDir(t)
	_, testKey := generateSSHKey(t, dir)

	t.Setenv("VM_MANAGER_BOOT_TIMEOUT", clusterBootTimeout.String())
	srv := startServer(ctx, t, dir, image.Dir)
	m := newMCPClient(ctx, t, srv.URL)
	var img images.Image
	m.call(ctx, api.ToolGetImage, map[string]any{"ref": imageID}, &img)
	require.NotEmpty(t, img.KubernetesVersions, "get_image lists no Kubernetes versions; run `make -C images kubernetes verify-kubernetes`")
	version := img.KubernetesVersions[0]

	c := &cluster{t: t, m: m, key: testKey, version: version}
	t.Cleanup(c.teardown)
	m.call(ctx, api.ToolCreateNetwork, map[string]any{"name": clusterNetwork, "cidr": clusterCIDR}, nil)
	c.network = clusterNetwork

	// Control plane: kubeadm init at boot, then the CNI from inside.
	cpStarted := time.Now()
	initConfig := fmt.Sprintf(kubeadmInitConfig, containerdSocket, providerIDPrefix+controlPlaneName, version, podCIDR)
	cp := c.createNode(ctx, controlPlaneName, controlPlaneMiB, capiIgnition(t, "init", initConfig))
	cpg := c.bootstrapped(ctx, cp)
	initSeconds := kubeadmSeconds(ctx, cpg)
	applyCNI(ctx, t, cpg)
	waitClusterSettled(ctx, t, cpg, []string{controlPlaneName}, cniReadyWithin)
	cpReady := time.Since(cpStarted)
	cp = waitFor(ctx, t, m, cp.ID, readyAfterBootstrap, "ready after kubeadm init", func(v vm.VM) bool { return v.State == vm.StateReady })
	t.Logf("cp_ready_seconds=%.1f kubeadm_init_seconds=%.1f boot_to_ready_seconds=%.1f", cpReady.Seconds(), initSeconds, cp.ReadyAt.Sub(*cp.BootedAt).Seconds())

	// Worker: a join token from the control plane, kubeadm join at boot.
	join := parseJoinCommand(t, cpg.sh(ctx, "kubeadm token create --print-join-command --ttl 30m"))
	assert.Equal(t, net.JoinHostPort(cp.IP, strconv.Itoa(apiServerPort)), join.endpoint, "the API server advertises the control plane VM's IP")
	workerStarted := time.Now()
	joinConfig := fmt.Sprintf(kubeadmJoinConfig, join.endpoint, join.token, join.caCertHash, containerdSocket, providerIDPrefix+workerName)
	w := c.createNode(ctx, workerName, 0, capiIgnition(t, "join", joinConfig))
	wg := c.bootstrapped(ctx, w)
	joinSeconds := kubeadmSeconds(ctx, wg)
	waitClusterSettled(ctx, t, cpg, []string{controlPlaneName, workerName}, joinWithin)
	workerJoin := time.Since(workerStarted)
	w = waitFor(ctx, t, m, w.ID, readyAfterBootstrap, "ready after kubeadm join", func(v vm.VM) bool { return v.State == vm.StateReady })
	t.Logf("worker_join_seconds=%.1f kubeadm_join_seconds=%.1f boot_to_ready_seconds=%.1f", workerJoin.Seconds(), joinSeconds, w.ReadyAt.Sub(*w.BootedAt).Seconds())

	// The host: the admin kubeconfig through forward_port.
	var fwd api.ForwardResponse
	m.call(ctx, api.ToolForwardPort, map[string]any{"id": cp.ID, "port": apiServerPort}, &fwd)
	kubeconfig := hostKubeconfig(t, dir, cpg.sh(ctx, "cat "+adminKubeconfig), fwd.Address)
	assertNodesFromHost(ctx, t, kubectl, kubeconfig, fwd.Address)

	assertNodeHealthy(ctx, t, cpg, controlPlaneName)
	assertNodeHealthy(ctx, t, wg, workerName)
	assertPodNetwork(ctx, t, map[string]*guest{controlPlaneName: cpg, workerName: wg})
	assertReport(ctx, t, m, cp.ID)

	c.teardown()
	srv.stop()
	fmt.Printf("cp_ready_seconds=%.1f\nworker_join_seconds=%.1f\n", cpReady.Seconds(), workerJoin.Seconds())
}

// cluster tracks what the test created so that teardown removes it on every
// exit path; the VMs first (delete_network refuses while one is attached).
type cluster struct {
	t       *testing.T
	m       *mcpClient
	key     string
	version string
	network string
	vms     []string
}

// createNode creates a VM on the cluster network with the image's Kubernetes
// sysext and the given user-data, waiting for the install only (see the
// ceilings above). memoryMiB 0 keeps the create_vm default.
func (c *cluster) createNode(ctx context.Context, name string, memoryMiB int, userData string) vm.VM {
	c.t.Helper()
	args := map[string]any{
		"name":                name,
		"hostname":            name,
		"network":             c.network,
		"ssh_authorized_keys": []string{c.key},
		"kubernetes_version":  c.version,
		"user_data":           userData,
		// TODO: require_attestation true once the initrd attestation agent
		// (vm-agent attest --stage=initrd) ships in the image; until then
		// the IMDS would never release the user-data (TestIgnition, gated).
		"require_attestation": false,
		"wait_for":            string(vm.WaitInstalled),
	}
	if memoryMiB > 0 {
		args["memory_mib"] = memoryMiB
	}
	started := time.Now()
	var v vm.VM
	c.m.call(ctx, api.ToolCreateVM, args, &v)
	c.vms = append(c.vms, v.ID)
	c.t.Logf("create_vm %s returned after %.1fs: id %s state %s ip %s", name, time.Since(started).Seconds(), v.ID, v.State, v.IP)
	require.NotNil(c.t, v.InstalledAt, "create_vm %s with wait_for installed: state %s lastError %q", name, v.State, v.LastError)
	return v
}

// bootstrapped waits for the installed boot of v to start, for sshd, and for
// kubeadm.service to write the bootstrap marker. A failed unit or the
// ceiling fails the test with the unit's journal, which holds kubeadm's
// preflight output.
func (c *cluster) bootstrapped(ctx context.Context, v vm.VM) *guest {
	c.t.Helper()
	v = waitFor(ctx, c.t, c.m, v.ID, installedBootWithin, "installed boot of "+v.Name, func(v vm.VM) bool { return v.BootedAt != nil })
	g := &guest{m: c.m, id: v.ID}
	g.waitSSH(ctx)
	deadline := time.Now().Add(bootstrapWithin)
	for {
		res, err := g.run(ctx, "test -f "+bootstrapMarker+" && echo bootstrapped; systemctl is-failed "+kubeadmUnit)
		if err == nil && strings.HasPrefix(res.Stdout, "bootstrapped") {
			c.t.Logf("%s: %s after %.1fs of the installed boot", v.Name, bootstrapMarker, time.Since(*v.BootedAt).Seconds())
			return g
		}
		if err == nil && strings.Contains(res.Stdout, "failed") {
			c.t.Fatalf("%s on %s failed:\n%s", kubeadmUnit, v.Name, diagnostics(ctx, g, kubeadmDiagnostics))
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("no %s on %s within %s (last: %v %+v):\n%s", bootstrapMarker, v.Name, bootstrapWithin, err, res, diagnostics(ctx, g, kubeadmDiagnostics))
		}
		time.Sleep(clusterPollEvery)
	}
}

// kubeadmDiagnostics is what a stuck or failed bootstrap should show: the
// unit's state, the pending jobs and the end of the kubeadm journal, which
// holds the preflight output.
var kubeadmDiagnostics = []string{
	"systemctl show -p ActiveState -p SubState -p Result -p ExecMainStatus " + kubeadmUnit,
	"systemctl list-jobs --no-pager --no-legend",
	"systemctl is-active vm-kubernetes.service containerd.service kubelet.service",
	"journalctl -b -o cat -u " + kubeadmUnit + " --no-pager",
}

// clusterDiagnostics is what a cluster that does not settle should show, from
// the control plane: where the pods are, what CoreDNS, kube-proxy and flannel
// log and were told, whether the host reaches the CoreDNS pods and the
// node's routes, rules, sysctls and modules behind that.
var clusterDiagnostics = []string{
	kubectlInGuest + " get pods -A -o wide",
	kubectlInGuest + " -n kube-system describe pods -l k8s-app=kube-dns | grep -A 15 '^Events:'",
	kubectlInGuest + " -n kube-system logs -l k8s-app=kube-dns --tail=15 --prefix",
	kubectlInGuest + " -n kube-system logs -l k8s-app=kube-dns --tail=15 --prefix --previous",
	kubectlInGuest + " -n kube-system logs -l k8s-app=kube-proxy --tail=15 --prefix",
	kubectlInGuest + " -n kube-flannel logs -l app=flannel --tail=15 --prefix",
	"for ip in $(" + kubectlInGuest + " -n kube-system get pods -l k8s-app=kube-dns -o 'jsonpath={.items[*].status.podIP}'); do echo \"$ip: $(curl -sS --max-time 3 http://$ip:8080/health 2>&1)\"; done",
	"ip -br addr; ip route; networkctl list --no-pager --no-legend",
	"iptables -t nat -S KUBE-SERVICES 2>&1 | head -8; iptables -S FORWARD 2>&1 | head -8; nft list tables 2>&1",
	"sysctl net.ipv4.ip_forward net.bridge.bridge-nf-call-iptables; lsmod | grep -E '^(br_netfilter|vxlan|nf_conntrack|nf_tables|ip_tables|xt_[a-z]+) '",
	"journalctl -b -o cat -u kubelet.service --no-pager | grep -iE 'prob|fail|error' | tail -15",
}

// diagnostics runs cmds in the guest and renders their output for a failure
// message, each capped at journalTailLines.
func diagnostics(ctx context.Context, g *guest, cmds []string) string {
	var b strings.Builder
	for _, cmd := range cmds {
		res, err := g.run(ctx, cmd)
		fmt.Fprintf(&b, "$ %s\n%s%s", cmd, tail(res.Stdout, journalTailLines), tail(res.Stderr, journalTailLines))
		if err != nil {
			fmt.Fprintf(&b, "(exec_vm: %v)", err)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// teardown deletes the VMs and the network; safe to call twice, the second
// call (t.Cleanup after a pass) finds nothing left.
func (c *cluster) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), teardownWithin)
	defer cancel()
	for _, id := range c.vms {
		res := c.m.callRaw(ctx, api.ToolDeleteVM, map[string]any{"id": id})
		assert.False(c.t, res.IsError, "delete_vm %s: %s", id, resultText(c.t, res))
	}
	c.vms = nil
	if c.network != "" {
		res := c.m.callRaw(ctx, api.ToolDeleteNetwork, map[string]any{"name": c.network})
		assert.False(c.t, res.IsError, "delete_network %s: %s", c.network, resultText(c.t, res))
		c.network = ""
	}
}

// capiIgnition renders the Ignition v3.4.0 config CAPI's kubeadm bootstrap
// provider emits for command (init or join) with kubeadmConfig: the two
// files and the enabled kubeadm.service. The unit is ordered after
// vm-kubernetes.service, which pulls and merges the sysext that brings
// kubeadm, containerd and kubelet (docs/design.md, boot flow step 7); CAPI
// users add that line through additionalConfig.
func capiIgnition(t *testing.T, command, kubeadmConfig string) string {
	t.Helper()
	script := "#!/bin/bash\nset -e\n" +
		"kubeadm " + command + " --config " + kubeadmConfigPath + "\n" +
		"mkdir -p " + filepath.Dir(bootstrapMarker) + " && echo success > " + bootstrapMarker + "\n" +
		"mv " + kubeadmConfigPath + " /tmp/\n"
	unit := "[Unit]\nDescription=kubeadm\n" +
		"# Run only once. After successful run, this file is moved to /tmp/.\n" +
		"ConditionPathExists=" + kubeadmConfigPath + "\n" +
		"Wants=network-online.target\n" +
		"After=network-online.target vm-kubernetes.service\n\n" +
		"[Service]\n# To not restart the unit when it exits, as it is expected.\n" +
		"Type=oneshot\nExecStart=" + kubeadmScriptPath + "\n\n" +
		"[Install]\nWantedBy=multi-user.target\n"
	cfg := map[string]any{
		"ignition": map[string]any{"version": "3.4.0"},
		"storage": map[string]any{"files": []map[string]any{
			ignitionFile(kubeadmConfigPath, 0o640, kubeadmConfig),
			ignitionFile(kubeadmScriptPath, 0o700, script),
		}},
		"systemd": map[string]any{
			"units": []map[string]any{{"name": kubeadmUnit, "enabled": true, "contents": unit}},
		},
	}
	b, err := json.Marshal(cfg)
	require.NoError(t, err)
	return string(b)
}

// ignitionFile is one storage.files entry with inline contents.
func ignitionFile(path string, mode int, contents string) map[string]any {
	return map[string]any{
		"path":      path,
		"mode":      mode,
		"overwrite": true,
		"contents":  map[string]any{"source": "data:;base64," + base64.StdEncoding.EncodeToString([]byte(contents))},
	}
}

// kubeadmSeconds is the run time of the kubeadm unit, from systemd's
// monotonic timestamps, for the log; 0 when they are not there.
func kubeadmSeconds(ctx context.Context, g *guest) float64 {
	secs, _ := unitRunSeconds(g.sh(ctx, "systemctl show -p ExecMainStartTimestampMonotonic -p ExecMainExitTimestampMonotonic "+kubeadmUnit))
	return secs
}

// applyCNI installs flannel from inside the control plane with the admin
// kubeconfig kubeadm wrote, and logs the image it runs.
func applyCNI(ctx context.Context, t *testing.T, g *guest) {
	t.Helper()
	out := g.sh(ctx, kubectlInGuest+" apply -f "+flannelManifest)
	t.Logf("kubectl apply -f %s:\n%s", flannelManifest, out)
	image := g.sh(ctx, kubectlInGuest+" -n kube-flannel get daemonset kube-flannel-ds -o 'jsonpath={.spec.template.spec.containers[0].image}'")
	t.Logf("cni image: %s", image)
}

// waitClusterSettled polls the control plane until kubectl lists exactly the
// wanted nodes, all Ready, and every pod in the cluster is Running with all
// its containers ready or Completed.
func waitClusterSettled(ctx context.Context, t *testing.T, g *guest, nodes []string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	started := time.Now()
	for {
		nodesRes, nodesErr := g.run(ctx, kubectlInGuest+" get nodes --no-headers")
		podsRes, podsErr := g.run(ctx, kubectlInGuest+" get pods -A --no-headers")
		if nodesErr == nil && podsErr == nil && nodesReady(nodesRes.Stdout, nodes) && podsSettled(podsRes.Stdout) {
			t.Logf("nodes %v Ready and every pod settled after %.1fs:\n%s%s", nodes, time.Since(started).Seconds(), nodesRes.Stdout, podsRes.Stdout)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nodes %v not Ready with every pod settled within %s (%v %v):\n%s%s%s%s\n%s", nodes, within, nodesErr, podsErr,
				nodesRes.Stdout, nodesRes.Stderr, podsRes.Stdout, podsRes.Stderr, diagnostics(ctx, g, clusterDiagnostics))
		}
		time.Sleep(clusterPollEvery)
	}
}

// nodesReady reports whether `kubectl get nodes --no-headers` lists exactly
// want, each with status Ready.
func nodesReady(out string, want []string) bool {
	ready := map[string]bool{}
	for _, row := range strings.Split(strings.TrimSpace(out), "\n") {
		if fields := strings.Fields(row); len(fields) >= 2 {
			ready[fields[0]] = fields[1] == "Ready"
		}
	}
	if len(ready) != len(want) {
		return false
	}
	for _, name := range want {
		if !ready[name] {
			return false
		}
	}
	return true
}

// podsSettled reports whether `kubectl get pods -A --no-headers` (columns
// NAMESPACE NAME READY STATUS ...) shows at least one pod and only pods that
// are Completed or Running with every container ready.
func podsSettled(out string) bool {
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if len(rows) == 0 || rows[0] == "" {
		return false
	}
	for _, row := range rows {
		fields := strings.Fields(row)
		if len(fields) < 4 {
			return false
		}
		switch fields[3] {
		case "Completed":
		case "Running":
			ready, total, ok := strings.Cut(fields[2], "/")
			if !ok || ready != total {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// joinCommand is what `kubeadm token create --print-join-command` prints,
// the three values a JoinConfiguration needs.
type joinCommand struct {
	endpoint, token, caCertHash string
}

// parseJoinCommand reads `kubeadm join <endpoint> --token <token>
// --discovery-token-ca-cert-hash sha256:<hash>`.
func parseJoinCommand(t *testing.T, out string) joinCommand {
	t.Helper()
	fields := strings.Fields(out)
	require.GreaterOrEqual(t, len(fields), 7, "join command: %q", out)
	require.Equal(t, []string{"kubeadm", "join"}, fields[:2], "join command: %q", out)
	j := joinCommand{endpoint: fields[2]}
	for i := 3; i+1 < len(fields); i += 2 {
		switch fields[i] {
		case "--token":
			j.token = fields[i+1]
		case "--discovery-token-ca-cert-hash":
			j.caCertHash = fields[i+1]
		}
	}
	require.NotEmpty(t, j.token, "join command: %q", out)
	require.True(t, strings.HasPrefix(j.caCertHash, "sha256:"), "join command: %q", out)
	return j
}

// hostKubeconfig writes the guest's admin kubeconfig for the host, with the
// cluster's server rewritten to the forward_port address (127.0.0.1 is among
// the API server's SANs, see kubeadmInitConfig).
func hostKubeconfig(t *testing.T, dir, adminConf, address string) string {
	t.Helper()
	require.Regexp(t, serverLine, adminConf, "admin.conf has a server entry")
	rewritten := serverLine.ReplaceAllString(adminConf, "${1}https://"+address)
	path := filepath.Join(dir, "admin.conf")
	require.NoError(t, os.WriteFile(path, []byte(rewritten), 0o600))
	return path
}

// nodeList is the part of `kubectl get nodes -o json` the assertions read.
type nodeList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			ProviderID string `json:"providerID"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// assertNodesFromHost runs the host's kubectl against the forwarded API
// server: two Ready nodes whose providerIDs are the ones the user-data set.
func assertNodesFromHost(ctx context.Context, t *testing.T, kubectl, kubeconfig, address string) {
	t.Helper()
	wide, err := exec.CommandContext(ctx, kubectl, "--kubeconfig", kubeconfig, "get", "nodes", "-o", "wide").CombinedOutput() // #nosec G204 -- kubectl from PATH, arguments built here
	require.NoError(t, err, "kubectl get nodes through %s: %s", address, wide)
	t.Logf("kubectl get nodes -o wide through %s:\n%s", address, wide)

	raw, err := exec.CommandContext(ctx, kubectl, "--kubeconfig", kubeconfig, "get", "nodes", "-o", "json").Output() // #nosec G204 -- kubectl from PATH, arguments built here
	require.NoError(t, err)
	var list nodeList
	require.NoError(t, json.Unmarshal(raw, &list))
	providerIDs := map[string]string{}
	for _, n := range list.Items {
		providerIDs[n.Metadata.Name] = n.Spec.ProviderID
		ready := false
		for _, cond := range n.Status.Conditions {
			if cond.Type == "Ready" {
				ready = cond.Status == "True"
			}
		}
		assert.True(t, ready, "node %s Ready from the host", n.Metadata.Name)
	}
	assert.Equal(t, map[string]string{
		controlPlaneName: providerIDPrefix + controlPlaneName,
		workerName:       providerIDPrefix + workerName,
	}, providerIDs, "providerIDs from the kubelet flag in the user-data")
}

// assertNodeHealthy checks a node after the bootstrap: the kubeadm unit
// succeeded, the node stack runs, nothing failed, the sysctls kube-proxy and
// the CNI rely on are set, and flannel could install into /opt/cni/bin and
// /etc/cni/net.d.
func assertNodeHealthy(ctx context.Context, t *testing.T, g *guest, name string) {
	t.Helper()
	show := g.sh(ctx, "systemctl show -p ActiveState -p Result -p ExecMainStatus "+kubeadmUnit)
	assert.Contains(t, show, "Result=success", "%s on %s", kubeadmUnit, name)
	assert.Contains(t, show, "ExecMainStatus=0", "%s on %s", kubeadmUnit, name)
	assert.Equal(t, "active\nactive", g.sh(ctx, "systemctl is-active containerd.service kubelet.service"), "node stack on %s", name)
	assert.Empty(t, g.sh(ctx, "systemctl --failed --no-legend --plain"), "failed units on %s", name)
	assert.Equal(t, "running", g.sh(ctx, "systemctl is-system-running || true"), "system state of %s", name)
	assert.Equal(t, "1\n1", g.sh(ctx, "sysctl -n net.ipv4.ip_forward net.bridge.bridge-nf-call-iptables"), "forwarding sysctls on %s", name)
	assert.Equal(t, "installed", g.sh(ctx, "test -x /opt/cni/bin/flannel && test -s /etc/cni/net.d/10-flannel.conflist && echo installed"), "flannel's plugin and config on %s", name)

	// The CNI's links belong to the CNI (images/README.md,
	// 70-kubernetes-cni.network): networkctl's columns are IDX LINK TYPE
	// OPERATIONAL SETUP, and SETUP must be unmanaged for cni0, flannel.1
	// and every pod veth, or networkd detaches the veths from the bridge.
	links := g.sh(ctx, "networkctl list --no-legend --no-pager | grep -E '^ *[0-9]+ (cni|veth|flannel)' || true")
	require.NotEmpty(t, links, "CNI links on %s", name)
	for _, row := range strings.Split(links, "\n") {
		fields := strings.Fields(row)
		if assert.Len(t, fields, 5, "networkctl row %q on %s", row, name) {
			assert.Equal(t, "unmanaged", fields[4], "networkd must leave %s on %s to the CNI", fields[1], name)
		}
	}
}

// assertPodNetwork proves the pod network across nodes without another
// image: CoreDNS runs on one node, and its ready endpoint answers on the pod
// IP from the other node's host network, through flannel's vxlan.
func assertPodNetwork(ctx context.Context, t *testing.T, guests map[string]*guest) {
	t.Helper()
	cp := guests[controlPlaneName]
	podIP := cp.sh(ctx, kubectlInGuest+" -n kube-system get pods -l k8s-app=kube-dns -o 'jsonpath={.items[0].status.podIP}'")
	podNode := cp.sh(ctx, kubectlInGuest+" -n kube-system get pods -l k8s-app=kube-dns -o 'jsonpath={.items[0].spec.nodeName}'")
	require.NotEmpty(t, podIP, "coredns pod IP")
	for name, g := range guests {
		if name == podNode {
			continue
		}
		res, err := g.run(ctx, fmt.Sprintf("curl -sS --max-time 5 http://%s/ready", net.JoinHostPort(podIP, strconv.Itoa(corednsReadyPort))))
		require.NoError(t, err)
		assert.Equal(t, 0, res.ExitCode, "coredns %s on %s from %s: %s%s", podIP, podNode, name, res.Stdout, res.Stderr)
		assert.Equal(t, "OK", strings.TrimSpace(res.Stdout), "coredns ready endpoint through the pod network from %s", name)
		t.Logf("pod network: %s reached coredns %s on %s: %q", name, podIP, podNode, strings.TrimSpace(res.Stdout))
	}
}

// requireKubectl returns the host's kubectl or skips.
func requireKubectl(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skipf("e2e: %v", err)
	}
	return path
}
