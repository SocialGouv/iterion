package codexsdk

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	agenterrclass "github.com/ethpandaops/agent-sdk-observability/errclass"
	genaiconv "github.com/ethpandaops/agent-sdk-observability/semconv/genaiconv"
	agenttracer "github.com/ethpandaops/agent-sdk-observability/tracer"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	upstreamgenai "go.opentelemetry.io/otel/semconv/v1.40.0/genaiconv"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/message"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/observability"
)

// otelRecorder adapts *observability.Observer to the config.SessionMetricsRecorder
// interface used by the SDK runtime. It threads per-session state (tool span
// correlation, TTFT, model caching) alongside the shared Observer.
type otelRecorder struct {
	obs *observability.Observer

	// Tool-call correlation: tool_use_id -> span + start time.
	toolsMu sync.Mutex
	tools   map[string]*toolCallState

	// TTFT: set by MarkQueryStart, cleared after first AssistantMessage records.
	queryStartNs atomic.Int64
	ttftDone     atomic.Bool

	// Cached model name from the first AssistantMessage — used as the
	// request.model label when observing events that lack explicit model info.
	modelMu sync.RWMutex
	model   string
}

type toolCallState struct {
	span      *agenttracer.Span
	ctx       context.Context //nolint:containedctx // intentional: correlates tool span context across AssistantMessage→UserMessage boundary for trace-linked metrics
	name      string
	startedAt time.Time
}

// Compile-time checks.
var (
	_ config.SessionMetricsRecorder = (*otelRecorder)(nil)
	_ config.QueryLifecycleNotifier = (*otelRecorder)(nil)
)

// newOTelRecorder creates a recorder from typed OTel providers.
// Returns nil if both providers are nil, ensuring zero overhead for unconfigured sessions.
func newOTelRecorder(meterProvider metric.MeterProvider, tracerProvider trace.TracerProvider) *otelRecorder {
	if meterProvider == nil && tracerProvider == nil {
		return nil
	}

	obs, err := observability.New(observability.Config{
		MeterProvider:  meterProvider,
		TracerProvider: tracerProvider,
	})
	if err != nil {
		// The shared library only fails on real meter provider errors
		// (custom providers). Fall back to noop so the SDK remains usable.
		return nil
	}

	return &otelRecorder{
		obs:   obs,
		tools: make(map[string]*toolCallState, 4),
	}
}

// setModel caches the first observed model name.
func (r *otelRecorder) setModel(model string) {
	if model == "" {
		return
	}

	r.modelMu.RLock()
	current := r.model
	r.modelMu.RUnlock()

	if current != "" {
		return
	}

	r.modelMu.Lock()
	if r.model == "" {
		r.model = model
	}
	r.modelMu.Unlock()
}

// currentModel returns the cached model name (may be empty).
func (r *otelRecorder) currentModel() string {
	r.modelMu.RLock()
	defer r.modelMu.RUnlock()

	return r.model
}

// markQueryStart captures the query start time for TTFT recording.
func (r *otelRecorder) markQueryStart() {
	r.queryStartNs.Store(time.Now().UnixNano())
	r.ttftDone.Store(false)
}

// MarkQueryStart implements config.QueryLifecycleNotifier by recording the
// query start time for TTFT measurement.
func (r *otelRecorder) MarkQueryStart() {
	r.markQueryStart()
}

// recordTTFTOnce records TTFT on the first qualifying observation.
func (r *otelRecorder) recordTTFTOnce(ctx context.Context, model string) {
	start := r.queryStartNs.Load()
	if start == 0 {
		return
	}

	if !r.ttftDone.CompareAndSwap(false, true) {
		return
	}

	elapsed := time.Since(time.Unix(0, start)).Seconds()
	r.obs.RecordTTFT(ctx, elapsed, model)
}

// Observe records metrics from a parsed message, dispatching to type-specific handlers.
// The context enables trace correlation and exemplar propagation.
func (r *otelRecorder) Observe(ctx context.Context, msg message.Message) {
	if r == nil || msg == nil {
		return
	}

	switch typed := msg.(type) {
	case *message.AssistantMessage:
		r.observeAssistant(ctx, typed)
	case *message.UserMessage:
		r.observeUser(ctx, typed)
	case *message.ResultMessage:
		r.observeResult(ctx, typed)
	}
}

// observeAssistant records TTFT on the first assistant message and opens
// per-tool spans for each ToolUseBlock. In Codex, completed tool events
// produce both ToolUseBlock and ToolResultBlock in the same AssistantMessage,
// so after opening spans we immediately close any that have a co-located result.
func (r *otelRecorder) observeAssistant(ctx context.Context, msg *message.AssistantMessage) {
	if msg == nil {
		return
	}

	r.setModel(msg.Model)
	r.recordTTFTOnce(ctx, msg.Model)

	// First pass: open tool spans for each ToolUseBlock.
	for _, block := range msg.Content {
		toolUse, ok := block.(*message.ToolUseBlock)
		if !ok {
			continue
		}

		toolCtx, span := r.obs.StartToolSpan(ctx, toolUse.Name, toolUse.ID)

		r.toolsMu.Lock()
		r.tools[toolUse.ID] = &toolCallState{
			span:      span,
			ctx:       toolCtx,
			name:      toolUse.Name,
			startedAt: time.Now(),
		}
		r.toolsMu.Unlock()
	}

	// Second pass: close any tool spans that have a co-located ToolResultBlock.
	// This handles the Codex-specific pattern where item.completed events
	// produce both ToolUseBlock and ToolResultBlock in the same message.
	for _, block := range msg.Content {
		result, ok := block.(*message.ToolResultBlock)
		if !ok {
			continue
		}

		r.closeToolSpan(result)
	}
}

// observeUser closes tool spans for ToolResultBlocks that match tracked tool
// uses. Records duration, tool-call counter, and outcome.
func (r *otelRecorder) observeUser(_ context.Context, msg *message.UserMessage) {
	if msg == nil {
		return
	}

	for _, block := range msg.Content.Blocks() {
		result, ok := block.(*message.ToolResultBlock)
		if !ok {
			continue
		}

		r.closeToolSpan(result)
	}
}

// closeToolSpan ends the tracked tool span matching the given ToolResultBlock,
// recording duration, counter, and outcome metrics. No-op if no matching span exists.
func (r *otelRecorder) closeToolSpan(result *message.ToolResultBlock) {
	r.toolsMu.Lock()

	state, exists := r.tools[result.ToolUseID]
	if exists {
		delete(r.tools, result.ToolUseID)
	}

	r.toolsMu.Unlock()

	if !exists {
		return
	}

	duration := time.Since(state.startedAt).Seconds()
	outcome := "ok"

	if result.IsError {
		outcome = "error"

		state.span.MarkError(observability.ClassExecution)
	}

	state.span.SetAttributes(observability.Outcome(outcome))
	r.obs.RecordToolCallDuration(state.ctx, duration, state.name)
	r.obs.RecordToolCall(state.ctx, state.name, outcome)
	state.span.End()
}

// enrichSpanFromResult sets response attributes and error status on the
// query/session span carried by ctx. Called once when a ResultMessage arrives.
func (r *otelRecorder) enrichSpanFromResult(ctx context.Context, result *message.ResultMessage) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}

	// Response model.
	model := r.currentModel()
	if model != "" {
		span.SetAttributes(genaiconv.ResponseModel(model))
	}

	// Stop reason → gen_ai.response.finish_reasons.
	if result.StopReason != nil && *result.StopReason != "" {
		span.SetAttributes(observability.FinishReasons(*result.StopReason))
	}

	// Error status.
	if result.IsError {
		class := r.classifyResult(result)
		if class != "" {
			span.SetAttributes(class.Attr())
			span.SetStatus(codes.Error, string(class))
		} else {
			span.SetStatus(codes.Error, "unknown error")
		}
	}
}

// observeResult records token usage, cost, and operation duration from a
// terminal result.
func (r *otelRecorder) observeResult(ctx context.Context, result *message.ResultMessage) {
	if result == nil {
		return
	}

	// Enrich the query/session span with response attributes and error status.
	r.enrichSpanFromResult(ctx, result)

	model := r.currentModel()

	// Token usage from the flat Usage struct.
	if result.Usage != nil {
		if result.Usage.InputTokens > 0 {
			r.obs.RecordTokenUsage(ctx, int64(result.Usage.InputTokens),
				upstreamgenai.TokenTypeInput, upstreamgenai.OperationNameChat, model)
		}

		if result.Usage.OutputTokens > 0 {
			r.obs.RecordTokenUsage(ctx, int64(result.Usage.OutputTokens),
				upstreamgenai.TokenTypeOutput, upstreamgenai.OperationNameChat, model)
		}

		if result.Usage.CachedInputTokens > 0 {
			r.obs.RecordTokenUsage(ctx, int64(result.Usage.CachedInputTokens),
				upstreamgenai.TokenTypeAttr("cache_read"), upstreamgenai.OperationNameChat, model)
		}

		if result.Usage.ReasoningOutputTokens > 0 {
			r.obs.RecordTokenUsage(ctx, int64(result.Usage.ReasoningOutputTokens),
				upstreamgenai.TokenTypeAttr("thinking"), upstreamgenai.OperationNameChat, model)
		}
	}

	// Cost from the top-level TotalCostUSD.
	if result.TotalCostUSD != nil && *result.TotalCostUSD > 0 {
		r.obs.RecordCost(ctx, *result.TotalCostUSD, model)
	}

	// Operation duration with model and error.type labels.
	if result.DurationMs > 0 {
		duration := time.Duration(result.DurationMs) * time.Millisecond
		class := r.classifyResult(result)

		r.obs.RecordOperationDuration(ctx, duration.Seconds(),
			upstreamgenai.OperationNameChat, model, class)
	}
}

// classifyResult maps a ResultMessage to an errclass.Class. Uses the shared
// registry for string-matching errors and falls back to subtype-based mapping.
func (r *otelRecorder) classifyResult(result *message.ResultMessage) agenterrclass.Class {
	if !result.IsError {
		return ""
	}

	// Try classifying from stop_reason.
	if result.StopReason != nil {
		if class := r.obs.Classify(errFromText(*result.StopReason)); class != "" && class != agenterrclass.Unknown {
			return class
		}
	}

	// Try classifying from result text.
	if result.Result != nil {
		if class := r.obs.Classify(errFromText(*result.Result)); class != "" && class != agenterrclass.Unknown {
			return class
		}
	}

	// Subtype-based fallback.
	if result.Subtype != "" {
		if class := r.obs.Classify(errFromText(result.Subtype)); class != "" && class != agenterrclass.Unknown {
			return class
		}
	}

	return observability.ClassExecution
}

// errFromText wraps a plain string as an error for classification.
func errFromText(text string) error {
	if text == "" {
		return nil
	}

	return errors.New(text)
}

// initMetricsRecorder creates and stores the OTel recorder on options if providers are configured.
// This is called at runtime entry points (Query, QueryStream, Client.Start).
func initMetricsRecorder(options *config.Options) {
	if options == nil || options.MetricsRecorder != nil {
		return
	}

	mp := options.MeterProvider

	if mp == nil && options.PrometheusRegisterer != nil {
		var err error
		if mp, err = observability.NewPrometheusMeterProvider(options.PrometheusRegisterer); err != nil {
			slog.Warn("failed to create prometheus meter provider, falling back to noop metrics", "error", err)

			mp = nil
		}
	}

	recorder := newOTelRecorder(mp, options.TracerProvider)
	if recorder != nil {
		options.MetricsRecorder = recorder
		options.Observer = recorder.obs
	}
}

// otelRecorderFromOptions extracts the *otelRecorder from options for extended recording methods.
// Returns nil if options or MetricsRecorder is nil, or if the recorder is not an *otelRecorder.
func otelRecorderFromOptions(options *config.Options) *otelRecorder {
	if options == nil || options.MetricsRecorder == nil {
		return nil
	}

	rec, ok := options.MetricsRecorder.(*otelRecorder)
	if !ok {
		return nil
	}

	return rec
}

// startQuerySpan starts a trace span for a query operation, if a tracer is configured.
// The span is named per GenAI semantic conventions ("chat {model}" or just
// "chat" when the model is unset). The first returned value is the derived
// context carrying the new span; the second is the *raw* trace.Span so callers
// can defer span.End() directly.
func startQuerySpan(ctx context.Context, options *config.Options, _ string) (context.Context, trace.Span) {
	rec := otelRecorderFromOptions(options)
	if rec == nil || rec.obs == nil {
		return ctx, tracenoop.Span{}
	}

	model := options.Model
	if model == "" {
		model = rec.currentModel()
	}

	sessionID := options.Resume

	rec.markQueryStart()

	ctx, span := rec.obs.StartQuerySpan(ctx, upstreamgenai.OperationNameChat, model, sessionID)

	return ctx, span.Raw()
}
