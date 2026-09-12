// Package metrics is the Prometheus registry of vm-manager: host-side
// metrics about every VM and the metrics the guests upload with
// systemd-report, served on /metrics and summarized per VM for
// get_vm_metrics.
//
// # Host side (vm_manager_*)
//
// The host collector reads a Source (the VM service) on every scrape and
// emits, per VM, its state and attestation state as 1/0 series, the
// timestamps-derived install and boot-to-ready durations, the provisioned
// disk size, and the CPU time and resident memory of its QEMU process read
// from /proc/<pid>/stat and statm. Per network it emits the byte counters
// and the lease count. Install and boot-to-ready durations are also
// histograms, observed by the VM service on each transition. The VM id is
// the vm label everywhere; vm_manager_vm_info carries name, network, IP
// and image for joins.
//
// # Guest side (vm_guest_*)
//
// systemd-report upload POSTs the report of systemd-report generate
// (Content-Type: application/json, mediaType application/vnd.io.systemd.report)
// to the IMDS /report endpoint; the VM service persists it and hands it to
// Registry.StoreReport. Each entry of the report becomes one series:
//
//   - the family name is snake-cased and prefixed: io.systemd.Manager.NRestarts
//     becomes vm_guest_io_systemd_manager_nrestarts_total;
//   - the entry's object (a unit, an interface) is the object label, its
//     fields become labels of the same name;
//   - numeric values are gauges, or counters (with the _total suffix) for the
//     families io.systemd.Metrics.Describe declares as counters; string values
//     become info-style series named <family>_info with the string in the
//     value label and a value of 1.
//
// A report replaces the previous one of that VM wholesale, so a family that
// disappears from the guest disappears from the exposition. Every VM also
// gets vm_guest_report_age_seconds, vm_guest_report_series,
// vm_guest_reports_total and vm_guest_report_series_dropped_total.
//
// # Cardinality policy
//
// The guest decides what it reports, so the host bounds it: at most
// Options.MaxGuestSeries series per VM are kept from one report (the first
// ones in report order; DefaultMaxGuestSeries is 1000), the rest are counted
// in vm_guest_report_series_dropped_total{reason="limit"}. Entries whose value
// cannot be represented (null, arrays, objects) and duplicate series within
// one report are dropped and counted with reason unsupported and duplicate.
// Series of a deleted VM are removed with Registry.ForgetVM.
package metrics
