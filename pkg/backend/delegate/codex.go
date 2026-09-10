package delegate

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

//go:embed codex_output_discipline.txt
var codexOutputDisciplinePreamble string

// CodexBackend delegates work to the `codex` CLI (OpenAI Codex)
// via the Codex Agent SDK.
type CodexBackend struct {
	// Command overrides the CLI binary path (default: "codex").
	Command string
	// Logger is the leveled logger for diagnostic output.
	Logger *iterlog.Logger
}

// Execute runs the codex CLI with the given task using the Codex Agent SDK.
func (b *CodexBackend) Execute(ctx context.Context, task Task) (Result, error) {
	if task.Permission.Enabled() {
		return Result{ExitCode: -1, BackendName: BackendCodex}, fmt.Errorf(
			"delegate: codex cannot enforce this node's permission: %s gate; refusing to run ungated",
			task.Permission.Mode,
		)
	}
	if task.Sandbox != nil && task.Sandbox.Driver() != "noop" {
		return Result{ExitCode: -1, BackendName: BackendCodex}, fmt.Errorf(
			"delegate: codex cannot run inside Iterion %s sandbox with the pinned SDK; "+
				"set sandbox: none or use claude_code/claw",
			task.Sandbox.Driver(),
		)
	}
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
	}
	// Every invocation receives an explicit web_search mode, including
	// disabled. Validate both sides of that capability contract and pin the SDK
	// to the exact binary that was probed.
	codexCLIPath, err := validateCodexWebSearchCapability(ctx, b.Command)
	if err != nil {
		return Result{ExitCode: -1, BackendName: BackendCodex}, err
	}

	var opts []codexsdk.Option

	// Preamble teaches frugal tool usage; task prompt follows and may override.
	systemPrompt := codexOutputDisciplinePreamble + task.BuildSystemPrompt()
	if systemPrompt != "" {
		opts = append(opts, codexsdk.WithSystemPrompt(systemPrompt))
	}
	if task.WorkDir != "" {
		opts = append(opts, codexsdk.WithCwd(task.WorkDir))
	}
	if model := strings.TrimPrefix(task.Model, "openai/"); model != "" {
		opts = append(opts, codexsdk.WithModel(model))
	}
	if task.ToolMaxSteps > 0 {
		opts = append(opts, codexsdk.WithMaxTurns(task.ToolMaxSteps))
	}
	// Codex executes its built-in shell without routing tool_use through any
	// SDK callback, so per-name allow/deny at the SDK level has no effect on
	// the shell. Our only real lever is codex's sandbox mode. Translate the
	// AllowedTools intent into the least-privilege sandbox that still lets the
	// node do its job. bypassPermissions skips user-escalation prompts so
	// non-interactive runs don't hang; the explicit Sandbox wins over the
	// permission-mode default via session.go:187.
	sandboxMode := codexSandboxForTask(task)
	opts = append(opts, codexsdk.WithSandbox(sandboxMode))
	opts = append(opts, codexsdk.WithPermissionMode("bypassPermissions"))
	// Codex defaults Web search to cached mode, which would silently expose it
	// to nodes that did not declare the DSL tool. Force the native capability
	// on only for tools: [web_search], and request live (not cached) retrieval.
	opts = append(opts, codexWebSearchOption(task))

	opts = append(opts, codexsdk.WithCliPath(codexCLIPath))

	// Structured output always uses a dedicated formatting pass via session
	// resume. Codex's native tools cannot currently be disabled by Iterion: even
	// a readonly task with no declared tools may read/grep/shell. Applying the
	// schema to the work pass would therefore combine tools + strict output in
	// exactly the mode this split is designed to avoid.
	needsTwoPass := codexNeedsTwoPass(task)
	if len(task.OutputSchema) > 0 && !needsTwoPass {
		opts = append(opts, codexsdk.WithOutputSchema(string(task.OutputSchema)))
	}

	if task.ReasoningEffort != "" {
		opts = append(opts, codexsdk.WithEffort(mapReasoningEffort(task.ReasoningEffort)))
	}

	if task.SessionID != "" {
		opts = append(opts, codexsdk.WithResume(task.SessionID))
		if task.ForkSession {
			opts = append(opts, codexsdk.WithForkSession(true))
		}
	}

	// Stream stderr for live observability and capture for diagnostics.
	var stderrCapture codexStderrCapture
	// Keep credential setup in one helper: structured-output formatting starts a
	// second CLI process and must use the exact same per-run auth source.
	if envOverride := codexCredEnvForCLI(ctx); len(envOverride) > 0 {
		opts = append(opts, codexsdk.WithEnv(envOverride))
	}

	opts = append(opts, codexsdk.WithStderr(func(line string) {
		stderrCapture.AppendLine(line)
		if line != "" {
			b.Logger.Info("[%s#%d/codex:err] %s", task.NodeID, task.Iteration, line)
		}
	}))

	resultMsg, totalDuration, lastThreadID, err := b.runQueryWithRetry(ctx, task, task.UserPrompt, task.Images, opts)
	if err != nil {
		return Result{
			Duration:    totalDuration,
			ExitCode:    -1,
			Stderr:      stderrCapture.String(),
			BackendName: BackendCodex,
		}, err
	}

	if resultMsg == nil {
		diag := inspectCodexRollout(ctx, lastThreadID)
		errMsg := fmt.Sprintf("delegate: codex: no result message received after %d attempts", maxCodexRetries)
		if diag != "" {
			errMsg += " (" + diag + ")"
		}
		return Result{
			Duration:    totalDuration,
			ExitCode:    -1,
			Stderr:      stderrCapture.String(),
			BackendName: BackendCodex,
		}, fmt.Errorf("%s", errMsg)
	}

	result := Result{
		Duration:    totalDuration,
		ExitCode:    0,
		Stderr:      stderrCapture.String(),
		BackendName: BackendCodex,
		SessionID:   resultMsg.SessionID,
	}

	var totalIn, totalOut int
	if resultMsg.Usage != nil {
		totalIn += resultMsg.Usage.InputTokens
		totalOut += resultMsg.Usage.OutputTokens
	}
	result.Tokens = totalIn + totalOut

	if resultMsg.IsError && resultMsg.Subtype != "success" {
		return result, fmt.Errorf("delegate: codex error: subtype=%s", resultMsg.Subtype)
	}
	if terminalErr := codexTerminalFailure(resultMsg, result.Stderr); terminalErr != nil {
		return result, terminalErr
	}

	// Pass 1 output is free-form text. Pass 2 uses WithOutputSchema + read-only
	// sandbox (no writes during formatting) to guarantee structured output via
	// session resume.
	if needsTwoPass {
		if resultMsg.SessionID == "" {
			return result, fmt.Errorf("delegate: codex structured output requires a resumable session, but the work pass returned no session ID")
		}
		const maxFmtAttempts = 2
		for attempt := 1; attempt <= maxFmtAttempts; attempt++ {
			b.Logger.Debug("codex [formatting pass %d/%d] starting structured output extraction (session=%s)", attempt, maxFmtAttempts, resultMsg.SessionID)
			fmtRM, fmtDuration, fmtErr := b.formatOutput(ctx, task, resultMsg.SessionID, codexCLIPath)
			result.Duration += fmtDuration
			if fmtErr != nil {
				if attempt < maxFmtAttempts {
					b.Logger.Warn("codex [formatting pass %d/%d] failed, retrying: %v", attempt, maxFmtAttempts, fmtErr)
					continue
				}
				return result, fmt.Errorf("delegate: codex formatting pass failed: %w", fmtErr)
			}
			if fmtRM.Usage != nil {
				totalIn += fmtRM.Usage.InputTokens
				totalOut += fmtRM.Usage.OutputTokens
				result.Tokens = totalIn + totalOut
			}
			result.FormattingPassUsed = true

			output, rawLen, fallback := parseSDKOutput(fmtRM.Result, fmtRM.StructuredOutput, task.OutputSchema)
			if (len(output) == 0 || fallback) && attempt < maxFmtAttempts {
				b.Logger.Warn("codex [formatting pass %d/%d] produced empty/fallback output, retrying", attempt, maxFmtAttempts)
				continue
			}
			if len(output) == 0 {
				return result, fmt.Errorf("delegate: codex formatting pass returned empty structured output")
			}
			if fallback {
				return result, fmt.Errorf("delegate: codex formatting pass did not return schema-conforming JSON")
			}
			result.Output = output
			result.RawOutputLen = rawLen
			result.ParseFallback = fallback
			cost.Annotate(result.Output, task.Model, totalIn, totalOut)
			return result, nil
		}
		return result, fmt.Errorf("delegate: codex formatting pass exhausted without a result")
	}

	output, rawLen, fallback := parseSDKOutput(resultMsg.Result, resultMsg.StructuredOutput, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	if len(result.Output) == 0 {
		return result, fmt.Errorf("delegate: codex returned empty output")
	}

	cost.Annotate(result.Output, task.Model, totalIn, totalOut)
	return result, nil
}

const maxCodexRetries = 3

// runQueryWithRetry drives codex Query to completion, retrying up to
// maxCodexRetries when the process exits without producing a ResultMessage
// (a known transient failure mode).
func (b *CodexBackend) runQueryWithRetry(ctx context.Context, task Task, prompt string, images []string, opts []codexsdk.Option) (*codexsdk.ResultMessage, time.Duration, string, error) {
	var totalDuration time.Duration
	var lastThreadID string
	content := codexQueryContent(prompt, images)

	for attempt := 1; attempt <= maxCodexRetries; attempt++ {
		startTime := time.Now()
		var resultMsg *codexsdk.ResultMessage
		var queryErr error
		inFlightTools := make(map[string]string)

		for msg, err := range codexsdk.Query(ctx, content, opts...) {
			if err != nil {
				queryErr = err
				break
			}
			switch m := msg.(type) {
			case *codexsdk.AssistantMessage:
				emitCodexToolHooks(task.Hooks, m, inFlightTools)
				b.logAssistantActivity(task.NodeID, task.Iteration, m)
			case *codexsdk.ResultMessage:
				resultMsg = m
			case *codexsdk.SystemMessage:
				switch m.Subtype {
				case "thread.started":
					if tid, ok := m.Data["thread_id"].(string); ok && tid != "" {
						lastThreadID = tid
					}
				case "thread.token_usage.updated":
					b.logTokenUsage(task.NodeID, task.Iteration, m.Data)
				}
			}
		}

		totalDuration += time.Since(startTime)

		if queryErr != nil {
			// A transport error surfaced after the result (e.g. a read
			// failure during shutdown) concerns the teardown, not the
			// work: the completed turn wins.
			if resultMsg != nil {
				b.Logger.Warn("[%s#%d/codex] ignoring transport error received after result: %v", task.NodeID, task.Iteration, queryErr)

				return resultMsg, totalDuration, lastThreadID, nil
			}

			return nil, totalDuration, lastThreadID, fmt.Errorf("delegate: codex failed: %w", queryErr)
		}

		if resultMsg != nil {
			return resultMsg, totalDuration, lastThreadID, nil
		}

		// No ResultMessage: inspect the rollout log to classify the failure.
		// Overflow is not retryable — same prompt will overflow again and
		// burn tokens. Break with a clear error so the caller fails fast.
		diag := inspectCodexRollout(ctx, lastThreadID)
		if strings.Contains(diag, "context window") {
			return nil, totalDuration, lastThreadID, fmt.Errorf("delegate: codex: %s", diag)
		}

		if attempt < maxCodexRetries {
			// Exponential backoff: 1s, 2s, 4s … capped at 8s. Without
			// this, three retries fire back-to-back in microseconds on
			// rate-limit / 5xx errors, hammering the API without giving
			// it any recovery window.
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, totalDuration, lastThreadID, fmt.Errorf("delegate: codex: context cancelled during retry: %w", ctx.Err())
			case <-time.After(backoff):
			}
			if diag != "" {
				b.Logger.Warn("[%s#%d/codex] returned no result (attempt %d/%d, %s), retrying after %s", task.NodeID, task.Iteration, attempt, maxCodexRetries, diag, backoff)
			} else {
				b.Logger.Warn("[%s#%d/codex] returned no result (attempt %d/%d), retrying after %s", task.NodeID, task.Iteration, attempt, maxCodexRetries, backoff)
			}
		}
	}

	// All attempts exhausted with no result and no terminal error from
	// codex itself — surface that explicitly so the caller's retry
	// classifier can distinguish this from a clean exit.
	return nil, totalDuration, lastThreadID, fmt.Errorf("delegate: codex: no result after %d attempts", maxCodexRetries)
}

func codexQueryContent(prompt string, images []string) codexsdk.UserMessageContent {
	if len(images) == 0 {
		return codexsdk.Text(prompt)
	}

	blocks := make([]codexsdk.ContentBlock, 0, len(images)+1)
	blocks = append(blocks, codexsdk.TextInput(prompt))
	for _, path := range images {
		// The forked SDK marshals this to the localImage discriminator
		// required by the live app-server (see the go.mod replace).
		blocks = append(blocks, codexsdk.LocalImageInput(path))
	}

	return codexsdk.Blocks(blocks...)
}

// formatOutput performs a second pass: resumes the work-pass session with
// WithOutputSchema and a tight formatting prompt. Sandbox is forced to
// read-only so the pass cannot mutate state while rendering the final JSON.
func (b *CodexBackend) formatOutput(ctx context.Context, task Task, sessionID, codexCLIPath string) (*codexsdk.ResultMessage, time.Duration, error) {
	var stderrCapture codexStderrCapture
	opts := []codexsdk.Option{
		codexsdk.WithResume(sessionID),
		codexsdk.WithOutputSchema(string(task.OutputSchema)),
		codexsdk.WithSandbox("read-only"),
		codexsdk.WithPermissionMode("bypassPermissions"),
		// Formatting is intentionally tool-free work. Do not inherit Codex's
		// cached-search default (or the work pass's live-search capability).
		codexsdk.WithConfig(map[string]string{"web_search": codexWebSearchModeDisabled}),
		codexsdk.WithStderr(func(line string) {
			stderrCapture.AppendLine(line)
			if line != "" {
				b.Logger.Info("[%s#%d/fmt] %s", task.NodeID, task.Iteration, line)
			}
		}),
	}
	if task.WorkDir != "" {
		opts = append(opts, codexsdk.WithCwd(task.WorkDir))
	}
	if model := strings.TrimPrefix(task.Model, "openai/"); model != "" {
		opts = append(opts, codexsdk.WithModel(model))
	}
	opts = append(opts, codexsdk.WithCliPath(codexCLIPath))
	if task.ReasoningEffort != "" {
		opts = append(opts, codexsdk.WithEffort(mapReasoningEffort(task.ReasoningEffort)))
	}
	if envOverride := codexCredEnvForCLI(ctx); len(envOverride) > 0 {
		opts = append(opts, codexsdk.WithEnv(envOverride))
	}

	prompt := "Format your complete findings as JSON matching the required output schema. Do not call any tools; just return the JSON."

	rm, duration, _, err := b.runQueryWithRetry(ctx, task, prompt, nil, opts)
	if err != nil {
		return nil, duration, err
	}
	if rm == nil {
		return nil, duration, fmt.Errorf("codex formatting pass: no result message")
	}
	if rm.IsError && rm.Subtype != "success" {
		return rm, duration, fmt.Errorf("codex formatting pass error: subtype=%s", rm.Subtype)
	}
	if terminalErr := codexTerminalFailure(rm, stderrCapture.String()); terminalErr != nil {
		return rm, duration, terminalErr
	}
	return rm, duration, nil
}

// logTokenUsage extracts totals from a thread.token_usage.updated event and
// logs them live. Codex emits this a few times per turn; surfacing it lets
// operators see context growth before a silent overflow (inspectCodexRollout
// remains the post-mortem safety net). Data shape: tokenUsage.last.{total,input,cached,output,reasoning}Tokens.
func (b *CodexBackend) logTokenUsage(nodeID string, iteration int, data map[string]any) {
	tu, ok := data["tokenUsage"].(map[string]any)
	if !ok {
		return
	}
	last, ok := tu["last"].(map[string]any)
	if !ok {
		return
	}
	total := asInt(last["totalTokens"])
	input := asInt(last["inputTokens"])
	cached := asInt(last["cachedInputTokens"])
	output := asInt(last["outputTokens"])
	reasoning := asInt(last["reasoningOutputTokens"])
	if total == 0 && input == 0 && output == 0 {
		return
	}
	b.Logger.Info("[%s#%d/codex] 📊 tokens total=%d (input=%d cached=%d output=%d reasoning=%d)",
		nodeID, iteration, total, input, cached, output, reasoning)
}

func asInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}

// logAssistantActivity logs tool calls, text output, and tool errors from an
// AssistantMessage in real-time, mirroring the claude_code backend's activity
// streaming.
func (b *CodexBackend) logAssistantActivity(nodeID string, iteration int, msg *codexsdk.AssistantMessage) {
	for _, block := range msg.Content {
		switch blk := block.(type) {
		case *codexsdk.ToolUseBlock:
			header := fmt.Sprintf("[%s#%d/codex] 🔧 %s %s", nodeID, iteration, blk.Name, toolUseDetail(blk.Name, blk.Input))
			b.Logger.LogBlock(iterlog.LevelInfo, "ℹ️ ", header, toolUseBody(blk.Name, blk.Input))
		case *codexsdk.ToolResultBlock:
			if blk.IsError {
				b.Logger.Info("[%s#%d/codex] ❌ tool error: %s", nodeID, iteration, contentBlocksText(blk.Content))
			}
		case *codexsdk.TextBlock:
			if blk.Text != "" {
				// LogBlock so the assistant text folds in the studio's
				// run log; full content, no truncation.
				b.Logger.LogBlock(iterlog.LevelInfo, "ℹ️ ",
					fmt.Sprintf("[%s#%d/codex] 💬", nodeID, iteration),
					blk.Text)
			}
		}
	}
}

// inspectCodexRollout returns a short diagnostic pulled from the last event
// of ~/.codex/sessions/.../rollout-*-<threadID>.jsonl. Used when codex exits
// without sending turn.completed/turn.failed (e.g. context-window overflow).
// Returns "" when nothing useful can be extracted.
func inspectCodexRollout(ctx context.Context, threadID string) string {
	if threadID == "" {
		return ""
	}
	codexHome := ""
	if env := codexCredEnvForCLI(ctx); env != nil {
		codexHome = env["CODEX_HOME"]
	}
	if codexHome == "" {
		codexHome = os.Getenv("CODEX_HOME")
	}
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		codexHome = filepath.Join(home, ".codex")
	}
	// Codex writes rollouts to ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<thread_id>.jsonl.
	pattern := filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*-"+threadID+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return ""
	}
	// With a unique thread_id there should be exactly one match; pick the
	// first defensively.
	path := matches[0]

	// Read the last non-empty JSONL event. Small files — full scan is fine.
	f, err := os.Open(path) // #nosec G304 — path is built from a thread_id we just saw come out of the SDK and a fixed ~/.codex/sessions prefix.
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	var last map[string]any
	scanner := bufio.NewScanner(f)
	// Allow large lines (tool outputs can be big).
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err == nil {
			last = ev
		}
	}
	if last == nil {
		return ""
	}

	// Pull out the payload type and check for a context-window overflow.
	payload, _ := last["payload"].(map[string]any)
	pType, _ := payload["type"].(string)
	evType, _ := last["type"].(string)

	if evType == "event_msg" && pType == "token_count" {
		info, _ := payload["info"].(map[string]any)
		tot, _ := info["total_token_usage"].(map[string]any)
		total, _ := tot["total_tokens"].(float64)
		window, _ := info["model_context_window"].(float64)
		if total > 0 && window > 0 && total > window {
			return fmt.Sprintf("codex likely hit context window: total_tokens=%d > model_context_window=%d; reduce prompt size or use a larger-context model", int(total), int(window))
		}
		return fmt.Sprintf("codex exited without completion; last event was token_count (total_tokens=%d, window=%d)", int(total), int(window))
	}
	if evType != "" || pType != "" {
		return fmt.Sprintf("codex exited without completion; last rollout event was %s/%s", evType, pType)
	}
	return ""
}

// contentBlocksText flattens a ContentBlock slice for logging; truncates to 500 chars.
func contentBlocksText(blocks []codexsdk.ContentBlock) string {
	if len(blocks) == 0 {
		return "<empty>"
	}
	var sb strings.Builder
	for i, blk := range blocks {
		if i > 0 {
			sb.WriteString(" | ")
		}
		switch b := blk.(type) {
		case *codexsdk.TextBlock:
			sb.WriteString(b.Text)
		default:
			fmt.Fprintf(&sb, "<%s>", blk.BlockType())
		}
	}
	return truncate(sb.String(), 500)
}

// codexStderrCapture is safe for SDK callbacks, which may run on their own
// stderr-draining goroutine while the query loop reads the accumulated text.
type codexStderrCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *codexStderrCapture) AppendLine(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.WriteString(line)
	c.buf.WriteString("\n")
}

func (c *codexStderrCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// codexCredEnvForCLI resolves the per-run Codex credential environment. Keep
// this shared by the work and formatting passes: the latter resumes the first
// pass in a new CLI process and otherwise loses tenant-scoped auth.
func codexCredEnvForCLI(ctx context.Context) map[string]string {
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok {
		return nil
	}
	if key := creds.APIKey(secrets.ProviderOpenAI); key != "" {
		return map[string]string{
			"OPENAI_API_KEY": key,
			"CODEX_API_KEY":  "",
		}
	}
	if dir := creds.OAuthDir(string(secrets.OAuthKindCodex)); dir != "" {
		// A shared runner key must not shadow the per-run ChatGPT OAuth bundle.
		return map[string]string{
			"CODEX_HOME":     dir,
			"OPENAI_API_KEY": "",
			"CODEX_API_KEY":  "",
		}
	}
	return nil
}

// codexTerminalFailure recognises the SDK failure mode where Codex emits a
// nominal ResultMessage whose text is actually a terminal CLI/API error.
// Without this guard a single-string schema can wrap that text as a valid
// result, or an empty result can look like a successful side-effect-only task.
func codexTerminalFailure(rm *codexsdk.ResultMessage, stderr string) error {
	if rm == nil {
		return nil
	}
	// A schema-validated payload is authoritative. Stale stderr or a human-facing
	// textual summary must never turn a valid structured result into a failure.
	if hasNonEmptyStructuredOutput(rm.StructuredOutput) {
		return nil
	}
	resultText := ""
	if rm.Result != nil {
		resultText = strings.TrimSpace(*rm.Result)
	}
	if isCodexBareLimitNotice(resultText) {
		return &ErrRateLimited{Provider: BackendCodex, Detail: resultText}
	}
	if resultText != "" && hasCodexTerminalErrorEnvelope(resultText) {
		lower := strings.ToLower(resultText)
		if isCodexRateLimitError(lower) {
			return &ErrRateLimited{Provider: BackendCodex, Detail: resultText}
		}
		if isCodexTerminalNetworkError(lower) {
			return &ErrTransient{Provider: BackendCodex, Reason: "network", Detail: resultText}
		}
		if isCodexAuthOrAPIStatusError(lower) {
			return fmt.Errorf("delegate: codex terminal error: %s", truncate(resultText, 500))
		}
	}
	if resultText == "" && MatchesNetworkSignature(stderr) {
		return &ErrTransient{
			Provider: BackendCodex,
			Reason:   "network",
			Detail:   truncate(strings.TrimSpace(stderr), 500),
		}
	}
	return nil
}

// isCodexBareLimitNotice covers short provider notices that the CLI can expose
// as a nominal ResultMessage without an "Error:" envelope. Prefix matching plus
// a tight length cap avoids classifying normal agent discussion of quotas or
// capacity as a terminal failure.
func isCodexBareLimitNotice(text string) bool {
	if len(text) == 0 || len(text) > 300 {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, prefix := range []string{
		"you've hit your usage limit",
		"you’ve hit your usage limit",
		"you have hit your usage limit",
		"you've hit your limit",
		"you’ve hit your limit",
		"usage limit reached",
		"rate limit exceeded",
		"quota exceeded",
		"selected model is at capacity",
		"we're currently experiencing high demand",
		"we’re currently experiencing high demand",
		"we are currently experiencing high demand",
	} {
		if strings.HasPrefix(lower, prefix) {
			rest := strings.TrimSpace(strings.TrimPrefix(lower, prefix))
			first, _ := utf8.DecodeRuneInString(rest)
			if rest == "" || strings.ContainsRune(".!:;·—-…", first) {
				return true
			}
		}
	}
	// The enumerated prefixes carry no qualifier, so every inserted-noun
	// variant ("… your weekly limit", "… your org's monthly spend limit")
	// escapes them — the same masking bug the claude_code detector was
	// widened for. hitYourLimitRe subsumes the whole family; anchoring it
	// to a leading "you(')ve hit your" keeps the prefix discipline that
	// stops an agent quoting a limit mid-paragraph from qualifying.
	for _, opener := range []string{"you've hit your", "you’ve hit your", "you have hit your"} {
		if strings.HasPrefix(lower, opener) && hitYourLimitRe.MatchString(lower) {
			return true
		}
	}
	return false
}

func isCodexRateLimitError(lower string) bool {
	return strings.Contains(lower, "unexpected status 429") ||
		strings.Contains(lower, "rate limit exceeded") ||
		strings.Contains(lower, "you've hit your usage limit") ||
		strings.Contains(lower, "usage limit reached") ||
		strings.Contains(lower, "quota exceeded") ||
		strings.Contains(lower, "insufficient_quota")
}

func isCodexTerminalNetworkError(lower string) bool {
	for _, signature := range []string{
		"stream disconnected", "error sending request", "unable to connect to api",
		"connection refused", "connection reset", "connection closed before",
		"unexpected eof", "tls handshake timeout", "temporary failure in name resolution",
		"unexpected status 500", "unexpected status 502", "unexpected status 503",
		"unexpected status 504",
	} {
		if strings.Contains(lower, signature) {
			return true
		}
	}
	return false
}

// hasCodexTerminalErrorEnvelope limits classification to the shapes emitted by
// the CLI itself. Ordinary task output such as "Failed tests: ..." or
// "Error handling strategy: ..." must remain valid content.
func hasCodexTerminalErrorEnvelope(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, prefix := range []string{
		"error:", "fatal:", "api error:", "stream disconnected",
		"connection closed", "connection reset", "connection refused",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func isCodexAuthOrAPIStatusError(lower string) bool {
	return strings.Contains(lower, "unexpected status 401") ||
		strings.Contains(lower, "unexpected status 403") ||
		strings.Contains(lower, "401 unauthorized") ||
		strings.Contains(lower, "403 forbidden") ||
		strings.Contains(lower, "authentication failed") ||
		strings.Contains(lower, "missing bearer or basic authentication") ||
		strings.Contains(lower, "unexpected status 4") ||
		strings.Contains(lower, "unexpected status 5")
}

func hasNonEmptyStructuredOutput(value any) bool {
	if value == nil {
		return false
	}
	if obj, ok := value.(map[string]any); ok {
		return len(obj) > 0
	}
	b, err := json.Marshal(value)
	return err == nil && string(b) != "null" && string(b) != "{}" && string(b) != "[]"
}

// codexSandboxForAllowedTools picks the least-privilege codex sandbox mode
// compatible with the intent expressed by a non-empty AllowedTools list.
// Iterion accepts both Claude-style TitleCase names and its native snake_case
// aliases, so normalise before deciding whether filesystem mutation is needed.
//
// "danger-full-access" is intentionally never chosen here — workflows that
// truly need unrestricted network or out-of-workspace writes must request it
// with the node-level `full_access: true`, not by listing a mutating tool.
func codexSandboxForAllowedTools(allowed []string) string {
	for _, t := range allowed {
		if isCodexWebSearchTool(t) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "read", "read_file", "readfile", "cat", "glob", "grep", "ls":
			continue
		case "bash", "shell", "sh",
			"edit", "edit_file", "file_edit", "multiedit", "str_replace",
			"write", "write_file", "writefile",
			"notebookedit", "notebook_edit", "patch", "apply_patch", "run_command":
			return "workspace-write"
		default:
			// Unknown/custom tool names cannot prove the task is read-only. Prefer
			// preserving writer semantics; explicit `readonly:` remains the lock.
			return "workspace-write"
		}
	}
	return "read-only"
}

// codexSandboxForTask maps the DSL's access intent to Codex. readonly is the
// explicit lock-down and wins over a conflicting full_access opt-in.
// With neither flag, an empty tools list means "native toolset unrestricted"
// throughout Iterion, so Codex must receive workspace-write rather than silently
// changing that contract to read-only. A restricted list is classified by name.
func codexSandboxForTask(task Task) string {
	if task.Readonly {
		return "read-only"
	}
	if task.FullAccess {
		return "danger-full-access"
	}
	if len(task.AllowedTools) == 0 {
		return "workspace-write"
	}
	return codexSandboxForAllowedTools(task.AllowedTools)
}

func codexNeedsTwoPass(task Task) bool {
	return len(task.OutputSchema) > 0
}

// mapReasoningEffort converts iterion reasoning effort strings to Codex SDK Effort constants.
// Codex only supports low/medium/high/max — xhigh maps down to high (matching the
// "fall back to highest supported at or below" convention used by Claude Code).
func mapReasoningEffort(s string) codexsdk.Effort {
	switch s {
	case "low":
		return codexsdk.EffortLow
	case "medium":
		return codexsdk.EffortMedium
	case "high", "xhigh":
		return codexsdk.EffortHigh
	case "max":
		return codexsdk.EffortMax
	default:
		return codexsdk.EffortMedium
	}
}
