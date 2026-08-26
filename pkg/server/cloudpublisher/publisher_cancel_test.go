package cloudpublisher

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A cancel's reason lands in run.Error — what the run list, board cards and
// merge-gate synthetic statuses all read. Pinned here: an automated cancel
// (the webhook supersede lane) records what actually happened instead of
// "cancelled by user", and the prior error is carried forward rather than
// erased (a supersede once overwrote a runner validation error, and the PR's
// synthetic gate then blamed a human who had touched nothing).
func TestCancelRunWithReason(t *testing.T) {
	newPub := func(t *testing.T) (*Publisher, store.RunStore) {
		t.Helper()
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatalf("store.New: %v", err)
		}
		return &Publisher{
			store:     st,
			cancelRun: func(string) error { return nil },
		}, st
	}
	seed := func(t *testing.T, st store.RunStore, id string, status store.RunStatus, runErr string) {
		t.Helper()
		run, err := st.CreateRun(context.Background(), id, "wf", nil)
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		run.Status = status
		run.Error = runErr
		if err := st.SaveRun(context.Background(), run); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
	}

	t.Run("an automated reason is recorded verbatim and keeps the prior error", func(t *testing.T) {
		p, st := newPub(t)
		seed(t, st, "run-1", store.RunStatusFailedResumable, `runner: reject repo ref: git: branch name "renovate/npm-(non-major)"`)
		if err := p.CancelRunWithReason(context.Background(), "run-1", "superseded by a newer delivery for the same subject"); err != nil {
			t.Fatalf("CancelRunWithReason: %v", err)
		}
		run, err := st.LoadRun(context.Background(), "run-1")
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != store.RunStatusCancelled {
			t.Fatalf("status = %s, want cancelled", run.Status)
		}
		if !strings.HasPrefix(run.Error, "superseded by a newer delivery") {
			t.Errorf("error = %q, want the automated reason first", run.Error)
		}
		if !strings.Contains(run.Error, "was failed_resumable") || !strings.Contains(run.Error, "reject repo ref") {
			t.Errorf("error = %q — the prior failure is the only record of WHY the run was dead and must survive the cancel", run.Error)
		}
	})

	t.Run("the operator click stays 'cancelled by user'", func(t *testing.T) {
		p, st := newPub(t)
		seed(t, st, "run-2", store.RunStatusRunning, "")
		if err := p.CancelRun(context.Background(), "run-2"); err != nil {
			t.Fatalf("CancelRun: %v", err)
		}
		run, err := st.LoadRun(context.Background(), "run-2")
		if err != nil {
			t.Fatal(err)
		}
		if run.Error != "cancelled by user" {
			t.Errorf("error = %q, want the plain operator shape", run.Error)
		}
	})

	// The composition that lost the reason in production: for a RUNNING run
	// the publisher flips the doc first, then the runner (holding the lease)
	// unwinds and writes its own generic "run cancelled". The engine's write
	// is a CAS from non-terminal statuses (pkg/runtime/run_failure.go), so
	// the publisher's specific reason must survive it.
	t.Run("the runner's engine cancel does not overwrite a recorded reason", func(t *testing.T) {
		p, st := newPub(t)
		seed(t, st, "run-4", store.RunStatusRunning, "")
		if err := p.CancelRunWithReason(context.Background(), "run-4", "superseded by a newer delivery for the same subject"); err != nil {
			t.Fatalf("CancelRunWithReason: %v", err)
		}
		// The exact write the engine performs on ctx-cancel.
		changed, err := st.UpdateRunStatusIf(context.Background(), "run-4", store.RunStatusCancelled, "run cancelled", []store.RunStatus{
			store.RunStatusRunning,
			store.RunStatusPausedWaitingHuman,
			store.RunStatusPausedOperator,
			store.RunStatusFailedResumable,
		})
		if err != nil {
			t.Fatalf("engine CAS: %v", err)
		}
		if changed {
			t.Fatal("the engine's write applied over an already-cancelled run — the recorded reason is lost")
		}
		run, err := st.LoadRun(context.Background(), "run-4")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(run.Error, "superseded by a newer delivery") {
			t.Errorf("error = %q — the supersede reason did not survive the runner's cancel", run.Error)
		}
	})

	t.Run("an empty reason still names the fact", func(t *testing.T) {
		p, st := newPub(t)
		seed(t, st, "run-3", store.RunStatusQueued, "")
		if err := p.CancelRunWithReason(context.Background(), "run-3", "  "); err != nil {
			t.Fatalf("CancelRunWithReason: %v", err)
		}
		run, err := st.LoadRun(context.Background(), "run-3")
		if err != nil {
			t.Fatal(err)
		}
		if run.Error != "cancelled" {
			t.Errorf("error = %q, want %q", run.Error, "cancelled")
		}
	})
}
