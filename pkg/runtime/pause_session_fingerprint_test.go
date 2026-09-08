package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The id a pause records is only half of what a resume needs to USE that
// session. The backend drops a `session: fork` whose parent provider it
// cannot identify — the conservative guard against cross-provider
// thinking-block 400s — so a checkpoint carrying the id and not the
// fingerprint hands the resume a session it is then forbidden to open, and
// the node silently starts fresh on exactly the path that asked for
// continuity.
func TestPauseCarriesTheFingerprintOfTheSessionItRecords(t *testing.T) {
	wf := interactionWorkflow(ir.InteractionHuman)
	wf.Nodes["worker"] = &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "worker"},
		Session:           ir.SessionFork,
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
	}

	var resumeInput map[string]any
	calls := 0
	exec := newStubExecutor()
	exec.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, &model.ErrNeedsInteraction{
				NodeID:             "worker",
				Questions:          map[string]any{delegate.AskUserQuestionKey: "ok?"},
				SessionID:          "sess-ask",
				SessionFingerprint: "anthropic-oauth",
				Backend:            "claude_code",
			}
		}
		resumeInput = in
		return map[string]any{"text": "done", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fp", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("want paused, got %v", err)
	}

	r, err := s.LoadRun(context.Background(), "run-fp")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint == nil {
		t.Fatal("pause wrote no checkpoint")
	}
	if r.Checkpoint.BackendSessionFingerprint != "anthropic-oauth" {
		t.Fatalf("checkpoint fingerprint = %q, want anthropic-oauth — an id without it names a session the fork guard refuses",
			r.Checkpoint.BackendSessionFingerprint)
	}

	if err := eng.Resume(context.Background(), "run-fp", map[string]any{delegate.AskUserQuestionKey: "yes"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got, _ := resumeInput[delegate.SessionIDKey].(string); got != "sess-ask" {
		t.Fatalf("re-invocation _session_id = %q, want sess-ask", got)
	}
	if got, _ := resumeInput[delegate.SessionFingerprintKey].(string); got != "anthropic-oauth" {
		t.Fatalf("re-invocation _session_fingerprint = %q, want anthropic-oauth — the fork this resumes is dropped without it", got)
	}
	// And the id is declared best-effort: the transcript behind it lives
	// on the host that ran the node, and this resume can land on another
	// one. Without the marker a `session: fork`/`inherit` node re-issues
	// `--resume <gone>` and fails identically forever.
	if opt, _ := resumeInput[delegate.SessionOptionalKey].(bool); !opt {
		t.Fatalf("re-invocation _session_optional = %v, want true — a pause can outlive the host that holds its transcript", resumeInput[delegate.SessionOptionalKey])
	}
}

// The pause's id is THIS node's own session, so an upstream fingerprint
// left beside it would describe a different one — and a fingerprint that
// happens to match would wave through precisely the cross-provider fork the
// guard exists to refuse. A pause that cannot name its provider therefore
// clears the stale value rather than letting it stand in.
func TestPauseWithoutAFingerprintClearsTheUpstreamOne(t *testing.T) {
	wf := interactionWorkflow(ir.InteractionHuman)
	wf.Nodes["worker"] = &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "worker"},
		Session:           ir.SessionFork,
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
	}
	wf.Nodes["seed"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "seed"}}
	wf.Entry = "seed"
	wf.Edges = []*ir.Edge{
		{From: "seed", To: "worker", With: []*ir.DataMapping{
			{Key: delegate.SessionFingerprintKey, Raw: "from-upstream"},
		}},
		{From: "worker", To: "done"},
	}

	var resumeInput map[string]any
	calls := 0
	exec := newStubExecutor()
	exec.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			if got, _ := in[delegate.SessionFingerprintKey].(string); got != "from-upstream" {
				t.Fatalf("edge did not carry the fingerprint: %q", got)
			}
			return nil, &model.ErrNeedsInteraction{
				NodeID:    "worker",
				Questions: map[string]any{delegate.AskUserQuestionKey: "ok?"},
				SessionID: "sess-ask",
				Backend:   "claude_code",
			}
		}
		resumeInput = in
		return map[string]any{"text": "done", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fp-stale", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("want paused, got %v", err)
	}
	if err := eng.Resume(context.Background(), "run-fp-stale", map[string]any{delegate.AskUserQuestionKey: "yes"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got, ok := resumeInput[delegate.SessionFingerprintKey]; ok {
		t.Fatalf("_session_fingerprint = %v, want absent — it described the upstream session, not the one the pause recorded", got)
	}
}
