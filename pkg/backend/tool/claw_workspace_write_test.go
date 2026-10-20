package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func executeClawFileTool(t *testing.T, reg *Registry, name string, input map[string]any) {
	t.Helper()
	td, err := reg.Resolve(name)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", name, err)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := td.Execute(context.Background(), raw); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func TestClawWriteToolsRebaseOnlyRelativePaths(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)

	reg := NewRegistry()
	if err := RegisterClawBuiltins(reg, workspace); err != nil {
		t.Fatal(err)
	}

	t.Run("write_file relative", func(t *testing.T) {
		const rel = "nested/created.txt"
		executeClawFileTool(t, reg, "write_file", map[string]any{"path": rel, "content": "workspace\n"})
		body, err := os.ReadFile(filepath.Join(workspace, rel))
		if err != nil {
			t.Errorf("read the workspace file: %v", err)
		} else if string(body) != "workspace\n" {
			t.Errorf("workspace content = %q, want workspace", body)
		}
		if _, err := os.Stat(filepath.Join(cwd, rel)); !os.IsNotExist(err) {
			t.Errorf("relative write_file also touched the process cwd: %v", err)
		}
	})

	t.Run("write_file absolute", func(t *testing.T) {
		path := filepath.Join(cwd, "absolute.txt")
		executeClawFileTool(t, reg, "write_file", map[string]any{"path": path, "content": "absolute\n"})
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "absolute\n" {
			t.Errorf("absolute content = %q, want absolute", body)
		}
	})

	t.Run("file_edit relative", func(t *testing.T) {
		const rel = "relative-edit.txt"
		workspacePath := filepath.Join(workspace, rel)
		cwdPath := filepath.Join(cwd, rel)
		if err := os.WriteFile(workspacePath, []byte("before workspace"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cwdPath, []byte("before cwd"), 0o600); err != nil {
			t.Fatal(err)
		}
		executeClawFileTool(t, reg, "file_edit", map[string]any{
			"file_path": rel, "old_string": "before", "new_string": "after",
		})
		body, err := os.ReadFile(workspacePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "after workspace" {
			t.Errorf("workspace edit = %q, want %q", body, "after workspace")
		}
		cwdBody, err := os.ReadFile(cwdPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(cwdBody) != "before cwd" {
			t.Errorf("relative file_edit touched the process cwd: %q", cwdBody)
		}
	})

	t.Run("file_edit absolute", func(t *testing.T) {
		path := filepath.Join(cwd, "absolute-edit.txt")
		if err := os.WriteFile(path, []byte("before absolute"), 0o600); err != nil {
			t.Fatal(err)
		}
		executeClawFileTool(t, reg, "file_edit", map[string]any{
			"file_path": path, "old_string": "before", "new_string": "after",
		})
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "after absolute" {
			t.Errorf("absolute edit = %q, want %q", body, "after absolute")
		}
	})
}
