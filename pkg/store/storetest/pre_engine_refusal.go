package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// RunPreEngineRefusalConformance exercises the optional port on real twins.
func RunPreEngineRefusalConformance(t *testing.T, s store.RunStore) {
	recorder, ok := s.(store.PreEngineRefusalRecorder)
	if !ok {
		t.Skip("backend does not implement PreEngineRefusalRecorder")
	}
	ctx := testCtx()
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	seed := func(id string) *store.Run {
		t.Helper()
		inputs := map[string]any{"kept": "$status"}
		r, err := s.CreateRun(ctx, id, "refusal", inputs)
		if err != nil {
			t.Fatal(err)
		}
		r.TenantID = "_test"
		r.Status = store.RunStatusQueued
		r.QueuedAt = &at
		r.Checkpoint = &store.Checkpoint{NodeID: "question", InteractionID: "answered-question"}
		if err := s.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	load := func(id string) *store.Run {
		t.Helper()
		r, e := s.LoadRun(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	request := store.PreEngineRefusalRequest{Phase: "mcp", Code: store.FailureExecutionFailed, Message: "$status: catalogue unreadable", Hint: "repair catalogue", PublishedAt: at, Disposition: store.PreEngineRetryPending}
	record := func(id string, q store.PreEngineRefusalRequest) store.PreEngineRefusalResult {
		t.Helper()
		r, e := recorder.RecordPreEngineRefusal(ctx, id, q)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	t.Run("pending_preserves_resume_and_save_ownership", func(t *testing.T) {
		seed("pending")
		before := load("pending")
		result := record("pending", request)
		after := load("pending")
		if !result.Changed || result.Status != store.RunStatusQueued || after.PreEngineRefusal == nil || after.Error != request.Message {
			t.Fatalf("missing queued diagnostic: result=%+v run=%+v", result, after)
		}
		if !reflect.DeepEqual(before.Checkpoint, after.Checkpoint) || before.OutcomeSeq != after.OutcomeSeq || before.ContinuationState != after.ContinuationState || !reflect.DeepEqual(before.RetryState, after.RetryState) || !reflect.DeepEqual(before.Inputs, after.Inputs) {
			t.Fatal("pending refusal consumed resume evidence or outcome")
		}
		d := after.PreEngineRefusal
		if d.Phase != request.Phase || d.Code != request.Code || d.Hint != request.Hint || d.ObservedAt.IsZero() || !d.PublishedAt.Equal(at) || d.QueuedAt == nil || !d.QueuedAt.Equal(at) {
			t.Fatalf("diagnostic=%+v", d)
		}
		stale := *after
		after.PreEngineRefusal = nil
		after.Error = ""
		if e := s.SaveRun(ctx, after); e != nil {
			t.Fatal(e)
		}
		saved := load("pending")
		if saved.PreEngineRefusal == nil || saved.Error != request.Message {
			t.Fatal("OWNERSHIP_ERASE: metadata saver erased diagnostic/message")
		}
		claim, ok := s.(store.QueuedAttemptClaimer)
		if !ok {
			t.Fatal("missing claim port")
		}
		if changed, e := claim.ClaimQueuedRunIfAttempt(ctx, "pending", at); e != nil || !changed {
			t.Fatalf("claim=%v %v", changed, e)
		}
		current := load("pending")
		if current.PreEngineRefusal != nil {
			t.Fatal("claim retained stale diagnostic")
		}
		if e := s.SaveRun(ctx, &stale); !errors.Is(e, store.ErrRunConflict) {
			t.Fatalf("stale save=%v", e)
		}
		current.PreEngineRefusal = d
		if e := s.SaveRun(ctx, current); e != nil {
			t.Fatal(e)
		}
		if load("pending").PreEngineRefusal != nil {
			t.Fatal("fresh-version stale field resurrected diagnostic")
		}
		forged := *load("pending")
		forged.ID = "forged"
		forged.CASVersion = 0
		forged.PreEngineRefusal = d
		if e := s.SaveRun(ctx, &forged); e != nil {
			t.Fatal(e)
		}
		if load("forged").PreEngineRefusal != nil {
			t.Fatal("create accepted caller-authored diagnostic")
		}
	})
	t.Run("publication_fences_running_too", func(t *testing.T) {
		seed("newer")
		r := load("newer")
		later := at.Add(time.Second)
		r.QueuedAt = &later
		if e := s.SaveRun(ctx, r); e != nil {
			t.Fatal(e)
		}
		if result := record("newer", request); result.Changed {
			t.Fatal("old delivery changed newer queued")
		}
		claim := s.(store.QueuedAttemptClaimer)
		if changed, e := claim.ClaimQueuedRunIfAttempt(ctx, "newer", later); e != nil || !changed {
			t.Fatalf("claim=%v %v", changed, e)
		}
		before := load("newer")
		if result := record("newer", request); result.Changed {
			t.Fatal("OLD_RUNNING: old delivery changed newer running")
		}
		after := load("newer")
		if !reflect.DeepEqual(before, after) {
			t.Fatal("refused recorder changed document")
		}
		q := request
		q.PublishedAt = later
		if result := record("newer", q); !result.Changed || result.Status != store.RunStatusFailedResumable {
			t.Fatalf("current running refused: %+v", result)
		}
		if r := load("newer"); r.ContinuationState != "" || r.FailureCode != request.Code {
			t.Fatalf("current verdict=%+v", r)
		}
	})
	t.Run("legacy_and_invalid_identity", func(t *testing.T) {
		seed("legacy")
		r := load("legacy")
		r.QueuedAt = nil
		if e := s.SaveRun(ctx, r); e != nil {
			t.Fatal(e)
		}
		q := request
		q.PublishedAt = time.Time{}
		if !record("legacy", q).Changed {
			t.Fatal("unmarked legacy refused")
		}
		seed("marked")
		if record("marked", q).Changed {
			t.Fatal("missing publication changed marked run")
		}
		q = request
		q.Phase = ""
		if _, e := recorder.RecordPreEngineRefusal(ctx, "marked", q); !errors.Is(e, store.ErrInvalidPreEngineRefusal) {
			t.Fatalf("invalid request=%v", e)
		}
		if result, e := recorder.RecordPreEngineRefusal(store.WithIdentity(context.Background(), "other", "other"), "marked", request); e == nil && result.Changed {
			t.Fatal("cross-tenant write")
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if result, e := recorder.RecordPreEngineRefusal(cancelled, "marked", request); e == nil || result.Changed {
			t.Fatalf("cancelled write=%+v %v", result, e)
		}
	})
	t.Run("usage_park_and_final_release", func(t *testing.T) {
		seed("usage")
		q := request
		q.Disposition = store.PreEngineUsagePark
		q.Code = store.FailureUsageLimitBlocked
		if result := record("usage", q); !result.Changed || result.Status != store.RunStatusFailedResumable {
			t.Fatalf("usage not armable=%+v", result)
		}
		if r := load("usage"); r.ContinuationState != "" || r.FailureCode != store.FailureUsageLimitBlocked {
			t.Fatalf("premature continuation=%+v", r)
		}
		if result := record("usage", q); result.Changed {
			t.Fatal("usage widened to already failed_resumable")
		}
		seed("release")
		q = request
		q.Disposition = store.PreEngineFinalVerdict
		q.FinalStatus = store.RunStatusFailed
		q.LeaveQueued = true
		if result := record("release", q); result.Changed {
			t.Fatal("final recorder stole queued release")
		}
		q.LeaveQueued = false
		if result := record("release", q); !result.Changed || result.Status != store.RunStatusFailed {
			t.Fatalf("final refused=%+v", result)
		}
		if load("release").ContinuationState != store.ContinuationFinal {
			t.Fatal("final verdict not final")
		}
	})
	t.Run("same_status_cause_and_rearm", func(t *testing.T) {
		for _, change := range []string{"message", "code", "both", "same", "arm"} {
			t.Run(change, func(t *testing.T) {
				id := "cause-" + change
				seed(id)
				q := request
				if changed, err := s.(store.QueuedAttemptClaimer).ClaimQueuedRunIfAttempt(ctx, id, at); err != nil || !changed {
					t.Fatalf("claim changed=%v err=%v", changed, err)
				}
				if !record(id, q).Changed {
					t.Fatal("initial refusal missing")
				}
				message, code := request.Message, request.Code
				if change == "message" || change == "both" {
					message = "$dlq: parked; resume to retry"
				}
				if change == "code" || change == "both" {
					code = store.FailureDLQParked
				}
				if change == "arm" {
					message, code = "", ""
				}
				before := load(id)
				if changed, err := s.UpdateRunOutcome(ctx, id, store.RunStatusFailedResumable, message, store.RunOutcomeMeta{Code: code, Continuation: store.ContinuationRetryArmed}, nil); err != nil || !changed {
					t.Fatalf("new outcome changed=%v err=%v", changed, err)
				}
				run := load(id)
				keep := change == "same" || change == "arm"
				if (run.PreEngineRefusal != nil) != keep {
					t.Fatalf("diagnostic after cause change: %+v", run.PreEngineRefusal)
				}
				if run.OutcomeSeq != before.OutcomeSeq {
					t.Fatal("same-status cause created an episode")
				}
				wantError := run.Error
				run.Name = "renamed"
				if err := s.SaveRun(ctx, run); err != nil {
					t.Fatal(err)
				}
				after := load(id)
				if after.Error != wantError || (after.PreEngineRefusal != nil) != keep {
					t.Fatalf("metadata save replaced current cause: %+v", after)
				}
			})
		}
	})
	t.Run("lifecycle_clears_diagnostic", func(t *testing.T) {
		for _, name := range []string{"publication", "cancel", "pause", "terminal", "delete"} {
			t.Run(name, func(t *testing.T) {
				seed(name)
				if !record(name, request).Changed {
					t.Fatal("seed diagnostic refused")
				}
				var e error
				switch name {
				case "publication":
					e = s.UpdateRunStatus(ctx, name, store.RunStatusQueued, "")
				case "cancel":
					e = s.UpdateRunStatus(ctx, name, store.RunStatusCancelled, "cancelled")
				case "pause":
					e = s.PauseRun(ctx, name, &store.Checkpoint{NodeID: "waiting", InteractionID: "next-question"})
				case "terminal":
					e = s.UpdateRunStatus(ctx, name, store.RunStatusFinished, "")
				case "delete":
					e = s.DeleteRun(ctx, name)
				}
				if e != nil {
					t.Fatal(e)
				}
				if name == "delete" {
					if result, e := recorder.RecordPreEngineRefusal(ctx, name, request); result.Changed {
						t.Fatalf("deleted changed=%+v %v", result, e)
					}
					return
				}
				if load(name).PreEngineRefusal != nil {
					t.Fatal("lifecycle retained obsolete diagnostic")
				}
			})
		}
	})
}
