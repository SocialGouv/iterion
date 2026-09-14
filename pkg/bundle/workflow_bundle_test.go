package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenForWorkflowFindsExportedChild(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "workflows", "child.bot")
	if err := os.Mkdir(filepath.Dir(child), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, MainBotFile), []byte("workflow main {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(child, []byte("workflow child {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte("name: sample\nschema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := OpenForWorkflow(child)
	if err != nil {
		t.Fatalf("OpenForWorkflow: %v", err)
	}
	if b == nil || b.Dir != dir {
		t.Fatalf("bundle = %+v", b)
	}
}
