package apikit

import (
	"io"
	"sync/atomic"
)

// SessionTracer records telemetry events scoped to a session. It emits both
// the primary event and a SessionTrace record for each operation.
type SessionTracer struct {
	sessionID string
	sequence  atomic.Uint64
	sink      TelemetrySink
}

// NewSessionTracer creates a tracer bound to a session ID and sink.
func NewSessionTracer(sessionID string, sink TelemetrySink) *SessionTracer {
	return &SessionTracer{
		sessionID: sessionID,
		sink:      sink,
	}
}

// SessionID returns the session identifier.
func (t *SessionTracer) SessionID() string {
	return t.sessionID
}

// Close closes the underlying sink if it implements io.Closer.
// Returns nil if the sink does not support closing.
func (t *SessionTracer) Close() error {
	if c, ok := t.sink.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// Record emits a named session trace event with attributes.
func (t *SessionTracer) Record(name string, attributes map[string]any) {
	seq := t.sequence.Add(1) - 1
	record := SessionTraceRecord{
		SessionID:   t.sessionID,
		Sequence:    seq,
		Name:        name,
		TimestampMs: currentTimestampMs(),
		Attributes:  attributes,
	}
	t.sink.Record(TelemetryEvent{
		Type:         EventTypeSessionTrace,
		SessionTrace: &record,
	})
}

// RecordHTTPRequestStarted emits an HTTP request started event + trace.
func (t *SessionTracer) RecordHTTPRequestStarted(attempt uint32, method, path string, attributes map[string]any) {
	t.sink.Record(TelemetryEvent{
		Type:       EventTypeHTTPRequestStarted,
		SessionID:  t.sessionID,
		Attempt:    attempt,
		Method:     method,
		Path:       path,
		Attributes: cloneAttrs(attributes),
	})
	t.Record("http_request_started", mergeTraceFields(method, path, attempt, attributes))
}

// RecordHTTPRequestSucceeded emits an HTTP request succeeded event + trace.
func (t *SessionTracer) RecordHTTPRequestSucceeded(attempt uint32, method, path string, status uint16, requestID string, attributes map[string]any) {
	t.sink.Record(TelemetryEvent{
		Type:       EventTypeHTTPRequestSucceeded,
		SessionID:  t.sessionID,
		Attempt:    attempt,
		Method:     method,
		Path:       path,
		Status:     status,
		RequestID:  requestID,
		Attributes: cloneAttrs(attributes),
	})
	traceAttrs := mergeTraceFields(method, path, attempt, attributes)
	traceAttrs["status"] = status
	if requestID != "" {
		traceAttrs["request_id"] = requestID
	}
	t.Record("http_request_succeeded", traceAttrs)
}

// RecordHTTPRequestFailed emits an HTTP request failed event + trace.
func (t *SessionTracer) RecordHTTPRequestFailed(attempt uint32, method, path, errMsg string, retryable bool, attributes map[string]any) {
	t.sink.Record(TelemetryEvent{
		Type:       EventTypeHTTPRequestFailed,
		SessionID:  t.sessionID,
		Attempt:    attempt,
		Method:     method,
		Path:       path,
		Error:      errMsg,
		Retryable:  retryable,
		Attributes: cloneAttrs(attributes),
	})
	traceAttrs := mergeTraceFields(method, path, attempt, attributes)
	traceAttrs["error"] = errMsg
	traceAttrs["retryable"] = retryable
	t.Record("http_request_failed", traceAttrs)
}

// RecordAnalytics emits an analytics event + trace.
func (t *SessionTracer) RecordAnalytics(event AnalyticsEvent) {
	attrs := make(map[string]any)
	for k, v := range event.Properties {
		attrs[k] = v
	}
	attrs["namespace"] = event.Namespace
	attrs["action"] = event.Action

	t.sink.Record(TelemetryEvent{
		Type:      EventTypeAnalytics,
		Analytics: &event,
	})
	t.Record("analytics", attrs)
}

func mergeTraceFields(method, path string, attempt uint32, attributes map[string]any) map[string]any {
	result := make(map[string]any)
	for k, v := range attributes {
		result[k] = v
	}
	result["method"] = method
	result["path"] = path
	result["attempt"] = attempt
	return result
}

func cloneAttrs(attrs map[string]any) map[string]any {
	if attrs == nil {
		return nil
	}
	result := make(map[string]any, len(attrs))
	for k, v := range attrs {
		result[k] = v
	}
	return result
}
