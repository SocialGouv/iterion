package operatormcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// local_validate reads an author document — the one MCP tool that takes one —
// and answers with the CLI's JSON: valid, named as an author source.
func TestLocalValidateReadsAnAuthorDocument(t *testing.T) {
	s := newTestServer(t)
	doc := "dsl: 2\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"
	if err := os.WriteFile(filepath.Join(s.WorkDir, "hello.bot.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, s, "local_validate", `{"file_path":"hello.bot.yaml"}`)
	if isErr {
		t.Fatalf("local_validate on a document: %s", text)
	}
	var got struct {
		Valid      bool   `json:"valid"`
		SourceKind string `json:"source_kind"`
		BotPath    string `json:"bot_path"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	if !got.Valid || got.SourceKind != "author" || filepath.Base(got.BotPath) != "hello.bot" {
		t.Fatalf("valid=%v source_kind=%q bot_path=%q, want true / author / …/hello.bot", got.Valid, got.SourceKind, got.BotPath)
	}
	// The tool says so: an agent that writes documents reads it there.
	for _, tool := range localTools() {
		if tool.Name != "local_validate" {
			continue
		}
		if !strings.Contains(tool.Description, ".bot.yaml") || !strings.Contains(string(tool.InputSchema), ".bot.yaml") {
			t.Fatalf("local_validate does not say it reads an author document:\n%s\n%s", tool.Description, tool.InputSchema)
		}
	}
}
