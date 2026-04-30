package benchmark

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/apikit/telemetry/otlpgrpc"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestNewOTLPGRPCExporter_RejectsEmptyEndpoint(t *testing.T) {
	_, err := NewOTLPGRPCExporter("run_test", otlpgrpc.Config{})
	if !errors.Is(err, otlpgrpc.ErrEndpointMissing) {
		t.Fatalf("expected ErrEndpointMissing, got %T: %v", err, err)
	}
	if !IsEndpointMissing(err) {
		t.Errorf("IsEndpointMissing convenience helper returned false")
	}
}

// TestOTLPGRPCExporter_ObserverFiresEvents constructs an exporter with
// a closed-loopback endpoint (the SDK will queue records and let the
// gRPC retry layer fail asynchronously) and pushes a representative
// event through the observer. The contract is non-blocking + non-panic
// — Record must enqueue synchronously and return regardless of the
// downstream connection state, otherwise the runtime event bus stalls.
func TestOTLPGRPCExporter_ObserverFiresEvents(t *testing.T) {
	exp, err := NewOTLPGRPCExporter("run_test", otlpgrpc.Config{
		Endpoint:      "127.0.0.1:1",
		Insecure:      true,
		ExportTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewOTLPGRPCExporter: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = exp.Stop(ctx)
	}()

	observe := exp.EventObserver()
	events := []store.Event{
		{Type: store.EventRunStarted, RunID: "run_test", Seq: 1},
		{Type: store.EventNodeStarted, RunID: "run_test", NodeID: "ingest", Seq: 2, Data: map[string]interface{}{"backend": "claw"}},
		{Type: store.EventLLMRequest, RunID: "run_test", NodeID: "ingest", Seq: 3, Data: map[string]interface{}{"model": "anthropic/claude-haiku-4-5", "tool_count": 3}},
		{Type: store.EventToolCalled, RunID: "run_test", NodeID: "ingest", Seq: 4, Data: map[string]interface{}{"tool": "bash", "duration_ms": 42}},
		{Type: store.EventLLMCompacted, RunID: "run_test", NodeID: "ingest", Seq: 5, Data: map[string]interface{}{"before_messages": 12, "after_messages": 5}},
		{Type: store.EventRunFinished, RunID: "run_test", Seq: 6},
	}
	done := make(chan struct{})
	go func() {
		for _, evt := range events {
			observe(evt)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observer blocked — Record must be non-blocking")
	}
}

// TestOTLPGRPCExporter_StopOnNilReceiver lets cli wiring defer Stop
// unconditionally without nil-checking. A panic here would break the
// CLI defer chain on every run that didn't enable OTLP.
func TestOTLPGRPCExporter_StopOnNilReceiver(t *testing.T) {
	var exp *OTLPGRPCExporter
	if err := exp.Stop(context.Background()); err != nil {
		t.Errorf("Stop on nil receiver returned %v, want nil", err)
	}
}
