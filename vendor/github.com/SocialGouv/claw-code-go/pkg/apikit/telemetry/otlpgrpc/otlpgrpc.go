// Package otlpgrpc is the public façade over the OTLP/gRPC log exporter
// implemented in internal/apikit/telemetry/otlpgrpc. External consumers
// (iterion and any other host shipping claw-code-go as a library) import
// this package to push telemetry into an OTLP collector without taking a
// dependency on internal/.
//
// The internal exporter satisfies apikit.TelemetrySink and is exposed
// here verbatim via type aliases — methods, struct layout, and config
// shape are identical to the internal package, so this façade is a thin
// re-export, not a wrapper. Internal callers continue importing the
// internal path unchanged.
package otlpgrpc

import (
	intl "github.com/SocialGouv/claw-code-go/internal/apikit/telemetry/otlpgrpc"
)

// EnvEndpoint is the environment variable consulted by FromEnv to
// discover the collector endpoint (CLAWD_OTLP_GRPC_ENDPOINT).
const EnvEndpoint = intl.EnvEndpoint

// Default batching and timeout knobs. Re-exported as constants so
// external callers can reference them in their own configs.
const (
	DefaultBatchSize     = intl.DefaultBatchSize
	DefaultFlushInterval = intl.DefaultFlushInterval
	DefaultExportTimeout = intl.DefaultExportTimeout
)

// Config configures the gRPC exporter.
type Config = intl.Config

// Exporter satisfies apikit.TelemetrySink and ships events to an
// OTLP/gRPC collector. Stop must be called for a clean shutdown — it
// drains the batch processor and closes the underlying gRPC client.
type Exporter = intl.Exporter

// New constructs a gRPC exporter and the underlying OTLP client.
var New = intl.New

// FromEnv builds a Config from environment variables. Returns
// ErrEndpointMissing when no endpoint is configured so callers can
// distinguish "user opted out" from "config invalid".
var FromEnv = intl.FromEnv

// ErrEndpointMissing is returned by FromEnv when CLAWD_OTLP_GRPC_ENDPOINT
// is unset or blank. Use errors.Is to detect.
var ErrEndpointMissing = intl.ErrEndpointMissing
