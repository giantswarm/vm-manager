package vm

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
	"time"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/network"
	"github.com/giantswarm/vm-manager/internal/runtime/proc"
	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/storage"
	"github.com/giantswarm/vm-manager/internal/tpm"
)

// events is the shared call log the fakes append to, so tests can assert
// ordering across dependencies.
type events struct {
	mu  sync.Mutex
	log []string
}

func (ev *events) add(s string) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.log = append(ev.log, s)
}

func (ev *events) list() []string {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return append([]string(nil), ev.log...)
}

func (ev *events) reset() {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.log = nil
}

// fakeClock fires After channels on Advance.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeTimer
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, fakeTimer{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var keep []fakeTimer
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		keep = append(keep, w)
	}
	c.waiters = keep
}

func (c *fakeClock) timers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// fakeRuntime hands out fakeInstances the test ends by hand.
type fakeRuntime struct {
	mu    sync.Mutex
	ev    *events
	insts []*fakeInstance
	// failOn makes Start fail for matching specs.
	failOn func(qemu.Spec) error
	// hold runs before Start does anything; a test blocks in it to freeze a
	// start mid-flight.
	hold func(qemu.Spec)
}

func (r *fakeRuntime) Start(_ context.Context, spec qemu.Spec) (Instance, error) {
	r.mu.Lock()
	hold := r.hold
	r.mu.Unlock()
	if hold != nil {
		hold(spec)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.add("qemu.start:" + string(spec.Phase))
	if r.failOn != nil {
		if err := r.failOn(spec); err != nil {
			return nil, err
		}
	}
	inst := &fakeInstance{spec: spec, ev: r.ev, exit: make(chan proc.ExitStatus, 1)}
	r.insts = append(r.insts, inst)
	return inst, nil
}

func (r *fakeRuntime) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.insts)
}

func (r *fakeRuntime) at(i int) *fakeInstance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.insts[i]
}

func (r *fakeRuntime) setFailOn(f func(qemu.Spec) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOn = f
}

func (r *fakeRuntime) setHold(f func(qemu.Spec)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hold = f
}

type fakeInstance struct {
	spec qemu.Spec
	ev   *events
	exit chan proc.ExitStatus
	once sync.Once
}

func (i *fakeInstance) Wait() <-chan proc.ExitStatus { return i.exit }

func (i *fakeInstance) Stop(context.Context) error {
	i.ev.add("qemu.stop")
	i.Exit(0)
	return nil
}

func (i *fakeInstance) Kill() error {
	i.ev.add("qemu.kill")
	i.end(proc.ExitStatus{Code: -1, Err: errors.New("signal: killed")})
	return nil
}

// Exit ends the process with code, once.
func (i *fakeInstance) Exit(code int) {
	st := proc.ExitStatus{Code: code}
	if code != 0 {
		st.Err = fmt.Errorf("exit status %d", code)
	}
	i.end(st)
}

func (i *fakeInstance) end(st proc.ExitStatus) { i.once.Do(func() { i.exit <- st }) }

// fakeTPM counts live swtpm instances.
type fakeTPM struct {
	mu       sync.Mutex
	ev       *events
	live     map[*fakeTPMInstance]bool
	startErr error
}

func (m *fakeTPM) Start(_ context.Context, cfg tpm.Config) (TPMInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ev.add("tpm.start")
	if m.startErr != nil {
		return nil, m.startErr
	}
	if m.live == nil {
		m.live = make(map[*fakeTPMInstance]bool)
	}
	inst := &fakeTPMInstance{m: m, dir: cfg.StateDir}
	m.live[inst] = true
	return inst, nil
}

func (m *fakeTPM) running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live)
}

type fakeTPMInstance struct {
	m   *fakeTPM
	dir string
}

func (i *fakeTPMInstance) SocketPath() string { return filepath.Join(i.dir, tpm.DefaultSocketName) }

func (i *fakeTPMInstance) Stop(context.Context) error {
	i.m.mu.Lock()
	defer i.m.mu.Unlock()
	i.m.ev.add("tpm.stop")
	delete(i.m.live, i)
	return nil
}

// fakeStorage keeps volumes as files below dir.
type fakeStorage struct {
	mu         sync.Mutex
	ev         *events
	dir        string
	open       map[string]int
	acquireErr error
}

func (p *fakeStorage) path(name string) string {
	return filepath.Join(p.dir, name+storage.VolumeSuffix)
}

func (p *fakeStorage) Acquire(_ context.Context, spec storage.AcquireSpec) (*storage.Volume, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ev.add("storage.acquire:" + spec.Create.String())
	if p.acquireErr != nil {
		return nil, p.acquireErr
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

func (p *fakeStorage) Release(_ context.Context, v *storage.Volume) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ev.add("storage.release")
	p.open[v.Name]--
	return nil
}

func (p *fakeStorage) Delete(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ev.add("storage.delete")
	if p.open[name] > 0 {
		return fmt.Errorf("%w: volume %s is open", apierr.ErrConflict, name)
	}
	err := os.Remove(p.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: volume %s", apierr.ErrNotFound, name)
	}
	return err
}

func (p *fakeStorage) names() []string {
	matches, _ := filepath.Glob(p.path("*"))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, filepath.Base(m))
	}
	return out
}

// fakeNetworks is a network.Manager with loopback networks: leases come from
// 127.0.0.0/24, so an IMDS listener is a real TCP listener and a test client
// can present a lease address as its source.
type fakeNetworks struct {
	mu       sync.Mutex
	ev       *events
	nets     map[string]*fakeNetwork
	restored [][]network.State
}

func (m *fakeNetworks) Create(_ context.Context, spec network.Spec) (Network, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nets == nil {
		m.nets = make(map[string]*fakeNetwork)
	}
	if _, ok := m.nets[spec.Name]; ok {
		return nil, fmt.Errorf("%w: network %s", apierr.ErrConflict, spec.Name)
	}
	prefix, err := netip.ParsePrefix(spec.CIDR)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", apierr.ErrInvalid, err)
	}
	n := &fakeNetwork{ev: m.ev, spec: spec, prefix: prefix, leases: make(map[string]netip.Addr)}
	m.nets[spec.Name] = n
	return n, nil
}

func (m *fakeNetworks) Get(name string) (Network, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nets[name]
	if !ok {
		return nil, fmt.Errorf("%w: network %s", apierr.ErrNotFound, name)
	}
	return n, nil
}

func (m *fakeNetworks) List() []Network {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Network, 0, len(m.nets))
	for _, n := range m.nets {
		out = append(out, n)
	}
	return out
}

func (m *fakeNetworks) States() []network.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]network.State, 0, len(m.nets))
	for _, n := range m.nets {
		out = append(out, n.state())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out
}

func (m *fakeNetworks) Restore(ctx context.Context, states []network.State) error {
	m.mu.Lock()
	m.restored = append(m.restored, states)
	m.mu.Unlock()
	for _, st := range states {
		n, err := m.Create(ctx, st.Spec)
		if err != nil {
			return err
		}
		for vmID, ip := range st.Leases {
			n.(*fakeNetwork).leases[vmID] = netip.MustParseAddr(ip)
		}
	}
	return nil
}

func (m *fakeNetworks) Delete(_ context.Context, name string) error {
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

type fakeNetwork struct {
	mu     sync.Mutex
	ev     *events
	spec   network.Spec
	prefix netip.Prefix
	leases map[string]netip.Addr
	// dial replaces Dial when set (the ssh test plugs a server in).
	dial     func(ctx context.Context, addr string) (net.Conn, error)
	imdsAddr string
}

func (n *fakeNetwork) Name() string          { return n.spec.Name }
func (n *fakeNetwork) Spec() network.Spec    { return n.spec }
func (n *fakeNetwork) GatewayIP() netip.Addr { return n.prefix.Addr().Next() }

func (n *fakeNetwork) Leases() []network.Lease {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]network.Lease, 0, len(n.leases))
	for id, ip := range n.leases {
		out = append(out, network.Lease{VMID: id, IP: ip, MAC: macFor(ip)})
	}
	return out
}

func (n *fakeNetwork) state() network.State {
	st := network.State{Spec: n.spec, Leases: make(map[string]string)}
	for _, l := range n.Leases() {
		st.Leases[l.VMID] = l.IP.String()
	}
	return st
}

func (n *fakeNetwork) Attach(_ context.Context, vmID string) (*network.Attachment, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ev.add("net.attach")
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

func (n *fakeNetwork) Detach(vmID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ev.add("net.detach")
	if _, ok := n.leases[vmID]; !ok {
		return fmt.Errorf("%w: vm %s is not attached", apierr.ErrNotFound, vmID)
	}
	delete(n.leases, vmID)
	return nil
}

func (n *fakeNetwork) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if n.dial == nil {
		return nil, errors.New("fake network: no dialer")
	}
	return n.dial(ctx, addr)
}

func (n *fakeNetwork) ListenIMDS() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.imdsAddr = ln.Addr().String()
	n.mu.Unlock()
	return ln, nil
}

func (n *fakeNetwork) Forward(_ context.Context, hostAddr, _ string, _ int) (PortForward, error) {
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

// fakeNotifier delivers notifications the test injects.
type fakeNotifier struct {
	mu   sync.Mutex
	subs map[uint32]chan qemu.Notification
}

func (f *fakeNotifier) Credential() string { return "vsock-stream:2:4711" }

func (f *fakeNotifier) Subscribe(cid uint32) <-chan qemu.Notification {
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

func (f *fakeNotifier) Unsubscribe(cid uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.subs[cid]; ok {
		close(ch)
		delete(f.subs, cid)
	}
}

func (f *fakeNotifier) subscribed(cid uint32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.subs[cid]
	return ok
}

// notify sends fields to cid; false when nobody listens.
func (f *fakeNotifier) notify(cid uint32, fields map[string]string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.subs[cid]
	if !ok {
		return false
	}
	ch <- qemu.Notification{CID: cid, Fields: fields}
	return true
}

// fakeImages is a fixed catalog.
type fakeImages struct {
	imgs []images.Image
	dir  string
}

func (c *fakeImages) Get(ref string) (images.Image, error) {
	for _, img := range c.imgs {
		if img.Ref() == ref || img.ID == ref {
			return img, nil
		}
	}
	return images.Image{}, fmt.Errorf("%w: image %q", apierr.ErrNotFound, ref)
}

func (c *fakeImages) Default() (images.Image, error) {
	if len(c.imgs) == 0 {
		return images.Image{}, fmt.Errorf("%w: no images", apierr.ErrNotFound)
	}
	return c.imgs[0], nil
}

func (c *fakeImages) SysupdateDir() string { return filepath.Join(c.dir, images.SysupdateDirName) }
