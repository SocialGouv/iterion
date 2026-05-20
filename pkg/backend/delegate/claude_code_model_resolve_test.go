package delegate

import "testing"

// TestResolveClaudeCodeModel covers the env-override path that lets
// operators pin a single model (typically a z.ai-gateway alias like
// "glm-5.1") across every claude_code node, bypassing whatever the bot
// declared. The override matters because z.ai's gateway silently maps
// bot-pinned "claude-opus-4-7" to its mid-tier default (~glm-4.6 at
// time of writing) — explicit pinning is the only way to land on the
// flagship GLM.
func TestResolveClaudeCodeModel(t *testing.T) {
	tests := []struct {
		name      string
		envValue  string // value to set ITERION_CLAUDE_CODE_MODEL to ("" = unset)
		taskModel string
		want      string
	}{
		{
			name:      "no override, bot pins a model — task wins",
			taskModel: "claude-opus-4-7",
			want:      "claude-opus-4-7",
		},
		{
			name:      "no override, bot leaves model empty — default",
			taskModel: "",
			want:      defaultClaudeCodeModel,
		},
		{
			name:      "env override beats bot-pinned model",
			envValue:  "glm-5.1",
			taskModel: "claude-opus-4-7",
			want:      "glm-5.1",
		},
		{
			name:      "env override beats default",
			envValue:  "glm-4.6",
			taskModel: "",
			want:      "glm-4.6",
		},
		{
			name:      "env override with whitespace trimmed",
			envValue:  "  glm-5.1  ",
			taskModel: "claude-opus-4-7",
			want:      "glm-5.1",
		},
		{
			name:      "empty env (export X=) treated as no override",
			envValue:  "",
			taskModel: "claude-sonnet-4-6",
			want:      "claude-sonnet-4-6",
		},
		{
			name:      "whitespace-only env treated as no override",
			envValue:  "   ",
			taskModel: "claude-sonnet-4-6",
			want:      "claude-sonnet-4-6",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv handles the restore after the subtest. Using "" to
			// clear is the standard Go convention since os.Unsetenv from
			// the test body races with parallel subtests; t.Setenv is the
			// safe primitive even for the "unset" cases.
			t.Setenv(claudeCodeModelOverrideEnv, tc.envValue)
			if got := resolveClaudeCodeModel(tc.taskModel); got != tc.want {
				t.Fatalf("resolveClaudeCodeModel(%q) with env=%q: got %q, want %q",
					tc.taskModel, tc.envValue, got, tc.want)
			}
		})
	}
}
