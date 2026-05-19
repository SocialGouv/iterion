package delegate

import (
	"context"
	"encoding/json"
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
	"github.com/SocialGouv/iterion/pkg/backend/tooldisplay"
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
// Claude Code CLI default — Opus 4.7 (1M context window). Workflows can
// always override via the node's `model:` field.
const defaultClaudeCodeModel = "claude-opus-4-7"

// defaultClaudeCodeEffort is the reasoning effort iterion forces on the
// claude_code backend when the workflow doesn't specify one. Mirrors the
// official Claude Code CLI default ("xhigh" thinking budget for Opus 4.7,
// per code.claude.com/docs/en/model-config). Workflows can always override
// via `reasoning_effort:`.
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

	var opts []claudesdk.Option

	systemPrompt := task.SystemPromptWithInteraction()
	if systemPrompt != "" {
		opts = append(opts, claudesdk.WithSystemPrompt(systemPrompt))
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
	if len(task.AllowedTools) > 0 {
		opts = append(opts, claudesdk.WithAllowedTools(task.AllowedTools...))
	}
	// Bypass interactive permission prompts: the runtime enforces safety via
	// workspace isolation and allowed-tool lists, so the delegate subprocess
	// does not need its own permission gate.
	opts = append(opts, claudesdk.WithPermissionMode("bypassPermissions"))

	// The CLI requires --verbose when using --output-format=stream-json in
	// --print mode. The SDK always uses stream-json, so we must enable verbose.
	opts = append(opts, claudesdk.WithVerbose(true))

	// Forward stderr from the CLI to the backend logger so silent-failure
	// modes ("session ended without result message") leave a trail of the
	// underlying process output instead of being completely opaque. Auth
	// errors, network errors, config parse errors all emit on stderr —
	// without this they vanish into the SDK's pump and only the wrapping
	// "no result message" surfaces upstream.
	// *iterlog.Logger methods are nil-safe (see pkg/log/log.go), so the
	// per-call guard is redundant; the stderr stream is always worth
	// wiring and the nil-receiver short-circuit costs nothing.
	opts = append(opts, claudesdk.WithStderrCallback(func(line string) {
		if line != "" {
			b.Logger.Warn("claude-code [stderr]: %s", line)
		}
	}))

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

	// Inject Anthropic-flavoured credentials into the CLI subprocess.
	// Single helper so Pass 1 and Pass 2 (formatter) stay symmetric.
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

	// Structured output handling:
	// - When schema is set and NO tools: use native WithOutputFormat (single pass).
	// - When schema is set and tools are present: two-pass execution (see below).
	prompt := task.UserPrompt
	needsTwoPass := len(task.OutputSchema) > 0 && len(task.AllowedTools) > 0
	if len(task.OutputSchema) > 0 && !needsTwoPass {
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

	// Native ask_user interception. When the workflow enables interaction, we
	// register iterion itself as an MCP server exposing the ask_user tool, and
	// install a PreToolUse hook that captures the question and short-circuits
	// the session as soon as the LLM calls it. This mirrors the claw backend's
	// in-process ask_user path. The system prompt's [INTERACTION PROTOCOL]
	// suffix is preserved so the existing JSON-output fallback still works if
	// the LLM bypasses the native tool.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	var pendingQuestion atomic.Value // string

	// Native ask_user is unsupported when the run is sandboxed. The MCP
	// stdio server would point at the iterion binary's host path
	// (os.Executable) which doesn't exist inside the container, and the
	// claudesdk writes the resolved --mcp-config JSON to the host's /tmp
	// — also invisible to `docker exec`. claude rejects the missing
	// config file and exits before producing a result message. The
	// system prompt's [INTERACTION PROTOCOL] suffix already carries a
	// JSON-output fallback, so the LLM can still surface questions
	// without the native tool. This mirrors the claw backend limitation
	// announced via EventSandboxClawRoutedViaRunner.
	if task.InteractionEnabled && task.Sandbox == nil {
		if selfPath, err := os.Executable(); err == nil {
			opts = append(opts, claudesdk.WithMCPServer(askUserMCPServerName, &claudesdk.MCPStdioServer{
				Command: selfPath,
				Args:    []string{askUserMCPSubcommand},
			}))
			// Only extend the allowlist when the workflow already restricts tools.
			// An empty AllowedTools means "no restriction", and the MCP tool will
			// be discoverable without explicit listing.
			if len(task.AllowedTools) > 0 {
				combined := make([]string, 0, len(task.AllowedTools)+1)
				combined = append(combined, task.AllowedTools...)
				combined = append(combined, askUserMCPToolName)
				opts = append(opts, claudesdk.WithAllowedTools(combined...))
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
		} else {
			b.Logger.Warn("[%s#%d/claude-code] could not resolve iterion binary path; native ask_user MCP server disabled (falling back to JSON _needs_interaction protocol): %v", task.NodeID, task.Iteration, err)
		}
	}

	// Board MCP wiring. When the node was granted any board.* capability,
	// register the internal __mcp-board server so the bot can mutate the
	// kanban from inside its reasoning loop. Same sandbox limitation as
	// ask_user — Phase 2 (HTTP transport) addresses the sandboxed path.
	if HasBoardCapability(task.Capabilities) && task.Sandbox == nil {
		if selfPath, err := os.Executable(); err == nil {
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
				combined := append([]string(nil), task.AllowedTools...)
				combined = append(combined, BoardToolsFor(task.Capabilities)...)
				opts = append(opts, claudesdk.WithAllowedTools(combined...))
			}
		} else {
			b.Logger.Warn("[%s#%d/claude-code] could not resolve iterion binary path; board MCP server disabled: %v", task.NodeID, task.Iteration, err)
		}
	} else if HasBoardCapability(task.Capabilities) && task.Sandbox != nil {
		if task.BoardHTTPEndpoint != "" && task.BoardRunToken != "" {
			opts = append(opts, claudesdk.WithMCPServer(boardMCPServerName, &claudesdk.MCPHTTPServer{
				URL: task.BoardHTTPEndpoint,
				Headers: map[string]string{
					"X-Iterion-Run": task.BoardRunToken,
				},
			}))
			if len(task.AllowedTools) > 0 {
				combined := append([]string(nil), task.AllowedTools...)
				combined = append(combined, BoardToolsFor(task.Capabilities)...)
				opts = append(opts, claudesdk.WithAllowedTools(combined...))
			}
		} else {
			b.Logger.Warn("[%s#%d/claude-code] board capabilities granted but workflow is sandboxed and BoardHTTPEndpoint/BoardRunToken not configured; board MCP disabled for this node", task.NodeID, task.Iteration)
		}
	}

	startTime := time.Now()
	rm, sessMeta, streamErr := b.runSession(streamCtx, prompt, task, opts)
	duration := time.Since(startTime)

	// Native ask_user capture takes precedence over any error: if the hook
	// fired, the resulting context cancellation surfaces here as ctx.Err(),
	// which we must not treat as a failure.
	if q, ok := pendingQuestion.Load().(string); ok && q != "" {
		b.Logger.Info("[%s#%d/claude-code] 🛑 ask_user escalated via native MCP tool", task.NodeID, task.Iteration)
		sessID := ""
		if rm != nil {
			sessID = rm.SessionID
		}
		askResult := Result{
			Output: map[string]interface{}{
				"_needs_interaction": true,
				"_interaction_questions": map[string]interface{}{
					AskUserQuestionKey: q,
				},
			},
			Duration:           duration,
			ExitCode:           0,
			Stderr:             stderrBuf.String(),
			BackendName:        BackendClaudeCode,
			SessionID:          sessID,
			SessionFingerprint: currentFingerprint,
		}
		applyClaudeCodeSessionMeta(&askResult, rm, sessMeta)
		return askResult, nil
	}

	if streamErr != nil {
		errResult := Result{
			Duration:    duration,
			ExitCode:    -1,
			Stderr:      stderrBuf.String(),
			BackendName: BackendClaudeCode,
		}
		applyClaudeCodeSessionMeta(&errResult, rm, sessMeta)
		return errResult, fmt.Errorf("delegate: claude-code failed: %w", streamErr)
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
		return result, fmt.Errorf("delegate: claude-code error: subtype=%s", rm.Subtype)
	}

	// Two-pass execution: when tools + schema are both present, Pass 1 output
	// is free-form text. Pass 2 resumes the session with WithOutputFormat to
	// extract a structured output. Both passes route through the sandbox
	// command builder when sandboxed, so the resumed session is found inside
	// the container where Pass 1 created it.
	if needsTwoPass && rm.SessionID != "" {
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
					cost.Annotate(result.Output, task.Model, totalIn, totalOut)
					return result, nil
				}
				return result, fmt.Errorf("delegate: claude-code formatting pass failed: %w", fmtErr)
			}
			if fmtRM.Usage != nil {
				totalIn += fmtRM.Usage.InputTokens
				totalOut += fmtRM.Usage.OutputTokens
				result.Tokens = totalIn + totalOut
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
			cost.Annotate(result.Output, task.Model, totalIn, totalOut)
			return result, nil
		}
		// Defensive: loop fell through without returning. Shouldn't happen
		// (every iteration either returns or continues), but if it did,
		// surface the last formatting error rather than a generic one.
		if lastFmtErr != nil {
			return result, fmt.Errorf("delegate: claude-code formatting pass failed: %w", lastFmtErr)
		}
	}

	// Single-pass path: parse Pass 1 directly.
	output, rawLen, fallback := parseSDKOutput(rm.Result, rm.StructuredOutput, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	// Safety net: if we have a schema but got empty/nil output or only a
	// fallback text wrapper, attempt a formatting pass via session resume.
	// This catches cases where the agent did real work (tools, code changes)
	// but the SDK didn't capture structured output — e.g., backend agents
	// where tools are implicit.
	if (len(output) == 0 || fallback) && len(task.OutputSchema) > 0 && rm.SessionID != "" {
		b.Logger.Debug("claude-code: empty output with schema — attempting recovery formatting pass (session=%s)", rm.SessionID)
		fmtRM, fmtErr := b.formatOutput(ctx, task, rm.SessionID)
		if fmtErr == nil {
			if fmtRM.Usage != nil {
				totalIn += fmtRM.Usage.InputTokens
				totalOut += fmtRM.Usage.OutputTokens
				result.Tokens = totalIn + totalOut
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
		} else {
			b.Logger.Warn("claude-code: recovery formatting pass failed: %v", fmtErr)
		}
	}

	cost.Annotate(result.Output, task.Model, totalIn, totalOut)
	return result, nil
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

func resolveStreamColdTimeout() time.Duration {
	if v := os.Getenv("ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultStreamColdTimeout
}

func resolveStreamHotTimeout() time.Duration {
	if v := os.Getenv("ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultStreamHotTimeout
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

	// Forward messages from the SDK iterator into a channel so we can
	// select on (msg, idle-timer, ctx.Done) and abort cleanly when
	// the session falls silent. Using range-over-func directly would
	// block the goroutine until ctx is cancelled, which the runtime
	// only does at the workflow's max_duration (way too late).
	type streamItem struct {
		msg claudesdk.Message
		err error
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	items := make(chan streamItem, 1)
	go func() {
		defer close(items)
		for msg, err := range sess.Stream(streamCtx) {
			select {
			case items <- streamItem{msg: msg, err: err}:
			case <-streamCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

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
	// Per-session map correlating tool_use_id → tool name so we can echo
	// the name on the completion hook (ToolResultBlock only carries the
	// correlation ID).
	inFlightTools := make(map[string]string)
	currentTimeout := coldTimeout
	idle := time.NewTimer(currentTimeout)
	defer idle.Stop()

	for {
		// Pick the timeout that matches the current phase and reset
		// the timer for this iteration. Any progress (assistant
		// tokens, tool calls, tool results) flips us into hot mode
		// and grants the longer budget on every subsequent wait.
		if receivedAny {
			currentTimeout = hotTimeout
		} else {
			currentTimeout = coldTimeout
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
				if (result.Result == nil || *result.Result == "") && lastAssistantText != "" {
					txt := lastAssistantText
					result.Result = &txt
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
				// `init` is the canonical session start: model + tool list +
				// MCP server count are interesting for debugging at info
				// level. Hook lifecycle subtypes (hook_started, hook_response,
				// hook_progress) fire repeatedly during a session and would
				// flood the log; route them to debug.
				if m.Subtype == "init" {
					b.Logger.Info("[%s#%d/claude-code] ⚙️  system/init session=%s model=%s tools=%d mcp=%d",
						task.NodeID, task.Iteration, m.SessionID, m.Model, m.ToolCount(), m.MCPServerCount())
					// Capture the effective model the CLI resolved to —
					// after env vars (ANTHROPIC_MODEL, ANTHROPIC_BASE_URL)
					// and settings.json have taken effect. Differs from
					// the workflow-declared `model:` when a proxy (GLM,
					// Kimi, …) or an Anthropic alias is in play.
					if m.Model != "" {
						meta.effectiveModel = m.Model
					}
				} else {
					b.Logger.Debug("[%s#%d/claude-code] ⚙️  system/%s session=%s",
						task.NodeID, task.Iteration, m.Subtype, m.SessionID)
				}
			case *claudesdk.AssistantMessage:
				if m.Message != nil {
					logAssistantContent(b.Logger, task.NodeID, task.Iteration, m.Message.Content)
					emitToolHooks(task.Hooks, m.Message.Content, inFlightTools)
					// Peak prompt size across turns ≈ how full the
					// context window got at its busiest moment.
					u := m.Message.Usage
					load := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
					if load > meta.peakContextLoad {
						meta.peakContextLoad = load
					}
					// Capture the latest non-empty text block — the
					// final assistant message is what the LLM intended
					// as its "answer" and is where it puts the JSON
					// when the recipe asks for one. Overwriting on
					// each AssistantMessage means we always end with
					// the most recent text, mirroring how a human
					// reads the conversation.
					for _, block := range m.Message.Content {
						if tb, ok := block.(*claudesdk.TextBlock); ok && tb.Text != "" {
							lastAssistantText = tb.Text
							// Rate-limit detection: Anthropic forfait
							// surfaces quota exhaustion as a plain assistant
							// text block ("You've hit your limit · resets …")
							// followed by a normal result event with the
							// limit text as Result. Without an early bail,
							// the downstream parseSDKOutput / schema
							// validation turns this into a misleading
							// "missing required field" error. Fail fast
							// with a typed error so the runtime can
							// surface clear "switch provider" guidance.
							if isRateLimitMessage(tb.Text) {
								b.Logger.Warn("[%s#%d/claude-code] 🚦 rate-limit signal in assistant text — aborting: %s", task.NodeID, task.Iteration, truncate(tb.Text, 200))
								cancelStream()
								return result, meta, &ErrRateLimited{Provider: BackendClaudeCode, Detail: strings.TrimSpace(tb.Text)}
							}
						}
					}
				}
			case *claudesdk.UserMessage:
				b.Logger.Debug("[%s#%d/claude-code] 👤 user message echoed back", task.NodeID, task.Iteration)
				if m.Message != nil {
					emitToolHooks(task.Hooks, m.Message.Content, inFlightTools)
				}
			case *claudesdk.ResultMessage:
				result = m
				if (result.Result == nil || *result.Result == "") && lastAssistantText != "" {
					txt := lastAssistantText
					result.Result = &txt
				}
			default:
				if it.msg != nil {
					b.Logger.Debug("[%s#%d/claude-code] 📨 %T message", task.NodeID, task.Iteration, it.msg)
				}
			}
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
