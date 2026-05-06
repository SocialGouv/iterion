// Package metrics centralises the Prometheus metrics exposed by the
// cloud-mode iterion server and runner pods. A single registry is
// shared across both binaries so the Helm chart's PodMonitor scrapes
// both endpoints with the same metric names — operators tune
// alerts on iterion_* without caring whether the value comes from
// a server pod or a runner pod.
//
// Plan §F (T-40).
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Registry is the shared metric registry. Tests reset it between
// runs via NewForTesting; production code calls Default() once at
// boot and reuses the same registry across the server + runner
// stacks of metrics defined here.
type Registry struct {
	reg *prometheus.Registry

	// --- Server-side metrics (plan §F T-40) ----------------------
	RunsCreatedTotal       *prometheus.CounterVec   // by status
	RunsActive             *prometheus.GaugeVec     // by status
	RunDurationSeconds     *prometheus.HistogramVec // by status
	WSConnections          prometheus.Gauge
	MongoChangeStreamLagS  prometheus.Gauge

	// --- Runner-side metrics -------------------------------------
	NATSPendingMessages    prometheus.Gauge
	WorkspaceCloneDuration prometheus.Histogram
	LLMTokensTotal         *prometheus.CounterVec // backend, model, direction
	LLMCostUSDTotal        *prometheus.CounterVec // backend, model
	RunnerHeartbeatErrors  prometheus.Counter
}

// New registers the metrics on a fresh registry. Each call gives a
// fully-isolated registry — convenient for tests, mandatory for
// production where Default() returns the singleton.
func New() *Registry {
	reg := prometheus.NewRegistry()
	r := &Registry{reg: reg}

	r.RunsCreatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_runs_created_total",
		Help: "Total number of runs accepted, broken down by terminal status.",
	}, []string{"status"})
	r.RunsActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "iterion_runs_active",
		Help: "Current count of in-flight runs by status (running, queued, paused).",
	}, []string{"status"})
	r.RunDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "iterion_run_duration_seconds",
		Help:    "Wall-clock duration of completed runs, excluding queued + paused intervals.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 14), // 1s … ~16k s
	}, []string{"status"})
	r.WSConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iterion_ws_connections",
		Help: "Number of currently connected run-console WebSocket clients.",
	})
	r.MongoChangeStreamLagS = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iterion_mongo_change_stream_lag_seconds",
		Help: "Seconds between event creation and change-stream delivery on the runview subscription.",
	})

	r.NATSPendingMessages = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iterion_nats_pending_messages",
		Help: "Pending JetStream messages on the iterion.queue.runs durable consumer.",
	})
	r.WorkspaceCloneDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "iterion_workspace_clone_duration_seconds",
		Help:    "Time spent cloning the workspace repository before engine start.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 12), // 100ms … ~400s
	})
	r.LLMTokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_llm_tokens_total",
		Help: "LLM token usage by backend, model and direction (input/output/cache_read/cache_write).",
	}, []string{"backend", "model", "direction"})
	r.LLMCostUSDTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iterion_llm_cost_usd_total",
		Help: "Cumulative LLM cost in USD by backend and model.",
	}, []string{"backend", "model"})
	r.RunnerHeartbeatErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "iterion_runner_heartbeat_errors_total",
		Help: "Number of NATS KV lease refresh failures encountered while a run was in flight.",
	})

	reg.MustRegister(
		r.RunsCreatedTotal, r.RunsActive, r.RunDurationSeconds,
		r.WSConnections, r.MongoChangeStreamLagS,
		r.NATSPendingMessages, r.WorkspaceCloneDuration,
		r.LLMTokensTotal, r.LLMCostUSDTotal, r.RunnerHeartbeatErrors,
	)
	return r
}

// Handler returns an http.Handler suitable for mounting at /metrics.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{Registry: r.reg})
}

// StartServer binds a dedicated HTTP listener for /metrics. Returns
// the *http.Server so the caller can Shutdown it cleanly. addr is
// host:port (e.g. ":9090"); empty addr disables the listener and
// returns nil, nil.
//
// On listener-bind failure StartServer returns the error
// synchronously so an operator who configured ITERION_METRICS_ADDR
// observes the gap at boot, not in a silent goroutine.
func (r *Registry) StartServer(addr string, logger *iterlog.Logger) (*http.Server, error) {
	if addr == "" {
		return nil, nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", r.Handler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics: bind %s: %w", addr, err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if logger != nil {
				logger.Error("metrics: serve %s: %v", addr, err)
			}
		}
	}()
	if logger != nil {
		logger.Info("metrics: serving /metrics on %s", addr)
	}
	return srv, nil
}

// ShutdownTimeout is the bounded wait the StartServer-spawned listener
// gets before it is killed via context. Callers can wrap with their
// own context if they need a different budget.
const ShutdownTimeout = 5 * time.Second

// ShutdownServer is a convenience wrapper around srv.Shutdown that
// applies the package-level timeout.
func ShutdownServer(srv *http.Server) error {
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(ctx)
}
