package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-162 decision 5 on the run launch API: a launch built FROM a fork run —
// naming it as parent_run_id (a REST child, a cloud shard) or as a
// delegation's source_run_id — runs under the fork's trust and meters its
// author (runview's inheritSourceTrust). The gate judges THAT subject
// (Service.LaunchSourceAdmission, read before the gate), so the launch is the
// author's spend on every arm: metered on the author's bound, refused at the
// author's ceiling, refused on the super-admin bypass. The review's probe: the
// gate judged the request alone, admitted the launch as trusted work, and the
// run service then published it untrusted — author bound neither checked nor
// charged, super-admin bypass unrefused.
//
// Mutation: gate the launch on launchSubject{BotID: …} again (the pre-fix
// subject) → every row but the trusted control is red on its own status; the
// run service's choke point then answers 409 launch_not_admitted where the
// gate's verdict is expected. Drop req.SourceRunID from the handler's
// LaunchSourceAdmission call → the delegation row reaches the delegation's
// own preparation (422) instead of the gate's refusal.

// forkLaunchSource compiles under a fork run's trust: no tool node, nothing
// the withdrawn capabilities would refuse.
const forkLaunchSource = "workflow fork_child:\n  entry: done\n"

func launchFromSource(t *testing.T, fx *forkResumeFixture, ctx context.Context, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	fx.s.handleLaunchRun(rec, httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(string(raw))).WithContext(ctx))
	return rec
}

// launchErrorCode is the denial's stable token: renderLaunchDenial puts it in
// error_code on a 5xx and in error below that.
func launchErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if body.ErrorCode != "" {
		return body.ErrorCode
	}
	return body.Error
}

func meteredOn(t *testing.T, c *orgusage.MemoryCounter, subject orgusage.Subject) int {
	t.Helper()
	u, err := c.Usage(context.Background(), subject, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return u.Runs
}

func TestHandleLaunchRun_ALaunchBuiltFromAForkRunIsJudgedAsTheForkRun(t *testing.T) {
	t.Run("a member's launch naming a fork parent meters its author beside the org", func(t *testing.T) {
		fx := newForkResumeFixture(t, store.RunStatusFinished)
		rec := launchFromSource(t, fx, fx.ctx, map[string]any{"source": forkLaunchSource, "parent_run_id": "run-fork"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d (%s), want 202", rec.Code, rec.Body.String())
		}
		launches := fx.pub.launched()
		if len(launches) != 1 {
			t.Fatalf("%d launches published, want 1", len(launches))
		}
		if got := launches[0]; got.Trust != store.RunTrustFork || got.ForkAuthorSubject != string(forkResumeSubject) {
			t.Fatalf("published trust %q, author %q; want the fork run's %q, %q", got.Trust, got.ForkAuthorSubject, store.RunTrustFork, forkResumeSubject)
		}
		assertForkTwinMetered(t, fx.counter)
	})

	t.Run("an author at their bound is refused, the org's unit handed back", func(t *testing.T) {
		fx := newForkResumeFixture(t, store.RunStatusFinished)
		fx.s.orgDefaults.ForkAuthorRunQuota = 1
		if _, err := fx.counter.AllowRun(context.Background(), forkResumeSubject, time.Now().UTC(), 0, 0); err != nil {
			t.Fatal(err)
		}
		rec := launchFromSource(t, fx, fx.ctx, map[string]any{"source": forkLaunchSource, "parent_run_id": "run-fork"})
		if rec.Code != http.StatusPaymentRequired || launchErrorCode(t, rec) != denyForkAuthorRuns {
			t.Fatalf("status = %d (%s), want 402 %s — the launch is the author's spend", rec.Code, rec.Body.String(), denyForkAuthorRuns)
		}
		if n := len(fx.pub.launched()); n != 0 {
			t.Fatalf("%d launches published past the author's bound", n)
		}
		if got := meteredOn(t, fx.counter, orgusage.OrgSubject("t1")); got != 0 {
			t.Fatalf("org runs = %d after a refused launch, want 0", got)
		}
		if got := meteredOn(t, fx.counter, forkResumeSubject); got != 1 {
			t.Fatalf("author runs = %d, want the 1 charged before the launch", got)
		}
	})

	for _, row := range []struct {
		name string
		body map[string]any
	}{
		{"a super-admin's launch naming a fork parent is refused, not admitted unmetered",
			map[string]any{"source": forkLaunchSource, "parent_run_id": "run-fork"}},
		{"a super-admin's delegation of a fork run is refused the same way",
			map[string]any{"source": forkLaunchSource, "source_run_id": "run-fork", "instructions": "repair it"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			fx := newForkResumeFixture(t, store.RunStatusFailed)
			rec := launchFromSource(t, fx, superAdminCtx(), row.body)
			if rec.Code != http.StatusServiceUnavailable || launchErrorCode(t, rec) != denyForkMeteringUnavailable {
				t.Fatalf("status = %d (%s), want 503 %s", rec.Code, rec.Body.String(), denyForkMeteringUnavailable)
			}
			if n := len(fx.pub.launched()); n != 0 {
				t.Fatalf("%d launches published on the super-admin bypass", n)
			}
			assertNothingMetered(t, fx.counter)
		})
	}

	t.Run("a trusted parent hands nothing over (the control)", func(t *testing.T) {
		fx := newForkResumeFixture(t, store.RunStatusFinished)
		if _, err := fx.rs.CreateRun(context.Background(), "run-trusted", "probe", nil); err != nil {
			t.Fatal(err)
		}
		rec := launchFromSource(t, fx, fx.ctx, map[string]any{"source": forkLaunchSource, "parent_run_id": "run-trusted"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d (%s), want 202", rec.Code, rec.Body.String())
		}
		launches := fx.pub.launched()
		if len(launches) != 1 || !launches[0].Trust.Trusted() || launches[0].ForkAuthorSubject != "" {
			t.Fatalf("published %+v, want one trusted launch naming no author", launches)
		}
		if got := meteredOn(t, fx.counter, forkResumeSubject); got != 0 {
			t.Fatalf("author runs = %d after a trusted launch, want 0", got)
		}
		if got := meteredOn(t, fx.counter, orgusage.OrgSubject("t1")); got != 1 {
			t.Fatalf("org runs = %d, want 1", got)
		}
	})
}

// flippingTrustStore, once armed, answers the NEXT read of one run as a
// trusted document and every later read as the fork run it is — the shape of
// a source run whose trust moved between the gate's read and the run
// service's. Armed after the run service is built, whose own startup reads
// the runs it recovers.
type flippingTrustStore struct {
	*store.FilesystemRunStore
	runID string
	armed atomic.Bool
}

func (f *flippingTrustStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	r, err := f.FilesystemRunStore.LoadRun(ctx, id)
	if err == nil && id == f.runID && f.armed.CompareAndSwap(true, false) {
		r.Trust, r.ForkAuthorSubject = store.RunTrustDefault, ""
	}
	return r, err
}

// TestHandleLaunchRun_ASourceWhoseTrustMovedUnderTheGateIsRefused: the gate
// judged the source run's first read (trusted), the run service read it again
// (fork) — the launch is refused 409 launch_not_admitted by the run service's
// choke point, nothing published, the org's unit handed back.
//
// Mutation: drop the ErrLaunchNotAdmitted arm of handleLaunchRun's error
// mapping → the refusal answers the generic 400 (red on its own assertion).
func TestHandleLaunchRun_ASourceWhoseTrustMovedUnderTheGateIsRefused(t *testing.T) {
	fx := newForkResumeFixture(t, store.RunStatusFinished)
	flip := &flippingTrustStore{FilesystemRunStore: fx.rs, runID: "run-fork"}
	fx.s.runs = newTestRunviewService(t, "", runview.WithStore(flip), runview.WithLaunchPublisher(fx.pub))
	flip.armed.Store(true)
	rec := launchFromSource(t, fx, fx.ctx, map[string]any{"source": forkLaunchSource, "parent_run_id": "run-fork"})
	if rec.Code != http.StatusConflict || launchErrorCode(t, rec) != launchNotAdmittedErrorCode {
		t.Fatalf("status = %d (%s), want 409 %s", rec.Code, rec.Body.String(), launchNotAdmittedErrorCode)
	}
	if n := len(fx.pub.launched()); n != 0 {
		t.Fatalf("%d launches published on an admission judged for another trust", n)
	}
	if got := meteredOn(t, fx.counter, orgusage.OrgSubject("t1")); got != 0 {
		t.Fatalf("org runs = %d after a launch refused before anything was published, want 0", got)
	}
}
