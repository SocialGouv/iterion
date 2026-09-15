package runtime

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sources a run records are one decision for the pair: the files the
// launch compiled when the launcher handed them over — a studio run's
// FilePath is the store's copy of its main, beside which no fragment lives
// — main first, the rest by path; a unit of one file records its main
// alone; a unit past the cap records nothing, not the main either.
func TestRecordedSourcesFollowTheCompile(t *testing.T) {
	copyDir := t.TempDir()
	copyPath := filepath.Join(copyDir, "a1b2c3-main.bot")
	main := "import \"lib/nodes.bot\"\n\nworkflow w:\n  entry: a\n  a -> done\n"
	if err := os.WriteFile(copyPath, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	// The copy alone, as the studio used to hand it over: the unit beside
	// it does not load, so the main is recorded as a file that is not a
	// unit — the state a run of a bot in several files must never be in
	// when the launch read its fragments.
	e := &Engine{filePath: copyPath}
	if src, files := e.recordedSources(); src != main || files != nil {
		t.Fatalf("copy alone: src=%q files=%v", src, files)
	}
	e = &Engine{filePath: copyPath, compiledMain: "main.bot", compiledFiles: map[string]string{
		"main.bot": main, "lib/nodes.bot": "agent a:\n  model: \"m\"\n", "lib/a.bot": "schema s:\n  ok: bool\n",
	}}
	src, files := e.recordedSources()
	if src != main || len(files) != 3 || files[0].Path != "main.bot" || files[1].Path != "lib/a.bot" || files[2].Path != "lib/nodes.bot" {
		t.Fatalf("compiled unit: src=%q files=%+v", src, files)
	}
	e = &Engine{compiledMain: "main.bot", compiledFiles: map[string]string{"main.bot": main}}
	if src, files := e.recordedSources(); src != main || files != nil {
		t.Fatalf("a unit of one file: src=%q files=%v", src, files)
	}
	big := strings.Repeat("x", maxPersistedWorkflowSource/2+1)
	e = &Engine{compiledMain: "main.bot", compiledFiles: map[string]string{"main.bot": main, "lib/a.bot": big, "lib/b.bot": big}}
	if src, files := e.recordedSources(); src != "" || files != nil {
		t.Fatalf("a unit past the cap recorded its main without its fragments: src=%d bytes files=%v", len(src), files)
	}
	// The same cap holds a unit read beside filePath.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("import \"lib/a.bot\"\nimport \"lib/b.bot\"\n\nworkflow w:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, "lib", name+".bot"), []byte("## "+big+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e = &Engine{filePath: filepath.Join(dir, "main.bot")}
	if src, files := e.recordedSources(); src != "" || files != nil {
		t.Fatalf("a unit on disk past the cap: src=%d bytes files=%v", len(src), files)
	}
	// The text the caller supplied wins over the path, as ever.
	e = &Engine{filePath: copyPath, workflowSource: "workflow w:\n  entry: done\n"}
	if src, files := e.recordedSources(); src != "workflow w:\n  entry: done\n" || files != nil {
		t.Fatalf("supplied text: src=%q files=%v", src, files)
	}
}

// A run recorded before the files were sorted holds them in import order:
// the same files in another order are the same sources, not a change.
func TestSameSourceFilesIgnoresOrder(t *testing.T) {
	a := []store.WorkflowSourceFile{{Path: "main.bot", Text: "m"}, {Path: "lib/nodes.bot", Text: "n"}, {Path: "lib/a.bot", Text: "a"}}
	b := []store.WorkflowSourceFile{{Path: "main.bot", Text: "m"}, {Path: "lib/a.bot", Text: "a"}, {Path: "lib/nodes.bot", Text: "n"}}
	if !sameSourceFiles(a, b) {
		t.Fatal("the same files in another order read as changed")
	}
	b[1].Text = "changed"
	if sameSourceFiles(a, b) {
		t.Fatal("a changed fragment read as the same sources")
	}
	if sameSourceFiles(a, a[:2]) || !sameSourceFiles(nil, nil) {
		t.Fatal("length is part of sameness")
	}
	// The restamp of an unchanged unit recorded in the old order touches
	// nothing.
	r := &store.Run{WorkflowSource: "m", WorkflowSources: a, WorkflowHash: "old"}
	e := &Engine{compiledMain: "main.bot", compiledFiles: map[string]string{"main.bot": "m", "lib/nodes.bot": "n", "lib/a.bot": "a"}, workflowHash: "new"}
	e.restampWorkflowSource(context.Background(), r)
	if r.WorkflowHash != "old" || len(r.WorkflowSources) != 3 || r.WorkflowSources[1].Path != "lib/nodes.bot" {
		t.Fatalf("an unchanged unit was restamped: hash %q files %v", r.WorkflowHash, r.WorkflowSources)
	}
}
