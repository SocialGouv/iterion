// Package apikit re-exports selected symbols from internal/apikit via type
// aliases. This is the public surface for external consumers (e.g., iterion).
// Internal consumers continue importing internal/apikit unchanged.
package apikit

import (
	"github.com/SocialGouv/claw-code-go/internal/apikit"
)

// SessionTracer records telemetry events scoped to a session.
type SessionTracer = apikit.SessionTracer

// PromptCache manages completion caching and cache break detection.
type PromptCache = apikit.PromptCache

// ModelTokenLimit holds the token limits for a known model.
type ModelTokenLimit = apikit.ModelTokenLimit

// PreflightMessageRequest checks whether a request would exceed the model's
// context window before sending it.
var PreflightMessageRequest = apikit.PreflightMessageRequest

// ResolveModelAlias resolves a model alias to its canonical name.
var ResolveModelAlias = apikit.ResolveModelAlias

// ModelTokenLimitForModel returns the token limits for a known model.
var ModelTokenLimitForModel = apikit.ModelTokenLimitForModel

// ---------------------------------------------------------------------------
// Telemetry surface
// ---------------------------------------------------------------------------
//
// Hosts (iterion, third-party SDK consumers) build TelemetryEvent values
// to feed into a TelemetrySink — typically the OTLP/gRPC exporter shipped
// at pkg/apikit/telemetry/otlpgrpc — without depending on internal/apikit.

// TelemetryEvent is the cross-cutting event payload emitted by clients
// and consumed by sinks (HTTP exporter, gRPC exporter, in-process sinks).
// Variants are discriminated by Type; only the fields relevant to the
// variant are populated.
type TelemetryEvent = apikit.TelemetryEvent

// TelemetryEventType discriminates TelemetryEvent variants.
type TelemetryEventType = apikit.TelemetryEventType

// TelemetrySink consumes TelemetryEvent values. Implementations must be
// safe for concurrent use.
type TelemetrySink = apikit.TelemetrySink

// AnalyticsEvent describes a custom analytics event with a namespace +
// action and free-form properties.
type AnalyticsEvent = apikit.AnalyticsEvent

// SessionTraceRecord is an individual trace within a session.
type SessionTraceRecord = apikit.SessionTraceRecord

// ClientIdentity identifies the client application for the User-Agent
// and embedded telemetry attributes.
type ClientIdentity = apikit.ClientIdentity

// AnthropicRequestProfile holds Anthropic-specific request configuration.
type AnthropicRequestProfile = apikit.AnthropicRequestProfile

// TelemetryEvent type-discriminator constants.
const (
	EventTypeHTTPRequestStarted   = apikit.EventTypeHTTPRequestStarted
	EventTypeHTTPRequestSucceeded = apikit.EventTypeHTTPRequestSucceeded
	EventTypeHTTPRequestFailed    = apikit.EventTypeHTTPRequestFailed
	EventTypeAnalytics            = apikit.EventTypeAnalytics
	EventTypeSessionTrace         = apikit.EventTypeSessionTrace
)

// NewAnalyticsEvent creates an AnalyticsEvent with the given namespace
// and action. Properties are initialised to an empty map.
var NewAnalyticsEvent = apikit.NewAnalyticsEvent

// NewClientIdentity creates a ClientIdentity with the default runtime
// (DefaultRuntime, "go").
var NewClientIdentity = apikit.NewClientIdentity

// NewAnthropicRequestProfile creates a profile with the default betas
// (agentic + prompt-caching-scope).
var NewAnthropicRequestProfile = apikit.NewAnthropicRequestProfile
