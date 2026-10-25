package cloudpublisher

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F18: mutate marshalAttemptIR to reread FilePath. The executable AST then
// differs from the immutable source attached to the same queue message.
func TestAttemptQueueUsesSameRevision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.bot")
	src := "schema out:\n  ok: bool\n\ntool old:\n  command: `true`\n  output: out\n\nworkflow provenance:\n  entry: old\n  old -> done\n"
	if err := os.WriteFile(path, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	wf, hash, err := runview.CompileWorkflowWithHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(src, "old", "new")), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var msg *queue.RunMessage
	p := &Publisher{store: st, publishRun: func(_ context.Context, m *queue.RunMessage) error { msg = m; return nil }}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	_, err = p.SubmitLaunch(ctx, "review", runview.LaunchSpec{FilePath: path}, wf, &runview.CompiledSource{Hash: hash, Main: "main.bot", Files: map[string]string{"main.bot": src}})
	if err != nil {
		t.Fatal(err)
	}
	assertMessage := func() {
		t.Helper()
		file, err := ast.UnmarshalFile(msg.IRCompiled)
		if err != nil {
			t.Fatal(err)
		}
		executed := ir.Compile(file)
		if executed.HasErrors() {
			t.Fatal(executed.Diagnostics)
		}
		source, err := runview.SourceSnapshotFromTransport(msg.IRCompiled)
		if err != nil {
			t.Fatal(err)
		}
		_, err = st.RecordAttempt(ctx, "review", &store.AttemptSnapshot{WorkflowHash: hash, Source: source})
		if err != nil {
			t.Fatal(err)
		}
		graph, err := runview.BuildWireWorkflowFromStore(ctx, st, "review")
		if err != nil {
			t.Fatal(err)
		}
		if graph.Entry != "old" || graph.Entry != executed.Workflow.Entry {
			t.Fatalf("PROVENANCE MISMATCH: queued executing entry=%q; immutable snapshot/projected entry=%q", executed.Workflow.Entry, graph.Entry)
		}
	}
	assertMessage()
	if err := st.FailRunResumable(ctx, "review", &store.Checkpoint{NodeID: "old"}, "transient", ""); err != nil {
		t.Fatal(err)
	}
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: "review", FilePath: path, Force: true}, wf, &runview.CompiledSource{Hash: hash, Main: "main.bot", Files: map[string]string{"main.bot": src}}); err != nil {
		t.Fatal(err)
	}
	assertMessage()
}
