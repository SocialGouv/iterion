package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildOraclePromptInlinesFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a"), 0o644); err != nil {
		t.Fatal(err)
	}

	sys, user, err := BuildOraclePrompt(map[string]any{
		"question": "Should we split this package?",
		"context":  "It has 40 files.",
		"files":    []any{"a.go"},
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sys, "Oracle") || !strings.Contains(sys, "NO tools") {
		t.Errorf("system prompt must frame the advisor role: %q", sys)
	}
	for _, want := range []string{"Should we split", "## Additional context", "It has 40 files.", "## File: a.go", "package a"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q:\n%s", want, user)
		}
	}
}

func TestBuildOraclePromptErrors(t *testing.T) {
	if _, _, err := BuildOraclePrompt(map[string]any{}, ""); err == nil {
		t.Error("missing question must error")
	}
	if _, _, err := BuildOraclePrompt(map[string]any{
		"question": "q", "files": []any{"does-not-exist.txt"},
	}, t.TempDir()); err == nil {
		t.Error("unreadable file must be an explicit error, not silently skipped")
	}
	many := make([]any, oracleMaxFiles+1)
	for i := range many {
		many[i] = "f.txt"
	}
	if _, _, err := BuildOraclePrompt(map[string]any{"question": "q", "files": many}, ""); err == nil {
		t.Error("too many files must error")
	}
}

func TestBuildOraclePromptTruncatesWithMarker(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("z", oraclePerFileCap+100)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	_, user, err := BuildOraclePrompt(map[string]any{"question": "q", "files": []any{"big.txt"}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(user, "file truncated at") {
		t.Error("truncation must be visible to the oracle")
	}
	if got := strings.Count(user, "z"); got != oraclePerFileCap {
		t.Errorf("content must be capped at exactly %d bytes, got %d", oraclePerFileCap, got)
	}
}
