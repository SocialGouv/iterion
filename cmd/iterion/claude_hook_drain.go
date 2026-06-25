package main

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/SocialGouv/iterion/pkg/supervise"
	"github.com/spf13/cobra"
)

// claudeHookDrainCmd is the hidden subcommand a raw Claude Code session
// invokes from its Stop / PostToolUse hooks (installed by
// `iterion supervise install-hook`). It reads the hook payload on stdin,
// drains the supervisor inbox for that session, and prints the hook JSON
// that injects the queued operator/supervisor messages — the public
// equivalent of the in-process inbox-drain the claude_code delegate uses.
//
// Output contract (Claude Code hook protocol):
//   - Stop:        {"decision":"block","reason":<msg>}
//   - PostToolUse: {"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":<msg>}}
//   - nothing queued: empty output (no-op) so the session is never wedged.
var claudeHookDrainCmd = &cobra.Command{
	Use:    "__claude-hook-drain",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		var in struct {
			SessionID      string `json:"session_id"`
			Cwd            string `json:"cwd"`
			HookEventName  string `json:"hook_event_name"`
			StopHookActive bool   `json:"stop_hook_active"`
		}
		if raw, err := io.ReadAll(os.Stdin); err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &in)
		}
		if in.Cwd == "" {
			return nil // can't resolve the inbox without a cwd — no-op
		}
		projectKey := supervise.ClaudeProjectKey(in.Cwd)

		ctx := context.Background()
		var texts []string
		if in.SessionID != "" {
			t, _ := supervise.DrainClaudeInbox(ctx, projectKey, in.SessionID)
			texts = append(texts, t...)
		}
		// Also drain the project-wide fallback inbox (used when the
		// supervisor couldn't resolve a session id).
		t, _ := supervise.DrainClaudeInbox(ctx, projectKey, "")
		texts = append(texts, t...)

		msg := supervise.FormatOperatorMessages(texts)
		if msg == "" {
			return nil // no-op; never block a stop with nothing new
		}

		enc := json.NewEncoder(os.Stdout)
		switch in.HookEventName {
		case "Stop":
			return enc.Encode(map[string]any{"decision": "block", "reason": msg})
		default: // PostToolUse (and any tool-boundary event)
			return enc.Encode(map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":     "PostToolUse",
					"additionalContext": msg,
				},
			})
		}
	},
}

func init() {
	rootCmd.AddCommand(claudeHookDrainCmd)
}
