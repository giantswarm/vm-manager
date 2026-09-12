package vm

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/network"
	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
)

// State is where a VM is in its lifecycle; see the package documentation for
// the transitions.
type State string

const (
	StateCreating   State = "creating"
	StateInstalling State = "installing"
	StateBooting    State = "booting"
	StateAttesting  State = "attesting"
	StateReady      State = "ready"
	StateRunning    State = "running"
	StateStopping   State = "stopping"
	StateStopped    State = "stopped"
	StateFailed     State = "failed"
	StateDeleting   State = "deleting"
)

// Live reports whether a QEMU process belongs to this state.
func (s State) Live() bool {
	switch s {
	case StateInstalling, StateBooting, StateAttesting, StateReady, StateRunning, StateStopping:
		return true
	default:
		return false
	}
}

// WaitFor is the milestone Create blocks on before returning.
type WaitFor string

const (
	// WaitNone returns as soon as the installer boot has started.
	WaitNone WaitFor = "none"
	// WaitInstalled returns once the installer exited and the VM boots from its disk.
	WaitInstalled WaitFor = "installed"
	// WaitAttested returns once user-data is released: after a verified initrd
	// quote, or as soon as the installed boot starts when attestation is not
	// required.
	WaitAttested WaitFor = "attested"
	// WaitReady returns once the guest sent READY=1.
	WaitReady WaitFor = "ready"
)

// Valid reports whether w is a known milestone; the empty string means WaitNone.
func (w WaitFor) Valid() bool {
	switch w {
	case "", WaitNone, WaitInstalled, WaitAttested, WaitReady:
		return true
	default:
		return false
	}
}

// Errors Create's wait and the lifecycle methods return besides the apierr
// sentinels. The VM record is returned alongside so the caller can report it.
var (
	// ErrTimeout is a WaitFor milestone that was not reached in time; the VM
	// keeps running.
	ErrTimeout = errors.New("timeout")
	// ErrFailed is a VM that reached StateFailed (or stopped) before the
	// awaited milestone; LastError has the details.
	ErrFailed = errors.New("vm failed")
)

// Spec is what a caller asks for in create_vm.
type Spec struct {
	// Name is the VM name, a DNS label; it is also the default hostname.
	Name string `json:"name"`
	// Image is an image reference ("giantswarm-vm-base" or
	// "giantswarm-vm-base_0.1.0"); empty picks the catalog default. The VM
	// record carries the resolved reference.
	Image string `json:"image"`
	// KubernetesVersion is served as /kubernetes-version; empty picks the
	// highest version the image offers, none when it offers none.
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	CPUs              int    `json:"cpus"`
	MemoryMiB         int    `json:"memoryMiB"`
	DiskGiB           int    `json:"diskGiB"`
	// Network names the network the VM attaches to.
	Network string `json:"network"`
	// UserData is the CAPI bootstrap data (Ignition JSON) served as /user-data
	// once released; kept in its own file, never in vm.json.
	UserData []byte `json:"-"`
	// SSHAuthorizedKeys are appended to the guest's root authorized keys,
	// after vm-manager's own per-VM key.
	SSHAuthorizedKeys []string `json:"sshAuthorizedKeys,omitempty"`
	// Hostname defaults to Name.
	Hostname string `json:"hostname,omitempty"`
	// Metadata is served as /metadata/<key>.
	Metadata map[string]string `json:"metadata,omitempty"`
	// Region and Zone default to Options.Region (the host name) and the
	// network name.
	Region string `json:"region,omitempty"`
	Zone   string `json:"zone,omitempty"`
	// RequireAttestation gates /user-data behind a verified initrd quote.
	RequireAttestation bool `json:"requireAttestation"`
	// WaitFor makes Create block until the milestone is reached.
	WaitFor WaitFor `json:"-"`
}

// Quote is the recorded outcome of one attestation stage.
type Quote struct {
	Verified bool      `json:"verified"`
	Message  string    `json:"message,omitempty"`
	At       time.Time `json:"at"`
}

// Attestation summarizes the current boot's attestation; it is reset on every
// installed boot so user-data is gated again.
type Attestation struct {
	// Required mirrors Spec.RequireAttestation.
	Required bool `json:"required"`
	// UserDataReleased says whether /user-data is served.
	UserDataReleased bool `json:"userDataReleased"`
	// NonceIssuedAt is when the guest first asked for a nonce in this boot.
	NonceIssuedAt *time.Time `json:"nonceIssuedAt,omitempty"`
	// Initrd is the quote that gates user-data, Ready the one taken once the
	// system is up (PCR 13 included).
	Initrd *Quote `json:"initrd,omitempty"`
	Ready  *Quote `json:"ready,omitempty"`
}

// Paths are the files of one VM below <state>/vms/<id>.
type Paths struct {
	Dir       string `json:"dir"`
	Volume    string `json:"volume"`
	Console   string `json:"console"`
	TPMState  string `json:"tpmState"`
	OVMFVars  string `json:"ovmfVars"`
	QMPSocket string `json:"qmpSocket"`
	SSHKey    string `json:"sshKey"`
	UserData  string `json:"userData"`
	Report    string `json:"report"`
}

// VM is the persisted record of one VM (<state>/vms/<id>/vm.json).
type VM struct {
	ID string `json:"id"`
	Spec
	State State `json:"state"`
	// Phase is the boot phase of the current (or last) QEMU process.
	Phase qemu.Phase `json:"phase,omitempty"`
	// IP and MAC are the VM's lease on its network, CID its vsock context id.
	IP  string `json:"ip,omitempty"`
	MAC string `json:"mac,omitempty"`
	CID uint32 `json:"cid"`
	// MachineID seeds /etc/machine-id; it must not change between phases.
	MachineID string `json:"machineID"`
	// Booted is set once an installed boot reached READY=1; only the first
	// installed boot gets ignition.firstboot.
	Booted bool `json:"booted"`
	// Status is the last STATUS= the guest sent over the notify socket.
	Status string `json:"status,omitempty"`
	// SSHPublicKey is vm-manager's per-VM key, the first root authorized key.
	SSHPublicKey string      `json:"sshPublicKey"`
	Attestation  Attestation `json:"attestation"`

	CreatedAt   time.Time  `json:"createdAt"`
	InstalledAt *time.Time `json:"installedAt,omitempty"`
	// BootedAt is when the current installed boot started, ReadyAt when it
	// sent READY=1.
	BootedAt *time.Time `json:"bootedAt,omitempty"`
	ReadyAt  *time.Time `json:"readyAt,omitempty"`
	// LastError explains a failed, stopped or degraded state.
	LastError string `json:"lastError,omitempty"`
	Paths     Paths  `json:"paths"`
}

// clone returns a copy the caller may hand out.
func (v *VM) clone() *VM {
	c := *v
	c.SSHAuthorizedKeys = append([]string(nil), v.SSHAuthorizedKeys...)
	if v.Metadata != nil {
		c.Metadata = make(map[string]string, len(v.Metadata))
		for k, val := range v.Metadata {
			c.Metadata[k] = val
		}
	}
	c.UserData = nil
	return &c
}

// ExecResult is the outcome of a command run over ssh.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// NetworkInfo is a network as the API reports it.
type NetworkInfo struct {
	Spec    network.Spec    `json:"spec"`
	Gateway string          `json:"gateway"`
	Leases  []network.Lease `json:"leases"`
}

// Limits on Spec.
const (
	MaxCPUs      = 256
	MinMemoryMiB = 256
	MaxDiskGiB   = 4096
	MaxNameLen   = 63
)

var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// validate checks the caller-controlled fields; defaults are applied first.
func (s *Spec) validate() error {
	var problems []string
	if len(s.Name) > MaxNameLen || !labelRE.MatchString(s.Name) {
		problems = append(problems, "name must be a DNS label (lowercase letters, digits, dashes)")
	}
	if len(s.Hostname) > MaxNameLen || !labelRE.MatchString(s.Hostname) {
		problems = append(problems, "hostname must be a DNS label")
	}
	if s.CPUs < 1 || s.CPUs > MaxCPUs {
		problems = append(problems, fmt.Sprintf("cpus must be 1..%d", MaxCPUs))
	}
	if s.MemoryMiB < MinMemoryMiB {
		problems = append(problems, fmt.Sprintf("memoryMiB must be at least %d", MinMemoryMiB))
	}
	if s.DiskGiB < 1 || s.DiskGiB > MaxDiskGiB {
		problems = append(problems, fmt.Sprintf("diskGiB must be 1..%d", MaxDiskGiB))
	}
	if s.Network == "" {
		problems = append(problems, "network is required")
	}
	if !s.WaitFor.Valid() {
		problems = append(problems, "waitFor must be none, installed, attested or ready")
	}
	for k := range s.Metadata {
		if k == "" || strings.ContainsAny(k, "/ \t\n") {
			problems = append(problems, fmt.Sprintf("metadata key %q must not be empty or contain slashes or spaces", k))
		}
	}
	for _, key := range s.SSHAuthorizedKeys {
		if strings.ContainsAny(key, "\n\r") {
			problems = append(problems, "ssh authorized keys must be single lines")
			break
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", apierr.ErrInvalid, strings.Join(problems, "; "))
	}
	return nil
}
