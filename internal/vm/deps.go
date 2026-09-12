package vm

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"time"

	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/imds"
	"github.com/giantswarm/vm-manager/internal/network"
	"github.com/giantswarm/vm-manager/internal/runtime/proc"
	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/storage"
	"github.com/giantswarm/vm-manager/internal/tpm"
)

// The interfaces below are the seams between the service and the host
// packages, narrowed to what the service calls so unit tests can drive the
// state machine with fakes. real.go adapts the concrete types.

// Runtime starts QEMU processes (qemu.Runtime through QEMURuntime).
type Runtime interface {
	Start(ctx context.Context, spec qemu.Spec) (Instance, error)
}

// Instance is a running QEMU process.
type Instance interface {
	// Wait yields the exit status once; callers keep the channel.
	Wait() <-chan proc.ExitStatus
	// Stop shuts the guest down gracefully, escalating to SIGKILL.
	Stop(ctx context.Context) error
	// Kill ends the process immediately.
	Kill() error
	// PID is the process id, what the metrics read /proc/<pid> for.
	PID() int
}

// TPMManager starts swtpm processes (tpm.Manager through TPM).
type TPMManager interface {
	Start(ctx context.Context, cfg tpm.Config) (TPMInstance, error)
}

// TPMInstance is a running swtpm.
type TPMInstance interface {
	SocketPath() string
	Stop(ctx context.Context) error
}

// StorageProvider is the part of storage.Provider the service uses.
type StorageProvider interface {
	Acquire(ctx context.Context, spec storage.AcquireSpec) (*storage.Volume, error)
	Release(ctx context.Context, v *storage.Volume) error
	Delete(ctx context.Context, name string) error
}

// NetworkManager owns the virtual networks (network.Manager through Networks).
type NetworkManager interface {
	Create(ctx context.Context, spec network.Spec) (Network, error)
	Get(name string) (Network, error)
	List() []Network
	States() []network.State
	Restore(ctx context.Context, states []network.State) error
	Delete(ctx context.Context, name string) error
}

// Network is one virtual network.
type Network interface {
	Name() string
	Spec() network.Spec
	GatewayIP() netip.Addr
	Leases() []network.Lease
	Stats() network.Stats
	Attach(ctx context.Context, vmID string) (*network.Attachment, error)
	Detach(vmID string) error
	Dial(ctx context.Context, addr string) (net.Conn, error)
	ListenIMDS() (net.Listener, error)
	Forward(ctx context.Context, hostAddr, vmIP string, port int) (PortForward, error)
}

// PortForward exposes one guest port on the host until closed.
type PortForward interface {
	Addr() net.Addr
	Close() error
}

// Notifier delivers sd_notify messages from guests (qemu.NotifyListener).
type Notifier interface {
	// Credential is the value of the vmm.notify_socket credential.
	Credential() string
	Subscribe(cid uint32) <-chan qemu.Notification
	Unsubscribe(cid uint32)
}

// ImageCatalog resolves image references (images.Catalog).
type ImageCatalog interface {
	Get(ref string) (images.Image, error)
	Default() (images.Image, error)
	// SysupdateDir is the tree served at /sysupdate/.
	SysupdateDir() string
}

// Clock is the time source; tests inject a fake to drive timeouts.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Metrics receives what the service observes: guest report uploads (after
// the service persisted them), the phase durations and deletions.
// *metrics.Registry implements it; the service in turn is its Source.
type Metrics interface {
	imds.ReportSink
	ObserveInstall(d time.Duration)
	ObserveBootToReady(d time.Duration)
	ForgetVM(id string)
}

// noMetrics is the Metrics of a service without a registry.
type noMetrics struct{}

func (noMetrics) StoreReport(context.Context, string, json.RawMessage) error { return nil }
func (noMetrics) ObserveInstall(time.Duration)                               {}
func (noMetrics) ObserveBootToReady(time.Duration)                           {}
func (noMetrics) ForgetVM(string)                                            {}
