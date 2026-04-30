// Package benchmark — OTLP/gRPC integration.
//
// OTLPGRPCExporter ships iterion run events to an OpenTelemetry collector
// over gRPC. It coexists with the Prometheus exporter (different
// observability planes — Prometheus for metric scraping, OTLP for
// per-event traces / log aggregation), so wiring both is supported.
//
// The exporter is a thin adapter over claw-code-go's
// pkg/apikit/telemetry/otlpgrpc Exporter — batching, retries on
// transient gRPC errors, and shutdown drain are inherited from the
// official OpenTelemetry SDK upstream.
package benchmark

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/claw-code-go/pkg/apikit"
	"github.com/SocialGouv/claw-code-go/pkg/apikit/telemetry/otlpgrpc"

	"github.com/SocialGouv/iterion/pkg/store"
)

// OTLPGRPCExporter wraps an upstream OTLP/gRPC exporter and translates
// iterion store events into apikit.TelemetryEvent analytics records.
// Every event in events.jsonl produces a corresponding analytics event
// on the OTLP wire so collectors can index per-event flow without
// needing to tail the JSONL files directly.
type OTLPGRPCExporter struct {
	runID string
	exp   *otlpgrpc.Exporter
}

// NewOTLPGRPCExporter constructs an exporter from the given config. It
// returns the typed otlpgrpc.ErrEndpointMissing when the endpoint is
// blank so callers can distinguish "user opted out" from "config
// invalid" using errors.Is.
func NewOTLPGRPCExporter(runID string, cfg otlpgrpc.Config) (*OTLPGRPCExporter, error) {
	if cfg.Endpoint == "" {
		return nil, otlpgrpc.ErrEndpointMissing
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "iterion"
	}
	exp, err := otlpgrpc.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("otlp: build exporter: %w", err)
	}
	return &OTLPGRPCExporter{runID: runID, exp: exp}, nil
}

// EventObserver returns a callback compatible with
// runtime.WithEventObserver. Every store event is translated into an
// apikit.TelemetryEvent of analytics shape — namespace "iterion",
// action set to the event type — with run_id, node_id, branch_id, and
// the event's Data map flattened into Properties. Marshalling errors
// in the Data map are silently dropped: telemetry must never abort a
// run.
func (e *OTLPGRPCExporter) EventObserver() func(store.Event) {
	return func(evt store.Event) {
		defer func() { _ = recover() }() // never let a sink panic kill a run

		props := make(map[string]any, len(evt.Data)+3)
		props["run_id"] = evt.RunID
		if evt.NodeID != "" {
			props["node_id"] = evt.NodeID
		}
		if evt.BranchID != "" {
			props["branch_id"] = evt.BranchID
		}
		props["seq"] = evt.Seq
		for k, v := range evt.Data {
			// Drop binary or non-JSON-friendly values via the apikit
			// flattener at marshal time; here we just forward the map.
			props[k] = v
		}

		analytics := apikit.NewAnalyticsEvent("iterion", string(evt.Type))
		analytics.Properties = props

		e.exp.Record(apikit.TelemetryEvent{
			Type:      apikit.EventTypeAnalytics,
			SessionID: e.runID,
			Analytics: &analytics,
		})
	}
}

// Stop drains the underlying batch processor and closes the gRPC
// client. Safe to call multiple times. A nil receiver is a no-op so
// callers can defer Stop unconditionally.
func (e *OTLPGRPCExporter) Stop(ctx context.Context) error {
	if e == nil || e.exp == nil {
		return nil
	}
	return e.exp.Stop(ctx)
}

// IsEndpointMissing reports whether err signals the operator did not
// configure an OTLP endpoint. Convenience wrapper over errors.Is so
// CLI wiring stays a single line.
func IsEndpointMissing(err error) bool {
	return errors.Is(err, otlpgrpc.ErrEndpointMissing)
}
