package vm

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/runtime/proc"
	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/storage"
	"github.com/giantswarm/vm-manager/internal/tpm"
)

const (
	// netdevID is the QEMU netdev of the VM's only NIC.
	netdevID = "net0"
	// firstBootCmdline makes Ignition run on the first installed boot.
	firstBootCmdline = "ignition.firstboot"
	// consoleTailLines is how much console goes into LastError on failure.
	consoleTailLines = 40
	// idBytes is the length of the random VM id in bytes (hex doubles it).
	idBytes = 4
	// machineIDBytes is the length of system.machine_id (32 hex characters).
	machineIDBytes = 16
)

// errShuttingDown refuses a process start once Close has begun.
var errShuttingDown = fmt.Errorf("%w: vm-manager is shutting down", apierr.ErrConflict)

// Create validates the spec, allocates the VM (id, vsock CID, ssh key, OVMF
// variable store, target volume, network lease), starts the installer boot
// and returns. With WaitFor set it blocks until the milestone, a failure,
// the wait timeout (ErrTimeout) or ctx ends; the record is returned in every
// case it exists. A failure before the installer is up leaves nothing behind.
func (s *Service) Create(ctx context.Context, spec Spec) (*VM, error) {
	s.applyDefaults(&spec)
	if err := spec.validate(); err != nil {
		return nil, err
	}
	img, err := s.resolveImage(&spec)
	if err != nil {
		return nil, err
	}
	nw, err := s.opts.Networks.Get(spec.Network)
	if err != nil {
		return nil, fmt.Errorf("%w: network %q: %v", apierr.ErrInvalid, spec.Network, err)
	}
	e, err := s.allocate(spec)
	if err != nil {
		return nil, err
	}
	if err := s.launch(ctx, e, img, nw); err != nil {
		return nil, err
	}
	return s.waitFor(ctx, e, spec.WaitFor)
}

// launch provisions the VM and starts the installer under e.opMu, which the
// install-to-boot handoff also takes; the wait happens outside it.
func (s *Service) launch(ctx context.Context, e *entry, img images.Image, nw Network) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := s.provision(ctx, e, nw); err != nil {
		s.discard(ctx, e)
		return err
	}
	p, err := s.startProcess(ctx, e, qemu.PhaseInstall, img)
	if err != nil {
		s.discard(ctx, e)
		return err
	}
	s.mu.Lock()
	e.proc = p
	e.rec.Processes = p.handles()
	e.rec.State = StateInstalling
	e.rec.Phase = qemu.PhaseInstall
	s.save(e)
	s.broadcastLocked()
	s.mu.Unlock()
	s.supervise(e, p)
	s.log.Info("vm installing", "id", e.rec.ID, "name", e.rec.Name, "ip", e.rec.IP)
	return nil
}

func (s *Service) applyDefaults(spec *Spec) {
	if spec.Hostname == "" {
		spec.Hostname = spec.Name
	}
	if spec.Region == "" {
		spec.Region = s.opts.Region
	}
	if spec.Zone == "" {
		spec.Zone = spec.Network
	}
	if spec.WaitFor == "" {
		spec.WaitFor = WaitNone
	}
}

// resolveImage pins spec.Image to a full reference and spec.KubernetesVersion
// to one the image offers.
func (s *Service) resolveImage(spec *Spec) (images.Image, error) {
	var (
		img images.Image
		err error
	)
	if spec.Image == "" {
		img, err = s.opts.Images.Default()
	} else {
		img, err = s.opts.Images.Get(spec.Image)
	}
	if err != nil {
		return img, fmt.Errorf("%w: image %q: %v", apierr.ErrInvalid, spec.Image, err)
	}
	spec.Image = img.Ref()
	switch {
	case spec.KubernetesVersion == "" && len(img.KubernetesVersions) > 0:
		spec.KubernetesVersion = img.KubernetesVersions[0]
	case spec.KubernetesVersion != "" && !img.HasKubernetesVersion(spec.KubernetesVersion):
		return img, fmt.Errorf("%w: kubernetes version %q is not available for image %s (available: %s)",
			apierr.ErrInvalid, spec.KubernetesVersion, img.Ref(), strings.Join(img.KubernetesVersions, ", "))
	}
	return img, nil
}

// allocate reserves the id, CID and directory and writes the first record.
// Under s.mu, which Close also holds to set closing and collect the VMs it
// stops: an entry allocated before that is stopped by Close, one after it
// is never created.
func (s *Service) allocate(spec Spec) (*entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, errShuttingDown
	}
	for _, e := range s.vms {
		if e.rec.Name == spec.Name {
			return nil, fmt.Errorf("%w: vm name %q is in use by %s", apierr.ErrConflict, spec.Name, e.rec.ID)
		}
	}
	id, err := s.newID()
	if err != nil {
		return nil, err
	}
	machineID, err := randomHex(machineIDBytes)
	if err != nil {
		return nil, err
	}
	e := &entry{userData: spec.UserData}
	spec.UserData = nil // kept in its own file
	spec.WaitFor = ""   // a Create parameter, not part of the record
	e.rec = VM{
		ID:          id,
		Spec:        spec,
		State:       StateCreating,
		CID:         s.freeCID(),
		MachineID:   machineID,
		Attestation: Attestation{Required: spec.RequireAttestation},
		CreatedAt:   s.clock.Now(),
		Paths:       s.paths(id),
	}
	if err := os.MkdirAll(e.rec.Paths.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create vm dir: %w", err)
	}
	if err := s.persist(e); err != nil {
		_ = os.RemoveAll(e.rec.Paths.Dir)
		return nil, err
	}
	s.vms[id] = e
	return e, nil
}

func (s *Service) newID() (string, error) {
	for {
		id, err := randomHex(idBytes)
		if err != nil {
			return "", err
		}
		if _, taken := s.vms[id]; !taken && !fileExists(s.vmDir(id)) {
			return id, nil
		}
	}
}

// cidSpread is the range the per-service CID base is drawn from. vsock CIDs
// are host-global, and several vm-managers (developer runs, parallel e2e
// suites) share one host; each starts from a base derived from its state dir
// so their allocations rarely meet, and startProcess retries when they do.
const cidSpread = 1 << 20

// maxCIDRetries bounds how often a start is retried with a fresh CID when
// the kernel reports the guest CID in use by a VM this service does not know.
const maxCIDRetries = 16

// cidBase is where this service starts scanning for free CIDs.
func (s *Service) cidBase() uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s.opts.StateDir))
	return qemu.MinCID + h.Sum32()%cidSpread
}

// freeCID returns the first vsock CID at or after the service's base that
// no VM of this service holds; the caller holds s.mu.
func (s *Service) freeCID() uint32 { return s.freeCIDFrom(s.cidBase()) }

// freeCIDFrom returns the first free CID at or after start, wrapping around
// the valid range; the caller holds s.mu.
func (s *Service) freeCIDFrom(start uint32) uint32 {
	used := make(map[uint32]bool, len(s.vms))
	for _, e := range s.vms {
		used[e.rec.CID] = true
	}
	const span = uint64(qemu.MaxCID - qemu.MinCID)
	if start < qemu.MinCID || start >= qemu.MaxCID {
		start = qemu.MinCID
	}
	for i := uint64(0); i < span; i++ {
		cid := qemu.MinCID + uint32((uint64(start-qemu.MinCID)+i)%span)
		if !used[cid] {
			return cid
		}
	}
	return qemu.MaxCID
}

// reassignCID moves e to the next free CID after prev and persists it.
func (s *Service) reassignCID(e *entry, prev uint32) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.freeCIDFrom(prev + 1)
	e.rec.CID = next
	if err := s.persist(e); err != nil {
		return 0, err
	}
	return next, nil
}

// isCIDInUse recognises QEMU's vhost-vsock failure for a guest CID another
// VM on the host already holds.
func isCIDInUse(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "unable to set guest cid") ||
		(strings.Contains(msg, "guest-cid") && strings.Contains(msg, "in use"))
}

// provision creates the per-VM files and takes the volume and the lease.
// Each handle is recorded as soon as it exists so discard can release it.
func (s *Service) provision(ctx context.Context, e *entry, nw Network) error {
	p := e.rec.Paths
	pub, err := generateSSHKey(p.SSHKey, e.rec.ID)
	if err != nil {
		return err
	}
	if len(e.userData) > 0 {
		if err := writeAtomic(p.UserData, e.userData, 0o600); err != nil {
			return fmt.Errorf("write user-data: %w", err)
		}
	}
	if err := copyFile(s.opts.OVMFVarsTemplate, p.OVMFVars); err != nil {
		return fmt.Errorf("copy OVMF variable store: %w", err)
	}
	vol, err := s.opts.Storage.Acquire(ctx, storage.AcquireSpec{
		Name:      volumeName(e.rec.ID),
		SizeBytes: int64(e.rec.DiskGiB) << 30,
		Create:    storage.CreateNew,
	})
	if err != nil {
		return fmt.Errorf("acquire volume: %w", err)
	}
	s.mu.Lock()
	e.volume = vol
	e.rec.Paths.Volume = vol.Path
	e.rec.SSHPublicKey = pub
	s.mu.Unlock()

	att, err := nw.Attach(ctx, e.rec.ID)
	if err != nil {
		return fmt.Errorf("attach network: %w", err)
	}
	s.mu.Lock()
	e.rec.IP = att.IP.String()
	e.rec.MAC = att.MAC
	s.save(e)
	s.mu.Unlock()
	return s.persistNetworks()
}

// discard undoes a Create that failed before the installer was up.
func (s *Service) discard(ctx context.Context, e *entry) {
	s.teardown(ctx, e)
	s.mu.Lock()
	delete(s.vms, e.rec.ID)
	s.broadcastLocked()
	s.mu.Unlock()
	if err := s.persistNetworks(); err != nil {
		s.log.Warn("persist networks", "err", err)
	}
}

// teardown releases everything but the process: forwards, lease, volume,
// directory. Errors are logged; teardown always finishes.
func (s *Service) teardown(ctx context.Context, e *entry) {
	id := e.rec.ID
	s.mu.Lock()
	forwards := e.forwards
	e.forwards = nil
	vol := e.volume
	e.volume = nil
	netName := e.rec.Network
	s.mu.Unlock()

	for _, f := range forwards {
		_ = f.Close()
	}
	if nw, err := s.opts.Networks.Get(netName); err == nil {
		if err := nw.Detach(id); err != nil && !errors.Is(err, apierr.ErrNotFound) {
			s.log.Warn("detach vm", "id", id, "err", err)
		}
	}
	if vol != nil {
		if err := s.opts.Storage.Release(ctx, vol); err != nil {
			s.log.Warn("release volume", "id", id, "err", err)
		}
	}
	if err := s.opts.Storage.Delete(ctx, volumeName(id)); err != nil && !errors.Is(err, apierr.ErrNotFound) {
		s.log.Warn("delete volume", "id", id, "err", err)
	}
	if err := os.RemoveAll(e.rec.Paths.Dir); err != nil {
		s.log.Warn("remove vm dir", "id", id, "err", err)
	}
}

// startProcess starts swtpm and then QEMU for one phase. swtpm is stopped
// again when QEMU does not start; the caller owns the returned process.
func (s *Service) startProcess(ctx context.Context, e *entry, phase qemu.Phase, img images.Image) (*process, error) {
	rec := s.snapshot(e)
	nw, err := s.opts.Networks.Get(rec.Network)
	if err != nil {
		return nil, fmt.Errorf("network %q: %w", rec.Network, err)
	}
	att, err := nw.Attach(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("attach network: %w", err)
	}
	t, err := s.opts.TPM.Start(ctx, tpmConfig(rec))
	if err != nil {
		return nil, fmt.Errorf("start swtpm: %w", err)
	}
	p := &process{phase: phase, tpm: t, done: make(chan struct{})}
	if phase == qemu.PhaseBoot {
		p.notify = s.opts.Notify.Subscribe(rec.CID)
	}
	var inst Instance
	for attempt := 0; ; attempt++ {
		spec := s.qemuSpec(rec, phase, img, att.SocketPath, att.MAC, t.SocketPath())
		inst, err = s.opts.Runtime.Start(ctx, spec)
		if err == nil {
			break
		}
		if !isCIDInUse(err) || attempt >= maxCIDRetries {
			s.stopTPM(rec.ID, t)
			if phase == qemu.PhaseBoot {
				s.opts.Notify.Unsubscribe(rec.CID)
			}
			return nil, fmt.Errorf("start qemu: %w", err)
		}
		// Another vm-manager on this host holds the CID: move on.
		prev := rec.CID
		next, rerr := s.reassignCID(e, prev)
		if rerr != nil {
			s.stopTPM(rec.ID, t)
			if phase == qemu.PhaseBoot {
				s.opts.Notify.Unsubscribe(prev)
			}
			return nil, fmt.Errorf("start qemu: %w; reassign cid: %w", err, rerr)
		}
		s.log.Warn("vsock cid in use on this host, retrying with the next one", "id", rec.ID, "cid", prev, "next", next)
		rec.CID = next
		if phase == qemu.PhaseBoot {
			s.opts.Notify.Unsubscribe(prev)
			p.notify = s.opts.Notify.Subscribe(next)
		}
	}
	p.inst = inst
	p.wait = inst.Wait()
	return p, nil
}

// tpmConfig is the swtpm of a VM: state and log in Paths.TPMState, the
// unit named after the VM.
func tpmConfig(rec *VM) tpm.Config {
	return tpm.Config{ID: rec.ID, StateDir: rec.Paths.TPMState, Log: filepath.Join(rec.Paths.TPMState, tpm.LogName)}
}

// handles are the process handles the record persists for reattaching.
func (p *process) handles() *Processes {
	return &Processes{QEMU: p.inst.Handle(), TPM: p.tpm.Handle()}
}

// qemuSpec builds the QEMU spec of one phase; see docs/design.md "Boot flow".
func (s *Service) qemuSpec(rec *VM, phase qemu.Phase, img images.Image, netSocket, mac, tpmSocket string) qemu.Spec {
	creds := map[string]string{
		qemu.CredentialNotifySocket:          s.opts.Notify.Credential(),
		qemu.CredentialMachineID:             rec.MachineID,
		qemu.CredentialHostname:              rec.Hostname,
		qemu.CredentialSSHAuthorizedKeysRoot: authorizedKeys(rec),
	}
	spec := qemu.Spec{
		ID:        rec.ID,
		Phase:     phase,
		CPUs:      rec.CPUs,
		MemoryMiB: rec.MemoryMiB,
		Target:    rec.Paths.Volume,
		Netdevs: []qemu.Netdev{{
			ID:      netdevID,
			Backend: "stream,addr.type=unix,addr.path=" + qemu.EscapeOption(netSocket) + ",reconnect-ms=1000",
			MAC:     mac,
		}},
		SMBIOS:      qemu.SMBIOS{Manufacturer: qemu.DefaultManufacturer, Product: qemu.DefaultProduct, Serial: rec.ID},
		Credentials: creds,
		VsockCID:    rec.CID,
		TPMSocket:   tpmSocket,
		OVMFCode:    s.opts.OVMFCode,
		OVMFVars:    rec.Paths.OVMFVars,
		SerialLog:   rec.Paths.Console,
		QMPSocket:   rec.Paths.QMPSocket,
		ProcessLog:  rec.Paths.QEMULog,
	}
	switch phase {
	case qemu.PhaseInstall:
		spec.UKI = img.UKI
		spec.Installer = img.Disk
		spec.NoReboot = true
		creds[qemu.CredentialInstallTarget] = qemu.InstallTargetDevice
	case qemu.PhaseBoot:
		if !rec.Booted {
			spec.KernelCmdlineExtra = firstBootCmdline
		}
	}
	return spec
}

// authorizedKeys is the guest's root authorized_keys: vm-manager's key
// first, the caller's after it.
func authorizedKeys(rec *VM) string {
	keys := append([]string{rec.SSHPublicKey}, rec.SSHAuthorizedKeys...)
	return strings.Join(keys, "\n") + "\n"
}

// supervise follows one process until it exits.
func (s *Service) supervise(e *entry, p *process) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run(e, p)
	}()
}

func (s *Service) run(e *entry, p *process) {
	timeout := s.opts.InstallTimeout
	if p.phase == qemu.PhaseBoot {
		timeout = s.opts.BootTimeout
	}
	timer := s.clock.NewTimer(timeout)
	due := timer.C()
	for {
		select {
		case <-s.ctx.Done():
			// Close is detaching: the process lives on, unsupervised here.
			timer.Stop()
			return
		case exit := <-p.wait:
			// Stopped before onExit, which may hand over to the next
			// phase's supervisor: only that one's timer is armed then.
			timer.Stop()
			s.onExit(e, p, exit, timeout)
			return
		case n, ok := <-p.notify:
			if !ok {
				p.notify = nil
				continue
			}
			s.onNotify(e, n)
		case <-due:
			due = nil
			s.onTimeout(e, p, timeout)
		}
	}
}

// onTimeout ends an installer that overran and degrades an installed boot
// that has not reported READY=1 to running; the process stays up.
func (s *Service) onTimeout(e *entry, p *process, timeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case p.phase == qemu.PhaseInstall && e.rec.State == StateInstalling:
		p.timedOut = true
		s.log.Warn("installer did not finish in time, killing it", "id", e.rec.ID, "timeout", timeout)
		if err := p.inst.Kill(); err != nil {
			s.log.Warn("kill installer", "id", e.rec.ID, "err", err)
		}
	case p.phase == qemu.PhaseBoot && (e.rec.State == StateBooting || e.rec.State == StateAttesting):
		e.rec.State = StateRunning
		e.rec.LastError = fmt.Sprintf("no READY=1 within %s; the guest is up but has not reported ready", timeout)
		s.save(e)
		s.broadcastLocked()
	}
}

// onNotify records STATUS= and turns READY=1 into StateReady.
func (s *Service) onNotify(e *entry, n qemu.Notification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := n.Status(); st != "" {
		e.rec.Status = st
	}
	if n.Ready() {
		switch e.rec.State {
		case StateBooting, StateAttesting, StateRunning:
			e.rec.State = StateReady
			e.rec.ReadyAt = s.now()
			e.rec.Booted = true
			e.rec.LastError = ""
			if e.rec.BootedAt != nil {
				s.opts.Metrics.ObserveBootToReady(e.rec.ReadyAt.Sub(*e.rec.BootedAt))
			}
			s.log.Info("vm ready", "id", e.rec.ID)
		}
	}
	s.save(e)
	s.broadcastLocked()
}

// onExit stops swtpm, settles the state and, after a successful installer
// run, starts the installed boot.
func (s *Service) onExit(e *entry, p *process, exit proc.ExitStatus, timeout time.Duration) {
	id := e.rec.ID
	s.stopTPM(id, p.tpm)
	if p.phase == qemu.PhaseBoot {
		s.opts.Notify.Unsubscribe(e.rec.CID)
	}

	s.mu.Lock()
	e.proc = nil
	e.rec.Processes = nil
	prev := e.rec.State
	installed := false
	switch {
	case prev == StateDeleting:
		// Delete settles the record.
	case prev == StateStopping:
		e.rec.State = StateStopped
	case p.phase == qemu.PhaseInstall && prev == StateInstalling && exit.Code == 0 && !p.timedOut:
		e.rec.InstalledAt = s.now()
		s.opts.Metrics.ObserveInstall(e.rec.InstalledAt.Sub(e.rec.CreatedAt))
		installed = true
	case p.phase == qemu.PhaseInstall && prev == StateInstalling:
		e.rec.State = StateFailed
		e.rec.LastError = s.failureMessage(e, "installer", exit, p.timedOut, timeout)
	case exit.Code == 0:
		e.rec.State = StateStopped
		e.rec.LastError = "guest shut down"
	default:
		e.rec.State = StateFailed
		e.rec.LastError = s.failureMessage(e, "qemu", exit, false, 0)
	}
	s.save(e)
	s.broadcastLocked()
	s.log.Info("vm process exited", "id", id, "phase", p.phase, "exit", exit.String(), "state", e.rec.State)
	s.mu.Unlock()
	close(p.done)

	if installed {
		s.boot(e)
	}
}

// failureMessage is LastError for a process that ended badly; the caller
// holds s.mu.
func (s *Service) failureMessage(e *entry, what string, exit proc.ExitStatus, timedOut bool, timeout time.Duration) string {
	msg := fmt.Sprintf("%s exited: %s", what, exit)
	if timedOut {
		msg = fmt.Sprintf("%s did not finish within %s", what, timeout)
	}
	if tail, err := tailLines(e.rec.Paths.Console, consoleTailLines); err == nil && tail != "" {
		msg += "\nconsole:\n" + tail
	}
	return msg
}

// boot is the install-to-boot handoff, run by the installer's supervisor.
// Delete moves the record out of installing under opMu before it runs;
// Close sets closing, so an installed VM whose boot it overtook is left
// stopped and startable.
func (s *Service) boot(e *entry) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	s.mu.Lock()
	ok := e.rec.State == StateInstalling && e.proc == nil
	if ok && s.closing {
		ok = false
		e.rec.State = StateStopped
		e.rec.LastError = "vm-manager shut down before the installed boot started; start the VM"
		s.save(e)
		s.broadcastLocked()
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	if err := s.startBoot(s.ctx, e); err != nil {
		s.fail(e, fmt.Errorf("start installed boot: %w", err))
	}
}

// startBoot starts phase B; the caller holds e.opMu and there is no process.
func (s *Service) startBoot(ctx context.Context, e *entry) error {
	p, err := s.startProcess(ctx, e, qemu.PhaseBoot, images.Image{})
	if err != nil {
		return err
	}
	s.mu.Lock()
	e.proc = p
	e.rec.Processes = p.handles()
	e.rec.State = StateBooting
	e.rec.Phase = qemu.PhaseBoot
	e.rec.BootedAt = s.now()
	e.rec.ReadyAt = nil
	e.rec.Status = ""
	e.rec.LastError = ""
	e.rec.Attestation = Attestation{Required: e.rec.RequireAttestation, UserDataReleased: !e.rec.RequireAttestation}
	s.save(e)
	s.broadcastLocked()
	s.mu.Unlock()
	s.supervise(e, p)
	s.log.Info("vm booting", "id", e.rec.ID)
	return nil
}

func (s *Service) fail(e *entry, err error) {
	s.mu.Lock()
	e.rec.State = StateFailed
	e.rec.LastError = err.Error()
	s.save(e)
	s.broadcastLocked()
	s.mu.Unlock()
	s.log.Error("vm failed", "id", e.rec.ID, "err", err)
}

func (s *Service) stopTPM(id string, t TPMInstance) {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.StopTimeout)
	defer cancel()
	if err := t.Stop(ctx); err != nil {
		s.log.Warn("stop swtpm", "id", id, "err", err)
	}
}

// Start boots a stopped or failed VM from its disk (phase B). A VM whose
// installer never finished cannot be started; delete and recreate it.
func (s *Service) Start(ctx context.Context, id string) (*VM, error) {
	e, err := s.entry(id)
	if err != nil {
		return nil, err
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()

	s.mu.Lock()
	switch {
	case s.closing:
		err = errShuttingDown
	case e.rec.State == StateDeleting:
		err = fmt.Errorf("%w: vm %s is being deleted", apierr.ErrConflict, id)
	case e.proc != nil:
		err = fmt.Errorf("%w: vm %s is %s", apierr.ErrConflict, id, e.rec.State)
	case e.rec.InstalledAt == nil:
		err = fmt.Errorf("%w: vm %s never finished installing; delete and recreate it", apierr.ErrConflict, id)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.openVolume(ctx, e); err != nil {
		return nil, err
	}
	if err := s.startBoot(ctx, e); err != nil {
		s.fail(e, err)
		return nil, err
	}
	return s.snapshot(e), nil
}

// openVolume reopens the target volume after a vm-manager restart.
func (s *Service) openVolume(ctx context.Context, e *entry) error {
	s.mu.Lock()
	open := e.volume != nil
	s.mu.Unlock()
	if open {
		return nil
	}
	vol, err := s.opts.Storage.Acquire(ctx, storage.AcquireSpec{Name: volumeName(e.rec.ID), Create: storage.CreateOpen})
	if err != nil {
		return fmt.Errorf("open volume: %w", err)
	}
	s.mu.Lock()
	e.volume = vol
	e.rec.Paths.Volume = vol.Path
	s.mu.Unlock()
	return nil
}

// closeVolume undoes openVolume; the volume itself stays.
func (s *Service) closeVolume(ctx context.Context, e *entry) {
	s.mu.Lock()
	vol := e.volume
	e.volume = nil
	s.mu.Unlock()
	if vol == nil {
		return
	}
	if err := s.opts.Storage.Release(ctx, vol); err != nil {
		s.log.Warn("release volume", "id", e.rec.ID, "err", err)
	}
}

// Stop shuts a running VM down: ACPI powerdown, then SIGTERM and SIGKILL
// within StopTimeout. It returns once the process is gone.
func (s *Service) Stop(ctx context.Context, id string) (*VM, error) {
	e, err := s.entry(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	p := e.proc
	switch {
	case e.rec.State == StateDeleting:
		err = fmt.Errorf("%w: vm %s is being deleted", apierr.ErrConflict, id)
	case p == nil:
		err = fmt.Errorf("%w: vm %s is not running (%s)", apierr.ErrConflict, id, e.rec.State)
	default:
		e.rec.State = StateStopping
		s.save(e)
		s.broadcastLocked()
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.stopProcess(ctx, id, p)
	return s.snapshot(e), nil
}

// stopProcess stops p and waits for its supervisor to settle. The stop is
// bounded by StopTimeout and not cut short by a caller that goes away.
func (s *Service) stopProcess(ctx context.Context, id string, p *process) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.StopTimeout)
	defer cancel()
	if err := p.inst.Stop(sctx); err != nil {
		s.log.Warn("stop qemu, killing", "id", id, "err", err)
		if err := p.inst.Kill(); err != nil {
			s.log.Warn("kill qemu", "id", id, "err", err)
		}
	}
	<-p.done
}

// Reboot is Stop followed by Start.
func (s *Service) Reboot(ctx context.Context, id string) (*VM, error) {
	if _, err := s.Stop(ctx, id); err != nil {
		return nil, err
	}
	return s.Start(ctx, id)
}

// Delete stops the VM if it runs, then releases its lease, volume and
// directory, in that order. It fails with apierr.ErrConflict while another
// Delete is in progress.
func (s *Service) Delete(ctx context.Context, id string) error {
	e, err := s.entry(id)
	if err != nil {
		return err
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()

	s.mu.Lock()
	if _, present := s.vms[id]; !present {
		s.mu.Unlock()
		return fmt.Errorf("%w: vm %q", apierr.ErrNotFound, id)
	}
	if e.rec.State == StateDeleting {
		s.mu.Unlock()
		return fmt.Errorf("%w: vm %s is already being deleted", apierr.ErrConflict, id)
	}
	e.rec.State = StateDeleting
	s.save(e)
	s.broadcastLocked()
	p := e.proc
	s.mu.Unlock()

	if p != nil {
		s.stopProcess(ctx, id, p)
	}
	s.teardown(ctx, e)
	s.mu.Lock()
	delete(s.vms, id)
	s.broadcastLocked()
	s.mu.Unlock()
	s.opts.Metrics.ForgetVM(id)
	if f, ok := s.opts.Attestor.(Forgetter); ok {
		f.Forget(id)
	}
	s.log.Info("vm deleted", "id", id)
	return s.persistNetworks()
}

// waitFor blocks until the milestone w is reached, the VM fails or stops
// (ErrFailed), the wait times out (ErrTimeout) or ctx ends.
func (s *Service) waitFor(ctx context.Context, e *entry, w WaitFor) (*VM, error) {
	if w == WaitNone {
		return s.snapshot(e), nil
	}
	timeout := s.opts.InstallTimeout + s.opts.StopTimeout
	if w != WaitInstalled {
		timeout += s.opts.BootTimeout
	}
	deadline := s.clock.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		rec := e.rec.clone()
		ch := s.changed
		s.mu.Unlock()
		if done, err := reached(w, rec); done {
			return rec, err
		}
		select {
		case <-ch:
		case <-deadline.C():
			return rec, fmt.Errorf("%w: vm %s did not reach %s within %s (state %s)", ErrTimeout, rec.ID, w, timeout, rec.State)
		case <-ctx.Done():
			return rec, ctx.Err()
		}
	}
}

func reached(w WaitFor, v *VM) (bool, error) {
	switch v.State {
	case StateFailed:
		return true, fmt.Errorf("%w: %s", ErrFailed, v.LastError)
	case StateStopping, StateStopped, StateDeleting:
		return true, fmt.Errorf("%w: vm is %s", ErrFailed, v.State)
	}
	switch w {
	case WaitInstalled:
		return v.InstalledAt != nil, nil
	case WaitAttested:
		return v.BootedAt != nil && v.Attestation.UserDataReleased, nil
	case WaitReady:
		return v.ReadyAt != nil, nil
	default:
		return true, nil
	}
}

// entry looks a VM up under the lock.
func (s *Service) entry(id string) (*entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookup(id)
}

func (s *Service) snapshot(e *entry) *VM {
	s.mu.Lock()
	defer s.mu.Unlock()
	return e.rec.clone()
}

func volumeName(id string) string { return volumePrefix + id }
