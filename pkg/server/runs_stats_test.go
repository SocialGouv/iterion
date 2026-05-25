package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestAggregateRunStatsBucketsCostByEventDay(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc, err := runview.NewService("", runview.WithStore(rs))
	if err != nil {
		t.Fatalf("runview.NewService: %v", err)
	}

	created := time.Date(2026, 5, 1, 23, 30, 0, 0, time.UTC)
	nextDay := created.Add(2 * time.Hour)
	finished := nextDay.Add(time.Minute)
	run, err := rs.CreateRun(ctx, "run-1", "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	run.CreatedAt = created
	run.UpdatedAt = finished
	run.FinishedAt = &finished
	run.Status = store.RunStatusFinished
	if err := rs.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if _, err := rs.AppendEvent(ctx, run.ID, store.Event{
		Type:      store.EventNodeFinished,
		Timestamp: created,
		Data:      map[string]interface{}{"_cost_usd": 1.25},
	}); err != nil {
		t.Fatalf("AppendEvent day 1: %v", err)
	}
	if _, err := rs.AppendEvent(ctx, run.ID, store.Event{
		Type:      store.EventNodeFinished,
		Timestamp: nextDay,
		Data:      map[string]interface{}{"_cost_usd": 2.50},
	}); err != nil {
		t.Fatalf("AppendEvent day 2: %v", err)
	}

	out := aggregateRunStats(ctx, svc, []runview.RunSummary{{
		ID:           run.ID,
		WorkflowName: run.WorkflowName,
		Status:       run.Status,
		CreatedAt:    run.CreatedAt,
		UpdatedAt:    run.UpdatedAt,
		FinishedAt:   run.FinishedAt,
	}}, 30)

	if len(out.CostByDay) != 2 {
		t.Fatalf("want 2 cost buckets, got %+v", out.CostByDay)
	}
	if out.CostByDay[0].Day != "2026-05-01" || out.CostByDay[0].Total != 1.25 {
		t.Fatalf("first bucket = %+v", out.CostByDay[0])
	}
	if out.CostByDay[1].Day != "2026-05-02" || out.CostByDay[1].Total != 2.50 {
		t.Fatalf("second bucket = %+v", out.CostByDay[1])
	}
	if len(out.Workflows) != 1 || out.Workflows[0].TotalCostUSD != 3.75 {
		t.Fatalf("workflow totals = %+v", out.Workflows)
	}
}
