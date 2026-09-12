// Package vmtest holds in-memory fakes of the host services behind
// vm.Service (runtime, swtpm, storage, networks, notify, image catalog) so
// the VM state machine and the API surfaces can be tested without QEMU. The
// fakes share one event log and let the test end processes, fire timers and
// deliver notifications by hand.
package vmtest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/network"
	"github.com/giantswarm/vm-manager/internal/runtime/proc"
	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/storage"
	"github.com/giantswarm/vm-manager/internal/tpm"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// Events is the shared call log the fakes append to, so tests can assert
// ordering across dependencies.
type Events struct {
	mu  sync.Mutex
	log []string
}

func (ev *Events) Add(s string) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.log = append(ev.log, s)
}

func (ev *Events) List() []string {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return append([]string(nil), ev.log...)
}

func (ev *Events) Reset() {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.log = nil
}

// Clock fires After channels on Advance.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []timer
}

type timer struct {
	at time.Time
	ch chan time.Time
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, timer{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var keep []timer
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		keep = append(keep, w)
	}
	c.waiters = keep
}

func (c *Clock) Timers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// Runtime hands out fakeInstances the test ends by hand.
type Runtime struct {
	mu    sync.Mutex
	ev    *Events
	insts []*Instance
	// failOn makes Start fail for matching specs.
	failOn func(qemu.Spec) error
	// hold runs before Start does anything; a test blocks in it to freeze a
	// start mid-flight.
	hold func(qemu.Spec)
	// attachable are the PIDs Attach finds, see SetAttachable.
	attachable map[int]bool
}

// SetAttachable declares the PIDs a later Attach finds still running, as a
// launcher whose processes outlived the previous vm-manager would report;
// any other handle is proc.ErrGone.
func (r *Runtime) SetAttachable(pids ...int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attachable = make(map[int]bool, len(pids))
	for _, pid := range pids {
		r.attachable[pid] = true
	}
}

// Attach implements vm.Runtime: an Instance for an attachable PID, whose
// exit the test drives like any other.
func (r *Runtime) Attach(_ context.Context, spec qemu.Spec, h proc.Handle) (vm.Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.Add("qemu.attach:" + string(spec.Phase))
	if !r.attachable[h.PID] {
		return nil, fmt.Errorf("%w: pid %d", proc.ErrGone, h.PID)
	}
	inst := &Instance{spec: spec, ev: r.ev, exit: make(chan proc.ExitStatus, 1), Pid: h.PID}
	r.insts = append(r.insts, inst)
	return inst, nil
}

func (r *Runtime) Start(_ context.Context, spec qemu.Spec) (vm.Instance, error) {
	r.mu.Lock()
	hold := r.hold
	r.mu.Unlock()
	if hold != nil {
		hold(spec)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.Add("qemu.start:" + string(spec.Phase))
	if r.failOn != nil {
		if err := r.failOn(spec); err != nil {
			return nil, err
		}
	}
	inst := &Instance{spec: spec, ev: r.ev, exit: make(chan proc.ExitStatus, 1)}
	r.insts = append(r.insts, inst)
	return inst, nil
}

func (r *Runtime) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.insts)
}

func (r *Runtime) At(i int) *Instance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.insts[i]
}

func (r *Runtime) SetFailOn(f func(qemu.Spec) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOn = f
}

func (r *Runtime) SetHold(f func(qemu.Spec)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hold = f
}

type Instance struct {
	spec qemu.Spec
	ev   *Events
	exit chan proc.ExitStatus
	once sync.Once
	// Pid is what PID returns; 0 (no process to read) unless a test sets it.
	Pid int
}

func (i *Instance) Wait() <-chan proc.ExitStatus { return i.exit }
func (i *Instance) PID() int                     { return i.Pid }

// Handle names the unit the way the systemd launcher would.
func (i *Instance) Handle() proc.Handle {
	return proc.Handle{Unit: proc.UnitName(i.spec.ID, qemu.UnitRole), PID: i.Pid}
}

// Detach records the handover; the fake process keeps "running".
func (i *Instance) Detach() error {
	i.ev.Add("qemu.detach")
	return nil
}

func (i *Instance) Stop(context.Context) error {
	i.ev.Add("qemu.stop")
	i.Exit(0)
	return nil
}

func (i *Instance) Kill() error {
	i.ev.Add("qemu.kill")
	i.end(proc.ExitStatus{Code: -1, Err: errors.New("signal: killed")})
	return nil
}

// Exit ends the process with code, once.
func (i *Instance) Exit(code int) {
	st := proc.ExitStatus{Code: code}
	if code != 0 {
		st.Err = fmt.Errorf("exit status %d", code)
	}
	i.end(st)
}

func (i *Instance) end(st proc.ExitStatus) { i.once.Do(func() { i.exit <- st }) }

// TPM counts live swtpm instances.
type TPM struct {
	mu sync.Mutex
	ev *Events
	// live are the running swtpms by state dir: one process per VM, which
	// an Attach finds again rather than duplicates.
	live      map[string]bool
	StartErr  error
	AttachErr error
}

func (m *TPM) Start(_ context.Context, cfg tpm.Config) (vm.TPMInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ev.Add("tpm.start")
	if m.StartErr != nil {
		return nil, m.StartErr
	}
	return m.instance(cfg), nil
}

// Attach implements vm.TPMManager: the swtpm is found again unless
// AttachErr is set; it counts as running from then on.
func (m *TPM) Attach(_ context.Context, cfg tpm.Config, _ proc.Handle) (vm.TPMInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ev.Add("tpm.attach")
	if m.AttachErr != nil {
		return nil, m.AttachErr
	}
	return m.instance(cfg), nil
}

// instance marks cfg's swtpm live; the caller holds m.mu.
func (m *TPM) instance(cfg tpm.Config) *TPMInstance {
	if m.live == nil {
		m.live = make(map[string]bool)
	}
	m.live[cfg.StateDir] = true
	return &TPMInstance{m: m, id: cfg.ID, dir: cfg.StateDir}
}

func (m *TPM) Running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live)
}

type TPMInstance struct {
	m   *TPM
	id  string
	dir string
}

func (i *TPMInstance) SocketPath() string { return filepath.Join(i.dir, tpm.DefaultSocketName) }

// Handle names the unit the way the systemd launcher would.
func (i *TPMInstance) Handle() proc.Handle {
	return proc.Handle{Unit: proc.UnitName(i.id, tpm.UnitRole), PID: 1}
}

func (i *TPMInstance) Stop(context.Context) error {
	i.m.mu.Lock()
	defer i.m.mu.Unlock()
	i.m.ev.Add("tpm.stop")
	delete(i.m.live, i.dir)
	return nil
}

// Storage keeps volumes as files below dir.
type Storage struct {
	mu         sync.Mutex
	ev         *Events
	dir        string
	open       map[string]int
	AcquireErr error
}

func (p *Storage) path(name string) string {
	return filepath.Join(p.dir, name+storage.VolumeSuffix)
}

func (p *Storage) Acquire(_ context.Context, spec storage.AcquireSpec) (*storage.Volume, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ev.Add("storage.acquire:" + spec.Create.String())
	if p.AcquireErr != nil {
		return nil, p.AcquireErr
	}
	if p.open == nil {
		p.open = make(map[string]int)
	}
	path := p.path(spec.Name)
	_, statErr := os.Stat(path)
	exists := statErr == nil
	switch {
	case spec.Create == storage.CreateNew && exists:
		return nil, fmt.Errorf("%w: volume %s exists", apierr.ErrConflict, spec.Name)
	case spec.Create == storage.CreateOpen && !exists:
		return nil, fmt.Errorf("%w: volume %s", apierr.ErrNotFound, spec.Name)
	case !exists:
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return nil, err
		}
	}
	p.open[spec.Name]++
	return &storage.Volume{Name: spec.Name, Path: path, SizeBytes: spec.SizeBytes}, nil
}

func (p *Storage) Release(_ context.Context, v *storage.Volume) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ev.Add("storage.release")
	p.open[v.Name]--
	return nil
}

func (p *Storage) Delete(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ev.Add("storage.delete")
	if p.open[name] > 0 {
		return fmt.Errorf("%w: volume %s is open", apierr.ErrConflict, name)
	}
	err := os.Remove(p.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: volume %s", apierr.ErrNotFound, name)
	}
	return err
}

func (p *Storage) Names() []string {
	matches, _ := filepath.Glob(p.path("*"))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, filepath.Base(m))
	}
	return out
}

// Networks is a network.Manager with loopback networks: leases come from
// 127.0.0.0/24, so an IMDS listener is a real TCP listener and a test client
// can present a lease address as its source.
type Networks struct {
	mu       sync.Mutex
	ev       *Events
	nets     map[string]*Network
	Restored [][]network.State
}

func (m *Networks) Create(_ context.Context, spec network.Spec) (vm.Network, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nets == nil {
		m.nets = make(map[string]*Network)
	}
	if _, ok := m.nets[spec.Name]; ok {
		return nil, fmt.Errorf("%w: network %s", apierr.ErrConflict, spec.Name)
	}
	prefix, err := netip.ParsePrefix(spec.CIDR)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", apierr.ErrInvalid, err)
	}
	n := &Network{ev: m.ev, spec: spec, prefix: prefix, leases: make(map[string]netip.Addr)}
	m.nets[spec.Name] = n
	return n, nil
}

func (m *Networks) Get(name string) (vm.Network, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nets[name]
	if !ok {
		return nil, fmt.Errorf("%w: network %s", apierr.ErrNotFound, name)
	}
	return n, nil
}

func (m *Networks) List() []vm.Network {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]vm.Network, 0, len(m.nets))
	for _, n := range m.nets {
		out = append(out, n)
	}
	return out
}

func (m *Networks) States() []network.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]network.State, 0, len(m.nets))
	for _, n := range m.nets {
		out = append(out, n.state())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out
}

func (m *Networks) Restore(ctx context.Context, states []network.State) error {
	m.mu.Lock()
	m.Restored = append(m.Restored, states)
	m.mu.Unlock()
	for _, st := range states {
		n, err := m.Create(ctx, st.Spec)
		if err != nil {
			return err
		}
		for vmID, ip := range st.Leases {
			n.(*Network).leases[vmID] = netip.MustParseAddr(ip)
		}
	}
	return nil
}

func (m *Networks) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nets[name]
	if !ok {
		return fmt.Errorf("%w: network %s", apierr.ErrNotFound, name)
	}
	if len(n.Leases()) > 0 {
		return fmt.Errorf("%w: network %s has attached vms", apierr.ErrConflict, name)
	}
	delete(m.nets, name)
	return nil
}

type Network struct {
	mu     sync.Mutex
	ev     *Events
	spec   network.Spec
	prefix netip.Prefix
	leases map[string]netip.Addr
	// Dialer replaces Dial when set (the ssh test plugs a server in).
	Dialer   func(ctx context.Context, addr string) (net.Conn, error)
	IMDSAddr string
	// Sent and Received are the counters Stats reports; tests set them.
	Sent, Received atomic.Uint64
}

func (n *Network) Name() string          { return n.spec.Name }
func (n *Network) Spec() network.Spec    { return n.spec }
func (n *Network) GatewayIP() netip.Addr { return n.prefix.Addr().Next() }

func (n *Network) Stats() network.Stats {
	n.mu.Lock()
	defer n.mu.Unlock()
	return network.Stats{BytesSent: n.Sent.Load(), BytesReceived: n.Received.Load(), Attachments: len(n.leases)}
}

func (n *Network) Leases() []network.Lease {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]network.Lease, 0, len(n.leases))
	for id, ip := range n.leases {
		out = append(out, network.Lease{VMID: id, IP: ip, MAC: macFor(ip)})
	}
	return out
}

func (n *Network) state() network.State {
	st := network.State{Spec: n.spec, Leases: make(map[string]string)}
	for _, l := range n.Leases() {
		st.Leases[l.VMID] = l.IP.String()
	}
	return st
}

func (n *Network) Attach(_ context.Context, vmID string) (*network.Attachment, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ev.Add("net.attach")
	ip, ok := n.leases[vmID]
	if !ok {
		used := make(map[netip.Addr]bool, len(n.leases))
		for _, a := range n.leases {
			used[a] = true
		}
		ip = n.prefix.Addr().Next().Next() // .2
		for used[ip] {
			ip = ip.Next()
		}
		n.leases[vmID] = ip
	}
	return &network.Attachment{VMID: vmID, MAC: macFor(ip), IP: ip, SocketPath: "/run/" + n.spec.Name + "/qemu.sock"}, nil
}

func (n *Network) Detach(vmID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ev.Add("net.detach")
	if _, ok := n.leases[vmID]; !ok {
		return fmt.Errorf("%w: vm %s is not attached", apierr.ErrNotFound, vmID)
	}
	delete(n.leases, vmID)
	return nil
}

func (n *Network) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if n.Dialer == nil {
		return nil, errors.New("fake network: no dialer")
	}
	return n.Dialer(ctx, addr)
}

func (n *Network) ListenIMDS() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.IMDSAddr = ln.Addr().String()
	n.mu.Unlock()
	return ln, nil
}

func (n *Network) Forward(_ context.Context, hostAddr, _ string, _ int) (vm.PortForward, error) {
	ln, err := net.Listen("tcp", hostAddr)
	if err != nil {
		return nil, err
	}
	return ln, nil
}

func macFor(ip netip.Addr) string {
	b := ip.As4()
	return fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3])
}

// Notifier delivers notifications the test injects.
type Notifier struct {
	mu   sync.Mutex
	subs map[uint32]chan qemu.Notification
}

func (f *Notifier) Credential() string { return "vsock-stream:2:4711" }

func (f *Notifier) Subscribe(cid uint32) <-chan qemu.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = make(map[uint32]chan qemu.Notification)
	}
	if ch, ok := f.subs[cid]; ok {
		return ch
	}
	ch := make(chan qemu.Notification, 8)
	f.subs[cid] = ch
	return ch
}

func (f *Notifier) Unsubscribe(cid uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.subs[cid]; ok {
		close(ch)
		delete(f.subs, cid)
	}
}

func (f *Notifier) Subscribed(cid uint32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.subs[cid]
	return ok
}

// notify sends fields to cid; false when nobody listens.
func (f *Notifier) Notify(cid uint32, fields map[string]string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.subs[cid]
	if !ok {
		return false
	}
	ch <- qemu.Notification{CID: cid, Fields: fields}
	return true
}

// Images is a fixed catalog.
type Images struct {
	imgs []images.Image
	dir  string
}

func (c *Images) Get(ref string) (images.Image, error) {
	for _, img := range c.imgs {
		if img.Ref() == ref || img.ID == ref {
			return img, nil
		}
	}
	return images.Image{}, fmt.Errorf("%w: image %q", apierr.ErrNotFound, ref)
}

func (c *Images) Default() (images.Image, error) {
	if len(c.imgs) == 0 {
		return images.Image{}, fmt.Errorf("%w: no images", apierr.ErrNotFound)
	}
	return c.imgs[0], nil
}

func (c *Images) SysupdateDir() string { return filepath.Join(c.dir, images.SysupdateDirName) }

// Deps is one set of fakes sharing an event log and a clock.
type Deps struct {
	Events   *Events
	Clock    *Clock
	Runtime  *Runtime
	TPM      *TPM
	Storage  *Storage
	Networks *Networks
	Notifier *Notifier
}

// NewDeps builds the fakes; storageDir is where Storage keeps its volumes
// and now is the clock's starting time.
func NewDeps(storageDir string, now time.Time) *Deps {
	ev := &Events{}
	return &Deps{
		Events:   ev,
		Clock:    &Clock{now: now},
		Runtime:  &Runtime{ev: ev},
		TPM:      &TPM{ev: ev},
		Storage:  &Storage{ev: ev, dir: storageDir},
		Networks: &Networks{ev: ev},
		Notifier: &Notifier{},
	}
}

// NewImages is a fixed catalog whose sysupdate tree lives below dir; the
// first image is the default.
func NewImages(dir string, imgs ...images.Image) *Images {
	return &Images{dir: dir, imgs: imgs}
}

// Spec is the qemu.Spec the process was started with.
func (i *Instance) Spec() qemu.Spec { return i.spec }

// NewNetworks is a fresh network manager sharing ev, as after a restart.
func NewNetworks(ev *Events) *Networks { return &Networks{ev: ev} }
