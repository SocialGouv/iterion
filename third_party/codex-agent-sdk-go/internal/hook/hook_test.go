package hook

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBaseInput_Getters(t *testing.T) {
	t.Parallel()

	mode := "plan"
	input := &BaseInput{
		SessionID:      "session-1",
		TranscriptPath: "/tmp/transcript.jsonl",
		Cwd:            "/tmp/project",
		PermissionMode: &mode,
	}

	require.Equal(t, "session-1", input.GetSessionID())
	require.Equal(t, "/tmp/transcript.jsonl", input.GetTranscriptPath())
	require.Equal(t, "/tmp/project", input.GetCwd())
	require.Equal(t, &mode, input.GetPermissionMode())
}

func TestInput_GetHookEventName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   Input
		want Event
	}{
		{name: "pre tool use", in: &PreToolUseInput{}, want: EventPreToolUse},
		{name: "post tool use", in: &PostToolUseInput{}, want: EventPostToolUse},
		{name: "user prompt submit", in: &UserPromptSubmitInput{}, want: EventUserPromptSubmit},
		{name: "stop", in: &StopInput{}, want: EventStop},
		{name: "subagent stop", in: &SubagentStopInput{}, want: EventSubagentStop},
		{name: "pre compact", in: &PreCompactInput{}, want: EventPreCompact},
		{name: "post tool use failure", in: &PostToolUseFailureInput{}, want: EventPostToolUseFailure},
		{name: "notification", in: &NotificationInput{}, want: EventNotification},
		{name: "subagent start", in: &SubagentStartInput{}, want: EventSubagentStart},
		{name: "permission request", in: &PermissionRequestInput{}, want: EventPermissionRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.in.GetHookEventName())
		})
	}
}

func TestSpecificOutput_GetHookEventName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  SpecificOutput
		want string
	}{
		{name: "pre tool use", out: &PreToolUseSpecificOutput{}, want: "PreToolUse"},
		{name: "post tool use", out: &PostToolUseSpecificOutput{}, want: "PostToolUse"},
		{name: "user prompt submit", out: &UserPromptSubmitSpecificOutput{}, want: "UserPromptSubmit"},
		{name: "post tool use failure", out: &PostToolUseFailureSpecificOutput{}, want: "PostToolUseFailure"},
		{name: "notification", out: &NotificationSpecificOutput{}, want: "Notification"},
		{name: "subagent start", out: &SubagentStartSpecificOutput{}, want: "SubagentStart"},
		{name: "permission request", out: &PermissionRequestSpecificOutput{}, want: "PermissionRequest"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.out.GetHookEventName())
		})
	}
}
