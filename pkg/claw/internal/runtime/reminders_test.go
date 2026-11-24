package runtime

import (
	"strings"
	"testing"
)

func TestSystemReminderWrap(t *testing.T) {
	got := SystemReminder("  todo may be stale  ")
	if got != "<system-reminder>\ntodo may be stale\n</system-reminder>" {
		t.Errorf("unexpected envelope: %q", got)
	}
}

func TestQueueAndFlushSystemReminders(t *testing.T) {
	loop := &ConversationLoop{Session: NewSession()}

	loop.QueueSystemReminder("first notice")
	loop.QueueSystemReminder("second notice")
	loop.QueueSystemReminder("   ") // blank: dropped

	loop.flushSystemReminders()

	if n := len(loop.Session.Messages); n != 1 {
		t.Fatalf("expected 1 injected message, got %d", n)
	}
	msg := loop.Session.Messages[0]
	if msg.Role != "user" || !msg.IsInjected {
		t.Errorf("reminder message must be injected user-role, got role=%q injected=%v", msg.Role, msg.IsInjected)
	}
	text := msg.Content[0].Text
	if !strings.Contains(text, "<system-reminder>\nfirst notice\n</system-reminder>") ||
		!strings.Contains(text, "<system-reminder>\nsecond notice\n</system-reminder>") {
		t.Errorf("both reminders must be wrapped and delivered:\n%s", text)
	}

	// Injected reminders never count as real user turns.
	if got := CountRealUserTurns(loop.Session.Messages); got != 0 {
		t.Errorf("CountRealUserTurns = %d, want 0", got)
	}

	// Queue drained: a second flush is a no-op.
	loop.flushSystemReminders()
	if n := len(loop.Session.Messages); n != 1 {
		t.Errorf("second flush must not append, got %d messages", n)
	}
}

func TestFlushSystemRemindersNilSession(t *testing.T) {
	loop := &ConversationLoop{}
	loop.QueueSystemReminder("orphan")
	loop.flushSystemReminders() // must not panic
}

func TestQueuePlanModeReminderProducer(t *testing.T) {
	loop := &ConversationLoop{Session: NewSession()}

	loop.queuePlanModeReminder(true, nil)
	loop.queuePlanModeReminder(false, nil)
	if len(loop.pendingReminders) != 2 {
		t.Fatalf("expected 2 queued reminders, got %d", len(loop.pendingReminders))
	}
	if !strings.Contains(loop.pendingReminders[0], "Plan mode is now active") {
		t.Errorf("enter reminder wrong: %q", loop.pendingReminders[0])
	}
	if !strings.Contains(loop.pendingReminders[1], "Plan mode is off") {
		t.Errorf("exit reminder wrong: %q", loop.pendingReminders[1])
	}

	// A failed transition queues nothing.
	loop.pendingReminders = nil
	loop.queuePlanModeReminder(true, errAlreadyActive)
	if len(loop.pendingReminders) != 0 {
		t.Errorf("failed transition must not queue a reminder")
	}
}

var errAlreadyActive = &LoopError{Kind: LoopErrInvalidArgs, Subsystem: "plan", Message: "already active"}
