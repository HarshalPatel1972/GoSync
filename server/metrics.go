package server

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are Prometheus instruments for a Server. A nil *Metrics records
// nothing.
type Metrics struct {
	sessions          prometheus.Gauge
	sessionsTotal     prometheus.Counter
	authFailures      prometheus.Counter
	messages          *prometheus.CounterVec
	errors            *prometheus.CounterVec
	mutationsApplied  prometheus.Counter
	mutationsRejected prometheus.Counter
	pokes             prometheus.Counter
	duration          *prometheus.HistogramVec
	compacted         prometheus.Counter
}

// NewMetrics creates the instruments and registers them with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		sessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gosync_sessions", Help: "Authenticated sync sessions currently open.",
		}),
		sessionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gosync_sessions_total", Help: "Sync sessions authenticated since start.",
		}),
		authFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gosync_auth_failures_total", Help: "Rejected hello frames (bad token, version or client id).",
		}),
		messages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gosync_messages_total", Help: "Frames received from clients, by type.",
		}, []string{"type"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gosync_errors_total", Help: "Error frames sent to clients, by code.",
		}, []string{"code"}),
		mutationsApplied: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gosync_mutations_received_total", Help: "Valid mutations received (including idempotent replays).",
		}),
		mutationsRejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gosync_mutations_rejected_total", Help: "Mutations rejected by validation or clock-skew checks.",
		}),
		pokes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gosync_change_notifications_total", Help: "Namespace change notifications published.",
		}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gosync_request_duration_seconds",
			Help:    "Time to handle push and pull requests, including the database.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"op"}),
		compacted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gosync_tombstones_compacted_total", Help: "Deleted documents purged by compaction.",
		}),
	}
	reg.MustRegister(m.sessions, m.sessionsTotal, m.authFailures, m.messages, m.errors,
		m.mutationsApplied, m.mutationsRejected, m.pokes, m.duration, m.compacted)
	return m
}

func (m *Metrics) sessionStarted() {
	if m != nil {
		m.sessions.Inc()
		m.sessionsTotal.Inc()
	}
}

func (m *Metrics) sessionEnded() {
	if m != nil {
		m.sessions.Dec()
	}
}

func (m *Metrics) authFailed() {
	if m != nil {
		m.authFailures.Inc()
	}
}

func (m *Metrics) message(typ string) {
	if m != nil {
		switch typ {
		case "push", "pull":
		default:
			typ = "other" // bound label cardinality
		}
		m.messages.WithLabelValues(typ).Inc()
	}
}

func (m *Metrics) errorSent(code string) {
	if m != nil {
		m.errors.WithLabelValues(code).Inc()
	}
}

func (m *Metrics) mutations(valid, rejected int) {
	if m != nil {
		m.mutationsApplied.Add(float64(valid))
		m.mutationsRejected.Add(float64(rejected))
	}
}

func (m *Metrics) poked() {
	if m != nil {
		m.pokes.Inc()
	}
}

func (m *Metrics) observe(op string, start time.Time) {
	if m != nil {
		m.duration.WithLabelValues(op).Observe(time.Since(start).Seconds())
	}
}

func (m *Metrics) compactedDocs(n int) {
	if m != nil {
		m.compacted.Add(float64(n))
	}
}

// AuthFailures exposes the auth failure counter (for tests and dashboards).
func (m *Metrics) AuthFailures() prometheus.Counter { return m.authFailures }
