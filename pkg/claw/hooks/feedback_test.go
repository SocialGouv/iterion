package hooks

import "testing"

func TestMergeHookFeedbackEmptyMessages(t *testing.T) {
	// Empty messages returns output unchanged (no-op).
	out := MergeHookFeedback(nil, "tool output", false)
	if out != "tool output" {
		t.Errorf("expected unchanged output, got %q", out)
	}
	out = MergeHookFeedback([]string{}, "tool output", false)
	if out != "tool output" {
		t.Errorf("expected unchanged output, got %q", out)
	}
}

func TestMergeHookFeedbackSingleMessage(t *testing.T) {
	out := MergeHookFeedback([]string{"msg1"}, "tool output", false)
	expected := "tool output\n\nHook feedback:\nmsg1"
	if out != expected {
		t.Errorf("expected %q, got %q", expected, out)
	}
}

func TestMergeHookFeedbackMultipleMessages(t *testing.T) {
	out := MergeHookFeedback([]string{"msg1", "msg2", "msg3"}, "tool output", false)
	expected := "tool output\n\nHook feedback:\nmsg1\nmsg2\nmsg3"
	if out != expected {
		t.Errorf("expected %q, got %q", expected, out)
	}
}

func TestMergeHookFeedbackErrorLabel(t *testing.T) {
	out := MergeHookFeedback([]string{"denied"}, "tool output", true)
	expected := "tool output\n\nHook feedback (error):\ndenied"
	if out != expected {
		t.Errorf("expected %q, got %q", expected, out)
	}
}

func TestMergeHookFeedbackEmptyOutput(t *testing.T) {
	// When output is empty/whitespace, only the feedback section is returned.
	out := MergeHookFeedback([]string{"msg1"}, "", false)
	expected := "Hook feedback:\nmsg1"
	if out != expected {
		t.Errorf("expected %q, got %q", expected, out)
	}
}

func TestMergeHookFeedbackWhitespaceOutput(t *testing.T) {
	out := MergeHookFeedback([]string{"msg1"}, "   \n  ", false)
	expected := "Hook feedback:\nmsg1"
	if out != expected {
		t.Errorf("expected %q, got %q", expected, out)
	}
}

func TestMergeHookFeedbackEmptyOutputWithError(t *testing.T) {
	out := MergeHookFeedback([]string{"error info"}, "", true)
	expected := "Hook feedback (error):\nerror info"
	if out != expected {
		t.Errorf("expected %q, got %q", expected, out)
	}
}
