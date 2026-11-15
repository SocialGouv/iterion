package runtime

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

func TestAdmissionSameStatusNewAttempt(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "run", true: "resume"}[resume], func(t *testing.T) {
			base := tmpStore(t)
			ref, err := store.ResolveStoreContextRef(t.Context(), base)
			if err != nil {
				t.Fatal(err)
			}
			c := &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce, RunStore: ref}
			row, err := base.CreateRun(t.Context(), "cas-aba", "admission", nil)
			if err != nil {
				t.Fatal(err)
			}
			row.ExecutionContext = c
			at := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
			row.QueuedAt = &at
			if resume {
				row.Status = store.RunStatusFailedResumable
			}
			if err := base.SaveRun(t.Context(), row); err != nil {
				t.Fatal(err)
			}
			wrapped := &admissionSnapshotStore{RunStore: base, change: func(fresh *store.Run) {
				if !resume {
					ok, err := base.UpdateRunStatusIf(t.Context(), fresh.ID, store.RunStatusFailedResumable, "interrupted", []store.RunStatus{store.RunStatusRunning})
					if err != nil || !ok {
						t.Fatalf("park: %v %v", ok, err)
					}
				}
				flip, ok, err := store.AsQueuedFlipper(base).FlipToQueued(t.Context(), fresh.ID, store.RunStatusFailedResumable, fresh.QueuedAt.Add(time.Second))
				if err != nil || !ok {
					t.Fatalf("flip: %v %v", ok, err)
				}
				ok, err = base.(store.QueuedAttemptClaimer).ClaimQueuedRunIfAttempt(t.Context(), fresh.ID, flip.At)
				if err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
				if resume {
					ok, err = base.UpdateRunStatusIf(t.Context(), fresh.ID, store.RunStatusFailedResumable, "new attempt interrupted", []store.RunStatus{store.RunStatusRunning})
					if err != nil || !ok {
						t.Fatalf("park new: %v %v", ok, err)
					}
				}
				latest, err := base.LoadRun(t.Context(), fresh.ID)
				if err != nil {
					t.Fatal(err)
				}
				*fresh = *latest
			}}
			calls := 0
			eng := admissionSnapshotEngine(t, wrapped, c, &calls)
			if resume {
				err = eng.Resume(t.Context(), row.ID, nil)
			} else {
				err = eng.Run(t.Context(), row.ID, nil)
			}
			saved, le := base.LoadRun(t.Context(), row.ID)
			if le != nil {
				t.Fatal(le)
			}
			t.Logf("resume=%v conflicts=%d calls=%d err=%v final=%s old=%s new=%s", resume, wrapped.conflicts, calls, err, saved.Status, row.QueuedAt, saved.QueuedAt)
			if wrapped.conflicts != 1 {
				t.Fatal("did not cross CAS")
			}
			if calls != 0 || !errors.Is(err, ErrResumeSuperseded) {
				t.Fatal("old delivery executed the newer attempt with the same status")
			}
			want := store.RunStatusRunning
			if resume {
				want = store.RunStatusFailedResumable
			}
			if saved.Status != want || saved.Admission != nil {
				t.Fatal("superseded delivery wrote on newer attempt")
			}
		})
	}
}

func TestAdmissionAttemptNegativeControls(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunStatusCancelled, store.RunStatusFailedResumable} {
		t.Run(string(status), func(t *testing.T) {
			base := tmpStore(t)
			ref, err := store.ResolveStoreContextRef(t.Context(), base)
			if err != nil {
				t.Fatal(err)
			}
			c := &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce, RunStore: ref}
			row, err := base.CreateRun(t.Context(), "cas-control", "admission", nil)
			if err != nil {
				t.Fatal(err)
			}
			row.Status = status
			row.ExecutionContext = c
			row.RunnerID = "old-observability"
			at := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
			row.QueuedAt = &at
			if err := base.SaveRun(t.Context(), row); err != nil {
				t.Fatal(err)
			}
			wrapped := &admissionSnapshotStore{RunStore: base, change: func(fresh *store.Run) {
				fresh.Name = "new name"
				fresh.RunnerID = "new-observability"
				z := fresh.QueuedAt.In(time.FixedZone("different representation", 3600))
				fresh.QueuedAt = &z
			}}
			calls := 0
			eng := admissionSnapshotEngine(t, wrapped, c, &calls)
			err = eng.Resume(t.Context(), row.ID, nil)
			saved, le := base.LoadRun(t.Context(), row.ID)
			if le != nil {
				t.Fatal(le)
			}
			t.Logf("initial=%s conflicts=%d calls=%d err=%v final=%s", status, wrapped.conflicts, calls, err, saved.Status)
			if err != nil || calls != 1 || saved.Status != store.RunStatusFinished {
				t.Fatal("legitimate resume or non-attempt metadata rejected")
			}
		})
	}
}
