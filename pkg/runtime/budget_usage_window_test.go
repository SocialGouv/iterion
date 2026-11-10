package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The Egapro shape (#3127): a review did real work, then parked on a provider
// usage window, and was resumed again and again while the window stayed shut.
// Every resume re-met the wall at zero tokens and still cost ~10 active
// minutes, so after twelve of them the run died on "budget hard limit
// reached: duration at 90%" days before the window reopened. The minutes an
// attempt spends only meeting the wall leave max_duration; the work stays
// counted.
func TestUsageWindowParks_DoNotSpendTheDurationBudget(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "usage_window_duration",
		Entry: "prepare",
		Nodes: map[string]ir.Node{
			"prepare": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "prepare"}},
			"review":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "prepare", To: "review"}, {From: "review", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxDuration: "120m"},
	}
	const work = 30 * time.Minute
	const blockedAttempt = 10 * time.Minute
	const attempts = 12

	clock := newBudgetTestClock()
	walled := true
	exec := newStubExecutor()
	exec.on("prepare", func(map[string]any) (map[string]any, error) {
		clock.Advance(work)
		return map[string]any{"ok": true, "_tokens": 1200}, nil
	})
	exec.on("review", func(map[string]any) (map[string]any, error) {
		clock.Advance(blockedAttempt)
		if walled {
			return nil, &delegate.ErrRateLimited{
				Provider: "zai",
				Detail:   "429 · [1310] Weekly Limit Exhausted. Your limit will reset at 2026-11-14 17:24:00",
				Kind:     delegate.RateLimitKindUsageWindow,
				ResetAt:  time.Date(2026, 11, 14, 17, 24, 0, 0, time.UTC),
			}
		}
		return map[string]any{"ok": true, "_tokens": 800}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	eng.budgetClock = clock.Now
	const runID = "run-egapro-shape"
	ctx := context.Background()

	err := eng.Run(ctx, runID, nil)
	for i := 0; i < attempts; i++ {
		var rtErr *RuntimeError
		if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeUsageLimitBlocked {
			t.Fatalf("attempt %d ended on %v, want a stop on the usage window — a parked run must not reach its duration cap", i+1, err)
		}
		clock.Advance(time.Hour) // parked between attempts: never counted
		err = eng.Resume(ctx, runID, nil)
	}
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeUsageLimitBlocked {
		t.Fatalf("the last blocked attempt ended on %v, want the usage window", err)
	}
	r, lerr := s.LoadRun(ctx, runID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if r.Checkpoint == nil {
		t.Fatal("the parked run left no checkpoint")
	}
	if got := time.Duration(r.Checkpoint.BudgetElapsedNS); got != work {
		t.Fatalf("after %d parks the duration budget carries %s, want the %s of real work only — an attempt that only met the wall must leave max_duration, and the work must stay counted",
			attempts+1, got, work)
	}
	refunds := 0
	events, lerr := s.LoadEvents(ctx, runID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	for _, ev := range events {
		if ev.Type == store.EventRunFailed {
			if ms, ok := ev.Data["duration_refunded_ms"].(float64); ok && time.Duration(ms)*time.Millisecond == blockedAttempt {
				refunds++
			}
		}
	}
	// The first stop refunds the walled node's own span after the work; each
	// resume refunds its whole attempt.
	if refunds != attempts+1 {
		t.Errorf("run_failed events naming a %s refund = %d, want %d — the operator reads on the timeline that the minutes left the budget",
			blockedAttempt, refunds, attempts+1)
	}

	// The window reopens: the run finishes under its own budget.
	walled = false
	if err := eng.Resume(ctx, runID, nil); err != nil {
		t.Fatalf("the resume after the window reopened failed: %v", err)
	}
}
