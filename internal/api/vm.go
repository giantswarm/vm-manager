package api

import (
	"context"
	"fmt"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/metrics"
	"github.com/giantswarm/vm-manager/internal/network"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// Defaults create_vm applies to fields the caller leaves empty. Image and
// Kubernetes version default inside the VM service (catalog default, newest
// available version); hostname defaults to the name.
const (
	DefaultCPUs      = 2
	DefaultMemoryMiB = 2048
	DefaultDiskGiB   = 20
	// DefaultNetwork is the network VMs attach to when none is named; serve
	// creates it at startup.
	DefaultNetwork = "default"
	// DefaultConsoleLines is how many lines get_vm_console returns.
	DefaultConsoleLines = 100
	// MetricsNote is set on get_vm_metrics while the guest has not uploaded
	// a report, MetricsUnparsedNote when the last upload is not a
	// systemd-report.
	MetricsNote         = "no systemd-report upload from the guest yet; host metrics only"
	MetricsUnparsedNote = "the guest's last upload is not a systemd-report and was not exported; see raw_report_url"
)

// The request bodies below are shared by both surfaces: the MCP tools bind
// their arguments into them and the REST handlers decode them from JSON, so
// the field names are the tool argument names.

// CreateNetworkRequest is create_network / POST /networks.
type CreateNetworkRequest struct {
	Name            string `json:"name"`
	CIDR            string `json:"cidr"`
	DNSSearchDomain string `json:"dns_search_domain,omitempty"`
}

func (r CreateNetworkRequest) spec() network.Spec {
	return network.Spec{Name: r.Name, CIDR: r.CIDR, DNSSearchDomain: r.DNSSearchDomain, EnableIMDS: true}
}

// CreateVMRequest is create_vm / POST /vms.
type CreateVMRequest struct {
	Name               string            `json:"name"`
	Image              string            `json:"image,omitempty"`
	KubernetesVersion  string            `json:"kubernetes_version,omitempty"`
	CPUs               int               `json:"cpus,omitempty"`
	MemoryMiB          int               `json:"memory_mib,omitempty"`
	DiskGiB            int               `json:"disk_gib,omitempty"`
	Network            string            `json:"network,omitempty"`
	UserData           string            `json:"user_data,omitempty"`
	SSHAuthorizedKeys  []string          `json:"ssh_authorized_keys,omitempty"`
	Hostname           string            `json:"hostname,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	RequireAttestation *bool             `json:"require_attestation,omitempty"`
	WaitFor            string            `json:"wait_for,omitempty"`
}

// spec applies the defaults; validation is the service's.
func (r CreateVMRequest) spec() vm.Spec {
	s := vm.Spec{
		Name:               r.Name,
		Image:              r.Image,
		KubernetesVersion:  r.KubernetesVersion,
		CPUs:               r.CPUs,
		MemoryMiB:          r.MemoryMiB,
		DiskGiB:            r.DiskGiB,
		Network:            r.Network,
		SSHAuthorizedKeys:  r.SSHAuthorizedKeys,
		Hostname:           r.Hostname,
		Metadata:           r.Metadata,
		RequireAttestation: true,
		WaitFor:            vm.WaitFor(r.WaitFor),
	}
	if r.UserData != "" {
		s.UserData = []byte(r.UserData)
	}
	if r.RequireAttestation != nil {
		s.RequireAttestation = *r.RequireAttestation
	}
	if s.CPUs == 0 {
		s.CPUs = DefaultCPUs
	}
	if s.MemoryMiB == 0 {
		s.MemoryMiB = DefaultMemoryMiB
	}
	if s.DiskGiB == 0 {
		s.DiskGiB = DefaultDiskGiB
	}
	if s.Network == "" {
		s.Network = DefaultNetwork
	}
	if s.WaitFor == "" {
		s.WaitFor = vm.WaitReady
	}
	return s
}

// ExecRequest is exec_vm / POST /vms/{id}/exec.
type ExecRequest struct {
	Command []string `json:"command"`
}

// ForwardRequest is forward_port / POST /vms/{id}/forward.
type ForwardRequest struct {
	Port int `json:"port"`
}

// ConsoleResponse is the body of get_vm_console / GET /vms/{id}/console.
type ConsoleResponse struct {
	ID      string `json:"id"`
	Lines   int    `json:"lines"`
	Console string `json:"console"`
}

// MetricsResponse is the body of get_vm_metrics / GET /vms/{id}/metrics:
// the host's view of the VM and a summary of the guest's last report.
type MetricsResponse struct {
	ID string `json:"id"`
	metrics.VMMetrics
	// RawReportURL serves the guest's last upload in full (REST only, it is
	// too large for a tool result); Note explains a missing guest section.
	RawReportURL string `json:"raw_report_url,omitempty"`
	Note         string `json:"note,omitempty"`
}

// ForwardResponse is the body of forward_port / POST /vms/{id}/forward.
type ForwardResponse struct {
	ID   string `json:"id"`
	Port int    `json:"port"`
	// Address is the host loopback address the guest port is reachable on.
	Address string `json:"address"`
}

// DeletedResponse is what the delete tools return; REST answers 204.
type DeletedResponse struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}

// createVM creates a VM. When the service returns a record together with an
// error (the awaited milestone failed or timed out) the VM exists and the
// error says how to follow it.
func (s Services) createVM(ctx context.Context, req CreateVMRequest) (*vm.VM, error) {
	v, err := s.VM.Create(ctx, req.spec())
	if err != nil && v != nil {
		detail := ""
		if v.LastError != "" {
			detail = ": " + v.LastError
		}
		return v, fmt.Errorf("%w; vm %s is %s%s (follow it with get_vm %s, delete it with delete_vm)", err, v.ID, v.State, detail, v.ID)
	}
	return v, err
}

func (s Services) console(id string, lines int) (ConsoleResponse, error) {
	if lines <= 0 {
		lines = DefaultConsoleLines
	}
	out, err := s.VM.Console(id, lines)
	if err != nil {
		return ConsoleResponse{}, err
	}
	return ConsoleResponse{ID: id, Lines: lines, Console: out}, nil
}

func (s Services) metrics(id string) (MetricsResponse, error) {
	report, err := s.VM.Report(id)
	if err != nil {
		return MetricsResponse{}, err
	}
	m, ok := s.Metrics.VM(id)
	if !ok {
		return MetricsResponse{}, fmt.Errorf("%w: vm %q", apierr.ErrNotFound, id)
	}
	res := MetricsResponse{ID: id, VMMetrics: m}
	switch {
	case len(report) > 0:
		res.RawReportURL = reportPath(id)
		if m.Guest == nil {
			res.Note = MetricsUnparsedNote
		}
	default:
		res.Note = MetricsNote
	}
	return res, nil
}

// reportPath is the REST route of the raw report.
func reportPath(id string) string { return Prefix + "/vms/" + id + "/report" }

// report returns the guest's last upload; apierr.ErrNotFound when none.
func (s Services) report(id string) ([]byte, error) {
	report, err := s.VM.Report(id)
	if err != nil {
		return nil, err
	}
	if len(report) == 0 {
		return nil, fmt.Errorf("%w: vm %q has no systemd-report upload yet", apierr.ErrNotFound, id)
	}
	return report, nil
}

func (s Services) forward(ctx context.Context, id string, port int) (ForwardResponse, error) {
	addr, err := s.VM.Forward(ctx, id, port)
	if err != nil {
		return ForwardResponse{}, err
	}
	return ForwardResponse{ID: id, Port: port, Address: addr}, nil
}

func (s Services) deleteVM(ctx context.Context, id string) (DeletedResponse, error) {
	if err := s.VM.Delete(ctx, id); err != nil {
		return DeletedResponse{}, err
	}
	return DeletedResponse{ID: id, Deleted: true}, nil
}

func (s Services) deleteNetwork(ctx context.Context, name string) (DeletedResponse, error) {
	if err := s.VM.DeleteNetwork(ctx, name); err != nil {
		return DeletedResponse{}, err
	}
	return DeletedResponse{ID: name, Deleted: true}, nil
}
