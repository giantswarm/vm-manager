package metrics

import "time"

// VMMetrics is the per-VM summary behind get_vm_metrics: the host's view of
// the VM and what its guest reported last.
type VMMetrics struct {
	Host HostMetrics `json:"host"`
	// Guest is nil until the guest uploaded a report.
	Guest *GuestMetrics `json:"guest"`
}

// HostMetrics is what the host knows without the guest's help.
type HostMetrics struct {
	State       string `json:"state"`
	Attestation string `json:"attestation"`
	// PID is the QEMU process, absent when none runs.
	PID int `json:"pid,omitempty"`
	// CPUSeconds and MemoryRSSBytes are the QEMU process's, absent without one.
	CPUSeconds     *float64 `json:"cpu_seconds,omitempty"`
	MemoryRSSBytes *int64   `json:"memory_rss_bytes,omitempty"`
	DiskBytes      int64    `json:"disk_bytes"`
	// InstallSeconds is absent until installed, BootToReadySeconds until the
	// current boot sent READY=1.
	InstallSeconds     *float64 `json:"install_seconds,omitempty"`
	BootToReadySeconds *float64 `json:"boot_to_ready_seconds,omitempty"`
	// Network is the VM's network; its counters are per network, shared by
	// every VM attached to it.
	Network NetworkMetrics `json:"network"`
}

// NetworkMetrics is the VM's network as HostMetrics reports it.
type NetworkMetrics struct {
	Name          string `json:"name"`
	BytesSent     uint64 `json:"bytes_sent"`
	BytesReceived uint64 `json:"bytes_received"`
	Leases        int    `json:"leases"`
}

// GuestMetrics summarizes the last systemd-report upload.
type GuestMetrics struct {
	ReceivedAt time.Time `json:"received_at"`
	// GeneratedAt is the report's own timestamp when it parsed.
	GeneratedAt      *time.Time `json:"generated_at,omitempty"`
	ReportAgeSeconds float64    `json:"report_age_seconds"`
	// Families is the number of distinct family names, Series the number of
	// entries in the report, SeriesExported how many are on /metrics and
	// SeriesDropped how many this report lost to the cap or its values.
	Families       int `json:"families"`
	Series         int `json:"series"`
	SeriesExported int `json:"series_exported"`
	SeriesDropped  int `json:"series_dropped"`
	// Sample is the first SampleSize entries of the report.
	Sample []Entry `json:"sample"`
}

// VM summarizes one VM; ok is false when the source does not know it.
func (r *Registry) VM(id string) (VMMetrics, bool) {
	src := r.host.source()
	if src == nil {
		return VMMetrics{}, false
	}
	var snap Snapshot
	found := false
	for _, v := range src.VMs() {
		if v.ID == id {
			snap, found = v, true
			break
		}
	}
	if !found {
		return VMMetrics{}, false
	}
	m := VMMetrics{Host: HostMetrics{
		State:       snap.State,
		Attestation: snap.Attestation,
		PID:         snap.PID,
		DiskBytes:   snap.DiskBytes,
		Network:     NetworkMetrics{Name: snap.Network},
	}}
	if d, ok := snap.InstallSeconds(); ok {
		m.Host.InstallSeconds = &d
	}
	if d, ok := snap.BootToReadySeconds(); ok {
		m.Host.BootToReadySeconds = &d
	}
	if st, ok := r.host.procStats(snap); ok {
		m.Host.CPUSeconds = &st.CPUSeconds
		m.Host.MemoryRSSBytes = &st.RSSBytes
	}
	for _, n := range src.Networks() {
		if n.Name == snap.Network {
			m.Host.Network = NetworkMetrics(n)
			break
		}
	}
	if rep := r.guest.report(id); rep != nil {
		m.Guest = &GuestMetrics{
			ReceivedAt:       rep.receivedAt,
			ReportAgeSeconds: r.guest.now().Sub(rep.receivedAt).Seconds(),
			Families:         rep.families,
			Series:           rep.entries,
			SeriesExported:   len(rep.series),
			Sample:           rep.sample,
		}
		if !rep.generatedAt.IsZero() {
			m.Guest.GeneratedAt = &rep.generatedAt
		}
		for _, n := range rep.dropped {
			m.Guest.SeriesDropped += n
		}
	}
	return m, true
}
