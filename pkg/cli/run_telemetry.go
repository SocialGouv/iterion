package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/apikit/telemetry/otlpgrpc"

	"github.com/SocialGouv/iterion/pkg/benchmark"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// runTelemetry bundles the optional Prometheus + OTLP exporters started
// at run boot. Either field may be nil when the corresponding env var
// is unset. shutdown stops both, with bounded timeouts, and is safe to
// defer even when nothing was started.
type runTelemetry struct {
	prometheus       *benchmark.PrometheusExporter
	prometheusServer *http.Server
	otlp             *benchmark.OTLPGRPCExporter
}

// shutdown gracefully stops every exporter that booted, using small
// fixed timeouts because we never want telemetry teardown to extend
// the run's exit by much. Each call is best-effort: a stuck server
// won't block the process beyond the timeout.
func (t *runTelemetry) shutdown() {
	if t == nil {
		return
	}
	if t.prometheusServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = t.prometheusServer.Shutdown(ctx)
		cancel()
	}
	if t.otlp != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = t.otlp.Stop(ctx)
		cancel()
	}
}

// startRunTelemetry reads ITERION_PROMETHEUS_ADDR and the OTLP env
// suite, starts whichever exporters are configured, and returns a
// handle for shutdown. Returns an error only when configuration is
// present but invalid — silent absence stays a no-op.
func startRunTelemetry(runID string, logger *iterlog.Logger) (*runTelemetry, error) {
	t := &runTelemetry{}
	exporter, srv, err := startPrometheusFromEnv(runID, logger)
	if err != nil {
		return nil, err
	}
	t.prometheus = exporter
	t.prometheusServer = srv

	otlpExp, err := startOTLPGRPCFromEnv(runID, logger)
	if err != nil {
		// Reverse the half-built start so a Prometheus exporter doesn't
		// leak a goroutine when OTLP setup fails further down.
		t.shutdown()
		return nil, err
	}
	t.otlp = otlpExp
	return t, nil
}

// startPrometheusFromEnv builds a PrometheusExporter and serves /metrics
// on the address from the ITERION_PROMETHEUS_ADDR env var (e.g. ":9464").
// Returns (nil, nil, nil) when the env var is empty.
//
// The HTTP server runs in a goroutine; the caller should Shutdown it on
// exit. By default ListenAndServe failures (port in use, permission) are
// logged at error level and the exporter is returned anyway so the rest
// of the run can proceed (fail-soft).
//
// When ITERION_PROMETHEUS_REQUIRED is truthy, the address is bound
// synchronously upfront so a startup failure (port in use, missing
// permission, malformed addr) is surfaced as an error instead of being
// hidden in the background goroutine.
func startPrometheusFromEnv(runID string, logger *iterlog.Logger) (*benchmark.PrometheusExporter, *http.Server, error) {
	addr := strings.TrimSpace(os.Getenv("ITERION_PROMETHEUS_ADDR"))
	if addr == "" {
		return nil, nil, nil
	}
	required := isTruthyEnv("ITERION_PROMETHEUS_REQUIRED")
	exporter := benchmark.NewPrometheusExporter(runID, nil)
	mux := http.NewServeMux()
	mux.Handle("/metrics", exporter.Handler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if required {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, nil, fmt.Errorf("prometheus: bind %s (ITERION_PROMETHEUS_REQUIRED=1): %w", addr, err)
		}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("prometheus: serve %s: %v", addr, err)
			}
		}()
	} else {
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("prometheus: serve %s: %v", addr, err)
			}
		}()
	}
	logger.Info("prometheus: serving /metrics on %s (run_id=%s)", addr, runID)
	return exporter, srv, nil
}

// startOTLPGRPCFromEnv builds an OTLP/gRPC exporter from environment
// configuration and registers it as a secondary observer on the run's
// event bus. Returns (nil, nil) when no endpoint is configured.
//
// Recognised env vars (claw-code-go upstream):
//
//	CLAWD_OTLP_GRPC_ENDPOINT   host:port or URL (required to enable)
//	CLAWD_OTLP_GRPC_INSECURE   "1" / "true" disables TLS
//	CLAWD_OTLP_GRPC_HEADERS    comma-separated key=value pairs
//	CLAWD_SERVICE_NAME         service.name resource attr
//	CLAWD_SERVICE_VERSION      service.version resource attr
//
// ITERION_OTLP_GRPC_ENDPOINT is honored as an iterion-prefixed alias for
// the endpoint so operators can keep CLAWD_* reserved for claw-internal
// traffic if their deployment runs both side-by-side.
func startOTLPGRPCFromEnv(runID string, logger *iterlog.Logger) (*benchmark.OTLPGRPCExporter, error) {
	if alias := strings.TrimSpace(os.Getenv("ITERION_OTLP_GRPC_ENDPOINT")); alias != "" {
		if os.Getenv(otlpgrpc.EnvEndpoint) == "" {
			_ = os.Setenv(otlpgrpc.EnvEndpoint, alias)
		}
	}

	cfg, err := otlpgrpc.FromEnv()
	if err != nil {
		if errors.Is(err, otlpgrpc.ErrEndpointMissing) {
			return nil, nil
		}
		return nil, fmt.Errorf("otlp/grpc: %w", err)
	}
	if strings.TrimSpace(cfg.ServiceName) == "" {
		cfg.ServiceName = "iterion"
	}
	exp, err := benchmark.NewOTLPGRPCExporter(runID, cfg)
	if err != nil {
		return nil, fmt.Errorf("otlp/grpc: %w", err)
	}
	logger.Info("otlp/grpc: exporting to %s (run_id=%s, service=%s)", cfg.Endpoint, runID, cfg.ServiceName)
	return exp, nil
}

// isTruthyEnv returns true for the conventional "yes" values: 1, true, yes, on.
func isTruthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
