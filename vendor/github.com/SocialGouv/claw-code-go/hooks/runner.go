package hooks

import (
	"fmt"
	"strings"
)

// HookConfig holds the hook command lists per event type.
type HookConfig struct {
	PreToolUse         []string
	PostToolUse        []string
	PostToolUseFailure []string
}

// HookProgressEvent reports hook execution progress.
type HookProgressEvent struct {
	Kind     string // "started", "completed", "cancelled"
	Event    HookEvent
	ToolName string
	Command  string
}

// HookProgressReporter receives progress notifications.
type HookProgressReporter interface {
	OnEvent(event HookProgressEvent)
}

// HookRunner executes hook commands for tool lifecycle events.
type HookRunner struct {
	config HookConfig
}

// NewHookRunner creates a new HookRunner with the given config.
func NewHookRunner(config HookConfig) *HookRunner {
	return &HookRunner{config: config}
}

// RunPreToolUse runs hooks for the PreToolUse event.
func (r *HookRunner) RunPreToolUse(toolName, toolInput string) HookRunResult {
	return r.RunPreToolUseWithContext(toolName, toolInput, nil, nil)
}

// RunPostToolUse runs hooks for the PostToolUse event.
func (r *HookRunner) RunPostToolUse(toolName, toolInput, toolOutput string, isError bool) HookRunResult {
	return r.RunPostToolUseWithContext(toolName, toolInput, toolOutput, isError, nil, nil)
}

// RunPostToolUseFailure runs hooks for the PostToolUseFailure event.
func (r *HookRunner) RunPostToolUseFailure(toolName, toolInput, toolError string) HookRunResult {
	return r.RunPostToolUseFailureWithContext(toolName, toolInput, toolError, nil, nil)
}

// RunPreToolUseWithSignal runs PreToolUse hooks with an abort signal but no progress reporter.
func (r *HookRunner) RunPreToolUseWithSignal(toolName, toolInput string, abort *HookAbortSignal) HookRunResult {
	return r.RunPreToolUseWithContext(toolName, toolInput, abort, nil)
}

// RunPostToolUseWithSignal runs PostToolUse hooks with an abort signal but no progress reporter.
func (r *HookRunner) RunPostToolUseWithSignal(toolName, toolInput, toolOutput string, isError bool, abort *HookAbortSignal) HookRunResult {
	return r.RunPostToolUseWithContext(toolName, toolInput, toolOutput, isError, abort, nil)
}

// RunPostToolUseFailureWithSignal runs PostToolUseFailure hooks with an abort signal but no progress reporter.
func (r *HookRunner) RunPostToolUseFailureWithSignal(toolName, toolInput, toolError string, abort *HookAbortSignal) HookRunResult {
	return r.RunPostToolUseFailureWithContext(toolName, toolInput, toolError, abort, nil)
}

// RunPreToolUseWithContext runs PreToolUse hooks with abort signal and progress reporter.
func (r *HookRunner) RunPreToolUseWithContext(toolName, toolInput string, abort *HookAbortSignal, reporter HookProgressReporter) HookRunResult {
	return r.runHooks(PreToolUse, toolName, toolInput, nil, false, abort, reporter)
}

// RunPostToolUseWithContext runs PostToolUse hooks with abort signal and progress reporter.
func (r *HookRunner) RunPostToolUseWithContext(toolName, toolInput, toolOutput string, isError bool, abort *HookAbortSignal, reporter HookProgressReporter) HookRunResult {
	return r.runHooks(PostToolUse, toolName, toolInput, &toolOutput, isError, abort, reporter)
}

// RunPostToolUseFailureWithContext runs PostToolUseFailure hooks with abort signal and progress reporter.
func (r *HookRunner) RunPostToolUseFailureWithContext(toolName, toolInput, toolError string, abort *HookAbortSignal, reporter HookProgressReporter) HookRunResult {
	return r.runHooks(PostToolUseFailure, toolName, toolInput, &toolError, true, abort, reporter)
}

// commandsForEvent returns the command list for the given event type.
func (r *HookRunner) commandsForEvent(event HookEvent) []string {
	switch event {
	case PreToolUse:
		return r.config.PreToolUse
	case PostToolUse:
		return r.config.PostToolUse
	case PostToolUseFailure:
		return r.config.PostToolUseFailure
	default:
		return nil
	}
}

// runHooks is the core execution loop for hook commands.
//
// Rust flow: run_command → parse stdout → with_fallback_message (per-command) →
// merge_parsed_hook_output (extends result.messages). We replicate this by
// applying fallback to parsed.Messages BEFORE accumulating into allMessages.
func (r *HookRunner) runHooks(event HookEvent, toolName, toolInput string, toolOutput *string, isError bool, abort *HookAbortSignal, reporter HookProgressReporter) HookRunResult {
	commands := r.commandsForEvent(event)
	if len(commands) == 0 {
		return Allow(nil)
	}

	var allMessages []string
	var permOverride *PermissionDecision
	var permReason string
	var updatedInput string

	// Rust checks abort once before entering the loop (hooks.rs:324).
	if abort != nil && abort.IsAborted() {
		return HookRunResult{
			Cancelled: true,
			Messages:  []string{fmt.Sprintf("%s hook cancelled before execution", event)},
		}
	}

	for _, command := range commands {
		// Report started.
		if reporter != nil {
			reporter.OnEvent(HookProgressEvent{
				Kind:     "started",
				Event:    event,
				ToolName: toolName,
				Command:  command,
			})
		}

		// Build env vars.
		env := map[string]string{
			"HOOK_EVENT":      string(event),
			"HOOK_TOOL_NAME":  toolName,
			"HOOK_TOOL_INPUT": toolInput,
		}
		if toolOutput != nil {
			env["HOOK_TOOL_OUTPUT"] = *toolOutput
		}
		if isError {
			env["HOOK_TOOL_IS_ERROR"] = "1"
		} else {
			env["HOOK_TOOL_IS_ERROR"] = "0"
		}

		// Build payload.
		payload := BuildPayload(event, toolName, toolInput, toolOutput, isError)

		// Execute command.
		result := runShellCommand(command, env, payload, abort)

		// Handle cancellation during execution (Rust hooks.rs:473-478).
		if result.Cancelled {
			if reporter != nil {
				reporter.OnEvent(HookProgressEvent{
					Kind:     "cancelled",
					Event:    event,
					ToolName: toolName,
					Command:  command,
				})
			}
			return HookRunResult{
				Cancelled:          true,
				Messages:           append(allMessages, fmt.Sprintf("%s hook `%s` cancelled while handling `%s`", event, command, toolName)),
				PermissionOverride: permOverride,
				PermissionReason:   permReason,
				UpdatedInput:       updatedInput,
			}
		}

		// Handle failed-to-start (Rust hooks.rs:479-488).
		if result.FailedToStart {
			if reporter != nil {
				reporter.OnEvent(HookProgressEvent{
					Kind:     "completed",
					Event:    event,
					ToolName: toolName,
					Command:  command,
				})
			}
			msg := fmt.Sprintf("%s hook `%s` failed to start for `%s`: %s", event, command, toolName, result.Stderr)
			return HookRunResult{
				Failed:             true,
				Messages:           append(allMessages, msg),
				PermissionOverride: permOverride,
				PermissionReason:   permReason,
				UpdatedInput:       updatedInput,
			}
		}

		// Parse response from stdout.
		parsed := parseHookOutput(result.Stdout)

		// Merge permission fields (Rust merge_parsed_hook_output: override, not accumulate).
		if parsed.PermissionOverride != nil {
			permOverride = parsed.PermissionOverride
		}
		if parsed.PermissionReason != "" {
			permReason = parsed.PermissionReason
		}
		if parsed.UpdatedInput != "" {
			updatedInput = parsed.UpdatedInput
		}

		// Interpret exit code. Apply fallback to parsed.Messages (per-command)
		// BEFORE accumulating into allMessages. This matches Rust's
		// with_fallback_message being applied to the per-command ParsedHookOutput
		// before merge_parsed_hook_output extends result.messages.
		switch {
		case result.ExitCode == 0 && parsed.Deny:
			// JSON response requested denial.
			if reporter != nil {
				reporter.OnEvent(HookProgressEvent{
					Kind:     "completed",
					Event:    event,
					ToolName: toolName,
					Command:  command,
				})
			}
			allMessages = append(allMessages, parsed.Messages...)
			return HookRunResult{
				Denied:             true,
				Messages:           allMessages,
				PermissionOverride: permOverride,
				PermissionReason:   permReason,
				UpdatedInput:       updatedInput,
			}
		case result.ExitCode == 0:
			// Success, continue chain. Accumulate messages.
			allMessages = append(allMessages, parsed.Messages...)
		case result.ExitCode == 2:
			// Explicit denial. Apply per-command fallback (Rust hooks.rs:450-455).
			if len(parsed.Messages) == 0 {
				parsed.Messages = append(parsed.Messages, fmt.Sprintf("%s hook denied tool `%s`", event, toolName))
			}
			allMessages = append(allMessages, parsed.Messages...)
			if reporter != nil {
				reporter.OnEvent(HookProgressEvent{
					Kind:     "completed",
					Event:    event,
					ToolName: toolName,
					Command:  command,
				})
			}
			return HookRunResult{
				Denied:             true,
				Messages:           allMessages,
				PermissionOverride: permOverride,
				PermissionReason:   permReason,
				UpdatedInput:       updatedInput,
			}
		case result.ExitCode == -1:
			// Signal-terminated process (Rust None exit code, hooks.rs:464-470).
			if len(parsed.Messages) == 0 {
				parsed.Messages = append(parsed.Messages, fmt.Sprintf("%s hook `%s` terminated by signal while handling `%s`", event, command, toolName))
			}
			allMessages = append(allMessages, parsed.Messages...)
			if reporter != nil {
				reporter.OnEvent(HookProgressEvent{
					Kind:     "completed",
					Event:    event,
					ToolName: toolName,
					Command:  command,
				})
			}
			return HookRunResult{
				Failed:             true,
				Messages:           allMessages,
				PermissionOverride: permOverride,
				PermissionReason:   permReason,
				UpdatedInput:       updatedInput,
			}
		default:
			// Non-zero (other than 2 or -1): failure (Rust hooks.rs:456-463).
			// Rust passes parsed.primary_message() to format_hook_failure.
			var primaryMsg string
			if len(parsed.Messages) > 0 {
				primaryMsg = parsed.Messages[0]
			}
			if len(parsed.Messages) == 0 {
				parsed.Messages = append(parsed.Messages, formatHookFailure(command, result.ExitCode, primaryMsg, result.Stderr))
			}
			allMessages = append(allMessages, parsed.Messages...)
			if reporter != nil {
				reporter.OnEvent(HookProgressEvent{
					Kind:     "completed",
					Event:    event,
					ToolName: toolName,
					Command:  command,
				})
			}
			return HookRunResult{
				Failed:             true,
				Messages:           allMessages,
				PermissionOverride: permOverride,
				PermissionReason:   permReason,
				UpdatedInput:       updatedInput,
			}
		}

		// Report completed (only reached for exit code 0, no deny).
		if reporter != nil {
			reporter.OnEvent(HookProgressEvent{
				Kind:     "completed",
				Event:    event,
				ToolName: toolName,
				Command:  command,
			})
		}
	}

	return HookRunResult{
		Messages:           allMessages,
		PermissionOverride: permOverride,
		PermissionReason:   permReason,
		UpdatedInput:       updatedInput,
	}
}

// formatHookFailure matches Rust's format_hook_failure:
// "Hook `{command}` exited with status {code}: {stdout_or_stderr}"
func formatHookFailure(command string, code int, stdout, stderr string) string {
	msg := fmt.Sprintf("Hook `%s` exited with status %d", command, code)
	stdout = strings.TrimSpace(stdout)
	stderr = strings.TrimSpace(stderr)
	if stdout != "" {
		msg += ": " + stdout
	} else if stderr != "" {
		msg += ": " + stderr
	}
	return msg
}
