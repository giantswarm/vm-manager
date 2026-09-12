package metrics

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const guestPrefix = "vm_guest_"

// DefaultMaxGuestSeries is the per-VM series cap when Options leaves it 0.
const DefaultMaxGuestSeries = 1000

// SampleSize is how many report entries GuestMetrics.Sample carries.
const SampleSize = 10

// Reasons of vm_guest_report_series_dropped_total.
const (
	dropLimit       = "limit"
	dropUnsupported = "unsupported"
	dropDuplicate   = "duplicate"
)

var dropReasons = []string{dropLimit, dropUnsupported, dropDuplicate}

// guestReport is the parsed state of one VM's last upload. It is immutable
// once stored; the cumulative counters are carried over from the previous
// report of the same VM.
type guestReport struct {
	receivedAt  time.Time
	generatedAt time.Time
	families    int
	entries     int
	series      []series
	dropped     map[string]int
	sample      []Entry

	uploads      int
	droppedTotal map[string]int
}

// guestCollector exposes the last report of every VM as constant metrics.
// It is an unchecked collector: the families are whatever the guests sent.
type guestCollector struct {
	max int
	now func() time.Time
	log *slog.Logger

	ageDesc, seriesDesc, droppedDesc, uploadsDesc *prometheus.Desc

	mu  sync.Mutex
	vms map[string]*guestReport
}

func newGuestCollector(max int, now func() time.Time, log *slog.Logger) *guestCollector {
	vmLabel := []string{labelVM}
	return &guestCollector{
		max: max,
		now: now,
		log: log,
		ageDesc: prometheus.NewDesc(guestPrefix+"report_age_seconds",
			"Seconds since the VM's last systemd-report upload was received.", vmLabel, nil),
		seriesDesc: prometheus.NewDesc(guestPrefix+"report_series",
			"Series exported from the VM's last systemd-report upload.", vmLabel, nil),
		droppedDesc: prometheus.NewDesc(guestPrefix+"report_series_dropped_total",
			"Report entries not exported, by reason: limit (per-VM series cap), unsupported (value type), duplicate.",
			[]string{labelVM, "reason"}, nil),
		uploadsDesc: prometheus.NewDesc(guestPrefix+"reports_total",
			"systemd-report uploads received from the VM.", vmLabel, nil),
		vms: make(map[string]*guestReport),
	}
}

// store parses raw and replaces the VM's report.
func (g *guestCollector) store(vmID string, raw json.RawMessage) error {
	r, err := ParseReport(raw)
	if err != nil {
		return err
	}
	rep := &guestReport{
		receivedAt:   g.now(),
		families:     r.Families(),
		entries:      len(r.Metrics),
		dropped:      make(map[string]int),
		droppedTotal: make(map[string]int),
	}
	rep.generatedAt, _ = r.GeneratedAt()
	if n := min(len(r.Metrics), SampleSize); n > 0 {
		rep.sample = append([]Entry(nil), r.Metrics[:n]...)
	}
	seen := make(map[string]struct{}, len(r.Metrics))
	for _, e := range r.Metrics {
		s, ok := convert(e)
		switch {
		case !ok:
			rep.dropped[dropUnsupported]++
			continue
		case len(rep.series) >= g.max:
			rep.dropped[dropLimit]++
			continue
		}
		if _, dup := seen[s.key()]; dup {
			rep.dropped[dropDuplicate]++
			continue
		}
		seen[s.key()] = struct{}{}
		rep.series = append(rep.series, s)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if prev := g.vms[vmID]; prev != nil {
		rep.uploads = prev.uploads
		for k, v := range prev.droppedTotal {
			rep.droppedTotal[k] = v
		}
	}
	rep.uploads++
	for k, v := range rep.dropped {
		rep.droppedTotal[k] += v
	}
	g.vms[vmID] = rep
	if n := rep.dropped[dropLimit]; n > 0 {
		g.log.Warn("guest report exceeds the series limit", "vm", vmID, "limit", g.max, "dropped", n)
	}
	return nil
}

func (g *guestCollector) forget(vmID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.vms, vmID)
}

// report returns the VM's last report, nil when none.
func (g *guestCollector) report(vmID string) *guestReport {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.vms[vmID]
}

// snapshot copies the map so Collect works without the lock.
func (g *guestCollector) snapshot() (ids []string, reports map[string]*guestReport) {
	g.mu.Lock()
	defer g.mu.Unlock()
	reports = make(map[string]*guestReport, len(g.vms))
	for id, r := range g.vms {
		reports[id] = r
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, reports
}

// Describe sends nothing: the collector is unchecked because the guest
// families are only known at collect time.
func (g *guestCollector) Describe(chan<- *prometheus.Desc) {}

// family groups the series of one metric name across VMs; the label set is
// the union of their labels so the exposition stays consistent when guests
// report different fields.
type family struct {
	name   string
	help   string
	kind   kind
	labels map[string]struct{}
	items  []familyItem
}

type familyItem struct {
	vm string
	s  series
}

func (g *guestCollector) Collect(ch chan<- prometheus.Metric) {
	ids, reports := g.snapshot()
	now := g.now()
	families := make(map[string]*family)
	for _, id := range ids {
		rep := reports[id]
		ch <- prometheus.MustNewConstMetric(g.ageDesc, prometheus.GaugeValue, now.Sub(rep.receivedAt).Seconds(), id)
		ch <- prometheus.MustNewConstMetric(g.seriesDesc, prometheus.GaugeValue, float64(len(rep.series)), id)
		ch <- prometheus.MustNewConstMetric(g.uploadsDesc, prometheus.CounterValue, float64(rep.uploads), id)
		for _, reason := range dropReasons {
			ch <- prometheus.MustNewConstMetric(g.droppedDesc, prometheus.CounterValue, float64(rep.droppedTotal[reason]), id, reason)
		}
		for _, s := range rep.series {
			f := families[s.name]
			if f == nil {
				f = &family{name: s.name, help: "Guest systemd-report family " + s.family + ".", kind: s.kind, labels: make(map[string]struct{})}
				families[s.name] = f
			}
			for k := range s.labels {
				f.labels[k] = struct{}{}
			}
			f.items = append(f.items, familyItem{vm: id, s: s})
		}
	}

	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := families[name]
		keys := make([]string, 0, len(f.labels))
		for k := range f.labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		desc := prometheus.NewDesc(name, f.help, append([]string{labelVM}, keys...), nil)
		valueType := prometheus.GaugeValue
		if f.kind == kindCounter {
			valueType = prometheus.CounterValue
		}
		values := make([]string, len(keys)+1)
		for _, it := range f.items {
			values[0] = it.vm
			for i, k := range keys {
				values[i+1] = it.s.labels[k]
			}
			m, err := prometheus.NewConstMetric(desc, valueType, it.s.value, values...)
			if err != nil {
				g.log.Warn("guest series not exported", "vm", it.vm, "family", it.s.family, "err", err)
				continue
			}
			ch <- m
		}
	}
}

// StoreReport implements the report sink the IMDS /report handler feeds
// (through the VM service, which persists the upload first).
func (r *Registry) StoreReport(_ context.Context, vmID string, report json.RawMessage) error {
	return r.guest.store(vmID, report)
}

// ForgetVM drops the guest series and counters of a deleted VM.
func (r *Registry) ForgetVM(vmID string) { r.guest.forget(vmID) }
