package trigger

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestBuildRunOutcomeKindsWithoutRun(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()

	cases := []struct {
		name    string
		bodyErr error
		want    string
	}{
		{"finished", nil, KindRunFinished},
		{"failed", errors.New("boom"), KindRunFailed},
		{"cancelled", runtime.ErrRunCancelled, KindRunCancelled},
		{"paused_human", runtime.ErrRunPaused, KindRunPaused},
		{"paused_operator", runtime.ErrRunPausedOperator, KindRunPaused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := BuildRunOutcome(ctx, st, "missing-run", tc.bodyErr)
			if ev.Kind != tc.want {
				t.Fatalf("kind = %q, want %q", ev.Kind, tc.want)
			}
			if ev.Source != SourceRun || ev.Subject.ID != "missing-run" {
				t.Fatalf("unexpected event: %+v", ev)
			}
			// Load-resilient: the bare event must still carry a usable ID.
			if ev.ID != "run:missing-run" {
				t.Fatalf("ID = %q, want run:missing-run", ev.ID)
			}
		})
	}
}

func TestBuildRunOutcomeEnrichment(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()

	r, err := st.CreateRun(ctx, "run-enrich", "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r.TenantID = "team-1"
	r.OwnerID = "user-1"
	r.BotID = "bot-1"
	r.Name = "My run"
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := st.PauseRun(ctx, "run-enrich", &store.Checkpoint{
		NodeID:        "ask",
		InteractionID: "run-enrich_ask",
	}); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}

	ev := BuildRunOutcome(ctx, st, "run-enrich", runtime.ErrRunPaused)
	if ev.Kind != KindRunPaused {
		t.Fatalf("kind = %q, want run.paused", ev.Kind)
	}
	if ev.TenantID != "team-1" {
		t.Fatalf("tenant = %q, want team-1", ev.TenantID)
	}
	if got := ev.Payload["owner_id"]; got != "user-1" {
		t.Fatalf("owner_id = %v, want user-1", got)
	}
	if got := ev.Payload["interaction_id"]; got != "run-enrich_ask" {
		t.Fatalf("interaction_id = %v, want run-enrich_ask", got)
	}
	if got := ev.Payload["node_id"]; got != "ask" {
		t.Fatalf("node_id = %v, want ask", got)
	}
	if ev.Subject.Title != "My run" {
		t.Fatalf("title = %q, want My run", ev.Subject.Title)
	}
	// Per-episode ID: a pause dedups on its interaction, not the bare run.
	if ev.ID != "run:run-enrich:run-enrich_ask" {
		t.Fatalf("ID = %q, want run:run-enrich:run-enrich_ask", ev.ID)
	}

	// A later terminal outcome yields a distinct episode key.
	if err := st.UpdateRunStatus(ctx, "run-enrich", store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	ev2 := BuildRunOutcome(ctx, st, "run-enrich", nil)
	if ev2.Kind != KindRunFinished {
		t.Fatalf("kind = %q, want run.finished", ev2.Kind)
	}
	if ev2.ID == ev.ID {
		t.Fatalf("episode IDs must differ: %q", ev2.ID)
	}
}
