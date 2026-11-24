package hooks

import (
	"encoding/json"
	"strings"
)

// parsedHookOutput is the parsed JSON response from a hook command.
type parsedHookOutput struct {
	Messages           []string
	Deny               bool
	PermissionOverride *PermissionDecision
	PermissionReason   string
	UpdatedInput       string
}

// hookOutputJSON mirrors the expected JSON structure from hook stdout.
type hookOutputJSON struct {
	SystemMessage      string                  `json:"systemMessage"`
	Reason             string                  `json:"reason"`
	Continue           *bool                   `json:"continue"`
	Decision           string                  `json:"decision"`
	HookSpecificOutput *hookSpecificOutputJSON `json:"hookSpecificOutput"`
}

type hookSpecificOutputJSON struct {
	AdditionalContext        string          `json:"additionalContext"`
	PermissionDecision       string          `json:"permissionDecision"`
	PermissionDecisionReason string          `json:"permissionDecisionReason"`
	UpdatedInput             json.RawMessage `json:"updatedInput"`
}

// parseHookOutput parses stdout from a hook command.
// If stdout is not valid JSON, return empty parsedHookOutput (plain-text fallback).
func parseHookOutput(stdout string) parsedHookOutput {
	var result parsedHookOutput

	var raw hookOutputJSON
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		// Plain text fallback: push entire stdout as message (matching Rust hooks.rs:584-586).
		if strings.TrimSpace(stdout) != "" {
			result.Messages = append(result.Messages, stdout)
		}
		return result
	}

	// Collect messages.
	if raw.SystemMessage != "" {
		result.Messages = append(result.Messages, raw.SystemMessage)
	}
	if raw.Reason != "" {
		result.Messages = append(result.Messages, raw.Reason)
	}

	// Check deny conditions.
	if raw.Continue != nil && !*raw.Continue {
		result.Deny = true
	}
	if raw.Decision == "block" {
		result.Deny = true
	}

	// Process hookSpecificOutput.
	if raw.HookSpecificOutput != nil {
		hso := raw.HookSpecificOutput

		if hso.AdditionalContext != "" {
			result.Messages = append(result.Messages, hso.AdditionalContext)
		}

		if hso.PermissionDecision != "" {
			pd := PermissionDecision(hso.PermissionDecision)
			switch pd {
			case PermissionAllow, PermissionDeny, PermissionAsk:
				result.PermissionOverride = &pd
			}
		}

		if hso.PermissionDecisionReason != "" {
			result.PermissionReason = hso.PermissionDecisionReason
		}

		if len(hso.UpdatedInput) > 0 && string(hso.UpdatedInput) != "null" {
			result.UpdatedInput = string(hso.UpdatedInput)
		}
	}

	// Fallback: if JSON was valid but no message fields were found, push
	// the raw stdout as a message (matching Rust hooks.rs:584-586).
	if len(result.Messages) == 0 && strings.TrimSpace(stdout) != "" {
		result.Messages = append(result.Messages, stdout)
	}

	return result
}
