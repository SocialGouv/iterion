package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/thinktokens"
	"github.com/SocialGouv/iterion/pkg/backend/tooldisplay"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// askUserMCPServerName is the name under which iterion registers itself as an
// MCP server exposing the ask_user tool. The CLI prefixes MCP tool names as
// "mcp__<server>__<tool>", so the LLM sees the tool as "mcp__iterion__ask_user".
const askUserMCPServerName = "iterion"

// askUserMCPToolName is the fully-qualified name of the ask_user tool as the
// CLI exposes it to the LLM.
const askUserMCPToolName = "mcp__iterion__ask_user"

// askUserMCPSubcommand is the hidden iterion subcommand that runs an MCP stdio
// server exposing only the ask_user tool. See cmd/iterion/mcp_ask_user.go.
const askUserMCPSubcommand = "__mcp-ask-user"

// defaultClaudeCodeModel is the model iterion forces on the claude_code
// backend when the workflow doesn't specify one. Mirrors the official
// Claude Code CLI default — Opus 4.8 (1M context window). Workflows can
// always override via the node's `model:` field — including the
// env-driven form `model: "${ITERION_CLAUDE_CODE_MODEL:-claude-opus-4-8}"`
// which the IR expander in pkg/backend/model/executor.go resolves
// before this backend ever sees the task. Operators who want to pin
// every claude_code node to a single gateway-side alias (e.g. GLM 5.1
// on z.ai) should put the env var in their .env and use the DSL form
// above in the bots that opt in.
const defaultClaudeCodeModel = "claude-opus-4-8"

// defaultMaxConsecutiveToolErrors aborts a claude_code session once this
// many tool results error in a row (any success resets the count). It
// guards against degenerate tool-error loops — a resumed/confused agent
// spinning out tool calls that all fail (e.g. a parallel batch cancelled
// by one bad relative-path call), which otherwise burns tokens until the
// run hits its cost/duration budget (observed: ~50 errors / 3 successes
// with zero progress). Override via ITERION_CLAUDE_CODE_MAX_TOOL_ERRORS
// (0 disables the guard).
const defaultMaxConsecutiveToolErrors = 25

// editMissHintAfter is how many consecutive Edit/MultiEdit "String to
// replace not found in file" failures trigger a re-Read hint injection
// (see the PostToolUse Edit-resilience hook). The model otherwise tends to
// blind-retry a mismatching old_string until defaultMaxConsecutiveToolErrors
// aborts the whole session — and the runtime's recovery re-runs the node
// straight back into the same wedge. Small enough to break the loop early;
// >1 so a single self-correcting miss isn't nagged.
const editMissHintAfter = 2

// editMissCount updates the running tally of consecutive Edit/MultiEdit
// "String to replace not found" failures given the latest tool call:
//   - a non-Edit tool leaves the tally unchanged (a Read between two
//     misses is the model trying to recover — it shouldn't reset the
//     wedge signal),
//   - an Edit/MultiEdit whose response carries the not-found error bumps
//     the tally,
//   - any other Edit/MultiEdit result (success, or a different error)
//     resets it to 0.
//
// Extracted from the PostToolUse hook so the wedge-detection is unit-
// testable without driving a live claude session.
func editMissCount(toolName, response string, prev int) int {
	if toolName != "Edit" && toolName != "MultiEdit" {
		return prev
	}
	if strings.Contains(response, "to replace not found") {
		return prev + 1
	}
	return 0
}

// installEditMissResilience appends the PostToolUse hook that breaks the
// Edit/MultiEdit blind-retry wedge: claude_code's Edit fails with "String
// to replace not found in file" when old_string doesn't match the file
// verbatim (a stale read or whitespace drift). The model tends to
// blind-retry a mismatching edit until defaultMaxConsecutiveToolErrors
// aborts the session — and recovery re-runs the node into the same wedge
// (observed: a feature_dev act burned 4 recovery attempts integrating into
// existing server files). After editMissHintAfter consecutive Edit-misses,
// inject a corrective system message so the model re-Reads the verbatim
// current text before editing. editMisses is closure-local: a session's
// tool calls are sequential, so no synchronisation is needed. Counts misses
// across intervening non-Edit tools (a Read between two misses doesn't reset
// — the model still hasn't landed the edit); resets only on a successful Edit.
func (b *ClaudeCodeBackend) installEditMissResilience(opts []claudesdk.Option, task Task) []claudesdk.Option {
	editMisses := 0
	return append(opts, claudesdk.WithHook(claudesdk.HookPostToolUse, claudesdk.HookMatcher{
		Handler: func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			editMisses = editMissCount(in.ToolName, fmt.Sprintf("%v", in.ToolResponse), editMisses)
			if editMisses < editMissHintAfter {
				return claudesdk.HookOutput{}, nil
			}
			b.Logger.Info("[%s#%d/claude-code] 🩹 %d consecutive Edit-misses — injecting re-Read hint", task.NodeID, task.Iteration, editMisses)
			hint := "Your Edit/MultiEdit failed: \"String to replace not found in file\". " +
				"The old_string does not match the file's CURRENT content verbatim (usually a whitespace or stale-read mismatch). " +
				"Do NOT retry the same edit. First Read the exact lines you intend to change to capture their verbatim current text (including leading whitespace), then issue the edit with that exact old_string. " +
				"If edits keep failing on a file, Read the whole surrounding region before editing again."
			return claudesdk.HookOutput{SystemMessage: hint, AdditionalContext: hint}, nil
		},
	}))
}

func resolveMaxConsecutiveToolErrors() int {
	if v := os.Getenv("ITERION_CLAUDE_CODE_MAX_TOOL_ERRORS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultMaxConsecutiveToolErrors
}

// settingSourcesFromEnv returns the CLI --setting-sources for claude_code
// nodes. Default "user,project": load the operator's user-level CLAUDE.md /
// settings.json and the target repo's project CLAUDE.md / .claude/settings.json
// so the agent honours the same conventions native Claude Code would — a core
// part of closing the adaptivity gap. Override via
// ITERION_CLAUDE_CODE_SETTING_SOURCES (comma-separated user/project/local);
// "" or "none" disables it, restoring the CLI's headless no-settings default.
// "local" is omitted from the default: .claude/settings.local.json is
// machine-specific and may carry absolute paths that don't resolve in a sandbox.
func settingSourcesFromEnv() []claudesdk.SettingSource {
	raw, ok := os.LookupEnv("ITERION_CLAUDE_CODE_SETTING_SOURCES")
	if !ok {
		raw = "user,project"
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "none") {
		return nil
	}
	var out []claudesdk.SettingSource
	for _, part := range strings.Split(raw, ",") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "user":
			out = append(out, claudesdk.SettingSourceUser)
		case "project":
			out = append(out, claudesdk.SettingSourceProject)
		case "local":
			out = append(out, claudesdk.SettingSourceLocal)
		}
	}
	return out
}

// defaultClaudeCodeEffort is the reasoning effort iterion forces on the
// claude_code backend when the workflow doesn't specify one. The bare API
// default on Opus 4.8 is "high", but the claude_code backend runs
// implementers/fixers — coding and agentic work — for which Anthropic
// recommends starting at "xhigh" (platform.claude.com/docs/en/build-with-claude/effort).
// Workflows can always override via `reasoning_effort:`.
const defaultClaudeCodeEffort = "xhigh"

// ClaudeCodeBackend delegates work to the `claude` CLI (claude-code)
// via the Claude Agent SDK.
type ClaudeCodeBackend struct {
	// Command overrides the CLI binary path (default: "claude").
	Command string
	// Logger is the leveled logger for diagnostic output.
	Logger *iterlog.Logger
}

// Execute runs the claude CLI with the given task using the Claude Agent SDK.
// buildTransportOptions assembles the base claudesdk options for a claude_code
// run — system prompt, setting sources, cwd, CLI path, permission mode, model,
// sandbox command builder, reasoning effort, and max-turns. Split out of the
// long Execute method; this prefix carries no post-session state (the
// closure-capturing hooks — stderr/ask_user/secret/board/inbox — stay in
// Execute).
func (b *ClaudeCodeBackend) buildTransportOptions(task Task) []claudesdk.Option {
	var opts []claudesdk.Option

	// APPEND, do not REPLACE. --system-prompt would discard Claude Code's
	// native agentic system prompt (tool-use discipline, plan-before-act,
	// read-before-edit, parallel-tool reflex, file:line conventions, refusal
	// posture) and leave the model with only the recipe's task text — the root
	// cause of iterion-via-Claude-Code being less adaptive than native Claude
	// Code. --append-system-prompt keeps the native prompt as the base and adds
	// the workflow's instructions on top. Task.SystemPromptMode is
	// SystemPromptAppendToNative for this backend, so BuildSystemPrompt emits
	// author + suffixes only (no iterion-authored base — the native prompt is it).
	systemPrompt := task.BuildSystemPrompt()
	if systemPrompt != "" {
		opts = append(opts, claudesdk.WithAppendSystemPrompt(systemPrompt))
	}
	// Load the operator's settings sources so the agent behaves like native
	// Claude Code in the target repo: user-level (~/.claude/CLAUDE.md +
	// settings.json) and project-level (the repo's CLAUDE.md + .claude/
	// settings.json). --append-system-prompt alone does not re-enable settings
	// discovery in --print mode; --setting-sources does. Honours the same paths
	// in a sandbox (the workspace and ~/.claude are bind-mounted at their host
	// absolute paths). Tunable/disable-able via ITERION_CLAUDE_CODE_SETTING_SOURCES.
	if srcs := settingSourcesFromEnv(); len(srcs) > 0 {
		opts = append(opts, claudesdk.WithSettingSources(srcs...))
	}
	// Cwd handling differs by sandbox state. On the host (no sandbox)
	// we pass the workdir straight through to claudesdk → cmd.Dir.
	// In the sandbox it's the host worktree path that doesn't exist
	// inside the container — the docker driver's Command falls back
	// to the spec's WorkspaceFolder (the bind-mount target) when
	// Cwd is empty, which is the path we actually want.
	if task.WorkDir != "" && task.Sandbox == nil {
		opts = append(opts, claudesdk.WithCwd(task.WorkDir))
	}
	// Same lifetime trade-off for the CLI binary path: the SDK's
	// default exec.LookPath("claude") runs on the host and returns
	// the operator's host path (e.g. /home/jo/.local/bin/claude).
	// Forwarded into a `docker exec` invocation that path doesn't
	// exist inside the container, and claude exits silently with
	// "session ended without result message" upstream. Pin to the
	// bare name so the in-container PATH lookup wins.
	if task.Sandbox != nil {
		opts = append(opts, claudesdk.WithCLIPath("claude"))
	}
	// Bypass interactive permission prompts: the runtime enforces safety via
	// workspace isolation and allowed-tool lists, so the delegate subprocess
	// does not need its own permission gate.
	opts = append(opts, claudesdk.WithPermissionMode("bypassPermissions"))

	// The CLI requires --verbose when using --output-format=stream-json in
	// --print mode. The SDK always uses stream-json, so we must enable verbose.
	opts = append(opts, claudesdk.WithVerbose(true))

	// Stderr forwarding is registered once, further down (the
	// stderrBuf-capturing callback): WithStderrCallback assigns (not
	// appends) the SDK's single callback slot, so a logger-only
	// registration here would simply be overwritten by that later,
	// richer one (live Info logging + buffered capture for diagnostics).

	model := task.Model
	if model == "" {
		model = defaultClaudeCodeModel
	}
	opts = append(opts, claudesdk.WithModel(model))

	if b.Command != "" {
		opts = append(opts, claudesdk.WithCLIPath(b.Command))
	}

	// When the run is sandboxed, route the claude CLI subprocess
	// through the sandbox driver so the agent's bash/edit tools
	// execute inside the container, not on the host. Cwd/Env are
	// passed via the runtime-native channels (e.g. `docker exec
	// --workdir / --env`); the SDK disables its own cmd.Dir / cmd.Env
	// application when a builder is set.
	if task.Sandbox != nil {
		run := task.Sandbox
		opts = append(opts, claudesdk.WithCommandBuilder(func(ctx context.Context, path string, args []string, cwd string, env map[string]string, openStdin bool) *exec.Cmd {
			// Surface the resolved CLI invocation so failures like
			// "session ended without result" can be traced back to a
			// concrete `docker exec` command. Without this every silent
			// claude exit is opaque even with stderr capture. (Logger
			// methods are nil-safe — no guard needed.)
			preview := append([]string{path}, args...)
			b.Logger.Info("claude-code: exec %v (cwd=%s, env_keys=%d, stdin=%v)", preview, cwd, len(env), openStdin)
			// KeepStdinOpen mirrors the SDK's OpenStdin flag so the docker
			// driver adds `--interactive` to docker exec. Without this,
			// Session-mode (NDJSON over stdin) silently fails: the SDK
			// later wires cmd.StdinPipe() but docker has already closed
			// stdin on the child, claude reads EOF, and exits 0 with no
			// output — matching the cli_exit_code=0 silent-failure path.
			return run.Command(ctx, append([]string{path}, args...), sandbox.ExecOpts{
				WorkDir:       cwd,
				Env:           env,
				KeepStdinOpen: openStdin,
			})
		}))
	}

	effort := task.ReasoningEffort
	if effort == "" {
		effort = defaultClaudeCodeEffort
	}
	opts = append(opts, claudesdk.WithEnv("CLAUDE_CODE_EFFORT_LEVEL", effort))

	// tool_max_steps caps agentic tool-use iterations. Until now this
	// field was defined in delegate.Task but never wired into the CLI,
	// so recipe authors who set `tool_max_steps: 25` got silent infinity
	// — observed with GLM running discover_outdated through 60+ tool
	// calls instead of stopping at 25. Map it to claude's --max-turns
	// (the closest semantic: one turn = one assistant message exchange,
	// which usually contains one tool call + response).
	if task.ToolMaxSteps > 0 {
		opts = append(opts, claudesdk.WithMaxTurns(task.ToolMaxSteps))
	}

	return opts
}

func (b *ClaudeCodeBackend) Execute(ctx context.Context, task Task) (result Result, err error) {
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
	}
	// Fire OnTurnFinished once on the way out, when the runtime wired
	// the hook and the delegate produced a SessionID. Wrapped in a
	// defer so every successful return path (Pass 1, recovery, two-
	// pass, ask_user escalation) flows through the same notification —
	// avoiding the maintenance trap of remembering to call it before
	// every `return result, ...`. Skipped on hard errors with no
	// captured session (rm.SessionID empty).
	defer func() {
		if task.Hooks.OnTurnFinished == nil {
			return
		}
		if result.SessionID == "" {
			return
		}
		text := ""
		if s := result.Output["_assistant_text"]; s != nil {
			text, _ = s.(string)
		}
		task.Hooks.OnTurnFinished(TurnFinishedInfo{
			SessionID:    result.SessionID,
			FinishReason: "", // claude_code SDK doesn't surface a granular reason at Result level
			Text:         text,
			// Token totals come from Result.Tokens (in+out) but the
			// claude_code path doesn't split them apart — the hooks
			// layer logs the total under InputTokens for now; a future
			// refinement would track input/output split through the
			// stream parser.
			InputTokens: result.Tokens,
		})
	}()

	opts := b.buildTransportOptions(task)
	// Allowed-tools registration is deferred to a single call near the end
	// of this function. WithAllowedTools APPENDS to the SDK's slice, so
	// registering the base set here and again below (combined with MCP
	// extras) would list every base tool twice. We accumulate the MCP
	// extras (ask_user, board.*) into extraAllowedTools and emit one call.
	var extraAllowedTools []string

	// Inject Anthropic-flavoured credentials and resolve session resume/fork
	// (see helper). The returned fingerprint is recorded on the Result so a
	// later resume can detect a credential change.
	opts, currentFingerprint := b.setupCredsAndSession(ctx, task, opts)

	// Structured output handling. claude CLI >= 2.1 accepts --json-schema
	// (WithOutputFormat) TOGETHER with --allowedTools in a single pass: the
	// agent does its tool work and then calls the native StructuredOutput
	// tool, which populates result.structured_output. So we always set
	// WithOutputFormat when a schema is present, even WITH tools. The
	// `needsTwoPass` flag no longer gates whether structured output is
	// requested — it gates only the Pass-2 FALLBACK (resume with no tools to
	// extract the schema) used when Pass 1 returns no structured output
	// (e.g. the agent hit --max-turns before calling StructuredOutput, or a
	// sandbox edge case). Setting the schema in Pass 1 also stops the agent
	// from reaching for an unregistered StructuredOutput tool and logging a
	// spurious "No such tool available: StructuredOutput" error. Empirically
	// the agent still completes its tool work BEFORE finalizing (verified
	// against claude 2.1.177), so this does not make it rush its output.
	prompt := task.UserPrompt
	needsTwoPass := len(task.OutputSchema) > 0 && len(task.AllowedTools) > 0
	if len(task.OutputSchema) > 0 {
		var schema map[string]any
		if json.Unmarshal(task.OutputSchema, &schema) == nil {
			opts = append(opts, claudesdk.WithOutputFormat(schema))
		}
	}

	// Capture stderr for post-session diagnostics AND surface every
	// line live so the user can see what the CLI is doing during long
	// reasoning intervals. Without live stderr, the SDK is a black box
	// while it streams thinking tokens or reads files: the runtime
	// emits nothing between "Delegation started" and the final
	// AssistantMessage, which can be many minutes for Opus xhigh/max.
	var stderrBuf strings.Builder
	opts = append(opts, claudesdk.WithStderrCallback(func(line string) {
		stderrBuf.WriteString(line)
		stderrBuf.WriteString("\n")
		if line != "" {
			b.Logger.Info("[%s#%d/claude-code:err] %s", task.NodeID, task.Iteration, line)
		}
	}))

	// Native ask_user interception (see wireAskUserHook). streamCtx is the
	// session context; cancelStream short-circuits the stream when the LLM
	// calls ask_user, and pendingQuestion carries the captured question to
	// the post-session escalation check below. The system prompt's
	// [INTERACTION PROTOCOL] suffix is preserved so the JSON-output fallback
	// still works (and is the only path when sandboxed).
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	var pendingQuestion atomic.Value   // string
	var pendingPermission atomic.Value // map[string]any (permission marker)
	opts = b.wireAskUserHook(task, opts, &extraAllowedTools, &pendingQuestion, cancelStream)

	// Tool-permission gate (anti-prompt-injection boundary). Shares the
	// pendingQuestion/cancelStream pause path with ask_user so an Ask
	// decision surfaces to the human exactly like a clarifying question;
	// pendingPermission additionally carries the structured marker so the
	// pause becomes an approval card and the runtime can auto-grant on
	// resume.
	opts = b.wirePermissionHook(task, opts, &pendingQuestion, &pendingPermission, cancelStream)

	// Secret materialisation, rtk command compression, and board MCP wiring —
	// each a self-contained set of opts hooks/servers (see helpers). Board
	// and ask_user extend extraAllowedTools; the single registration below
	// emits one WithAllowedTools call.
	opts = installMaterializeSecretsHook(task, opts)
	opts = installRewriteHook(task, opts)
	opts = b.wireBoardMCP(task, opts, &extraAllowedTools)

	// Watch capabilities (watch.subscribe / watch.unsubscribe) are wired for
	// the claw backend only so far — the claude_code stdio (__mcp-watch) and
	// sandbox HTTP transports are not built yet (board's own rollout was
	// stdio-then-HTTP across phases; watch is at the claw-only phase). Warn
	// so the gap is visible instead of the bot calling a tool that isn't
	// there mid-loop.
	if HasWatchCapability(task.Capabilities) {
		b.Logger.Warn("[%s#%d/claude-code] watch.* capabilities are not yet supported on the claude_code backend (claw only); ignoring for this node", task.NodeID, task.Iteration)
	}

	// Single allowed-tools registration: the node's restrictive base list
	// plus any MCP extras accumulated above (ask_user, board.*), built once
	// so no tool is listed twice (WithAllowedTools appends). An empty base
	// list means "no restriction", so we register nothing in that case —
	// matching the per-block guards that only extended the allowlist when
	// task.AllowedTools was non-empty.
	if len(task.AllowedTools) > 0 {
		combined := append([]string(nil), task.AllowedTools...)
		combined = append(combined, extraAllowedTools...)
		opts = append(opts, claudesdk.WithAllowedTools(combined...))
	}

	// Operator-chatbox mid-session inbox delivery (see helper) and
	// Edit-miss resilience (PostToolUse) — breaks the Edit/MultiEdit
	// blind-retry wedge; see installEditMissResilience for the rationale.
	opts = b.installInboxDrainHooks(task, opts)
	opts = b.installEditMissResilience(opts, task)

	startTime := time.Now()
	rm, sessMeta, streamErr := b.runSession(streamCtx, prompt, task, opts)
	duration := time.Since(startTime)

	// Native ask_user capture takes precedence over any error: if the hook
	// fired, the resulting context cancellation surfaces here as ctx.Err(),
	// which we must not treat as a failure.
	if q, ok := pendingQuestion.Load().(string); ok && q != "" {
		marker, _ := pendingPermission.Load().(map[string]any)
		return b.buildAskUserPendingResult(task, q, marker, rm, sessMeta, currentFingerprint, duration, stderrBuf.String()), nil
	}

	if streamErr != nil {
		return b.buildStreamErrorResult(rm, sessMeta, streamErr, stderrBuf.String(), duration, task)
	}

	result = Result{
		Duration:           duration,
		ExitCode:           0,
		Stderr:             stderrBuf.String(),
		BackendName:        BackendClaudeCode,
		SessionID:          rm.SessionID,
		SessionFingerprint: currentFingerprint,
	}
	applyClaudeCodeSessionMeta(&result, rm, sessMeta)

	var totalIn, totalOut int
	if rm.Usage != nil {
		totalIn += rm.Usage.InputTokens
		totalOut += rm.Usage.OutputTokens
	}
	result.Tokens = totalIn + totalOut

	if rm.IsError && rm.Subtype != claudesdk.ResultSuccess {
		if errResult, errOut, fatal := b.handleCLIErrorSubtype(rm, task, result); fatal {
			return errResult, errOut
		}
	}

	// Overload/5xx guard. The claude CLI sometimes completes the stream
	// "successfully" (subtype=success, IsError=false) but renders an
	// unrecoverable upstream API failure AS the result text — e.g.
	// "API Error: 529 Overloaded". Left untouched, that string becomes the
	// node's output AND poisons any downstream session that inherits this
	// one (observed in a test-coverage dogfood: a 529 on the `plan` node
	// flowed a non-plan into `act`). Re-type it as ErrTransient so the
	// executor's retry loop rides the outage out — exactly as it does for a
	// connectivity drop surfaced on stderr (retypeNetworkError). Only
	// transient classes (429/5xx/overload/connectivity) retry; a 4xx
	// client/auth error falls through as the visible node output.
	if rm.Result != nil && isTransientAPIErrorResult(*rm.Result) {
		detail := strings.TrimSpace(*rm.Result)
		b.Logger.Warn("[%s#%d/claude-code] upstream API-error result text detected — flagging for retry: %.120s",
			task.NodeID, task.Iteration, detail)
		return result, &ErrTransient{Provider: BackendClaudeCode, Reason: "api_error_result", Detail: detail}
	}

	// Model-unavailable guard. An invalid/unauthorized `--model` does NOT fail
	// the stream (subtype=success, IsError=false): the claude CLI renders its
	// model-error sentence AS the result text (e.g. "There's an issue with the
	// selected model (openai/gpt-5.5). It may not exist or you may not have
	// access to it."). Left untouched that prose flows into the formatting
	// passes and finally surfaces as an opaque "missing required field" schema
	// error, masking the real cause. Fail fast with a legible error naming the
	// offending model. Non-transient (unlike the API-error guard above) — a
	// retry can't fix a bad/unauthorized model; the usual cause is a
	// claude_code node pinned to a non-Anthropic model (e.g. the shared
	// ITERION_SEC_AUDIT_BACKEND/MODEL override dragging detect_tech onto
	// openai/gpt-5.5).
	if rm.Result != nil && isModelUnavailableResult(*rm.Result) {
		detail := strings.TrimSpace(*rm.Result)
		b.Logger.Error("[%s#%d/claude-code] model %q unavailable to the CLI — failing fast: %.160s",
			task.NodeID, task.Iteration, task.Model, detail)
		return result, fmt.Errorf("claude-code: model %q is unavailable or unauthorized (check the node's backend/model — a claude_code node cannot run a non-Anthropic model): %s", task.Model, detail)
	}

	if needsTwoPass && rm.SessionID != "" {
		if handled, twoPassResult, twoPassErr := b.runTwoPassFormatting(ctx, task, rm, result, &totalIn, &totalOut); handled {
			return twoPassResult, twoPassErr
		}
	}

	// Single-pass path: parse Pass 1 directly.
	output, rawLen, fallback := parseSDKOutput(rm.Result, rm.StructuredOutput, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	// Safety net: schema declared but Pass 1 gave empty/fallback output —
	// try one recovery formatting pass via session resume (see helper).
	if (len(output) == 0 || fallback) && len(task.OutputSchema) > 0 && rm.SessionID != "" {
		b.runRecoveryFormatterPass(ctx, task, rm.SessionID, &result, &totalIn, &totalOut)
	}

	cost.Annotate(result.Output, task.Model, totalIn, totalOut)
	return result, nil
}

// buildAskUserPendingResult packages the Result returned when the native
// ask_user MCP hook fired mid-session: it short-circuits the stream and
// surfaces the captured question to the runtime via the
// `_needs_interaction` / `_interaction_questions` envelope so the engine
// can pause the run and elicit the operator. Extracted from Execute for
// readability; the per-field semantics (Duration, ExitCode=0, Stderr,
// SessionID-from-rm, SessionFingerprint) are identical to the original
// inline path.
func (b *ClaudeCodeBackend) buildAskUserPendingResult(task Task, q string, marker map[string]any, rm *claudesdk.ResultMessage, sessMeta sessionMeta, currentFingerprint string, duration time.Duration, stderr string) Result {
	if marker != nil {
		b.Logger.Info("[%s#%d/claude-code] 🔐 tool-permission approval escalated to the runtime", task.NodeID, task.Iteration)
	} else {
		b.Logger.Info("[%s#%d/claude-code] 🛑 ask_user escalated via native MCP tool", task.NodeID, task.Iteration)
	}
	sessID := ""
	if rm != nil {
		sessID = rm.SessionID
	}
	questions := map[string]interface{}{AskUserQuestionKey: q}
	if marker != nil {
		questions[permission.InteractionMarkerKey] = marker
	}
	askResult := Result{
		Output: map[string]interface{}{
			"_needs_interaction":     true,
			"_interaction_questions": questions,
		},
		Duration:           duration,
		ExitCode:           0,
		Stderr:             stderr,
		BackendName:        BackendClaudeCode,
		SessionID:          sessID,
		SessionFingerprint: currentFingerprint,
	}
	applyClaudeCodeSessionMeta(&askResult, rm, sessMeta)
	return askResult
}

// buildStreamErrorResult packages the Result + wrapped error returned when
// the claude session streaming failed. Network-shaped errors are re-typed
// to ErrTransient so the executor's retry loop rides connectivity blips
// out instead of failing the whole node. Extracted from Execute; behavior
// (ExitCode=-1, Stderr, BackendName, session-meta application, "delegate:
// claude-code failed: %w" wrapping) matches the original inline path.
func (b *ClaudeCodeBackend) buildStreamErrorResult(rm *claudesdk.ResultMessage, sessMeta sessionMeta, streamErr error, stderr string, duration time.Duration, task Task) (Result, error) {
	errResult := Result{
		Duration:    duration,
		ExitCode:    -1,
		Stderr:      stderr,
		BackendName: BackendClaudeCode,
	}
	applyClaudeCodeSessionMeta(&errResult, rm, sessMeta)
	// A connectivity drop during the API call surfaces as an opaque
	// "session ended without result" — the CLI exits non-zero and the
	// only network evidence (fetch failed / ECONNRESET / overloaded …)
	// lands on stderr. Re-type it as ErrTransient so the executor's
	// retry loop rides the blip out instead of failing the whole node.
	streamErr = b.retypeNetworkError(streamErr, stderr, task)
	return errResult, fmt.Errorf("delegate: claude-code failed: %w", streamErr)
}

// handleCLIErrorSubtype branches on the CLI's error subtype after a stream
// completed with rm.IsError=true. error_max_turns is a SOFT stop (the agent
// hit its tool_max_steps cap) — for an implementer (act/fix, no output
// schema) the work it did is already in the worktree, so we return the
// partial result and let the workflow continue; for a node with structured
// output (a judge), the partial result lacks the required fields and
// upstream schema validation fails it, which is the correct outcome. Other
// error subtypes (error_during_execution, error_max_budget_usd) remain
// hard failures. Returns `fatal=true` when the caller must return the
// (result, err) pair immediately; `fatal=false` lets Execute fall through.
func (b *ClaudeCodeBackend) handleCLIErrorSubtype(rm *claudesdk.ResultMessage, task Task, result Result) (Result, error, bool) {
	// error_max_turns is a SOFT stop, not a failure: the agent hit its
	// tool_max_steps cap (claude --max-turns). For an implementer
	// (act/fix, no output schema) the work it did is already in the
	// worktree, so return the partial result and let the workflow
	// continue — the review/fix loop completes any gaps. For a node
	// with structured output (a judge), the partial result lacks the
	// required fields and upstream schema validation fails it, which is
	// the correct outcome. Other error subtypes (error_during_execution,
	// error_max_budget_usd) remain hard failures.
	if rm.Subtype == claudesdk.ResultErrorMaxTurns {
		b.Logger.Warn("[%s#%d/claude-code] hit max turns (tool_max_steps) — returning partial result; downstream review/fix completes any gaps", task.NodeID, task.Iteration)
		return result, nil, false
	}
	return result, fmt.Errorf("delegate: claude-code error: subtype=%s", rm.Subtype), true
}

// runTwoPassFormatting runs the Pass-2 structured-output extraction loop
// when tools + schema are both present and Pass 1 produced a SessionID.
// Returns `handled=true` when an explicit return path was hit (and the
// caller must propagate result,err); `handled=false` lets Execute fall
// through to the single-pass parsing path. totalIn / totalOut are
// updated in place across formatting attempts so cost annotation at the
// caller sees the cumulative usage.
//
// Two-pass execution: when tools + schema are both present, Pass 1 now
// carries --json-schema (set above), so a well-behaved agent finishes its
// tool work and calls the native StructuredOutput tool, populating
// rm.StructuredOutput. The formatting pass below is therefore a FALLBACK,
// not the default: it runs only when Pass 1 returned no usable structured
// output. Both passes route through the sandbox command builder when
// sandboxed, so the resumed session is found inside the container where
// Pass 1 created it.
func (b *ClaudeCodeBackend) runTwoPassFormatting(ctx context.Context, task Task, rm *claudesdk.ResultMessage, result Result, totalIn, totalOut *int) (bool, Result, error) {
	// Fast path: Pass 1 already produced valid structured output. The
	// empty-map guard in parseSDKOutput rejects the `structured_output: {}`
	// a tool session emits when the agent never called StructuredOutput
	// (e.g. --max-turns), so a non-empty, non-fallback result here means
	// the schema was genuinely satisfied in one pass — skip Pass 2.
	if output, rawLen, fallback := parseSDKOutput(rm.Result, rm.StructuredOutput, task.OutputSchema); len(output) > 0 && !fallback {
		result.Output = output
		result.RawOutputLen = rawLen
		result.ParseFallback = false
		cost.Annotate(result.Output, task.Model, *totalIn, *totalOut)
		return true, result, nil
	}
	const maxFmtAttempts = 2
	var lastFmtErr error
	for attempt := 1; attempt <= maxFmtAttempts; attempt++ {
		b.Logger.Debug("claude-code [formatting pass %d/%d] starting structured output extraction (session=%s)", attempt, maxFmtAttempts, rm.SessionID)
		fmtRM, fmtErr := b.formatOutput(ctx, task, rm.SessionID)
		if fmtErr != nil {
			lastFmtErr = fmtErr
			if attempt < maxFmtAttempts {
				b.Logger.Warn("claude-code [formatting pass %d/%d] failed, retrying: %v", attempt, maxFmtAttempts, fmtErr)
				continue
			}
			// Both attempts exhausted. Before failing the whole delegation,
			// try parsing Pass 1's free-form output: agents typically emit
			// a fenced ```json block matching the schema as their final
			// message, and parseSDKOutput already extracts that. This
			// recovers from the common infra failure where the sandbox
			// container dies mid-formatting (observed: container SIGKILL
			// at formatting-pass invocation → claude exits 137 →
			// "container is not running" on retry → whole delegation
			// fails despite Pass 1 having produced shippable output).
			output, rawLen, fallback := parseSDKOutput(rm.Result, rm.StructuredOutput, task.OutputSchema)
			if len(output) > 0 && !fallback {
				b.Logger.Warn("claude-code [formatting pass] failed (%v); recovered structured output from Pass 1 free-form result", fmtErr)
				result.Output = output
				result.RawOutputLen = rawLen
				result.ParseFallback = false
				cost.Annotate(result.Output, task.Model, *totalIn, *totalOut)
				return true, result, nil
			}
			return true, result, fmt.Errorf("delegate: claude-code formatting pass failed: %w", fmtErr)
		}
		if fmtRM.Usage != nil {
			*totalIn += fmtRM.Usage.InputTokens
			*totalOut += fmtRM.Usage.OutputTokens
			result.Tokens = *totalIn + *totalOut
		}
		result.FormattingPassUsed = true

		output, rawLen, fallback := parseSDKOutput(fmtRM.Result, fmtRM.StructuredOutput, task.OutputSchema)
		if fallback && attempt < maxFmtAttempts {
			b.Logger.Warn("claude-code [formatting pass %d/%d] produced fallback text, retrying", attempt, maxFmtAttempts)
			continue
		}
		result.Output = output
		result.RawOutputLen = rawLen
		result.ParseFallback = fallback
		cost.Annotate(result.Output, task.Model, *totalIn, *totalOut)
		return true, result, nil
	}
	// Defensive: loop fell through without returning. Shouldn't happen
	// (every iteration either returns or continues), but if it did,
	// surface the last formatting error rather than a generic one.
	if lastFmtErr != nil {
		return true, result, fmt.Errorf("delegate: claude-code formatting pass failed: %w", lastFmtErr)
	}
	return false, result, nil
}

// setupCredsAndSession injects Anthropic-flavoured credentials into the CLI
// subprocess (single helper so Pass 1 and Pass 2 stay symmetric) and, when
// the task carries a SessionID, decides whether to resume/fork that session
// or drop it on a provider-fingerprint mismatch. Returns the extended opts
// and the current provider fingerprint.
func (b *ClaudeCodeBackend) setupCredsAndSession(ctx context.Context, task Task, opts []claudesdk.Option) ([]claudesdk.Option, string) {
	credEnv := anthropicCredEnvForCLI(ctx, task.ProviderHint)
	opts = append(opts, credEnvToOpts(credEnv)...)
	currentFingerprint := providerFingerprint(credEnv)

	if task.SessionID != "" {
		drop, reason := shouldDropSessionFork(task, currentFingerprint)
		if drop {
			b.Logger.Warn("[%s#%d/claude-code] dropping session fork: %s",
				task.NodeID, task.Iteration, reason)
		} else {
			opts = append(opts, claudesdk.WithResume(task.SessionID))
			if task.ForkSession {
				opts = append(opts, claudesdk.WithForkSession(true))
			}
		}
	}
	return opts, currentFingerprint
}

// wireAskUserHook registers iterion's native ask_user MCP server and a
// PreToolUse hook that captures the question and cancels the stream the
// moment the LLM calls ask_user (mirrors the claw backend's in-process
// path). Disabled when sandboxed: the stdio MCP server's host binary path
// (os.Executable) and the host /tmp --mcp-config are both invisible inside
// the container, so claude would reject the missing config and exit before
// producing a result — the [INTERACTION PROTOCOL] JSON fallback covers that
// case. Stores the captured question into pendingQuestion and extends extras
// with the ask_user tool name only when the node already restricts its
// toolset (an empty AllowedTools means "no restriction").
func (b *ClaudeCodeBackend) wireAskUserHook(task Task, opts []claudesdk.Option, extras *[]string, pendingQuestion *atomic.Value, cancelStream context.CancelFunc) []claudesdk.Option {
	if !task.InteractionEnabled || task.Sandbox != nil {
		return opts
	}
	selfPath := proc.LocateIterionBinary()
	if selfPath == "" {
		b.Logger.Warn("[%s#%d/claude-code] could not resolve iterion CLI binary path; native ask_user MCP server disabled (falling back to JSON _needs_interaction protocol)", task.NodeID, task.Iteration)
		return opts
	}
	opts = append(opts, claudesdk.WithMCPServer(askUserMCPServerName, &claudesdk.MCPStdioServer{
		Command: selfPath,
		Args:    []string{askUserMCPSubcommand},
	}))
	if len(task.AllowedTools) > 0 {
		*extras = append(*extras, askUserMCPToolName)
	}
	matcher := "^" + askUserMCPToolName + "$"
	noContinue := false
	opts = append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Matcher: &matcher,
		Handler: func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			if q, ok := in.ToolInput["question"].(string); ok && q != "" {
				pendingQuestion.Store(q)
				cancelStream()
			}
			return claudesdk.HookOutput{
				Decision:      "deny",
				Continue:      &noContinue,
				SystemMessage: "ask_user has been escalated to the iterion runtime; stop generating.",
			}, nil
		},
	}))
	return opts
}

// wirePermissionHook installs the tool-permission gate for claude_code:
// a broad PreToolUse hook (Matcher nil = every tool) that evaluates the
// resolved policy before the CLI runs a tool. This is claude_code's half
// of cross-backend parity with claw's executeToolsDirect gate — both
// honour the SAME permission.Policy.
//
// Under the always-on --permission-mode bypassPermissions, PreToolUse
// hooks STILL run and a "deny" decision STILL blocks the tool (per the
// Agent SDK permission-evaluation order: hooks run first). So the gate
// needs no --permission-mode change:
//   - Allow → empty HookOutput → falls through → bypass approves.
//   - Deny  → permissionDecision "deny" with a reason the model adapts to.
//   - Ask   → capture the approval prompt + cancel the stream so the run
//     PAUSES for the human (reuses the ask_user pause path:
//     pendingQuestion + buildAskUserPendingResult). On resume the operator
//     grants and the model re-issues the now-authorized call.
//
// Infrastructure tools (ask_user, board.*) are exempt inside
// permission.Policy.Evaluate, so this hook never blocks iterion's own
// interaction plumbing.
func (b *ClaudeCodeBackend) wirePermissionHook(task Task, opts []claudesdk.Option, pendingQuestion, pendingPermission *atomic.Value, cancelStream context.CancelFunc) []claudesdk.Option {
	policy := task.Permission
	if !policy.Enabled() {
		return opts
	}
	noContinue := false
	return append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Handler: func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			dec, rule := policy.Evaluate(in.ToolName, in.ToolInput)
			switch dec {
			case permission.Deny:
				return claudesdk.HookOutput{
					Decision:       "deny",
					DecisionReason: permission.DenyMessage(in.ToolName, in.ToolInput, rule),
				}, nil
			case permission.Ask:
				// Surface the approval request to the human and stop the
				// stream — the post-session check reuses the ask_user
				// pending path to pause the run. The marker carries the
				// structured request so the runtime can auto-grant on resume.
				pendingQuestion.Store(permission.AskPrompt(in.ToolName, in.ToolInput, rule))
				pendingPermission.Store(permission.Marker(in.ToolName, in.ToolInput, rule))
				cancelStream()
				return claudesdk.HookOutput{
					Decision:      "deny",
					Continue:      &noContinue,
					SystemMessage: "This action requires operator approval; it has been escalated to the iterion runtime. Stop generating.",
				}, nil
			default: // permission.Allow
				return claudesdk.HookOutput{}, nil
			}
		},
	}))
}

// installMaterializeSecretsHook adds a PreToolUse hook that swaps
// __ITERION_SECRET_<name>__ placeholders for their real values in
// agent-emitted tool input, immediately before the CLI runs the tool
// (Layer 1, structural). The placeholder is all the model ever emits/sees;
// the real value is spliced in here and never enters the prompt, the event
// stream, or the run store. Matches all tools (Matcher nil); a no-op for
// input that carries no placeholder.
func installMaterializeSecretsHook(task Task, opts []claudesdk.Option) []claudesdk.Option {
	materialize := task.MaterializeSecrets
	if materialize == nil {
		return opts
	}
	return append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Handler: func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			if len(in.ToolInput) == 0 {
				return claudesdk.HookOutput{}, nil
			}
			raw, err := json.Marshal(in.ToolInput)
			if err != nil {
				return claudesdk.HookOutput{}, nil
			}
			swapped := materialize(string(raw))
			if swapped == string(raw) {
				return claudesdk.HookOutput{}, nil // no placeholder present
			}
			var updated map[string]any
			if err := json.Unmarshal([]byte(swapped), &updated); err != nil {
				return claudesdk.HookOutput{}, nil
			}
			return claudesdk.HookOutput{Decision: "allow", UpdatedInput: updated}, nil
		},
	}))
}

// installRewriteHook adds a PreToolUse hook on the Bash tool that rewrites
// commands to their compressed equivalent (e.g. "git status" → "rtk git
// status"), saving 60–90% of output tokens, when compression is enabled for
// this node and at least one rewriter plugin's binary is present. The rewrite
// decision is delegated to each rewriter's own contract (single source of
// truth); iterion uses rewriters purely as compressors — never a permission
// gate — so it always auto-allows the rewritten command. The rewrite runs
// host-side; the (sandboxed) CLI runs the rewritten command in-container
// against the bind-mounted rewriter binary.
func installRewriteHook(task Task, opts []claudesdk.Option) []claudesdk.Option {
	mode := rewrite.ParseMode(task.CompressMode)
	chain := rewrite.NewChain(task.Rewriters)
	if !mode.Enabled() || !chain.Available() {
		return opts
	}
	bashMatcher := "^Bash$"
	return append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Matcher: &bashMatcher,
		Handler: func(hookCtx context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			updated, changed := chain.RewriteCommandField(hookCtx, mode, in.ToolInput)
			if !changed {
				return claudesdk.HookOutput{}, nil
			}
			return claudesdk.HookOutput{
				Decision:       "allow",
				DecisionReason: "compress auto-rewrite",
				UpdatedInput:   updated,
			}, nil
		},
	}))
}

// wireBoardMCP registers the internal __mcp-board MCP server when the node
// holds any board.* capability, so the bot can mutate the kanban from inside
// its reasoning loop. Non-sandboxed runs use the stdio transport (iterion
// binary subcommand); sandboxed runs use the HTTP transport when the runtime
// configured BoardHTTPEndpoint+BoardRunToken (else the capability is disabled
// with a warning). Extends extras with the granted board tool names when the
// node already restricts its toolset.
func (b *ClaudeCodeBackend) wireBoardMCP(task Task, opts []claudesdk.Option, extras *[]string) []claudesdk.Option {
	if !HasBoardCapability(task.Capabilities) {
		return opts
	}
	if task.Sandbox == nil {
		selfPath := proc.LocateIterionBinary()
		if selfPath == "" {
			b.Logger.Warn("[%s#%d/claude-code] could not resolve iterion CLI binary path; board MCP server disabled", task.NodeID, task.Iteration)
			return opts
		}
		env := map[string]string{
			"ITERION_BOARD_CAPS": strings.Join(task.Capabilities, ","),
		}
		if task.StoreDir != "" {
			env["ITERION_STORE_DIR"] = task.StoreDir
		}
		opts = append(opts, claudesdk.WithMCPServer(boardMCPServerName, &claudesdk.MCPStdioServer{
			Command: selfPath,
			Args:    []string{boardMCPSubcommand},
			Env:     env,
		}))
		if len(task.AllowedTools) > 0 {
			*extras = append(*extras, BoardToolsFor(task.Capabilities)...)
		}
		return opts
	}
	// Sandboxed: HTTP transport (Phase 2 of board's stdio-then-HTTP rollout).
	if task.BoardHTTPEndpoint != "" && task.BoardRunToken != "" {
		opts = append(opts, claudesdk.WithMCPServer(boardMCPServerName, &claudesdk.MCPHTTPServer{
			URL: task.BoardHTTPEndpoint,
			Headers: map[string]string{
				"X-Iterion-Run": task.BoardRunToken,
			},
			// Force the board server past claude-code's tool-search deferral
			// so board.* tools surface without a ToolSearch hit, and fail
			// loudly at startup if unreachable (C082).
			AlwaysLoad: true,
		}))
		if len(task.AllowedTools) > 0 {
			*extras = append(*extras, BoardToolsFor(task.Capabilities)...)
		}
		return opts
	}
	b.Logger.Warn("[%s#%d/claude-code] board capabilities granted but workflow is sandboxed and BoardHTTPEndpoint/BoardRunToken not configured; board MCP disabled for this node", task.NodeID, task.Iteration)
	return opts
}

// installInboxDrainHooks delivers operator-chatbox messages mid-session
// (parity with the claw backend's per-iteration drain). PostToolUse fires
// after every tool call and Stop fires when the LLM tries to end the turn;
// both consult the same drain closure and surface queued operator messages
// so the LLM sees operator input on its next turn without waiting for the
// run to finish or pause at a human boundary.
func (b *ClaudeCodeBackend) installInboxDrainHooks(task Task, opts []claudesdk.Option) []claudesdk.Option {
	if task.InboxDrain == nil {
		return opts
	}
	drainAndFormat := func() string {
		texts := task.InboxDrain()
		if len(texts) == 0 {
			return ""
		}
		var sb strings.Builder
		sb.WriteString("Operator queued message")
		if len(texts) > 1 {
			sb.WriteString("s")
		}
		sb.WriteString(":\n\n")
		for i, t := range texts {
			if i > 0 {
				sb.WriteString("\n---\n")
			}
			sb.WriteString(t)
		}
		return sb.String()
	}
	opts = append(opts, claudesdk.WithHook(claudesdk.HookPostToolUse, claudesdk.HookMatcher{
		Handler: func(_ context.Context, _ claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			msg := drainAndFormat()
			if msg == "" {
				return claudesdk.HookOutput{}, nil
			}
			b.Logger.Info("[%s#%d/claude-code] 📥 delivered queued operator message via PostToolUse", task.NodeID, task.Iteration)
			return claudesdk.HookOutput{AdditionalContext: msg, SystemMessage: msg}, nil
		},
	}))
	opts = append(opts, claudesdk.WithHook(claudesdk.HookStop, claudesdk.HookMatcher{
		Handler: func(_ context.Context, _ claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			msg := drainAndFormat()
			if msg == "" {
				return claudesdk.HookOutput{}, nil
			}
			b.Logger.Info("[%s#%d/claude-code] 📥 delivered queued operator message via Stop (blocking stop)", task.NodeID, task.Iteration)
			return claudesdk.HookOutput{BlockStop: true, Reason: msg, SystemMessage: msg}, nil
		},
	}))
	return opts
}

// runRecoveryFormatterPass is the single-pass safety net: when a schema is
// declared but Pass 1 produced empty/nil output or only a fallback text
// wrapper, resume the session for one formatting pass to extract structured
// output. Catches agents that did real work (tools, code changes) but whose
// structured output the SDK didn't capture (e.g. backends where tools are
// implicit). Mutates result and the running token totals in place; failures
// are logged and left non-fatal (the caller keeps Pass 1's output).
func (b *ClaudeCodeBackend) runRecoveryFormatterPass(ctx context.Context, task Task, sessionID string, result *Result, totalIn, totalOut *int) {
	b.Logger.Debug("claude-code: empty output with schema — attempting recovery formatting pass (session=%s)", sessionID)
	fmtRM, fmtErr := b.formatOutput(ctx, task, sessionID)
	if fmtErr != nil {
		b.Logger.Warn("claude-code: recovery formatting pass failed: %v", fmtErr)
		return
	}
	if fmtRM.Usage != nil {
		*totalIn += fmtRM.Usage.InputTokens
		*totalOut += fmtRM.Usage.OutputTokens
		result.Tokens = *totalIn + *totalOut
	}
	result.FormattingPassUsed = true
	fmtOutput, fmtRawLen, fmtFallback := parseSDKOutput(fmtRM.Result, fmtRM.StructuredOutput, task.OutputSchema)
	if len(fmtOutput) > 0 {
		result.Output = fmtOutput
		result.RawOutputLen = fmtRawLen
		result.ParseFallback = fmtFallback
	} else {
		b.Logger.Warn("claude-code: recovery formatting pass also produced empty output")
	}
}

// formatOutput performs the second pass of two-pass execution: resumes the
// Pass 1 session with WithOutputFormat (no tools) to guarantee structured JSON
// output conforming to the schema. The model already has full context from the
// session, so only a short formatting instruction is needed.
func (b *ClaudeCodeBackend) formatOutput(ctx context.Context, task Task, sessionID string) (*claudesdk.ResultMessage, error) {
	// Use the parent context directly — the runtime already enforces budget
	// timeouts. Adding a short artificial timeout here risks cancelling the
	// formatting pass while the CLI is still loading the resumed session.
	fmtCtx := ctx

	var schema map[string]any
	if err := json.Unmarshal(task.OutputSchema, &schema); err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}

	opts := []claudesdk.Option{
		claudesdk.WithResume(sessionID),
		claudesdk.WithOutputFormat(schema),
		claudesdk.WithPermissionMode("bypassPermissions"),
		claudesdk.WithVerbose(true),
		claudesdk.WithStderrCallback(func(line string) {
			if line != "" {
				b.Logger.Info("[%s#%d/fmt] %s", task.NodeID, task.Iteration, line)
			}
		}),
	}

	// Cwd / CLI path handling mirrors Execute(): on the host, pass workdir
	// through; in the sandbox, leave cwd unset (the docker driver picks the
	// spec's WorkspaceFolder) and pin the CLI to the bare in-container name.
	if task.WorkDir != "" && task.Sandbox == nil {
		opts = append(opts, claudesdk.WithCwd(task.WorkDir))
	}
	if task.Sandbox != nil {
		opts = append(opts, claudesdk.WithCLIPath("claude"))
	}

	model := task.Model
	if model == "" {
		model = defaultClaudeCodeModel
	}
	opts = append(opts, claudesdk.WithModel(model))
	if b.Command != "" {
		opts = append(opts, claudesdk.WithCLIPath(b.Command))
	}

	// Capture every spawned subprocess so promptWithTimeout can SIGKILL
	// them if the SDK's read loop gets stuck and ctx cancellation alone
	// fails to wake it. The CommandBuilder we install wraps either the
	// sandbox-routing path or the default exec.CommandContext path; both
	// arms collect the returned cmd into killables.
	var killMu sync.Mutex
	var killables []*exec.Cmd
	captureCmd := func(cmd *exec.Cmd) {
		if cmd == nil {
			return
		}
		killMu.Lock()
		killables = append(killables, cmd)
		killMu.Unlock()
	}
	killAll := func() {
		killMu.Lock()
		defer killMu.Unlock()
		for _, cmd := range killables {
			if cmd == nil || cmd.Process == nil {
				continue
			}
			_ = cmd.Process.Kill()
		}
	}

	if task.Sandbox != nil {
		// When sandboxed, route the CLI subprocess through the sandbox driver so
		// it resumes the session inside the container (where the session file
		// lives) rather than spawning a host claude that can't see it.
		run := task.Sandbox
		opts = append(opts, claudesdk.WithCommandBuilder(func(ctx context.Context, path string, args []string, cwd string, env map[string]string, openStdin bool) *exec.Cmd {
			preview := append([]string{path}, args...)
			b.Logger.Info("claude-code [fmt]: exec %v (cwd=%s, env_keys=%d, stdin=%v)", preview, cwd, len(env), openStdin)
			cmd := run.Command(ctx, append([]string{path}, args...), sandbox.ExecOpts{
				WorkDir:       cwd,
				Env:           env,
				KeepStdinOpen: openStdin,
			})
			captureCmd(cmd)
			return cmd
		}))
	} else {
		// Host-side fallback: the SDK normally constructs its own
		// exec.CommandContext, so we install a builder solely to capture
		// the cmd reference. exec.CommandContext kills the subprocess
		// when ctx fires; the explicit Kill() in killAll is the
		// belt-and-braces hedge for the case where ctx propagation is
		// what's stuck.
		opts = append(opts, claudesdk.WithCommandBuilder(func(ctx context.Context, path string, args []string, cwd string, env map[string]string, openStdin bool) *exec.Cmd {
			cmd := exec.CommandContext(ctx, path, args...)
			cmd.Dir = cwd
			if len(env) > 0 {
				cmd.Env = make([]string, 0, len(env))
				for k, v := range env {
					cmd.Env = append(cmd.Env, k+"="+v)
				}
			}
			captureCmd(cmd)
			return cmd
		}))
	}

	// Forward BYOK credentials and effort level into the formatting pass so
	// the resumed session uses the same auth path as Pass 1.
	opts = append(opts, anthropicCredOptsForCLI(ctx, task.ProviderHint)...)
	effort := task.ReasoningEffort
	if effort == "" {
		effort = defaultClaudeCodeEffort
	}
	opts = append(opts, claudesdk.WithEnv("CLAUDE_CODE_EFFORT_LEVEL", effort))

	prompt := "Format your complete findings as JSON matching the required output schema."

	return promptWithTimeout(fmtCtx, prompt, killAll, opts...)
}

// promptWithTimeout wraps claudesdk.Prompt in a goroutine with
// context-aware cancellation AND a hard subprocess kill on ctx cancel.
//
// The Claude Agent SDK's Prompt() function does not always check
// ctx.Done() in its internal ReadLine() loop — a stuck stream that
// stops emitting bytes will block the goroutine indefinitely, leaking
// the subprocess and pinning the host slot. The killCmd callback,
// when non-nil, is invoked on ctx cancellation to SIGKILL whatever
// subprocesses the SDK spawned via the caller's CommandBuilder. See
// formatOutput for an example of how to wire the cmd capture.
func promptWithTimeout(ctx context.Context, prompt string, killCmd func(), opts ...claudesdk.Option) (*claudesdk.ResultMessage, error) {
	type result struct {
		rm  *claudesdk.ResultMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		rm, err := claudesdk.Prompt(ctx, prompt, opts...)
		ch <- result{rm, err}
	}()

	select {
	case res := <-ch:
		return res.rm, res.err
	case <-ctx.Done():
		if killCmd != nil {
			killCmd()
		}
		// Drain in the background so the Prompt goroutine doesn't
		// leak — Prompt() will return now that the subprocess is dead.
		go func() { <-ch }()
		return nil, fmt.Errorf("claude prompt cancelled: %w", ctx.Err())
	}
}

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

// apiErrorResultStatusRe extracts the HTTP-ish status code the claude CLI
// prints when it renders an upstream API failure AS its result text, e.g.
// "API Error: 529 Overloaded" or "API Error: 503 {...}".
var apiErrorResultStatusRe = regexp.MustCompile(`(?i)^api error:?\s+(\d{3})\b`)

// isTransientAPIErrorResult reports whether a claude CLI "successful" result
// string is actually a rendered upstream API failure of a TRANSIENT class
// (rate-limit / server / overload / connectivity) that a bounded retry can
// recover from. The CLI occasionally finishes a stream with subtype=success
// yet puts an unrecoverable error in the result text; treating that as a
// valid node output silently corrupts the run (and any inheriting session).
//
// Precision matters — a genuine assistant answer must not be mistaken for an
// error. Two guards keep false positives near-zero: the text must be SHORT
// (the CLI's bare error render is; a real answer that merely discusses an API
// error is longer and embedded) and must BEGIN with "api error". A parseable
// 4xx client/auth status returns false so it surfaces as the visible output
// (a retry won't fix a misconfig); 429/5xx/529 and bare connectivity markers
// return true.
func isTransientAPIErrorResult(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || len(t) >= 400 {
		return false
	}
	low := strings.ToLower(t)
	if !strings.HasPrefix(low, "api error") {
		return false
	}
	if m := apiErrorResultStatusRe.FindStringSubmatch(t); m != nil {
		switch m[1] {
		case "408", "409", "425", "429", "500", "502", "503", "504", "529":
			return true
		default:
			return false // 4xx client/auth error — a retry won't fix it
		}
	}
	// No parseable status code (e.g. "API Error: Connection error.") → fall
	// back to the shared connectivity-marker classifier.
	return MatchesNetworkSignature(low)
}

// isModelUnavailableResult detects the claude CLI's "bad/unauthorized model"
// completion. An invalid or inaccessible `--model` does NOT fail the stream
// (subtype=success, IsError=false); the CLI renders its model-error sentence AS
// the result text, e.g. "There's an issue with the selected model
// (openai/gpt-5.5). It may not exist or you may not have access to it. Run
// --model to pick a different model." Untyped, that prose flows into the
// formatting passes and finally surfaces as an opaque schema "missing required
// field" error, hiding the real cause. Detecting it lets the node fail fast and
// legibly. Bounded length + two distinctive markers keep this from matching a
// model's own prose that happens to discuss "selected model".
func isModelUnavailableResult(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || len(t) >= 400 {
		return false
	}
	low := strings.ToLower(t)
	return strings.Contains(low, "selected model") &&
		(strings.Contains(low, "may not exist") || strings.Contains(low, "access to it"))
}

// retypeNetworkError re-classifies an opaque claude_code failure as an
// ErrTransient when the error message or captured stderr shows a transient-
// connectivity marker (fetch failed, ECONNRESET, overloaded, 5xx, …), so the
// executor retries it with backoff instead of failing the node on a blip.
// Already-typed transient / rate-limit errors pass through unchanged. Emits
// one explicit warn so the operator sees a connectivity issue, not just a
// generic retry.
func (b *ClaudeCodeBackend) retypeNetworkError(err error, stderr string, task Task) error {
	if err == nil {
		return nil
	}
	var t *ErrTransient
	var rl *ErrRateLimited
	if errors.As(err, &t) || errors.As(err, &rl) {
		return err
	}
	if !MatchesNetworkSignature(err.Error()) && !MatchesNetworkSignature(stderr) {
		return err
	}
	b.Logger.Warn("[%s#%d/claude-code] network connectivity issue detected; flagging for retry: %v",
		task.NodeID, task.Iteration, err)
	return &ErrTransient{Provider: BackendClaudeCode, Reason: "network", Detail: err.Error()}
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

	for {
		// Pick the timeout that matches the current phase and reset
		// the timer for this iteration. Any progress (assistant
		// tokens, tool calls, tool results) flips us into hot mode
		// and grants the longer budget on every subsequent wait.
		currentTimeout = resetIdleTimer(idle, receivedAny, coldTimeout, hotTimeout)

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
			// hot-timeout mode for the rest of the session.
			receivedAny = true
			switch m := it.msg.(type) {
			case *claudesdk.SystemMessage:
				b.handleSystemMessage(m, task, &meta)
			case *claudesdk.AssistantMessage:
				if err := b.handleAssistantMessage(m, task, inFlightTools, &meta, &lastAssistantText, lastItemTime, cancelStream); err != nil {
					return result, meta, err
				}
			case *claudesdk.UserMessage:
				if err := b.handleUserMessage(m, task, inFlightTools, &consecutiveToolErrors, maxToolErrors, cancelStream); err != nil {
					return result, meta, err
				}
			case *claudesdk.ResultMessage:
				result = m
				backfillEmptyResult(result, lastAssistantText)
			default:
				if it.msg != nil {
					b.Logger.Debug("[%s#%d/claude-code] 📨 %T message", task.NodeID, task.Iteration, it.msg)
				}
			}
			// Advance the thinking-time anchor to this item's arrival so the
			// next thinking-bearing turn measures only its own reasoning gap.
			lastItemTime = time.Now()
		case <-idle.C:
			if currentTimeout <= 0 {
				continue
			}
			cancelStream()
			phase := "cold"
			envHint := "ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT"
			if receivedAny {
				phase = "hot"
				envHint = "ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT"
			}
			b.Logger.Warn("[%s#%d/claude-code] no SDK message for %s (%s phase) — aborting",
				task.NodeID, task.Iteration, currentTimeout, phase)
			return result, meta, fmt.Errorf("claude session idle for %s (%s phase) — aborting (set %s to extend, or 0 to disable)", currentTimeout, phase, envHint)
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
	var turnThinking string
	for _, block := range m.Message.Content {
		if tk, ok := block.(*claudesdk.ThinkingBlock); ok && tk.Thinking != "" {
			turnThinking += tk.Thinking
		}
	}
	if turnThinking != "" {
		tokens := thinktokens.Count(turnThinking)
		ms := int(time.Since(lastItemTime) / time.Millisecond)
		meta.thinkingTokens += tokens
		meta.thinkingMs += ms
		b.Logger.Info("[%s#%d/claude-code] 🧠 thinking: ~%d tok, %dms", task.NodeID, task.Iteration, tokens, ms)
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
				return &ErrRateLimited{Provider: BackendClaudeCode, Detail: strings.TrimSpace(tb.Text)}
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
// errors, and text deltas from a single assistant message.
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
			if bl.IsError {
				logger.Info("[%s#%d/claude-code] ❌ tool error: %v", nodeID, iteration, bl.Content)
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

// rateLimitSignals are case-insensitive substrings of assistant text
// that indicate the upstream provider has cut us off. Two observed
// shapes so far:
//   - Anthropic forfait quota: "You've hit your limit · resets …" —
//     short standalone assistant text, no HTTP 429.
//   - ZAI / Anthropic-shaped facade: "API Error: Request rejected (429)
//     · Usage limit reached for 5 hour. Your limit will reset at …" —
//     the CLI relays the upstream 429 into assistant text.
//
// Kept narrow: generic substrings like "rate_limit_error" were dropped
// because security-audit agents legitimately mention them in prose.
// The 200-char length cap is the second guard against agents quoting
// these phrases mid-paragraph.
var rateLimitSignals = []string{
	"hit your limit",
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
	for _, sig := range rateLimitSignals {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
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

// anthropicCredOptsForCLI returns claudesdk.WithEnv options that point
// the spawned Claude Code subprocess at the right credentials.
//
// providerHint, when non-empty, overrides the default precedence with
// a per-node routing decision (from the DSL `provider:` field):
//   - "anthropic" — force Anthropic-direct (API key or OAuth dir),
//     skip z.ai even if ZAI_API_KEY is set on the process. Use when a
//     specific node needs Anthropic's full context window (1M on
//     Claude Opus 4.7) instead of the smaller z.ai window.
//   - "zai" — force z.ai routing (Anthropic-shaped facade backed by
//     GLM-4.6) even if Anthropic credentials are present. Use to pin
//     a node to GLM regardless of process-env precedence.
//   - "" / "auto" — current process-env-driven precedence (below).
//
// Default precedence (first match wins, returned options are mutually
// exclusive — never set both ANTHROPIC_API_KEY and CLAUDE_CONFIG_DIR):
//
//  1. Per-run BYOK z.ai key: ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN
//     (z.ai's Coding-Plan token routes through Anthropic-shaped wire to
//     z.ai's gateway, which aliases the model to GLM-4.5/4.6 internally).
//  2. Per-run BYOK Anthropic key: ANTHROPIC_API_KEY.
//  3. Per-run OAuth-forfait credentials.json (desktop): CLAUDE_CONFIG_DIR.
//     NB: on the cloud the same kind is scheduled for removal under
//     Anthropic Consumer Terms — see .plans/zai-glm-oauth.md.
//  4. Process-env fallback ZAI_API_KEY: same shape as case 1, lets
//     desktop users put `ZAI_API_KEY=...` in ~/.iterion/env without
//     also having to set ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN by
//     hand. ANTHROPIC_API_KEY in env (if present) takes precedence
//     via the CLI's own resolution; we don't set anything in that
//     case so the inherited env wins.
func anthropicCredOptsForCLI(ctx context.Context, providerHint string) []claudesdk.Option {
	return credEnvToOpts(anthropicCredEnvForCLI(ctx, providerHint))
}

// credEnvToOpts converts a credential env map into claudesdk.Option
// values with a stable key order. Extracted so the cross-provider
// fingerprint path can compute the env map once, derive a fingerprint,
// and pass the same map to the SDK without recomputing.
func credEnvToOpts(env map[string]string) []claudesdk.Option {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	opts := make([]claudesdk.Option, 0, len(keys))
	for _, k := range keys {
		opts = append(opts, claudesdk.WithEnv(k, env[k]))
	}
	return opts
}

// shouldDropSessionFork decides whether to skip --resume + --fork-session
// for the incoming task. Thinking blocks in a Claude session carry
// provider-specific signatures; reusing a session built on a different
// provider surfaces HTTP 400 "Invalid signature in thinking block"
// the moment the new provider reads the prior conversation.
//
// Drop policy (forks only — a bare resume from the same daemon process
// is always same-provider continuation, so signatures are trustworthy):
//
//   - parent fingerprint set AND differs from current → drop.
//   - parent fingerprint EMPTY (legacy output produced by a binary
//     that predates the stamp, or by a daemon restarted across a
//     provider switch) → drop conservatively. The alternative —
//     "proceed when unknown" — was the actual observed failure mode:
//     a fresh Anthropic daemon attempting to fork a session-id
//     produced by an older ZAI-side binary blew up on the 400 with
//     nothing flagging the mismatch. Losing head-session continuity
//     for one node is recoverable; a 400 is not.
//   - parent fingerprint set, current fingerprint EMPTY (provider
//     env is currently unresolved — e.g. cred ctx not wired) → keep
//     the fork. The CLI subprocess will fall back to inherited env,
//     and if that's a mismatch the surface error is the same 400 we
//     started with; we don't gain anything by dropping pre-emptively
//     when we can't classify ourselves.
//
// Returns (drop, reason). The reason string carries no secrets and is
// safe to log verbatim.
func shouldDropSessionFork(task Task, currentFingerprint string) (bool, string) {
	if !task.ForkSession {
		return false, ""
	}
	if task.SessionFingerprint == "" {
		return true, "parent session has no recorded provider fingerprint (legacy output or pre-stamp binary) — starting fresh to avoid cross-provider thinking-block 400s"
	}
	if currentFingerprint != "" && task.SessionFingerprint != currentFingerprint {
		return true, fmt.Sprintf("parent session was built on %q but current provider is %q (signed thinking blocks would 400 on cross-provider reuse)",
			task.SessionFingerprint, currentFingerprint)
	}
	return false, ""
}

// providerFingerprint derives a stable identifier for the routing
// decision encoded by a cred env map. Two calls to anthropicCredEnvForCLI
// with the same provider precedence return the same fingerprint, so
// sessions produced under one provider can be detected (and dropped)
// when a later run targets a different one. Key values are NOT
// included — fingerprints are safe to log and to ferry through the
// recipe output map.
func providerFingerprint(env map[string]string) string {
	if env == nil {
		return "anthropic-env"
	}
	if base := env["ANTHROPIC_BASE_URL"]; base != "" {
		return "facade:" + base
	}
	if env["ANTHROPIC_API_KEY"] != "" {
		return "anthropic-direct"
	}
	if env["CLAUDE_CONFIG_DIR"] != "" {
		return "anthropic-oauth"
	}
	// Explicit zeroing of BASE_URL/AUTH_TOKEN (the providerHint==anthropic
	// path) lands here too — it means "use the inherited ANTHROPIC_API_KEY
	// from the process env", which is also Anthropic-direct semantically.
	return "anthropic-env"
}

// anthropicCredEnvForCLI is the testable core: it returns the env
// variables (key → value) the claude_code subprocess should be invoked
// with, based on the context-bound credentials and the optional
// providerHint. anthropicCredOptsForCLI wraps it into claudesdk.Option
// values for the SDK call site. Separated so unit tests can assert
// routing decisions without reflecting on closures.
//
// An empty key string with a non-empty key entry means "clear this
// inherited env var" (e.g. {"ANTHROPIC_BASE_URL": ""} actively
// suppresses a stale z.ai value in the parent env when the hint asks
// for Anthropic-direct).
func anthropicCredEnvForCLI(ctx context.Context, providerHint string) map[string]string {
	creds, hasCreds := secrets.CredentialsFromContext(ctx)

	// providerHint=="anthropic": force Anthropic-direct. Skip the z.ai
	// branches entirely, even if ZAI_API_KEY is in the process env.
	if providerHint == "anthropic" {
		if hasCreds {
			if k := creds.APIKey(secrets.ProviderAnthropic); k != "" {
				return map[string]string{"ANTHROPIC_API_KEY": k}
			}
			if d := creds.OAuthDir(string(secrets.OAuthKindClaudeCode)); d != "" {
				return map[string]string{"CLAUDE_CONFIG_DIR": d}
			}
		}
		// Process-env path: rely on ANTHROPIC_API_KEY inherited by the
		// CLI. Actively clear ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN
		// so a stale z.ai value from the parent env doesn't leak in.
		return map[string]string{
			"ANTHROPIC_BASE_URL":   "",
			"ANTHROPIC_AUTH_TOKEN": "",
		}
	}

	// providerHint=="zai": force the z.ai facade. Prefer in-context
	// creds; fall back to ZAI_API_KEY in the process env.
	if providerHint == "zai" {
		if hasCreds {
			if k := creds.APIKey(secrets.ProviderZAI); k != "" {
				return map[string]string{
					"ANTHROPIC_BASE_URL":   secrets.ZAIDefaultBaseURL,
					"ANTHROPIC_AUTH_TOKEN": k,
				}
			}
		}
		if zai := os.Getenv("ZAI_API_KEY"); zai != "" {
			baseURL := os.Getenv("ANTHROPIC_BASE_URL")
			if baseURL == "" {
				baseURL = secrets.ZAIDefaultBaseURL
			}
			return map[string]string{
				"ANTHROPIC_BASE_URL":   baseURL,
				"ANTHROPIC_AUTH_TOKEN": zai,
			}
		}
		// No z.ai key reachable — clear hostile env and let downstream
		// surface the "no credential" error rather than silently
		// falling back to a different provider.
		return map[string]string{
			"ANTHROPIC_BASE_URL":   "",
			"ANTHROPIC_AUTH_TOKEN": "",
		}
	}

	// Default precedence (providerHint is "" / "auto").
	if hasCreds {
		switch {
		case creds.APIKey(secrets.ProviderZAI) != "":
			return map[string]string{
				"ANTHROPIC_BASE_URL":   secrets.ZAIDefaultBaseURL,
				"ANTHROPIC_AUTH_TOKEN": creds.APIKey(secrets.ProviderZAI),
			}
		case creds.APIKey(secrets.ProviderAnthropic) != "":
			return map[string]string{"ANTHROPIC_API_KEY": creds.APIKey(secrets.ProviderAnthropic)}
		case creds.OAuthDir(string(secrets.OAuthKindClaudeCode)) != "":
			return map[string]string{"CLAUDE_CONFIG_DIR": creds.OAuthDir(string(secrets.OAuthKindClaudeCode))}
		}
	}
	// Env-fallback: ZAI_API_KEY is the convenience knob for desktop
	// users. Only honoured when no Anthropic-flavoured creds are
	// already wired by env — ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN
	// from the inherited env stays authoritative.
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		if zai := os.Getenv("ZAI_API_KEY"); zai != "" {
			baseURL := os.Getenv("ANTHROPIC_BASE_URL")
			if baseURL == "" {
				baseURL = secrets.ZAIDefaultBaseURL
			}
			return map[string]string{
				"ANTHROPIC_BASE_URL":   baseURL,
				"ANTHROPIC_AUTH_TOKEN": zai,
			}
		}
	}
	return nil
}
