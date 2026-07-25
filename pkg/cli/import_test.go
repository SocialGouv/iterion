package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const importFixtureJS = `export const meta = { name: 'tiny-flow', description: 'one agent' }
const r = await agent('Say hi and stop.', { label: 'greeter' })
return { r }
`

func TestRunImport_WritesDraft(t *testing.T) {
	dir := t.TempDir()
	js := filepath.Join(dir, "tiny.js")
	if err := os.WriteFile(js, []byte(importFixtureJS), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputHuman}
	if err := RunImport(ImportOptions{File: js}, p); err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	out := filepath.Join(dir, "tiny_flow.bot")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("draft not written: %v", err)
	}
	for _, want := range []string{"## IMPORT REPORT", "workflow tiny_flow:", "agent greeter:", "greeter -> done"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("draft missing %q", want)
		}
	}

	// Second run must refuse to overwrite.
	if err := RunImport(ImportOptions{File: js}, p); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("expected overwrite refusal, got %v", err)
	}
}

func TestRunImport_DryRunAndGates(t *testing.T) {
	dir := t.TempDir()
	js := filepath.Join(dir, "tiny.js")
	if err := os.WriteFile(js, []byte(importFixtureJS), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputHuman}
	if err := RunImport(ImportOptions{File: js, DryRun: true}, p); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(buf.String(), "workflow tiny_flow:") {
		t.Error("dry-run should print the draft")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Error("dry-run must not write files")
	}

	// Wrong extension is a clear user error, not a JS parse error.
	if err := RunImport(ImportOptions{File: filepath.Join(dir, "x.bot")}, p); err == nil || !strings.Contains(err.Error(), "expects a workflow script") {
		t.Fatalf("expected extension gate, got %v", err)
	}
}
