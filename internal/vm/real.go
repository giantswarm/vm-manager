package vm

import (
	"context"
	"time"

	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/network"
	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/storage"
	"github.com/giantswarm/vm-manager/internal/tpm"
)

// Adapters from the concrete host packages to the service's interfaces. The
// concrete Start/Create/Get methods return pointers to their own types, which
// Go does not treat as the interface-typed returns declared in deps.go, so
// each is wrapped once here. Types whose methods already match are asserted.

var (
	_ StorageProvider = storage.Provider(nil)
	_ Notifier        = (*qemu.NotifyListener)(nil)
	_ ImageCatalog    = (*images.Catalog)(nil)
	_ TPMInstance     = (*tpm.Instance)(nil)
	_ Instance        = (*qemu.Instance)(nil)
	_ PortForward     = (*network.PortForward)(nil)
)

// QEMURuntime adapts qemu.Runtime.
func QEMURuntime(r *qemu.Runtime) Runtime { return qemuRuntime{r} }

type qemuRuntime struct{ r *qemu.Runtime }

func (q qemuRuntime) Start(ctx context.Context, spec qemu.Spec) (Instance, error) {
	inst, err := q.r.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	return inst, nil
}

// TPM adapts tpm.Manager.
func TPM(m *tpm.Manager) TPMManager { return tpmManager{m} }

type tpmManager struct{ m *tpm.Manager }

func (t tpmManager) Start(ctx context.Context, cfg tpm.Config) (TPMInstance, error) {
	inst, err := t.m.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return inst, nil
}

// Networks adapts network.Manager.
func Networks(m *network.Manager) NetworkManager { return networkManager{m} }

type networkManager struct{ m *network.Manager }

func (n networkManager) Create(ctx context.Context, spec network.Spec) (Network, error) {
	net, err := n.m.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return realNetwork{net}, nil
}

func (n networkManager) Get(name string) (Network, error) {
	net, err := n.m.Get(name)
	if err != nil {
		return nil, err
	}
	return realNetwork{net}, nil
}

func (n networkManager) List() []Network {
	nets := n.m.List()
	out := make([]Network, len(nets))
	for i, net := range nets {
		out[i] = realNetwork{net}
	}
	return out
}

func (n networkManager) States() []network.State { return n.m.States() }

func (n networkManager) Restore(ctx context.Context, states []network.State) error {
	return n.m.Restore(ctx, states)
}

func (n networkManager) Delete(ctx context.Context, name string) error { return n.m.Delete(ctx, name) }

type realNetwork struct{ *network.Network }

func (n realNetwork) Forward(ctx context.Context, hostAddr, vmIP string, port int) (PortForward, error) {
	fwd, err := n.Network.Forward(ctx, hostAddr, vmIP, port)
	if err != nil {
		return nil, err
	}
	return fwd, nil
}

// SystemClock is the wall clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time                         { return time.Now() }
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
