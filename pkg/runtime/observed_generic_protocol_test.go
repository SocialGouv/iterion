package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

type genericNoLegacySetter struct {
	*store.FilesystemRunStore
	legacy int
}

func (s *genericNoLegacySetter) SetRunOutputCorrection(context.Context, string, string, store.OutputCorrectionEpisode) error {
	s.legacy++
	return errors.New("unfenced legacy setter must not run")
}
func TestObservedGenericUsesCommonCASAndHoldsNeeds(t *testing.T) {
	fs, x := correctionFixture(t)
	s := &genericNoLegacySetter{FilesystemRunStore: fs}
	wf := validationWorkflow()
	wf.Resources = map[string]int{"single": 1}
	wf.Nodes["my_agent"].(*ir.AgentNode).Needs = []string{"single"}
	ex := &usageCorrectingExecutor{stubExecutor: newStubExecutor()}
	e := New(wf, s, ex, WithOutputValidation(true), WithOutputCorrectionBudget(2))
	rs := e.newRunState("correction", nil)
	ctx := x.context(context.Background())
	rs.ctx = ctx
	ex.on("my_agent", func(map[string]any) (map[string]any, error) {
		if len(rs.resourceSemaphores["single"]) != 0 {
			t.Fatal("initial did not hold declared needs")
		}
		return map[string]any{"summary": "initial", "score": "bad", "_tokens": 2}, nil
	})
	ex.correct = func(c context.Context, out map[string]any, _ error) (map[string]any, OutputCorrectionUsage, error) {
		if _, ok := model.OutputCorrectionOwnershipFromContext(c); ok {
			t.Fatal("generic adapter granted a producer handle to a nested call")
		}
		if len(rs.resourceSemaphores["single"]) != 0 {
			t.Fatal("generic round ran after needs release")
		}
		row, err := s.LoadRun(c, "correction")
		if err != nil {
			t.Fatal(err)
		}
		ep := row.OutputCorrections["my_agent"]
		if ep.Attempts != ex.calls || len(ep.Reservations) != ex.calls || ep.Reservations[ex.calls-1].Receipt != nil {
			t.Fatal("round started without its durable reservation")
		}
		candidate := map[string]any{"summary": "fixed", "score": 7}
		if ex.calls == 1 {
			candidate["score"] = "different-invalid"
		}
		return candidate, OutputCorrectionUsage{Tokens: 3}, nil
	}
	out, retry, err := e.execLoopRunNode(ctx, rs, "my_agent", wf.Nodes["my_agent"])
	if err != nil || retry || out["score"] != 7 || ex.calls != 2 || s.legacy != 0 {
		t.Fatalf("generic calls=%d legacy=%d retry=%v out=%v err=%v", ex.calls, s.legacy, retry, out, err)
	}
	if len(rs.resourceSemaphores["single"]) != 1 {
		t.Fatal("needs lease leaked after common loop")
	}
	if got := rs.budget.Status().Tokens; got != 8 {
		t.Fatalf("generic bill=%v want8", got)
	}
	row, err := s.LoadRun(ctx, "correction")
	if err != nil {
		t.Fatal(err)
	}
	ep := row.OutputCorrections["my_agent"]
	if ep.Status != "succeeded" || ep.Usage.Tokens != 6 {
		t.Fatalf("generic CAS episode %+v", ep)
	}
	events, err := s.LoadEvents(ctx, "correction")
	if err != nil {
		t.Fatal(err)
	}
	attempts, success := 0, 0
	for _, ev := range events {
		if ev.Data["strategy"] == "schema_output_correction" {
			if ev.Data["phase"] == "attempt" {
				attempts++
			}
			if ev.Data["phase"] == "succeeded" {
				success++
			}
		}
	}
	if attempts != 2 || success != 1 {
		t.Fatalf("phases attempt=%d success=%d", attempts, success)
	}
}
func TestObservedGenericCannotAcquireFromOutput(t *testing.T) {
	fs, _ := correctionFixture(t)
	ex := &usageCorrectingExecutor{stubExecutor: newStubExecutor()}
	e := New(validationWorkflow(), fs, ex, WithOutputValidation(true))
	rs := e.newRunState("correction", nil)
	before := readCorrection(t, fs)
	_, err := e.correctAndValidateNodeOutput(context.Background(), rs, "my_agent", e.workflow.Nodes["my_agent"], map[string]any{"summary": "bad", "score": "no"})
	if !errors.Is(err, errCorrectionOwnershipLost) || ex.calls != 0 {
		t.Fatalf("private adapter manufactured a grant: %v calls=%d", err, ex.calls)
	}
	after := readCorrection(t, fs)
	if before.ExecutionEpoch != after.ExecutionEpoch || len(after.InitialUsageReservations) != 0 {
		t.Fatal("private adapter changed authority")
	}
}
