package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/imds"
	"github.com/giantswarm/vm-manager/internal/network"
	"github.com/giantswarm/vm-manager/internal/runtime/proc"
	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/storage"
)

// Defaults for Options.
const (
	DefaultOVMFCode         = "/usr/share/edk2/x64/OVMF_CODE.4m.fd"
	DefaultOVMFVarsTemplate = qemu.DefaultOVMFVarsTemplate
	DefaultInstallTimeout   = 5 * time.Minute
	// DefaultBootTimeout leaves room for the two attestation stages (90 s of
	// IMDS retries each) plus the Kubernetes sysext pull on a slow network.
	DefaultBootTimeout = 4 * time.Minute
	DefaultStopTimeout = 30 * time.Second
)

// Files and directories below the state dir.
const (
	vmsDir       = "vms"
	networksFile = "networks.json"
	recordFile   = "vm.json"
	consoleFile  = "console.log"
	tpmDir       = "tpm"
	ovmfVarsFile = "ovmf_vars.fd"
	qmpFile      = "qmp.sock"
	qemuLogFile  = "qemu.log"
	sshKeyFile   = "ssh_key"
	userDataFile = "user-data"
	reportFile   = "report.json"
	// volumePrefix names the storage volume of a VM: vm-<id>.
	volumePrefix = "vm-"
)

// ovmfCandidates are the firmware pairs FindOVMF probes, in order: Arch
// edk2-ovmf, Ubuntu/Debian ovmf (4M and legacy names), Fedora edk2-ovmf.
var ovmfCandidates = [][2]string{
	{DefaultOVMFCode, DefaultOVMFVarsTemplate},
	{"/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"},
	{"/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"},
	{"/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
}

// FindOVMF returns the first installed firmware pair (code image, variable
// store template), or the defaults when none is found.
func FindOVMF() (code, vars string) {
	for _, c := range ovmfCandidates {
		if fileExists(c[0]) && fileExists(c[1]) {
			return c[0], c[1]
		}
	}
	return DefaultOVMFCode, DefaultOVMFVarsTemplate
}

// Options configure the Service. Images, Storage, TPM, Runtime, Networks and
// Notify are required.
type Options struct {
	// StateDir holds vms/<id>/ and networks.json. Keep it short: unix socket
	// paths below it are limited to qemu.MaxUnixSocketPath bytes.
	StateDir string
	Images   ImageCatalog
	Storage  StorageProvider
	TPM      TPMManager
	Runtime  Runtime
	Networks NetworkManager
	Notify   Notifier
	// Attestor judges guest quotes; nil uses imds.NoopAttestor, which accepts
	// every quote and is for bring-up only.
	Attestor imds.Attestor
	// OVMFCode and OVMFVarsTemplate are the firmware files; empty runs FindOVMF.
	OVMFCode         string
	OVMFVarsTemplate string
	// Region is served as /region; empty uses the host name.
	Region string
	// InstallTimeout bounds the installer boot, BootTimeout the wait for
	// READY=1 after an installed boot, StopTimeout a graceful stop.
	InstallTimeout time.Duration
	BootTimeout    time.Duration
	StopTimeout    time.Duration
	// DetachOnClose makes Close leave the running VMs to the next
	// vm-manager instead of stopping them: their records keep the process
	// handles and Load reattaches. Only sound with a launcher whose
	// processes outlive vm-manager (proc.Launcher.Persistent).
	DetachOnClose bool
	// Logger for lifecycle events; nil uses slog.Default().
	Logger *slog.Logger
	// Clock drives timeouts; nil uses SystemClock.
	Clock Clock
	// Metrics receives report uploads, phase durations and deletions; nil
	// discards them.
	Metrics Metrics
}

func (o *Options) defaults() error {
	var missing []string
	for name, ok := range map[string]bool{
		"StateDir": o.StateDir != "", "Images": o.Images != nil, "Storage": o.Storage != nil,
		"TPM": o.TPM != nil, "Runtime": o.Runtime != nil, "Networks": o.Networks != nil, "Notify": o.Notify != nil,
	} {
		if !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("vm: options missing %s", strings.Join(missing, ", "))
	}
	if o.Attestor == nil {
		o.Attestor = &imds.NoopAttestor{}
	}
	if o.OVMFCode == "" || o.OVMFVarsTemplate == "" {
		code, vars := FindOVMF()
		if o.OVMFCode == "" {
			o.OVMFCode = code
		}
		if o.OVMFVarsTemplate == "" {
			o.OVMFVarsTemplate = vars
		}
	}
	if o.Region == "" {
		o.Region, _ = os.Hostname()
	}
	if o.InstallTimeout <= 0 {
		o.InstallTimeout = DefaultInstallTimeout
	}
	if o.BootTimeout <= 0 {
		o.BootTimeout = DefaultBootTimeout
	}
	if o.StopTimeout <= 0 {
		o.StopTimeout = DefaultStopTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Clock == nil {
		o.Clock = SystemClock{}
	}
	if o.Metrics == nil {
		o.Metrics = noMetrics{}
	}
	return nil
}

// Service is the VM lifecycle: it owns the records, the running processes
// and the IMDS servers of every network.
type Service struct {
	opts  Options
	log   *slog.Logger
	clock Clock

	// ctx bounds the supervisors; cancel is Close.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// mu guards vms, every entry's record, imds, changed and closing.
	mu      sync.Mutex
	vms     map[string]*entry
	imds    map[string]*imdsServer
	changed chan struct{}
	// closing is set once Close has begun; no process is started after
	// that (allocate, Start and the install-to-boot handoff check it).
	closing bool
}

// entry is a VM in memory: the persisted record plus the live handles.
type entry struct {
	rec      VM
	userData []byte
	report   []byte
	volume   *storage.Volume
	forwards []PortForward
	// proc is the running QEMU (with its swtpm), nil when none.
	proc *process
	// opMu serializes the operations that start or tear down processes and
	// resources: Create, Start, Reboot, Delete and the install-to-boot
	// handoff. Stop only needs the record lock.
	opMu sync.Mutex
	// sshMu guards the pinned host key.
	sshMu sync.Mutex
}

// process is one QEMU run with the swtpm started for it.
type process struct {
	phase    qemu.Phase
	tpm      TPMInstance
	inst     Instance
	wait     <-chan proc.ExitStatus
	notify   <-chan qemu.Notification
	timedOut bool
	// done is closed by the supervisor once the process exited and swtpm
	// was stopped.
	done chan struct{}
}

type imdsServer struct {
	srv *http.Server
	ln  io.Closer
}

// New returns a service; call Load before serving requests.
func New(opts Options) (*Service, error) {
	if err := opts.defaults(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(opts.StateDir, vmsDir), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		opts:    opts,
		log:     opts.Logger,
		clock:   opts.Clock,
		ctx:     ctx,
		cancel:  cancel,
		vms:     make(map[string]*entry),
		imds:    make(map[string]*imdsServer),
		changed: make(chan struct{}),
	}, nil
}

// Load restores the persisted state: networks (with their leases) and their
// IMDS servers first, then the VM records. A VM recorded with a live
// process is reattached (reattach.go) when its processes still run under
// their launcher, and marked stopped with the reason otherwise.
func (s *Service) Load(ctx context.Context) error {
	states, err := s.loadNetworks()
	if err != nil {
		return err
	}
	if len(states) > 0 {
		if err := s.opts.Networks.Restore(ctx, states); err != nil {
			return fmt.Errorf("restore networks: %w", err)
		}
	}
	for _, st := range states {
		if err := s.ServeIMDS(ctx, st.Spec.Name); err != nil {
			return err
		}
	}

	dirs, err := os.ReadDir(filepath.Join(s.opts.StateDir, vmsDir))
	if err != nil {
		return fmt.Errorf("read vms dir: %w", err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		e, err := s.loadEntry(d.Name())
		if err != nil {
			s.log.Warn("skipping unreadable vm record", "id", d.Name(), "err", err)
			continue
		}
		s.mu.Lock()
		s.vms[e.rec.ID] = e
		s.mu.Unlock()
		if e.rec.State.Live() {
			s.reattachOrStop(ctx, e)
		}
		s.mu.Lock()
		s.save(e)
		s.mu.Unlock()
	}
	s.broadcast()
	return nil
}

// reattachOrStop picks the processes of a VM recorded live back up, or
// settles the record as stopped with the reason when that is not possible.
func (s *Service) reattachOrStop(ctx context.Context, e *entry) {
	err := s.reattach(ctx, e)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.log.Info("vm reattached", "id", e.rec.ID, "state", e.rec.State, "pid", e.proc.inst.PID())
		return
	}
	s.log.Warn("vm not reattached", "id", e.rec.ID, "state", e.rec.State, "err", err)
	e.rec.LastError = fmt.Sprintf("vm-manager restarted while the VM was %s and could not reattach to its QEMU (%v); start the VM again", e.rec.State, err)
	e.rec.State = StateStopped
	e.rec.Processes = nil
}

func (s *Service) loadEntry(id string) (*entry, error) {
	dir := s.vmDir(id)
	e := &entry{}
	if err := readJSON(filepath.Join(dir, recordFile), &e.rec); err != nil {
		return nil, err
	}
	if e.rec.ID != id {
		return nil, fmt.Errorf("record id %q does not match directory", e.rec.ID)
	}
	e.rec.Paths = s.paths(id)
	e.userData, _ = os.ReadFile(e.rec.Paths.UserData) // #nosec G304 -- state dir file.
	e.report, _ = os.ReadFile(e.rec.Paths.Report)     // #nosec G304 -- state dir file.
	if e.rec.State == StateCreating || e.rec.State == StateDeleting {
		e.rec.LastError = fmt.Sprintf("vm-manager restarted while the VM was %s; delete it", e.rec.State)
		e.rec.State = StateFailed
	}
	return e, nil
}

// Close ends the service: with DetachOnClose the running VMs are left to
// the next vm-manager (detachAll), otherwise every one is stopped
// gracefully (stopAll); then the IMDS servers go down. Once Close has begun
// no process is started any more: each VM is visited under its opMu, so a
// start in flight (Create, Start, the install-to-boot handoff) completes
// first and is what gets stopped or detached, and a start that has not
// begun sees the closing flag and refuses.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	entries := make([]*entry, 0, len(s.vms))
	for _, e := range s.vms {
		entries = append(entries, e)
	}
	s.mu.Unlock()

	if s.opts.DetachOnClose {
		s.detachAll(entries)
	} else {
		s.stopAll(ctx, entries)
	}

	s.mu.Lock()
	servers := s.imds
	s.imds = make(map[string]*imdsServer)
	s.mu.Unlock()
	var errs []error
	for name, srv := range servers {
		if err := srv.close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("imds %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// stopAll stops every VM, each under its opMu, and waits for the
// supervisors to settle the records.
func (s *Service) stopAll(ctx context.Context, entries []*entry) {
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			e.opMu.Lock()
			defer e.opMu.Unlock()
			_, err := s.Stop(ctx, e.rec.ID)
			if err != nil && !errors.Is(err, apierr.ErrConflict) && !errors.Is(err, apierr.ErrNotFound) {
				s.log.Warn("stopping vm on shutdown", "id", e.rec.ID, "err", err)
			}
		}(e)
	}
	wg.Wait()
	s.cancel()
	s.wg.Wait()
}

// detachAll leaves the processes running for the next vm-manager: starts in
// flight finish (their records then carry the handles), the supervisors
// return, and each live instance is detached (QMP closed, notify
// subscription dropped) without a signal to the guest. The records stay
// live on disk, which is what Load reattaches from.
func (s *Service) detachAll(entries []*entry) {
	for _, e := range entries {
		e.opMu.Lock()
		e.opMu.Unlock() //nolint:staticcheck // the lock only waits for an in-flight start
	}
	s.cancel()
	s.wg.Wait()
	detached := 0
	for _, e := range entries {
		s.mu.Lock()
		p := e.proc
		id, cid, state := e.rec.ID, e.rec.CID, e.rec.State
		s.mu.Unlock()
		if p == nil {
			continue
		}
		if err := p.inst.Detach(); err != nil {
			s.log.Warn("detaching vm", "id", id, "err", err)
		}
		if p.phase == qemu.PhaseBoot {
			s.opts.Notify.Unsubscribe(cid)
		}
		s.log.Info("vm left running", "id", id, "state", state, "pid", p.inst.PID())
		detached++
	}
	if detached > 0 {
		s.log.Info("vms keep running under their launcher; the next vm-manager reattaches", "count", detached)
	}
}

// Get returns one VM record.
func (s *Service) Get(id string) (*VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookup(id)
	if err != nil {
		return nil, err
	}
	return e.rec.clone(), nil
}

// List returns every VM record, sorted by creation time then id.
func (s *Service) List() []*VM {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*VM, 0, len(s.vms))
	for _, e := range s.vms {
		out = append(out, e.rec.clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Attestation returns the current boot's attestation summary.
func (s *Service) Attestation(id string) (Attestation, error) {
	v, err := s.Get(id)
	if err != nil {
		return Attestation{}, err
	}
	return v.Attestation, nil
}

// Report returns the last systemd-report upload of the VM, nil when none.
func (s *Service) Report(id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookup(id)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), e.report...), nil
}

// lookup finds an entry; the caller holds s.mu.
func (s *Service) lookup(id string) (*entry, error) {
	e, ok := s.vms[id]
	if !ok {
		return nil, fmt.Errorf("%w: vm %q", apierr.ErrNotFound, id)
	}
	return e, nil
}

// CreateNetwork brings up a network for VMs: the IMDS is always enabled and
// served before any guest can boot.
func (s *Service) CreateNetwork(ctx context.Context, spec network.Spec) (*NetworkInfo, error) {
	spec.EnableIMDS = true
	n, err := s.opts.Networks.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := s.ServeIMDS(ctx, n.Name()); err != nil {
		_ = s.opts.Networks.Delete(ctx, n.Name())
		return nil, err
	}
	if err := s.persistNetworks(); err != nil {
		return nil, err
	}
	return networkInfo(n), nil
}

// GetNetwork returns one network.
func (s *Service) GetNetwork(name string) (*NetworkInfo, error) {
	n, err := s.opts.Networks.Get(name)
	if err != nil {
		return nil, err
	}
	return networkInfo(n), nil
}

// ListNetworks returns every network, sorted by name.
func (s *Service) ListNetworks() []*NetworkInfo {
	nets := s.opts.Networks.List()
	out := make([]*NetworkInfo, 0, len(nets))
	for _, n := range nets {
		out = append(out, networkInfo(n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out
}

// DeleteNetwork tears a network down. It refuses (apierr.ErrConflict) while
// a VM is attached to it, running or not.
func (s *Service) DeleteNetwork(ctx context.Context, name string) error {
	s.mu.Lock()
	var users []string
	for _, e := range s.vms {
		if e.rec.Network == name {
			users = append(users, e.rec.Name)
		}
	}
	s.mu.Unlock()
	if len(users) > 0 {
		sort.Strings(users)
		return fmt.Errorf("%w: network %q is used by vm %s", apierr.ErrConflict, name, strings.Join(users, ", "))
	}
	if err := s.opts.Networks.Delete(ctx, name); err != nil {
		return err
	}
	s.mu.Lock()
	srv := s.imds[name]
	delete(s.imds, name)
	s.mu.Unlock()
	if srv != nil {
		if err := srv.close(ctx); err != nil {
			s.log.Warn("closing imds server", "network", name, "err", err)
		}
	}
	return s.persistNetworks()
}

func networkInfo(n Network) *NetworkInfo {
	leases := n.Leases()
	sort.Slice(leases, func(i, j int) bool { return leases[i].IP.Less(leases[j].IP) })
	return &NetworkInfo{Spec: n.Spec(), Gateway: n.GatewayIP().String(), Leases: leases}
}

// broadcast wakes every waiter; call without s.mu held or with it, both work.
func (s *Service) broadcast() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

// broadcastLocked is broadcast for callers holding s.mu.
func (s *Service) broadcastLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Service) vmDir(id string) string { return filepath.Join(s.opts.StateDir, vmsDir, id) }

func (s *Service) paths(id string) Paths {
	dir := s.vmDir(id)
	return Paths{
		Dir:       dir,
		Console:   filepath.Join(dir, consoleFile),
		TPMState:  filepath.Join(dir, tpmDir),
		OVMFVars:  filepath.Join(dir, ovmfVarsFile),
		QMPSocket: filepath.Join(dir, qmpFile),
		QEMULog:   filepath.Join(dir, qemuLogFile),
		SSHKey:    filepath.Join(dir, sshKeyFile),
		UserData:  filepath.Join(dir, userDataFile),
		Report:    filepath.Join(dir, reportFile),
	}
}

func (s *Service) now() *time.Time {
	t := s.clock.Now()
	return &t
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}
