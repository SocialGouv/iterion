package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/usagecap"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// defaultClaudeCodeModel is the model iterion forces on the claude_code
// backend when the workflow doesn't specify one. Mirrors the official
// Claude Code CLI default — Opus 5 (1M context window). Workflows can
// always override via the node's `model:` field — including the
// env-driven form `model: "${ITERION_CLAUDE_CODE_MODEL:-claude-opus-5}"`
// which the IR expander in pkg/backend/model/executor.go resolves
// before this backend ever sees the task. Operators who want to pin
// every claude_code node to a single gateway-side alias (e.g. GLM 5.1
// on z.ai) should put the env var in their .env and use the DSL form
// above in the bots that opt in.
const defaultClaudeCodeModel = "claude-opus-5"

// defaultClaudeCodeEffort is the reasoning effort iterion forces on the
// claude_code backend when the workflow doesn't specify one. The bare API
// default on the opus tier is "high", but the claude_code backend runs
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
	// formatOutputFn replaces the CLI-spawning formatting pass in tests: the
	// loop around it — retry, terminal verdict, usage, cost — is where the
	// accounting defects lived, and it had no seam to be exercised through.
	formatOutputFn func(ctx context.Context, task Task, sessionID string) (*claudesdk.ResultMessage, error)
	// formatRetryDelay overrides the pause before a repeated formatting
	// attempt: zero means the default, negative means none (tests). Per
	// backend, not package-wide, so parallel tests cannot race on it.
	formatRetryDelay time.Duration
}

// defaultFormatRetryDelay is the pause before a formatting attempt is
// repeated after a retryable render: a throttle answered immediately is a
// throttle again.
const defaultFormatRetryDelay = 2 * time.Second

// retryDelay is the pause this backend takes before repeating a formatting
// attempt.
func (b *ClaudeCodeBackend) retryDelay() time.Duration {
	switch {
	case b.formatRetryDelay < 0:
		return 0
	case b.formatRetryDelay == 0:
		return defaultFormatRetryDelay
	}
	return b.formatRetryDelay
}

// formatPass runs one formatting pass: the CLI, or the test seam.
func (b *ClaudeCodeBackend) formatPass(ctx context.Context, task Task, sessionID string) (*claudesdk.ResultMessage, error) {
	if b.formatOutputFn != nil {
		return b.formatOutputFn(ctx, task, sessionID)
	}
	return b.formatOutput(ctx, task, sessionID)
}

// Execute runs the claude CLI with the given task using the Claude Agent SDK.
// buildTransportOptions assembles the base claudesdk options for a claude_code
// run — system prompt, setting sources, cwd, CLI path, permission mode, model,
// sandbox command builder, reasoning effort, and max-turns. Split out of the
// long Execute method; this prefix carries no post-session state (the
// closure-capturing hooks — stderr/ask_user/secret/board/inbox — stay in
// Execute).
//
// The second return value is non-nil only for sandboxed tasks: a
// best-effort cleanup that terminates the in-container claude process
// recorded by the command wrapper (native:221edac8). Execute must defer
// it so aborted sessions cannot leak the subprocess.
func (b *ClaudeCodeBackend) buildTransportOptions(task Task) ([]claudesdk.Option, func()) {
	var opts []claudesdk.Option
	var sandboxCleanup func()

	// Route the SDK's internal error diagnostics (control-protocol
	// delivery failures and the like) to the backend logger.
	opts = append(opts, claudesdk.WithLogf(func(format string, args ...any) {
		b.Logger.Error(format, args...)
	}))

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
	// Setting sources are inherited (above); MCP servers are NOT. The node's
	// resolved MCP set — .bot `mcp_server:`/`mcp:` blocks, the repo's
	// .mcp.json via autoload_project, iterion's ask_user/board servers —
	// travels via --mcp-config, and --strict-mcp-config makes that set
	// authoritative: the operator's personal ~/.claude.json servers don't
	// boot inside bot nodes (undeclared tools, per-visit npx/chromium boots
	// on loop-heavy bots, API keys on the argv — issue #506).
	// ITERION_CLAUDE_CODE_STRICT_MCP=0 restores host inheritance.
	if strictMCPFromEnv() {
		opts = append(opts, claudesdk.WithStrictMCPConfig(true))
	}
	// Opt-in: withhold the subagent/orchestration tool surface from nodes
	// that are not in ultracode mode. Off by default — a claude_code node
	// keeps its full native toolset, subagents included; that adaptivity is
	// the point of the backend. A deployment whose served model family
	// hallucinates task ids and deadlocks on TaskOutput can switch the
	// surface off wholesale instead of relying on the stall recovery.
	// Ultracode nodes keep it: orchestration is what the mode grants.
	if disallowOrchestrationToolsFromEnv() && !task.Ultracode {
		opts = append(opts, claudesdk.WithDisallowedTools(orchestrationTools...))
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
	// The DSL's canonical model spec is provider-prefixed
	// ("anthropic/claude-…", the form claw parses), but the claude CLI
	// only accepts bare model names and rejects the prefixed form as an
	// unknown model. Strip the anthropic prefix; any other provider
	// prefix stays and fails fast as a genuinely non-Anthropic model.
	model = strings.TrimPrefix(model, "anthropic/")
	opts = append(opts, claudesdk.WithModel(model))

	// CLI binary path: the per-node task override (DSL `command:`, an
	// alternate claude-code-compatible CLI) wins over the backend-level
	// default; the shared backend is
	// left unmutated so it can serve other nodes with their own override.
	cliPath := b.Command
	if task.Command != "" {
		cliPath = task.Command
	}
	if cliPath != "" {
		opts = append(opts, claudesdk.WithCLIPath(cliPath))
	}

	// When the run is sandboxed, route the claude CLI subprocess
	// through the sandbox driver so the agent's bash/edit tools
	// execute inside the container, not on the host. Cwd/Env are
	// passed via the runtime-native channels (e.g. `docker exec
	// --workdir / --env`); the SDK disables its own cmd.Dir / cmd.Env
	// application when a builder is set.
	if task.Sandbox != nil {
		run := task.Sandbox
		// Record the in-container PID so the session end can actually
		// terminate claude: killing the host-side `docker exec` client
		// leaks the in-container process (native:221edac8 — leaked
		// claudes stack across retries and starve the forfait). The
		// wrapper writes its PID to a pidfile then exec's claude (same
		// PID, same fds); Execute defers killSandboxDelegate.
		mark := sandboxDelegateMark(task)
		sandboxCleanup = killSandboxDelegate(run, mark, b.Logger)
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
			return run.Command(ctx, wrapSandboxDelegateArgv(mark, append([]string{path}, args...)), sandbox.ExecOpts{
				WorkDir:       cwd,
				Env:           env,
				KeepStdinOpen: openStdin,
			})
		}))
	} else {
		// Host path: install a builder solely to (a) surface the resolved
		// claude invocation — the default spawn is opaque, so a silent
		// "0 tokens / formatting-pass-fallback" structured-output failure can't
		// be traced to the concrete command + per-task env overrides — and
		// (b) keep the env identical to the SDK default (os.Environ() + the
		// per-task entries via hostSpawnEnv), so this is behaviour-neutral.
		opts = append(opts, claudesdk.WithCommandBuilder(func(ctx context.Context, path string, args []string, cwd string, env map[string]string, openStdin bool) *exec.Cmd {
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.Logger.Info("claude-code: host exec %v (cwd=%s, stdin=%v, task_env_keys=%v)",
				append([]string{path}, args...), cwd, openStdin, keys)
			cmd := exec.CommandContext(ctx, path, args...)
			if cwd != "" {
				cmd.Dir = cwd
			}
			cmd.Env = hostSpawnEnv(env)
			return cmd
		}))
	}

	opts = append(opts, perTaskSpawnOpts(task)...)

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

	return opts, sandboxCleanup
}

// claudeCodeThinkingDisplay resolves the --thinking-display value passed
// to the CLI. In headless (--print) mode the CLI defaults thinking
// display to omitted on Opus 4.8+ — thinking blocks stream with empty
// text and only the encrypted signature — so iterion requests the
// readable summary by default. ITERION_CLAUDE_CODE_THINKING_DISPLAY
// overrides: "omitted" restores the latency-optimised default, "off"
// stops passing the flag entirely (required for claude CLIs older than
// the flag, which reject unknown options).
func claudeCodeThinkingDisplay() string {
	switch v := os.Getenv("ITERION_CLAUDE_CODE_THINKING_DISPLAY"); v {
	case "":
		return "summarized"
	case "off":
		return ""
	default:
		return v
	}
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

	opts, sandboxCleanup := b.buildTransportOptions(task)
	// Terminate the in-container claude on every exit path — clean,
	// aborted, or panicking. Idempotent: after a clean CLI exit the
	// recorded PID is gone and the kill script no-ops (native:221edac8).
	if sandboxCleanup != nil {
		defer sandboxCleanup()
	}
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

	// Stamp every usage reading with the provider-routing label of THIS
	// session. One wrap here covers all three detection sites (the
	// rate_limit_event stream and both text-relayed refusal paths): the
	// consumer keys the reading under the credential the session actually
	// ran on, not the bundle's default precedence.
	task.Hooks.OnUsageWindow = stampUsageSource(task.Hooks.OnUsageWindow, currentFingerprint)

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
	// The SDK invokes WithStderrCallback from its own drainStderr goroutine,
	// which cmd.Wait() does not synchronise with — so it can still be writing
	// when we read stderrBuf.String() after runSession returns. Guard both
	// sides with a mutex (strings.Builder is not concurrency-safe).
	var (
		stderrMu  sync.Mutex
		stderrBuf strings.Builder
	)
	readStderr := func() string {
		stderrMu.Lock()
		defer stderrMu.Unlock()
		return stderrBuf.String()
	}
	opts = append(opts, claudesdk.WithStderrCallback(func(line string) {
		stderrMu.Lock()
		stderrBuf.WriteString(line)
		stderrBuf.WriteString("\n")
		stderrMu.Unlock()
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
	var pendingQuestion atomic.Value   // pendingAskUser
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
	opts = b.wireUserMCP(task, opts, &extraAllowedTools)

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
	if p, ok := pendingQuestion.Load().(pendingAskUser); ok && p.Question != "" {
		marker, _ := pendingPermission.Load().(map[string]any)
		return b.buildAskUserPendingResult(task, p, marker, rm, sessMeta, currentFingerprint, duration, readStderr()), nil
	}

	if streamErr != nil {
		return b.buildStreamErrorResult(rm, sessMeta, streamErr, readStderr(), duration, task)
	}

	result = Result{
		Duration:           duration,
		ExitCode:           0,
		Stderr:             readStderr(),
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

	// Every guard on the result TEXT lives in renderedFailure, shared with
	// the formatting passes: a render is never an answer, on any pass. The
	// session was billed all the same: its cost goes out with the verdict.
	if err := b.renderedFailure(rm, task, "pass 1"); err != nil {
		typed := typedFailure(&result, task, totalIn, totalOut, err, rm)
		return result, typed
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
	var recoveryRM *claudesdk.ResultMessage
	if (len(output) == 0 || fallback) && len(task.OutputSchema) > 0 && rm.SessionID != "" {
		var rerr error
		if recoveryRM, rerr = b.runRecoveryFormatterPass(ctx, task, rm.SessionID, &result, &totalIn, &totalOut); rerr != nil {
			typed := typedFailure(&result, task, totalIn, totalOut, rerr, rm, recoveryRM)
			return result, typed
		}
	}

	annotateCost(&result, task, totalIn, totalOut, rm, recoveryRM)
	return result, nil
}

// renderedFailure re-types a result whose TEXT is the CLI's render of an
// upstream failure — a quota window, a rejected credential, an unavailable
// model, a transient API error — into the typed error the executor knows how
// to route. One predicate for every result message the delegation reads:
// pass 1, each formatting pass, the recovery pass. A render that reaches
// parseSDKOutput becomes the node's answer, and the graph continues on it
// (measured: a campaign node "rendered" an upstream 500 and the next node
// spent 283 minutes on it). Order is the most specific verdict first: a
// window notice carries evidence the generic retry would lose, and a dead
// credential must not be retried at all.
func (b *ClaudeCodeBackend) renderedFailure(rm *claudesdk.ResultMessage, task Task, pass string) error {
	if rm == nil || rm.Result == nil {
		return nil
	}
	// An object the TEXT itself is (direct or fenced) is the answer, whatever
	// words it contains: a short JSON answer about quotas must not read as a
	// quota notice — structuredObject is the one definition, the one
	// parseSDKOutput ships. Two vetoes: the CLI's render form ("API Error:
	// …") is never an answer, whatever it carries after the prefix; an
	// error envelope ({"error": …}, {"type":"error", …}) is not one either.
	// An object the SDK carried BESIDE a text earns no exemption: the text
	// goes through the guards below like any other — a resumed pass could
	// echo a prior turn's object next to a refusal, and a window verdict
	// shipped as an answer is the class this predicate closes; beside plain
	// prose no guard matches and the answer ships anyway. No evidence is
	// filed from a shipped answer: a false bench costs more than a reading
	// the next pass files.
	// Read from the text alone: with the SDK object passed too, a populated
	// structured_output — the normal shape on a schema pass — answers first
	// and hides that the text is that very object.
	if obj, _, found, fromText := structuredObject(rm.Result, nil); found && fromText && len(obj) > 0 &&
		!errorBodyObject(obj) && !hasRenderPrefix(*rm.Result) {
		return nil
	}
	// Quota / usage-window guard on the RESULT. The forfait's weekly / session /
	// 5h caps can come back as the result text (subtype=success, IsError=true)
	// with no assistant text block for the stream classifier to catch — re-check
	// here so the notice becomes a typed, resumable rate-limit error instead of
	// flowing into structured-output validation as a misleading "missing
	// required field". Usage-window → resumable after reset; a plain throttle →
	// the executor's transient retry.
	if rm.Result != nil && isRateLimitMessage(*rm.Result) {
		detail := strings.TrimSpace(*rm.Result)
		kind, window, resetAt := classifyRateLimit(detail, time.Now())
		b.Logger.Warn("[%s#%d/claude-code %s] provider quota/rate-limit result (%s) — failing: %.120s",
			task.NodeID, task.Iteration, pass, kind, detail)
		// Same evidence duty as the stream path: a text-relayed refusal
		// that names a meter window must reach the store, or the
		// credential-tier skip stays blind to it.
		if window != "" && task.Hooks.OnUsageWindow != nil {
			_ = task.Hooks.OnUsageWindow(usagecap.Reading{
				Window:     window,
				Status:     usagecap.StatusRejected,
				ObservedAt: time.Now().UTC(),
				ResetsAt:   resetAt,
			})
		}
		return &ErrRateLimited{Provider: BackendClaudeCode, Detail: detail, Kind: kind, ResetAt: resetAt}
	}

	// Auth-failure guard. A dead/expired forfait token (or a rejected API key)
	// does NOT fail the stream (subtype=success, IsError=true): the claude CLI
	// renders the auth error AS the result text (e.g. "Failed to authenticate.
	// API Error: 401 Invalid bearer token"). Left untouched it flows into the
	// formatting passes and finally surfaces as an opaque "missing required
	// field" schema error — the exact masking that turns a dead credential into
	// a wild goose chase through the structured-output machinery. Fail fast with
	// a legible auth error. Non-transient (a retry can't revive a dead token).
	if authErr := authFailureFast(rm.Result, task); authErr != nil {
		b.Logger.Error("[%s#%d/claude-code %s] authentication failed — failing fast: %.160s",
			task.NodeID, task.Iteration, pass, redactAuthRender(strings.TrimSpace(*rm.Result)))
		return authErr
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
		b.Logger.Error("[%s#%d/claude-code %s] model %q unavailable to the CLI — failing fast: %.160s",
			task.NodeID, task.Iteration, pass, task.Model, detail)
		return fmt.Errorf("claude-code: model %q is unavailable or unauthorized (check the node's backend/model — a claude_code node cannot run a non-Anthropic model): %s", task.Model, detail)
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
		b.Logger.Warn("[%s#%d/claude-code %s] upstream API-error result text detected — flagging for retry: %.120s",
			task.NodeID, task.Iteration, pass, detail)
		return &ErrTransient{Provider: BackendClaudeCode, Reason: "api_error_result", Detail: detail}
	}

	return nil
}

// hasRenderPrefix reports the CLI's own render form of an upstream failure:
// "API Error: …", whatever follows — a relayed body, fenced or not, is not an
// answer.
func hasRenderPrefix(text string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "api error")
}

// errorBodyObject reports an object that is an error envelope — a bare
// {"error": …} body, or the provider's own {"type":"error","error":{…}}
// with whatever else it carries (a request id) — relayed verbatim as the
// result text: not an answer even though it parses as one. A legitimate
// answer that carries an `error` field beside real data, without the
// provider's type marker, stays an answer.
func errorBodyObject(obj map[string]any) bool {
	if _, ok := obj["error"]; !ok {
		return false
	}
	return len(obj) == 1 || obj["type"] == "error"
}

// typedFailure returns err with the delegation's spend stamped on the result
// first: a typed failure still spent Pass 1 and whatever passes ran, and the
// caps, the fallback chain's carried spend and a donor's ledger read the
// cost from the output map — an unallocated map records nothing. Callers
// hoist the call into its own statement before returning `result`: Go leaves
// the order between a plain operand and a call in one return list
// unspecified, and the stamp must land before the copy is taken.
func typedFailure(result *Result, task Task, totalIn, totalOut int, err error, rms ...*claudesdk.ResultMessage) error {
	if result.Output == nil {
		result.Output = map[string]any{}
	}
	annotateCost(result, task, totalIn, totalOut, rms...)
	return err
}

// renderRetryable reports whether a rendered failure is one a repeat of the
// same pass can recover from: the transient class (5xx, overload,
// connectivity) and a bare throttle. The throttle is safe by construction —
// classifyRateLimit returns RateLimitKindTransient only together with an
// empty window, and the usagecap write is gated on a named window, so a
// repeat cannot re-file evidence. A credential, model or usage-window verdict
// is terminal for this delegation — retrying it re-spends the pass against a
// provider that just refused and re-files the same usage evidence.
func renderRetryable(err error) bool {
	var tr *ErrTransient
	if errors.As(err, &tr) {
		return true
	}
	var rl *ErrRateLimited
	return errors.As(err, &rl) && rl.Kind == RateLimitKindTransient
}

// annotateCost stamps `_tokens` / `_model` / `_cost_usd` on the delegation
// output. The pricing model is the workflow-declared task.Model when set,
// else the CLI-resolved effective model captured from system/init — a node
// that omits `model:` (backend auto-detection) otherwise annotates with an
// empty model id, prices to zero, and the whole run reports tokens but no
// cost (the studio report then claims "no LLM cost recorded" forever). A
// cost the CLI itself computed (ResultMessage.TotalCostUSD, metered API
// runs) wins over the static estimate; sessions that report none (OAuth
// forfait) fall back to the token estimate. Across multiple result
// messages (Pass 1 + a formatting pass) the MAX is used, never the sum:
// per the CLI's session-cumulative accounting the later message subsumes
// the earlier, and max degrades to a small under-count rather than a
// double-count if that accounting is ever per-invocation.
func annotateCost(result *Result, task Task, totalIn, totalOut int, rms ...*claudesdk.ResultMessage) {
	model := task.Model
	if model == "" {
		model = result.EffectiveModel
	}
	var cliCost float64
	for _, rm := range rms {
		if rm != nil && rm.TotalCostUSD != nil && *rm.TotalCostUSD > cliCost {
			cliCost = *rm.TotalCostUSD
		}
	}
	cost.AnnotateWithUSD(result.Output, model, totalIn, totalOut, cliCost)
}

// buildAskUserPendingResult packages the Result returned when the native
// ask_user MCP hook fired mid-session: it short-circuits the stream and
// surfaces the captured question to the runtime via the
// `_needs_interaction` / `_interaction_questions` envelope so the engine
// can pause the run and elicit the operator. Extracted from Execute for
// readability; the per-field semantics (Duration, ExitCode=0, Stderr,
// SessionID-from-rm, SessionFingerprint) are identical to the original
// inline path.
func (b *ClaudeCodeBackend) buildAskUserPendingResult(task Task, p pendingAskUser, marker map[string]any, rm *claudesdk.ResultMessage, sessMeta sessionMeta, currentFingerprint string, duration time.Duration, stderr string) Result {
	if marker != nil {
		b.Logger.Info("[%s#%d/claude-code] 🔐 tool-permission approval escalated to the runtime", task.NodeID, task.Iteration)
	} else {
		b.Logger.Info("[%s#%d/claude-code] 🛑 ask_user escalated via native MCP tool", task.NodeID, task.Iteration)
	}
	sessID := ""
	if rm != nil {
		sessID = rm.SessionID
	}
	questions := map[string]any{AskUserQuestionKey: p.Question}
	AddAskUserOptionKeys(questions, p.Options, p.AllowFreeText)
	if marker != nil {
		questions[permission.InteractionMarkerKey] = marker
	}
	if len(p.AwaitPending) > 0 {
		questions[AwaitPendingInteractionsKey] = AwaitPendingToQuestions(p.AwaitPending)
	}
	askResult := Result{
		Output: map[string]any{
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
		annotateCost(&result, task, *totalIn, *totalOut, rm)
		return true, result, nil
	}
	const maxFmtAttempts = 2
	var lastFmtErr error
	// Every attempt that produced a message: each was billed, and the
	// pricing takes the highest CLI figure among them — an earlier attempt
	// must count when a later one could not spawn, or answered.
	var ranRMs []*claudesdk.ResultMessage
	for attempt := 1; attempt <= maxFmtAttempts; attempt++ {
		b.Logger.Debug("claude-code [formatting pass %d/%d] starting structured output extraction (session=%s)", attempt, maxFmtAttempts, rm.SessionID)
		fmtRM, fmtErr := b.formatPass(ctx, task, rm.SessionID)
		if fmtErr == nil {
			ranRMs = append(ranRMs, fmtRM)
			// The pass ran and was billed, whatever its result says: its
			// usage counts on every path out of here, the typed ones too.
			if fmtRM.Usage != nil {
				*totalIn += fmtRM.Usage.InputTokens
				*totalOut += fmtRM.Usage.OutputTokens
				result.Tokens = *totalIn + *totalOut
			}
			result.FormattingPassUsed = true
			// The formatter's result is read through the same predicate as
			// pass 1: a render here would otherwise be parsed as the output.
			if rerr := b.renderedFailure(fmtRM, task, fmt.Sprintf("formatting pass %d/%d", attempt, maxFmtAttempts)); rerr != nil {
				if !renderRetryable(rerr) {
					// A credential, model or window verdict is terminal: a
					// second attempt re-spends the pass against a provider
					// that just refused and re-files the same evidence.
					typed := typedFailure(&result, task, *totalIn, *totalOut,
						fmt.Errorf("delegate: claude-code formatting pass failed: %w", rerr), append([]*claudesdk.ResultMessage{rm}, ranRMs...)...)
					return true, result, typed
				}
				fmtErr = rerr
			}
		}
		if fmtErr != nil {
			lastFmtErr = fmtErr
			if attempt < maxFmtAttempts {
				b.Logger.Warn("claude-code [formatting pass %d/%d] failed, retrying: %v", attempt, maxFmtAttempts, fmtErr)
				// A throttle or an overload answered at once is the same
				// answer again: a short pause before the repeat, bounded by
				// the run's context.
				if d := b.retryDelay(); d > 0 {
					select {
					case <-ctx.Done():
						// Cancellation wins over the typed cause: a run being
						// cancelled must not read as rate-limited downstream.
						typed := typedFailure(&result, task, *totalIn, *totalOut, ctx.Err(), append([]*claudesdk.ResultMessage{rm}, ranRMs...)...)
						return true, result, typed
					case <-time.After(d):
					}
				}
				continue
			}
			// Both attempts exhausted. Pass 1's own output was already tried
			// by the fast path above with the same arguments, so there is
			// nothing left to recover from it: the delegation fails typed —
			// priced from Pass 1 and from the last attempt that produced a
			// message, whether or not the final one did (annotateCost takes
			// the highest CLI figure and skips a nil message).
			typed := typedFailure(&result, task, *totalIn, *totalOut,
				fmt.Errorf("delegate: claude-code formatting pass failed: %w", fmtErr), append([]*claudesdk.ResultMessage{rm}, ranRMs...)...)
			return true, result, typed
		}

		output, rawLen, fallback := parseSDKOutput(fmtRM.Result, fmtRM.StructuredOutput, task.OutputSchema)
		if fallback && attempt < maxFmtAttempts {
			b.Logger.Warn("claude-code [formatting pass %d/%d] produced fallback text, retrying", attempt, maxFmtAttempts)
			continue
		}
		result.Output = output
		result.RawOutputLen = rawLen
		result.ParseFallback = fallback
		// Priced from every attempt that ran, the one that answered included.
		annotateCost(&result, task, *totalIn, *totalOut, append([]*claudesdk.ResultMessage{rm}, ranRMs...)...)
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
	credEnv := anthropicCredEnvForCLI(ctx, task.ProviderHint, taskSandboxed(task))
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

// runRecoveryFormatterPass is the single-pass safety net: when a schema is
// declared but Pass 1 produced empty/nil output or only a fallback text
// wrapper, resume the session for one formatting pass to extract structured
// output. Catches agents that did real work (tools, code changes) but whose
// structured output the SDK didn't capture (e.g. backends where tools are
// implicit). Mutates result and the running token totals in place. A pass
// that fails to run is logged and left non-fatal (the caller keeps Pass 1's
// output, which the schema then judges); a pass that RENDERS an upstream
// failure is returned typed — the executor routes it, instead of shipping
// Pass 1's fallback text to an opaque schema failure. Returns the pass's own
// ResultMessage whenever it ran (with the verdict too — a billed pass is a
// billed pass), nil when it did not, so the caller's cost annotation sees
// its CLI-reported cost, not just Pass 1's.
func (b *ClaudeCodeBackend) runRecoveryFormatterPass(ctx context.Context, task Task, sessionID string, result *Result, totalIn, totalOut *int) (*claudesdk.ResultMessage, error) {
	b.Logger.Debug("claude-code: empty output with schema — attempting recovery formatting pass (session=%s)", sessionID)
	fmtRM, fmtErr := b.formatPass(ctx, task, sessionID)
	if fmtErr != nil {
		b.Logger.Warn("claude-code: recovery formatting pass failed: %v", fmtErr)
		return nil, nil
	}
	// The pass ran and was billed, whatever its result says.
	if fmtRM.Usage != nil {
		*totalIn += fmtRM.Usage.InputTokens
		*totalOut += fmtRM.Usage.OutputTokens
		result.Tokens = *totalIn + *totalOut
	}
	result.FormattingPassUsed = true
	// A render on the recovery pass is typed and returned, never parsed as
	// the output nor swallowed into an opaque schema failure — with its
	// message, so the caller's cost annotation sees the billed pass.
	if rerr := b.renderedFailure(fmtRM, task, "recovery formatting pass"); rerr != nil {
		return fmtRM, rerr
	}
	fmtOutput, fmtRawLen, fmtFallback := parseSDKOutput(fmtRM.Result, fmtRM.StructuredOutput, task.OutputSchema)
	if len(fmtOutput) > 0 {
		result.Output = fmtOutput
		result.RawOutputLen = fmtRawLen
		result.ParseFallback = fmtFallback
	} else {
		b.Logger.Warn("claude-code: recovery formatting pass also produced empty output")
	}
	return fmtRM, nil
}

// hostSpawnEnv returns the process environment with the per-task env entries
// appended (last-wins), matching the SDK's default host spawn
// (claudesdk/process.go: cmd.Env = os.Environ() then append) and Pass 1. The
// host-side CommandBuilder installed by formatOutput to capture the spawned
// cmd would otherwise set cmd.Env to ONLY the per-task entries, stripping
// PATH/HOME and any ambient credential env from the structured-output format
// pass. Appending the per-task entries last preserves their precedence over
// inherited values (os/exec keeps the last occurrence of a duplicate key).
func hostSpawnEnv(extra map[string]string) []string {
	base := os.Environ()
	for k, v := range extra {
		base = append(base, k+"="+v)
	}
	return base
}

// perTaskSpawnOpts are the per-task knobs BOTH claude spawns must carry: the
// main pass and the structured-output formatting pass that resumes the same
// session.
//
// It is one function rather than two copies because the failure mode of two
// copies is silent and asymmetric — a knob wired into the main pass only lets
// the CLI change its behaviour halfway through a node, with nothing in the
// output to show for it.
func perTaskSpawnOpts(task Task) []claudesdk.Option {
	effort := task.ReasoningEffort
	if effort == "" {
		effort = defaultClaudeCodeEffort
	}
	opts := []claudesdk.Option{claudesdk.WithEnv("CLAUDE_CODE_EFFORT_LEVEL", effort)}
	opts = append(opts, autoMemoryOpts(task)...)
	opts = append(opts, taskExtraEnvOpts(task)...)
	if d := claudeCodeThinkingDisplay(); d != "" {
		opts = append(opts, claudesdk.WithThinkingDisplay(d))
	}
	return opts
}

// autoMemoryOpts wires the node's resolved auto-memory decision into the CLI
// spawn. It is emitted for EVERY claude_code node, including the off case,
// because the CLI's own default is ON: leaving it alone means a bot run reads
// and writes the operator's personal `~/.claude/projects/<cwd>/memory/`
// without anyone asking for it.
//
// The switch rides CLAUDE_CODE_DISABLE_AUTO_MEMORY, which the CLI resolves
// BEFORE any settings file — so both directions beat an operator's
// `autoMemoryEnabled`, and the node's declared behaviour is what actually
// happens. "0" is not a no-op there: it force-ENABLES against a settings.json
// that turned auto-memory off.
//
// The directory rides `--settings`, whose values land in the CLI's
// `flagSettings` layer. That layer outranks user and local settings, and
// `autoMemoryDirectory` is one key the CLI refuses to read from a checked-in
// `.claude/settings.json` at all — so the target repository cannot redirect
// where the memory is written.
func autoMemoryOpts(task Task) []claudesdk.Option {
	disable, settings := autoMemorySpawn(task)
	opts := []claudesdk.Option{claudesdk.WithEnv(autoMemoryDisableEnv, disable)}
	if len(settings) > 0 {
		opts = append(opts, claudesdk.WithSettingsJSON(settings))
	}
	return opts
}

// autoMemoryDisableEnv is the CLI's own auto-memory switch, resolved BEFORE
// any settings file — which is why both directions of the knob ride it.
const autoMemoryDisableEnv = "CLAUDE_CODE_DISABLE_AUTO_MEMORY"

// autoMemorySpawn is autoMemoryOpts' decision, split out so the mapping is
// testable without reaching into the SDK's unexported config. It returns the
// value for CLAUDE_CODE_DISABLE_AUTO_MEMORY and, when memory is on, the
// inline settings JSON pinning the directory (nil otherwise).
func autoMemorySpawn(task Task) (disable string, settings []byte) {
	if task.AutoMemoryDir == "" {
		return "1", nil
	}
	raw, err := json.Marshal(map[string]any{
		"autoMemoryEnabled":   true,
		"autoMemoryDirectory": task.AutoMemoryDir,
	})
	if err != nil {
		// A map of a bool and a string cannot fail to marshal; if it somehow
		// did, enabling auto-memory without pinning the directory would send
		// the agent's notes to the operator's personal memory instead of the
		// run's space. Stay off rather than write to the wrong place.
		return "1", nil
	}
	return "0", raw
}

// taskExtraEnvOpts converts Task.ExtraEnv (KEY=value entries — run-level
// provisioning such as the devbox profile PATH) into per-spawn env
// options. Entries without an '=' are dropped: they cannot form a valid
// environment assignment.
func taskExtraEnvOpts(task Task) []claudesdk.Option {
	opts := make([]claudesdk.Option, 0, len(task.ExtraEnv))
	for _, kv := range task.ExtraEnv {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			opts = append(opts, claudesdk.WithEnv(k, v))
		}
	}
	return opts
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
	// The DSL's canonical model spec is provider-prefixed
	// ("anthropic/claude-…", the form claw parses), but the claude CLI
	// only accepts bare model names and rejects the prefixed form as an
	// unknown model. Strip the anthropic prefix; any other provider
	// prefix stays and fails fast as a genuinely non-Anthropic model.
	model = strings.TrimPrefix(model, "anthropic/")
	opts = append(opts, claudesdk.WithModel(model))
	// CLI binary path: the per-node task override (DSL `command:`, an
	// alternate claude-code-compatible CLI) wins over the backend-level
	// default; the shared backend is
	// left unmutated so it can serve other nodes with their own override.
	cliPath := b.Command
	if task.Command != "" {
		cliPath = task.Command
	}
	if cliPath != "" {
		opts = append(opts, claudesdk.WithCLIPath(cliPath))
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
			// Seed os.Environ() before the per-task entries — matching the
			// SDK's default host spawn (claudesdk/process.go) and Pass 1 — so
			// the format pass inherits PATH/HOME and any ambient credential
			// env instead of running with a stripped environment. The prior
			// code set cmd.Env to ONLY the per-task entries, dropping the
			// inherited env (CLAUDE_CODE_EFFORT_LEVEL is always present, so the
			// strip always fired). Per-task entries stay last so they still win.
			cmd.Env = hostSpawnEnv(env)
			captureCmd(cmd)
			return cmd
		}))
	}

	// Forward BYOK credentials and effort level into the formatting pass so
	// the resumed session uses the same auth path as Pass 1.
	opts = append(opts, anthropicCredOptsForCLI(ctx, task.ProviderHint, taskSandboxed(task))...)
	opts = append(opts, perTaskSpawnOpts(task)...)

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
