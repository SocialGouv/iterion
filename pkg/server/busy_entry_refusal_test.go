package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// busyEntryRefusal is the publisher's refusal of a frozen team_gateway
// resume whose attached pool's gateway entry exists but another run holds
// its one slot (cloudpublisher's TestBusyPoolEntry_* produce it); absent is
// the same refusal with nothing holding the provider — the entry gone.
func busyEntryRefusal(absent bool) error {
	r := &cloudpublisher.UnfundedProvidersError{Tenant: "team-1", Providers: []string{"openai_compatible"},
		Sites: map[string][]string{"openai_compatible": {"implement"}}, TeamGateway: true}
	if !absent {
		r.Busy = map[string][]string{"openai_compatible": {"the attached pool pool-att's entry gw-lender|api_key|openai_compatible (serving)"}}
	}
	return r
}

// A pool entry that exists but is busy is a wait on every surface that asks
// for the run again (P-1 of the round-3 review): the retry sweeper defers it
// on the slot cadence without spending an attempt — the team key at its cap
// gets the same — where a refusal naming an absent credential spends one; an
// operator's resume and a launch answer 429 with the capacity code, where the
// absent credential answers 422. Red when the busy refusal is read as a
// missing credential.
func TestBusyPoolEntry_ARefusalOnABusyEntryIsACapacityWait(t *testing.T) {
	t.Run("retry sweeper", func(t *testing.T) {
		for _, absent := range []bool{false, true} {
			st := newFakeRetryStore()
			st.claimWins["run-busy"] = true
			resumer := &fakeResumer{failing: busyEntryRefusal(absent)}
			s := newRetrySweeperServer(t, st, resumer)
			now := time.Now().UTC()
			s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-busy", now.Add(-time.Minute))}}, resumer, now)
			_, paid := st.armBudget["run-busy"]
			if absent {
				if !paid {
					t.Fatalf("absent credential: the sweeper did not spend an attempt (delayedAs %q) — only a wait is free", st.delayedAs["run-busy"])
				}
				continue
			}
			if paid {
				t.Fatal("busy entry: the sweeper spent an attempt (ScheduleRunRetry) on a slot wait")
			}
			if st.delayedAs["run-busy"] != store.RetryReasonCredSlot {
				t.Fatalf("busy entry: deferred as %q, want %q", st.delayedAs["run-busy"], store.RetryReasonCredSlot)
			}
			want := now.Add(cloudpublisher.CredSlotProbeInterval)
			if got := st.rearmed["run-busy"]; got.Before(want) || got.After(want.Add(time.Minute)) {
				t.Fatalf("busy entry: next ask %s, want about %s (one slot probe interval)", got.Format(time.RFC3339), want.Format(time.RFC3339))
			}
		}
	})
	t.Run("operator resume", func(t *testing.T) {
		srv := newQueueOutageHTTPTestServer(t)
		const runID = "run-busy-resume"
		installQueueOutageTestPublisher(t, srv, busyEntryRefusal(false))
		seedCapRefusalRun(t, srv, runID, store.RunStatusPausedOperator)
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/resume", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", runID)
		rec := httptest.NewRecorder()
		srv.handleResumeRun(rec, req)
		assertCredentialAtCapacityResponse(t, rec)
	})
	t.Run("operator resume, the entry absent (control)", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/runs/r1/resume", nil)
		(&Server{}).writeResumeError(rec, req, busyEntryRefusal(true))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d (%s), want 422 for an absent credential", rec.Code, rec.Body.String())
		}
	})
	t.Run("launch", func(t *testing.T) {
		srv := newQueueOutageHTTPTestServer(t)
		installQueueOutageTestPublisher(t, srv, busyEntryRefusal(false))
		body, err := json.Marshal(map[string]any{
			"file_path": "busy.bot",
			"source":    "workflow busy:\n  entry: done\n",
			"run_id":    "run-busy-launch",
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleLaunchRun(rec, req)
		assertCredentialAtCapacityResponse(t, rec)
	})
}
