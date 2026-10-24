package runner

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

type nodeDeathStore struct {
	cancelAwareStore
	outcomes int
}

func (s *nodeDeathStore) UpdateRunOutcome(_ context.Context, _ string, status store.RunStatus, _ string, _ store.RunOutcomeMeta, _ []store.RunStatus) (bool, error) {
	s.outcomes++
	s.run.Status = status
	return true, nil
}

func assertNodeDeathPark(t *testing.T, caps usagecap.Store, wf *ir.Workflow, vars map[string]any, wantPark bool) {
	t.Helper()
	s := &nodeDeathStore{cancelAwareStore: cancelAwareStore{run: &store.Run{ID: "node-death", Status: store.RunStatusQueued}}}
	r := capRunner(capTestPolicy(), caps, s)
	ctx := forfaitBesidePinnedKeys()
	err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: s.run.ID, Vars: vars}, iterlog.Nop())
	if (err != nil) != wantPark {
		t.Errorf("pre-flight error = %v, want park=%v", err, wantPark)
	}
	if (s.outcomes != 0) != wantPark {
		t.Errorf("pre-flight writes %d outcomes, want park=%v", s.outcomes, wantPark)
	}
	got := r.armUsageWindowRetry(ctx, err, s.run.ID, spentNothing, iterlog.Nop())
	if (got == usageRetryArmed) != wantPark || s.armed != wantPark {
		t.Errorf("retry outcome=%v armed=%v, want park=%v", got, s.armed, wantPark)
	}
}

type nodeDeathExecutor struct{ calls map[string]int }

func (x nodeDeathExecutor) Execute(_ context.Context, n ir.Node, _ map[string]any) (map[string]any, error) {
	x.calls[n.NodeID()]++
	return map[string]any{}, nil
}

// W2: C012 accepts the bare bounded-iteration fallback, but after its budget
// is spent the launch has no exit. Mutation: count bounded iterations as
// keptExit in walkable; the global safeguard then discards the prune and
// the HARD cap parks the run. The engine witness proves actual exhaustion.
func TestUsageCapPreflight_SpentIterationKeepsThePrune(t *testing.T) {
	blankAnthropicWireEnv(t)
	src, err := os.ReadFile("testdata/walk-node-death.bot")
	if err != nil {
		t.Fatal(err)
	}
	pr := parsertest.Parse("walk-node-death.bot", string(src))
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatal(d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError || d.Code == ir.DiagMissingFallback {
			t.Fatal(d.Error())
		}
	}
	wf := cr.Workflow
	vars := map[string]any{"family": "gpt"}
	open := wf.LaunchVarEdgeFilter(vars)
	if open == nil || reaches(wf, open, "capped") {
		t.Fatal("spent iteration discards the prune or restores the excluded exit")
	}
	if !wf.AlwaysReachesLLM() || !reaches(wf, open, "warmup") {
		t.Fatal("premise: the iteration body spends before the node dies")
	}
	caps := usagecap.NewMemStore()
	weekCapped(t, caps, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))
	assertNodeDeathPark(t, caps, wf, vars, false)
	assertNodeDeathPark(t, caps, wf, map[string]any{"family": "claude"}, true)

	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	x := nodeDeathExecutor{calls: map[string]int{}}
	eng := runtime.New(wf, s, x, runtime.WithWorkDir(gittest.SourceRepo(t)))
	err = eng.Run(t.Context(), "spent-iteration", vars)
	var rtErr *runtime.RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != runtime.ErrCodeLoopExhausted {
		t.Fatalf("engine error=%v, want LOOP_EXHAUSTED", err)
	}
	if x.calls["warmup"] != 3 || x.calls["pass"] != 3 || x.calls["capped"] != 0 {
		t.Fatalf("executed %v, want two back-edge crossings and no capped call", x.calls)
	}
}
