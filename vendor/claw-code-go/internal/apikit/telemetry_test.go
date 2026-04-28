package apikit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMemorySinkRecordsAndReturnsEvents(t *testing.T) {
	sink := &MemoryTelemetrySink{}
	sink.Record(TelemetryEvent{Type: EventTypeAnalytics, Analytics: &AnalyticsEvent{Namespace: "cli", Action: "start"}})
	sink.Record(TelemetryEvent{Type: EventTypeHTTPRequestStarted, SessionID: "s1", Attempt: 1, Method: "POST", Path: "/v1/messages"})

	events := sink.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != EventTypeAnalytics {
		t.Errorf("expected analytics event, got %s", events[0].Type)
	}
	if events[1].Type != EventTypeHTTPRequestStarted {
		t.Errorf("expected http_request_started, got %s", events[1].Type)
	}
}

func TestMemorySinkReturnsCopy(t *testing.T) {
	sink := &MemoryTelemetrySink{}
	sink.Record(TelemetryEvent{Type: EventTypeAnalytics})
	events := sink.Events()
	events[0].Type = "mutated"
	original := sink.Events()
	if original[0].Type != EventTypeAnalytics {
		t.Error("Events() should return a copy, not a reference")
	}
}

func TestJsonlSinkWritesValidJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry.jsonl")

	sink, err := NewJsonlTelemetrySink(path)
	if err != nil {
		t.Fatalf("failed to create sink: %v", err)
	}
	defer sink.Close()

	sink.Record(TelemetryEvent{
		Type:      EventTypeAnalytics,
		Analytics: &AnalyticsEvent{Namespace: "cli", Action: "turn_completed"},
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	content := string(data)
	if len(content) == 0 {
		t.Fatal("empty file")
	}

	var parsed TelemetryEvent
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("invalid JSON line: %v", err)
	}
	if parsed.Type != EventTypeAnalytics {
		t.Errorf("expected analytics type, got %s", parsed.Type)
	}
}

func TestTelemetryEventFlatJSONLayoutAnalytics(t *testing.T) {
	// Verify Analytics events serialize flat, matching Rust serde(tag="type")
	event := TelemetryEvent{
		Type: EventTypeAnalytics,
		Analytics: &AnalyticsEvent{
			Namespace:  "cli",
			Action:     "turn_completed",
			Properties: map[string]any{"ok": true},
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}

	// Parse into raw map to check layout
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	// Must have flat fields, NOT a nested "analytics" key
	if _, nested := raw["analytics"]; nested {
		t.Error("analytics event should be flat, not nested under 'analytics' key")
	}
	if raw["type"] != "analytics" {
		t.Errorf("expected type=analytics, got %v", raw["type"])
	}
	if raw["namespace"] != "cli" {
		t.Errorf("expected namespace=cli, got %v", raw["namespace"])
	}
	if raw["action"] != "turn_completed" {
		t.Errorf("expected action=turn_completed, got %v", raw["action"])
	}
	props, ok := raw["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map")
	}
	if props["ok"] != true {
		t.Errorf("expected properties.ok=true, got %v", props["ok"])
	}
}

func TestTelemetryEventFlatJSONLayoutSessionTrace(t *testing.T) {
	// Verify SessionTrace events serialize flat, matching Rust serde(tag="type")
	event := TelemetryEvent{
		Type: EventTypeSessionTrace,
		SessionTrace: &SessionTraceRecord{
			SessionID:   "sess-1",
			Sequence:    42,
			Name:        "http_request_started",
			TimestampMs: 1234567890,
			Attributes:  map[string]any{"method": "POST"},
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	// Must have flat fields, NOT a nested "session_trace" key
	if _, nested := raw["session_trace"]; nested {
		t.Error("session_trace event should be flat, not nested under 'session_trace' key")
	}
	if raw["type"] != "session_trace" {
		t.Errorf("expected type=session_trace, got %v", raw["type"])
	}
	if raw["session_id"] != "sess-1" {
		t.Errorf("expected session_id=sess-1, got %v", raw["session_id"])
	}
	// JSON numbers are float64
	if raw["sequence"] != float64(42) {
		t.Errorf("expected sequence=42, got %v", raw["sequence"])
	}
	if raw["name"] != "http_request_started" {
		t.Errorf("expected name=http_request_started, got %v", raw["name"])
	}
}

func TestTelemetryEventFlatJSONLayoutHTTP(t *testing.T) {
	// Verify HTTP events also serialize flat
	event := TelemetryEvent{
		Type:      EventTypeHTTPRequestStarted,
		SessionID: "sess-2",
		Attempt:   1,
		Method:    "POST",
		Path:      "/v1/messages",
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	if raw["type"] != "http_request_started" {
		t.Errorf("expected type=http_request_started, got %v", raw["type"])
	}
	if raw["session_id"] != "sess-2" {
		t.Errorf("expected session_id=sess-2, got %v", raw["session_id"])
	}
	if raw["method"] != "POST" {
		t.Errorf("expected method=POST, got %v", raw["method"])
	}
}

func TestTelemetryEventJSONRoundTrip(t *testing.T) {
	// Verify Marshal → Unmarshal round-trip for all event types
	events := []TelemetryEvent{
		{
			Type: EventTypeAnalytics,
			Analytics: &AnalyticsEvent{
				Namespace: "cli", Action: "start",
				Properties: map[string]any{"model": "claude"},
			},
		},
		{
			Type: EventTypeSessionTrace,
			SessionTrace: &SessionTraceRecord{
				SessionID: "s1", Sequence: 7, Name: "trace",
				TimestampMs: 9999, Attributes: map[string]any{"k": "v"},
			},
		},
		{
			Type:      EventTypeHTTPRequestSucceeded,
			SessionID: "s2", Attempt: 3, Method: "POST",
			Path: "/v1/messages", Status: 200, RequestID: "req-1",
		},
	}

	for _, original := range events {
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("marshal %s: %v", original.Type, err)
		}
		var decoded TelemetryEvent
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal %s: %v", original.Type, err)
		}
		if decoded.Type != original.Type {
			t.Errorf("type mismatch: %s != %s", decoded.Type, original.Type)
		}
		switch original.Type {
		case EventTypeAnalytics:
			if decoded.Analytics == nil {
				t.Fatal("analytics should be reconstructed")
			}
			if decoded.Analytics.Namespace != original.Analytics.Namespace {
				t.Error("analytics namespace mismatch")
			}
			if decoded.Analytics.Action != original.Analytics.Action {
				t.Error("analytics action mismatch")
			}
		case EventTypeSessionTrace:
			if decoded.SessionTrace == nil {
				t.Fatal("session trace should be reconstructed")
			}
			if decoded.SessionTrace.SessionID != original.SessionTrace.SessionID {
				t.Error("session trace session_id mismatch")
			}
			if decoded.SessionTrace.Sequence != original.SessionTrace.Sequence {
				t.Error("session trace sequence mismatch")
			}
		case EventTypeHTTPRequestSucceeded:
			if decoded.Status != original.Status {
				t.Error("HTTP status mismatch")
			}
			if decoded.RequestID != original.RequestID {
				t.Error("HTTP request_id mismatch")
			}
		}
	}
}

func TestJsonlSinkCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "telemetry.jsonl")

	sink, err := NewJsonlTelemetrySink(path)
	if err != nil {
		t.Fatalf("failed to create sink with nested dirs: %v", err)
	}
	defer sink.Close()

	if _, err := os.Stat(filepath.Dir(path)); os.IsNotExist(err) {
		t.Error("parent directories should have been created")
	}
}

func TestSessionTracerAutoIncrementsSequence(t *testing.T) {
	sink := &MemoryTelemetrySink{}
	tracer := NewSessionTracer("session-42", sink)

	tracer.Record("first", nil)
	tracer.Record("second", nil)

	events := sink.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].SessionTrace.Sequence != 0 {
		t.Errorf("first sequence should be 0, got %d", events[0].SessionTrace.Sequence)
	}
	if events[1].SessionTrace.Sequence != 1 {
		t.Errorf("second sequence should be 1, got %d", events[1].SessionTrace.Sequence)
	}
}

func TestSessionTracerHTTPLifecycle(t *testing.T) {
	sink := &MemoryTelemetrySink{}
	tracer := NewSessionTracer("session-123", sink)

	tracer.RecordHTTPRequestStarted(1, "POST", "/v1/messages", nil)
	tracer.RecordHTTPRequestSucceeded(1, "POST", "/v1/messages", 200, "req-abc", nil)
	tracer.RecordHTTPRequestFailed(2, "POST", "/v1/messages", "timeout", true, nil)

	events := sink.Events()
	// Each HTTP method emits 2 events: the primary + a trace record
	if len(events) != 6 {
		t.Fatalf("expected 6 events (3 primary + 3 traces), got %d", len(events))
	}

	// First: HTTPRequestStarted
	if events[0].Type != EventTypeHTTPRequestStarted {
		t.Errorf("expected http_request_started, got %s", events[0].Type)
	}
	if events[0].SessionID != "session-123" {
		t.Errorf("wrong session ID: %s", events[0].SessionID)
	}
	if events[0].Method != "POST" || events[0].Path != "/v1/messages" {
		t.Error("method/path mismatch")
	}

	// Second: trace for started
	if events[1].Type != EventTypeSessionTrace {
		t.Errorf("expected session_trace, got %s", events[1].Type)
	}
	if events[1].SessionTrace.Name != "http_request_started" {
		t.Errorf("expected trace name 'http_request_started', got '%s'", events[1].SessionTrace.Name)
	}

	// Third: HTTPRequestSucceeded
	if events[2].Type != EventTypeHTTPRequestSucceeded {
		t.Errorf("expected http_request_succeeded, got %s", events[2].Type)
	}
	if events[2].Status != 200 {
		t.Errorf("expected status 200, got %d", events[2].Status)
	}

	// Fifth: HTTPRequestFailed
	if events[4].Type != EventTypeHTTPRequestFailed {
		t.Errorf("expected http_request_failed, got %s", events[4].Type)
	}
	if events[4].Error != "timeout" {
		t.Errorf("expected error 'timeout', got '%s'", events[4].Error)
	}
	if !events[4].Retryable {
		t.Error("expected retryable to be true")
	}
}

func TestSessionTracerRecordAnalytics(t *testing.T) {
	sink := &MemoryTelemetrySink{}
	tracer := NewSessionTracer("session-123", sink)

	evt := NewAnalyticsEvent("cli", "prompt_sent").WithProperty("model", "claude-opus")
	tracer.RecordAnalytics(evt)

	events := sink.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != EventTypeAnalytics {
		t.Errorf("expected analytics, got %s", events[0].Type)
	}
	if events[0].Analytics.Namespace != "cli" || events[0].Analytics.Action != "prompt_sent" {
		t.Error("analytics event fields mismatch")
	}
	if events[1].Type != EventTypeSessionTrace {
		t.Errorf("expected session_trace, got %s", events[1].Type)
	}
}

func TestConcurrentRecordIsSafe(t *testing.T) {
	sink := &MemoryTelemetrySink{}
	tracer := NewSessionTracer("concurrent", sink)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tracer.Record("event", map[string]any{"n": n})
		}(i)
	}
	wg.Wait()

	events := sink.Events()
	if len(events) != 100 {
		t.Errorf("expected 100 events from concurrent writes, got %d", len(events))
	}

	// Verify all sequence numbers are unique
	seqs := make(map[uint64]bool)
	for _, e := range events {
		if e.SessionTrace == nil {
			t.Fatal("expected session trace event")
		}
		if seqs[e.SessionTrace.Sequence] {
			t.Errorf("duplicate sequence number: %d", e.SessionTrace.Sequence)
		}
		seqs[e.SessionTrace.Sequence] = true
	}
}

func TestConcurrentSessionTracer100x10(t *testing.T) {
	// 100 goroutines × 10 events = 1000 events, all with unique sequences.
	sink := &MemoryTelemetrySink{}
	tracer := NewSessionTracer("stress", sink)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				tracer.Record("event", map[string]any{"goroutine": g, "iteration": j})
			}
		}(i)
	}
	wg.Wait()

	events := sink.Events()
	if len(events) != 1000 {
		t.Fatalf("expected 1000 events, got %d", len(events))
	}

	seqs := make(map[uint64]bool)
	for _, e := range events {
		if e.SessionTrace == nil {
			t.Fatal("expected session trace event")
		}
		if seqs[e.SessionTrace.Sequence] {
			t.Errorf("duplicate sequence: %d", e.SessionTrace.Sequence)
		}
		seqs[e.SessionTrace.Sequence] = true
	}
	if len(seqs) != 1000 {
		t.Errorf("expected 1000 unique sequences, got %d", len(seqs))
	}
}

func TestJsonlSinkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roundtrip.jsonl")

	sink, err := NewJsonlTelemetrySink(path)
	if err != nil {
		t.Fatalf("failed to create sink: %v", err)
	}

	events := []TelemetryEvent{
		{Type: EventTypeAnalytics, Analytics: &AnalyticsEvent{Namespace: "cli", Action: "start"}},
		{Type: EventTypeHTTPRequestStarted, SessionID: "s1", Attempt: 1, Method: "POST", Path: "/v1/messages"},
		{Type: EventTypeSessionTrace, SessionTrace: &SessionTraceRecord{SessionID: "s1", Sequence: 0, Name: "trace", TimestampMs: 100}},
	}
	for _, e := range events {
		sink.Record(e)
	}
	sink.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	lines := splitNonEmpty(string(data))
	if len(lines) != len(events) {
		t.Fatalf("expected %d lines, got %d", len(events), len(lines))
	}
	for i, line := range lines {
		var decoded TelemetryEvent
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line %d: invalid JSON: %v", i, err)
		}
		if decoded.Type != events[i].Type {
			t.Errorf("line %d: expected type %s, got %s", i, events[i].Type, decoded.Type)
		}
	}
}

func TestJsonlSinkConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.jsonl")

	sink, err := NewJsonlTelemetrySink(path)
	if err != nil {
		t.Fatalf("failed to create sink: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sink.Record(TelemetryEvent{
				Type:      EventTypeHTTPRequestStarted,
				SessionID: "concurrent",
				Attempt:   uint32(n),
				Method:    "GET",
				Path:      "/test",
			})
		}(i)
	}
	wg.Wait()
	sink.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	lines := splitNonEmpty(string(data))
	if len(lines) != 100 {
		t.Errorf("expected 100 lines from concurrent writes, got %d", len(lines))
	}
	for i, line := range lines {
		var decoded TelemetryEvent
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Errorf("line %d: invalid JSON: %v", i, err)
		}
	}
}

func TestSessionTracerClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tracer_close.jsonl")

	sink, err := NewJsonlTelemetrySink(path)
	if err != nil {
		t.Fatalf("failed to create sink: %v", err)
	}
	tracer := NewSessionTracer("close-test", sink)
	tracer.Record("before_close", nil)

	if err := tracer.Close(); err != nil {
		t.Errorf("expected nil error from Close, got %v", err)
	}
}

func TestSessionTracerCloseNonCloser(t *testing.T) {
	sink := &MemoryTelemetrySink{}
	tracer := NewSessionTracer("no-closer", sink)
	tracer.Record("event", nil)

	if err := tracer.Close(); err != nil {
		t.Errorf("expected nil error from Close on non-closer sink, got %v", err)
	}
}

func TestJsonlSinkErrorHandlerCalled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "error_handler.jsonl")

	sink, err := NewJsonlTelemetrySink(path)
	if err != nil {
		t.Fatalf("failed to create sink: %v", err)
	}

	// Close the sink to cause write errors.
	sink.Close()

	var capturedErr error
	sink.ErrorHandler = func(e error) {
		capturedErr = e
	}

	// This should trigger the error handler.
	sink.Record(TelemetryEvent{Type: EventTypeAnalytics, Analytics: &AnalyticsEvent{Namespace: "cli", Action: "fail"}})

	if capturedErr == nil {
		t.Error("ErrorHandler should have been called with an error")
	}
}

func TestJsonlSinkNoErrorHandlerSilent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "silent.jsonl")

	sink, err := NewJsonlTelemetrySink(path)
	if err != nil {
		t.Fatalf("failed to create sink: %v", err)
	}

	// Close to cause errors, but no error handler set.
	sink.Close()

	// Should not panic.
	sink.Record(TelemetryEvent{Type: EventTypeAnalytics, Analytics: &AnalyticsEvent{Namespace: "cli", Action: "silent"}})
}

func TestJsonlSinkRecordErrReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "err_test.jsonl")

	sink, err := NewJsonlTelemetrySink(path)
	if err != nil {
		t.Fatalf("failed to create sink: %v", err)
	}

	// Writing before close should succeed
	if err := sink.RecordErr(TelemetryEvent{Type: EventTypeAnalytics, Analytics: &AnalyticsEvent{Namespace: "cli", Action: "ok"}}); err != nil {
		t.Errorf("expected no error before close, got %v", err)
	}

	// Close the sink, then try to write
	sink.Close()
	err = sink.RecordErr(TelemetryEvent{Type: EventTypeAnalytics, Analytics: &AnalyticsEvent{Namespace: "cli", Action: "fail"}})
	if err == nil {
		t.Error("expected error writing to closed sink, got nil")
	}
}

func TestTelemetryEventFlatJSONLayoutHTTPFailed(t *testing.T) {
	event := TelemetryEvent{
		Type:      EventTypeHTTPRequestFailed,
		SessionID: "sess-fail",
		Attempt:   2,
		Method:    "POST",
		Path:      "/v1/messages",
		Error:     "connection reset",
		Retryable: true,
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	if raw["type"] != "http_request_failed" {
		t.Errorf("expected type=http_request_failed, got %v", raw["type"])
	}
	if raw["session_id"] != "sess-fail" {
		t.Errorf("expected session_id=sess-fail, got %v", raw["session_id"])
	}
	if raw["error"] != "connection reset" {
		t.Errorf("expected error='connection reset', got %v", raw["error"])
	}
	if raw["retryable"] != true {
		t.Errorf("expected retryable=true, got %v", raw["retryable"])
	}
	if raw["method"] != "POST" {
		t.Errorf("expected method=POST, got %v", raw["method"])
	}
	if raw["attempt"] != float64(2) {
		t.Errorf("expected attempt=2, got %v", raw["attempt"])
	}
}

// splitNonEmpty splits s by newline and returns non-empty strings.
func splitNonEmpty(s string) []string {
	var result []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func TestAnthropicRequestProfileHeaders(t *testing.T) {
	profile := NewAnthropicRequestProfile(
		NewClientIdentity("claude-code", "1.2.3").WithRuntime("go-cli"),
	).WithBeta("tools-2026-04-01").
		WithExtraBody("metadata", map[string]any{"source": "test"})

	headers := profile.HeaderPairs()
	if len(headers) != 3 {
		t.Fatalf("expected 3 header pairs, got %d", len(headers))
	}
	if headers[0][0] != "anthropic-version" || headers[0][1] != DefaultAnthropicVersion {
		t.Errorf("wrong anthropic-version header: %v", headers[0])
	}
	if headers[1][0] != "user-agent" || headers[1][1] != "claude-code/1.2.3" {
		t.Errorf("wrong user-agent header: %v", headers[1])
	}
	expected := "claude-code-20250219,prompt-caching-scope-2026-01-05,tools-2026-04-01"
	if headers[2][0] != "anthropic-beta" || headers[2][1] != expected {
		t.Errorf("wrong anthropic-beta header: %v", headers[2])
	}
}

func TestAnthropicRequestProfileRenderJSONBody(t *testing.T) {
	profile := NewAnthropicRequestProfile(NewClientIdentity("claude-code", "1.2.3")).
		WithExtraBody("metadata", map[string]any{"source": "test"})

	body, err := profile.RenderJSONBody(map[string]any{"model": "claude-sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatal("metadata field not found or wrong type")
	}
	if metadata["source"] != "test" {
		t.Errorf("expected source=test, got %v", metadata["source"])
	}
	if body["betas"] == nil {
		t.Error("betas should be in body")
	}
}

func TestWithBetaDeduplication(t *testing.T) {
	profile := NewAnthropicRequestProfile(NewClientIdentity("test", "1.0")).
		WithBeta(DefaultAgenticBeta) // Already present by default
	if len(profile.Betas) != 2 {
		t.Errorf("expected 2 betas (no duplicate), got %d", len(profile.Betas))
	}
}

func TestClientIdentityUserAgent(t *testing.T) {
	ci := NewClientIdentity("my-app", "2.0.0")
	if ci.UserAgent() != "my-app/2.0.0" {
		t.Errorf("unexpected user agent: %s", ci.UserAgent())
	}
}

func TestNopSinkSatisfiesInterface(t *testing.T) {
	var sink TelemetrySink = NopTelemetrySink{}
	sink.Record(TelemetryEvent{Type: EventTypeAnalytics})
	// No panic, no side effects — just verifying interface satisfaction.
}

func TestMemorySinkReset(t *testing.T) {
	sink := &MemoryTelemetrySink{}
	sink.Record(TelemetryEvent{Type: EventTypeAnalytics})
	sink.Record(TelemetryEvent{Type: EventTypeHTTPRequestStarted})
	if len(sink.Events()) != 2 {
		t.Fatalf("expected 2 events before reset, got %d", len(sink.Events()))
	}
	sink.Reset()
	if len(sink.Events()) != 0 {
		t.Errorf("expected 0 events after reset, got %d", len(sink.Events()))
	}
}

func TestTelemetryEventGoldenFixtures(t *testing.T) {
	goldenDir := "../../testdata/golden"

	tests := []struct {
		name  string
		file  string
		event TelemetryEvent
	}{
		{
			name: "analytics",
			file: "telemetry_event_analytics.json",
			event: TelemetryEvent{
				Type: EventTypeAnalytics,
				Analytics: &AnalyticsEvent{
					Namespace:  "cli",
					Action:     "turn_completed",
					Properties: map[string]any{"model": "claude-sonnet"},
				},
			},
		},
		{
			name: "http_started",
			file: "telemetry_event_http_started.json",
			event: TelemetryEvent{
				Type:      EventTypeHTTPRequestStarted,
				SessionID: "sess-1",
				Attempt:   1,
				Method:    "POST",
				Path:      "/v1/messages",
			},
		},
		{
			name: "http_succeeded",
			file: "telemetry_event_http_succeeded.json",
			event: TelemetryEvent{
				Type:      EventTypeHTTPRequestSucceeded,
				SessionID: "sess-1",
				Attempt:   1,
				Method:    "POST",
				Path:      "/v1/messages",
				Status:    200,
				RequestID: "req-abc",
			},
		},
		{
			name: "http_failed",
			file: "telemetry_event_http_failed.json",
			event: TelemetryEvent{
				Type:      EventTypeHTTPRequestFailed,
				SessionID: "sess-1",
				Attempt:   2,
				Method:    "POST",
				Path:      "/v1/messages",
				Error:     "timeout",
				Retryable: true,
			},
		},
		{
			name: "session_trace",
			file: "telemetry_event_session_trace.json",
			event: TelemetryEvent{
				Type: EventTypeSessionTrace,
				SessionTrace: &SessionTraceRecord{
					SessionID:   "sess-1",
					Sequence:    0,
					Name:        "http_request_started",
					TimestampMs: 1234567890,
					Attributes:  map[string]any{"method": "POST"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			goldenPath := filepath.Join(goldenDir, tt.file)
			golden, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden file %s: %v", goldenPath, err)
			}

			// Marshal event and compare with golden fixture
			got, err := json.Marshal(tt.event)
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}

			// Normalize both for comparison (unmarshal to map then re-marshal sorted)
			var gotMap, wantMap map[string]any
			if err := json.Unmarshal(got, &gotMap); err != nil {
				t.Fatalf("unmarshal got: %v", err)
			}
			if err := json.Unmarshal(golden, &wantMap); err != nil {
				t.Fatalf("unmarshal golden: %v", err)
			}

			gotNorm, _ := json.Marshal(gotMap)
			wantNorm, _ := json.Marshal(wantMap)

			if string(gotNorm) != string(wantNorm) {
				t.Errorf("golden mismatch:\ngot:  %s\nwant: %s", gotNorm, wantNorm)
			}

			// Also verify round-trip
			var decoded TelemetryEvent
			if err := json.Unmarshal(golden, &decoded); err != nil {
				t.Fatalf("unmarshal golden to TelemetryEvent: %v", err)
			}
			if decoded.Type != tt.event.Type {
				t.Errorf("type mismatch: got %s, want %s", decoded.Type, tt.event.Type)
			}
		})
	}
}
