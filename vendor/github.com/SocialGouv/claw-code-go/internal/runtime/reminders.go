package runtime

import (
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

// System reminders are short harness-origin notices injected between turns,
// wrapped in the <system-reminder> envelope models are trained to read as
// background context from the harness — not user input (the communication
// prompt section states this contract). Producers queue reminders at the
// moment something changes (compaction, plan-mode transitions); the loop
// flushes the queue as ONE injected user message at the next turn boundary,
// where consecutive user-role messages are wire-safe on every provider and
// tool_result ordering cannot be broken.

// reminderPostCompaction nudges the model to reconcile its todo list after
// the history it lived in was summarized away.
const reminderPostCompaction = `The conversation history was just compacted. Your todo list may no longer reflect reality: re-read it (todo_write with action "read"), reconcile it with the summary above, then continue the task directly — do not re-ask questions the user already answered.`

// reminderPlanModeEntered / reminderPlanModeExited bracket plan-mode
// transitions so the read-only contract survives even when the tool result
// scrolls out of attention.
const (
	reminderPlanModeEntered = `Plan mode is now active: investigate and design only. Do not edit files, run state-changing commands, or commit; present your plan and let the user approve exiting plan mode.`
	reminderPlanModeExited  = `Plan mode is off: normal tool execution has resumed.`
)

// SystemReminder wraps harness guidance in the <system-reminder> envelope.
func SystemReminder(text string) string {
	return "<system-reminder>\n" + strings.TrimSpace(text) + "\n</system-reminder>"
}

// QueueSystemReminder schedules a reminder for injection at the next turn
// boundary. Safe to call from tool execution (same goroutine as the loop).
func (loop *ConversationLoop) QueueSystemReminder(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	loop.pendingReminders = append(loop.pendingReminders, text)
}

// flushSystemReminders drains the queue into a single injected user-role
// message appended to the session. Called at the start of each model turn:
// at that boundary the previous message is either the user's text or a
// tool_result message, so an extra user message merges cleanly after it.
func (loop *ConversationLoop) flushSystemReminders() {
	if len(loop.pendingReminders) == 0 || loop.Session == nil {
		return
	}
	wrapped := make([]string, len(loop.pendingReminders))
	for i, r := range loop.pendingReminders {
		wrapped[i] = SystemReminder(r)
	}
	text := strings.Join(wrapped, "\n\n")
	loop.pendingReminders = nil

	loop.Session.Messages = append(loop.Session.Messages, api.Message{
		Role:       "user",
		IsInjected: true,
		Content:    []api.ContentBlock{{Type: "text", Text: text}},
	})
	loop.Session.PromptHistory = append(loop.Session.PromptHistory, PromptHistoryEntry{
		TimestampMs: time.Now().UnixMilli(),
		Text:        "[reminder] " + truncate(text, 100),
	})
}

// queuePlanModeReminder is the plan-mode producer, shared by the streaming
// and non-streaming tool dispatch paths.
func (loop *ConversationLoop) queuePlanModeReminder(entering bool, err error) {
	if err != nil {
		return
	}
	if entering {
		loop.QueueSystemReminder(reminderPlanModeEntered)
		return
	}
	loop.QueueSystemReminder(reminderPlanModeExited)
}
