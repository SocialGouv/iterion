package model

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

func TestSystemReminderEnvelope(t *testing.T) {
	got := systemReminder("  hello  ")
	if got != "<system-reminder>\nhello\n</system-reminder>" {
		t.Errorf("unexpected envelope: %q", got)
	}
}

func TestTodoReseedMessageShape(t *testing.T) {
	msg := todoReseedMessage()
	if msg.Role != "user" {
		t.Errorf("role = %q, want user", msg.Role)
	}
	text := msg.Content[0].Text
	if !strings.HasPrefix(text, "<system-reminder>") || !strings.Contains(text, "todo_write") {
		t.Errorf("reseed must be an enveloped todo nudge: %q", text)
	}
}

func TestHasTodoTool(t *testing.T) {
	if hasTodoTool(nil) {
		t.Error("nil tools must not report todo_write")
	}
	if hasTodoTool([]GenerationTool{{Name: "bash"}}) {
		t.Error("bash-only tools must not report todo_write")
	}
	if !hasTodoTool([]GenerationTool{{Name: "bash"}, {Name: "todo_write"}}) {
		t.Error("todo_write present but not detected")
	}
}

func TestCustomInteractionRestoresCapabilityAnswerAtPromptStart(t *testing.T) {
	answer := "[page context: bot/bots/demo/main.bot]\n<active-editor-document>{\"complete\":true}</active-editor-document>\n\ncorrige le buffer"
	got := prependPriorAskUser("Operator message:\nold request", map[string]any{
		delegate.PriorInteractionQuestionsKey: map[string]any{
			"active_editor_document": "Send the active editor document.",
		},
		delegate.PriorInteractionAnswersKey: map[string]any{
			"active_editor_document": answer,
		},
	})

	if !strings.HasPrefix(got, answer+"\n\n<system-reminder>") {
		t.Fatalf("capability answer must lead the resumed prompt, got: %q", got)
	}
	if !strings.Contains(got, `field "active_editor_document"`) {
		t.Fatalf("missing interaction framing: %q", got)
	}
}
