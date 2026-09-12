package metrics

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

var t0 = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

// testClock is a settable Options.Now.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestRegistry(t *testing.T, opts Options) (*Registry, *testClock) {
	t.Helper()
	clock := &testClock{now: t0}
	opts.Now = clock.Now
	opts.Logger = quiet()
	if opts.ProcRoot == "" {
		opts.ProcRoot = t.TempDir()
	}
	return New(opts), clock
}

// gather indexes the exposition by family name.
func gather(t *testing.T, r *Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := r.Gatherer().Gather()
	require.NoError(t, err, "the exposition is consistent")
	out := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		out[f.GetName()] = f
	}
	return out
}

func labelsOf(m *dto.Metric) map[string]string {
	out := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		out[l.GetName()] = l.GetValue()
	}
	return out
}

func TestGuestFixtureExposition(t *testing.T) {
	r, clock := newTestRegistry(t, Options{})
	require.NoError(t, r.StoreReport(context.Background(), "vm1", fixture(t)))
	clock.now = t0.Add(30 * time.Second)
	fams := gather(t, r)

	restarts := fams["vm_guest_io_systemd_manager_nrestarts_total"]
	require.NotNil(t, restarts, "a counter family gets the _total suffix")
	assert.Equal(t, dto.MetricType_COUNTER, restarts.GetType())
	assert.Equal(t, "Guest systemd-report family io.systemd.Manager.NRestarts.", restarts.GetHelp())
	assert.Len(t, restarts.GetMetric(), 8)
	// Gather sorts a family's series by label values.
	assert.Equal(t, map[string]string{"vm": "vm1", "object": "accounts-daemon.service"}, labelsOf(restarts.GetMetric()[0]))

	active := fams["vm_guest_io_systemd_manager_unit_active_state_info"]
	require.NotNil(t, active, "a string family becomes an info series")
	assert.Equal(t, dto.MetricType_GAUGE, active.GetType())
	assert.Equal(t, float64(1), active.GetMetric()[0].GetGauge().GetValue())
	assert.Contains(t, labelsOf(active.GetMetric()[0]), "value")
	assert.Equal(t, "vm1", labelsOf(active.GetMetric()[0])["vm"])

	stamps := fams["vm_guest_io_systemd_manager_active_timestamp"]
	require.NotNil(t, stamps)
	assert.Equal(t, dto.MetricType_GAUGE, stamps.GetType())
	assert.Equal(t, map[string]string{"vm": "vm1", "object": "initrd.target", "event": "enter"}, labelsOf(stamps.GetMetric()[0]), "fields are labels")
	assert.Equal(t, float64(1789199899473434), stamps.GetMetric()[0].GetGauge().GetValue())

	byLoad := fams["vm_guest_io_systemd_manager_units_by_load_state_total"]
	require.NotNil(t, byLoad)
	assert.Equal(t, dto.MetricType_GAUGE, byLoad.GetType(), "a gauge keeps its own Total suffix without becoming a counter")
	assert.Equal(t, map[string]string{"vm": "vm1", "object": "", "load_state": "bad-setting"}, labelsOf(byLoad.GetMetric()[0]), "object is empty for host-wide families")

	assert.NotNil(t, fams["vm_guest_io_systemd_network_ipv4_address_state_info"], "acronyms with digits snake-case as one word")

	expected := `
# HELP vm_guest_report_age_seconds Seconds since the VM's last systemd-report upload was received.
# TYPE vm_guest_report_age_seconds gauge
vm_guest_report_age_seconds{vm="vm1"} 30
# HELP vm_guest_report_series Series exported from the VM's last systemd-report upload.
# TYPE vm_guest_report_series gauge
vm_guest_report_series{vm="vm1"} 137
# HELP vm_guest_report_series_dropped_total Report entries not exported, by reason: limit (per-VM series cap), unsupported (value type), duplicate.
# TYPE vm_guest_report_series_dropped_total counter
vm_guest_report_series_dropped_total{reason="duplicate",vm="vm1"} 0
vm_guest_report_series_dropped_total{reason="limit",vm="vm1"} 0
vm_guest_report_series_dropped_total{reason="unsupported",vm="vm1"} 0
# HELP vm_guest_reports_total systemd-report uploads received from the VM.
# TYPE vm_guest_reports_total counter
vm_guest_reports_total{vm="vm1"} 1
`
	require.NoError(t, testutil.GatherAndCompare(r.Gatherer(), strings.NewReader(expected),
		"vm_guest_report_age_seconds", "vm_guest_report_series", "vm_guest_report_series_dropped_total", "vm_guest_reports_total"))
}

func TestGuestCardinalityCap(t *testing.T) {
	r, _ := newTestRegistry(t, Options{MaxGuestSeries: 10})
	require.NoError(t, r.StoreReport(context.Background(), "vm1", fixture(t)))
	fams := gather(t, r)
	total := 0
	for name, f := range fams {
		if strings.HasPrefix(name, "vm_guest_io_") {
			total += len(f.GetMetric())
		}
	}
	assert.Equal(t, 10, total, "the first entries in report order are kept")
	assert.Len(t, fams["vm_guest_io_systemd_manager_active_timestamp"].GetMetric(), 8)
	assert.Len(t, fams["vm_guest_io_systemd_manager_inactive_exit_timestamp"].GetMetric(), 2)

	// A second upload replaces the series; the dropped counter accumulates.
	require.NoError(t, r.StoreReport(context.Background(), "vm1", fixture(t)))
	expected := `
# HELP vm_guest_report_series Series exported from the VM's last systemd-report upload.
# TYPE vm_guest_report_series gauge
vm_guest_report_series{vm="vm1"} 10
# HELP vm_guest_report_series_dropped_total Report entries not exported, by reason: limit (per-VM series cap), unsupported (value type), duplicate.
# TYPE vm_guest_report_series_dropped_total counter
vm_guest_report_series_dropped_total{reason="duplicate",vm="vm1"} 0
vm_guest_report_series_dropped_total{reason="limit",vm="vm1"} 254
vm_guest_report_series_dropped_total{reason="unsupported",vm="vm1"} 0
# HELP vm_guest_reports_total systemd-report uploads received from the VM.
# TYPE vm_guest_reports_total counter
vm_guest_reports_total{vm="vm1"} 2
`
	require.NoError(t, testutil.GatherAndCompare(r.Gatherer(), strings.NewReader(expected),
		"vm_guest_report_series", "vm_guest_report_series_dropped_total", "vm_guest_reports_total"))

	m, ok := r.VM("vm1")
	assert.False(t, ok, "no source, no VM summary")
	assert.Nil(t, m.Guest)
}

func TestGuestUnsupportedDuplicateAndForget(t *testing.T) {
	r, _ := newTestRegistry(t, Options{})
	report := `{"metrics":[
		{"name":"org.example.Gauge","object":"a","value":1},
		{"name":"org.example.Gauge","object":"a","value":2},
		{"name":"org.example.Null","value":null},
		{"name":"org.example.List","value":[1,2]},
		{"name":"org.example.Flag","value":true}
	]}`
	require.NoError(t, r.StoreReport(context.Background(), "vm1", json.RawMessage(report)))
	fams := gather(t, r)
	require.Len(t, fams["vm_guest_org_example_gauge"].GetMetric(), 1)
	assert.Equal(t, float64(1), fams["vm_guest_org_example_gauge"].GetMetric()[0].GetGauge().GetValue(), "the first duplicate wins")
	assert.Equal(t, float64(1), fams["vm_guest_org_example_flag"].GetMetric()[0].GetGauge().GetValue())
	assert.Nil(t, fams["vm_guest_org_example_null"])
	assert.Nil(t, fams["vm_guest_org_example_list"])
	dropped := map[string]float64{}
	for _, m := range fams["vm_guest_report_series_dropped_total"].GetMetric() {
		dropped[labelsOf(m)["reason"]] = m.GetCounter().GetValue()
	}
	assert.Equal(t, map[string]float64{"duplicate": 1, "unsupported": 2, "limit": 0}, dropped)

	require.Error(t, r.StoreReport(context.Background(), "vm1", json.RawMessage(`nope`)), "an unparseable upload is reported")
	assert.Len(t, gather(t, r)["vm_guest_org_example_gauge"].GetMetric(), 1, "and keeps the previous report")

	r.ForgetVM("vm1")
	fams = gather(t, r)
	for name := range fams {
		assert.NotContains(t, name, "vm_guest_", "a deleted VM leaves no guest series behind")
	}
}

func TestGuestLabelUnionAcrossVMs(t *testing.T) {
	r, _ := newTestRegistry(t, Options{})
	require.NoError(t, r.StoreReport(context.Background(), "vm1", json.RawMessage(`{"metrics":[{"name":"org.example.G","value":1,"fields":{"a":"x"}}]}`)))
	require.NoError(t, r.StoreReport(context.Background(), "vm2", json.RawMessage(`{"metrics":[{"name":"org.example.G","value":2,"fields":{"b":"y"}}]}`)))
	fams := gather(t, r)
	g := fams["vm_guest_org_example_g"]
	require.Len(t, g.GetMetric(), 2, "different field sets for one family still gather")
	byVM := map[string]map[string]string{}
	for _, m := range g.GetMetric() {
		byVM[labelsOf(m)["vm"]] = labelsOf(m)
	}
	assert.Equal(t, map[string]string{"vm": "vm1", "object": "", "a": "x", "b": ""}, byVM["vm1"])
	assert.Equal(t, map[string]string{"vm": "vm2", "object": "", "a": "", "b": "y"}, byVM["vm2"])
}
