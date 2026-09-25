//go:build e2e

package e2e

import (
	"context"
	"path/filepath"
	"slices"
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

// The test's network; the CIDR stays clear of the other e2e tests and of the
// server's default network. One VM per published Kubernetes version lives on
// it at the same time, so both guests pull from the same served directory.
const (
	versionsNetwork = "e2e-kubernetes-versions"
	versionsCIDR    = "192.168.144.0/24"
	versionsTimeout = 20 * time.Minute
	unknownVersion  = "9.9.9"
)

// node is one VM and the artifact it must end up running.
type node struct {
	version string
	file    string
	line    string
	pcr13   string
	vm      vm.VM
}

// The image directory serves several Kubernetes sysext versions side by side
// (images/README.md, "Kubernetes sysext": `make -C images kubernetes-all`).
// get_image lists every published version, newest first; a VM created with
// one of them pulls, merges and measures exactly that version although the
// directory offers the others too; a version that is not published is an
// invalid argument to create_vm.
func TestKubernetesVersions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), versionsTimeout)
	defer cancel()
	requireHost(t)
	image := locateImage(t)
	dir := stateDir(t)
	_, testKey := generateSSHKey(t, dir)

	sums := readSHA256SUMS(t, filepath.Join(image.Dir, "sysupdate", "kubernetes", "SHA256SUMS"))
	policy := readPolicy(t, filepath.Join(image.Dir, "policy.json"))
	var published []string
	for file := range sums {
		if strings.HasPrefix(file, "kubernetes_") && strings.HasSuffix(file, ".raw") {
			published = append(published, strings.TrimSuffix(strings.TrimPrefix(file, "kubernetes_"), ".raw"))
		}
	}
	require.GreaterOrEqual(t, len(published), 2, "SHA256SUMS lists %v; two versions are needed, run `make -C images kubernetes-all verify-kubernetes`", published)

	srv := startServer(ctx, t, dir, image.Dir, flagLearnGolden)
	m := newMCPClient(ctx, t, srv.URL)

	// The catalog: exactly the published versions, newest first, each with
	// a PCR 13 policy entry that agrees with attest.SysextPCR.
	var img images.Image
	m.call(ctx, api.ToolGetImage, map[string]any{"ref": imageID}, &img)
	assert.ElementsMatch(t, published, img.KubernetesVersions, "get_image lists what SHA256SUMS lists")
	assert.True(t, slices.IsSortedFunc(img.KubernetesVersions, func(a, b string) int { return images.CompareVersions(b, a) }),
		"kubernetesVersions newest first: %v", img.KubernetesVersions)
	nodes := make([]*node, 0, len(img.KubernetesVersions))
	for _, version := range img.KubernetesVersions {
		file := "kubernetes_" + version + ".raw"
		line := attest.SysextMeasurement(sums[file], file)
		want := attest.SysextPCR(line)
		require.Equal(t, want, policy.PCR13[version], "policy.json pcr13[%s] disagrees with attest.SysextPCR(%q); run `make -C images verify-kubernetes`", version, line)
		nodes = append(nodes, &node{version: version, file: file, line: line, pcr13: want})
	}
	t.Logf("kubernetes versions: %v", img.KubernetesVersions)

	m.call(ctx, api.ToolCreateNetwork, map[string]any{"name": versionsNetwork, "cidr": versionsCIDR}, nil)

	// A version the directory does not serve is refused up front.
	res := m.callRaw(ctx, api.ToolCreateVM, map[string]any{
		"name":                "kubernetes-unknown",
		"network":             versionsNetwork,
		"kubernetes_version":  unknownVersion,
		"require_attestation": false,
	})
	require.True(t, res.IsError, "create_vm with kubernetes_version %s must fail: %s", unknownVersion, resultText(t, res))
	text := resultText(t, res)
	t.Logf("create_vm kubernetes_version=%s: %s", unknownVersion, text)
	assert.Contains(t, text, "invalid_request", "an invalid argument, not a server error")
	assert.Contains(t, text, `"`+unknownVersion+`"`)
	assert.Contains(t, text, "not available")
	for _, version := range img.KubernetesVersions {
		assert.Contains(t, text, version, "the error names the available versions")
	}

	// One VM per version. All stay up until every guest was inspected.
	for _, n := range nodes {
		name := "k8s-" + strings.ReplaceAll(n.version, ".", "-")
		started := time.Now()
		m.call(ctx, api.ToolCreateVM, map[string]any{
			"name":                name,
			"hostname":            name,
			"network":             versionsNetwork,
			"ssh_authorized_keys": []string{testKey},
			"kubernetes_version":  n.version,
			"require_attestation": false,
			"wait_for":            string(vm.WaitReady),
		}, &n.vm)
		t.Logf("%s: create_vm returned after %.1fs: id %s state %s ip %s", name, time.Since(started).Seconds(), n.vm.ID, n.vm.State, n.vm.IP)
		require.Equal(t, vm.StateReady, n.vm.State, "%s: create_vm with wait_for ready: lastError %q\n%s", name, n.vm.LastError, srv.logTail())
		assert.Equal(t, n.version, n.vm.KubernetesVersion, "the record pins the version served as /kubernetes-version")
	}

	for _, n := range nodes {
		t.Run(n.version, func(t *testing.T) {
			g := &guest{m: m, id: n.vm.ID}
			g.waitSSH(ctx)
			assertKubernetesPulled(ctx, t, g, n.version, n.file, n.line)
			assert.Equal(t, "v"+n.version, g.sh(ctx, "kubeadm version -o short"))
			assert.Equal(t, "Kubernetes v"+n.version, g.sh(ctx, "kubelet --version"))
			assertPCR13(ctx, t, g, n.pcr13)
			assertOtherVersionsUntouched(ctx, t, g, n, nodes)
		})
	}

	var deleted api.DeletedResponse
	for _, n := range nodes {
		m.call(ctx, api.ToolDeleteVM, map[string]any{"id": n.vm.ID}, &deleted)
		assert.True(t, deleted.Deleted)
	}
	m.call(ctx, api.ToolDeleteNetwork, map[string]any{"name": versionsNetwork}, &deleted)
	assert.True(t, deleted.Deleted)
	srv.stop()
}

// assertOtherVersionsUntouched checks that the guest saw the other versions
// on offer (sysupdate lists the whole served directory) and took none of
// them: nothing else was downloaded, nothing else is merged, and the
// extension-release of the merged tree is this version's.
func assertOtherVersionsUntouched(ctx context.Context, t *testing.T, g *guest, n *node, nodes []*node) {
	t.Helper()
	list := g.sh(ctx, "/usr/lib/systemd/systemd-sysupdate --component=kubernetes list --no-pager 2>&1 || true")
	t.Logf("systemd-sysupdate --component=kubernetes list:\n%s", list)
	journal := g.sh(ctx, "journalctl -b -o cat -u "+kubernetesUnit+" --no-pager")
	assert.Len(t, pulledLine.FindAllString(journal, -1), 1, "one download")
	for _, other := range nodes {
		assert.Contains(t, list, other.version, "sysupdate sees every served version")
		if other == n {
			continue
		}
		assert.NotContains(t, journal, other.file, "%s must not be mentioned by the unit", other.file)
		assert.Equal(t, "", g.sh(ctx, "ls "+extensionsDir+"/"+other.file+" 2>/dev/null || true"), "%s not on disk", other.file)
	}
	release := g.sh(ctx, "cat /usr/lib/extension-release.d/extension-release.kubernetes_"+n.version)
	assert.Contains(t, release, "SYSEXT_VERSION_ID="+n.version)
}
