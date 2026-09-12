package vm

import (
	"time"

	"github.com/giantswarm/vm-manager/internal/metrics"
)

// States lists every lifecycle state, in lifecycle order; the metrics
// enumerate it.
var States = []State{
	StateCreating, StateInstalling, StateBooting, StateAttesting, StateReady,
	StateRunning, StateStopping, StateStopped, StateFailed, StateDeleting,
}

// StateNames is States as strings, what metrics.Options.States takes.
func StateNames() []string {
	out := make([]string, len(States))
	for i, s := range States {
		out[i] = string(s)
	}
	return out
}

var _ metrics.Source = (*Service)(nil)

// VMs implements metrics.Source: one snapshot per VM record.
func (s *Service) VMs() []metrics.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]metrics.Snapshot, 0, len(s.vms))
	for _, e := range s.vms {
		out = append(out, snapshotOf(e))
	}
	return out
}

// Networks implements metrics.Source: the counters of every network.
func (s *Service) Networks() []metrics.NetworkSnapshot {
	nets := s.opts.Networks.List()
	out := make([]metrics.NetworkSnapshot, 0, len(nets))
	for _, n := range nets {
		st := n.Stats()
		out = append(out, metrics.NetworkSnapshot{Name: n.Name(), BytesSent: st.BytesSent, BytesReceived: st.BytesReceived, Leases: st.Attachments})
	}
	return out
}

// snapshotOf reads an entry; the caller holds s.mu. The disk size is the
// open volume's, else the provisioned size of the spec.
func snapshotOf(e *entry) metrics.Snapshot {
	rec := &e.rec
	snap := metrics.Snapshot{
		ID:          rec.ID,
		Name:        rec.Name,
		State:       string(rec.State),
		Network:     rec.Network,
		IP:          rec.IP,
		Image:       rec.Image,
		Attestation: attestationState(rec.Attestation),
		DiskBytes:   int64(rec.DiskGiB) << 30,
		CreatedAt:   rec.CreatedAt,
		InstalledAt: deref(rec.InstalledAt),
		BootedAt:    deref(rec.BootedAt),
		ReadyAt:     deref(rec.ReadyAt),
	}
	if e.volume != nil && e.volume.SizeBytes > 0 {
		snap.DiskBytes = e.volume.SizeBytes
	}
	if e.proc != nil && e.proc.inst != nil {
		snap.PID = e.proc.inst.PID()
	}
	return snap
}

// attestationState folds the attestation record into the metrics states.
func attestationState(a Attestation) string {
	switch {
	case !a.Required:
		return metrics.AttestationDisabled
	case a.UserDataReleased:
		return metrics.AttestationReleased
	case a.Initrd != nil && !a.Initrd.Verified:
		return metrics.AttestationFailed
	default:
		return metrics.AttestationPending
	}
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
