package claudesdk

import (
	"slices"
	"strings"
	"testing"
)

// flagValue returns the argument following the first occurrence of flag in
// args, or "" if the flag is absent or has no value after it.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasFlag(args []string, flag string) bool {
	return slices.Contains(args, flag)
}

func TestBuildArgs_AppendSystemPromptNotReplace(t *testing.T) {
	// The claude_code backend routes the assembled prompt to
	// --append-system-prompt so Claude Code's native system prompt is kept.
	args := buildArgs(processConfig{AppendSystemPrompt: "extra instructions"}, true)
	if got := flagValue(args, "--append-system-prompt"); got != "extra instructions" {
		t.Errorf("--append-system-prompt = %q, want %q", got, "extra instructions")
	}
	if hasFlag(args, "--system-prompt") {
		t.Error("--system-prompt must not be emitted when only AppendSystemPrompt is set (would replace the native prompt)")
	}
}

func TestBuildArgs_SettingSources(t *testing.T) {
	args := buildArgs(processConfig{
		SettingSources: []SettingSource{SettingSourceUser, SettingSourceProject},
	}, true)
	if got := flagValue(args, "--setting-sources"); got != "user,project" {
		t.Errorf("--setting-sources = %q, want %q", got, "user,project")
	}

	// No sources → flag omitted (CLI falls back to its own default).
	if hasFlag(buildArgs(processConfig{}, true), "--setting-sources") {
		t.Error("--setting-sources must be omitted when no sources are configured")
	}
}

func TestBuildArgs_ThinkingDisplay(t *testing.T) {
	args := buildArgs(processConfig{ThinkingDisplay: "summarized"}, true)
	if got := flagValue(args, "--thinking-display"); got != "summarized" {
		t.Errorf("--thinking-display = %q, want summarized", got)
	}

	// Empty → flag omitted (older CLIs reject unknown options).
	if hasFlag(buildArgs(processConfig{}, true), "--thinking-display") {
		t.Error("--thinking-display must be omitted when unset")
	}
}

func TestBuildArgs_SettingSourcesAllThree(t *testing.T) {
	args := buildArgs(processConfig{
		SettingSources: []SettingSource{SettingSourceUser, SettingSourceProject, SettingSourceLocal},
	}, true)
	got := flagValue(args, "--setting-sources")
	for _, want := range []string{"user", "project", "local"} {
		if !strings.Contains(got, want) {
			t.Errorf("--setting-sources %q missing %q", got, want)
		}
	}
}

func TestBuildArgs_StrictMCPConfig(t *testing.T) {
	if !hasFlag(buildArgs(processConfig{StrictMCPConfig: true}, true), "--strict-mcp-config") {
		t.Error("--strict-mcp-config must be emitted when StrictMCPConfig is set")
	}
	// Emitted with no --mcp-config too: "no servers at all" is the point —
	// the CLI must not fall back to user/project MCP scopes.
	if !hasFlag(buildArgs(processConfig{StrictMCPConfig: true}, false), "--strict-mcp-config") {
		t.Error("--strict-mcp-config must be emitted on the one-shot path as well")
	}
	if hasFlag(buildArgs(processConfig{}, true), "--strict-mcp-config") {
		t.Error("--strict-mcp-config must be omitted when unset (host inheritance opt-in)")
	}
}

func TestBuildArgs_Settings(t *testing.T) {
	args := buildArgs(processConfig{
		SettingsJSON: []byte(`{"autoMemoryEnabled":true}`),
	}, true)
	if got := flagValue(args, "--settings"); got != `{"autoMemoryEnabled":true}` {
		t.Errorf("--settings = %q, want the inline JSON verbatim", got)
	}

	// Empty → flag omitted. The CLI treats an empty --settings as a parse
	// error, and there is nothing to merge anyway.
	if hasFlag(buildArgs(processConfig{}, true), "--settings") {
		t.Error("--settings must be omitted when no inline settings are configured")
	}
}
