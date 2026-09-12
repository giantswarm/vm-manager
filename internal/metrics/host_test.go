package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSource is a Source with fixed snapshots.
type fakeSource struct {
	vms  []Snapshot
	nets []NetworkSnapshot
}

func (f *fakeSource) VMs() []Snapshot             { return f.vms }
func (f *fakeSource) Networks() []NetworkSnapshot { return f.nets }

// procRoot writes a /proc/<pid>/stat and statm pair: the command name has a
// space in it, utime is 150 and stime 50 ticks (2 s at 100 Hz), the resident
// set 2500 pages.
func procRoot(t *testing.T, pid int) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	stat := strconv.Itoa(pid) + " (qemu-system x86) S 1 4242 4242 0 -1 4194560 12345 0 0 0 150 50 0 0 20 0 9 0 12345 1234567890 2500 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "statm"), []byte("10000 2500 800 100 0 3000 0\n"), 0o600))
	return root
}

func testSource() *fakeSource {
	created := time.Unix(1_700_000_000, 0).UTC()
	return &fakeSource{
		vms: []Snapshot{
			{
				ID: "vm1", Name: "node1", State: "ready", Network: "lan", IP: "10.0.0.2", Image: "base_0.1.0",
				Attestation: AttestationReleased, PID: 4242, DiskBytes: 20 << 30,
				CreatedAt: created, InstalledAt: created.Add(40 * time.Second),
				BootedAt: created.Add(41 * time.Second), ReadyAt: created.Add(47500 * time.Millisecond),
			},
			{
				// Still installing: no process to read (the pid does not exist
				// below the proc root), no durations yet, a state outside the
				// configured list.
				ID: "vm2", Name: "node2", State: "installing", Network: "lan", IP: "10.0.0.3", Image: "base_0.1.0",
				Attestation: AttestationPending, PID: 99999, DiskBytes: 10 << 30, CreatedAt: created,
			},
		},
		nets: []NetworkSnapshot{{Name: "lan", BytesSent: 100, BytesReceived: 200, Leases: 2}},
	}
}

func TestHostExposition(t *testing.T) {
	r, _ := newTestRegistry(t, Options{States: []string{"ready", "stopped"}, ProcRoot: procRoot(t, 4242), ClockTicks: 100, PageSize: 4096})
	r.SetSource(testSource())
	expected := `
# HELP vm_manager_network_bytes_total Bytes moved through the virtual network, direction sent (to VMs) or received (from VMs).
# TYPE vm_manager_network_bytes_total counter
vm_manager_network_bytes_total{direction="received",network="lan"} 200
vm_manager_network_bytes_total{direction="sent",network="lan"} 100
# HELP vm_manager_network_leases VMs holding a lease on the virtual network.
# TYPE vm_manager_network_leases gauge
vm_manager_network_leases{network="lan"} 2
# HELP vm_manager_vm_attestation 1 for the VM's attestation state (disabled, pending, released, failed), 0 for the others.
# TYPE vm_manager_vm_attestation gauge
vm_manager_vm_attestation{name="node1",state="disabled",vm="vm1"} 0
vm_manager_vm_attestation{name="node1",state="failed",vm="vm1"} 0
vm_manager_vm_attestation{name="node1",state="pending",vm="vm1"} 0
vm_manager_vm_attestation{name="node1",state="released",vm="vm1"} 1
vm_manager_vm_attestation{name="node2",state="disabled",vm="vm2"} 0
vm_manager_vm_attestation{name="node2",state="failed",vm="vm2"} 0
vm_manager_vm_attestation{name="node2",state="pending",vm="vm2"} 1
vm_manager_vm_attestation{name="node2",state="released",vm="vm2"} 0
# HELP vm_manager_vm_boot_to_ready_seconds Seconds from the current installed boot's start to READY=1; absent until ready.
# TYPE vm_manager_vm_boot_to_ready_seconds gauge
vm_manager_vm_boot_to_ready_seconds{name="node1",vm="vm1"} 6.5
# HELP vm_manager_vm_cpu_seconds_total CPU time of the VM's QEMU process (user and system).
# TYPE vm_manager_vm_cpu_seconds_total counter
vm_manager_vm_cpu_seconds_total{name="node1",vm="vm1"} 2
# HELP vm_manager_vm_created_timestamp_seconds Unix time the VM record was created.
# TYPE vm_manager_vm_created_timestamp_seconds gauge
vm_manager_vm_created_timestamp_seconds{name="node1",vm="vm1"} 1.7e+09
vm_manager_vm_created_timestamp_seconds{name="node2",vm="vm2"} 1.7e+09
# HELP vm_manager_vm_disk_bytes Provisioned size of the VM's volume.
# TYPE vm_manager_vm_disk_bytes gauge
vm_manager_vm_disk_bytes{name="node1",vm="vm1"} 2.147483648e+10
vm_manager_vm_disk_bytes{name="node2",vm="vm2"} 1.073741824e+10
# HELP vm_manager_vm_info Constant 1 with the VM's name, network, IP and image for joins.
# TYPE vm_manager_vm_info gauge
vm_manager_vm_info{image="base_0.1.0",ip="10.0.0.2",name="node1",network="lan",vm="vm1"} 1
vm_manager_vm_info{image="base_0.1.0",ip="10.0.0.3",name="node2",network="lan",vm="vm2"} 1
# HELP vm_manager_vm_install_seconds Seconds from creation to the end of the installer boot; absent until installed.
# TYPE vm_manager_vm_install_seconds gauge
vm_manager_vm_install_seconds{name="node1",vm="vm1"} 40
# HELP vm_manager_vm_memory_rss_bytes Resident memory of the VM's QEMU process.
# TYPE vm_manager_vm_memory_rss_bytes gauge
vm_manager_vm_memory_rss_bytes{name="node1",vm="vm1"} 1.024e+07
# HELP vm_manager_vm_state 1 for the VM's current lifecycle state, 0 for the others.
# TYPE vm_manager_vm_state gauge
vm_manager_vm_state{name="node1",state="ready",vm="vm1"} 1
vm_manager_vm_state{name="node1",state="stopped",vm="vm1"} 0
vm_manager_vm_state{name="node2",state="installing",vm="vm2"} 1
vm_manager_vm_state{name="node2",state="ready",vm="vm2"} 0
vm_manager_vm_state{name="node2",state="stopped",vm="vm2"} 0
# HELP vm_manager_vms Number of VMs per lifecycle state.
# TYPE vm_manager_vms gauge
vm_manager_vms{state="installing"} 1
vm_manager_vms{state="ready"} 1
vm_manager_vms{state="stopped"} 0
`
	require.NoError(t, testutil.GatherAndCompare(r.Gatherer(), strings.NewReader(expected),
		"vm_manager_network_bytes_total", "vm_manager_network_leases", "vm_manager_vm_attestation",
		"vm_manager_vm_boot_to_ready_seconds", "vm_manager_vm_cpu_seconds_total", "vm_manager_vm_created_timestamp_seconds",
		"vm_manager_vm_disk_bytes", "vm_manager_vm_info", "vm_manager_vm_install_seconds", "vm_manager_vm_memory_rss_bytes",
		"vm_manager_vm_state", "vm_manager_vms"))
}

func TestProcReader(t *testing.T) {
	p := procReader{root: procRoot(t, 7), ticks: 100, pageSize: 4096}
	st, err := p.read(7)
	require.NoError(t, err)
	assert.Equal(t, procStats{CPUSeconds: 2, RSSBytes: 10_240_000}, st)
	_, err = p.read(8)
	require.Error(t, err, "a gone process is an error, not zeros")

	dir := filepath.Join(p.root, "9")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte("9 (x) S 1 2\n"), 0o600))
	_, err = p.read(9)
	assert.ErrorContains(t, err, "fields after the command name")
}

func TestObserveAndHandler(t *testing.T) {
	r, _ := newTestRegistry(t, Options{Version: "1.2.3", Commit: "abc"})
	r.ObserveInstall(30 * time.Second)
	r.ObserveBootToReady(7 * time.Second)
	r.ObserveBootToReady(9 * time.Second)
	fams := gather(t, r)
	install := fams["vm_manager_vm_install_duration_seconds"].GetMetric()[0].GetHistogram()
	assert.Equal(t, uint64(1), install.GetSampleCount())
	assert.Equal(t, float64(30), install.GetSampleSum())
	boot := fams["vm_manager_vm_boot_to_ready_duration_seconds"].GetMetric()[0].GetHistogram()
	assert.Equal(t, uint64(2), boot.GetSampleCount())
	assert.Equal(t, float64(16), boot.GetSampleSum())
	assert.Equal(t, map[string]string{"version": "1.2.3", "commit": "abc", "go_version": fams["vm_manager_build_info"].GetMetric()[0].GetLabel()[1].GetValue()},
		labelsOf(fams["vm_manager_build_info"].GetMetric()[0]))

	ts := httptest.NewServer(r.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
	body := make([]byte, 1<<20)
	n, _ := resp.Body.Read(body)
	assert.Contains(t, string(body[:n]), "vm_manager_build_info{")
	assert.Contains(t, string(body[:n]), "go_goroutines", "the process's own runtime metrics are included")
}

func TestVMSummary(t *testing.T) {
	r, clock := newTestRegistry(t, Options{States: []string{"ready"}, ProcRoot: procRoot(t, 4242), ClockTicks: 100, PageSize: 4096})
	_, ok := r.VM("vm1")
	assert.False(t, ok, "no source yet")
	r.SetSource(testSource())
	_, ok = r.VM("nope")
	assert.False(t, ok)

	m, ok := r.VM("vm1")
	require.True(t, ok)
	require.NotNil(t, m.Host.CPUSeconds)
	require.NotNil(t, m.Host.MemoryRSSBytes)
	require.NotNil(t, m.Host.InstallSeconds)
	require.NotNil(t, m.Host.BootToReadySeconds)
	assert.Equal(t, "ready", m.Host.State)
	assert.Equal(t, AttestationReleased, m.Host.Attestation)
	assert.Equal(t, 4242, m.Host.PID)
	assert.Equal(t, float64(2), *m.Host.CPUSeconds)
	assert.Equal(t, int64(10_240_000), *m.Host.MemoryRSSBytes)
	assert.Equal(t, int64(20<<30), m.Host.DiskBytes)
	assert.Equal(t, float64(40), *m.Host.InstallSeconds)
	assert.Equal(t, 6.5, *m.Host.BootToReadySeconds)
	assert.Equal(t, NetworkMetrics{Name: "lan", BytesSent: 100, BytesReceived: 200, Leases: 2}, m.Host.Network)
	assert.Nil(t, m.Guest, "no upload yet")

	m, ok = r.VM("vm2")
	require.True(t, ok)
	assert.Nil(t, m.Host.CPUSeconds, "no readable process")
	assert.Nil(t, m.Host.InstallSeconds, "not installed yet")

	require.NoError(t, r.StoreReport(context.Background(), "vm1", fixture(t)))
	clock.now = t0.Add(5 * time.Second)
	m, _ = r.VM("vm1")
	require.NotNil(t, m.Guest)
	assert.Equal(t, t0, m.Guest.ReceivedAt)
	require.NotNil(t, m.Guest.GeneratedAt)
	assert.Equal(t, time.Date(2026, 9, 12, 14, 17, 48, 0, time.UTC), m.Guest.GeneratedAt.UTC())
	assert.Equal(t, float64(5), m.Guest.ReportAgeSeconds)
	assert.Equal(t, 22, m.Guest.Families)
	assert.Equal(t, 137, m.Guest.Series)
	assert.Equal(t, 137, m.Guest.SeriesExported)
	assert.Equal(t, 0, m.Guest.SeriesDropped)
	require.Len(t, m.Guest.Sample, SampleSize)
	assert.Equal(t, Entry{Name: "io.systemd.Manager.ActiveTimestamp", Object: "initrd.target", Fields: map[string]any{"event": "enter"}, Value: float64(1789199899473434)}, m.Guest.Sample[0])
}
