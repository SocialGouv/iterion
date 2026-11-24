package botregistry

import (
	"path/filepath"
	"testing"
)

func TestListWithSchemaFindsAPresetsOnlyBundle(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "planner")
	writeWalkFile(t, filepath.Join(dir, "main.bot"), "vars:\n  task: string = \"default\"\n\nworkflow planner {}\n")
	writeWalkFile(t, filepath.Join(dir, "presets", "sre.md"), "---\nname: sre\nvars:\n  task: audit\n---\nAudit the repository.\n")
	entries, err := ListWithSchema(ListOptions{Paths: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != dir {
		t.Fatalf("presets-only directory is not discovered as one bundle: %+v", entries)
	}
	entry := entries[0]
	if entry.SchemaError != "" || entry.Presets == nil || len(entry.Presets.Entries) != 1 {
		t.Fatalf("discovery lost the file preset: %+v", entry)
	}
	if p := entry.Presets.Entries[0]; p.Name != "sre" || p.Prompt != "Audit the repository." {
		t.Fatalf("preset body did not reach the launch surface: %+v", p)
	}
}
