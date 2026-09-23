//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/attest"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// The test's network and VMs; the CIDR stays clear of the other tests'.
const (
	attestNetwork  = "e2e-attest"
	attestCIDR     = "192.168.143.0/24"
	attestLearnVM  = "attest-learn-e2e"
	attestGoldenVM = "attest-golden-e2e"
	attestTamperVM = "attest-tamper-e2e"
	// attestUnit is the agent's unit in the initrd and in the system.
	attestUnit = "vm-agent-attest.service"
	// policyFile is the image policy the server reads next to the artifacts.
	policyFile = "policy.json"
	// agentRejected is what the agent prints (exit 2) when the verifier said
	// no; StandardOutput=journal+console puts it on the serial console.
	agentRejected = "Attestation rejected: "
	// learnedNote is the verdict's suffix in learn mode.
	learnedNote = "accepted without golden value"
)

// tamperedOVMFCandidates are the Secure Boot builds of the firmware the
// server boots with by default (OVMF_CODE.4m.fd on Arch, OVMF_CODE_4M.fd on
// Ubuntu/Debian): a different firmware volume and Secure Boot configuration,
// hence different PCR 0 and 7 than the golden values recorded with the
// default build. The tamper case of the design.
var tamperedOVMFCandidates = []string{
	"/usr/share/edk2/x64/OVMF_CODE.secboot.4m.fd",
	"/usr/share/OVMF/OVMF_CODE_4M.secboot.fd",
}

// findTamperedOVMF returns the first installed candidate, or the error of
// the last stat when the host has none.
func findTamperedOVMF() (string, error) {
	var err error
	for _, p := range tamperedOVMFCandidates {
		if _, err = os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", err
}

// Ceilings. Three VMs are created in turn, each through the installer boot;
// the tampered VM only has to reach the initrd's quote.
const (
	attestTestTimeout  = 25 * time.Minute
	initrdQuoteWithin  = 2 * time.Minute
	gatedRetriesWithin = 60 * time.Second
)

// goldenMismatch is the verifier's rejection for a golden firmware PCR
// (internal/attest); the tampered firmware must show up in one of them.
var goldenMismatch = regexp.MustCompile(`golden mismatch: .*\bpcr [02-47] expected [0-9a-f]{64}, got [0-9a-f]{64}`)

// TestAttestation proves the attestation gate end to end through the MCP
// API, with the image's policy.json in a private copy so that the golden
// values it records stay out of the shared build directory. Three servers
// run in turn on one state directory (the network is created once and
// restored by the next server):
//
//  1. learn: a server with --attestation-learn-golden; a VM with
//     require_attestation and user-data reaches ready, both quotes verify
//     (golden PCRs learned), user-data was released and Ignition applied it.
//  2. golden: `vm-manager image golden` writes the learned PCRs into the
//     copied policy.json; a fresh server without learning verifies a new VM
//     against them, nothing learned.
//  3. tamper: a server booting the Secure Boot OVMF build against the same
//     golden values; the initrd quote is rejected with a golden mismatch of
//     a firmware PCR, user-data stays gated and Ignition sits in its fetch
//     loop.
func TestAttestation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), attestTestTimeout)
	defer cancel()
	requireHost(t)
	image := locateImage(t)
	dir := stateDir(t)
	_, testKey := generateSSHKey(t, dir)
	imageDir := privateImageDir(t, image.Dir, filepath.Join(dir, "images"))
	require.FileExists(t, filepath.Join(imageDir, policyFile), "run `make -C images verify`")
	userData := ignitionConfig(t)

	create := func(t *testing.T, m *mcpClient, name string, waitFor vm.WaitFor) vm.VM {
		t.Helper()
		started := time.Now()
		var v vm.VM
		m.call(ctx, api.ToolCreateVM, map[string]any{
			"name":                name,
			"hostname":            name,
			"network":             attestNetwork,
			"ssh_authorized_keys": []string{testKey},
			"user_data":           userData,
			"require_attestation": true,
			"wait_for":            string(waitFor),
		}, &v)
		t.Logf("create_vm_seconds=%.1f (%s, wait_for %s): id %s state %s kubernetes %q", time.Since(started).Seconds(), name, waitFor, v.ID, v.State, v.KubernetesVersion)
		return v
	}
	remove := func(t *testing.T, m *mcpClient, id string) {
		t.Helper()
		var deleted api.DeletedResponse
		m.call(ctx, api.ToolDeleteVM, map[string]any{"id": id}, &deleted)
		assert.True(t, deleted.Deleted)
	}

	// Servers belong to the parent test: a subtest's cleanup would stop one
	// started inside it.
	srv := startServer(ctx, t, dir, imageDir, "--attestation=verify", flagLearnGolden)
	m := newMCPClient(ctx, t, srv.URL)
	m.call(ctx, api.ToolCreateNetwork, map[string]any{"name": attestNetwork, "cidr": attestCIDR}, nil)

	var learnID string
	t.Run("learn: verified quotes release user-data", func(t *testing.T) {
		v := create(t, m, attestLearnVM, vm.WaitReady)
		require.Equal(t, vm.StateReady, v.State, "create_vm with wait_for ready: lastError %q\n%s", v.LastError, srv.logTail())
		assert.True(t, v.Attestation.Required, "get_vm exposes the requirement")
		assert.True(t, v.Attestation.UserDataReleased, "get_vm exposes the release")

		att := getAttestation(ctx, t, m, v.ID)
		assertVerified(t, att)
		assert.ElementsMatch(t, []int{0, 2, 3, 4, 6, 7}, att.Initrd.Learned, "the initrd quote learned the golden firmware PCRs")
		assert.Contains(t, att.Initrd.Message, learnedNote, "learn mode is visible in the verdict")
		// PCR 13 is learned only when the policy has nothing for it: a VM with
		// a Kubernetes version (the image's newest is the default) is compared
		// against pcr13.<version> instead.
		if v.KubernetesVersion == "" {
			assert.Contains(t, att.Ready.Learned, attest.PCRSysext, "no golden 13 and no Kubernetes version: the ready quote learned PCR 13")
		} else {
			assert.NotContains(t, att.Ready.Learned, attest.PCRSysext, "pcr 13 was compared against pcr13.%s", v.KubernetesVersion)
			assert.Contains(t, att.Ready.Message, "pcr 13 kubernetes "+v.KubernetesVersion)
		}

		g := &guest{m: m, id: v.ID}
		g.waitSSH(ctx)
		assert.Equal(t, strings.TrimSpace(ignitionEtcContent), g.sh(ctx, "cat "+ignitionEtcFile), "Ignition applied the released user-data")
		assertAttestUnits(ctx, t, g)
		learnID = v.ID
	})
	require.NotEmpty(t, learnID, "the learn subtest must have produced a VM")

	policy := recordGolden(ctx, t, srv, imageDir, learnID)
	golden := policy.Golden[attest.Bank]
	for _, index := range attest.GoldenIndexes {
		assert.Len(t, golden[index], 64, "policy.json golden.%s.%d", attest.Bank, index)
	}
	assert.Len(t, golden, len(attest.GoldenIndexes), "golden values: the golden firmware PCRs and 13")
	remove(t, m, learnID)
	srv.stop()

	srv = startServer(ctx, t, dir, imageDir, "--attestation=verify")
	m = newMCPClient(ctx, t, srv.URL)
	t.Run("golden: recorded values verify without learning", func(t *testing.T) {
		v := create(t, m, attestGoldenVM, vm.WaitReady)
		require.Equal(t, vm.StateReady, v.State, "create_vm with wait_for ready against golden values: lastError %q\n%s", v.LastError, srv.logTail())
		att := getAttestation(ctx, t, m, v.ID)
		assertVerified(t, att)
		assert.Empty(t, att.Initrd.Learned, "golden values present: nothing to learn at initrd")
		assert.Empty(t, att.Ready.Learned, "golden values present: nothing to learn at ready")
		assert.NotContains(t, att.Initrd.Message, learnedNote)
		remove(t, m, v.ID)
	})

	tamperedOVMF, tamperErr := findTamperedOVMF()
	if tamperErr == nil {
		srv.stop()
		srv = startServer(ctx, t, dir, imageDir, "--attestation=verify", "--ovmf-code="+tamperedOVMF)
		m = newMCPClient(ctx, t, srv.URL)
	}
	t.Run("tamper: different firmware is rejected and user-data stays gated", func(t *testing.T) {
		if tamperErr != nil {
			t.Skipf("e2e: no Secure Boot OVMF build to tamper with: %v", tamperErr)
		}
		v := create(t, m, attestTamperVM, vm.WaitInstalled)
		require.NotNil(t, v.InstalledAt, "create_vm with wait_for installed: state %s lastError %q\n%s", v.State, v.LastError, srv.logTail())
		t.Cleanup(func() { remove(t, m, v.ID) })

		att := waitInitrdQuote(ctx, t, m, v.ID)
		t.Logf("initrd quote of the tampered VM: verified %v, message %q", att.Initrd.Verified, att.Initrd.Message)
		assert.False(t, att.Initrd.Verified, "a boot on other firmware must not verify")
		assert.Regexp(t, goldenMismatch, att.Initrd.Message, "the rejection names a golden firmware PCR")
		assert.False(t, att.UserDataReleased, "user-data stays gated")
		assert.Nil(t, att.Ready, "no ready quote: the boot does not get past the fetch stage")

		console := waitConsole(ctx, t, m, v.ID, gatedRetriesWithin, agentRejected, ignitionFetchRetry, ignitionGatedResult)
		t.Logf("console of the tampered VM (tail):\n%s", tail(console, 12))
		m.call(ctx, api.ToolGetVM, map[string]any{"id": v.ID}, &v)
		assert.Equal(t, vm.StateAttesting, v.State, "get_vm keeps the VM at attesting")
		assert.False(t, v.Attestation.UserDataReleased, "get_vm agrees with get_vm_attestation")
		assert.Nil(t, v.ReadyAt, "the guest cannot reach READY=1 while its initrd waits for user-data")
		assert.True(t, strings.HasPrefix(v.LastError, "initrd attestation rejected: "), "get_vm names the rejection, no vTPM stall in front: %q", v.LastError)
		assert.Regexp(t, goldenMismatch, v.LastError, "lastError carries the verifier's reason")
	})

	var deleted api.DeletedResponse
	m.call(ctx, api.ToolDeleteNetwork, map[string]any{"name": attestNetwork}, &deleted)
	assert.True(t, deleted.Deleted)
	srv.stop()
}

// privateImageDir stages an image directory for one test: every artifact but
// the omitted names is a symlink into src (the shared build directory),
// policy.json is a copy, so `vm-manager image golden` writes golden values
// without touching the artifacts the other tests and the next run read.
func privateImageDir(t *testing.T, src, dst string, omit ...string) string {
	t.Helper()
	src, err := filepath.Abs(src)
	require.NoError(t, err)
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dst, 0o750))
	for _, e := range entries {
		if slices.Contains(omit, e.Name()) {
			continue
		}
		from, to := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.Name() != policyFile {
			require.NoError(t, os.Symlink(from, to))
			continue
		}
		b, err := os.ReadFile(from) // #nosec G304 -- build artifact under the configured image dir
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(to, b, 0o600)) // #nosec G703 -- under the test's state dir
	}
	return dst
}

// getAttestation calls get_vm_attestation.
func getAttestation(ctx context.Context, t *testing.T, m *mcpClient, id string) vm.Attestation {
	t.Helper()
	var att vm.Attestation
	m.call(ctx, api.ToolGetVMAttestation, map[string]any{"id": id}, &att)
	return att
}

// assertVerified checks a ready VM's attestation: both stages verified by
// the same key, each verdict naming the key and the PCR 11 phase path of its
// stage, user-data released, the golden PCRs quoted at ready.
func assertVerified(t *testing.T, att vm.Attestation) {
	t.Helper()
	require.NotNil(t, att.Initrd, "initrd stage recorded")
	require.NotNil(t, att.Ready, "ready stage recorded")
	t.Logf("initrd: %s\nready:  %s", att.Initrd.Message, att.Ready.Message)
	assert.True(t, att.Initrd.Verified, "initrd quote: %s", att.Initrd.Message)
	assert.True(t, att.Ready.Verified, "ready quote: %s", att.Ready.Message)
	assert.True(t, att.UserDataReleased)
	assert.NotNil(t, att.NonceIssuedAt)
	require.NotEmpty(t, att.Initrd.AKFingerprint, "the verifier records the attestation key")
	assert.Equal(t, att.Initrd.AKFingerprint, att.Ready.AKFingerprint, "one key per VM, pinned at initrd")
	assert.Contains(t, att.Initrd.Message, "verified: ak "+att.Initrd.AKFingerprint[:16], "the verdict names the key")
	assert.Contains(t, att.Initrd.Message, "pcr 11 phase "+attest.PhaseInitrd, "the initrd quote covers the enter-initrd phase alone")
	assert.Contains(t, att.Ready.Message, "pcr 11 phase "+attest.PhaseReady, "the ready quote covers the full phase path")
	assert.True(t, att.Ready.At.After(att.Initrd.At), "the ready quote follows the initrd quote")
	for _, index := range attest.GoldenIndexes {
		assert.Len(t, att.Ready.PCRs[index], 64, "ready quote carries PCR %d", index)
	}
}

// assertAttestUnits checks the guest's side: the ready-stage unit succeeded
// and the journal holds the initrd stage's run of this boot.
func assertAttestUnits(ctx context.Context, t *testing.T, g *guest) {
	t.Helper()
	show := g.sh(ctx, "systemctl show -p UnitFileState -p ActiveState -p Result "+attestUnit)
	assert.Contains(t, show, "UnitFileState=enabled", attestUnit)
	assert.Contains(t, show, "ActiveState=active", attestUnit)
	assert.Contains(t, show, "Result=success", attestUnit)
	journal := g.sh(ctx, "journalctl -b -o cat -u "+attestUnit+" --no-pager")
	t.Logf("%s journal (initrd and system stage):\n%s", attestUnit, journal)
	for _, stage := range []string{"stage=initrd", "stage=ready"} {
		assert.Contains(t, journal, stage, "the journal shows the %s run", stage)
	}
	assert.Empty(t, g.sh(ctx, "systemctl --failed --no-legend --plain"), "failed units")
}

// recordGolden runs `vm-manager image golden` (the binary the server was
// built as) against srv and returns the policy it wrote.
func recordGolden(ctx context.Context, t *testing.T, srv *server, imageDir, vmID string) attest.Policy {
	t.Helper()
	cmd := exec.CommandContext(ctx, srv.cmd.Path, "image", "golden", imageID, // #nosec G204 -- the binary startServer built
		"--from-vm", vmID, "--server", srv.URL, "--image-dir", imageDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "vm-manager image golden: %s", out)
	t.Logf("%s", strings.TrimSpace(string(out)))
	assert.Contains(t, string(out), "recorded golden sha256 PCRs 0,2,3,4,6,7,13 of vm "+vmID)
	return readPolicy(t, filepath.Join(imageDir, policyFile))
}

// waitInitrdQuote polls get_vm_attestation until the initrd stage of the
// current boot is recorded.
func waitInitrdQuote(ctx context.Context, t *testing.T, m *mcpClient, id string) vm.Attestation {
	t.Helper()
	deadline := time.Now().Add(initrdQuoteWithin)
	for {
		att := getAttestation(ctx, t, m, id)
		if att.Initrd != nil {
			return att
		}
		var v vm.VM
		m.call(ctx, api.ToolGetVM, map[string]any{"id": id}, &v)
		require.NotEqual(t, vm.StateFailed, v.State, "waiting for the initrd quote: lastError %q", v.LastError)
		if time.Now().After(deadline) {
			var console api.ConsoleResponse
			m.call(ctx, api.ToolGetVMConsole, map[string]any{"id": id, "lines": 10000}, &console)
			t.Fatalf("no initrd quote within %s (state %s); last console lines:\n%s", initrdQuoteWithin, v.State, tail(console.Console, 40))
		}
		time.Sleep(pollEvery)
	}
}

// waitConsole polls get_vm_console until every substring appeared.
func waitConsole(ctx context.Context, t *testing.T, m *mcpClient, id string, within time.Duration, substrings ...string) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var console api.ConsoleResponse
		m.call(ctx, api.ToolGetVMConsole, map[string]any{"id": id, "lines": 10000}, &console)
		missing := 0
		for _, s := range substrings {
			if !strings.Contains(console.Console, s) {
				missing++
			}
		}
		if missing == 0 {
			return console.Console
		}
		require.False(t, time.Now().After(deadline), "%d of %d expected console lines missing after %s; last lines:\n%s", missing, len(substrings), within, tail(console.Console, 30))
		time.Sleep(pollEvery)
	}
}
