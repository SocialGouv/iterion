package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

// An engine that has no source of its own leaves the one already on the run.
//
// This is what makes a cloud launch's stamp worth writing. The publisher puts
// the unit's files on the queued document because nothing downstream can: the
// queue message carries the compiled IR and the identity hash, never the text,
// and a runner pod has no filesystem the launch ever touched. The engine that
// then claims the row builds from the IR alone — no file path, no source, no
// compiled files — so if it wrote what IT knows, it would replace the launch's
// record with nothing and `rewind --auto` would be back to refusing every cloud
// run (#1226).
func TestRunLeavesARecordedSourceTheEngineCannotKnow(t *testing.T) {
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &queuedCreatingStore{FilesystemRunStore: fs}
	src := `schema out:
  ok: bool

tool work:
  command: ` + "`printf '{\"ok\":true}'`" + `
  output: out

workflow w:
  entry: work
  work -> done
`
	pr := parser.Parse("w.bot", src)
	if pr.File == nil {
		t.Fatalf("parse: %v", pr.Diagnostics)
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatalf("compile: %v", cr.Diagnostics)
	}

	const (
		runID     = "cloud-stamped"
		mainText  = "workflow w:\n  entry: work\n  work -> done\n"
		fragText  = "tool work:\n  command: `true`\n"
		mainPath  = "main.bot"
		fragPath  = "lib/nodes.bot"
		queueHash = "hash-from-the-publisher"
	)
	ctx := context.Background()
	// The queued row exactly as SubmitLaunch leaves it.
	if _, err := st.CreateRun(ctx, runID, "w", nil); err != nil {
		t.Fatal(err)
	}
	seeded, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	seeded.WorkflowHash = queueHash
	seeded.WorkflowSource = mainText
	seeded.WorkflowSources = []store.WorkflowSourceFile{
		{Path: mainPath, Text: mainText},
		{Path: fragPath, Text: fragText},
	}
	if err := st.SaveRun(ctx, seeded); err != nil {
		t.Fatal(err)
	}

	exec := newStubExecutor()
	exec.on("work", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	// A runner's engine: the IR and the hash, nothing that names a file.
	eng := New(cr.Workflow, st, exec,
		WithWorkDir(t.TempDir()), WithSandboxOverride("none"), WithWorkflowHash(queueHash))
	if err := eng.Run(ctx, runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	final, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if final.WorkflowSource != mainText {
		t.Errorf("workflow_source after the run = %q, want the text the launch recorded — the engine overwrote what it could not know", final.WorkflowSource)
	}
	if len(final.WorkflowSources) != 2 ||
		final.WorkflowSources[0].Path != mainPath ||
		final.WorkflowSources[1].Path != fragPath {
		t.Errorf("workflow_sources after the run = %+v, want the unit the launch recorded, main first", final.WorkflowSources)
	}
}
