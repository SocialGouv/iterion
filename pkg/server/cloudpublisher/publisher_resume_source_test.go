package cloudpublisher

import (
	"context"
	"errors"
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

// A resume that never reaches a runner must leave the rewind baseline exactly
// as it was.
//
// The stamp sits immediately before the publish, so every refusal above it —
// credentials, contributions, the budget patch — rolls the status back with
// nothing else written. A publish that fails lands after it, and the rollback
// puts the pair back: otherwise the document would describe an attempt that
// never ran, and the next `rewind --auto` would diff against a program no run
// executed. That drops too FEW nodes and leaves stale downstream state — the
// direction the feature exists to prevent, and silent, where a refusal is not.
func TestSubmitResume_ARefusedResumeLeavesTheRewindBaselineAlone(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-resume-refused"
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

	p := &Publisher{store: st, publishRun: func(context.Context, *queue.RunMessage) error {
		return errors.New("nats unavailable")
	}}
	const edited = "workflow w:\n  entry: a\n  a -> b\n  b -> done\n"
	cs := &runview.CompiledSource{Hash: "hash-never-ran", Main: "main.bot", Files: map[string]string{"main.bot": edited}}
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "main.bot", Source: edited},
		&ir.Workflow{Name: "w"}, cs); err == nil {
		t.Fatal("SubmitResume returned nil, want the publish failure")
	}

	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusPausedOperator {
		t.Errorf("status = %s, want paused_operator", r.Status)
	}
	if r.WorkflowSource != launched || r.WorkflowHash != "hash-at-launch" {
		t.Errorf("baseline = hash %q / %q, want the launch's — this attempt never reached a runner, so a "+
			"rewind must still target from the program that DID run", r.WorkflowHash, r.WorkflowSource)
	}
	if len(r.WorkflowSources) != 1 || r.WorkflowSources[0].Text != launched {
		t.Errorf("recorded files = %+v, want the launch's", r.WorkflowSources)
	}
}
