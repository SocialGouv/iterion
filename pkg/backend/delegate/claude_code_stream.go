package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/thinktokens"
	"github.com/SocialGouv/iterion/pkg/backend/tooldisplay"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// defaultMaxConsecutiveToolErrors aborts a claude_code session once this
// many tool results error in a row (any success resets the count). It
// guards against degenerate tool-error loops — a resumed/confused agent
// spinning out tool calls that all fail (e.g. a parallel batch cancelled
// by one bad relative-path call), which otherwise burns tokens until the
// run hits its cost/duration budget (observed: ~50 errors / 3 successes
// with zero progress). Override via ITERION_CLAUDE_CODE_MAX_TOOL_ERRORS
// (0 disables the guard).
const defaultMaxConsecutiveToolErrors = 25

// Stream-timeout tiers calibrated for the two failure shapes we
// see in practice:
//
//   - **Cold timeout** (no message yet, session never produced a
//     SystemMessage/AssistantMessage): an SDK or process deadlock
//     manifests immediately. We want to fail fast so the recovery
//     dispatcher can retry without burning minutes on a corpse.
//
//   - **Hot timeout** (at least one message received): claude is
//     genuinely working — possibly waiting on a sub-agent or a
//     long-running tool call. We give it significantly more leeway
//     before declaring the session stuck. Sub-agent runs commonly
//     take 5–10 min before producing the next visible message.
//
// Override either via env (Go duration strings):
//   - ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT
//   - ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT (the hot timeout —
//     name kept for back-compat with earlier behavior).
//
// Set either to "0" to disable that tier.
const (
	defaultStreamColdTimeout = 90 * time.Second
	defaultStreamHotTimeout  = 15 * time.Minute
)

// envDurationOr returns the time.Duration parsed from environment
// variable `name`, falling back to `fallback` when the variable is
// unset or holds an unparseable value.
func envDurationOr(name string, fallback time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func resolveStreamColdTimeout() time.Duration {
	return envDurationOr("ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT", defaultStreamColdTimeout)
}

func resolveStreamHotTimeout() time.Duration {
	return envDurationOr("ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT", defaultStreamHotTimeout)
}

// defaultOrchStallTimeout bounds how long the session may sit idle while the
// model is BLOCKED on an orchestration tool (TaskOutput / Monitor) that it
// reached WITHOUT having spawned a subagent (Task) first. That is the exact
// shape of the observed deadlock: a reviewer called TaskOutput(block:true) on
// a background task that never existed and hung until the 15-min hot timeout,
// producing zero events meanwhile. Waiting on a task it never spawned can
// never make progress, so we abort fast — on this short budget instead of the
// hot budget — and, because the error still carries "session idle for", the
// executor's retry loop auto-re-executes the node on a fresh subprocess (no
// manual resume). A LEGITIMATE TaskOutput that follows a real Task spawn keeps
// the full hot budget: a working subagent can legitimately take minutes.
const defaultOrchStallTimeout = 4 * time.Minute

func resolveOrchStallTimeout() time.Duration {
	return envDurationOr("ITERION_CLAUDE_CODE_ORCH_STALL_TIMEOUT", defaultOrchStallTimeout)
}

// defaultNoProgressTimeout bounds how long the session may keep producing
// output WITHOUT making forward progress. The idle watchdog above only fires
// on SILENCE (no SDK message at all); it is blind to a session that keeps
// emitting AssistantMessages — pure text / thinking, re-planning in circles —
// without ever ACTING. That exact degraded loop was observed after an internet
// outage: claude streamed reasoning for 20+ minutes (so the idle timer kept
// resetting) while calling no tools and landing no commits — zero run-level
// tool events. "Forward progress" here = the agent ACTED or the SDK delivered a
// result: an AssistantMessage carrying a tool_use, a UserMessage carrying a tool
// result, or a turn's ResultMessage. Only those reset this timer; a text/
// thinking-only message does not. When no such progress happens for this budget
// the session is spinning, so we abort — the error carries "session idle for" so
// isDelegateRetryable auto-re-executes the node on a fresh subprocess. It is
// deliberately generous (longer than the hot idle timeout) so a single genuinely
// long-running tool (a big build/test between its tool_use and its result) does
// not trip it. 0 disables it.
const defaultNoProgressTimeout = 25 * time.Minute

func resolveNoProgressTimeout() time.Duration {
	return envDurationOr("ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT", defaultNoProgressTimeout)
}

// isBlockingOrchestrationTool reports whether a tool name is one that BLOCKS
// waiting on a subagent task (as opposed to Task, which spawns and returns).
func isBlockingOrchestrationTool(name string) bool {
	return name == "TaskOutput" || name == "Monitor"
}

// sessionMeta captures cross-cutting metadata extracted from the Claude
// Code message stream that the runtime needs to surface upstream: the
// resolved effective model (after env/settings overrides) and the peak
// "context loaded" — input + cache_creation + cache_read — observed on
// any single assistant turn. Combined with ResultMessage.ModelUsage it
// drives the run-view's per-node model name and context-usage gauge.
type sessionMeta struct {
	effectiveModel  string
	peakContextLoad int
	thinkingTokens  int // approximate extended-thinking tokens (re-encoded text)
	thinkingMs      int // best-effort wall-clock spent thinking, milliseconds
}

// applyClaudeCodeSessionMeta merges the streamed session metadata and
// the final ResultMessage's per-model usage into Result so the runtime
// can stamp them on the node's output for the studio's run view. The
// effective model comes from system/init; the context window + output
// cap come from result.ModelUsage[effective]. When the effective model
// is unknown but ModelUsage has exactly one entry, we use that — some
// proxies key ModelUsage by a name that differs from system/init.
func applyClaudeCodeSessionMeta(out *Result, rm *claudesdk.ResultMessage, sm sessionMeta) {
	if out == nil {
		return
	}
	out.EffectiveModel = sm.effectiveModel
	out.PeakInputTokens = sm.peakContextLoad
	out.ThinkingTokens = sm.thinkingTokens
	out.ThinkingMs = sm.thinkingMs
	if rm == nil {
		return
	}
	if mu, ok := rm.ModelUsage[sm.effectiveModel]; ok {
		out.ContextWindow = mu.ContextWindow
		out.MaxOutputTokens = mu.MaxOutputTokens
		return
	}
	if len(rm.ModelUsage) == 1 {
		for name, mu := range rm.ModelUsage {
			out.ContextWindow = mu.ContextWindow
			out.MaxOutputTokens = mu.MaxOutputTokens
			if out.EffectiveModel == "" {
				out.EffectiveModel = name
			}
			return
		}
	}
}

// runSession opens an interactive Session with the Claude CLI, sends the
// prompt, and consumes the message stream until a ResultMessage arrives. It
// streams agent activity (tool_use, tool_result, text) directly from the typed
// content blocks to the iterion logger — this replaces the previous raw-JSON
// WithMessageCallback path. Hooks (PreToolUse, etc.) only fire when configured
// via Session, which is why we use this mode rather than one-shot Prompt().
//
// An idle-timeout watchdog aborts the session when no message arrives for
// `streamIdleTimeout` — protecting against hung Claude CLI processes that
// otherwise block indefinitely (we observed the SDK occasionally getting
// stuck in ep_poll without any propagated error). The aborted session
// returns an error the runtime classifies as resumable, so the recovery
// dispatcher retries automatically.
func (b *ClaudeCodeBackend) runSession(ctx context.Context, prompt string, task Task, opts []claudesdk.Option) (*claudesdk.ResultMessage, sessionMeta, error) {
	sess := claudesdk.NewSession(opts...)
	defer func() { _ = sess.Close() }()

	// silentExitErr enriches the "session ended without result message" error
	// with the CLI's exit code when available. The bare error is useless for
	// diagnosing why claude died — closing the session forces cmd.Wait() to
	// resolve so ExitCode is populated. Exit 0 means claude exited cleanly
	// without surfacing a result (e.g. unhandled internal error before init,
	// auth pre-flight rejection); non-zero means it crashed (e.g. 127 = "exec
	// not found in container PATH" surfaced by docker exec, signal exits
	// reported as 128+signum).
	silentExitErr := func() error {
		_ = sess.Close()
		return fmt.Errorf("claude session ended without result message (cli_exit_code=%d)", sess.ExitCode())
	}

	if err := sess.Send(ctx, prompt); err != nil {
		return nil, sessionMeta{}, err
	}

	coldTimeout := resolveStreamColdTimeout()
	hotTimeout := resolveStreamHotTimeout()

	// Forward messages from the SDK iterator into a channel so we can select
	// on (item, idle-timer, ctx.Done) and abort cleanly when the session
	// falls silent — range-over-func directly would block until ctx is
	// cancelled (only at max_duration, way too late). See forwardSessionStream.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	items := forwardSessionStream(streamCtx, sess)

	// receivedAny tracks whether the session has emitted at least one
	// message. While false we apply the tighter cold timeout (hung
	// SDK / deadlocked subprocess); once any progress is observed we
	// switch to the hot timeout (give claude room for sub-agent runs
	// or other long tool calls).
	receivedAny := false
	var result *claudesdk.ResultMessage
	// Pass-1 fallback: claude-code's stream-json output sometimes
	// emits the final `result` event with an empty `result` text
	// (only token/duration metadata), even when the assistant
	// produced a substantive final message. Track the last text
	// content from any AssistantMessage so parseSDKOutput can fall
	// back to it when ResultMessage.Result is empty — critical for
	// sandboxed runs where the formatOutput Pass 2 can't recover
	// (the in-container session is unreachable from the host
	// claude that runs the formatting prompt).
	var lastAssistantText string
	var meta sessionMeta
	// lastItemTime anchors the best-effort thinking-time proxy: when an
	// assistant message leads with thinking, the gap since the previous
	// stream item is the wall-clock the model spent reasoning before
	// emitting. The SDK delivers assembled thinking blocks (not deltas), so
	// this inter-message gap is the closest signal available.
	lastItemTime := time.Now()
	// Per-session map correlating tool_use_id → tool name so we can echo
	// the name on the completion hook (ToolResultBlock only carries the
	// correlation ID).
	inFlightTools := make(map[string]string)
	// Circuit-breaker for degenerate tool-error loops (see
	// resolveMaxConsecutiveToolErrors): count CONSECUTIVE tool-result
	// errors, reset on any success, abort when the streak crosses the cap.
	maxToolErrors := resolveMaxConsecutiveToolErrors()
	consecutiveToolErrors := 0
	currentTimeout := coldTimeout
	idle := time.NewTimer(currentTimeout)
	defer idle.Stop()

	// Deadlock guard (see defaultOrchStallTimeout): spawnedTask records whether
	// the model ever spawned a subagent; awaitingBlockingTool records whether
	// its most recent turn left it blocked on TaskOutput/Monitor. Blocked on a
	// blocking orchestration tool with no prior Task spawn == a hung wait that
	// can never return → short-circuit the idle budget.
	spawnedTask := false
	awaitingBlockingTool := false
	orchStall := resolveOrchStallTimeout()

	// Forward-progress watchdog (see defaultNoProgressTimeout): a SECOND timer,
	// distinct from the silence-only idle timer, that resets ONLY on a message
	// proving the agent acted (tool_use / tool_result / Result) — never on
	// text/thinking. Fires when the session keeps talking but stops doing.
	noProgress := resolveNoProgressTimeout()
	var progressTimer *time.Timer
	var progressC <-chan time.Time
	if noProgress > 0 {
		progressTimer = time.NewTimer(noProgress)
		defer progressTimer.Stop()
		progressC = progressTimer.C
	}
	resetProgress := func() {
		if progressTimer == nil {
			return
		}
		if !progressTimer.Stop() {
			select {
			case <-progressTimer.C:
			default:
			}
		}
		progressTimer.Reset(noProgress)
	}

	for {
		// Pick the timeout that matches the current phase and reset
		// the timer for this iteration. Any progress (assistant
		// tokens, tool calls, tool results) flips us into hot mode
		// and grants the longer budget on every subsequent wait.
		currentTimeout = resetIdleTimer(idle, receivedAny, coldTimeout, hotTimeout)
		// If the model is blocked on TaskOutput/Monitor without ever having
		// spawned a Task, clamp the wait to the short orchestration-stall
		// budget: that wait cannot make progress.
		if awaitingBlockingTool && !spawnedTask && orchStall > 0 && (currentTimeout <= 0 || orchStall < currentTimeout) {
			currentTimeout = orchStall
			idle.Reset(orchStall)
		}

		select {
		case it, ok := <-items:
			if !ok {
				// Stream closed without surfacing an error.
				if result == nil {
					return nil, meta, silentExitErr()
				}
				// Backfill an empty Result with the captured last
				// assistant text. This is the load-bearing recovery
				// for sandboxed agents that produce structured JSON
				// in their final assistant message but emit a result
				// event with no `result` text — observed on Opus 4.7
				// xhigh + tools, where claude-code seems to defer
				// the final text to a separate AssistantMessage and
				// the result event carries only stats.
				if backfillEmptyResult(result, lastAssistantText) {
					b.Logger.Info("[%s#%d/claude-code] ↩️  backfilled empty Result with last assistant text at stream close", task.NodeID, task.Iteration)
				} else if result.Result != nil {
					b.Logger.Info("[%s#%d/claude-code] 🏁 stream close: Result already populated (%d chars)", task.NodeID, task.Iteration, len(*result.Result))
				} else {
					b.Logger.Info("[%s#%d/claude-code] 🏁 stream close: Result nil and no assistant text captured", task.NodeID, task.Iteration)
				}
				return result, meta, nil
			}
			if it.err != nil {
				return result, meta, it.err
			}
			// Any incoming item proves the SDK is alive — flip into
			// hot-timeout mode for the rest of the session. Log the
			// time-to-first-message once: the cold-phase silence bug
			// (native:221edac8) is only diagnosable with this crumb —
			// a healthy session lands well under the cold budget.
			if !receivedAny {
				b.Logger.Info("[%s#%d/claude-code] first stream message after %s (cold budget %s)",
					task.NodeID, task.Iteration, time.Since(lastItemTime).Round(100*time.Millisecond), coldTimeout)
			}
			receivedAny = true
			// progressed = this message proves the agent ACTED (invoked a tool)
			// or the SDK delivered a result. Only such messages reset the
			// forward-progress watchdog; a text/thinking-only AssistantMessage
			// does not (that is exactly the spin the watchdog must catch).
			progressed := false
			switch m := it.msg.(type) {
			case *claudesdk.SystemMessage:
				b.handleSystemMessage(m, task, &meta)
			case *claudesdk.AssistantMessage:
				if err := b.handleAssistantMessage(m, task, inFlightTools, &meta, &lastAssistantText, lastItemTime, cancelStream); err != nil {
					return result, meta, err
				}
				// Track subagent orchestration for the deadlock guard: a Task
				// call means real subagents are in play (legit long waits ahead);
				// a TaskOutput/Monitor call leaves this turn blocked awaiting a
				// result. Reset awaiting on each assistant turn so only the LATEST
				// blocking call counts.
				awaitingBlockingTool = false
				if m.Message != nil {
					for _, blk := range m.Message.Content {
						if tu, ok := blk.(*claudesdk.ToolUseBlock); ok {
							progressed = true // the agent invoked a tool
							if tu.Name == "Task" {
								spawnedTask = true
							} else if isBlockingOrchestrationTool(tu.Name) {
								awaitingBlockingTool = true
							}
						}
					}
				}
			case *claudesdk.UserMessage:
				// A tool result came back — the blocking wait (if any) returned.
				awaitingBlockingTool = false
				progressed = true // a tool completed and returned a result
				if err := b.handleUserMessage(m, task, inFlightTools, &consecutiveToolErrors, maxToolErrors, cancelStream); err != nil {
					return result, meta, err
				}
			case *claudesdk.ResultMessage:
				result = m
				progressed = true // a turn completed
				backfillEmptyResult(result, lastAssistantText)
			default:
				if it.msg != nil {
					b.Logger.Debug("[%s#%d/claude-code] 📨 %T message", task.NodeID, task.Iteration, it.msg)
				}
			}
			if progressed {
				resetProgress()
			}
			// Advance the thinking-time anchor to this item's arrival so the
			// next thinking-bearing turn measures only its own reasoning gap.
			lastItemTime = time.Now()
		case <-idle.C:
			if currentTimeout <= 0 {
				continue
			}
			cancelStream()
			// Deadlock case: blocked on TaskOutput/Monitor with no Task spawned.
			// Keep "session idle for" in the message so isDelegateRetryable still
			// classifies it retryable → the executor auto-re-executes the node.
			if awaitingBlockingTool && !spawnedTask {
				b.Logger.Warn("[%s#%d/claude-code] 🪤 blocked on an orchestration tool (TaskOutput/Monitor) with no subagent spawned for %s — likely a deadlock, aborting for auto-retry",
					task.NodeID, task.Iteration, currentTimeout)
				return result, meta, fmt.Errorf("claude session idle for %s — blocked on an orchestration tool (TaskOutput/Monitor) with no subagent spawned (likely deadlock); aborting for auto-retry (tune ITERION_CLAUDE_CODE_ORCH_STALL_TIMEOUT, 0 to disable)", currentTimeout)
			}
			phase := "cold"
			envHint := "ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT"
			if receivedAny {
				phase = "hot"
				envHint = "ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT"
			}
			b.Logger.Warn("[%s#%d/claude-code] no SDK message for %s (%s phase) — aborting",
				task.NodeID, task.Iteration, currentTimeout, phase)
			return result, meta, fmt.Errorf("claude session idle for %s (%s phase) — aborting (set %s to extend, or 0 to disable)", currentTimeout, phase, envHint)
		case <-progressC:
			// The session kept talking (idle timer never fired) but made no
			// forward progress (no tool call, no result) for the whole
			// no-progress budget: a spin (re-planning in circles, the
			// post-outage degraded loop). Abort for auto-retry. "session idle
			// for" keeps isDelegateRetryable classifying it retryable.
			cancelStream()
			b.Logger.Warn("[%s#%d/claude-code] no forward progress (no tool call/result) for %s while still streaming — aborting for auto-retry (spin/degraded loop)",
				task.NodeID, task.Iteration, noProgress)
			return result, meta, fmt.Errorf("claude session idle for %s — no forward progress (no tool call or result while still streaming); aborting for auto-retry (tune ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT, 0 to disable)", noProgress)
		case <-ctx.Done():
			cancelStream()
			return result, meta, ctx.Err()
		}
	}
}

// claudeStreamItem pairs a streamed SDK message with its iterator error so
// the select loop in runSession can watch (item, idle-timer, ctx.Done)
// together instead of blocking on range-over-func.
type claudeStreamItem struct {
	msg claudesdk.Message
	err error
}

// forwardSessionStream pumps the SDK's range-over-func stream into a
// buffered channel so runSession can select on it alongside the idle timer
// and ctx. The goroutine exits on the first iterator error or when streamCtx
// is cancelled; it closes the channel on the way out so a clean stream end
// surfaces as a channel close.
func forwardSessionStream(streamCtx context.Context, sess *claudesdk.Session) <-chan claudeStreamItem {
	items := make(chan claudeStreamItem, 1)
	go func() {
		defer close(items)
		for msg, err := range sess.Stream(streamCtx) {
			select {
			case items <- claudeStreamItem{msg: msg, err: err}:
			case <-streamCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return items
}

// resetIdleTimer picks the phase-appropriate idle budget (cold until the
// session emits its first message, hot thereafter) and re-arms idle for the
// next wait, draining a stale fire if Stop races the timer. A non-positive
// timeout disables the watchdog (idle is left stopped). Returns the chosen
// timeout so the caller can report it on an idle abort.
func resetIdleTimer(idle *time.Timer, receivedAny bool, cold, hot time.Duration) time.Duration {
	currentTimeout := cold
	if receivedAny {
		currentTimeout = hot
	}
	if !idle.Stop() {
		select {
		case <-idle.C:
		default:
		}
	}
	if currentTimeout > 0 {
		idle.Reset(currentTimeout)
	}
	return currentTimeout
}

// backfillEmptyResult copies the captured last assistant text into an empty
// ResultMessage.Result. claude-code sometimes emits the final result event
// with empty Result text even when the assistant produced a substantive
// final message; this recovers it (load-bearing for sandboxed runs where the
// Pass-2 formatter can't reach the in-container session). Returns true when a
// backfill happened.
func backfillEmptyResult(result *claudesdk.ResultMessage, lastAssistantText string) bool {
	if result == nil {
		return false
	}
	if (result.Result == nil || *result.Result == "") && lastAssistantText != "" {
		txt := lastAssistantText
		result.Result = &txt
		return true
	}
	return false
}

// handleSystemMessage logs a streamed SystemMessage and, on the canonical
// `init` event, captures the effective model the CLI resolved to (after env
// vars and settings.json took effect — differs from the workflow-declared
// model when a proxy or alias is in play). Hook-lifecycle subtypes are
// noisy and routed to debug.
func (b *ClaudeCodeBackend) handleSystemMessage(m *claudesdk.SystemMessage, task Task, meta *sessionMeta) {
	if m.Subtype == "init" {
		b.Logger.Info("[%s#%d/claude-code] ⚙️  system/init session=%s model=%s tools=%d mcp=%d",
			task.NodeID, task.Iteration, m.SessionID, m.Model, m.ToolCount(), m.MCPServerCount())
		if m.Model != "" {
			meta.effectiveModel = m.Model
		}
	} else {
		b.Logger.Debug("[%s#%d/claude-code] ⚙️  system/%s session=%s",
			task.NodeID, task.Iteration, m.Subtype, m.SessionID)
	}
}

// handleAssistantMessage processes a streamed AssistantMessage: logs its
// content, fires tool hooks, tracks the peak context load, accumulates the
// best-effort extended-thinking metrics (tokens re-encoded from the thinking
// text; time the gap since lastItemTime), and captures the latest non-empty
// text block as the candidate final answer. Returns a non-nil *ErrRateLimited
// when the assistant text is a forfait quota-exhaustion message, so the
// caller fails fast instead of letting schema validation mislabel it as a
// missing field. Mutates meta and lastAssistantText in place.
func (b *ClaudeCodeBackend) handleAssistantMessage(m *claudesdk.AssistantMessage, task Task, inFlightTools map[string]string, meta *sessionMeta, lastAssistantText *string, lastItemTime time.Time, cancelStream context.CancelFunc) error {
	if m.Message == nil {
		return nil
	}
	logAssistantContent(b.Logger, task.NodeID, task.Iteration, m.Message.Content)
	emitToolHooks(task.Hooks, m.Message.Content, inFlightTools)
	// Peak prompt size across turns ≈ how full the context window got.
	u := m.Message.Usage
	load := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	if load > meta.peakContextLoad {
		meta.peakContextLoad = load
	}
	// Extended-thinking metrics: the provider bills thinking inside
	// output_tokens with no breakdown, so re-encode the thinking text for an
	// approximate count; time is the gap since the previous stream item.
	//
	// Some models redact thinking client-side (observed: claude-opus-4-8 —
	// the CLI streams the block with empty text and only the encrypted
	// signature). A signed-but-empty block still proves the model reasoned,
	// so surface the timing instead of dropping every trace.
	var (
		turnThinking     string
		redactedThinking bool
	)
	for _, block := range m.Message.Content {
		if tk, ok := block.(*claudesdk.ThinkingBlock); ok {
			if tk.Thinking != "" {
				turnThinking += tk.Thinking
			} else if tk.Signature != "" {
				redactedThinking = true
			}
		}
	}
	if turnThinking != "" {
		tokens := thinktokens.Count(turnThinking)
		ms := int(time.Since(lastItemTime) / time.Millisecond)
		meta.thinkingTokens += tokens
		meta.thinkingMs += ms
		// LogBlock so the reasoning text folds under the header in the
		// studio's run log (expand/collapse), like tool I/O and 💬 text.
		b.Logger.LogBlock(iterlog.LevelInfo, "🧠",
			fmt.Sprintf("[%s#%d/claude-code] thinking ~%d tok, %dms:", task.NodeID, task.Iteration, tokens, ms),
			turnThinking)
	} else if redactedThinking {
		ms := int(time.Since(lastItemTime) / time.Millisecond)
		meta.thinkingMs += ms
		b.Logger.Info("[%s#%d/claude-code] 🧠 thinking: %dms (content withheld by provider)", task.NodeID, task.Iteration, ms)
	}
	// Capture the latest non-empty text block — the final assistant message
	// is the model's intended answer (and where it puts the JSON).
	for _, block := range m.Message.Content {
		if tb, ok := block.(*claudesdk.TextBlock); ok && tb.Text != "" {
			*lastAssistantText = tb.Text
			// Rate-limit detection: Anthropic forfait surfaces quota
			// exhaustion as a plain assistant text block; bail with a typed
			// error so the runtime can surface "switch provider" guidance.
			if isRateLimitMessage(tb.Text) {
				b.Logger.Warn("[%s#%d/claude-code] 🚦 rate-limit signal in assistant text — aborting: %s", task.NodeID, task.Iteration, truncate(tb.Text, 200))
				cancelStream()
				detail := strings.TrimSpace(tb.Text)
				kind, resetAt := classifyRateLimit(detail, time.Now())
				return &ErrRateLimited{Provider: BackendClaudeCode, Detail: detail, Kind: kind, ResetAt: resetAt}
			}
			// Narration hook: surface the agent's mid-turn prose to the
			// conversation views. The bridge filters structured-JSON
			// payloads (the node's answer) before persisting.
			if task.Hooks.OnAssistantText != nil {
				task.Hooks.OnAssistantText(tb.Text)
			}
		}
	}
	return nil
}

// handleUserMessage processes a streamed UserMessage (tool results echoed
// back to the model). It fires tool hooks and runs the degenerate-tool-error
// circuit breaker: count CONSECUTIVE tool-result errors, reset on any
// success, and abort once the streak crosses maxToolErrors. Returns a non-nil
// error on abort; mutates consecutiveToolErrors in place.
func (b *ClaudeCodeBackend) handleUserMessage(m *claudesdk.UserMessage, task Task, inFlightTools map[string]string, consecutiveToolErrors *int, maxToolErrors int, cancelStream context.CancelFunc) error {
	b.Logger.Debug("[%s#%d/claude-code] 👤 user message echoed back", task.NodeID, task.Iteration)
	if m.Message == nil {
		return nil
	}
	emitToolHooks(task.Hooks, m.Message.Content, inFlightTools)
	// Log the tool RESULTS (📤/❌). handleAssistantMessage logs the tool USE +
	// assistant text; the results echo back in this user message, so mirror
	// the logging here — otherwise the run log shows what each tool was asked
	// to do but never what it returned. Reuses logAssistantContent's
	// ToolResultBlock case (user content carries no tool_use/text blocks, so
	// the other cases are no-ops — no double logging with the assistant path).
	logAssistantContent(b.Logger, task.NodeID, task.Iteration, m.Message.Content)
	for _, block := range m.Message.Content {
		if tr, ok := block.(*claudesdk.ToolResultBlock); ok {
			if tr.IsError {
				*consecutiveToolErrors++
			} else {
				*consecutiveToolErrors = 0
			}
		}
	}
	if maxToolErrors > 0 && *consecutiveToolErrors >= maxToolErrors {
		cancelStream()
		b.Logger.Warn("[%s#%d/claude-code] %d consecutive tool errors — aborting degenerate tool-error loop", task.NodeID, task.Iteration, *consecutiveToolErrors)
		return fmt.Errorf("claude session aborted after %d consecutive tool errors — likely a degenerate tool-error loop (set ITERION_CLAUDE_CODE_MAX_TOOL_ERRORS to tune, 0 to disable)", *consecutiveToolErrors)
	}
	return nil
}

// emitToolHooks walks the content blocks of an AssistantMessage or
// UserMessage and fires the matching TaskHooks callbacks so the engine
// can persist `tool_started` / `tool_called` events for tools that run
// inside the Claude Code CLI subprocess. AssistantMessage carries
// ToolUseBlock (the model has requested a tool); UserMessage carries
// ToolResultBlock (the tool's result is being fed back to the model).
//
// inFlight is a per-session map[tool_use_id]toolName that lets us echo
// the tool's name back on the completion event — the SDK's
// ToolResultBlock only carries the correlation ID. Empty hooks make
// the whole function a no-op.
func emitToolHooks(hooks TaskHooks, blocks []claudesdk.ContentBlock, inFlight map[string]string) {
	for _, block := range blocks {
		switch bl := block.(type) {
		case *claudesdk.ToolUseBlock:
			inFlight[bl.ID] = bl.Name
			if hooks.OnToolStarted != nil {
				var raw json.RawMessage
				if len(bl.Input) > 0 {
					if b, err := json.Marshal(bl.Input); err == nil {
						raw = b
					}
				}
				hooks.OnToolStarted(bl.Name, bl.ID, raw)
			}
		case *claudesdk.ToolResultBlock:
			name := inFlight[bl.ToolUseID]
			delete(inFlight, bl.ToolUseID)
			if hooks.OnToolCalled != nil {
				hooks.OnToolCalled(name, bl.ToolUseID, bl.IsError, toolResultContentText(bl.Content))
			}
		}
	}
}

// toolResultContentText flattens the SDK's ToolResultBlock.Content (any —
// bare string or []claudesdk.ContentBlock) to a single string for the
// engine's tool_called event payload. TextBlocks contribute their Text;
// other block kinds render as a `<type>` sentinel so the operator at least
// knows non-text content was returned. Falls back to JSON marshalling for
// shapes the SDK might add later.
func toolResultContentText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []claudesdk.ContentBlock:
		var sb strings.Builder
		for i, blk := range c {
			if i > 0 {
				sb.WriteByte('\n')
			}
			switch b := blk.(type) {
			case *claudesdk.TextBlock:
				sb.WriteString(b.Text)
			case *claudesdk.ThinkingBlock:
				sb.WriteString("<thinking>")
			case *claudesdk.ToolUseBlock:
				sb.WriteString("<tool_use>")
			case *claudesdk.ToolResultBlock:
				sb.WriteString("<tool_result>")
			default:
				sb.WriteString("<unknown>")
			}
		}
		return sb.String()
	default:
		b, err := json.Marshal(c)
		if err != nil {
			return fmt.Sprintf("%v", c)
		}
		return string(b)
	}
}

// logAssistantContent emits human-readable info logs for tool calls, tool
// results, and text deltas from a single message's content blocks. Called for
// both AssistantMessage content (tool USE + text) and UserMessage content
// (tool RESULTS) — each message kind only carries its own block types, so the
// switch naturally logs the right side without overlap.
func logAssistantContent(logger *iterlog.Logger, nodeID string, iteration int, blocks []claudesdk.ContentBlock) {
	for _, block := range blocks {
		switch bl := block.(type) {
		case *claudesdk.ToolUseBlock:
			displayName := bl.Name
			for _, prefix := range []string{"mcp__claude_code__", "mcp__plugin_claude-mem_mcp-search__", "mcp__iterion__"} {
				if strings.HasPrefix(displayName, prefix) {
					displayName = displayName[len(prefix):]
					break
				}
			}
			header := fmt.Sprintf("[%s#%d/claude-code] 🔧 %s %s", nodeID, iteration, displayName, toolUseDetail(displayName, bl.Input))
			logger.LogBlock(iterlog.LevelInfo, "ℹ️ ", header, toolUseBody(displayName, bl.Input))
		case *claudesdk.ToolResultBlock:
			// Log the tool RESULT as an expandable block (📤 on success, ❌ on
			// error): a truncated one-line preview in the header, the full
			// (bounded) output folded underneath — symmetric with the tool
			// INPUT logged above, and identical to the claw path via the shared
			// tooldisplay.ResultDisplay.
			text := toolResultContentText(bl.Content)
			if text != "" || bl.IsError {
				header, body := tooldisplay.ResultDisplay(text)
				glyph := "📤"
				if bl.IsError {
					glyph = "❌"
					if header == "" {
						header = "tool error"
					}
				}
				logger.LogBlock(iterlog.LevelInfo, "ℹ️ ",
					fmt.Sprintf("[%s#%d/claude-code] %s %s", nodeID, iteration, glyph, header),
					body)
			}
		case *claudesdk.TextBlock:
			if bl.Text != "" {
				// LogBlock so the assistant text folds in the studio's
				// run log; full content, no truncation (the SPA log
				// view handles wrap + per-block expand/collapse).
				logger.LogBlock(iterlog.LevelInfo, "ℹ️ ",
					fmt.Sprintf("[%s#%d/claude-code] 💬", nodeID, iteration),
					bl.Text)
			}
		}
	}
}

// hitYourLimitRe matches the Anthropic forfait window-exhaustion notice
// while tolerating the noun the CLI inserts between "your" and "limit".
// The forfait has (at least) three window shapes and the CLI phrases each
// differently:
//   - 5h:      "You've hit your limit · resets …"
//   - session: "You've hit your session limit · resets 10:30am (UTC)"
//   - weekly:  "You've hit your weekly limit · resets 9pm (Europe/Paris)"
//
// A bare "hit your limit" substring misses the inserted-noun variants: the
// noun ("session" / "weekly", or a future "daily" / "5-hour") sits between
// "your" and "limit" and defeats it. Each missed shape sails through as a
// normal result and fails structured-output validation with a misleading
// "missing required field", crashing the run instead of producing a clean
// resumable rate-limit (observed for "session" on a claude-sonnet-5 fixer,
// see docs/bot-runs/whole-improve-loop.md; and for "weekly" on the
// feed-watch veille runner, 2026-07-20). One tolerant pattern subsumes
// every noun so a new window shape never re-opens this masking bug.
var hitYourLimitRe = regexp.MustCompile(`hit your (?:[a-z0-9-]+ )?limit`)

// rateLimitSignals are case-insensitive substrings of assistant text
// that indicate the upstream provider has cut us off. The forfait
// window-exhaustion shapes ("hit your … limit") are matched by
// hitYourLimitRe above; these cover the remaining upstream/facade forms:
//   - ZAI / Anthropic-shaped facade: "API Error: Request rejected (429)
//     · Usage limit reached for 5 hour. Your limit will reset at …" —
//     the CLI relays the upstream 429 into assistant text.
//
// Kept narrow: generic substrings like "rate_limit_error" were dropped
// because security-audit agents legitimately mention them in prose.
// The 200-char length cap is the second guard against agents quoting
// these phrases mid-paragraph.
var rateLimitSignals = []string{
	"rate limit exceeded",
	"quota exceeded",
	"usage limit reached",
	"request rejected (429)",
}

// isRateLimitMessage reports whether an assistant text block carries
// a quota / rate-limit signal from the upstream provider. The text
// length cap is load-bearing: real rate-limit notices are short
// one-liners, whereas agents that reason aloud about rate limiting
// produce much longer paragraphs that would false-positive otherwise.
func isRateLimitMessage(text string) bool {
	if len(text) == 0 || len(text) > 200 {
		return false
	}
	lower := strings.ToLower(text)
	if hitYourLimitRe.MatchString(lower) {
		return true
	}
	for _, sig := range rateLimitSignals {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// usageWindowSignals are the subset of rate-limit shapes that mean a
// subscription/quota WINDOW is exhausted (the ZAI 5h facade) — waiting
// for the reset is the only cure, so retries inside the window just burn
// attempts. The Anthropic forfait 5h / session / weekly caps ("hit your
// … limit") are all windows too and are matched by hitYourLimitRe in
// classifyRateLimit. Plain throttles ("rate limit exceeded") stay transient.
var usageWindowSignals = []string{
	"usage limit reached",
}

// classifyRateLimit refines a matched rate-limit message into
// (Kind, ResetAt). All parsing is best-effort: an unrecognized shape
// keeps Kind = transient and a zero ResetAt — never a hard failure.
func classifyRateLimit(text string, now time.Time) (kind string, resetAt time.Time) {
	lower := strings.ToLower(text)
	kind = RateLimitKindTransient
	if hitYourLimitRe.MatchString(lower) {
		kind = RateLimitKindUsageWindow
	}
	for _, sig := range usageWindowSignals {
		if strings.Contains(lower, sig) {
			kind = RateLimitKindUsageWindow
			break
		}
	}
	if kind != RateLimitKindUsageWindow {
		return kind, time.Time{}
	}
	return kind, parseResetHint(lower, now)
}

// resetAbsRe matches an explicit absolute instant: "reset at
// 2026-05-13 07:38:08" (the ZAI facade shape). Tried FIRST because the
// looser clock pattern below would otherwise chew the "20" out of "2026"
// and report hour 20 of today.
var resetAbsRe = regexp.MustCompile(`reset[s]?(?: at)?\s+(\d{4})-(\d{2})-(\d{2})[ t](\d{1,2}):(\d{2})(?::(\d{2}))?`)

// resetDateRe matches the DATED shape a weekly cap prints: "resets Jul 28,
// 9pm (UTC)", "resets december 30, 11pm". The month name is what makes this
// distinct from resetClockRe — which requires a digit right after "resets"
// and so matches none of these.
var resetDateRe = regexp.MustCompile(`reset[s]?(?: at)?\s+([a-z]{3,9})\.?\s+(\d{1,2})\s*,?\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)

// resetClockRe matches the clock-time reset hints observed in forfait
// notices: "resets 3pm", "resets 10:30am (UTC)", "reset at 7pm".
var resetClockRe = regexp.MustCompile(`reset[s]?(?: at)?\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)

// resetWindowRe matches the window-duration shape: "reached for 5 hour".
var resetWindowRe = regexp.MustCompile(`for\s+(\d{1,2})\s*hour`)

// monthNames maps the three-letter prefix of a month name to its number,
// which covers both the abbreviated ("Jul") and full ("december") forms
// the CLI has been observed to print.
var monthNames = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

// resetDateTrustWindow bounds how far a year-inferred reset instant may
// sit from now to be believed. Every real window (5h, session, daily,
// weekly) resets within days, so a candidate months away means the text
// was not a reset hint after all — better to report "no hint" and let the
// caller fall back to a bounded wait than to return a confidently wrong
// instant it would then wait on.
const resetDateTrustWindow = 45 * 24 * time.Hour

// ParseResetHint extracts the reset instant from a provider usage-window
// notice, reporting whether anything parsed. Exported so a host that only
// has the flattened error text (rather than the typed *ErrRateLimited) can
// still recover the instant instead of re-implementing the patterns.
func ParseResetHint(text string, now time.Time) (time.Time, bool) {
	at := parseResetHint(strings.ToLower(text), now)
	return at, !at.IsZero()
}

// parseResetHint extracts the next reset instant from a usage-window
// notice, interpreting bare clock times as the NEXT occurrence (UTC —
// the forfait notices print UTC). Zero when nothing parses.
func parseResetHint(lower string, now time.Time) time.Time {
	nowUTC := now.UTC()
	// An absolute instant is unambiguous: take it verbatim, with no
	// plausibility window. A value already in the past is a legitimate
	// answer (the window has reopened) and the caller floors it.
	if m := resetAbsRe.FindStringSubmatch(lower); m != nil {
		year, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		hour, _ := strconv.Atoi(m[4])
		minute, _ := strconv.Atoi(m[5])
		second := 0
		if m[6] != "" {
			second, _ = strconv.Atoi(m[6])
		}
		if month >= 1 && month <= 12 && day >= 1 && day <= 31 && hour <= 23 && minute <= 59 && second <= 59 {
			return time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC)
		}
		return time.Time{}
	}
	if m := resetDateRe.FindStringSubmatch(lower); m != nil {
		if month, ok := monthNames[m[1][:3]]; ok {
			day, _ := strconv.Atoi(m[2])
			hour, minute, ok := clockTo24h(m[3], m[4], m[5])
			if ok && day >= 1 && day <= 31 {
				return inferResetYear(month, day, hour, minute, nowUTC)
			}
			return time.Time{}
		}
		// A leading word that is not a month means this is not the dated
		// shape; fall through to the bare-clock pattern below.
	}
	if m := resetClockRe.FindStringSubmatch(lower); m != nil {
		hour, minute, ok := clockTo24h(m[1], m[2], m[3])
		if !ok {
			return time.Time{}
		}
		at := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), hour, minute, 0, 0, time.UTC)
		if !at.After(nowUTC) {
			at = at.Add(24 * time.Hour)
		}
		return at
	}
	if m := resetWindowRe.FindStringSubmatch(lower); m != nil {
		hours, _ := strconv.Atoi(m[1])
		if hours > 0 {
			return nowUTC.Add(time.Duration(hours) * time.Hour)
		}
	}
	return time.Time{}
}

// clockTo24h converts a matched clock triple (hour, optional minute,
// optional am/pm marker) to 24-hour values, reporting whether the result
// is a valid time of day.
func clockTo24h(rawHour, rawMinute, meridiem string) (hour, minute int, ok bool) {
	hour, _ = strconv.Atoi(rawHour)
	if rawMinute != "" {
		minute, _ = strconv.Atoi(rawMinute)
	}
	switch meridiem {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}
	if hour > 23 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

// inferResetYear resolves a month/day/clock with no year against now,
// picking the candidate year that lands closest to now (so a late-December
// notice naming January rolls forward, and an early-January notice naming
// December rolls back). Returns zero when even the closest candidate falls
// outside resetDateTrustWindow.
func inferResetYear(month time.Month, day, hour, minute int, nowUTC time.Time) time.Time {
	var best time.Time
	bestDelta := time.Duration(math.MaxInt64)
	for _, year := range []int{nowUTC.Year() - 1, nowUTC.Year(), nowUTC.Year() + 1} {
		at := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
		// Reject a normalized date (e.g. Feb 31 → Mar 3).
		if at.Month() != month || at.Day() != day {
			continue
		}
		delta := at.Sub(nowUTC)
		if delta < 0 {
			delta = -delta
		}
		if delta < bestDelta {
			best, bestDelta = at, delta
		}
	}
	if bestDelta > resetDateTrustWindow {
		return time.Time{}
	}
	return best
}

// toolUseDetail extracts a short single-line summary from tool input for
// the log header. Multi-line commands are clipped at the first newline so
// the header stays on one log line; the full body is emitted separately
// via toolUseBody + LogBlock.
//
// Both helpers delegate to the shared pkg/backend/tooldisplay so the
// claude_code and claw paths render identical detail given identical
// input, and so the dispatch table for new tools lives in one place.
func toolUseDetail(name string, input map[string]any) string {
	raw, ok := marshalToolInput(input)
	if !ok {
		return ""
	}
	return tooldisplay.HeaderDetail(name, raw, tooldisplay.CamelCaseKeys)
}

// toolUseBody returns the full multi-line body to attach under the log
// header when the tool's input has content the operator typically wants
// to read whole. Empty for tools where the header already says it all.
func toolUseBody(name string, input map[string]any) string {
	raw, ok := marshalToolInput(input)
	if !ok {
		return ""
	}
	return tooldisplay.BlockBody(name, raw)
}

// marshalToolInput re-serializes a tool input map for the tooldisplay
// helpers, which work in JSON bytes (so they can be reused by paths
// that have already-marshalled input). Returns (nil, false) for nil or
// empty maps so callers skip the parse.
func marshalToolInput(input map[string]any) ([]byte, bool) {
	if len(input) == 0 {
		return nil, false
	}
	b, err := json.Marshal(input)
	if err != nil {
		return nil, false
	}
	return b, true
}
