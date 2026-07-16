// Package observability provides OpenTelemetry metrics and tracing for the SDK.
package observability

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"

	agenterrclass "github.com/ethpandaops/agent-sdk-observability/errclass"
	"github.com/ethpandaops/agent-sdk-observability/semconv/genaiconv"
	agenttracer "github.com/ethpandaops/agent-sdk-observability/tracer"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	upstreamgenai "go.opentelemetry.io/otel/semconv/v1.40.0/genaiconv"
	"go.opentelemetry.io/otel/trace"

	sdkerrors "github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/version"
)

const (
	instrumentationName    = "github.com/ethpandaops/codex-agent-sdk-go"
	instrumentationVersion = version.Version
)

// providerName is the constant gen_ai.provider.name value for this SDK.
// Upstream's ProviderNameAttr is an open string type; "codex-cli" is
// semconv-conformant and identifies the Codex CLI as the backing provider.
const providerName = upstreamgenai.ProviderNameAttr("codex-cli")

// Config holds the providers and logger used when constructing an Observer.
type Config struct {
	MeterProvider  metric.MeterProvider
	TracerProvider trace.TracerProvider
	Logger         *slog.Logger
}

// Observer holds the meter/tracer and pre-constructed instruments.
type Observer struct {
	tracer  *agenttracer.Recorder
	classes *agenterrclass.Registry
	logger  *slog.Logger

	// Spec GenAI metrics — upstream instrument structs.
	opDuration upstreamgenai.ClientOperationDuration
	tokenUsage upstreamgenai.ClientTokenUsage

	// SDK-specific metrics — raw OTel.
	ttft                 metric.Float64Histogram
	costTotal            metric.Float64Counter
	toolCallsTotal       metric.Int64Counter
	toolCallDuration     metric.Float64Histogram
	hookDispatchDuration metric.Float64Histogram
}

// New returns an Observer. Providers default to noop when nil. The upstream
// instrument constructors can return errors; New propagates them.
func New(cfg Config) (*Observer, error) {
	mp := cfg.MeterProvider
	if mp == nil {
		mp = noopmetric.NewMeterProvider()
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	classes := newErrorRegistry()
	obs := &Observer{
		tracer:  agenttracer.New(cfg.TracerProvider, instrumentationName, instrumentationVersion, classes),
		classes: classes,
		logger:  logger,
	}

	meter := mp.Meter(instrumentationName, metric.WithInstrumentationVersion(instrumentationVersion))
	if err := obs.initMetrics(meter); err != nil {
		return nil, err
	}

	return obs, nil
}

// Noop returns an Observer with noop providers. A noop meter cannot produce a
// real instrument-construction error; a non-nil error here would be a library
// bug, so we panic rather than force every caller to handle an impossible case.
func Noop() *Observer {
	obs, err := New(Config{})
	if err != nil {
		panic("observability.Noop: " + err.Error())
	}

	return obs
}

// Classify returns the error.type class for err. Returns "" for nil.
func (o *Observer) Classify(err error) agenterrclass.Class {
	if err == nil {
		return ""
	}

	return o.classes.Classify(err)
}

// RegisterSentinel wires a sentinel error to a class.
func (o *Observer) RegisterSentinel(err error, class agenterrclass.Class) {
	if o == nil || err == nil {
		return
	}

	o.classes.RegisterSentinel(err, class)
}

// RegisterMatcher appends a custom matcher to the registry.
func (o *Observer) RegisterMatcher(matcher agenterrclass.Matcher) {
	if o == nil || matcher == nil {
		return
	}

	o.classes.RegisterMatcher(matcher)
}

// RecordOperationDuration records the spec gen_ai.client.operation.duration
// histogram with provider+operation positional attributes and optional
// request.model / error.type extras.
func (o *Observer) RecordOperationDuration(
	ctx context.Context,
	seconds float64,
	operationName upstreamgenai.OperationNameAttr,
	model string,
	class agenterrclass.Class,
) {
	errType := upstreamgenai.ErrorTypeOther
	if class != "" {
		errType = upstreamgenai.ErrorTypeAttr(class)
	}

	attrs := []attribute.KeyValue{o.opDuration.AttrErrorType(errType)}
	if model != "" {
		attrs = append(attrs, o.opDuration.AttrRequestModel(model))
	}

	o.opDuration.Record(ctx, seconds, operationName, providerName, attrs...)
}

// RecordTokenUsage records the spec gen_ai.client.token.usage histogram.
// Emits a span event for "thinking" tokens.
func (o *Observer) RecordTokenUsage(
	ctx context.Context,
	tokens int64,
	tokenType upstreamgenai.TokenTypeAttr,
	operationName upstreamgenai.OperationNameAttr,
	model string,
) {
	if tokens <= 0 {
		return
	}

	attrs := []attribute.KeyValue{}
	if model != "" {
		attrs = append(attrs, o.tokenUsage.AttrRequestModel(model))
	}

	o.tokenUsage.Record(ctx, tokens, operationName, providerName, tokenType, attrs...)

	if tokenType == upstreamgenai.TokenTypeAttr("thinking") {
		trace.SpanFromContext(ctx).AddEvent("thinking_tokens",
			trace.WithAttributes(ThinkingTokens(tokens)))
	}
}

// RecordTTFT records time-to-first-token on the SDK-local histogram.
func (o *Observer) RecordTTFT(ctx context.Context, seconds float64, model string) {
	attrs := []attribute.KeyValue{
		genaiconv.ProviderName(providerName),
		genaiconv.OperationName(upstreamgenai.OperationNameChat),
	}

	if model != "" {
		attrs = append(attrs, genaiconv.RequestModel(model))
	}

	o.ttft.Record(ctx, seconds, metric.WithAttributes(attrs...))
}

// RecordCost accumulates USD cost on the SDK-local counter.
func (o *Observer) RecordCost(ctx context.Context, costUSD float64, model string) {
	if costUSD <= 0 {
		return
	}

	attrs := []attribute.KeyValue{
		genaiconv.ProviderName(providerName),
		genaiconv.OperationName(upstreamgenai.OperationNameChat),
	}

	if model != "" {
		attrs = append(attrs, genaiconv.RequestModel(model))
	}

	o.costTotal.Add(ctx, costUSD, metric.WithAttributes(attrs...))
}

// RecordToolCall increments the tool-call counter.
func (o *Observer) RecordToolCall(ctx context.Context, toolName, outcome string) {
	o.toolCallsTotal.Add(ctx, 1,
		metric.WithAttributes(
			genaiconv.ProviderName(providerName),
			genaiconv.OperationName(upstreamgenai.OperationNameExecuteTool),
			genaiconv.ToolName(toolName),
			Outcome(outcome),
		))
}

// RecordToolCallDuration records the tool-call duration histogram.
func (o *Observer) RecordToolCallDuration(ctx context.Context, seconds float64, toolName string) {
	o.toolCallDuration.Record(ctx, seconds,
		metric.WithAttributes(
			genaiconv.ProviderName(providerName),
			genaiconv.OperationName(upstreamgenai.OperationNameExecuteTool),
			genaiconv.ToolName(toolName),
		))
}

// RecordHookDuration records the hook dispatch duration histogram.
func (o *Observer) RecordHookDuration(ctx context.Context, seconds float64, event, outcome string) {
	o.hookDispatchDuration.Record(ctx, seconds,
		metric.WithAttributes(HookEvent(event), Outcome(outcome)))
}

// StartQuerySpan opens the top-level span for a Query()/Client.Query() call.
func (o *Observer) StartQuerySpan(
	ctx context.Context,
	operationName upstreamgenai.OperationNameAttr,
	model string,
	conversationID string,
) (context.Context, *agenttracer.Span) {
	attrs := []attribute.KeyValue{
		genaiconv.OperationName(operationName),
		genaiconv.ProviderName(providerName),
	}
	if model != "" {
		attrs = append(attrs, genaiconv.RequestModel(model))
	}

	if conversationID != "" {
		attrs = append(attrs, genaiconv.ConversationID(conversationID))
	}

	return o.tracer.Start(ctx,
		genaiconv.SpanName(operationName, model),
		trace.SpanKindClient, attrs...)
}

// StartSessionSpan opens a session-level span with the "chat" operation name.
// This is a convenience wrapper around StartQuerySpan for persistent client sessions.
func (o *Observer) StartSessionSpan(ctx context.Context, model, sessionID string) (context.Context, *agenttracer.Span) {
	return o.StartQuerySpan(ctx, upstreamgenai.OperationNameChat, model, sessionID)
}

// StartToolSpan opens a child span for a tool invocation. Spec-conformant
// "execute_tool {name}" span name and gen_ai.operation.name=execute_tool
// attribute per GenAI semantic conventions. callID is optional.
func (o *Observer) StartToolSpan(ctx context.Context, toolName, callID string) (context.Context, *agenttracer.Span) {
	attrs := []attribute.KeyValue{
		genaiconv.OperationName(upstreamgenai.OperationNameExecuteTool),
		genaiconv.ProviderName(providerName),
		genaiconv.ToolName(toolName),
	}
	if callID != "" {
		attrs = append(attrs, genaiconv.ToolCallID(callID))
	}

	return o.tracer.Start(ctx,
		genaiconv.SpanName(upstreamgenai.OperationNameExecuteTool, toolName),
		trace.SpanKindInternal, attrs...,
	)
}

// StartHookSpan opens a child span for a hook dispatch.
func (o *Observer) StartHookSpan(ctx context.Context, event string) (context.Context, *agenttracer.Span) {
	return o.tracer.Start(ctx, "codex.hook.dispatch", trace.SpanKindInternal,
		HookEvent(event))
}

// newErrorRegistry wires Codex-specific errors into the shared error classifier.
func newErrorRegistry() *agenterrclass.Registry {
	reg := agenterrclass.New()
	reg.RegisterDefaults() // context.Canceled / context.DeadlineExceeded

	// Sentinel errors from the internal/errors package.
	reg.RegisterSentinel(sdkerrors.ErrOperationCancelled, agenterrclass.Canceled)
	reg.RegisterSentinel(sdkerrors.ErrRequestTimeout, agenterrclass.Timeout)
	reg.RegisterSentinel(sdkerrors.ErrTransportNotConnected, agenterrclass.Network)
	reg.RegisterSentinel(sdkerrors.ErrClientNotConnected, agenterrclass.Network)
	reg.RegisterSentinel(sdkerrors.ErrClientClosed, agenterrclass.Network)
	reg.RegisterSentinel(sdkerrors.ErrStdinClosed, agenterrclass.Network)
	reg.RegisterSentinel(sdkerrors.ErrControllerStopped, agenterrclass.Network)

	// Typed Codex SDK errors.
	reg.RegisterMatcher(func(err error) (agenterrclass.Class, bool) {
		if _, ok := errors.AsType[*sdkerrors.CLINotFoundError](err); ok {
			return ClassCLINotFound, true
		}

		return "", false
	})
	reg.RegisterMatcher(func(err error) (agenterrclass.Class, bool) {
		if _, ok := errors.AsType[*sdkerrors.CLIConnectionError](err); ok {
			return agenterrclass.Network, true
		}

		return "", false
	})
	reg.RegisterMatcher(func(err error) (agenterrclass.Class, bool) {
		if _, ok := errors.AsType[*sdkerrors.ProcessError](err); ok {
			return ClassProcessError, true
		}

		return "", false
	})
	reg.RegisterMatcher(func(err error) (agenterrclass.Class, bool) {
		if _, ok := errors.AsType[*sdkerrors.MessageParseError](err); ok {
			return ClassParseError, true
		}

		return "", false
	})
	reg.RegisterMatcher(func(err error) (agenterrclass.Class, bool) {
		if _, ok := errors.AsType[*sdkerrors.CLIJSONDecodeError](err); ok {
			return ClassParseError, true
		}

		return "", false
	})

	// String-matching fallback for errors that only surface as plain strings
	// (e.g., server-side errors reported via ResultMessage).
	reg.RegisterMatcher(func(err error) (agenterrclass.Class, bool) {
		joined := strings.ToLower(err.Error())
		switch {
		case strings.Contains(joined, "rate limit"), strings.Contains(joined, "429"):
			return agenterrclass.RateLimited, true
		case strings.Contains(joined, "unauthorized"),
			strings.Contains(joined, "forbidden"),
			strings.Contains(joined, "authentication"),
			strings.Contains(joined, "api key"):
			return agenterrclass.Auth, true
		case strings.Contains(joined, "overload"),
			strings.Contains(joined, "529"),
			strings.Contains(joined, "capacity off switch"):
			return ClassOverload, true
		case strings.Contains(joined, "prompt too long"),
			strings.Contains(joined, "context length"),
			strings.Contains(joined, "too many tokens"):
			return ClassPromptTooLong, true
		case strings.Contains(joined, "billing"),
			strings.Contains(joined, "payment"),
			strings.Contains(joined, "credits"):
			return ClassBilling, true
		}

		return "", false
	})

	return reg
}

// errClassFromHTTPStatus maps an HTTP status to a Class when a retry event
// surfaces one. Returns "" when the status does not map to a known class.
func errClassFromHTTPStatus(status int) agenterrclass.Class {
	switch {
	case status == 401 || status == 403:
		return agenterrclass.Auth
	case status == 402:
		return ClassBilling
	case status == 429:
		return agenterrclass.RateLimited
	case status == 408:
		return agenterrclass.Timeout
	case status == 503 || status == 529:
		return ClassOverload
	case status >= 500:
		return agenterrclass.Upstream5xx
	case status >= 400:
		return agenterrclass.InvalidRequest
	}

	return ""
}

// ClassifyHTTPStatus exposes the status-to-class mapping for use by observation
// paths that have a status code but no error value.
func ClassifyHTTPStatus(status int) agenterrclass.Class {
	return errClassFromHTTPStatus(status)
}
