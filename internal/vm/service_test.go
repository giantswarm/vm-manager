package vm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/giantswarm/vm-manager/internal/tpm"
	"github.com/giantswarm/vm-manager/internal/vm"
	"github.com/giantswarm/vm-manager/internal/vm/vmtest"
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
	svc      *vm.Service
	ev       *vmtest.Events
	clock    *vmtest.Clock
	rt       *vmtest.Runtime
	tpm      *vmtest.TPM
	store    *vmtest.Storage
	nets     *vmtest.Networks
	notify   *vmtest.Notifier
	stateDir string
	imageDir string
	imgs     *vmtest.Images
	metrics  *metricsRecorder
	// attestor, when set, is the Options.Attestor of the next start.
	attestor imds.Attestor
	// detach is the Options.DetachOnClose of the next start.
	detach bool
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// metricsRecorder is the harness' vm.Metrics: it keeps what the service
// reports so the tests can assert the hooks fire on the right transitions.
type metricsRecorder struct {
	mu        sync.Mutex
	installs  []time.Duration
	boots     []time.Duration
	forgotten []string
	reports   map[string]json.RawMessage
}

func (m *metricsRecorder) StoreReport(_ context.Context, id string, raw json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reports == nil {
		m.reports = make(map[string]json.RawMessage)
	}
	m.reports[id] = raw
	return nil
}

func (m *metricsRecorder) ObserveInstall(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.installs = append(m.installs, d)
}

func (m *metricsRecorder) ObserveBootToReady(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.boots = append(m.boots, d)
}

func (m *metricsRecorder) ForgetVM(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forgotten = append(m.forgotten, id)
}

func (m *metricsRecorder) snapshot() (installs, boots []time.Duration, forgotten []string, reports map[string]json.RawMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Duration(nil), m.installs...), append([]time.Duration(nil), m.boots...),
		append([]string(nil), m.forgotten...), m.reports
}

// newHarness wires a vm.Service to fakes with one network "lan".
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, ctx: context.Background(), stateDir: t.TempDir(), imageDir: t.TempDir(), metrics: &metricsRecorder{}}
	d := vmtest.NewDeps(t.TempDir(), time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC))
	h.ev, h.clock, h.rt, h.tpm, h.store, h.nets, h.notify = d.Events, d.Clock, d.Runtime, d.TPM, d.Storage, d.Networks, d.Notifier
	for _, f := range []string{"giantswarm-vm-base_0.1.0.efi", "giantswarm-vm-base_0.1.0.raw", "OVMF_CODE.fd", "OVMF_VARS.fd"} {
		require.NoError(t, os.WriteFile(filepath.Join(h.imageDir, f), []byte(f), 0o600))
	}
	h.imgs = vmtest.NewImages(h.imageDir, images.Image{
		ID: "giantswarm-vm-base", Version: "0.1.0",
		UKI:                filepath.Join(h.imageDir, "giantswarm-vm-base_0.1.0.efi"),
		Disk:               filepath.Join(h.imageDir, "giantswarm-vm-base_0.1.0.raw"),
		KubernetesVersions: []string{"1.32.0", "1.31.2"},
	})
	h.start()
	_, err := h.svc.CreateNetwork(h.ctx, network.Spec{Name: testNetwork, CIDR: "127.0.0.0/24"})
	require.NoError(t, err)
	return h
}

// start builds the vm.Service (again) on the harness' state and fakes.
func (h *harness) start() {
	h.t.Helper()
	svc, err := vm.New(vm.Options{
		StateDir:         h.stateDir,
		Images:           h.imgs,
		Storage:          h.store,
		TPM:              h.tpm,
		Runtime:          h.rt,
		Networks:         h.nets,
		Notify:           h.notify,
		Attestor:         h.attestor,
		DetachOnClose:    h.detach,
		OVMFCode:         filepath.Join(h.imageDir, "OVMF_CODE.fd"),
		OVMFVarsTemplate: filepath.Join(h.imageDir, "OVMF_VARS.fd"),
		Region:           "host1",
		Logger:           quiet(),
		Clock:            h.clock,
		Metrics:          h.metrics,
	})
	require.NoError(h.t, err)
	require.NoError(h.t, svc.Load(h.ctx))
	h.svc = svc
	h.t.Cleanup(func() { require.NoError(h.t, svc.Close(context.Background())) })
}

func (h *harness) spec(name string) vm.Spec {
	return vm.Spec{Name: name, CPUs: 2, MemoryMiB: 2048, DiskGiB: 10, Network: testNetwork,
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`), SSHAuthorizedKeys: []string{"ssh-ed25519 AAAAuser user@laptop"},
		Metadata: map[string]string{"role": "control-plane"}}
}

func (h *harness) create(name string) *vm.VM {
	h.t.Helper()
	v, err := h.svc.Create(h.ctx, h.spec(name))
	require.NoError(h.t, err)
	return v
}

// waitState polls until the VM is in state want. A plain loop rather than
// Eventually so the failure can name the state it last saw: Eventually's
// message arguments are evaluated before the wait.
func (h *harness) waitState(id string, want vm.State) *vm.VM {
	h.t.Helper()
	deadline := time.Now().Add(waitAtMost)
	for {
		v, err := h.svc.Get(id)
		if err == nil && v.State == want {
			return v
		}
		if time.Now().After(deadline) {
			if err != nil {
				h.t.Fatalf("vm %s did not reach %s: %v", id, want, err)
			}
			h.t.Fatalf("vm %s did not reach %s: state %s, last error %q", id, want, v.State, v.LastError)
		}
		time.Sleep(waitEvery)
	}
}

func (h *harness) waitInstances(n int) *vmtest.Instance {
	h.t.Helper()
	require.Eventually(h.t, func() bool { return h.rt.Count() >= n }, waitAtMost, waitEvery, "waiting for qemu start #%d", n)
	return h.rt.At(n - 1)
}

// waitTimers blocks until n timers are armed on the fake clock, so that an
// Advance past a timeout reaches the supervisor that set it up.
func (h *harness) waitTimers(n int) {
	h.t.Helper()
	require.Eventually(h.t, func() bool { return h.clock.Timers() >= n }, waitAtMost, waitEvery, "waiting for %d timers", n)
}

// install ends the installer with exit 0 and waits for phase B.
func (h *harness) install(id string) *vmtest.Instance {
	h.t.Helper()
	h.waitInstances(1).Exit(0)
	h.waitState(id, vm.StateBooting)
	return h.waitInstances(2)
}

func (h *harness) ready(id string, cid uint32) *vm.VM {
	h.t.Helper()
	require.Eventually(h.t, func() bool { return h.notify.Notify(cid, map[string]string{"READY": "1", "STATUS": "up"}) }, waitAtMost, waitEvery)
	return h.waitState(id, vm.StateReady)
}

func (h *harness) lan() *vmtest.Network {
	n, err := h.nets.Get(testNetwork)
	require.NoError(h.t, err)
	return n.(*vmtest.Network)
}

func (h *harness) lockOp(id string) func() {
	h.t.Helper()
	unlock, err := h.svc.LockOp(id)
	require.NoError(h.t, err)
	return unlock
}

// closeAsync starts Close and returns once it has begun (the closing flag is
// set); the result arrives on the returned channel.
func (h *harness) closeAsync() <-chan error {
	h.t.Helper()
	done := make(chan error, 1)
	go func() { done <- h.svc.Close(h.ctx) }()
	require.Eventually(h.t, func() bool {
		return h.svc.Closing()
	}, waitAtMost, waitEvery, "Close did not begin")
	return done
}

// awaitClose fails when Close does not return in time. Close waits for every
// supervisor, so its return proves no goroutine is left behind.
func (h *harness) awaitClose(done <-chan error) {
	h.t.Helper()
	select {
	case err := <-done:
		require.NoError(h.t, err)
	case <-time.After(waitAtMost):
		// End whatever escaped so the Cleanup Close cannot hang as well.
		for i := 0; i < h.rt.Count(); i++ {
			h.rt.At(i).Exit(0)
		}
		h.t.Fatalf("Close did not return within %s: a Process escaped it", waitAtMost)
	}
}

func TestCreateValidation(t *testing.T) {
	h := newHarness(t)
	ok := h.spec("good")
	tests := []struct {
		name string
		mod  func(*vm.Spec)
		err  error
	}{
		{"bad name", func(s *vm.Spec) { s.Name = "Bad_Name" }, apierr.ErrInvalid},
		{"no cpus", func(s *vm.Spec) { s.CPUs = 0 }, apierr.ErrInvalid},
		{"tiny memory", func(s *vm.Spec) { s.MemoryMiB = 64 }, apierr.ErrInvalid},
		{"no disk", func(s *vm.Spec) { s.DiskGiB = 0 }, apierr.ErrInvalid},
		{"no network", func(s *vm.Spec) { s.Network = "" }, apierr.ErrInvalid},
		{"unknown network", func(s *vm.Spec) { s.Network = "nope" }, apierr.ErrInvalid},
		{"unknown image", func(s *vm.Spec) { s.Image = "nope" }, apierr.ErrInvalid},
		{"unknown kubernetes version", func(s *vm.Spec) { s.KubernetesVersion = "9.9.9" }, apierr.ErrInvalid},
		{"bad waitFor", func(s *vm.Spec) { s.WaitFor = "soon" }, apierr.ErrInvalid},
		{"bad metadata key", func(s *vm.Spec) { s.Metadata = map[string]string{"a/b": "c"} }, apierr.ErrInvalid},
		{"multiline ssh key", func(s *vm.Spec) { s.SSHAuthorizedKeys = []string{"a\nb"} }, apierr.ErrInvalid},
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
	assert.Zero(t, h.rt.Count())

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
	require.Equal(t, vm.StateInstalling, v.State)
	assert.Equal(t, qemu.PhaseInstall, v.Phase)
	assert.GreaterOrEqual(t, v.CID, uint32(qemu.MinCID), "CID is in the vsock range")
	assert.Less(t, v.CID, uint32(qemu.MaxCID), "CID is in the vsock range")
	assert.Equal(t, "127.0.0.2", v.IP)
	assert.Len(t, v.MachineID, 32)
	assert.Contains(t, v.SSHPublicKey, "ssh-ed25519 ")
	assert.Equal(t, []string{"vm-" + v.ID + storage.VolumeSuffix}, h.store.Names())
	assert.FileExists(t, v.Paths.OVMFVars)
	assert.FileExists(t, v.Paths.SSHKey)
	assert.FileExists(t, v.Paths.UserData)
	assert.Equal(t, 1, h.tpm.Running())

	// Phase A spec.
	a := h.waitInstances(1).Spec()
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
	assert.False(t, h.notify.Subscribed(v.CID), "phase A does not subscribe to notify")

	// Phase B.
	b := h.install(v.ID).Spec()
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
	assert.Equal(t, 1, h.tpm.Running(), "a fresh swtpm per phase, the installer's is gone")
	assert.True(t, v.Attestation.UserDataReleased, "no attestation required: released at boot")

	v = h.ready(v.ID, v.CID)
	assert.NotNil(t, v.ReadyAt)
	assert.True(t, v.Booted)
	assert.Equal(t, "up", v.Status)

	// Stop, then start again without ignition.firstboot.
	h.ev.Reset()
	v, err := h.svc.Stop(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, vm.StateStopped, v.State)
	assert.Equal(t, []string{"qemu.stop", "tpm.stop"}, h.ev.List())
	assert.Zero(t, h.tpm.Running())
	assert.False(t, h.notify.Subscribed(v.CID))
	_, err = h.svc.Stop(h.ctx, v.ID)
	require.ErrorIs(t, err, apierr.ErrConflict)

	v, err = h.svc.Start(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, vm.StateBooting, v.State)
	assert.Nil(t, v.ReadyAt)
	c := h.waitInstances(3).Spec()
	assert.Empty(t, c.KernelCmdlineExtra, "ignition.firstboot only on the first installed boot")
	_, err = h.svc.Start(h.ctx, v.ID)
	require.ErrorIs(t, err, apierr.ErrConflict)
	v = h.ready(v.ID, v.CID)

	// Reboot.
	v, err = h.svc.Reboot(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, vm.StateBooting, v.State)
	h.waitInstances(4)
	h.ready(v.ID, v.CID)

	// Delete: stop, swtpm, lease, volume, directory, in that order.
	h.ev.Reset()
	require.NoError(t, h.svc.Delete(h.ctx, v.ID))
	assert.Equal(t, []string{"qemu.stop", "tpm.stop", "net.detach", "storage.release", "storage.delete"}, h.ev.List())
	_, err = h.svc.Get(v.ID)
	require.ErrorIs(t, err, apierr.ErrNotFound)
	assert.Empty(t, h.store.Names())
	assert.Empty(t, h.lan().Leases())
	assert.NoDirExists(t, v.Paths.Dir)
	assert.Zero(t, h.tpm.Running())
	require.ErrorIs(t, h.svc.Delete(h.ctx, v.ID), apierr.ErrNotFound)

	// The metrics hooks saw one install, every READY=1 and the deletion.
	installs, boots, forgotten, _ := h.metrics.snapshot()
	assert.Len(t, installs, 1)
	assert.Len(t, boots, 3, "first boot, start and reboot each reached READY=1")
	assert.Equal(t, []string{v.ID}, forgotten)
}

func TestCreateWaitForReady(t *testing.T) {
	h := newHarness(t)
	type result struct {
		v   *vm.VM
		err error
	}
	done := make(chan result, 1)
	go func() {
		s := h.spec("wait")
		s.WaitFor = vm.WaitReady
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
	require.Eventually(t, func() bool { return h.notify.Notify(inst.Spec().VsockCID, map[string]string{"READY": "1"}) }, waitAtMost, waitEvery)
	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, vm.StateReady, r.v.State)
	case <-time.After(waitAtMost):
		t.Fatal("Create did not return after READY")
	}
}

func TestWaitForTimeoutAndFailure(t *testing.T) {
	h := newHarness(t)
	done := make(chan error, 1)
	var got *vm.VM
	go func() {
		s := h.spec("slow")
		s.WaitFor = vm.WaitInstalled
		var err error
		got, err = h.svc.Create(h.ctx, s)
		done <- err
	}()
	h.waitInstances(1)
	h.waitTimers(2) // supervisor install timer + waiter deadline
	h.clock.Advance(vm.DefaultInstallTimeout + vm.DefaultStopTimeout + time.Second)
	// Both the install timeout (kill -> failed) and the wait deadline fire;
	// either outcome must be reported with the record attached.
	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, errors.Is(err, vm.ErrTimeout) || errors.Is(err, vm.ErrFailed), "got %v", err)
		require.NotNil(t, got)
	case <-time.After(waitAtMost):
		t.Fatal("Create did not return")
	}
	h.waitState(got.ID, vm.StateFailed)

	// A failure while waiting is vm.ErrFailed with the reason.
	go func() {
		s := h.spec("broken")
		s.WaitFor = vm.WaitReady
		got, _ = h.svc.Create(h.ctx, s)
		done <- nil
	}()
	h.waitInstances(2).Exit(7)
	<-done
	require.NotNil(t, got)
	assert.Equal(t, vm.StateFailed, got.State)
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
	h.clock.Advance(vm.DefaultInstallTimeout)
	v = h.waitState(v.ID, vm.StateFailed)
	assert.Contains(t, v.LastError, "installer did not finish within 5m0s")
	assert.Contains(t, v.LastError, "console:\n")
	assert.Equal(t, vm.ConsoleTailLines, strings.Count(v.LastError, "line "), "last 40 console lines")
	assert.Contains(t, h.ev.List(), "qemu.kill")
	assert.Zero(t, h.tpm.Running(), "swtpm stopped after the failure")
	assert.Len(t, h.store.Names(), 1, "the disk is kept for inspection")
	_, err := h.svc.Start(h.ctx, v.ID)
	require.ErrorIs(t, err, apierr.ErrConflict, "never installed")
	require.NoError(t, h.svc.Delete(h.ctx, v.ID))
	assert.Empty(t, h.store.Names())
}

// TestDeleteDuringInstall deletes a VM while its installer runs: the process
// is stopped and its exit awaited before anything else is released, no
// installed boot follows, and nothing of the VM remains.
func TestDeleteDuringInstall(t *testing.T) {
	h := newHarness(t)
	v := h.create("half-installed")
	h.waitInstances(1)
	h.waitTimers(1)
	require.Equal(t, 1, h.tpm.Running())
	h.ev.Reset()

	require.NoError(t, h.svc.Delete(h.ctx, v.ID))

	assert.Equal(t, []string{"qemu.stop", "tpm.stop", "net.detach", "storage.release", "storage.delete"}, h.ev.List(),
		"installer stopped and gone (swtpm stopped by its supervisor) before lease, volume and disk are released")
	assert.Equal(t, 1, h.rt.Count(), "the installer's exit did not start phase B")
	assert.Zero(t, h.tpm.Running())
	assert.Empty(t, h.store.Names())
	assert.Empty(t, h.lan().Leases())
	assert.NoDirExists(t, v.Paths.Dir)
	assert.Empty(t, h.svc.List())
	_, err := h.svc.Get(v.ID)
	require.ErrorIs(t, err, apierr.ErrNotFound)

	// The install timeout fires into nothing: no supervisor is left.
	h.clock.Advance(vm.DefaultInstallTimeout)
	h.awaitClose(h.closeAsync())
	assert.Equal(t, 1, h.rt.Count())
	assert.NotContains(t, h.ev.List(), "qemu.kill")
}

// TestCloseDuringInstallHandoff drives Close into the install-to-boot
// handoff. Once Close has begun, phase B is not started (pending handoff) or
// is stopped by Close like any other process (start already in flight), and
// Close returns.
func TestCloseDuringInstallHandoff(t *testing.T) {
	t.Run("handoff pending", func(t *testing.T) {
		h := newHarness(t)
		v := h.create("pending")
		inst := h.waitInstances(1)
		// Hold the handoff's lock: the installer's exit settles (installed,
		// no process) but boot() cannot start phase B before we let go.
		unlock := h.lockOp(v.ID)
		inst.Exit(0)
		require.Eventually(t, func() bool {
			v, err := h.svc.Get(v.ID)
			return err == nil && v.InstalledAt != nil
		}, waitAtMost, waitEvery, "installer exit not settled")
		assert.Zero(t, h.tpm.Running())

		done := h.closeAsync()
		unlock()
		h.awaitClose(done)

		assert.Equal(t, 1, h.rt.Count(), "phase B was not started once Close had begun")
		v, err := h.svc.Get(v.ID)
		require.NoError(t, err)
		assert.Equal(t, vm.StateStopped, v.State)
		assert.NotNil(t, v.InstalledAt, "the record stays startable")
		assert.Contains(t, v.LastError, "shut down before the installed boot started")
		_, err = h.svc.Start(h.ctx, v.ID)
		require.ErrorIs(t, err, apierr.ErrConflict, "no start after Close")
		_, err = h.svc.Create(h.ctx, h.spec("late"))
		require.ErrorIs(t, err, apierr.ErrConflict, "no create after Close")
	})

	t.Run("handoff in flight", func(t *testing.T) {
		h := newHarness(t)
		v := h.create("inflight")
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		h.rt.SetHold(func(spec qemu.Spec) {
			if spec.Phase == qemu.PhaseBoot {
				once.Do(func() { close(entered) })
				<-release
			}
		})
		h.waitInstances(1).Exit(0)
		select {
		case <-entered:
		case <-time.After(waitAtMost):
			t.Fatal("phase B start not Reached")
		}

		done := h.closeAsync()
		close(release)
		h.awaitClose(done)

		assert.Equal(t, 2, h.rt.Count(), "the start in flight completed")
		ev := h.ev.List()
		assert.Greater(t, lastIndex(ev, "qemu.stop"), lastIndex(ev, "qemu.start:boot"), "Close stopped the phase B it let start")
		v, err := h.svc.Get(v.ID)
		require.NoError(t, err)
		assert.Equal(t, vm.StateStopped, v.State)
		assert.Zero(t, h.tpm.Running())
		assert.False(t, h.notify.Subscribed(v.CID))
	})
}

func lastIndex(list []string, s string) int {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == s {
			return i
		}
	}
	return -1
}

func TestQEMUStartFailureLeavesNothing(t *testing.T) {
	h := newHarness(t)
	h.rt.SetFailOn(func(qemu.Spec) error { return errors.New("qemu: no kvm") })
	_, err := h.svc.Create(h.ctx, h.spec("nokvm"))
	require.ErrorContains(t, err, "no kvm")
	assert.Empty(t, h.svc.List())
	assert.Zero(t, h.tpm.Running(), "swtpm stopped")
	assert.Empty(t, h.store.Names(), "volume deleted")
	assert.Empty(t, h.lan().Leases(), "lease released")
	dirs, _ := os.ReadDir(filepath.Join(h.stateDir, vm.VMsDir))
	assert.Empty(t, dirs, "state dir removed")
	assert.Equal(t, []string{"tpm.start", "qemu.start:install", "tpm.stop", "net.detach", "storage.release", "storage.delete"}, h.ev.List()[len(h.ev.List())-6:])

	h.tpm.StartErr = errors.New("swtpm missing")
	_, err = h.svc.Create(h.ctx, h.spec("notpm"))
	require.ErrorContains(t, err, "swtpm missing")
	assert.Empty(t, h.svc.List())
	assert.Empty(t, h.store.Names())
}

// TestUnresponsiveVTPM: a vTPM that stops answering is named in LastError,
// from the console during a run and from swtpm itself at a start.
func TestUnresponsiveVTPM(t *testing.T) {
	t.Run("firmware TPM calls fail, installer times out", func(t *testing.T) {
		h := newHarness(t)
		v := h.create("vtpm-stall")
		console := "BdsDxe: loading Boot0001\nEFI stub: WARNING: Failed to measure data for event 1: 0x8000000000000007\nWelcome\n"
		require.NoError(t, os.WriteFile(v.Paths.Console, []byte(console), 0o600))
		h.waitInstances(1)
		h.waitTimers(1)
		h.clock.Advance(vm.DefaultInstallTimeout)
		v = h.waitState(v.ID, vm.StateFailed)
		assert.True(t, strings.HasPrefix(v.LastError, "vtpm not responding: the guest's TPM calls failed (console: EFI stub: WARNING: Failed to measure data for event 1: 0x8000000000000007); installer did not finish within 5m0s\nconsole:\n"), v.LastError)
	})
	t.Run("healthy console keeps the plain message", func(t *testing.T) {
		h := newHarness(t)
		v := h.create("no-stall")
		require.NoError(t, os.WriteFile(v.Paths.Console, []byte("tpm_crb MSFT0101:00: ready\n"), 0o600))
		h.waitInstances(1).Exit(3)
		v = h.waitState(v.ID, vm.StateFailed)
		assert.True(t, strings.HasPrefix(v.LastError, "installer exited: exit status 3"), v.LastError)
	})
	t.Run("swtpm does not answer at the installed boot", func(t *testing.T) {
		h := newHarness(t)
		v := h.create("vtpm-silent")
		inst := h.waitInstances(1)
		h.tpm.StartErr = fmt.Errorf("%w: swtpm control socket did not answer within 5s", tpm.ErrUnresponsive)
		inst.Exit(0)
		v = h.waitState(v.ID, vm.StateFailed)
		assert.Contains(t, v.LastError, "start installed boot: start swtpm: vtpm not responding")
	})
}

// rejectingAttestor is the verifier refusing a quote whose nonce matched:
// with reason set, every such quote is rejected with it.
type rejectingAttestor struct {
	imds.NoopAttestor
	reason string
}

func (a *rejectingAttestor) SubmitQuote(ctx context.Context, vmID string, req imds.QuoteRequest) (imds.QuoteResult, error) {
	res, err := a.NoopAttestor.SubmitQuote(ctx, vmID, req)
	if err != nil || !res.Verified || a.reason == "" {
		return res, err
	}
	return imds.QuoteResult{Message: a.reason}, nil
}

// TestAttestationRejected: a rejected quote is LastError through READY=1,
// behind the vTPM stall when the run's console shows one.
func TestAttestationRejected(t *testing.T) {
	const mismatch = "pcr 11 mismatch for phase enter-initrd: expected c6bc2f42, got 00000000"
	boot := func(t *testing.T, console string) (*harness, *rejectingAttestor, *vm.VM) {
		h := newHarness(t)
		att := &rejectingAttestor{reason: mismatch}
		h.attestor = att
		h.restart()
		s := h.spec("attest")
		s.RequireAttestation = true
		v, err := h.svc.Create(h.ctx, s)
		require.NoError(t, err)
		h.install(v.ID)
		require.NoError(t, os.WriteFile(v.Paths.Console, []byte(console), 0o600))
		return h, att, v
	}
	quote := func(h *harness, id string, stage imds.Stage) *vm.VM {
		h.t.Helper()
		a := h.svc.IMDSDeps().Attestor
		nonce, err := a.Nonce(h.ctx, id)
		require.NoError(h.t, err)
		_, err = a.SubmitQuote(h.ctx, id, imds.QuoteRequest{Stage: stage, Nonce: nonce})
		require.NoError(h.t, err)
		v, err := h.svc.Get(id)
		require.NoError(h.t, err)
		return v
	}

	t.Run("firmware vTPM stall, then READY=1", func(t *testing.T) {
		h, _, v := boot(t, "BdsDxe: loading Boot0001\nEFI stub: WARNING: Failed to measure data for event 1: 0x8000000000000007\ntpm_crb MSFT0101:00: ready\n")
		want := "vtpm not responding: the guest's TPM calls failed (console: EFI stub: WARNING: Failed to measure data for event 1: 0x8000000000000007); initrd attestation rejected: " + mismatch
		v = quote(h, v.ID, imds.StageInitrd)
		assert.Equal(t, want, v.LastError)
		v = h.ready(v.ID, v.CID)
		assert.Equal(t, want, v.LastError, "READY=1 keeps the rejection")
		assert.False(t, v.Attestation.UserDataReleased)
	})
	t.Run("no marker keeps the plain mismatch; a verified retry clears it", func(t *testing.T) {
		h, att, v := boot(t, "tpm_crb MSFT0101:00: ready\n")
		v = h.ready(v.ID, v.CID)
		v = quote(h, v.ID, imds.StageReady)
		assert.Equal(t, "ready attestation rejected: "+mismatch, v.LastError)
		assert.Equal(t, vm.StateReady, v.State)
		att.reason = ""
		v = quote(h, v.ID, imds.StageReady)
		assert.Empty(t, v.LastError)
	})
}

func TestPhaseBStartFailure(t *testing.T) {
	h := newHarness(t)
	h.rt.SetFailOn(func(s qemu.Spec) error {
		if s.Phase == qemu.PhaseBoot {
			return errors.New("boot: bad firmware")
		}
		return nil
	})
	v := h.create("b-fails")
	h.waitInstances(1).Exit(0)
	v = h.waitState(v.ID, vm.StateFailed)
	assert.Contains(t, v.LastError, "bad firmware")
	assert.NotNil(t, v.InstalledAt)
	assert.Zero(t, h.tpm.Running(), "swtpm of the failed start stopped")
	assert.False(t, h.notify.Subscribed(v.CID))
	assert.Len(t, h.store.Names(), 1, "the installed disk is kept")

	h.rt.SetFailOn(nil)
	v, err := h.svc.Start(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, vm.StateBooting, v.State)
	assert.Empty(t, v.LastError)
	assert.Equal(t, 1, h.tpm.Running())
}

func TestUnexpectedExitAndBootTimeout(t *testing.T) {
	h := newHarness(t)
	v := h.create("flaky")
	inst := h.install(v.ID)

	// No READY within BootTimeout: degraded to running, still up.
	h.waitTimers(1)
	h.clock.Advance(vm.DefaultBootTimeout)
	v = h.waitState(v.ID, vm.StateRunning)
	assert.Contains(t, v.LastError, "no READY=1 within 4m0s")
	v = h.ready(v.ID, v.CID)
	assert.Empty(t, v.LastError, "a late READY clears the note")

	// Guest powers itself off.
	inst.Exit(0)
	v = h.waitState(v.ID, vm.StateStopped)
	assert.Equal(t, "guest shut down", v.LastError)
	assert.Zero(t, h.tpm.Running())

	// Crash.
	_, err := h.svc.Start(h.ctx, v.ID)
	require.NoError(t, err)
	h.waitInstances(3).Exit(1)
	v = h.waitState(v.ID, vm.StateFailed)
	assert.Contains(t, v.LastError, "qemu exited: exit status 1")

	// Stop while still installing is allowed; the result cannot be started.
	w := h.create("half")
	_, err = h.svc.Stop(h.ctx, w.ID)
	require.NoError(t, err)
	w = h.waitState(w.ID, vm.StateStopped)
	assert.Nil(t, w.InstalledAt)
	_, err = h.svc.Start(h.ctx, w.ID)
	require.ErrorIs(t, err, apierr.ErrConflict)
	assert.Equal(t, 4, h.rt.Count(), "no phase B after a stopped installer")
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
		resp, err := client.Get("http://" + h.lan().IMDSAddr + imds.BasePath + key)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	quote := func(stage imds.Stage, nonce string) (int, imds.QuoteResult) {
		body, err := json.Marshal(imds.QuoteRequest{Stage: stage, Nonce: nonce, AKPub: []byte("ak"), Quote: []byte("q"),
			Signature: []byte("s"), PCRs: map[string]map[string]string{"sha256": {"11": "00"}}})
		require.NoError(t, err)
		resp, err := client.Post("http://"+h.lan().IMDSAddr+imds.BasePath+"/attest/quote", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		var res imds.QuoteResult
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&res))
		return resp.StatusCode, res
	}
	code, body := get("/hostname")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "attested", body)
	code, _ = get("/user-data")
	assert.Equal(t, http.StatusServiceUnavailable, code, "gated user-data is retryable for Ignition")

	att := h.svc.IMDSDeps().Attestor
	nonce, err := att.Nonce(h.ctx, v.ID)
	require.NoError(t, err)
	v = h.waitState(v.ID, vm.StateAttesting)
	assert.NotNil(t, v.Attestation.NonceIssuedAt)

	// A ready-stage quote never releases user-data.
	code, res := quote(imds.StageReady, nonce)
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, res.Verified)
	assert.False(t, res.UserDataReleased)
	a, err := h.svc.Attestation(v.ID)
	require.NoError(t, err)
	assert.False(t, a.UserDataReleased)
	require.NotNil(t, a.Ready)
	assert.True(t, a.Ready.Verified)

	nonce, err = att.Nonce(h.ctx, v.ID)
	require.NoError(t, err)
	code, res = quote(imds.StageInitrd, nonce)
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, res.Verified)
	assert.True(t, res.UserDataReleased, "the handler applies imds.ReleasesUserData")
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
	assert.Equal(t, vm.StateBooting, v.State)

	// Reports land in memory, on disk and in the metrics registry.
	require.NoError(t, h.svc.StoreReport(h.ctx, v.ID, json.RawMessage(`{"cpu":1}`)))
	rep, err := h.svc.Report(v.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"cpu":1}`, string(rep))
	assert.FileExists(t, v.Paths.Report)
	_, _, _, reports := h.metrics.snapshot()
	assert.JSONEq(t, `{"cpu":1}`, string(reports[v.ID]))
}

func TestPersistenceRoundTripAndLoad(t *testing.T) {
	h := newHarness(t)
	v := h.create("persist")
	h.install(v.ID)
	before := h.ready(v.ID, v.CID)
	require.NoError(t, h.svc.StoreReport(h.ctx, v.ID, json.RawMessage(`{"a":1}`)))

	// The record on disk is the snapshot.
	var onDisk vm.VM
	data, err := os.ReadFile(filepath.Join(v.Paths.Dir, vm.RecordFile))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &onDisk))
	assert.Equal(t, *before, onDisk)
	var states []network.State
	data, err = os.ReadFile(filepath.Join(h.stateDir, vm.NetworksFile))
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
	require.NoError(t, vm.WriteJSON(filepath.Join(v.Paths.Dir, vm.RecordFile), onDisk))
	h.nets = vmtest.NewNetworks(h.ev)
	h.start()
	require.Len(t, h.nets.Restored, 1)
	assert.Equal(t, states, h.nets.Restored[0])
	nets := h.svc.ListNetworks()
	require.Len(t, nets, 1)
	assert.Equal(t, v.IP, nets[0].Leases[0].IP.String())

	after, err := h.svc.Get(v.ID)
	require.NoError(t, err)
	assert.Equal(t, vm.StateStopped, after.State)
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
	h.ev.Reset()
	after, err = h.svc.Start(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, vm.StateBooting, after.State)
	assert.Contains(t, h.ev.List(), "storage.acquire:open")
	assert.Empty(t, h.waitInstances(3).Spec().KernelCmdlineExtra)
	assert.Equal(t, v.CID+1, h.create("second").CID, "CID allocation skips the restored VM")
}

func TestLoadMarksInterruptedRecords(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.svc.Close(h.ctx))
	for id, st := range map[string]vm.State{"aaaa0001": vm.StateCreating, "aaaa0002": vm.StateDeleting, "aaaa0003": vm.StateFailed} {
		dir := filepath.Join(h.stateDir, vm.VMsDir, id)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		rec := vm.VM{ID: id, Spec: vm.Spec{Name: "n" + id, Network: testNetwork}, State: st, CID: 3}
		data, _ := json.Marshal(rec)
		require.NoError(t, os.WriteFile(filepath.Join(dir, vm.RecordFile), data, 0o600))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(h.stateDir, vm.VMsDir, "garbage"), 0o700))
	h.nets = vmtest.NewNetworks(h.ev)
	h.start()
	list := h.svc.List()
	require.Len(t, list, 3)
	for _, v := range list {
		assert.Equal(t, vm.StateFailed, v.State, v.ID)
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
	assert.NotEmpty(t, h.lan().IMDSAddr, "IMDS served")

	v := h.create("user")
	err = h.svc.DeleteNetwork(h.ctx, testNetwork)
	require.ErrorIs(t, err, apierr.ErrConflict)
	assert.Contains(t, err.Error(), "user")
	require.NoError(t, h.svc.Delete(h.ctx, v.ID))
	require.NoError(t, h.svc.DeleteNetwork(h.ctx, testNetwork))
	require.NoError(t, h.svc.DeleteNetwork(h.ctx, "alpha"))
	require.ErrorIs(t, h.svc.DeleteNetwork(h.ctx, "alpha"), apierr.ErrNotFound)
	data, err := os.ReadFile(filepath.Join(h.stateDir, vm.NetworksFile))
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
		// Close the listener only once the connection has been accepted:
		// closing it while the dial still sits in the backlog resets that
		// connection and the client's handshake fails at random.
		accepted := make(chan struct{})
		go func() {
			c, err := ln.Accept()
			close(accepted)
			if err == nil {
				serve(c)
			}
		}()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			_ = ln.Close()
			return nil, err
		}
		<-accepted
		_ = ln.Close()
		return conn, nil
	}
}

func TestExecOverSSH(t *testing.T) {
	h := newHarness(t)
	v := h.create("ssh")
	_, err := h.svc.Exec(h.ctx, v.ID, nil)
	require.ErrorIs(t, err, apierr.ErrInvalid)

	hostKey := newSigner(t)
	var got string
	h.lan().Dialer = sshServer(t, v.SSHPublicKey, hostKey, func(cmd string) (string, uint32) {
		got = cmd
		return "hello\n", 3
	})
	res, err := h.svc.Exec(h.ctx, v.ID, []string{"echo", "it's here", "$HOME"})
	require.NoError(t, err)
	assert.Equal(t, `'echo' 'it'\''s here' '$HOME'`, got)
	assert.Equal(t, vm.ExecResult{Stdout: "hello\n", Stderr: "warn\n", ExitCode: 3}, res)
	pinned, err := os.ReadFile(filepath.Join(v.Paths.Dir, vm.HostKeyFile))
	require.NoError(t, err)
	assert.Equal(t, ssh.MarshalAuthorizedKey(hostKey.PublicKey()), pinned)

	// The same host key is accepted again, another one is not.
	res, err = h.svc.Exec(h.ctx, v.ID, []string{"true"})
	require.NoError(t, err)
	assert.Equal(t, 3, res.ExitCode)
	h.lan().Dialer = sshServer(t, v.SSHPublicKey, newSigner(t), func(string) (string, uint32) { return "", 0 })
	_, err = h.svc.Exec(h.ctx, v.ID, []string{"true"})
	require.ErrorContains(t, err, "host key of vm")

	// A key that is not vm-manager's is rejected by the guest.
	h.lan().Dialer = sshServer(t, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGb1vUmB8Uf9DR6vHCEwe8aSU2LLyOc0vHwwmN9Gpz1Y other", hostKey, nil)
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
	_, err := vm.GenerateSSHKey(filepath.Join(dir, "key"), "test")
	require.NoError(t, err)
	signer, err := vm.LoadSigner(filepath.Join(dir, "key"))
	require.NoError(t, err)
	return signer
}

func TestOptionsDefaults(t *testing.T) {
	_, err := vm.New(vm.Options{})
	require.ErrorContains(t, err, "Images, Networks, Notify, Runtime, StateDir, Storage, TPM")
	code, vars := vm.FindOVMF()
	assert.NotEmpty(t, code)
	assert.NotEmpty(t, vars)
}

// vsock CIDs are host-global: when QEMU reports the guest CID in use by a VM
// another vm-manager owns, the start is retried with the next free CID and
// the record keeps the one that worked.
func TestCreateRetriesWhenTheVsockCIDIsTaken(t *testing.T) {
	h := newHarness(t)
	var (
		first    uint32
		attempts atomic.Int32
	)
	h.rt.SetFailOn(func(spec qemu.Spec) error {
		attempts.Add(1)
		if first == 0 {
			first = spec.VsockCID
		}
		if spec.VsockCID == first {
			return errors.New("qemu-system-x86_64: -device vhost-vsock-pci,id=vsock0,guest-cid=" +
				strconv.FormatUint(uint64(spec.VsockCID), 10) + ": vhost-vsock: unable to set guest cid: Address already in use")
		}
		return nil
	})
	v := h.create("cid-taken")
	got := h.waitState(v.ID, vm.StateInstalling)
	assert.NotEqual(t, first, got.CID, "the record holds the CID that started")
	assert.Equal(t, first+1, got.CID, "the next free CID is tried")
	assert.Equal(t, int32(2), attempts.Load(), "exactly one retry")
	assert.Equal(t, 1, h.rt.Count(), "one instance running")
	assert.Equal(t, 1, h.tpm.Running(), "one swtpm for the VM")

	stored, err := h.svc.Get(v.ID)
	require.NoError(t, err)
	assert.Equal(t, got.CID, stored.CID)
}
