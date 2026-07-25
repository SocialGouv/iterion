package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// CLIAgentProtocol declares how to drive a third-party agent CLI whose
// argument protocol is NOT claude-code-compatible (see ADR-065). Unlike the
// per-node `command:` override (#76) — which only swaps the binary while
// keeping claude-code's Session-mode argv (`--print`, `--input-format
// stream-json`, prompt on stdin, …) — a protocol describes the CLI's *own*
// invocation. It is the seam a real backend (e.g. Moonshot's `kimi-code`,
// `kimi -p <prompt> --output-format stream-json -m <alias>`) plugs into.
//
// The zero value is not usable; construct one per target CLI (see kimi.go)
// and register a CLIAgentBackend wrapping it.
type CLIAgentProtocol struct {
	// Name is the backend registration name and the label stamped on
	// Result.BackendName / log lines (e.g. "kimi").
	Name string

	// DefaultBinary is the CLI executable to invoke when the backend's
	// Command override is empty (e.g. "kimi").
	DefaultBinary string

	// PromptFlag delivers the user prompt on the command line. When
	// PromptViaStdin is false, the prompt is passed as the flag's value:
	// `<PromptFlag> <prompt>` (e.g. "-p <prompt>"). When PromptViaStdin is
	// true and PromptFlag is set, PromptFlag is emitted as a bare switch and
	// the prompt is written to the process stdin instead. Empty PromptFlag +
	// PromptViaStdin means "prompt straight to stdin, no flag".
	PromptFlag     string
	PromptViaStdin bool

	// SystemPromptFlag, when set, delivers the composed system prompt as
	// `<SystemPromptFlag> <system>`. When empty (the common case for CLIs
	// with no system-prompt flag, incl. kimi-code), the system prompt is
	// folded in as a preamble to the user prompt so the node's `system:`
	// task still reaches the agent.
	SystemPromptFlag string

	// ModelFlag maps Task.Model onto the CLI's own model selector, emitted as
	// `<ModelFlag> <MapModel(model)>` when both ModelFlag and the mapped model
	// are non-empty. MapModel translates iterion's `provider/model` spec into
	// the CLI's native alias; nil passes Model through verbatim.
	ModelFlag string
	MapModel  func(model string) string

	// OutputFormatFlag + OutputFormat request a machine-readable stream from
	// the CLI (e.g. "--output-format" "stream-json"). Empty OutputFormatFlag
	// leaves the CLI at its default (text) format.
	OutputFormatFlag string
	OutputFormat     string

	// MapEffort maps an iterion reasoning-effort level ("low".."max") onto
	// extra argv the CLI understands. Return an empty slice for an effort the
	// CLI can't express; a nil MapEffort drops effort entirely (the CLI has no
	// effort dial).
	MapEffort func(effort string) []string

	// ExtraArgs are appended verbatim after all generated flags (static CLI
	// switches the protocol always wants, e.g. a non-interactive flag).
	ExtraArgs []string

	// ParseOutput extracts the assistant's final text (plus optional session
	// id and token count) from the CLI's raw stdout. For a stream-json
	// protocol it walks the NDJSON events; for a text protocol it returns
	// stdout verbatim. The returned text is then fed through the shared
	// schema-aware fallback (parseSDKOutput) so structured `output:` schemas
	// work the same way they do for the other backends. nil defaults to
	// treating stdout as plain text.
	ParseOutput func(stdout string) (text, sessionID string, tokens int)

	// ResolveEnv returns credential/endpoint environment overrides sourced
	// from the target CLI's own config/env conventions (and any per-run
	// injected credentials on the context). The returned map is layered on
	// top of the inherited process environment. nil means "inherit the host
	// environment unchanged" — the CLI resolves its own credentials, which is
	// the correct default for a CLI that reads e.g. $MOONSHOT_API_KEY itself.
	ResolveEnv func(ctx context.Context) map[string]string
}

// CLIAgentBackend is a delegate.Backend that drives a third-party agent CLI
// described by a CLIAgentProtocol. It mirrors the codex backend's shape —
// build native argv, run with a wall-clock timeout, parse stdout, retry on a
// no-output transient — but the invocation is the target CLI's own, not
// claude-code's. See ADR-065.
type CLIAgentBackend struct {
	// Protocol describes the target CLI's argument protocol.
	Protocol CLIAgentProtocol
	// Command overrides Protocol.DefaultBinary (the per-node `command:`
	// override / an operator-pinned build path).
	Command string
	// Timeout caps a single CLI invocation's wall-clock time. Zero uses
	// defaultCLIAgentTimeout. The context deadline (run budget) still applies
	// and wins when tighter.
	Timeout time.Duration
	// Logger is the leveled logger for diagnostic output.
	Logger *iterlog.Logger
}

const (
	maxCLIAgentRetries     = 3
	defaultCLIAgentTimeout = 20 * time.Minute
)

// Execute runs the target CLI for the given task and parses its output into a
// delegate.Result.
func (b *CLIAgentBackend) Execute(ctx context.Context, task Task) (Result, error) {
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
	}
	proto := b.Protocol
	backendName := proto.Name
	if backendName == "" {
		backendName = "cli_agent"
	}

	binary := b.Command
	if binary == "" {
		binary = proto.DefaultBinary
	}
	if binary == "" {
		return Result{BackendName: backendName, ExitCode: -1},
			fmt.Errorf("delegate: %s: no CLI binary configured (set command: or protocol DefaultBinary)", backendName)
	}

	systemPrompt := task.BuildSystemPrompt()
	userPrompt := task.UserPrompt

	// When the CLI exposes no system-prompt flag, fold the composed system
	// prompt in as a preamble so the node's task still reaches the agent.
	promptArg := userPrompt
	if proto.SystemPromptFlag == "" && systemPrompt != "" {
		if promptArg != "" {
			promptArg = systemPrompt + "\n\n" + promptArg
		} else {
			promptArg = systemPrompt
		}
	}

	args, stdinPrompt := b.buildArgs(proto, task, promptArg, systemPrompt)

	env := os.Environ()
	if proto.ResolveEnv != nil {
		for k, v := range proto.ResolveEnv(ctx) {
			env = append(env, k+"="+v)
		}
	}
	// Run-level provisioning (devbox profile PATH) — appended last so on
	// a duplicate key the run-level value wins.
	env = append(env, task.ExtraEnv...)

	timeout := b.Timeout
	if timeout <= 0 {
		timeout = defaultCLIAgentTimeout
	}

	start := time.Now()
	stdout, stderr, exitCode, err := b.runWithRetry(ctx, task, binary, args, stdinPrompt, env, timeout)
	duration := time.Since(start)

	result := Result{
		Duration:    duration,
		ExitCode:    exitCode,
		Stderr:      truncate(stderr, 8192),
		BackendName: backendName,
	}
	if err != nil {
		return result, err
	}

	// Parse the CLI's stdout into the assistant's final text, then apply the
	// shared schema-aware fallback so `output:` schemas behave uniformly.
	parse := proto.ParseOutput
	if parse == nil {
		parse = func(s string) (string, string, int) { return s, "", 0 }
	}
	text, sessionID, tokens := parse(stdout)
	result.SessionID = sessionID
	result.Tokens = tokens

	output, rawLen, fallback := parseSDKOutput(&text, nil, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	cost.Annotate(result.Output, task.Model, 0, tokens)
	return result, nil
}

// buildArgs assembles the target CLI's argv from the protocol + task. It
// returns the argument slice (excluding the binary itself) and the prompt to
// pipe on stdin (empty unless the protocol uses stdin delivery).
func (b *CLIAgentBackend) buildArgs(proto CLIAgentProtocol, task Task, promptArg, systemPrompt string) (args []string, stdinPrompt string) {
	if proto.PromptViaStdin {
		if proto.PromptFlag != "" {
			args = append(args, proto.PromptFlag)
		}
		stdinPrompt = promptArg
	} else if proto.PromptFlag != "" {
		args = append(args, proto.PromptFlag, promptArg)
	}

	if proto.OutputFormatFlag != "" && proto.OutputFormat != "" {
		args = append(args, proto.OutputFormatFlag, proto.OutputFormat)
	}

	if proto.ModelFlag != "" && task.Model != "" {
		model := task.Model
		if proto.MapModel != nil {
			model = proto.MapModel(model)
		}
		if model != "" {
			args = append(args, proto.ModelFlag, model)
		}
	}

	if proto.SystemPromptFlag != "" && systemPrompt != "" {
		args = append(args, proto.SystemPromptFlag, systemPrompt)
	}

	if proto.MapEffort != nil && task.ReasoningEffort != "" {
		args = append(args, proto.MapEffort(task.ReasoningEffort)...)
	}

	args = append(args, proto.ExtraArgs...)
	return args, stdinPrompt
}

// runWithRetry runs the CLI to completion, retrying up to maxCLIAgentRetries
// when the process exits successfully but produces no stdout (a known
// transient failure mode for streaming agent CLIs) or fails with a
// network-signature error. Overflow/quota/fatal exits are surfaced fast.
func (b *CLIAgentBackend) runWithRetry(ctx context.Context, task Task, binary string, args []string, stdinPrompt string, env []string, timeout time.Duration) (stdout, stderr string, exitCode int, err error) {
	for attempt := 1; attempt <= maxCLIAgentRetries; attempt++ {
		stdout, stderr, exitCode, err = b.runOnce(ctx, task, binary, args, stdinPrompt, env, timeout)

		if err == nil && strings.TrimSpace(stdout) != "" {
			return stdout, stderr, exitCode, nil
		}

		// Context cancelled/deadline: not retryable, surface immediately.
		if ctx.Err() != nil {
			return stdout, stderr, exitCode, fmt.Errorf("delegate: %s: context ended: %w", b.Protocol.Name, ctx.Err())
		}

		transient := err != nil && (MatchesNetworkSignature(err.Error()) || MatchesNetworkSignature(stderr))
		empty := err == nil && strings.TrimSpace(stdout) == ""
		if !transient && !empty {
			// A genuine non-transient failure (bad flag, auth error, …):
			// don't burn retries on a deterministic failure.
			return stdout, stderr, exitCode, err
		}

		if attempt < maxCLIAgentRetries {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			reason := "no output"
			if transient {
				reason = "transient error"
			}
			if b.Logger != nil {
				b.Logger.Warn("[%s#%d/%s] %s (attempt %d/%d), retrying after %s",
					task.NodeID, task.Iteration, b.Protocol.Name, reason, attempt, maxCLIAgentRetries, backoff)
			}
			select {
			case <-ctx.Done():
				return stdout, stderr, exitCode, fmt.Errorf("delegate: %s: context ended during retry: %w", b.Protocol.Name, ctx.Err())
			case <-time.After(backoff):
			}
		}
	}

	if err != nil {
		return stdout, stderr, exitCode, &ErrTransient{
			Provider: b.Protocol.Name,
			Reason:   "cli agent failed",
			Detail:   fmt.Sprintf("%v after %d attempts", err, maxCLIAgentRetries),
		}
	}
	return stdout, stderr, exitCode, fmt.Errorf("delegate: %s: no output after %d attempts", b.Protocol.Name, maxCLIAgentRetries)
}

// runOnce executes a single CLI invocation, on the host or (when the task is
// sandboxed) inside the run's container, honouring the wall-clock timeout.
func (b *CLIAgentBackend) runOnce(ctx context.Context, task Task, binary string, args []string, stdinPrompt string, env []string, timeout time.Duration) (stdout, stderr string, exitCode int, err error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var outBuf, errBuf bytes.Buffer
	argv := append([]string{binary}, args...)

	// A stdin-delivery protocol (PromptViaStdin) must route its prompt
	// through the sandbox ExecOpts, NOT by setting cmd.Stdin after the
	// driver builds the command: the docker/k8s drivers only allocate a
	// forwarded stdin (`docker exec --interactive`) when opts.Stdin (or
	// KeepStdinOpen) is set, so a post-hoc cmd.Stdin would be silently
	// dropped inside the container.
	var stdin io.Reader
	if stdinPrompt != "" {
		stdin = strings.NewReader(stdinPrompt)
	}

	var cmd *exec.Cmd
	if task.Sandbox != nil {
		// Record the in-container PID so cancellation/timeout can actually
		// terminate the agent: killing the host-side `docker exec` client
		// has no signal path to the exec'd process (same leak class as
		// native:221edac8 on the claude_code path — the fix there,
		// pidfile wrapper + explicit in-container kill, applies verbatim).
		// The wrapper also self-cleans on retry: attempt N kills whatever
		// attempt N-1 the pidfile still points to.
		mark := sandboxDelegateMark(task)
		cleanup := killSandboxDelegate(task.Sandbox, mark, b.Logger)
		defer cleanup()
		cmd = task.Sandbox.Command(runCtx, wrapSandboxDelegateArgv(mark, argv), sandbox.ExecOpts{
			Env:     envSliceToMap(b.Protocol.ResolveEnv, ctx),
			WorkDir: task.WorkDir,
			Stdin:   stdin,
		})
	} else {
		cmd = exec.CommandContext(runCtx, binary, args...) // #nosec G204 — binary/args are backend-configured, not attacker-controlled.
		cmd.Env = env
		if task.WorkDir != "" {
			cmd.Dir = task.WorkDir
		}
		cmd.Stdin = stdin
	}
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()
	if stderr != "" && b.Logger != nil {
		for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
			if line != "" {
				b.Logger.Info("[%s#%d/%s:err] %s", task.NodeID, task.Iteration, b.Protocol.Name, line)
			}
		}
	}
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if runErr != nil {
		return stdout, stderr, exitCode, fmt.Errorf("delegate: %s: %s failed: %w", b.Protocol.Name, binary, runErr)
	}
	return stdout, stderr, exitCode, nil
}

// envSliceToMap re-derives the ResolveEnv override map for the sandbox ExecOpts
// path (the container already inherits its own base env; we only layer the
// protocol's credential/endpoint overrides on top).
func envSliceToMap(resolve func(context.Context) map[string]string, ctx context.Context) map[string]string {
	if resolve == nil {
		return nil
	}
	return resolve(ctx)
}

// parseStreamJSONText walks both the legacy claude-code-style NDJSON stream
// (`type: assistant` + terminal `type: result`) and kimi-code 0.23+'s native
// role stream (`role: assistant`, followed by a `role: meta` resume hint).
// It extracts the assistant's final text, session id, and token usage. Unknown
// lines are skipped; when nothing parses it falls back to the raw stream.
func parseStreamJSONText(stdout string) (text, sessionID string, tokens int) {
	var legacyAssistantText strings.Builder
	var nativeAssistantText string
	var resultText string
	haveResult := false

	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev == nil {
			continue
		}
		if sid, ok := ev["session_id"].(string); ok && sid != "" {
			sessionID = sid
		}
		if u, ok := ev["usage"].(map[string]any); ok {
			tokens += asInt(u["input_tokens"]) + asInt(u["output_tokens"])
		}
		if role, _ := ev["role"].(string); role == "assistant" {
			var message strings.Builder
			switch content := ev["content"].(type) {
			case string:
				message.WriteString(content)
			case []any:
				for _, item := range content {
					if block, ok := item.(map[string]any); ok {
						if value, ok := block["text"].(string); ok {
							message.WriteString(value)
						}
					}
				}
			}
			// Native kimi role events are complete assistant messages, not
			// token deltas. Tool-using sessions can contain an early status
			// message followed by the actual final answer; keep the latest
			// non-empty message instead of concatenating both into invalid JSON.
			if message.Len() > 0 {
				nativeAssistantText = message.String()
			}
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "result":
			if r, ok := ev["result"].(string); ok {
				resultText = r
				haveResult = true
			}
		case "assistant":
			if msg, ok := ev["message"].(map[string]any); ok {
				if content, ok := msg["content"].([]any); ok {
					for _, c := range content {
						if blk, ok := c.(map[string]any); ok {
							if t, _ := blk["type"].(string); t == "text" {
								if txt, ok := blk["text"].(string); ok {
									legacyAssistantText.WriteString(txt)
								}
							}
						}
					}
				}
			}
		}
	}

	switch {
	case haveResult && resultText != "":
		return resultText, sessionID, tokens
	case nativeAssistantText != "":
		return nativeAssistantText, sessionID, tokens
	case legacyAssistantText.Len() > 0:
		return legacyAssistantText.String(), sessionID, tokens
	default:
		// Nothing recognisable — hand back the raw stream so the schema-aware
		// fallback can still try to extract a JSON object from it.
		return stdout, sessionID, tokens
	}
}
