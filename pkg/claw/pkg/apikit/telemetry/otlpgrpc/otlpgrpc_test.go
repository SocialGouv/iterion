package otlpgrpc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/apikit"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/apikit/telemetry/otlpgrpc"
)

// TestFromEnv_MissingEndpointTypedError verifies the public façade
// exposes the typed sentinel; hosts use errors.Is to gate "OTLP
// disabled" vs "config invalid".
func TestFromEnv_MissingEndpointTypedError(t *testing.T) {
	t.Setenv(otlpgrpc.EnvEndpoint, "")
	_, err := otlpgrpc.FromEnv()
	if !errors.Is(err, otlpgrpc.ErrEndpointMissing) {
		t.Fatalf("expected ErrEndpointMissing, got %T: %v", err, err)
	}
}

// TestNewExporterAndStop verifies the basic lifecycle is callable
// through the façade without touching internal/. The endpoint points
// at a localhost port that nothing is listening on; New must succeed
// (it doesn't dial in this path) and Stop must return without
// blocking longer than the export timeout.
func TestNewExporterAndStop(t *testing.T) {
	cfg := otlpgrpc.Config{
		Endpoint:      "127.0.0.1:1",
		Insecure:      true,
		ExportTimeout: 100 * time.Millisecond,
	}
	exp, err := otlpgrpc.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Record one event — must not panic, must not block.
	exp.Record(apikit.TelemetryEvent{Type: apikit.EventTypeAnalytics})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = exp.Stop(ctx)
}
