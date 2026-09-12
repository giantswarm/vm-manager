package metrics

import (
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const hostPrefix = "vm_manager_"

// Attestation states of a VM as the host collector labels them.
const (
	// AttestationDisabled: the VM was created without require_attestation.
	AttestationDisabled = "disabled"
	// AttestationPending: user-data is still gated behind the initrd quote.
	AttestationPending = "pending"
	// AttestationReleased: a verified initrd quote released user-data.
	AttestationReleased = "released"
	// AttestationFailed: the initrd quote did not verify.
	AttestationFailed = "failed"
)

// AttestationStates lists the states in exposition order.
var AttestationStates = []string{AttestationDisabled, AttestationPending, AttestationReleased, AttestationFailed}

// Snapshot is what the host collector reads about one VM on each scrape.
type Snapshot struct {
	ID      string
	Name    string
	State   string
	Network string
	IP      string
	Image   string
	// Attestation is one of AttestationStates.
	Attestation string
	// PID is the QEMU process, 0 when none runs.
	PID int
	// DiskBytes is the provisioned size of the VM's volume.
	DiskBytes int64
	CreatedAt time.Time
	// InstalledAt, BootedAt and ReadyAt are zero until reached.
	InstalledAt time.Time
	BootedAt    time.Time
	ReadyAt     time.Time
}

// InstallSeconds is the installer phase duration, ok when it completed.
func (s Snapshot) InstallSeconds() (float64, bool) {
	if s.InstalledAt.IsZero() {
		return 0, false
	}
	return s.InstalledAt.Sub(s.CreatedAt).Seconds(), true
}

// BootToReadySeconds is the current installed boot's time to READY=1, ok
// once the guest sent it.
func (s Snapshot) BootToReadySeconds() (float64, bool) {
	if s.BootedAt.IsZero() || s.ReadyAt.IsZero() {
		return 0, false
	}
	return s.ReadyAt.Sub(s.BootedAt).Seconds(), true
}

// NetworkSnapshot is one virtual network's counters.
type NetworkSnapshot struct {
	Name          string
	BytesSent     uint64
	BytesReceived uint64
	Leases        int
}

// Source is what the host collector scrapes; the VM service implements it.
type Source interface {
	VMs() []Snapshot
	Networks() []NetworkSnapshot
}

// hostCollector turns the Source into vm_manager_* metrics.
type hostCollector struct {
	states []string
	proc   procReader
	log    *slog.Logger

	vmState, vmAttestation, vmInfo, vmCreated, vmInstall, vmBootToReady *prometheus.Desc
	vmCPU, vmRSS, vmDisk, vms, netBytes, netLeases                      *prometheus.Desc

	mu  sync.RWMutex
	src Source
}

func newHostCollector(states []string, proc procReader, log *slog.Logger) *hostCollector {
	vmLabels := []string{labelVM, "name"}
	return &hostCollector{
		states: states,
		proc:   proc,
		log:    log,
		vmState: prometheus.NewDesc(hostPrefix+"vm_state",
			"1 for the VM's current lifecycle state, 0 for the others.", []string{labelVM, "name", "state"}, nil),
		vmAttestation: prometheus.NewDesc(hostPrefix+"vm_attestation",
			"1 for the VM's attestation state (disabled, pending, released, failed), 0 for the others.", []string{labelVM, "name", "state"}, nil),
		vmInfo: prometheus.NewDesc(hostPrefix+"vm_info",
			"Constant 1 with the VM's name, network, IP and image for joins.", []string{labelVM, "name", "network", "ip", "image"}, nil),
		vmCreated: prometheus.NewDesc(hostPrefix+"vm_created_timestamp_seconds",
			"Unix time the VM record was created.", vmLabels, nil),
		vmInstall: prometheus.NewDesc(hostPrefix+"vm_install_seconds",
			"Seconds from creation to the end of the installer boot; absent until installed.", vmLabels, nil),
		vmBootToReady: prometheus.NewDesc(hostPrefix+"vm_boot_to_ready_seconds",
			"Seconds from the current installed boot's start to READY=1; absent until ready.", vmLabels, nil),
		vmCPU: prometheus.NewDesc(hostPrefix+"vm_cpu_seconds_total",
			"CPU time of the VM's QEMU process (user and system).", vmLabels, nil),
		vmRSS: prometheus.NewDesc(hostPrefix+"vm_memory_rss_bytes",
			"Resident memory of the VM's QEMU process.", vmLabels, nil),
		vmDisk: prometheus.NewDesc(hostPrefix+"vm_disk_bytes",
			"Provisioned size of the VM's volume.", vmLabels, nil),
		vms: prometheus.NewDesc(hostPrefix+"vms",
			"Number of VMs per lifecycle state.", []string{"state"}, nil),
		netBytes: prometheus.NewDesc(hostPrefix+"network_bytes_total",
			"Bytes moved through the virtual network, direction sent (to VMs) or received (from VMs).", []string{"network", "direction"}, nil),
		netLeases: prometheus.NewDesc(hostPrefix+"network_leases",
			"VMs holding a lease on the virtual network.", []string{"network"}, nil),
	}
}

func (h *hostCollector) setSource(src Source) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.src = src
}

func (h *hostCollector) source() Source {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.src
}

func (h *hostCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{h.vmState, h.vmAttestation, h.vmInfo, h.vmCreated, h.vmInstall,
		h.vmBootToReady, h.vmCPU, h.vmRSS, h.vmDisk, h.vms, h.netBytes, h.netLeases} {
		ch <- d
	}
}

func (h *hostCollector) Collect(ch chan<- prometheus.Metric) {
	src := h.source()
	if src == nil {
		return
	}
	gauge := prometheus.GaugeValue
	byState := make(map[string]int, len(h.states))
	for _, st := range h.states {
		byState[st] = 0
	}
	for _, v := range src.VMs() {
		byState[v.State]++
		for _, st := range h.statesWith(v.State) {
			ch <- prometheus.MustNewConstMetric(h.vmState, gauge, boolValue(st == v.State), v.ID, v.Name, st)
		}
		for _, st := range AttestationStates {
			ch <- prometheus.MustNewConstMetric(h.vmAttestation, gauge, boolValue(st == v.Attestation), v.ID, v.Name, st)
		}
		ch <- prometheus.MustNewConstMetric(h.vmInfo, gauge, 1, v.ID, v.Name, v.Network, v.IP, v.Image)
		ch <- prometheus.MustNewConstMetric(h.vmCreated, gauge, float64(v.CreatedAt.Unix()), v.ID, v.Name)
		ch <- prometheus.MustNewConstMetric(h.vmDisk, gauge, float64(v.DiskBytes), v.ID, v.Name)
		if d, ok := v.InstallSeconds(); ok {
			ch <- prometheus.MustNewConstMetric(h.vmInstall, gauge, d, v.ID, v.Name)
		}
		if d, ok := v.BootToReadySeconds(); ok {
			ch <- prometheus.MustNewConstMetric(h.vmBootToReady, gauge, d, v.ID, v.Name)
		}
		if st, ok := h.procStats(v); ok {
			ch <- prometheus.MustNewConstMetric(h.vmCPU, prometheus.CounterValue, st.CPUSeconds, v.ID, v.Name)
			ch <- prometheus.MustNewConstMetric(h.vmRSS, gauge, float64(st.RSSBytes), v.ID, v.Name)
		}
	}
	for st, n := range byState {
		ch <- prometheus.MustNewConstMetric(h.vms, gauge, float64(n), st)
	}
	for _, n := range src.Networks() {
		ch <- prometheus.MustNewConstMetric(h.netBytes, prometheus.CounterValue, float64(n.BytesSent), n.Name, "sent")
		ch <- prometheus.MustNewConstMetric(h.netBytes, prometheus.CounterValue, float64(n.BytesReceived), n.Name, "received")
		ch <- prometheus.MustNewConstMetric(h.netLeases, gauge, float64(n.Leases), n.Name)
	}
}

// statesWith is the configured state list, extended by an unknown current
// state so the VM still shows a 1.
func (h *hostCollector) statesWith(current string) []string {
	for _, st := range h.states {
		if st == current {
			return h.states
		}
	}
	return append(append([]string(nil), h.states...), current)
}

// procStats reads the QEMU process; a VM without one, or one that exited
// between the snapshot and the read, has no CPU and memory series.
func (h *hostCollector) procStats(v Snapshot) (procStats, bool) {
	if v.PID <= 0 {
		return procStats{}, false
	}
	st, err := h.proc.read(v.PID)
	if err != nil {
		h.log.Debug("vm process stats unavailable", "vm", v.ID, "pid", v.PID, "err", err)
		return procStats{}, false
	}
	return st, true
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
