package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTodoWriteStoresOutOfTree(t *testing.T) {
	workspace := t.TempDir()
	todosDir := t.TempDir()
	t.Chdir(workspace)
	t.Setenv("CLAW_TODOS_DIR", todosDir)

	if _, err := ExecuteTodoWrite(map[string]any{
		"action": "write",
		"todos": []any{
			map[string]any{"id": "1", "content": "step", "status": "pending", "priority": "high"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The workspace stays clean — no .claude/ directory materializes.
	if _, err := os.Stat(filepath.Join(workspace, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("todo_write must not touch the workspace tree (err=%v)", err)
	}
	entries, err := os.ReadDir(todosDir)
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("todo file must land in CLAW_TODOS_DIR: %v err=%v", entries, err)
	}

	out, err := ExecuteTodoWrite(map[string]any{"action": "read"})
	if err != nil || !strings.Contains(out, "step") {
		t.Errorf("read-back failed: %q err=%v", out, err)
	}
}

func TestTodoWriteLegacyReadFallback(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	t.Setenv("CLAW_TODOS_DIR", t.TempDir())

	// A checklist written before the out-of-tree move still resolves.
	if err := os.MkdirAll(filepath.Join(workspace, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `[{"id":"1","content":"legacy item","status":"pending","priority":"low"}]`
	if err := os.WriteFile(filepath.Join(workspace, ".claude", "todos.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := ExecuteTodoWrite(map[string]any{"action": "read"})
	if err != nil || !strings.Contains(out, "legacy item") {
		t.Errorf("legacy fallback failed: %q err=%v", out, err)
	}

	// Writing migrates to the new location and takes precedence over legacy.
	if _, err := ExecuteTodoWrite(map[string]any{
		"action": "write",
		"todos":  []any{map[string]any{"id": "2", "content": "new item", "status": "pending", "priority": "low"}},
	}); err != nil {
		t.Fatal(err)
	}
	out, err = ExecuteTodoWrite(map[string]any{"action": "read"})
	if err != nil || !strings.Contains(out, "new item") || strings.Contains(out, "legacy item") {
		t.Errorf("new location must win after first write: %q err=%v", out, err)
	}
}

func TestTodosPathForKeyIsolatesAndSanitizes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAW_TODOS_DIR", dir)

	a := TodosPathForKey("fp-1")
	b := TodosPathForKey("fp-1-session-42")
	if a == b {
		t.Error("distinct keys must map to distinct files")
	}
	weird := TodosPathForKey("a/b\\c:d e")
	if strings.ContainsAny(filepath.Base(weird), "/\\: ") {
		t.Errorf("key must be sanitized: %s", weird)
	}
	if TodosPathForKey("") != filepath.Join(dir, "default.json") {
		t.Errorf("empty key must fall back to default: %s", TodosPathForKey(""))
	}
}
