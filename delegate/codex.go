package delegate

import (
	"context"
	"fmt"
	"strings"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"

	iterlog "github.com/SocialGouv/iterion/log"
)

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
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
	}

	var opts []codexsdk.Option

	// Build system prompt, optionally augmented with interaction instructions.
	systemPrompt := task.SystemPrompt
	if task.InteractionEnabled {
		systemPrompt += interactionSystemInstruction
	}
	if systemPrompt != "" {
		opts = append(opts, codexsdk.WithSystemPrompt(systemPrompt))
	}
	if task.WorkDir != "" {
		opts = append(opts, codexsdk.WithCwd(task.WorkDir))
	}
	if len(task.AllowedTools) > 0 {
		opts = append(opts, codexsdk.WithAllowedTools(task.AllowedTools...))
	}
	// Bypass interactive permission prompts: the runtime enforces safety via
	// workspace isolation and allowed-tool lists, so the delegate subprocess
	// does not need its own permission gate.
	opts = append(opts, codexsdk.WithPermissionMode("bypassPermissions"))

	if b.Command != "" {
		opts = append(opts, codexsdk.WithCliPath(b.Command))
	}

	if len(task.OutputSchema) > 0 {
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
	var stderrBuf strings.Builder
	opts = append(opts, codexsdk.WithStderr(func(line string) {
		stderrBuf.WriteString(line)
		stderrBuf.WriteString("\n")
		if line != "" {
			b.Logger.Debug("[%s] %s", task.NodeID, line)
		}
	}))

	prompt := task.UserPrompt

	const maxRetries = 3
	var resultMsg *codexsdk.ResultMessage
	var queryErr error
	var totalDuration time.Duration

	for attempt := 1; attempt <= maxRetries; attempt++ {
		startTime := time.Now()
		resultMsg = nil
		queryErr = nil

		for msg, err := range codexsdk.Query(ctx, codexsdk.Text(prompt), opts...) {
			if err != nil {
				queryErr = err
				break
			}
			if rm, ok := msg.(*codexsdk.ResultMessage); ok {
				resultMsg = rm
			}
		}

		totalDuration += time.Since(startTime)

		if queryErr != nil {
			return Result{
				Duration:    totalDuration,
				ExitCode:    -1,
				Stderr:      stderrBuf.String(),
				BackendName: BackendCodex,
			}, fmt.Errorf("delegate: codex failed: %w", queryErr)
		}

		if resultMsg != nil {
			break // success
		}

		// Codex process exited without producing a ResultMessage.
		// This is a known transient failure — retry.
		if attempt < maxRetries {
			b.Logger.Warn("[%s] codex returned no result (attempt %d/%d), retrying", task.NodeID, attempt, maxRetries)
		}
	}

	if resultMsg == nil {
		return Result{
			Duration:    totalDuration,
			ExitCode:    -1,
			Stderr:      stderrBuf.String(),
			BackendName: BackendCodex,
		}, fmt.Errorf("delegate: codex: no result message received after %d attempts", maxRetries)
	}

	result := Result{
		Duration:    totalDuration,
		ExitCode:    0,
		Stderr:      stderrBuf.String(),
		BackendName: BackendCodex,
		SessionID:   resultMsg.SessionID,
	}

	if resultMsg.Usage != nil {
		result.Tokens = resultMsg.Usage.InputTokens + resultMsg.Usage.OutputTokens
	}

	if resultMsg.IsError && resultMsg.Subtype != "success" {
		return result, fmt.Errorf("delegate: codex error: subtype=%s", resultMsg.Subtype)
	}

	output, rawLen, fallback := parseSDKOutput(resultMsg.Result, resultMsg.StructuredOutput, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	return result, nil
}

// mapReasoningEffort converts iterion reasoning effort strings to Codex SDK Effort constants.
func mapReasoningEffort(s string) codexsdk.Effort {
	switch s {
	case "low":
		return codexsdk.EffortLow
	case "medium":
		return codexsdk.EffortMedium
	case "high":
		return codexsdk.EffortHigh
	case "extra_high":
		return codexsdk.EffortMax
	default:
		return codexsdk.EffortMedium
	}
}
