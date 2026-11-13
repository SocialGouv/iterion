package apikit

import (
	"encoding/json"
	"fmt"
	"time"
)

// TelemetryEventType discriminates TelemetryEvent variants.
type TelemetryEventType string

const (
	EventTypeHTTPRequestStarted   TelemetryEventType = "http_request_started"
	EventTypeHTTPRequestSucceeded TelemetryEventType = "http_request_succeeded"
	EventTypeHTTPRequestFailed    TelemetryEventType = "http_request_failed"
	EventTypeAnalytics            TelemetryEventType = "analytics"
	EventTypeSessionTrace         TelemetryEventType = "session_trace"
)

// TelemetryEvent is a flat struct with a Type discriminator. Fields are
// populated according to the event type (matching Rust's serde(tag="type")
// layout).
type TelemetryEvent struct {
	Type TelemetryEventType `json:"type"`

	// Common HTTP fields (started/succeeded/failed)
	SessionID  string         `json:"session_id,omitempty"`
	Attempt    uint32         `json:"attempt,omitempty"`
	Method     string         `json:"method,omitempty"`
	Path       string         `json:"path,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`

	// Succeeded-only
	Status    uint16 `json:"status,omitempty"`
	RequestID string `json:"request_id,omitempty"`

	// Failed-only
	Error     string `json:"error,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`

	// Analytics
	Analytics *AnalyticsEvent `json:"analytics,omitempty"`

	// SessionTrace
	SessionTrace *SessionTraceRecord `json:"session_trace,omitempty"`
}

// AnalyticsEvent represents a custom analytics event with namespace/action.
type AnalyticsEvent struct {
	Namespace  string         `json:"namespace"`
	Action     string         `json:"action"`
	Properties map[string]any `json:"properties,omitempty"`
}

// NewAnalyticsEvent creates an AnalyticsEvent with the given namespace and action.
func NewAnalyticsEvent(namespace, action string) AnalyticsEvent {
	return AnalyticsEvent{
		Namespace:  namespace,
		Action:     action,
		Properties: make(map[string]any),
	}
}

// WithProperty returns a copy with the given property added.
func (e AnalyticsEvent) WithProperty(key string, value any) AnalyticsEvent {
	if e.Properties == nil {
		e.Properties = make(map[string]any)
	}
	e.Properties[key] = value
	return e
}

// SessionTraceRecord is an individual trace record within a session.
type SessionTraceRecord struct {
	SessionID   string         `json:"session_id"`
	Sequence    uint64         `json:"sequence"`
	Name        string         `json:"name"`
	TimestampMs uint64         `json:"timestamp_ms"`
	Attributes  map[string]any `json:"attributes,omitempty"`
}

// telemetryEventFlat is the JSON-level representation that matches Rust's
// serde(tag = "type", rename_all = "snake_case") layout — all variant fields
// are flattened into a single object alongside the "type" discriminator.
type telemetryEventFlat struct {
	Type TelemetryEventType `json:"type"`

	// HTTP + SessionTrace shared
	SessionID  string         `json:"session_id,omitempty"`
	Attempt    uint32         `json:"attempt,omitempty"`
	Method     string         `json:"method,omitempty"`
	Path       string         `json:"path,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`

	// Succeeded
	Status    uint16 `json:"status,omitempty"`
	RequestID string `json:"request_id,omitempty"`

	// Failed
	Error     string `json:"error,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`

	// Analytics
	Namespace  string         `json:"namespace,omitempty"`
	Action     string         `json:"action,omitempty"`
	Properties map[string]any `json:"properties,omitempty"`

	// SessionTrace — no omitempty on numeric fields: sequence=0 and
	// timestamp_ms=0 are valid values that must be emitted.
	Sequence    *uint64 `json:"sequence,omitempty"`
	Name        string  `json:"name,omitempty"`
	TimestampMs *uint64 `json:"timestamp_ms,omitempty"`
}

// MarshalJSON produces a flat JSON object matching Rust's serde(tag="type")
// layout. For Analytics and SessionTrace events, the inner struct fields are
// promoted to the top level instead of being nested under a sub-key.
func (e TelemetryEvent) MarshalJSON() ([]byte, error) {
	var flat telemetryEventFlat
	flat.Type = e.Type

	switch e.Type {
	case EventTypeAnalytics:
		if e.Analytics != nil {
			flat.Namespace = e.Analytics.Namespace
			flat.Action = e.Analytics.Action
			flat.Properties = e.Analytics.Properties
		}
	case EventTypeSessionTrace:
		if e.SessionTrace != nil {
			flat.SessionID = e.SessionTrace.SessionID
			seq := e.SessionTrace.Sequence
			flat.Sequence = &seq
			flat.Name = e.SessionTrace.Name
			ts := e.SessionTrace.TimestampMs
			flat.TimestampMs = &ts
			flat.Attributes = e.SessionTrace.Attributes
		}
	default:
		flat.SessionID = e.SessionID
		flat.Attempt = e.Attempt
		flat.Method = e.Method
		flat.Path = e.Path
		flat.Attributes = e.Attributes
		flat.Status = e.Status
		flat.RequestID = e.RequestID
		flat.Error = e.Error
		flat.Retryable = e.Retryable
	}

	return json.Marshal(flat)
}

// UnmarshalJSON reads the flat JSON layout and reconstructs the Go struct,
// re-nesting Analytics and SessionTrace fields into their typed sub-structs.
func (e *TelemetryEvent) UnmarshalJSON(data []byte) error {
	var flat telemetryEventFlat
	if err := json.Unmarshal(data, &flat); err != nil {
		return fmt.Errorf("telemetry event: %w", err)
	}

	e.Type = flat.Type

	switch flat.Type {
	case EventTypeAnalytics:
		e.Analytics = &AnalyticsEvent{
			Namespace:  flat.Namespace,
			Action:     flat.Action,
			Properties: flat.Properties,
		}
	case EventTypeSessionTrace:
		var seq, ts uint64
		if flat.Sequence != nil {
			seq = *flat.Sequence
		}
		if flat.TimestampMs != nil {
			ts = *flat.TimestampMs
		}
		e.SessionTrace = &SessionTraceRecord{
			SessionID:   flat.SessionID,
			Sequence:    seq,
			Name:        flat.Name,
			TimestampMs: ts,
			Attributes:  flat.Attributes,
		}
	default:
		e.SessionID = flat.SessionID
		e.Attempt = flat.Attempt
		e.Method = flat.Method
		e.Path = flat.Path
		e.Attributes = flat.Attributes
		e.Status = flat.Status
		e.RequestID = flat.RequestID
		e.Error = flat.Error
		e.Retryable = flat.Retryable
	}

	return nil
}

// TelemetrySink is the interface for recording telemetry events.
// Implementations must be safe for concurrent use.
type TelemetrySink interface {
	Record(event TelemetryEvent)
}

// currentTimestampMs returns the current time in milliseconds since epoch.
func currentTimestampMs() uint64 {
	return uint64(time.Now().UnixMilli())
}
