package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tooldisplay"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// redactingEmitter wraps an EventEmitter and scrubs secret values from
// every event Data field before it is persisted (Layer 0). It is the
// single chokepoint that covers events.jsonl regardless of which hook
// emitted the event. It deliberately implements ONLY AppendEvent — the
// optional capability interfaces (ToolBlobWriter / TurnWriter /
// AttachmentWriter) are detected on the original emitter in
// NewStoreEventHooks and redacted at their own call sites, so wrapping
// must not mask their presence.
//
// observers (ADR-046) fire on every persisted event. This is the
// backend-hook half of the dispatcher's stall-heartbeat seam: high-
// frequency tool_started / tool_called events flow through this emitter,
// NOT the runtime engine's WithEventObserver, so delivering the launch's
// ExtraObservers here — instead of interposing a RunStore wrapper that
// would shadow the store's optional capabilities against the type-probes
// above — keeps the observers fed without degrading the store.
type redactingEmitter struct {
	inner     EventEmitter
	guard     *secretguard.Guard
	observers []func(store.Event)
}

func (r redactingEmitter) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	if r.guard != nil && evt.Data != nil {
		evt.Data = r.guard.RedactMap(evt.Data)
	}
	persisted, err := r.inner.AppendEvent(ctx, runID, evt)
	if err == nil && persisted != nil {
		for _, obs := range r.observers {
			obs(*persisted)
		}
	}
	return persisted, err
}

// sha256Hex returns the hex-encoded SHA-256 of s, or "" when s is
// empty. Used as the TurnCheckpoint.TextDigest fingerprint so the
// studio's per-node timeline can detect identical-output retries
// without loading the full text payload.
func sha256Hex(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// turnMessagesRef names the sibling JSON blob a TurnCheckpoint's
// MessagesRef points at. Deterministic from (nodeID, loopIter, turn)
// so the filesystem TurnStore can synthesise the path from the
// checkpoint metadata alone.
func turnMessagesRef(nodeID string, loopIter, turn int) string {
	return nodeID + "/" + strconv.Itoa(loopIter) + "/" + strconv.Itoa(turn) + ".messages.json"
}

// maxFieldSize is the maximum byte length for a single content field in an event.
// Fields exceeding this limit are truncated to stay within the 10 MB event line limit.
const maxFieldSize = 1 << 20 // 1 MB

// toolInlineThreshold is the byte size up to which tool inputs/outputs
// are stored inline in the event payload. Above this, the full content
// lands in a sidecar blob (runs/<id>/tools/<tool_use_id>/{input,output}),
// and the event carries a 4 KB head preview + a `ref` so the studio can
// fetch the rest paginated on demand. Keeping the threshold equal to the
// preview size means small calls are zero-fetch (the studio sees the
// full content inline) while large outputs (Bash on big files, LLM-
// authored Write/Edit) don't bloat events.jsonl or flood the WS stream.
const toolInlineThreshold = 4096 // 4 KB

// EventEmitter is the subset of store.RunStore used by the event bridge.
type EventEmitter interface {
	AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error)
}

// AttachmentWriter is the optional capability the Browser pane uses to
// persist binary captures (PNG/JPEG screenshots) as run attachments
// reachable through `GET /api/runs/:id/attachments/:name`. The
// production EventEmitter is `*store.FilesystemRunStore` (or its
// Mongo equivalent), both of which already implement WriteAttachment.
// Mocks/tests that pass an emitter without this method silently skip
// screenshot capture.
type AttachmentWriter interface {
	WriteAttachment(ctx context.Context, runID string, rec store.AttachmentRecord, body io.Reader) error
}

// ToolBlobWriter is the optional capability filesystem stores satisfy
// for the per-tool-call sidecar I/O persistence path. When present, tool
// inputs/outputs exceeding `toolInlineThreshold` are written through it
// and the event carries a small head preview + a ref instead of the
// full body. Mongo (cloud) stores don't satisfy it today; the hook
// layer falls back to inline truncation in that case.
type ToolBlobWriter interface {
	WriteToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) (int64, error)
}

// TurnWriter is the optional capability filesystem stores satisfy for
// the per-LLM-turn snapshot persistence path. Each tool-loop iteration
// completing inside the claw backend (or a delegate-call boundary for
// claude_code) is persisted as a store.TurnCheckpoint so the studio's
// timeline + the Fork API have a stable anchor. Mongo (cloud) stores
// don't satisfy it today; the hook layer skips the write when the
// capability is missing rather than failing the LLM call.
type TurnWriter interface {
	WriteTurn(ctx context.Context, t *store.TurnCheckpoint) error
}

// PlanWriter is the optional capability stores satisfy for persisting
// the chronological plan snapshots agents produce via their TodoWrite
// (claude_code) / todo_write (claw) tool. When present, the tool-started
// hook captures each snapshot (filesystem → runs/<id>/plans/, Mongo →
// run_plans collection). The hook skips the capture when the capability
// is missing rather than failing the LLM call. In cloud mode the runner's
// metricsEmitter wrapper forwards this interface to the inner Mongo store
// (see pkg/runner metricsEmitter.AppendPlanSnapshot), otherwise the plain
// `emitter.(PlanWriter)` assertion below would hide it. See
// store.PlanStore for the format + dedup semantics.
type PlanWriter interface {
	AppendPlanSnapshot(ctx context.Context, runID string, snap store.PlanSnapshot) (store.PlanSnapshot, bool, error)
}

// NodeServedRecorder is the optional capability stores satisfy for
// persisting the last (backend, model) that served a node onto the
// run record. When present, delegate_finished / delegate_error stamp
// run.json so a finished run is self-describing without replaying
// events.jsonl. Missing on test emitters; the runner's metricsEmitter
// must forward it the same way it forwards PlanWriter (otherwise the
// `emitter.(NodeServedRecorder)` assertion would hide the inner store
// on every cloud run).
type NodeServedRecorder interface {
	RecordNodeServed(ctx context.Context, runID, nodeID string, served store.NodeServed) error
}

// persistToolPayload writes the given content into the event `data` map
// under the given key (`input` or `output`):
//   - if content fits inline (≤ toolInlineThreshold), `data[key]` carries
//     the full bytes;
//   - otherwise the full bytes go to a sidecar blob via blobSink, and the
//     event carries `data[key+"_preview"]` (first 4 KB), `data[key+"_size"]`
//     (total bytes), and `data[key+"_ref"]` (= toolUseID — the path is
//     deterministic from run_id + tool_use_id + kind).
//
// When blobSink is nil or toolUseID is empty (legacy paths, cloud
// stores), falls back to capped inline persistence so the studio still
// shows *something*.
func persistToolPayload(ctx context.Context, guard *secretguard.Guard, blobSink ToolBlobWriter, runID, toolUseID, key string, content []byte, data map[string]any) {
	if len(content) == 0 {
		return
	}
	// Scrub secrets once at the top so both the inline field and the
	// sidecar blob (which bypasses the redacting AppendEvent wrapper)
	// carry redacted content.
	content = guard.RedactBytes(content)
	if len(content) <= toolInlineThreshold {
		data[key] = string(content)
		return
	}
	if blobSink == nil || toolUseID == "" {
		// Fallback: cap inline at maxFieldSize so events.jsonl stays
		// readable even without sidecar support.
		data[key] = iterlog.Truncate(string(content), maxFieldSize)
		data[key+"_size"] = len(content)
		return
	}
	size, err := blobSink.WriteToolBlob(ctx, runID, toolUseID, key, content)
	if err != nil {
		// Sidecar write failed — fall back to capped inline so the
		// event still carries the data (degraded preview only).
		data[key] = iterlog.Truncate(string(content), maxFieldSize)
		data[key+"_size"] = len(content)
		return
	}
	data[key+"_preview"] = string(content[:toolInlineThreshold])
	data[key+"_size"] = size
	data[key+"_ref"] = toolUseID
}

// storeHooks bundles the captured state shared by every closure built
// by NewStoreEventHooks. Extracting the closures into methods on this
// receiver lets the constructor read as a flat assembly of named
// handlers without lengthening any single function. The fields mirror
// exactly what the original anonymous closures captured (ctx, runID,
// logger, guard, the redacting emitter wrapper, and the optional
// store-capability sinks), so behaviour is identical.
type storeHooks struct {
	ctx            context.Context
	emitter        EventEmitter // post-wrap; carries the redactingEmitter for AppendEvent
	runID          string
	logger         *iterlog.Logger
	guard          *secretguard.Guard
	red            func(string) string // guard.Redact (nil-safe)
	attachmentSink AttachmentWriter
	toolBlobSink   ToolBlobWriter
	turnSink       TurnWriter
	planSink       PlanWriter
	servedSink     NodeServedRecorder

	// recentInputs holds a redacted preview of each in-flight tool call's
	// input, keyed by ToolUseID, so the error path can show WHAT was rejected.
	// Without it a schema-validation failure logs only "must have required
	// property 'x'", and the offending payload has to be dug out of
	// events.jsonl by hand — which is how one run's six consecutive failures
	// went misdiagnosed as truncation when the model had in fact emitted XML
	// parameter tags inside a JSON string value.
	inputsMu     sync.Mutex
	recentInputs map[string]string

	// driftSeen dedupes model_drift events per (node, declared, effective)
	// so a 92-pass loop does not emit 92 identical warnings.
	driftMu   sync.Mutex
	driftSeen map[string]struct{}
}

// toolInputPreviewMax bounds what the error line carries: enough to see the
// shape of a malformed payload, not enough to flood a console with a large
// tool input.
const toolInputPreviewMax = 600

// maxRecentInputs caps the in-flight map. Entries are normally short-lived
// (one tool start, one tool completion), but a call that never completes would
// otherwise leak; oldest-wins eviction keeps the bound hard.
const maxRecentInputs = 64

func (h *storeHooks) rememberInput(toolUseID string, input []byte) {
	if toolUseID == "" || len(input) == 0 {
		return
	}
	preview := string(input)
	if len(preview) > toolInputPreviewMax {
		preview = preview[:toolInputPreviewMax] + "…"
	}
	if h.red != nil {
		preview = h.red(preview)
	}
	h.inputsMu.Lock()
	defer h.inputsMu.Unlock()
	if h.recentInputs == nil {
		h.recentInputs = make(map[string]string, maxRecentInputs)
	}
	if len(h.recentInputs) >= maxRecentInputs {
		for k := range h.recentInputs {
			delete(h.recentInputs, k)
			break
		}
	}
	h.recentInputs[toolUseID] = preview
}

func (h *storeHooks) takeInput(toolUseID string) string {
	if toolUseID == "" {
		return ""
	}
	h.inputsMu.Lock()
	defer h.inputsMu.Unlock()
	preview := h.recentInputs[toolUseID]
	delete(h.recentInputs, toolUseID)
	return preview
}

// emit is the closure-local shorthand for AppendEvent calls that share
// the captured (ctx, runID) and the (Type/RunID/NodeID/Data) Event
// shape — i.e. every event emitted from the storeHooks closures.
// AppendEvent's error is intentionally discarded here, as it was at
// every call site before this refactor: the store layer already logs
// persistence failures and the hook path must never fail the in-flight
// LLM call.
func (h *storeHooks) emit(nodeID string, evType store.EventType, data map[string]any) {
	_, _ = h.emitter.AppendEvent(h.ctx, h.runID, store.Event{
		Type:   evType,
		RunID:  h.runID,
		NodeID: nodeID,
		Data:   data,
	})
}

// onLLMPrompt implements the OnLLMPrompt hook.
func (h *storeHooks) onLLMPrompt(nodeID string, systemPrompt string, userMessage string) {
	data := map[string]any{
		"system_prompt": iterlog.Truncate(systemPrompt, maxFieldSize),
		"user_message":  iterlog.Truncate(userMessage, maxFieldSize),
	}
	h.emit(nodeID, store.EventLLMPrompt, data)

	// Use LogBlock so the prompt body folds under the header
	// in the studio's run log. Pass the full text — truncating
	// at the source loses signal (the studio already provides
	// a Wrap toggle + per-block expand/collapse).
	if userMessage != "" {
		h.logger.LogBlock(iterlog.LevelInfo, "💬",
			fmt.Sprintf("Prompt [%s]:", nodeID), h.red(userMessage))
	}
	if systemPrompt != "" {
		h.logger.LogBlock(iterlog.LevelDebug, "📝",
			fmt.Sprintf("System prompt [%s]:", nodeID), h.red(systemPrompt))
	}
}

// onLLMRequest implements the OnLLMRequest hook.
func (h *storeHooks) onLLMRequest(nodeID string, info LLMRequestInfo) {
	data := map[string]any{
		"model":         info.Model,
		"message_count": info.MessageCount,
		"tool_count":    info.ToolCount,
	}
	if info.ReasoningEffort != "" {
		data["reasoning_effort"] = info.ReasoningEffort
	}
	h.emit(nodeID, store.EventLLMRequest, data)

	toolInfo := ""
	if info.ToolCount > 0 {
		toolInfo = fmt.Sprintf(", %d tools", info.ToolCount)
	}
	reasoningInfo := ""
	if info.ReasoningEffort != "" {
		reasoningInfo = fmt.Sprintf(", reasoning=%s", info.ReasoningEffort)
	}
	h.logger.Logf(iterlog.LevelInfo, "🤖", "[%s#%d/claw] LLM call: %s (%d msgs%s%s)",
		nodeID, info.Iteration, info.Model, info.MessageCount, toolInfo, reasoningInfo)
}

// onLLMRetry implements the OnLLMRetry hook.
func (h *storeHooks) onLLMRetry(nodeID string, info RetryInfo) {
	data := map[string]any{
		"attempt":  info.Attempt,
		"delay_ms": info.Delay.Milliseconds(),
	}
	if info.Error != nil {
		data["error"] = info.Error.Error()
	}
	if info.StatusCode != 0 {
		data["status_code"] = info.StatusCode
	}
	h.emit(nodeID, store.EventLLMRetry, data)

	errMsg := ""
	if info.Error != nil {
		errMsg = info.Error.Error()
	}
	h.logger.Warn("LLM retry [%s]: attempt %d, delay %dms: %s",
		nodeID, info.Attempt, info.Delay.Milliseconds(), errMsg)
}

// onLLMStepFinish implements the OnLLMStepFinish hook.
func (h *storeHooks) onLLMStepFinish(nodeID string, step LLMStepInfo) {
	data := map[string]any{
		"step":          step.Number,
		"input_tokens":  step.InputTokens,
		"output_tokens": step.OutputTokens,
		"finish_reason": step.FinishReason,
		"tool_calls":    len(step.ToolCalls),
	}
	if step.CacheReadTokens > 0 {
		data["cache_read_tokens"] = step.CacheReadTokens
	}
	if step.CacheWriteTokens > 0 {
		data["cache_write_tokens"] = step.CacheWriteTokens
	}
	if step.ReasoningTokens > 0 {
		data["thinking_tokens"] = step.ReasoningTokens
	}
	if step.ThinkingMs > 0 {
		data["thinking_ms"] = step.ThinkingMs
	}

	// Always include response text in persisted events. Thinking text is
	// deliberately NOT persisted here: it is routinely 10-50 KB per step,
	// events.jsonl is bounded to small payloads (big bodies live in
	// sidecar blobs — see runview MaxEventsPerPage), and the run.log
	// LogBlock below is the surface that renders it.
	if step.Text != "" {
		data["response_text"] = iterlog.Truncate(step.Text, maxFieldSize)
	}

	// At trace, include tool call details.
	if h.logger.IsEnabled(iterlog.LevelTrace) && len(step.ToolCalls) > 0 {
		calls := make([]map[string]any, len(step.ToolCalls))
		for i, tc := range step.ToolCalls {
			calls[i] = map[string]any{
				"tool_name": tc.Name,
				"input":     iterlog.Truncate(string(tc.Input), maxFieldSize),
			}
		}
		data["tool_call_details"] = calls
	}

	h.emit(nodeID, store.EventLLMStepFinished, data)

	// Mid-loop narration for the conversation views. Only tool-bearing
	// steps qualify: in claw's agent loop the final (no-tools) step is
	// the node's answer — often raw structured JSON — which the output
	// card already renders; re-bubbling it as chat is noise.
	if step.Text != "" && len(step.ToolCalls) > 0 {
		h.onAssistantText(nodeID, AssistantTextInfo{Text: step.Text, Iteration: step.Iteration})
	}

	if step.Text != "" {
		// Full response, no preview cap — the studio folds the
		// body under the header so length doesn't crowd the log.
		// The [node#iter/claw] tag must lead the header so the
		// per-node Logs tab's prefix filter associates the line.
		h.logger.LogBlock(iterlog.LevelInfo, "💬",
			fmt.Sprintf("[%s#%d/claw] response step %d:", nodeID, step.Iteration, step.Number),
			h.red(step.Text))
	}
	// Per-tool log line for the claw (in-process) path. The
	// claude_code delegate prints its own
	// `[node#iter/claude-code] 🔧 <Tool> <detail>` line during
	// stream decoding, so we skip those here — the bridge
	// hook in executor.go only ferries event payloads, and
	// the LLMStepInfo arrives only for claw's direct loop.
	if len(step.ToolCalls) > 0 {
		for _, tc := range step.ToolCalls {
			detail := tooldisplay.HeaderDetail(tc.Name, tc.Input, tooldisplay.SnakeCaseKeys)
			if detail != "" {
				h.logger.Logf(iterlog.LevelInfo, "🔧", "[%s#%d/claw] %s %s", nodeID, step.Iteration, tc.Name, h.red(detail))
			} else {
				h.logger.Logf(iterlog.LevelInfo, "🔧", "[%s#%d/claw] %s", nodeID, step.Iteration, tc.Name)
			}
			if body := tooldisplay.BlockBody(tc.Name, tc.Input); body != "" {
				h.logger.LogBlock(iterlog.LevelInfo, "🔧",
					fmt.Sprintf("[%s#%d/claw] tool input %s:", nodeID, step.Iteration, tc.Name),
					h.red(body))
			}
			if h.logger.IsEnabled(iterlog.LevelDebug) {
				h.logger.LogBlock(iterlog.LevelDebug, "🔧",
					fmt.Sprintf("[%s#%d/claw] raw input %s:", nodeID, step.Iteration, tc.Name),
					h.red(string(tc.Input)))
			}
		}
	}
	// Per-step token counts are DEBUG-level detail: they are the noisiest
	// line in a multi-step tool loop (one per turn) and claude_code emits no
	// equivalent, so at INFO they crowd the log (and wrap in narrow panes).
	// The authoritative per-node total + cost still lands on the INFO-level
	// "Node finished" line, so nothing observable is lost at INFO.
	if step.CacheReadTokens > 0 || step.CacheWriteTokens > 0 {
		h.logger.Logf(iterlog.LevelDebug, "📊", "[%s#%d/claw] step %d: %d in / %d out tokens (cache: %d read, %d write)",
			nodeID, step.Iteration, step.Number, step.InputTokens, step.OutputTokens,
			step.CacheReadTokens, step.CacheWriteTokens)
	} else {
		h.logger.Logf(iterlog.LevelDebug, "📊", "[%s#%d/claw] step %d: %d in / %d out tokens",
			nodeID, step.Iteration, step.Number, step.InputTokens, step.OutputTokens)
	}
	// Thinking content folds under its header in the studio log view
	// (LogBlock), so full reasoning text at INFO doesn't crowd the log.
	// Metrics-only line kept as fallback when no text was captured.
	if step.Thinking != "" {
		h.logger.LogBlock(iterlog.LevelInfo, "🧠",
			fmt.Sprintf("[%s#%d/claw] thinking step %d (~%d tok, %dms):",
				nodeID, step.Iteration, step.Number, step.ReasoningTokens, step.ThinkingMs),
			h.red(step.Thinking))
	} else if step.ReasoningTokens > 0 || step.ThinkingMs > 0 {
		h.logger.Logf(iterlog.LevelInfo, "🧠", "[%s#%d/claw] step %d thinking: ~%d tok, %dms",
			nodeID, step.Iteration, step.Number, step.ReasoningTokens, step.ThinkingMs)
	}
}

// onAssistantText implements the OnAssistantText hook. It persists the
// agent's mid-turn narration as an assistant_text event, skipping
// payloads that are just the node's structured JSON answer (the output
// card renders those; a raw-JSON chat bubble is noise). Redaction is
// handled by the redactingEmitter wrapper like every other event.
func (h *storeHooks) onAssistantText(nodeID string, info AssistantTextInfo) {
	text := strings.TrimSpace(info.Text)
	if text == "" || isLikelyStructuredPayload(text) {
		return
	}
	h.emit(nodeID, store.EventAssistantText, map[string]any{
		"text":      iterlog.Truncate(text, maxFieldSize),
		"iteration": info.Iteration,
	})
}

// onUsageCap implements the OnUsageCap hook: it records that the
// operator's own ceiling — not the provider's — is what governed this run.
func (h *storeHooks) onUsageCap(nodeID string, info UsageCapInfo) {
	data := map[string]any{
		"window":  info.Window,
		"family":  info.Family,
		"percent": info.Percent,
		"cap":     info.Cap,
		"mode":    info.Mode,
		"stopped": info.Stopped,
	}
	if !info.ResetsAt.IsZero() {
		data["resets_at"] = info.ResetsAt.UTC().Format(time.RFC3339)
	}
	h.emit(nodeID, store.EventUsageCap, data)
}

// isLikelyStructuredPayload reports whether text is a bare JSON object
// or array — the shape of a structured-output answer rather than
// human-facing narration.
func isLikelyStructuredPayload(text string) bool {
	if text == "" {
		return false
	}
	if c := text[0]; c != '{' && c != '[' {
		return false
	}
	return json.Valid([]byte(text))
}

// onLLMTurnCapture implements the OnLLMTurnCapture hook.
func (h *storeHooks) onLLMTurnCapture(nodeID string, info LLMTurnCaptureInfo) {
	if h.turnSink == nil {
		// Cloud stores don't satisfy TurnWriter yet; skip silently
		// so the timeline + fork features simply don't light up
		// for those runs (the rest of the LLM loop is unaffected).
		return
	}
	// info.Iteration is threaded through applyHooks /
	// delegateHooksFor from the live per-execution context. The
	// hook closure's captured ctx is the engine-level one (always
	// iter 0), so reading it here stamped every TurnCheckpoint with
	// LoopIter=0 and broke per-iteration fork anchoring.
	iter := info.Iteration
	toolCalls := make([]store.TurnToolCall, len(info.ToolCalls))
	for i, tc := range info.ToolCalls {
		toolCalls[i] = store.TurnToolCall{
			Name:         tc.Name,
			InputPreview: iterlog.Truncate(h.red(string(tc.Input)), toolInlineThreshold),
		}
	}
	backend := info.Backend
	if backend == "" {
		backend = delegate.BackendClaw
	}
	turnIdx := info.Step - 1
	if turnIdx < 0 {
		turnIdx = 0
	}
	turn := &store.TurnCheckpoint{
		RunID:        h.runID,
		NodeID:       nodeID,
		LoopIter:     iter,
		TurnIndex:    turnIdx,
		Backend:      backend,
		FinishReason: info.FinishReason,
		ToolCalls:    toolCalls,
		TextDigest:   sha256Hex(info.Text),
		Usage: store.TurnUsage{
			InputTokens:  info.InputTokens,
			OutputTokens: info.OutputTokens,
		},
		SessionID: info.SessionID,
	}
	// Materialise the conversation bytes only when we're
	// about to persist them — the marshal is O(N) in
	// transcript length, and the hook fires on every turn.
	if conv := info.MarshalConversation(); len(conv) > 0 {
		turn.MessagesRef = turnMessagesRef(nodeID, iter, turnIdx)
		// The turn snapshot holds the full conversation (system +
		// user + every tool result) and feeds the Fork API — scrub
		// secrets before it lands on disk.
		turn.Messages = h.guard.RedactBytes(conv)
	}
	if err := h.turnSink.WriteTurn(h.ctx, turn); err != nil {
		h.logger.Warn("turn capture [%s] step %d: %v", nodeID, info.Step, err)
	}
}

// onLLMCompacted implements the OnLLMCompacted hook.
func (h *storeHooks) onLLMCompacted(nodeID string, info LLMCompactInfo) {
	data := map[string]any{
		"before_messages":       info.BeforeMessages,
		"after_messages":        info.AfterMessages,
		"removed_message_count": info.RemovedMessageCount,
	}
	h.emit(nodeID, store.EventLLMCompacted, data)

	h.logger.Logf(iterlog.LevelInfo, "📦", "[%s#%d/claw] compacted: %d → %d msgs (%d removed)",
		nodeID, info.Iteration, info.BeforeMessages, info.AfterMessages, info.RemovedMessageCount)
}

// onToolStarted implements the OnToolStarted hook.
func (h *storeHooks) onToolStarted(nodeID string, info LLMToolStartedInfo) {
	data := map[string]any{
		"tool":       info.ToolName,
		"input_size": info.InputSize,
	}
	if info.ToolUseID != "" {
		data["tool_use_id"] = info.ToolUseID
	}
	// Persist the raw JSON input. Small inputs land inline
	// (`data.input`); large inputs go to a sidecar blob so the
	// event stream stays bounded, with the event carrying a
	// 4 KB preview + a ref the studio uses to fetch the rest
	// paginated.
	persistToolPayload(h.ctx, h.guard, h.toolBlobSink, h.runID, info.ToolUseID, "input", info.Input, data)
	h.rememberInput(info.ToolUseID, info.Input)
	h.emit(nodeID, store.EventToolStarted, data)
	// ADDITIONAL, best-effort: when the tool is a plan write (claude_code
	// TodoWrite / claw todo_write), also snapshot the plan to the per-run
	// plan store. This is purely additive — the tool_started event above
	// (which the studio's live todoChecklist renders) is untouched.
	h.capturePlan(nodeID, info)
	// No console echo here: the claude_code delegate already
	// emits its own `[node#iter/claude-code] 🔧 <Tool> <detail>`
	// line as the SDK stream is decoded, and the claw path logs
	// its step's tool calls from OnLLMStepFinish below — adding
	// a third line here would double-up every entry.
}

// capturePlan persists a TodoWrite/todo_write plan snapshot to the run's
// plan store (filesystem runs/<id>/plans/ or the Mongo run_plans
// collection). Best-effort: any error is logged and swallowed — a
// plan-write failure must never fail the in-flight LLM call, exactly like
// the artifact/attachment sinks. No-ops when the store lacks the
// PlanWriter capability, when the tool isn't a plan write, or when the
// input carries no todos (e.g. a claw `todo_write` read). Todos are
// secret-redacted before landing in the store.
func (h *storeHooks) capturePlan(nodeID string, info LLMToolStartedInfo) {
	if h.planSink == nil || !isPlanTool(info.ToolName) {
		return
	}
	todos := parsePlanTodos(info.Input, h.red)
	if len(todos) == 0 {
		return
	}
	snap := store.PlanSnapshot{
		NodeID:    nodeID,
		Iteration: info.Iteration,
		Tool:      info.ToolName,
		Timestamp: time.Now().UTC(),
		Todos:     todos,
	}
	written, wrote, err := h.planSink.AppendPlanSnapshot(h.ctx, h.runID, snap)
	if err != nil {
		h.logger.Warn("plan capture [%s]: %v", nodeID, err)
		return
	}
	if !wrote {
		// Byte-identical to the previous snapshot — TodoWrite fired with no
		// change. Nothing persisted, no event: the studio already shows it.
		return
	}
	h.emit(nodeID, store.EventPlanWritten, map[string]any{
		"seq":       written.Seq,
		"node_id":   nodeID,
		"iteration": info.Iteration,
		"count":     len(todos),
	})
}

// isPlanTool reports whether a tool name is a plan-writing tool —
// claude_code's `TodoWrite` or claw's `todo_write`.
func isPlanTool(name string) bool {
	return name == "TodoWrite" || name == "todo_write"
}

// parsePlanTodos defensively extracts the normalized todo list from a raw
// TodoWrite/todo_write input. Both backends nest their items under a
// top-level `todos` array; the item fields differ (claude_code carries
// `activeForm`, claw carries `id`+`priority`), so the decode is a union.
// Status is canonicalised to the claude_code vocabulary (`done` →
// `completed`) so the studio renders both backends' plans identically.
// Returns nil on any parse failure or when there are no items (e.g. a
// claw `todo_write` read call). redact scrubs secret values from the
// free-text fields (nil-safe).
func parsePlanTodos(input json.RawMessage, redact func(string) string) []store.PlanTodo {
	if len(input) == 0 {
		return nil
	}
	var wire struct {
		Todos []struct {
			Content    string `json:"content"`
			Status     string `json:"status"`
			ActiveForm string `json:"activeForm"`
			Priority   string `json:"priority"`
			ID         string `json:"id"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(input, &wire); err != nil {
		return nil
	}
	if len(wire.Todos) == 0 {
		return nil
	}
	scrub := func(s string) string {
		if redact == nil {
			return s
		}
		return redact(s)
	}
	out := make([]store.PlanTodo, 0, len(wire.Todos))
	for _, t := range wire.Todos {
		status := t.Status
		if status == "done" { // claw vocabulary → claude_code canonical
			status = "completed"
		}
		out = append(out, store.PlanTodo{
			Content:    scrub(t.Content),
			Status:     status,
			ActiveForm: scrub(t.ActiveForm),
			Priority:   t.Priority,
			ID:         t.ID,
		})
	}
	return out
}

// onToolCall implements the OnToolCall hook.
func (h *storeHooks) onToolCall(nodeID string, info LLMToolCallInfo) {
	data := map[string]any{
		"tool":        info.ToolName,
		"input_size":  info.InputSize,
		"duration_ms": info.Duration.Milliseconds(),
	}
	if info.ToolUseID != "" {
		data["tool_use_id"] = info.ToolUseID
	}
	// Persist the tool's result so the studio's per-node Tools
	// tab renders in+out side-by-side (matching Claude Code's
	// inline display). Small outputs inline; large outputs go
	// to a sidecar blob with a 4 KB preview + ref, fetched
	// paginated on demand.
	persistToolPayload(h.ctx, h.guard, h.toolBlobSink, h.runID, info.ToolUseID, "output", []byte(info.Output), data)

	evtType := store.EventToolCalled
	if info.Error != nil {
		evtType = store.EventToolError
		data["error"] = info.Error.Error()
	}
	h.emit(nodeID, evtType, data)

	// Console output: errors only — the success case is fully
	// captured by the tool_called event (duration + tool name)
	// and rendered by the Tools tab + in-flight footer in the
	// run view, so a per-call log line is just noise.
	if info.Error != nil {
		// The rejected input goes on the same line: an error naming a missing
		// property is not actionable without the payload that omitted it.
		if preview := h.takeInput(info.ToolUseID); preview != "" {
			h.logger.Error("Tool error [%s]: %s — %v (%dms)\n  rejected input: %s",
				nodeID, info.ToolName, info.Error, info.Duration.Milliseconds(), preview)
		} else {
			h.logger.Error("Tool error [%s]: %s — %v (%dms)",
				nodeID, info.ToolName, info.Error, info.Duration.Milliseconds())
		}
	} else {
		h.takeInput(info.ToolUseID)
	}
}

// putDelegateModelFields copies the model/window fields onto an event
// payload, omitting empties and zeros so observers can tell "unknown"
// from a measured empty by the key's absence — the CostUSD precedent.
func putDelegateModelFields(data map[string]any, info DelegateInfo) {
	if info.DeclaredModel != "" {
		data["declared_model"] = info.DeclaredModel
	}
	if info.EffectiveModel != "" {
		data["effective_model"] = info.EffectiveModel
	}
	if info.ContextWindow > 0 {
		data["context_window"] = info.ContextWindow
	}
	if info.MaxOutputTokens > 0 {
		data["max_output_tokens"] = info.MaxOutputTokens
	}
	if info.PeakInputTokens > 0 {
		data["context_used"] = info.PeakInputTokens
	}
}

func (h *storeHooks) emitModelDrift(nodeID string, info DelegateInfo) {
	if info.DeclaredModel == "" || info.EffectiveModel == "" {
		return
	}
	if delegate.SameModelID(info.DeclaredModel, info.EffectiveModel) {
		return
	}
	key := nodeID + "\x00" + info.DeclaredModel + "\x00" + info.EffectiveModel
	h.driftMu.Lock()
	if h.driftSeen == nil {
		h.driftSeen = make(map[string]struct{})
	}
	if _, seen := h.driftSeen[key]; seen {
		h.driftMu.Unlock()
		return
	}
	h.driftSeen[key] = struct{}{}
	h.driftMu.Unlock()
	h.emit(nodeID, store.EventModelDrift, map[string]any{
		"backend":         info.BackendName,
		"declared_model":  info.DeclaredModel,
		"effective_model": info.EffectiveModel,
	})
}

func (h *storeHooks) recordServed(nodeID string, info DelegateInfo) {
	if h.servedSink == nil || nodeID == "" || info.BackendName == "" {
		return
	}
	served := store.NodeServed{
		Backend:         info.BackendName,
		Model:           info.EffectiveModel,
		DeclaredModel:   info.DeclaredModel,
		ContextWindow:   info.ContextWindow,
		MaxOutputTokens: info.MaxOutputTokens,
	}
	if err := h.servedSink.RecordNodeServed(h.ctx, h.runID, nodeID, served); err != nil {
		h.logger.Warn("Could not persist served model [%s]: %v", nodeID, err)
	}
}

func delegateRouteLabel(info DelegateInfo) string {
	label := info.BackendName
	if info.EffectiveModel != "" {
		return label + " " + info.EffectiveModel
	}
	if info.DeclaredModel != "" {
		return label + " " + info.DeclaredModel
	}
	return label
}

// onDelegateStarted implements the OnDelegateStarted hook.
func (h *storeHooks) onDelegateStarted(nodeID string, info DelegateInfo) {
	data := map[string]any{"backend": info.BackendName}
	putDelegateModelFields(data, info)
	h.emit(nodeID, store.EventDelegateStarted, data)
	h.logger.Logf(iterlog.LevelInfo, "🚀", "Delegation started [%s]: %s", nodeID, delegateRouteLabel(info))
}

// onDelegateFinished implements the OnDelegateFinished hook.
func (h *storeHooks) onDelegateFinished(nodeID string, info DelegateInfo) {
	data := map[string]any{
		"backend":              info.BackendName,
		"duration_ms":          info.Duration.Milliseconds(),
		"tokens":               info.Tokens,
		"exit_code":            info.ExitCode,
		"raw_output_len":       info.RawOutputLen,
		"parse_fallback":       info.ParseFallback,
		"formatting_pass_used": info.FormattingPassUsed,
	}
	putDelegateModelFields(data, info)
	// Omitted when the price table did not know the model, so an observer
	// can tell "no cost data" from a measured $0 by the key's absence.
	if info.CostUSD > 0 {
		data["cost_usd"] = info.CostUSD
	}
	if h.logger.IsEnabled(iterlog.LevelTrace) && info.Stderr != "" {
		data["stderr"] = iterlog.Truncate(info.Stderr, maxFieldSize)
	}
	h.emit(nodeID, store.EventDelegateFinished, data)
	h.emitModelDrift(nodeID, info)
	h.recordServed(nodeID, info)

	h.logger.Logf(iterlog.LevelInfo, "✅", "Delegation finished [%s]: %s (%dms, %d tokens)",
		nodeID, delegateRouteLabel(info), info.Duration.Milliseconds(), info.Tokens)
	if info.FormattingPassUsed {
		h.logger.Logf(iterlog.LevelDebug, "📐", "Delegation [%s]: two-pass execution used for structured output", nodeID)
	} else if info.ParseFallback {
		h.logger.Warn("Delegation [%s]: structured output parsing fell back to text wrapper", nodeID)
	}
	if info.Stderr != "" {
		h.logger.LogBlock(iterlog.LevelDebug, "⚠️",
			fmt.Sprintf("Delegation stderr [%s]:", nodeID), h.red(info.Stderr))
	}
}

// onDelegateError implements the OnDelegateError hook.
func (h *storeHooks) onDelegateError(nodeID string, info DelegateInfo) {
	data := map[string]any{
		"backend":     info.BackendName,
		"duration_ms": info.Duration.Milliseconds(),
		"tokens":      info.Tokens,
		"exit_code":   info.ExitCode,
	}
	putDelegateModelFields(data, info)
	if info.Error != nil {
		data["error"] = info.Error.Error()
	}
	if h.logger.IsEnabled(iterlog.LevelTrace) && info.Stderr != "" {
		data["stderr"] = iterlog.Truncate(info.Stderr, maxFieldSize)
	}
	h.emit(nodeID, store.EventDelegateError, data)
	h.emitModelDrift(nodeID, info)
	// A failed attempt typically has no EffectiveModel. Last-write-wins
	// would blank a model recorded by an earlier success — the fact a
	// failed run.json must still keep (#474). Only persist when the
	// backend actually reported one.
	if info.EffectiveModel != "" {
		h.recordServed(nodeID, info)
	}

	errMsg := ""
	if info.Error != nil {
		errMsg = info.Error.Error()
	}
	h.logger.Error("Delegation failed [%s]: %s — %s", nodeID, delegateRouteLabel(info), errMsg)
}

// onDelegateRetry implements the OnDelegateRetry hook.
func (h *storeHooks) onDelegateRetry(nodeID string, info DelegateInfo) {
	data := map[string]any{
		"backend":  info.BackendName,
		"attempt":  info.Attempt,
		"delay_ms": info.Delay.Milliseconds(),
	}
	putDelegateModelFields(data, info)
	if info.Error != nil {
		data["error"] = info.Error.Error()
	}
	h.emit(nodeID, store.EventDelegateRetry, data)

	errMsg := ""
	if info.Error != nil {
		errMsg = info.Error.Error()
	}
	h.logger.Warn("Delegation retry [%s]: %s attempt %d, delay %dms: %s",
		nodeID, info.BackendName, info.Attempt, info.Delay.Milliseconds(), errMsg)
}

// onProviderFallback implements the OnProviderFallback hook: it turns a
// chain fall-through into a first-class store event.
//
// The log line is a Warn rather than an Info deliberately — a
// fall-through means the primary route is gone, which an operator wants
// to see even when the run goes on to succeed.
func (h *storeHooks) onProviderFallback(nodeID string, info ProviderFallbackInfo) {
	data := map[string]any{
		"from_backend":  info.FromBackend,
		"to_backend":    info.ToBackend,
		"from_model":    info.FromModel,
		"to_model":      info.ToModel,
		"from_provider": info.From,
		"to_provider":   info.To,
		"reason":        info.Reason,
		"attempts":      info.Attempts,
	}
	if info.Err != nil {
		data["error"] = info.Err.Error()
	}
	h.emit(nodeID, store.EventModelFallback, data)

	errMsg := ""
	if info.Err != nil {
		errMsg = info.Err.Error()
	}
	h.logger.Warn("Model fallback [%s]: %s → %s (%s): %s",
		nodeID, fallbackRouteLabel(info.FromBackend, info.From, info.FromModel),
		fallbackRouteLabel(info.ToBackend, info.To, info.ToModel), info.Reason, errMsg)
}

// fallbackRouteLabel renders one side of a fall-through for the log line
// as `backend[/provider][ model]`, skipping the parts that carry no
// information (an empty provider hint means "auto"; a chain that does
// not vary the model repeats it on both sides).
func fallbackRouteLabel(backend, provider, model string) string {
	label := backend
	if label == "" {
		label = "?"
	}
	if provider != "" {
		label += "/" + provider
	}
	if model != "" {
		label += " " + model
	}
	return label
}

// onToolNodeResult implements the OnToolNodeResult hook for direct tool
// nodes (not LLM tool loops), surfacing full I/O content.
func (h *storeHooks) onToolNodeResult(nodeID string, toolName string, input []byte, output string, elapsed time.Duration, err error) {
	data := map[string]any{
		"tool":        toolName,
		"input_size":  len(input),
		"duration_ms": elapsed.Milliseconds(),
	}

	if h.logger.IsEnabled(iterlog.LevelTrace) {
		if len(input) > 0 {
			data["input"] = iterlog.Truncate(string(input), maxFieldSize)
		}
		if output != "" {
			data["output"] = iterlog.Truncate(output, maxFieldSize)
		}
	}

	evtType := store.EventToolCalled
	if err != nil {
		evtType = store.EventToolError
		data["error"] = err.Error()
	}
	h.emit(nodeID, evtType, data)

	if err != nil {
		h.logger.Error("Tool error [%s]: %s — %v (%dms)",
			nodeID, toolName, err, elapsed.Milliseconds())
	} else {
		h.logger.Logf(iterlog.LevelInfo, "🔧", "Tool result [%s]: %s → %s (%dms)",
			nodeID, toolName, humanSize(len(output)), elapsed.Milliseconds())
		if output != "" {
			h.logger.LogBlock(iterlog.LevelDebug, "🔬",
				fmt.Sprintf("Tool output [%s/%s]:", nodeID, toolName),
				iterlog.BlockPreview(h.red(output), 1500))
		}
		for _, payload := range scanPreviewURLs(output) {
			h.emit(nodeID, store.EventPreviewURLAvailable, payload)
			if url, _ := payload["url"].(string); url != "" {
				h.logger.Logf(iterlog.LevelInfo, "🌐", "Preview URL [%s]: %s", nodeID, url)
			}
		}
		if h.attachmentSink != nil {
			for _, dir := range scanPreviewScreenshots(output) {
				captureBrowserScreenshot(
					h.ctx, h.attachmentSink, h.emitter,
					h.runID, nodeID, dir, h.logger,
				)
			}
			// A tool that GENERATED a deliverable hands it over the same
			// way, so a downstream human gate can show it rather than
			// print a path the browser cannot reach.
			for _, dir := range scanToolAttachments(output) {
				publishToolAttachment(
					h.ctx, h.attachmentSink, h.emitter,
					h.runID, nodeID, dir, h.logger,
				)
			}
		}
	}
}

// NewStoreEventHooks returns EventHooks that emit store events for a given run
// and log emoji-rich console output via the provided logger.
// The logger controls which content fields are included in events:
//   - debug+: prompts, response text
//   - trace:  tool call inputs/outputs, tool call details
//
// ctx is captured by the returned hook closures: filesystem stores ignore
// it but cloud (Mongo) stores honor cancellation/timeout. The hook lifetime
// is bounded by the engine.Run call that constructed it.
// observers (ADR-046) fire on every persisted event emitted through the
// backend-hook layer — the dispatcher's stall-heartbeat seam for the
// high-frequency tool events that bypass the engine's WithEventObserver.
func NewStoreEventHooks(ctx context.Context, emitter EventEmitter, runID string, logger *iterlog.Logger, guard *secretguard.Guard, observers ...func(store.Event)) EventHooks {
	// Capability detection must happen on the ORIGINAL emitter — the
	// redacting wrapper below only implements AppendEvent.
	attachmentSink, _ := emitter.(AttachmentWriter)
	toolBlobSink, _ := emitter.(ToolBlobWriter)
	turnSink, _ := emitter.(TurnWriter)
	planSink, _ := emitter.(PlanWriter)
	servedSink, _ := emitter.(NodeServedRecorder)
	// All event payloads go through the redacting wrapper (Layer 0).
	emitter = redactingEmitter{inner: emitter, guard: guard, observers: observers}
	h := &storeHooks{
		ctx:     ctx,
		emitter: emitter,
		runID:   runID,
		logger:  logger,
		guard:   guard,
		// red scrubs run.log block bodies (a separate sink from
		// events.jsonl). Nil-safe: a nil guard returns the input unchanged.
		red:            guard.Redact,
		attachmentSink: attachmentSink,
		toolBlobSink:   toolBlobSink,
		turnSink:       turnSink,
		planSink:       planSink,
		servedSink:     servedSink,
	}
	return EventHooks{
		OnLLMPrompt:  h.onLLMPrompt,
		OnLLMRequest: h.onLLMRequest,
		// OnLLMResponse is intentionally nil: response data surfaces through
		// llm_step_finished events with richer per-step detail.
		OnLLMRetry:         h.onLLMRetry,
		OnLLMStepFinish:    h.onLLMStepFinish,
		OnAssistantText:    h.onAssistantText,
		OnUsageCap:         h.onUsageCap,
		OnLLMTurnCapture:   h.onLLMTurnCapture,
		OnLLMCompacted:     h.onLLMCompacted,
		OnToolStarted:      h.onToolStarted,
		OnToolCall:         h.onToolCall,
		OnDelegateStarted:  h.onDelegateStarted,
		OnDelegateFinished: h.onDelegateFinished,
		OnDelegateError:    h.onDelegateError,
		OnDelegateRetry:    h.onDelegateRetry,
		OnProviderFallback: h.onProviderFallback,
		// OnToolNodeResult handles direct tool nodes with full I/O content.
		OnToolNodeResult: h.onToolNodeResult,
	}
}

// captureBrowserScreenshot reads a screenshot file from the host
// filesystem (the path was emitted by a tool node via the
// `[iterion] preview_screenshot=<path>` directive) and persists it as
// a run attachment, then emits an `EventBrowserScreenshot` so the
// studio's Browser pane can fetch it through the existing
// `/api/runs/:id/attachments/:name` route. Failures are logged but
// non-fatal — a missing or unreadable file should never abort a tool
// node, since the directive is a best-effort hint.
//
// A future Playwright-driven fast path can bypass the stdout
// directive and write screenshots from inside the runtime; this
// helper stays useful for tools that already shell out to
// puppeteer/wkhtmltoimage/etc.
func captureBrowserScreenshot(
	ctx context.Context,
	sink AttachmentWriter,
	emitter EventEmitter,
	runID, nodeID string,
	dir ScreenshotDirective,
	logger *iterlog.Logger,
) {
	f, err := os.Open(dir.Path)
	if err != nil {
		logger.Warn("Browser screenshot [%s]: open %s: %v", nodeID, dir.Path, err)
		return
	}
	defer f.Close()

	mime := detectScreenshotMIME(dir.Path)
	// Sanitised attachment name. `/` is forbidden; nano-second timestamp
	// keeps captures from a single run unique without coordinating a
	// counter (events.jsonl seq isn't visible from this layer).
	safeNode := sanitizeAttachmentSegment(nodeID)
	if safeNode == "" {
		safeNode = "node"
	}
	name := fmt.Sprintf("browser-%s-%d", safeNode, time.Now().UnixNano())
	rec := store.AttachmentRecord{
		Name:             name,
		OriginalFilename: filepath.Base(dir.Path),
		MIME:             mime,
	}
	if err := sink.WriteAttachment(ctx, runID, rec, f); err != nil {
		logger.Warn("Browser screenshot [%s]: write %s: %v", nodeID, name, err)
		return
	}

	data := map[string]any{
		"attachment_name": name,
		"source":          "tool-stdout",
		"mime":            mime,
	}
	if dir.URL != "" {
		data["url"] = dir.URL
	}
	if dir.ToolCallID != "" {
		data["tool_call_id"] = dir.ToolCallID
	}
	_, _ = emitter.AppendEvent(ctx, runID, store.Event{
		Type:   store.EventBrowserScreenshot,
		RunID:  runID,
		NodeID: nodeID,
		Data:   data,
	})
	logger.Logf(iterlog.LevelInfo, "📸", "Browser screenshot [%s]: %s", nodeID, name)
}

// detectScreenshotMIME picks an image MIME from the file extension.
// We trust the tool author's choice rather than sniffing the body so
// we don't have to buffer the file twice (read once for the sniff,
// read once for the upload).
func detectScreenshotMIME(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "image/png"
	}
}

// sanitizeAttachmentSegment strips characters that store.sanitizePathComponent
// would reject (/, \, :, NUL, control, leading dot) and limits length so
// the eventual attachment dir name stays well-formed.
func sanitizeAttachmentSegment(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	out = strings.TrimLeft(out, "-")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// humanSize formats a byte count as a human-readable string.
func humanSize(bytes int) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%d B", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	}
}
