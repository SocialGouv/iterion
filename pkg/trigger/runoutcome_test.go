package trigger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	// Per-episode ID: a pause dedups on its interaction (+ the pause
	// instant, so a later re-pause on the SAME interaction re-notifies).
	if !strings.HasPrefix(ev.ID, "run:run-enrich:run-enrich_ask:") {
		t.Fatalf("ID = %q, want run:run-enrich:run-enrich_ask:<updated-at>", ev.ID)
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

// TestRunOutcomeEventIDRepeatEpisodes pins the disambiguation that keeps a
// resumed-then-refailed run (same terminal status) and a re-pause on the
// same interaction from deduping against their earlier episode.
func TestRunOutcomeEventIDRepeatEpisodes(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)

	// Same terminal status, different attempt → different keys.
	if a, b := RunOutcomeEventID("r", "failed_resumable", "", t0), RunOutcomeEventID("r", "failed_resumable", "", t1); a == b {
		t.Fatalf("repeat failed_resumable episodes collide: %q", a)
	}
	// Same interaction, re-paused later → different keys.
	if a, b := RunOutcomeEventID("r", "paused_waiting_human", "r_ask", t0), RunOutcomeEventID("r", "paused_waiting_human", "r_ask", t1); a == b {
		t.Fatalf("repeat pause episodes collide: %q", a)
	}
	// Sub-second precision differences (store round-trips) must NOT split
	// one episode into two keys.
	if a, b := RunOutcomeEventID("r", "finished", "", t0), RunOutcomeEventID("r", "finished", "", t0.Add(300*time.Millisecond)); a != b {
		t.Fatalf("precision jitter split the episode: %q vs %q", a, b)
	}
	// Missing run (no status, zero time) stays the bare load-resilient key.
	if got := RunOutcomeEventID("r", "", "", time.Time{}); got != "run:r" {
		t.Fatalf("bare key = %q, want run:r", got)
	}
}
