package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A cloud resume is the only holder of the text it compiled: the queue message
// carries the IR and the identity hash, never the files, so the runner's engine
// records nothing of its own. Whatever this call leaves on the document is all
// `rewind --auto` will ever have.
//
// It cost the feature its second cycle. SubmitLaunch was repaired to stamp the
// pair; SubmitResume still dropped it, so the first FORCED cloud resume met a
// document whose hash named the previous revision and cleared the recorded
// source — deliberately, since a source beside another revision's hash is a
// false baseline. One rewind per run, then nothing, with no error anywhere.
func TestSubmitResume_RecordsTheSourceItCompiledWithItsOwnHash(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-resume-records-source"
	const launched = "workflow w:\n  entry: a\n  a -> done\n"
	if err := st.SaveRun(ctx, &store.Run{
		ID: runID, TenantID: "team", OwnerID: "alice",
		Status:          store.RunStatusPausedOperator,
		WorkflowHash:    "hash-at-launch",
		WorkflowSource:  launched,
		WorkflowSources: []store.WorkflowSourceFile{{Path: "main.bot", Text: launched}},
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	p := &Publisher{store: st, publishRun: func(context.Context, *queue.RunMessage) error { return nil }}
	const edited = "workflow w:\n  entry: a\n  a -> b\n  b -> done\n"
	const frag = "agent b:\n  model: \"claude-opus-5\"\n"
	cs := &runview.CompiledSource{
		Hash: "hash-of-this-resume",
		Main: "main.bot",
		Files: map[string]string{
			"main.bot":      edited,
			"lib/nodes.bot": frag,
		},
	}
	spec := runview.ResumeSpec{RunID: runID, FilePath: "main.bot", Source: edited}
	if err := p.SubmitResume(ctx, spec, &ir.Workflow{Name: "w"}, cs); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}

	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.WorkflowSource != edited {
		t.Errorf("recorded source = %q, want the text THIS resume compiled — a rewind would diff against the launch's", r.WorkflowSource)
	}
	if len(r.WorkflowSources) != 2 {
		t.Errorf("recorded files = %d, want the whole unit: a fragment left out makes --auto refuse rather than miss an edit", len(r.WorkflowSources))
	}
	// The pair is what disarms the engine's forced-resume clear: it drops a
	// source whose neighbouring hash names ANOTHER revision, so the two have
	// to be written by one call.
	if r.WorkflowHash != cs.Hash {
		t.Errorf("hash = %q, want %q beside the source it describes — a forced resume clears the pair when they disagree",
			r.WorkflowHash, cs.Hash)
	}
}
