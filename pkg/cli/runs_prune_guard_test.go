package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestRunPrune_EveryPrunableStatusIsTerminal: prune's closed --status set
// IS its lifecycle guard — the reason `prune --status paused_waiting_human`
// cannot mint a tombstone on a live run is that the flag refuses it. Pin
// the property so a status added to the set without being terminal fails
// here rather than in production (a tombstone on a live run is read as
// proof of absence by every board launch authority).
func TestRunPrune_EveryPrunableStatusIsTerminal(t *testing.T) {
	for name, st := range pruneAllowedStatuses {
		if !st.IsTerminal() {
			t.Fatalf("--status %q (%s) is prunable but not terminal — prune would tombstone a live run", name, st)
		}
	}
	for _, live := range []string{"running", "queued", "paused_waiting_human", "paused_operator"} {
		if _, err := validatePruneStatuses([]string{live}); err == nil {
			t.Fatalf("--status %q must be refused: a live run's tombstone reads as proof of absence", live)
		}
	}
}

// TestRunPrune_RefusesARunResumedSinceTheScan: the scan is a snapshot.
// A failed_resumable run (opt-in prunable) resumed between the scan and
// the delete is RUNNING when the tombstone lands — and that tombstone is
// read as proof of absence by the board launch authorities, which then
// admit a fresh run on the same work while the resumed engine dies on
// typed refusals. The delete re-reads the run and refuses a non-terminal
// one, the same guard runview.DeleteRunCtx applies to the HTTP and MCP
// deletes.
func TestRunPrune_RefusesARunResumedSinceTheScan(t *testing.T) {
	f := newPruneFixture(t)
	f.seedRun(t, "resumed", store.RunStatusFailedResumable, 48*time.Hour)
	f.seedRun(t, "old", store.RunStatusFinished, 48*time.Hour)
	ctx := context.Background()

	buf := &bytes.Buffer{}
	p := &Printer{W: buf, Format: OutputJSON}
	err := RunPrune(PruneOptions{
		StoreDir:  f.dir,
		OlderThan: 24 * time.Hour,
		Statuses:  []string{"finished", "failed_resumable"},
		Now:       func() time.Time { return f.now },
		beforeDelete: func(id string) {
			if id == "resumed" {
				// The operator (or the usage-window retry) resumes it in
				// the window.
				if err := f.store.UpdateRunStatus(ctx, id, store.RunStatusRunning, ""); err != nil {
					t.Fatal(err)
				}
			}
		},
	}, p)
	if err != nil {
		t.Fatalf("RunPrune: %v", err)
	}

	r, lerr := f.store.LoadRun(ctx, "resumed")
	if lerr != nil || r.Status != store.RunStatusRunning {
		t.Fatalf("REPRODUCED: the run resumed since the scan was tombstoned (LoadRun: %v) — its absence now reads as "+
			"proof nothing is alive while the engine keeps running", lerr)
	}
	if _, lerr := f.store.LoadRun(ctx, "old"); !errors.Is(lerr, store.ErrRunDeleted) {
		t.Fatalf("the terminal run must still be pruned, got %v", lerr)
	}
	var res PruneResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("decode result: %v\n%s", err, buf.String())
	}
	if len(res.Refused) != 1 || res.Refused[0] != "resumed" {
		t.Fatalf("the refusal must be reported, got refused=%v", res.Refused)
	}
	if res.PrunedCount != 1 {
		t.Fatalf("pruned count = %d, want 1 (only the terminal run)", res.PrunedCount)
	}
}
