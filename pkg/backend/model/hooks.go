package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tooldisplay"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

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
func persistToolPayload(ctx context.Context, blobSink ToolBlobWriter, runID, toolUseID, key string, content []byte, data map[string]interface{}) {
	if len(content) == 0 {
		return
	}
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

// NewStoreEventHooks returns EventHooks that emit store events for a given run
// and log emoji-rich console output via the provided logger.
// The logger controls which content fields are included in events:
//   - debug+: prompts, response text
//   - trace:  tool call inputs/outputs, tool call details
//
// ctx is captured by the returned hook closures: filesystem stores ignore
// it but cloud (Mongo) stores honor cancellation/timeout. The hook lifetime
// is bounded by the engine.Run call that constructed it.
func NewStoreEventHooks(ctx context.Context, emitter EventEmitter, runID string, logger *iterlog.Logger) EventHooks {
	attachmentSink, _ := emitter.(AttachmentWriter)
	toolBlobSink, _ := emitter.(ToolBlobWriter)
	turnSink, _ := emitter.(TurnWriter)
	return EventHooks{
		OnLLMPrompt: func(nodeID string, systemPrompt string, userMessage string) {
			data := map[string]interface{}{
				"system_prompt": iterlog.Truncate(systemPrompt, maxFieldSize),
				"user_message":  iterlog.Truncate(userMessage, maxFieldSize),
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventLLMPrompt,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			// Use LogBlock so the prompt body folds under the header
			// in the studio's run log. Pass the full text — truncating
			// at the source loses signal (the studio already provides
			// a Wrap toggle + per-block expand/collapse).
			if userMessage != "" {
				logger.LogBlock(iterlog.LevelInfo, "💬",
					fmt.Sprintf("Prompt [%s]:", nodeID), userMessage)
			}
			if systemPrompt != "" {
				logger.LogBlock(iterlog.LevelDebug, "📝",
					fmt.Sprintf("System prompt [%s]:", nodeID), systemPrompt)
			}
		},

		OnLLMRequest: func(nodeID string, info LLMRequestInfo) {
			data := map[string]interface{}{
				"model":         info.Model,
				"message_count": info.MessageCount,
				"tool_count":    info.ToolCount,
			}
			if info.ReasoningEffort != "" {
				data["reasoning_effort"] = info.ReasoningEffort
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventLLMRequest,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			toolInfo := ""
			if info.ToolCount > 0 {
				toolInfo = fmt.Sprintf(", %d tools", info.ToolCount)
			}
			reasoningInfo := ""
			if info.ReasoningEffort != "" {
				reasoningInfo = fmt.Sprintf(", reasoning=%s", info.ReasoningEffort)
			}
			logger.Logf(iterlog.LevelInfo, "🤖", "LLM call [%s]: %s (%d msgs%s%s)",
				nodeID, info.Model, info.MessageCount, toolInfo, reasoningInfo)
		},

		// OnLLMResponse is intentionally nil: response data surfaces through
		// llm_step_finished events with richer per-step detail.

		OnLLMRetry: func(nodeID string, info RetryInfo) {
			data := map[string]interface{}{
				"attempt":  info.Attempt,
				"delay_ms": info.Delay.Milliseconds(),
			}
			if info.Error != nil {
				data["error"] = info.Error.Error()
			}
			if info.StatusCode != 0 {
				data["status_code"] = info.StatusCode
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventLLMRetry,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			errMsg := ""
			if info.Error != nil {
				errMsg = info.Error.Error()
			}
			logger.Warn("LLM retry [%s]: attempt %d, delay %dms: %s",
				nodeID, info.Attempt, info.Delay.Milliseconds(), errMsg)
		},

		OnLLMStepFinish: func(nodeID string, step LLMStepInfo) {
			data := map[string]interface{}{
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

			// Always include response text in persisted events.
			if step.Text != "" {
				data["response_text"] = iterlog.Truncate(step.Text, maxFieldSize)
			}

			// At trace, include tool call details.
			if logger.IsEnabled(iterlog.LevelTrace) && len(step.ToolCalls) > 0 {
				calls := make([]map[string]interface{}, len(step.ToolCalls))
				for i, tc := range step.ToolCalls {
					calls[i] = map[string]interface{}{
						"tool_name": tc.Name,
						"input":     iterlog.Truncate(string(tc.Input), maxFieldSize),
					}
				}
				data["tool_call_details"] = calls
			}

			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventLLMStepFinished,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			if step.Text != "" {
				// Full response, no preview cap — the studio folds the
				// body under the header so length doesn't crowd the log.
				logger.LogBlock(iterlog.LevelInfo, "💬",
					fmt.Sprintf("LLM response [%s] step %d:", nodeID, step.Number),
					step.Text)
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
						logger.Logf(iterlog.LevelInfo, "🔧", "Tool call [%s]: %s %s", nodeID, tc.Name, detail)
					} else {
						logger.Logf(iterlog.LevelInfo, "🔧", "Tool call [%s]: %s", nodeID, tc.Name)
					}
					if body := tooldisplay.BlockBody(tc.Name, tc.Input); body != "" {
						logger.LogBlock(iterlog.LevelInfo, "🔧",
							fmt.Sprintf("Tool input [%s/%s]:", nodeID, tc.Name),
							body)
					}
					if logger.IsEnabled(iterlog.LevelDebug) {
						logger.LogBlock(iterlog.LevelDebug, "🔧",
							fmt.Sprintf("raw input [%s/%s]:", nodeID, tc.Name),
							string(tc.Input))
					}
				}
			}
			if step.CacheReadTokens > 0 || step.CacheWriteTokens > 0 {
				logger.Logf(iterlog.LevelInfo, "📊", "Step %d [%s]: %d in / %d out tokens (cache: %d read, %d write)",
					step.Number, nodeID, step.InputTokens, step.OutputTokens,
					step.CacheReadTokens, step.CacheWriteTokens)
			} else {
				logger.Logf(iterlog.LevelInfo, "📊", "Step %d [%s]: %d in / %d out tokens",
					step.Number, nodeID, step.InputTokens, step.OutputTokens)
			}
		},

		OnLLMTurnCapture: func(nodeID string, info LLMTurnCaptureInfo) {
			if turnSink == nil {
				// Cloud stores don't satisfy TurnWriter yet; skip silently
				// so the timeline + fork features simply don't light up
				// for those runs (the rest of the LLM loop is unaffected).
				return
			}
			iter := LoopIterationFromContext(ctx)
			toolCalls := make([]store.TurnToolCall, len(info.ToolCalls))
			for i, tc := range info.ToolCalls {
				toolCalls[i] = store.TurnToolCall{
					Name:         tc.Name,
					InputPreview: iterlog.Truncate(string(tc.Input), toolInlineThreshold),
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
				RunID:        runID,
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
				turn.Messages = conv
			}
			if err := turnSink.WriteTurn(ctx, turn); err != nil {
				logger.Warn("turn capture [%s] step %d: %v", nodeID, info.Step, err)
			}
		},

		OnLLMCompacted: func(nodeID string, info LLMCompactInfo) {
			data := map[string]interface{}{
				"before_messages":       info.BeforeMessages,
				"after_messages":        info.AfterMessages,
				"removed_message_count": info.RemovedMessageCount,
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventLLMCompacted,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			logger.Logf(iterlog.LevelInfo, "📦", "Compacted [%s]: %d → %d msgs (%d removed)",
				nodeID, info.BeforeMessages, info.AfterMessages, info.RemovedMessageCount)
		},

		OnToolStarted: func(nodeID string, info LLMToolStartedInfo) {
			data := map[string]interface{}{
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
			persistToolPayload(ctx, toolBlobSink, runID, info.ToolUseID, "input", info.Input, data)
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventToolStarted,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})
			// No console echo here: the claude_code delegate already
			// emits its own `[node#iter/claude-code] 🔧 <Tool> <detail>`
			// line as the SDK stream is decoded, and the claw path logs
			// its step's tool calls from OnLLMStepFinish below — adding
			// a third line here would double-up every entry.
		},

		OnToolCall: func(nodeID string, info LLMToolCallInfo) {
			data := map[string]interface{}{
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
			persistToolPayload(ctx, toolBlobSink, runID, info.ToolUseID, "output", []byte(info.Output), data)

			evtType := store.EventToolCalled
			if info.Error != nil {
				evtType = store.EventToolError
				data["error"] = info.Error.Error()
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   evtType,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			// Console output: errors only — the success case is fully
			// captured by the tool_called event (duration + tool name)
			// and rendered by the Tools tab + in-flight footer in the
			// run view, so a per-call log line is just noise.
			if info.Error != nil {
				logger.Error("Tool error [%s]: %s — %v (%dms)",
					nodeID, info.ToolName, info.Error, info.Duration.Milliseconds())
			}
		},

		OnDelegateStarted: func(nodeID string, backendName string) {
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventDelegateStarted,
				RunID:  runID,
				NodeID: nodeID,
				Data:   map[string]interface{}{"backend": backendName},
			})
			logger.Logf(iterlog.LevelInfo, "🚀", "Delegation started [%s]: backend=%s", nodeID, backendName)
		},

		OnDelegateFinished: func(nodeID string, info DelegateInfo) {
			data := map[string]interface{}{
				"backend":              info.BackendName,
				"duration_ms":          info.Duration.Milliseconds(),
				"tokens":               info.Tokens,
				"exit_code":            info.ExitCode,
				"raw_output_len":       info.RawOutputLen,
				"parse_fallback":       info.ParseFallback,
				"formatting_pass_used": info.FormattingPassUsed,
			}
			if logger.IsEnabled(iterlog.LevelTrace) && info.Stderr != "" {
				data["stderr"] = iterlog.Truncate(info.Stderr, maxFieldSize)
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventDelegateFinished,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			logger.Logf(iterlog.LevelInfo, "✅", "Delegation finished [%s]: %s (%dms, %d tokens)",
				nodeID, info.BackendName, info.Duration.Milliseconds(), info.Tokens)
			if info.FormattingPassUsed {
				logger.Logf(iterlog.LevelDebug, "📐", "Delegation [%s]: two-pass execution used for structured output", nodeID)
			} else if info.ParseFallback {
				logger.Warn("Delegation [%s]: structured output parsing fell back to text wrapper", nodeID)
			}
			if info.Stderr != "" {
				logger.LogBlock(iterlog.LevelDebug, "⚠️",
					fmt.Sprintf("Delegation stderr [%s]:", nodeID), info.Stderr)
			}
		},

		OnDelegateError: func(nodeID string, info DelegateInfo) {
			data := map[string]interface{}{
				"backend":     info.BackendName,
				"duration_ms": info.Duration.Milliseconds(),
				"tokens":      info.Tokens,
				"exit_code":   info.ExitCode,
			}
			if info.Error != nil {
				data["error"] = info.Error.Error()
			}
			if logger.IsEnabled(iterlog.LevelTrace) && info.Stderr != "" {
				data["stderr"] = iterlog.Truncate(info.Stderr, maxFieldSize)
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventDelegateError,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			errMsg := ""
			if info.Error != nil {
				errMsg = info.Error.Error()
			}
			logger.Error("Delegation failed [%s]: %s — %s", nodeID, info.BackendName, errMsg)
		},

		OnDelegateRetry: func(nodeID string, info DelegateInfo) {
			data := map[string]interface{}{
				"backend":  info.BackendName,
				"attempt":  info.Attempt,
				"delay_ms": info.Delay.Milliseconds(),
			}
			if info.Error != nil {
				data["error"] = info.Error.Error()
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventDelegateRetry,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			errMsg := ""
			if info.Error != nil {
				errMsg = info.Error.Error()
			}
			logger.Warn("Delegation retry [%s]: %s attempt %d, delay %dms: %s",
				nodeID, info.BackendName, info.Attempt, info.Delay.Milliseconds(), errMsg)
		},

		// OnToolNodeResult handles direct tool nodes with full I/O content.
		OnToolNodeResult: func(nodeID string, toolName string, input []byte, output string, elapsed time.Duration, err error) {
			data := map[string]interface{}{
				"tool":        toolName,
				"input_size":  len(input),
				"duration_ms": elapsed.Milliseconds(),
			}

			if logger.IsEnabled(iterlog.LevelTrace) {
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
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   evtType,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			if err != nil {
				logger.Error("Tool error [%s]: %s — %v (%dms)",
					nodeID, toolName, err, elapsed.Milliseconds())
			} else {
				logger.Logf(iterlog.LevelInfo, "🔧", "Tool result [%s]: %s → %s (%dms)",
					nodeID, toolName, humanSize(len(output)), elapsed.Milliseconds())
				if output != "" {
					logger.LogBlock(iterlog.LevelDebug, "🔬",
						fmt.Sprintf("Tool output [%s/%s]:", nodeID, toolName),
						iterlog.BlockPreview(output, 1500))
				}
				for _, payload := range scanPreviewURLs(output) {
					_, _ = emitter.AppendEvent(ctx, runID, store.Event{
						Type:   store.EventPreviewURLAvailable,
						RunID:  runID,
						NodeID: nodeID,
						Data:   payload,
					})
					if url, _ := payload["url"].(string); url != "" {
						logger.Logf(iterlog.LevelInfo, "🌐", "Preview URL [%s]: %s", nodeID, url)
					}
				}
				if attachmentSink != nil {
					for _, dir := range scanPreviewScreenshots(output) {
						captureBrowserScreenshot(
							ctx, attachmentSink, emitter,
							runID, nodeID, dir, logger,
						)
					}
				}
			}
		},
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

	data := map[string]interface{}{
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
