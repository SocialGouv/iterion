package botregistry

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sampleBot = `## ---
## name: sample
## ---

vars:
  workspace_dir: string = "/tmp"
  scope_notes: string = ""
  loop_cap: int = 5
  dry_run: bool = false

agent a:
  model: "test"

workflow w:
  vars:
    workspace_dir: string = "/tmp"
    scope_notes: string = ""
    loop_cap: int = 5
    dry_run: bool = false
  a -> done
`

func TestLoadSchema_ExtractsVars(t *testing.T) {
	ClearSchemaCache()
	dir := t.TempDir()
	p := filepath.Join(dir, "sample.bot")
	writeFile(t, p, sampleBot)

	vars, _, err := LoadSchema(Entry{Path: p, Name: "sample"})
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	if vars == nil || len(vars.Fields) != 4 {
		t.Fatalf("expected 4 vars, got %+v", vars)
	}
	want := map[string]string{
		"workspace_dir": "string",
		"scope_notes":   "string",
		"loop_cap":      "int",
		"dry_run":       "bool",
	}
	for _, f := range vars.Fields {
		if want[f.Name] != f.Type {
			t.Errorf("field %s type = %q, want %q", f.Name, f.Type, want[f.Name])
		}
	}
}

func TestLoadSchema_ParseErrorSurfaces(t *testing.T) {
	ClearSchemaCache()
	dir := t.TempDir()
	p := filepath.Join(dir, "broken.bot")
	writeFile(t, p, "this is not a valid iterion source @@@\n")
	// We never want LoadSchema to panic on garbage; it should either
	// return nil vars or a non-nil error.
	vars, _, err := LoadSchema(Entry{Path: p, Name: "broken"})
	if err == nil && vars != nil && len(vars.Fields) > 0 {
		t.Fatalf("expected no usable schema for broken source, got %+v", vars)
	}
}

func TestLoadSchema_CacheHitAndInvalidate(t *testing.T) {
	ClearSchemaCache()
	dir := t.TempDir()
	p := filepath.Join(dir, "evolving.bot")
	writeFile(t, p, sampleBot)

	v1, _, err := LoadSchema(Entry{Path: p, Name: "evolving"})
	if err != nil || v1 == nil {
		t.Fatalf("first LoadSchema: %v %v", v1, err)
	}
	// Second call should hit the cache and return identical pointer
	// content (same underlying slice).
	v2, _, _ := LoadSchema(Entry{Path: p, Name: "evolving"})
	if v2 == nil || len(v2.Fields) != len(v1.Fields) {
		t.Fatalf("cache hit lost fields: %v", v2)
	}

	// Rewrite with different vars; bump mtime explicitly to defeat
	// filesystem timestamp granularity.
	updated := `## ---
## name: evolving
## ---

workflow w:
  vars:
    only_one: string = "hi"
  agent a:
    model: "test"
  a -> done

agent a:
  model: "test"
`
	if err := os.WriteFile(p, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}

	v3, _, err := LoadSchema(Entry{Path: p, Name: "evolving"})
	if err != nil {
		t.Fatalf("after-mutate LoadSchema: %v", err)
	}
	if v3 == nil {
		t.Fatal("expected new schema after rewrite, got nil")
	}
	if len(v3.Fields) == len(v1.Fields) && v3.Fields[0].Name != "only_one" {
		t.Fatalf("cache did not invalidate: %+v", v3.Fields)
	}
}

func TestListWithSchema_FoldsVars(t *testing.T) {
	ClearSchemaCache()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sample.bot"), sampleBot)

	entries, err := ListWithSchema(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatalf("ListWithSchema: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	e := entries[0]
	if e.Name != "sample" {
		t.Errorf("Name = %q", e.Name)
	}
	if e.Vars == nil || len(e.Vars.Fields) == 0 {
		t.Errorf("expected vars on schema-augmented entry, got %+v", e)
	}
	if e.SchemaError != "" {
		t.Errorf("unexpected SchemaError: %s", e.SchemaError)
	}
}
