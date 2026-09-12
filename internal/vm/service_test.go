package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/imds"
	"github.com/giantswarm/vm-manager/internal/network"
	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/storage"
)

const (
	testNetwork  = "lan"
	testImageRef = "giantswarm-vm-base_0.1.0"
	waitEvery    = 2 * time.Millisecond
	waitAtMost   = 5 * time.Second
)

type harness struct {
	t        *testing.T
	ctx      context.Context
	svc      *Service
	ev       *events
	clock    *fakeClock
	rt       *fakeRuntime
	tpm      *fakeTPM
	store    *fakeStorage
	nets     *fakeNetworks
	notify   *fakeNotifier
	stateDir string
	imageDir string
	imgs     *fakeImages
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newHarness wires a Service to fakes with one network "lan".
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, ctx: context.Background(), stateDir: t.TempDir(), imageDir: t.TempDir()}
	h.ev = &events{}
	h.clock = &fakeClock{now: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
	h.rt = &fakeRuntime{ev: h.ev}
	h.tpm = &fakeTPM{ev: h.ev}
	h.store = &fakeStorage{ev: h.ev, dir: t.TempDir()}
	h.nets = &fakeNetworks{ev: h.ev}
	h.notify = &fakeNotifier{}
	for _, f := range []string{"giantswarm-vm-base_0.1.0.efi", "giantswarm-vm-base_0.1.0.raw", "OVMF_CODE.fd", "OVMF_VARS.fd"} {
		require.NoError(t, os.WriteFile(filepath.Join(h.imageDir, f), []byte(f), 0o600))
	}
	h.imgs = &fakeImages{dir: h.imageDir, imgs: []images.Image{{
		ID: "giantswarm-vm-base", Version: "0.1.0",
		UKI:                filepath.Join(h.imageDir, "giantswarm-vm-base_0.1.0.efi"),
		Disk:               filepath.Join(h.imageDir, "giantswarm-vm-base_0.1.0.raw"),
		KubernetesVersions: []string{"1.32.0", "1.31.2"},
	}}}
	h.start()
	_, err := h.svc.CreateNetwork(h.ctx, network.Spec{Name: testNetwork, CIDR: "127.0.0.0/24"})
	require.NoError(t, err)
	return h
}

// start builds the Service (again) on the harness' state and fakes.
func (h *harness) start() {
	h.t.Helper()
	svc, err := New(Options{
		StateDir:         h.stateDir,
		Images:           h.imgs,
		Storage:          h.store,
		TPM:              h.tpm,
		Runtime:          h.rt,
		Networks:         h.nets,
		Notify:           h.notify,
		OVMFCode:         filepath.Join(h.imageDir, "OVMF_CODE.fd"),
		OVMFVarsTemplate: filepath.Join(h.imageDir, "OVMF_VARS.fd"),
		Region:           "host1",
		Logger:           quiet(),
		Clock:            h.clock,
	})
	require.NoError(h.t, err)
	require.NoError(h.t, svc.Load(h.ctx))
	h.svc = svc
	h.t.Cleanup(func() { require.NoError(h.t, svc.Close(context.Background())) })
}

func (h *harness) spec(name string) Spec {
	return Spec{Name: name, CPUs: 2, MemoryMiB: 2048, DiskGiB: 10, Network: testNetwork,
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`), SSHAuthorizedKeys: []string{"ssh-ed25519 AAAAuser user@laptop"},
		Metadata: map[string]string{"role": "control-plane"}}
}

func (h *harness) create(name string) *VM {
	h.t.Helper()
	v, err := h.svc.Create(h.ctx, h.spec(name))
	require.NoError(h.t, err)
	return v
}

func (h *harness) waitState(id string, want State) *VM {
	h.t.Helper()
	var v *VM
	require.Eventually(h.t, func() bool {
		var err error
		v, err = h.svc.Get(id)
		return err == nil && v.State == want
	}, waitAtMost, waitEvery, "vm %s did not reach %s (last: %+v)", id, want, v)
	return v
}

func (h *harness) waitInstances(n int) *fakeInstance {
	h.t.Helper()
	require.Eventually(h.t, func() bool { return h.rt.count() >= n }, waitAtMost, waitEvery, "waiting for qemu start #%d", n)
	return h.rt.at(n - 1)
}

func (h *harness) waitTimers(n int) {
	h.t.Helper()
	require.Eventually(h.t, func() bool { return h.clock.timers() >= n }, waitAtMost, waitEvery, "waiting for %d timers", n)
}

// install ends the installer with exit 0 and waits for phase B.
func (h *harness) install(id string) *fakeInstance {
	h.t.Helper()
	h.waitInstances(1).Exit(0)
	h.waitState(id, StateBooting)
	return h.waitInstances(2)
}

func (h *harness) ready(id string, cid uint32) *VM {
	h.t.Helper()
	require.Eventually(h.t, func() bool { return h.notify.notify(cid, map[string]string{"READY": "1", "STATUS": "up"}) }, waitAtMost, waitEvery)
	return h.waitState(id, StateReady)
}

func (h *harness) lan() *fakeNetwork {
	n, err := h.nets.Get(testNetwork)
	require.NoError(h.t, err)
	return n.(*fakeNetwork)
}

func TestCreateValidation(t *testing.T) {
	h := newHarness(t)
	ok := h.spec("good")
	tests := []struct {
		name string
		mod  func(*Spec)
		err  error
	}{
		{"bad name", func(s *Spec) { s.Name = "Bad_Name" }, apierr.ErrInvalid},
		{"no cpus", func(s *Spec) { s.CPUs = 0 }, apierr.ErrInvalid},
		{"tiny memory", func(s *Spec) { s.MemoryMiB = 64 }, apierr.ErrInvalid},
		{"no disk", func(s *Spec) { s.DiskGiB = 0 }, apierr.ErrInvalid},
		{"no network", func(s *Spec) { s.Network = "" }, apierr.ErrInvalid},
		{"unknown network", func(s *Spec) { s.Network = "nope" }, apierr.ErrInvalid},
		{"unknown image", func(s *Spec) { s.Image = "nope" }, apierr.ErrInvalid},
		{"unknown kubernetes version", func(s *Spec) { s.KubernetesVersion = "9.9.9" }, apierr.ErrInvalid},
		{"bad waitFor", func(s *Spec) { s.WaitFor = "soon" }, apierr.ErrInvalid},
		{"bad metadata key", func(s *Spec) { s.Metadata = map[string]string{"a/b": "c"} }, apierr.ErrInvalid},
		{"multiline ssh key", func(s *Spec) { s.SSHAuthorizedKeys = []string{"a\nb"} }, apierr.ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := ok
			tc.mod(&s)
			_, err := h.svc.Create(h.ctx, s)
			require.ErrorIs(t, err, tc.err)
		})
	}
	assert.Empty(t, h.svc.List(), "no record survives a rejected create")
	assert.Zero(t, h.rt.count())

	v := h.create("good")
	_, err := h.svc.Create(h.ctx, h.spec("good"))
	require.ErrorIs(t, err, apierr.ErrConflict, "duplicate name")
	assert.Equal(t, testImageRef, v.Image, "image reference is pinned")
	assert.Equal(t, "1.32.0", v.KubernetesVersion, "highest kubernetes version is the default")
	assert.Equal(t, "good", v.Hostname)
	assert.Equal(t, "host1", v.Region)
	assert.Equal(t, testNetwork, v.Zone)
}

func TestLifecycleInstallBootReady(t *testing.T) {
	h := newHarness(t)
	v := h.create("node1")
	require.Equal(t, StateInstalling, v.State)
	assert.Equal(t, qemu.PhaseInstall, v.Phase)
	assert.Equal(t, uint32(qemu.MinCID), v.CID)
	assert.Equal(t, "127.0.0.2", v.IP)
	assert.Len(t, v.MachineID, 32)
	assert.Contains(t, v.SSHPublicKey, "ssh-ed25519 ")
	assert.Equal(t, []string{"vm-" + v.ID + storage.VolumeSuffix}, h.store.names())
	assert.FileExists(t, v.Paths.OVMFVars)
	assert.FileExists(t, v.Paths.SSHKey)
	assert.FileExists(t, v.Paths.UserData)
	assert.Equal(t, 1, h.tpm.running())

	// Phase A spec.
	a := h.waitInstances(1).spec
	assert.Equal(t, qemu.PhaseInstall, a.Phase)
	assert.Equal(t, filepath.Join(h.imageDir, "giantswarm-vm-base_0.1.0.efi"), a.UKI)
	assert.Equal(t, filepath.Join(h.imageDir, "giantswarm-vm-base_0.1.0.raw"), a.Installer)
	assert.Equal(t, v.Paths.Volume, a.Target)
	assert.True(t, a.NoReboot)
	assert.Equal(t, qemu.SMBIOS{Manufacturer: "GiantSwarm", Product: "vm-manager", Serial: v.ID}, a.SMBIOS)
	assert.Equal(t, qemu.InstallTargetDevice, a.Credentials[qemu.CredentialInstallTarget])
	assert.Equal(t, "vsock-stream:2:4711", a.Credentials[qemu.CredentialNotifySocket])
	assert.Equal(t, v.MachineID, a.Credentials[qemu.CredentialMachineID])
	assert.Equal(t, "node1", a.Credentials[qemu.CredentialHostname])
	assert.Equal(t, v.SSHPublicKey+"\nssh-ed25519 AAAAuser user@laptop\n", a.Credentials[qemu.CredentialSSHAuthorizedKeysRoot])
	assert.Equal(t, []qemu.Netdev{{ID: "net0", Backend: "stream,addr.type=unix,addr.path=/run/lan/qemu.sock,reconnect-ms=1000", MAC: v.MAC}}, a.Netdevs)
	assert.Equal(t, filepath.Join(v.Paths.TPMState, "swtpm.sock"), a.TPMSocket)
	assert.Equal(t, v.Paths.OVMFVars, a.OVMFVars)
	assert.Equal(t, v.CID, a.VsockCID)
	assert.Empty(t, a.KernelCmdlineExtra)
	assert.False(t, h.notify.subscribed(v.CID), "phase A does not subscribe to notify")

	// Phase B.
	b := h.install(v.ID).spec
	v, _ = h.svc.Get(v.ID)
	assert.NotNil(t, v.InstalledAt)
	assert.NotNil(t, v.BootedAt)
	assert.Equal(t, qemu.PhaseBoot, v.Phase)
	assert.Equal(t, qemu.PhaseBoot, b.Phase)
	assert.Empty(t, b.UKI)
	assert.Empty(t, b.Installer)
	assert.False(t, b.NoReboot)
	assert.Equal(t, "ignition.firstboot", b.KernelCmdlineExtra)
	assert.Equal(t, a.Target, b.Target)
	assert.Equal(t, a.OVMFVars, b.OVMFVars)
	assert.Equal(t, a.Credentials[qemu.CredentialMachineID], b.Credentials[qemu.CredentialMachineID])
	assert.NotContains(t, b.Credentials, qemu.CredentialInstallTarget)
	assert.Equal(t, 1, h.tpm.running(), "a fresh swtpm per phase, the installer's is gone")
	assert.True(t, v.Attestation.UserDataReleased, "no attestation required: released at boot")

	v = h.ready(v.ID, v.CID)
	assert.NotNil(t, v.ReadyAt)
	assert.True(t, v.Booted)
	assert.Equal(t, "up", v.Status)

	// Stop, then start again without ignition.firstboot.
	h.ev.reset()
	v, err := h.svc.Stop(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, StateStopped, v.State)
	assert.Equal(t, []string{"qemu.stop", "tpm.stop"}, h.ev.list())
	assert.Zero(t, h.tpm.running())
	assert.False(t, h.notify.subscribed(v.CID))
	_, err = h.svc.Stop(h.ctx, v.ID)
	require.ErrorIs(t, err, apierr.ErrConflict)

	v, err = h.svc.Start(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, StateBooting, v.State)
	assert.Nil(t, v.ReadyAt)
	c := h.waitInstances(3).spec
	assert.Empty(t, c.KernelCmdlineExtra, "ignition.firstboot only on the first installed boot")
	_, err = h.svc.Start(h.ctx, v.ID)
	require.ErrorIs(t, err, apierr.ErrConflict)
	v = h.ready(v.ID, v.CID)

	// Reboot.
	v, err = h.svc.Reboot(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, StateBooting, v.State)
	h.waitInstances(4)
	h.ready(v.ID, v.CID)

	// Delete: stop, swtpm, lease, volume, directory, in that order.
	h.ev.reset()
	require.NoError(t, h.svc.Delete(h.ctx, v.ID))
	assert.Equal(t, []string{"qemu.stop", "tpm.stop", "net.detach", "storage.release", "storage.delete"}, h.ev.list())
	_, err = h.svc.Get(v.ID)
	require.ErrorIs(t, err, apierr.ErrNotFound)
	assert.Empty(t, h.store.names())
	assert.Empty(t, h.lan().Leases())
	assert.NoDirExists(t, v.Paths.Dir)
	assert.Zero(t, h.tpm.running())
	require.ErrorIs(t, h.svc.Delete(h.ctx, v.ID), apierr.ErrNotFound)
}

func TestCreateWaitForReady(t *testing.T) {
	h := newHarness(t)
	type result struct {
		v   *VM
		err error
	}
	done := make(chan result, 1)
	go func() {
		s := h.spec("wait")
		s.WaitFor = WaitReady
		v, err := h.svc.Create(h.ctx, s)
		done <- result{v, err}
	}()
	h.waitInstances(1).Exit(0)
	inst := h.waitInstances(2)
	select {
	case r := <-done:
		t.Fatalf("Create returned before READY: %+v %v", r.v, r.err)
	case <-time.After(20 * time.Millisecond):
	}
	require.Eventually(t, func() bool { return h.notify.notify(inst.spec.VsockCID, map[string]string{"READY": "1"}) }, waitAtMost, waitEvery)
	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, StateReady, r.v.State)
	case <-time.After(waitAtMost):
		t.Fatal("Create did not return after READY")
	}
}

func TestWaitForTimeoutAndFailure(t *testing.T) {
	h := newHarness(t)
	done := make(chan error, 1)
	var got *VM
	go func() {
		s := h.spec("slow")
		s.WaitFor = WaitInstalled
		var err error
		got, err = h.svc.Create(h.ctx, s)
		done <- err
	}()
	h.waitInstances(1)
	h.waitTimers(2) // supervisor install timer + waiter deadline
	h.clock.Advance(DefaultInstallTimeout + DefaultStopTimeout + time.Second)
	// Both the install timeout (kill -> failed) and the wait deadline fire;
	// either outcome must be reported with the record attached.
	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTimeout) || errors.Is(err, ErrFailed), "got %v", err)
		require.NotNil(t, got)
	case <-time.After(waitAtMost):
		t.Fatal("Create did not return")
	}
	h.waitState(got.ID, StateFailed)

	// A failure while waiting is ErrFailed with the reason.
	go func() {
		s := h.spec("broken")
		s.WaitFor = WaitReady
		got, _ = h.svc.Create(h.ctx, s)
		done <- nil
	}()
	h.waitInstances(2).Exit(7)
	<-done
	require.NotNil(t, got)
	assert.Equal(t, StateFailed, got.State)
	assert.Contains(t, got.LastError, "installer exited")
}

func TestInstallTimeoutKillsAndKeepsConsole(t *testing.T) {
	h := newHarness(t)
	v := h.create("stuck")
	var console strings.Builder
	for i := 1; i <= 50; i++ {
		console.WriteString("line " + string(rune('0'+i%10)) + "\n")
	}
	require.NoError(t, os.WriteFile(v.Paths.Console, []byte(console.String()), 0o600))
	h.waitInstances(1)
	h.waitTimers(1)
	h.clock.Advance(DefaultInstallTimeout)
	v = h.waitState(v.ID, StateFailed)
	assert.Contains(t, v.LastError, "installer did not finish within 5m0s")
	assert.Contains(t, v.LastError, "console:\n")
	assert.Equal(t, consoleTailLines, strings.Count(v.LastError, "line "), "last 40 console lines")
	assert.Contains(t, h.ev.list(), "qemu.kill")
	assert.Zero(t, h.tpm.running(), "swtpm stopped after the failure")
	assert.Len(t, h.store.names(), 1, "the disk is kept for inspection")
	_, err := h.svc.Start(h.ctx, v.ID)
	require.ErrorIs(t, err, apierr.ErrConflict, "never installed")
	require.NoError(t, h.svc.Delete(h.ctx, v.ID))
	assert.Empty(t, h.store.names())
}

func TestQEMUStartFailureLeavesNothing(t *testing.T) {
	h := newHarness(t)
	h.rt.setFailOn(func(qemu.Spec) error { return errors.New("qemu: no kvm") })
	_, err := h.svc.Create(h.ctx, h.spec("nokvm"))
	require.ErrorContains(t, err, "no kvm")
	assert.Empty(t, h.svc.List())
	assert.Zero(t, h.tpm.running(), "swtpm stopped")
	assert.Empty(t, h.store.names(), "volume deleted")
	assert.Empty(t, h.lan().Leases(), "lease released")
	dirs, _ := os.ReadDir(filepath.Join(h.stateDir, vmsDir))
	assert.Empty(t, dirs, "state dir removed")
	assert.Equal(t, []string{"tpm.start", "qemu.start:install", "tpm.stop", "net.detach", "storage.release", "storage.delete"}, h.ev.list()[len(h.ev.list())-6:])

	h.tpm.startErr = errors.New("swtpm missing")
	_, err = h.svc.Create(h.ctx, h.spec("notpm"))
	require.ErrorContains(t, err, "swtpm missing")
	assert.Empty(t, h.svc.List())
	assert.Empty(t, h.store.names())
}

func TestPhaseBStartFailure(t *testing.T) {
	h := newHarness(t)
	h.rt.setFailOn(func(s qemu.Spec) error {
		if s.Phase == qemu.PhaseBoot {
			return errors.New("boot: bad firmware")
		}
		return nil
	})
	v := h.create("b-fails")
	h.waitInstances(1).Exit(0)
	v = h.waitState(v.ID, StateFailed)
	assert.Contains(t, v.LastError, "bad firmware")
	assert.NotNil(t, v.InstalledAt)
	assert.Zero(t, h.tpm.running(), "swtpm of the failed start stopped")
	assert.False(t, h.notify.subscribed(v.CID))
	assert.Len(t, h.store.names(), 1, "the installed disk is kept")

	h.rt.setFailOn(nil)
	v, err := h.svc.Start(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, StateBooting, v.State)
	assert.Empty(t, v.LastError)
	assert.Equal(t, 1, h.tpm.running())
}

func TestUnexpectedExitAndBootTimeout(t *testing.T) {
	h := newHarness(t)
	v := h.create("flaky")
	inst := h.install(v.ID)

	// No READY within BootTimeout: degraded to running, still up.
	h.waitTimers(1)
	h.clock.Advance(DefaultBootTimeout)
	v = h.waitState(v.ID, StateRunning)
	assert.Contains(t, v.LastError, "no READY=1 within 2m0s")
	v = h.ready(v.ID, v.CID)
	assert.Empty(t, v.LastError, "a late READY clears the note")

	// Guest powers itself off.
	inst.Exit(0)
	v = h.waitState(v.ID, StateStopped)
	assert.Equal(t, "guest shut down", v.LastError)
	assert.Zero(t, h.tpm.running())

	// Crash.
	_, err := h.svc.Start(h.ctx, v.ID)
	require.NoError(t, err)
	h.waitInstances(3).Exit(1)
	v = h.waitState(v.ID, StateFailed)
	assert.Contains(t, v.LastError, "qemu exited: exit status 1")

	// Stop while still installing is allowed; the result cannot be started.
	w := h.create("half")
	_, err = h.svc.Stop(h.ctx, w.ID)
	require.NoError(t, err)
	w = h.waitState(w.ID, StateStopped)
	assert.Nil(t, w.InstalledAt)
	_, err = h.svc.Start(h.ctx, w.ID)
	require.ErrorIs(t, err, apierr.ErrConflict)
	assert.Equal(t, 4, h.rt.count(), "no phase B after a stopped installer")
}

func TestAttestationGating(t *testing.T) {
	h := newHarness(t)
	s := h.spec("attested")
	s.RequireAttestation = true
	v, err := h.svc.Create(h.ctx, s)
	require.NoError(t, err)
	h.install(v.ID)

	ip := netip.MustParseAddr(v.IP)
	inst, ok := h.svc.LookupByIP(h.ctx, ip)
	require.True(t, ok)
	assert.Equal(t, v.ID, inst.ID)
	assert.Equal(t, "attested", inst.Hostname)
	assert.Equal(t, "host1", inst.Region)
	assert.Equal(t, testNetwork, inst.Zone)
	assert.Equal(t, "1.32.0", inst.KubernetesVersion)
	assert.Equal(t, []string{v.SSHPublicKey, "ssh-ed25519 AAAAuser user@laptop"}, inst.SSHAuthorizedKeys)
	assert.Equal(t, map[string]string{"role": "control-plane"}, inst.Metadata)
	assert.JSONEq(t, `{"ignition":{"version":"3.4.0"}}`, string(inst.UserData))
	assert.False(t, inst.UserDataReleased, "gated until the initrd quote verifies")
	_, ok = h.svc.LookupByIP(h.ctx, netip.MustParseAddr("127.0.0.99"))
	assert.False(t, ok)

	// Through the real handler on the network's IMDS listener, from the
	// VM's lease address.
	client := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(v.IP)}}).DialContext}}
	get := func(key string) (int, string) {
		resp, err := client.Get("http://" + h.lan().imdsAddr + imds.BasePath + key)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	code, body := get("/hostname")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "attested", body)
	code, _ = get("/user-data")
	assert.Equal(t, http.StatusForbidden, code)

	att := h.svc.IMDSDeps().Attestor
	nonce, err := att.Nonce(h.ctx, v.ID)
	require.NoError(t, err)
	v = h.waitState(v.ID, StateAttesting)
	assert.NotNil(t, v.Attestation.NonceIssuedAt)

	// A ready-stage quote never releases user-data.
	res, err := att.SubmitQuote(h.ctx, v.ID, imds.QuoteRequest{Stage: imds.StageReady, Nonce: nonce})
	require.NoError(t, err)
	assert.True(t, res.Verified)
	assert.False(t, res.UserDataReleased)
	a, err := h.svc.Attestation(v.ID)
	require.NoError(t, err)
	assert.False(t, a.UserDataReleased)
	require.NotNil(t, a.Ready)
	assert.True(t, a.Ready.Verified)

	nonce, err = att.Nonce(h.ctx, v.ID)
	require.NoError(t, err)
	res, err = att.SubmitQuote(h.ctx, v.ID, imds.QuoteRequest{Stage: imds.StageInitrd, Nonce: nonce})
	require.NoError(t, err)
	assert.True(t, res.Verified)
	assert.True(t, res.UserDataReleased)
	a, _ = h.svc.Attestation(v.ID)
	assert.True(t, a.UserDataReleased)
	require.NotNil(t, a.Initrd)
	code, body = get("/user-data")
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, `{"ignition":{"version":"3.4.0"}}`, body)

	v = h.ready(v.ID, v.CID)
	assert.True(t, v.Attestation.UserDataReleased)

	// A restart gates user-data again.
	_, err = h.svc.Reboot(h.ctx, v.ID)
	require.NoError(t, err)
	v, _ = h.svc.Get(v.ID)
	assert.False(t, v.Attestation.UserDataReleased)
	assert.Nil(t, v.Attestation.Initrd)
	assert.Equal(t, StateBooting, v.State)

	// Reports land in memory and on disk.
	require.NoError(t, h.svc.StoreReport(h.ctx, v.ID, json.RawMessage(`{"cpu":1}`)))
	rep, err := h.svc.Report(v.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"cpu":1}`, string(rep))
	assert.FileExists(t, v.Paths.Report)
}

func TestPersistenceRoundTripAndLoad(t *testing.T) {
	h := newHarness(t)
	v := h.create("persist")
	h.install(v.ID)
	before := h.ready(v.ID, v.CID)
	require.NoError(t, h.svc.StoreReport(h.ctx, v.ID, json.RawMessage(`{"a":1}`)))

	// The record on disk is the snapshot.
	var onDisk VM
	data, err := os.ReadFile(filepath.Join(v.Paths.Dir, recordFile))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &onDisk))
	assert.Equal(t, *before, onDisk)
	var states []network.State
	data, err = os.ReadFile(filepath.Join(h.stateDir, networksFile))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &states))
	require.Len(t, states, 1)
	assert.True(t, states[0].Spec.EnableIMDS)
	assert.Equal(t, map[string]string{v.ID: v.IP}, states[0].Leases)

	// A new process on the same state dir: the QEMU is gone with the old
	// one, the network comes back with its leases.
	require.NoError(t, h.svc.Close(h.ctx))
	// Close stopped the VM cleanly; put the ready record back to simulate a
	// crash that left QEMU recorded as running.
	require.NoError(t, writeJSON(filepath.Join(v.Paths.Dir, recordFile), onDisk))
	h.nets = &fakeNetworks{ev: h.ev}
	h.start()
	require.Len(t, h.nets.restored, 1)
	assert.Equal(t, states, h.nets.restored[0])
	nets := h.svc.ListNetworks()
	require.Len(t, nets, 1)
	assert.Equal(t, v.IP, nets[0].Leases[0].IP.String())

	after, err := h.svc.Get(v.ID)
	require.NoError(t, err)
	assert.Equal(t, StateStopped, after.State)
	assert.Contains(t, after.LastError, "vm-manager restarted while the VM was ready")
	assert.Equal(t, before.IP, after.IP)
	assert.Equal(t, before.MAC, after.MAC)
	assert.Equal(t, before.CID, after.CID)
	assert.Equal(t, before.MachineID, after.MachineID)
	assert.Equal(t, before.InstalledAt, after.InstalledAt)
	assert.True(t, after.Booted)
	rep, _ := h.svc.Report(v.ID)
	assert.JSONEq(t, `{"a":1}`, string(rep))
	inst, ok := h.svc.LookupByIP(h.ctx, netip.MustParseAddr(v.IP))
	require.True(t, ok)
	assert.JSONEq(t, `{"ignition":{"version":"3.4.0"}}`, string(inst.UserData), "user-data reloaded")

	// Start reopens the volume and boots phase B without ignition.firstboot.
	h.ev.reset()
	after, err = h.svc.Start(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, StateBooting, after.State)
	assert.Contains(t, h.ev.list(), "storage.acquire:open")
	assert.Empty(t, h.waitInstances(3).spec.KernelCmdlineExtra)
	assert.Equal(t, uint32(qemu.MinCID+1), h.create("second").CID, "CID allocation skips the restored VM")
}

func TestLoadMarksInterruptedRecords(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.svc.Close(h.ctx))
	for id, st := range map[string]State{"aaaa0001": StateCreating, "aaaa0002": StateDeleting, "aaaa0003": StateFailed} {
		dir := filepath.Join(h.stateDir, vmsDir, id)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		rec := VM{ID: id, Spec: Spec{Name: "n" + id, Network: testNetwork}, State: st, CID: 3}
		data, _ := json.Marshal(rec)
		require.NoError(t, os.WriteFile(filepath.Join(dir, recordFile), data, 0o600))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(h.stateDir, vmsDir, "garbage"), 0o700))
	h.nets = &fakeNetworks{ev: h.ev}
	h.start()
	list := h.svc.List()
	require.Len(t, list, 3)
	for _, v := range list {
		assert.Equal(t, StateFailed, v.State, v.ID)
		if v.ID != "aaaa0003" {
			assert.Contains(t, v.LastError, "vm-manager restarted while the VM was")
		}
	}
}

func TestNetworks(t *testing.T) {
	h := newHarness(t)
	n, err := h.svc.GetNetwork(testNetwork)
	require.NoError(t, err)
	assert.True(t, n.Spec.EnableIMDS, "IMDS is forced on")
	assert.Equal(t, "127.0.0.1", n.Gateway)
	_, err = h.svc.GetNetwork("nope")
	require.ErrorIs(t, err, apierr.ErrNotFound)

	_, err = h.svc.CreateNetwork(h.ctx, network.Spec{Name: "alpha", CIDR: "127.0.1.0/24"})
	require.NoError(t, err)
	_, err = h.svc.CreateNetwork(h.ctx, network.Spec{Name: "alpha", CIDR: "127.0.1.0/24"})
	require.ErrorIs(t, err, apierr.ErrConflict)
	names := []string{}
	for _, n := range h.svc.ListNetworks() {
		names = append(names, n.Spec.Name)
	}
	assert.Equal(t, []string{"alpha", testNetwork}, names)
	assert.NotEmpty(t, h.lan().imdsAddr, "IMDS served")

	v := h.create("user")
	err = h.svc.DeleteNetwork(h.ctx, testNetwork)
	require.ErrorIs(t, err, apierr.ErrConflict)
	assert.Contains(t, err.Error(), "user")
	require.NoError(t, h.svc.Delete(h.ctx, v.ID))
	require.NoError(t, h.svc.DeleteNetwork(h.ctx, testNetwork))
	require.NoError(t, h.svc.DeleteNetwork(h.ctx, "alpha"))
	require.ErrorIs(t, h.svc.DeleteNetwork(h.ctx, "alpha"), apierr.ErrNotFound)
	data, err := os.ReadFile(filepath.Join(h.stateDir, networksFile))
	require.NoError(t, err)
	assert.JSONEq(t, "[]", string(data))
}

func TestConsoleAndForward(t *testing.T) {
	h := newHarness(t)
	v := h.create("console")
	out, err := h.svc.Console(v.ID, 10)
	require.NoError(t, err)
	assert.Empty(t, out, "no console yet")
	require.NoError(t, os.WriteFile(v.Paths.Console, []byte("one\ntwo\nthree\nfour\n"), 0o600))
	out, err = h.svc.Console(v.ID, 2)
	require.NoError(t, err)
	assert.Equal(t, "three\nfour", out)
	out, err = h.svc.Console(v.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\nthree\nfour", out)
	_, err = h.svc.Console("nope", 1)
	require.ErrorIs(t, err, apierr.ErrNotFound)

	addr, err := h.svc.Forward(h.ctx, v.ID, 6443)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(addr, "127.0.0.1:"), addr)
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	_ = conn.Close()
	_, err = h.svc.Forward(h.ctx, v.ID, 0)
	require.ErrorIs(t, err, apierr.ErrInvalid)

	require.NoError(t, h.svc.Delete(h.ctx, v.ID))
	_, err = net.Dial("tcp", addr)
	require.Error(t, err, "forward closed with the VM")
}

// sshServer serves one exec per connection over a loopback pair, accepting
// the authorized key only.
func sshServer(t *testing.T, authorized string, hostKey ssh.Signer, run func(cmd string) (string, uint32)) func(context.Context, string) (net.Conn, error) {
	t.Helper()
	allowed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	require.NoError(t, err)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() == "root" && bytes.Equal(key.Marshal(), allowed.Marshal()) {
			return &ssh.Permissions{}, nil
		}
		return nil, errors.New("denied")
	}}
	cfg.AddHostKey(hostKey)
	serve := func(c net.Conn) {
		defer func() { _ = c.Close() }()
		sconn, chans, reqs, err := ssh.NewServerConn(c, cfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for nc := range chans {
			ch, requests, err := nc.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = ch.Close() }()
				for req := range requests {
					if req.Type != "exec" {
						_ = req.Reply(false, nil)
						continue
					}
					var payload struct{ Command string }
					_ = ssh.Unmarshal(req.Payload, &payload)
					_ = req.Reply(true, nil)
					out, code := run(payload.Command)
					_, _ = io.WriteString(ch, out)
					_, _ = io.WriteString(ch.Stderr(), "warn\n")
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
					return
				}
			}()
		}
		_ = sconn.Wait()
	}
	return func(_ context.Context, _ string) (net.Conn, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		defer func() { _ = ln.Close() }()
		go func() {
			if c, err := ln.Accept(); err == nil {
				serve(c)
			}
		}()
		return net.Dial("tcp", ln.Addr().String())
	}
}

func TestExecOverSSH(t *testing.T) {
	h := newHarness(t)
	v := h.create("ssh")
	_, err := h.svc.Exec(h.ctx, v.ID, nil)
	require.ErrorIs(t, err, apierr.ErrInvalid)

	hostKey := newSigner(t)
	var got string
	h.lan().dial = sshServer(t, v.SSHPublicKey, hostKey, func(cmd string) (string, uint32) {
		got = cmd
		return "hello\n", 3
	})
	res, err := h.svc.Exec(h.ctx, v.ID, []string{"echo", "it's here", "$HOME"})
	require.NoError(t, err)
	assert.Equal(t, `'echo' 'it'\''s here' '$HOME'`, got)
	assert.Equal(t, ExecResult{Stdout: "hello\n", Stderr: "warn\n", ExitCode: 3}, res)
	pinned, err := os.ReadFile(filepath.Join(v.Paths.Dir, hostKeyFile))
	require.NoError(t, err)
	assert.Equal(t, ssh.MarshalAuthorizedKey(hostKey.PublicKey()), pinned)

	// The same host key is accepted again, another one is not.
	res, err = h.svc.Exec(h.ctx, v.ID, []string{"true"})
	require.NoError(t, err)
	assert.Equal(t, 3, res.ExitCode)
	h.lan().dial = sshServer(t, v.SSHPublicKey, newSigner(t), func(string) (string, uint32) { return "", 0 })
	_, err = h.svc.Exec(h.ctx, v.ID, []string{"true"})
	require.ErrorContains(t, err, "host key of vm")

	// A key that is not vm-manager's is rejected by the guest.
	h.lan().dial = sshServer(t, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGb1vUmB8Uf9DR6vHCEwe8aSU2LLyOc0vHwwmN9Gpz1Y other", hostKey, nil)
	_, err = h.svc.Exec(h.ctx, v.ID, []string{"true"})
	require.ErrorContains(t, err, "ssh ")

	_, err = h.svc.Stop(h.ctx, v.ID)
	require.NoError(t, err)
	_, err = h.svc.Exec(h.ctx, v.ID, []string{"true"})
	require.ErrorIs(t, err, apierr.ErrConflict, "not running")
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	dir := t.TempDir()
	_, err := generateSSHKey(filepath.Join(dir, "key"), "test")
	require.NoError(t, err)
	signer, err := loadSigner(filepath.Join(dir, "key"))
	require.NoError(t, err)
	return signer
}

func TestOptionsDefaults(t *testing.T) {
	_, err := New(Options{})
	require.ErrorContains(t, err, "Images, Networks, Notify, Runtime, StateDir, Storage, TPM")
	code, vars := FindOVMF()
	assert.NotEmpty(t, code)
	assert.NotEmpty(t, vars)
}
