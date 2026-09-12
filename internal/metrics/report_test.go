package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixture is the trimmed output of `systemd-report generate -j` on systemd
// 261: 137 entries over all 22 families, up to 8 per family.
func fixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "report.json"))
	require.NoError(t, err)
	return raw
}

func TestParseReportFixture(t *testing.T) {
	r, err := ParseReport(fixture(t))
	require.NoError(t, err)
	assert.Equal(t, ReportMediaType, r.MediaType)
	generated, ok := r.GeneratedAt()
	require.True(t, ok, "the systemd timestamp format parses")
	assert.Equal(t, time.Date(2026, 9, 12, 14, 17, 48, 0, time.UTC), generated.UTC())
	assert.Equal(t, 22, r.Families())
	assert.Len(t, r.Metrics, 137)

	_, err = ParseReport([]byte(`not json`))
	require.Error(t, err)
	r, err = ParseReport([]byte(`{"unknown":true}`))
	require.NoError(t, err, "a report without metrics is empty, not invalid")
	assert.Empty(t, r.Metrics)
	_, ok = r.GeneratedAt()
	assert.False(t, ok)
}

// TestCounterTableMatchesDescribe pins counterFamilies to what
// io.systemd.Metrics.Describe says on systemd 261 (testdata/describe.json).
func TestCounterTableMatchesDescribe(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "describe.json"))
	require.NoError(t, err)
	var described []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(raw, &described))
	require.NotEmpty(t, described)
	counters := 0
	for _, d := range described {
		if d.Type == "counter" {
			counters++
		}
		assert.Equal(t, d.Type == "counter", counterFamilies[d.Name], d.Name)
	}
	assert.Len(t, counterFamilies, counters, "no counter the describe output does not know")
}

func TestFamilyName(t *testing.T) {
	for in, want := range map[string]string{
		"io.systemd.Manager.UnitsByTypeTotal":  "io_systemd_manager_units_by_type_total",
		"io.systemd.Network.IPv4AddressState":  "io_systemd_network_ipv4_address_state",
		"io.systemd.Manager.NRestarts":         "io_systemd_manager_nrestarts",
		"io.systemd.Manager.StatusErrno":       "io_systemd_manager_status_errno",
		"org.example.HTTP2Server.requests-ok!": "org_example_http2_server_requests_ok",
		"..weird..":                            "weird",
	} {
		assert.Equal(t, want, familyName(in), in)
	}
}

func TestLabelName(t *testing.T) {
	for in, want := range map[string]string{
		"load_state": "load_state",
		"event":      "event",
		"vm":         "field_vm",
		"object":     "field_object",
		"value":      "field_value",
		"__meta":     "meta",
		"1st":        "field_1st",
		"a-b c":      "a_b_c",
		"":           "field_",
	} {
		assert.Equal(t, want, labelName(in), in)
	}
}

func TestConvert(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
		want  series
		ok    bool
	}{
		{
			name:  "counter family",
			entry: Entry{Name: "io.systemd.Manager.NRestarts", Object: "sshd.service", Value: 3.0},
			want: series{name: "vm_guest_io_systemd_manager_nrestarts_total", family: "io.systemd.Manager.NRestarts", kind: kindCounter,
				labels: map[string]string{"object": "sshd.service"}, value: 3},
			ok: true,
		},
		{
			name:  "gauge with fields and no object",
			entry: Entry{Name: "io.systemd.Manager.UnitsByLoadStateTotal", Value: 403.0, Fields: map[string]any{"load_state": "loaded"}},
			want: series{name: "vm_guest_io_systemd_manager_units_by_load_state_total", family: "io.systemd.Manager.UnitsByLoadStateTotal", kind: kindGauge,
				labels: map[string]string{"object": "", "load_state": "loaded"}, value: 403},
			ok: true,
		},
		{
			name:  "string becomes info",
			entry: Entry{Name: "io.systemd.Manager.UnitActiveState", Object: "sshd.service", Value: "active"},
			want: series{name: "vm_guest_io_systemd_manager_unit_active_state_info", family: "io.systemd.Manager.UnitActiveState", kind: kindInfo,
				labels: map[string]string{"object": "sshd.service", "value": "active"}, value: 1},
			ok: true,
		},
		{
			name:  "bool is a 0/1 gauge, numeric fields render",
			entry: Entry{Name: "org.example.Flag", Value: true, Fields: map[string]any{"slot": 2.0, "on": false}},
			want: series{name: "vm_guest_org_example_flag", family: "org.example.Flag", kind: kindGauge,
				labels: map[string]string{"object": "", "slot": "2", "on": "false"}, value: 1},
			ok: true,
		},
		{
			name:  "declared counter type wins over the table",
			entry: Entry{Name: "org.example.Bytes", Type: "counter", Value: 10.0},
			want: series{name: "vm_guest_org_example_bytes_total", family: "org.example.Bytes", kind: kindCounter,
				labels: map[string]string{"object": ""}, value: 10},
			ok: true,
		},
		{
			name:  "declared string type renders numbers as info",
			entry: Entry{Name: "org.example.Code", Type: "string", Value: 7.0},
			want: series{name: "vm_guest_org_example_code_info", family: "org.example.Code", kind: kindInfo,
				labels: map[string]string{"object": "", "value": "7"}, value: 1},
			ok: true,
		},
		{
			name:  "reserved and invalid field names are prefixed",
			entry: Entry{Name: "org.example.X", Value: 1.0, Fields: map[string]any{"vm": "spoof", "__hidden": "h"}},
			want: series{name: "vm_guest_org_example_x", family: "org.example.X", kind: kindGauge,
				labels: map[string]string{"object": "", "field_vm": "spoof", "hidden": "h"}, value: 1},
			ok: true,
		},
		{name: "null value", entry: Entry{Name: "org.example.Null", Value: nil}},
		{name: "array value", entry: Entry{Name: "org.example.List", Value: []any{1.0}}},
		{name: "object value", entry: Entry{Name: "org.example.Map", Value: map[string]any{"a": 1.0}}},
		{name: "empty name", entry: Entry{Value: 1.0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := convert(tc.entry)
			require.Equal(t, tc.ok, ok)
			if ok {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}
