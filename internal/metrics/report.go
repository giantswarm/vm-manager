package metrics

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ReportMediaType is the mediaType systemd-report generate writes.
const ReportMediaType = "application/vnd.io.systemd.report"

// reportTimeLayout is the report's timestamp format: "Sat 2026-09-12 14:17:48 UTC".
const reportTimeLayout = "Mon 2006-01-02 15:04:05 MST"

// Report is what systemd-report generate produces and systemd-report upload
// POSTs: a flat list of metric entries.
type Report struct {
	MediaType string  `json:"mediaType"`
	Timestamp string  `json:"timestamp"`
	Metrics   []Entry `json:"metrics"`
}

// Entry is one metric of a report: the family name, the optional object
// the value is about (a unit, a network interface), optional further
// dimensions and the value itself.
type Entry struct {
	Name   string         `json:"name"`
	Object string         `json:"object,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
	Value  any            `json:"value"`
	// Type is the io.systemd.Metrics family type (counter, gauge or string)
	// when a report carries it. systemd 261 reports do not: the type is then
	// taken from the value and the counter table, see kindOf.
	Type string `json:"type,omitempty"`
}

// ParseReport decodes a report; it accepts any JSON object that has a
// metrics array so a newer systemd can add top-level keys.
func ParseReport(raw []byte) (*Report, error) {
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode report: %w", err)
	}
	return &r, nil
}

// GeneratedAt parses the report's timestamp; ok is false when it is absent
// or in a format this package does not know.
func (r *Report) GeneratedAt() (t time.Time, ok bool) {
	t, err := time.Parse(reportTimeLayout, r.Timestamp)
	return t, err == nil
}

// Families counts the distinct family names.
func (r *Report) Families() int {
	seen := make(map[string]struct{}, len(r.Metrics))
	for _, e := range r.Metrics {
		seen[e.Name] = struct{}{}
	}
	return len(seen)
}

// kind is how an entry is exposed.
type kind int

const (
	kindGauge kind = iota
	kindCounter
	// kindInfo is a string value: a gauge of 1 with the string in the value label.
	kindInfo
)

// counterFamilies are the families io.systemd.Metrics.Describe types as
// counter in systemd 261 (testdata/describe.json); reports carry no type,
// so this is how a numeric value becomes a counter. Extend it when a newer
// systemd adds counters; a wrong guess only costs the _total suffix.
var counterFamilies = map[string]bool{
	"io.systemd.Manager.NRestarts":   true,
	"io.systemd.Manager.ReloadCount": true,
}

// Labels every guest series carries or reserves.
const (
	labelVM     = "vm"
	labelObject = "object"
	labelValue  = "value"
)

// kindOf picks the exposition of an entry: the declared type when present,
// else strings are info series and numbers gauges, or counters for the
// families known to be counters. ok is false for values Prometheus cannot
// carry (null, arrays, objects).
func kindOf(e Entry) (k kind, value float64, str string, ok bool) {
	switch e.Type {
	case "counter":
		k = kindCounter
	case "gauge":
		k = kindGauge
	case "string":
		k = kindInfo
	default:
		switch e.Value.(type) {
		case string:
			k = kindInfo
		case float64, bool:
			k = kindGauge
			if counterFamilies[e.Name] {
				k = kindCounter
			}
		default:
			return 0, 0, "", false
		}
	}
	if k == kindInfo {
		if e.Value == nil {
			return 0, 0, "", false
		}
		return k, 1, labelString(e.Value), true
	}
	switch v := e.Value.(type) {
	case float64:
		return k, v, "", true
	case bool:
		if v {
			return k, 1, "", true
		}
		return k, 0, "", true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, 0, "", false
		}
		return k, f, "", true
	default:
		return 0, 0, "", false
	}
}

// series is one guest entry converted for exposition; labels exclude vm.
type series struct {
	name   string
	family string
	kind   kind
	labels map[string]string
	value  float64
}

// key identifies the series within one report for duplicate detection.
func (s series) key() string {
	keys := make([]string, 0, len(s.labels))
	for k := range s.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.name)
	for _, k := range keys {
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(s.labels[k])
	}
	return b.String()
}

// convert turns an entry into a series under the vm_guest_ prefix; ok is
// false when the value cannot be represented.
func convert(e Entry) (series, bool) {
	k, value, str, ok := kindOf(e)
	if !ok || e.Name == "" {
		return series{}, false
	}
	name := guestPrefix + familyName(e.Name)
	switch k {
	case kindCounter:
		if !strings.HasSuffix(name, "_total") {
			name += "_total"
		}
	case kindInfo:
		name += "_info"
	}
	labels := make(map[string]string, len(e.Fields)+2)
	labels[labelObject] = boundLabel(e.Object)
	for f, v := range e.Fields {
		labels[labelName(f)] = boundLabel(labelString(v))
	}
	if k == kindInfo {
		labels[labelValue] = boundLabel(str)
	}
	return series{name: name, family: e.Name, kind: k, labels: labels, value: value}, true
}

// maxLabelValueBytes bounds every guest-supplied label value. The guest
// controls object, field and string values; without a bound a single
// oversized value would bloat every /metrics scrape for as long as that
// report is the VM's latest.
const maxLabelValueBytes = 128

// boundLabel truncates a label value to maxLabelValueBytes at a rune
// boundary and marks the cut with an ellipsis.
func boundLabel(v string) string {
	if len(v) <= maxLabelValueBytes {
		return v
	}
	cut := maxLabelValueBytes - 1
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return v[:cut] + "…"
}

// familyName snake-cases a dotted CamelCase family name into a metric name
// part: io.systemd.Network.IPv4AddressState -> io_systemd_network_ipv4_address_state.
func familyName(name string) string {
	var b strings.Builder
	b.Grow(len(name) + 8)
	prev := '_'
	for _, r := range name {
		switch {
		case unicode.IsUpper(r):
			if unicode.IsLower(prev) || unicode.IsDigit(prev) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		case unicode.IsLower(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			r = '_'
			if prev != '_' {
				b.WriteByte('_')
			}
		}
		prev = r
	}
	return strings.Trim(b.String(), "_")
}

// labelName makes a field key a valid, non-reserved label name.
func labelName(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.TrimLeft(b.String(), "_")
	switch {
	case name == "", name[0] >= '0' && name[0] <= '9',
		name == labelVM, name == labelObject, name == labelValue:
		return "field_" + name
	}
	return name
}

// labelString renders a field value as a label value.
func labelString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(raw)
	}
}
