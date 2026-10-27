package listener

import (
	"net/http"

	"github.com/privadosIA/self-healing/internal/core/model"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Outcomes recorded by healerd_webhooks_total.
const (
	resultQueued    = "queued"
	resultDuplicate = "duplicate"
	resultIgnored   = "ignored"
	resultMalformed = "malformed"
	resultError     = "error"
)

// Outcomes recorded by healerd_reports_total.
const (
	reportStored       = "stored"
	reportUnauthorized = "unauthorized"
	reportInvalid      = "invalid"
	reportTooLarge     = "too_large"
	reportError        = "error"
)

// Metrics owns a private Prometheus registry (no global state) with the Go
// runtime and process collectors plus the daemon's own counters.
type Metrics struct {
	registry *prometheus.Registry
	webhooks *prometheus.CounterVec
	reports  *prometheus.CounterVec
	jobs     *prometheus.CounterVec
}

// NewMetrics builds the registry. The counters for every outcome are
// initialized to zero so they appear on /metrics before the first event.
func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "healerd_webhooks_total",
			Help: "Authenticated webhooks processed, by outcome.",
		}, []string{"result"}),
		reports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "healerd_reports_total",
			Help: "Scanner report uploads, by outcome.",
		}, []string{"result"}),
	}
	m.jobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "healerd_jobs_total",
		Help: "Fix jobs finished, by outcome.",
	}, []string{"status"})
	m.registry.MustRegister(
		m.jobs,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.webhooks,
		m.reports,
	)
	for _, r := range []string{resultQueued, resultDuplicate, resultIgnored, resultMalformed, resultError} {
		m.webhooks.WithLabelValues(r)
	}
	for _, st := range []model.JobStatus{model.JobPROpened, model.JobSkipped, model.JobFailed} {
		m.jobs.WithLabelValues(string(st))
	}
	for _, r := range []string{reportStored, reportUnauthorized, reportInvalid, reportTooLarge, reportError} {
		m.reports.WithLabelValues(r)
	}
	return m
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// webhook counts one webhook outcome. It is a no-op on a nil Metrics so
// handlers can run without instrumentation in tests.
func (m *Metrics) webhook(result string) {
	if m != nil {
		m.webhooks.WithLabelValues(result).Inc()
	}
}

// JobFinished counts a finished job by its outcome; a no-op on a nil Metrics.
func (m *Metrics) JobFinished(status model.JobStatus) {
	if m != nil {
		m.jobs.WithLabelValues(string(status)).Inc()
	}
}

// report counts one report upload outcome; a no-op on a nil Metrics.
func (m *Metrics) report(result string) {
	if m != nil {
		m.reports.WithLabelValues(result).Inc()
	}
}

// HealthHandler answers liveness probes.
func HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
}
