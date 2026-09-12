package metrics

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Options configure the registry; the zero value works.
type Options struct {
	// Version and Commit label vm_manager_build_info.
	Version string
	Commit  string
	// States are the VM lifecycle states vm_manager_vm_state and
	// vm_manager_vms enumerate (a 0 for every absent state).
	States []string
	// MaxGuestSeries caps the series kept per VM from one report;
	// 0 is DefaultMaxGuestSeries.
	MaxGuestSeries int
	// ProcRoot is where procfs is mounted; tests point it at a fixture tree.
	ProcRoot string
	// ClockTicks is CLK_TCK for /proc stat times, PageSize the page size for
	// statm; 0 picks the Linux defaults.
	ClockTicks int
	PageSize   int
	// Now is the clock of report ages; nil uses time.Now.
	Now func() time.Time
	// Logger for dropped series; nil uses slog.Default().
	Logger *slog.Logger
}

func (o *Options) defaults() {
	if o.MaxGuestSeries <= 0 {
		o.MaxGuestSeries = DefaultMaxGuestSeries
	}
	if o.ProcRoot == "" {
		o.ProcRoot = DefaultProcRoot
	}
	if o.ClockTicks <= 0 {
		o.ClockTicks = DefaultClockTicks
	}
	if o.PageSize <= 0 {
		o.PageSize = os.Getpagesize()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Registry owns the Prometheus registry: build info, the Go and process
// collectors of vm-manager itself, the host collector and the guest
// reports. It is the VM service's metrics sink and the source of
// get_vm_metrics.
type Registry struct {
	reg   *prometheus.Registry
	host  *hostCollector
	guest *guestCollector
	log   *slog.Logger

	installHist     prometheus.Histogram
	bootToReadyHist prometheus.Histogram
}

// New builds a registry. Call SetSource once the VM service exists.
func New(opts Options) *Registry {
	opts.defaults()
	proc := procReader{root: opts.ProcRoot, ticks: float64(opts.ClockTicks), pageSize: int64(opts.PageSize)}
	r := &Registry{
		reg:   prometheus.NewRegistry(),
		host:  newHostCollector(opts.States, proc, opts.Logger),
		guest: newGuestCollector(opts.MaxGuestSeries, opts.Now, opts.Logger),
		log:   opts.Logger,
		installHist: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    hostPrefix + "vm_install_duration_seconds",
			Help:    "Installer phase durations, observed when a VM finishes installing.",
			Buckets: []float64{10, 20, 30, 45, 60, 90, 120, 180, 300},
		}),
		bootToReadyHist: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    hostPrefix + "vm_boot_to_ready_duration_seconds",
			Help:    "Installed boot to READY=1 durations, observed when a VM becomes ready.",
			Buckets: []float64{1, 2, 5, 10, 15, 20, 30, 60, 120},
		}),
	}
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: hostPrefix + "build_info",
		Help: "Constant 1 with the vm-manager version, commit and Go version.",
	}, []string{"version", "commit", "go_version"})
	buildInfo.WithLabelValues(opts.Version, opts.Commit, runtime.Version()).Set(1)
	r.reg.MustRegister(buildInfo, r.installHist, r.bootToReadyHist, r.host, r.guest,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return r
}

// SetSource attaches the VM service the host collector scrapes.
func (r *Registry) SetSource(src Source) { r.host.setSource(src) }

// Gatherer exposes the registry for tests.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.reg }

// Handler serves the exposition. A collector error does not fail the
// scrape: the remaining families are served and the error logged.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
		ErrorLog:      promLogger{r.log},
	})
}

// ObserveInstall records a finished installer phase.
func (r *Registry) ObserveInstall(d time.Duration) { r.installHist.Observe(d.Seconds()) }

// ObserveBootToReady records an installed boot that reached READY=1.
func (r *Registry) ObserveBootToReady(d time.Duration) { r.bootToReadyHist.Observe(d.Seconds()) }

// promLogger adapts slog to promhttp's error log.
type promLogger struct{ log *slog.Logger }

func (l promLogger) Println(v ...any) { l.log.Warn("metrics exposition", "err", fmt.Sprintln(v...)) }
