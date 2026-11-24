package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSemFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"retry.go": `package httpkit

// retryWithBackoff retries transient HTTP failures with exponential backoff.
func retryWithBackoff(attempts int) {
	// exponential delay between retries
}`,
		"parser.go": `package parser

// parseTokens turns source text into lexer tokens.
func parseTokens(src string) {}`,
		"notes.md": "The scheduler polls the queue every thirty seconds.",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "vendor", "dep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vendor", "dep", "retry.go"), []byte("retry backoff http vendor copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSemanticSearchRanksByVocabulary(t *testing.T) {
	dir := writeSemFixture(t)
	// Query wording differs from the code (no exact string match), but the
	// identifier split (retryWithBackoff → retry, with, backoff) carries it.
	out, err := ExecuteSemanticSearch(map[string]any{"query": "http retry backoff handling"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(out, "\n", 3)
	if len(first) < 2 || !strings.Contains(first[1], "retry.go") {
		t.Errorf("retry.go must rank first:\n%s", out)
	}
	if strings.Contains(out, "vendor") {
		t.Errorf("vendor/ must be skipped:\n%s", out)
	}
}

func TestSemanticSearchGlobFilter(t *testing.T) {
	dir := writeSemFixture(t)
	out, err := ExecuteSemanticSearch(map[string]any{"query": "scheduler polls queue", "glob": "*.md"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "notes.md") || strings.Contains(out, "retry.go") {
		t.Errorf("glob filter must restrict candidates:\n%s", out)
	}
}

func TestSemanticSearchErrors(t *testing.T) {
	if _, err := ExecuteSemanticSearch(map[string]any{}, t.TempDir()); err == nil {
		t.Error("missing query must error")
	}
	if _, err := ExecuteSemanticSearch(map[string]any{"query": "a ,"}, t.TempDir()); err == nil {
		t.Error("token-less query must error")
	}
}

func TestSemTokenizeSplitsIdentifiers(t *testing.T) {
	got := semTokenize("retryWithBackoff snake_case_name HTTPServer x")
	joined := " " + strings.Join(got, " ") + " "
	for _, want := range []string{" retry ", " backoff ", " snake ", " case ", " name ", " httpserver "} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing token%q in %v", want, got)
		}
	}
	if strings.Contains(joined, " x ") {
		t.Errorf("1-char tokens must be dropped: %v", got)
	}
}
