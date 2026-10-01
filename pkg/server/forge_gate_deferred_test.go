package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// rateLimited is the refusal the forge client types for an exhausted budget.
func rateLimited(resetAt time.Time) error {
	return &forge.StatusError{Provider: forge.ProviderGitHub, Op: "set commit status", Code: http.StatusForbidden, Limit: true, ResetAt: resetAt}
}

const (
	greenPinned   = `{"enabled":true,"context":"iterion/review","blocking_count":0,"threshold":"high","total_findings":0,"audited_sha":"deadbeef"}`
	greenUnpinned = `{"enabled":true,"context":"iterion/review","blocking_count":0,"threshold":"high","total_findings":0}`
)

// deferredWorld is a gating run whose publish meets an exhausted forge budget,
// on a clock the test moves.
type deferredWorld struct {
	s     *Server
	gc    *readCountingGateClient
	runID string
	now   *time.Time
}

func newDeferredWorld(t *testing.T) deferredWorld {
	t.Helper()
	now := time.Now().UTC()
	gc := &readCountingGateClient{listingGateClient: &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	s.gateClock = func() time.Time { return now }
	return deferredWorld{s: s, gc: gc, runID: runID, now: &now}
}

func (w deferredWorld) publish(t *testing.T, gate string) publishReviewResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	w.s.handleForgePublishReview(rec, publishReq("tok-gate", publishBodyWithGate(gate)))
	var resp publishReviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("publish answered HTTP %d with an unreadable body: %s", rec.Code, rec.Body.String())
	}
	return resp
}

// A verdict the forge refuses for a rate limit is kept on the grant until the
// moment the forge named. Before it, the run's silence is answered with
// nothing — no synthetic failure, no relaunch, not even a read of the pull
// request; after it, the verdict itself is posted, recorded and settled like
// one the endpoint posted.
func TestARateLimitedVerdictWaitsForTheResetAndIsPosted(t *testing.T) {
	w := newDeferredWorld(t)
	resetAt := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.gc.setErr = rateLimited(resetAt)

	resp := w.publish(t, greenPinned)
	if resp.GatePosted || !strings.Contains(resp.GateError, "deferred until") {
		t.Fatalf("publish answered posted=%v error=%q, want the verdict deferred", resp.GatePosted, resp.GateError)
	}
	g := mustGrant(t, w.s, "tok-gate")
	if g.Deferred == nil || !g.Deferred.RetryAt.Equal(resetAt) || g.Deferred.Gate.AuditedSHA != "deadbeef" || g.Verdict != nil {
		t.Fatalf("grant after a rate-limited publish: deferred=%+v verdict=%+v, want the verdict kept until %s", g.Deferred, g.Verdict, resetAt)
	}

	finishRun(t, w.s, w.runID)
	w.gc.gets, w.gc.setCalls = 0, 0
	if err := w.s.reconcileGateForRun(context.Background(), terminalEvent(w.runID)); err != nil {
		t.Fatal(err)
	}
	if w.gc.gets != 0 || w.gc.setCalls != 0 {
		t.Fatalf("before the reset the reconciler read the pull request %d times and wrote %d statuses, want neither", w.gc.gets, w.gc.setCalls)
	}

	w.gc.setErr = nil
	*w.now = resetAt.Add(time.Minute)
	if err := w.s.reconcileGateForRun(context.Background(), terminalEvent(w.runID)); err != nil {
		t.Fatal(err)
	}
	if w.gc.setCalls != 1 || w.gc.last.State != forge.CommitStateSuccess || w.gc.lastSHA != "deadbeef" {
		t.Fatalf("after the reset: %d status writes, last %q on %q — want the deferred green verdict posted once", w.gc.setCalls, w.gc.last.State, w.gc.lastSHA)
	}
	g = mustGrant(t, w.s, "tok-gate")
	if g.Deferred != nil || g.Verdict == nil || g.Verdict.State != string(forge.CommitStateSuccess) {
		t.Errorf("after the replay: deferred=%+v verdict=%+v, want the deferral cleared and the verdict recorded", g.Deferred, g.Verdict)
	}
	marks, err := w.s.gateSettles.settled(context.Background(), []string{w.runID})
	if err != nil || marks[w.runID].Reason != gateSettledVerdictSuccess {
		t.Errorf("after the replay the run is settled %+v (err %v), want verdict_success", marks[w.runID], err)
	}
}

// An unpinned verdict is not kept: replayed hours later it would land on
// whatever head the pull request has then.
func TestAnUnpinnedRateLimitedVerdictIsNotDeferred(t *testing.T) {
	w := newDeferredWorld(t)
	w.gc.setErr = rateLimited(w.now.Add(30 * time.Minute))
	resp := w.publish(t, greenUnpinned)
	if strings.Contains(resp.GateError, "deferred") {
		t.Errorf("an unpinned verdict was deferred: %q", resp.GateError)
	}
	if g := mustGrant(t, w.s, "tok-gate"); g.Deferred != nil {
		t.Errorf("an unpinned verdict was kept on the grant: %+v", g.Deferred)
	}
}

// A refusal that is not a rate limit is not deferred either: waiting cannot
// change it — whether the forge client typed it or not.
func TestARefusalThatIsNotARateLimitIsNotDeferred(t *testing.T) {
	for name, refusal := range map[string]error{
		"an untyped 403": forge.StatusErr("github", "set commit status", http.StatusForbidden),
		"a typed 403":    &forge.StatusError{Provider: forge.ProviderGitHub, Op: "set commit status", Code: http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			w := newDeferredWorld(t)
			w.gc.setErr = refusal
			w.publish(t, greenPinned)
			if g := mustGrant(t, w.s, "tok-gate"); g.Deferred != nil {
				t.Errorf("a 403 that is not a rate limit was deferred: %+v", g.Deferred)
			}
		})
	}
}

// A replay the forge still limits is re-armed at the new moment, with one
// more attempt counted — still no synthetic failure.
func TestADeferredVerdictStillLimitedIsRearmed(t *testing.T) {
	w := newDeferredWorld(t)
	first := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.gc.setErr = rateLimited(first)
	w.publish(t, greenPinned)
	finishRun(t, w.s, w.runID)

	second := first.Add(time.Hour)
	w.gc.setErr = rateLimited(second)
	*w.now = first.Add(time.Minute)
	w.gc.posted = nil
	if err := w.s.reconcileGateForRun(context.Background(), terminalEvent(w.runID)); err != nil {
		t.Fatal(err)
	}
	g := mustGrant(t, w.s, "tok-gate")
	if g.Deferred == nil || g.Deferred.Attempts != 2 || !g.Deferred.RetryAt.Equal(second) {
		t.Fatalf("a replay still limited left %+v, want attempt 2 re-armed at %s", g.Deferred, second)
	}
	for _, st := range w.gc.posted {
		if st.State != forge.CommitStateSuccess {
			t.Errorf("a replay still limited wrote a %q status (%q) — the run's silence was answered as a death", st.State, st.Description)
		}
	}
}

// A deferred verdict the forge refuses for another reason — here the head
// moved past the audited revision — is cleared, and the run handed back to
// the ordinary repair, which settles the moved head.
func TestADeferredVerdictForAMovedHeadIsClearedAndRepairedAsUsual(t *testing.T) {
	w := newDeferredWorld(t)
	resetAt := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.gc.setErr = rateLimited(resetAt)
	w.publish(t, greenPinned)
	finishRun(t, w.s, w.runID)

	w.gc.setErr = nil
	w.gc.headSHA = "0ther5ha"
	*w.now = resetAt.Add(time.Minute)
	w.gc.setCalls = 0
	if err := w.s.reconcileGateForRun(context.Background(), terminalEvent(w.runID)); err != nil {
		t.Fatal(err)
	}
	if w.gc.setCalls != 0 {
		t.Errorf("a verdict for a head that moved was posted (%d writes)", w.gc.setCalls)
	}
	if g := mustGrant(t, w.s, "tok-gate"); g.Deferred != nil {
		t.Errorf("the refused deferral was kept: %+v", g.Deferred)
	}
	marks, _ := w.s.gateSettles.settled(context.Background(), []string{w.runID})
	if marks[w.runID].Reason != gateSettledHeadMoved {
		t.Errorf("the run was settled %+v, want head_moved by the ordinary repair", marks[w.runID])
	}
}

// Past its attempts, a verdict the forge keeps limiting is cleared and the run
// answered like any run that left no verdict — and the line says so.
func TestADeferredVerdictPastItsAttemptsIsGivenUp(t *testing.T) {
	w := newDeferredWorld(t)
	out := &lockedBuffer{}
	w.s.logger = iterlog.New(iterlog.LevelWarn, out)
	resetAt := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.gc.setErr = rateLimited(resetAt)
	w.publish(t, greenPinned)
	finishRun(t, w.s, w.runID)
	if ok, err := w.s.forgePublishTokens.update("tok-gate", func(g *ForgePublishGrant) { g.Deferred.Attempts = gateDeferMaxAttempts }); err != nil || !ok {
		t.Fatal(ok, err)
	}
	*w.now = resetAt.Add(time.Minute)
	if err := w.s.reconcileGateForRun(context.Background(), terminalEvent(w.runID)); err != nil {
		t.Fatal(err)
	}
	if g := mustGrant(t, w.s, "tok-gate"); g.Deferred != nil {
		t.Errorf("a verdict past its %d attempts was kept: %+v", gateDeferMaxAttempts, g.Deferred)
	}
	if !strings.Contains(out.String(), "answering the run as unanswered") {
		t.Errorf("giving up the deferred verdict was not reported; log:\n%s", out.String())
	}
}

// The review itself refused for the limit still defers the verdict: both of
// the endpoint's posting paths keep it.
func TestAVerdictDeferredOnTheReviewFailedPath(t *testing.T) {
	w := newDeferredWorld(t)
	w.s.forgeReviewClientFor = func(context.Context, forge.Connection) (forge.ReviewClient, error) {
		return &fakeReviewClient{err: rateLimited(w.now.Add(time.Hour))}, nil
	}
	w.gc.setErr = rateLimited(w.now.Add(30 * time.Minute))
	resp := w.publish(t, greenPinned)
	if !strings.Contains(resp.GateError, "deferred until") {
		t.Fatalf("the review-failed path did not defer the verdict: %q", resp.GateError)
	}
	if g := mustGrant(t, w.s, "tok-gate"); g.Deferred == nil {
		t.Error("the review-failed path kept no deferral on the grant")
	}
}

// When the forge names no reset — or one already past — the wait is a backoff
// doubling with the attempt, capped at the hour a REST budget resets within.
func TestGateRetryAt(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		resetAt time.Time
		attempt int
		want    time.Time
	}{
		{"the forge's reset", now.Add(17 * time.Minute), 1, now.Add(17 * time.Minute)},
		{"no reset named", time.Time{}, 1, now.Add(5 * time.Minute)},
		{"a reset already past", now.Add(-time.Minute), 1, now.Add(5 * time.Minute)},
		{"the third attempt", time.Time{}, 3, now.Add(20 * time.Minute)},
		{"capped at the hour", time.Time{}, 10, now.Add(time.Hour)},
	} {
		if got := gateRetryAt(now, tc.resetAt, tc.attempt); !got.Equal(tc.want) {
			t.Errorf("%s: retry at %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The verdict-order marks, on both twins: the first decision on a head is
// granted, an older one after it refused, a newer one granted — and the mark
// names the newest decision and the status it posts.
func TestGateDecisionStores_OrderTheDecisionsOnAHead(t *testing.T) {
	for _, backend := range []string{"memory", "valkey"} {
		t.Run(backend, func(t *testing.T) {
			var st gateDecisionStore = newMemoryGateDecisionStore(nil)
			if backend == "valkey" {
				_, rdb := newTestRedis(t)
				st = newValkeyGateDecisionStore(rdb)
			}
			ctx := context.Background()
			t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
			older, middle, newer := gateDecision{ID: "a", At: t0}, gateDecision{ID: "b", At: t0.Add(time.Minute)}, gateDecision{ID: "c", At: t0.Add(2 * time.Minute)}
			github := forge.Connection{Provider: forge.ProviderGitHub}
			key := gateDecisionKey(github, "o/r", "deadbeef", "iterion/review")
			mark := func(d gateDecision) gateMark {
				return gateMark{Decision: d, Status: gateMarkStatus{State: "success", Description: "decided " + d.ID}}
			}
			for _, step := range []struct {
				d    gateDecision
				want bool
			}{{middle, true}, {older, false}, {middle, true}, {newer, true}, {middle, false}} {
				if got, err := st.claim(ctx, key, mark(step.d), time.Hour); err != nil || got != step.want {
					t.Fatalf("claim %s at %s = %v (err %v), want %v", step.d.ID, step.d.At.Format(time.Kitchen), got, err, step.want)
				}
			}
			if m, found, err := st.newest(ctx, key); err != nil || !found || m.Decision.ID != "c" || m.Status.Description != "decided c" {
				t.Errorf("the mark reads %+v (found %v, err %v), want decision c and its status", m, found, err)
			}
			if got, _ := st.claim(ctx, gateDecisionKey(github, "o/r", "0ther5ha", "iterion/review"), mark(older), time.Hour); !got {
				t.Error("a decision on another head was refused")
			}
			ghe := forge.Connection{Provider: forge.ProviderGitHub, ForgeBaseURL: "https://ghe.example.com/"}
			if got, _ := st.claim(ctx, gateDecisionKey(ghe, "o/r", "deadbeef", "iterion/review"), mark(older), time.Hour); !got {
				t.Error("a decision on another forge's o/r@deadbeef was refused: two forges carry two status streams")
			}
			if a, b := gateDecisionKey(ghe, "o/r", "deadbeef", "iterion/review"), gateDecisionKey(forge.Connection{Provider: forge.ProviderGitHub, ForgeBaseURL: "HTTPS://GHE.example.com"}, "O/R", "DEADBEEF", "iterion/review"); a != b {
				t.Error("two connections to one forge order apart: the key must not depend on how the base URL is spelled")
			}
		})
	}
}

// A refusal the forge decided is not retried; one on its side — a 5xx, or one
// it never answered — is.
func TestGateOutcome_NoteTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"a 502", &forge.StatusError{Provider: forge.ProviderGitHub, Code: http.StatusBadGateway}, true},
		{"a typed 403", &forge.StatusError{Provider: forge.ProviderGitHub, Code: http.StatusForbidden}, false},
		{"the forbidden sentinel", forge.ErrForbidden, false},
		{"the unauthorized sentinel", forge.ErrUnauthorized, false},
		{"a not-found", forge.StatusErr("github", "get pull", http.StatusNotFound), false},
		{"a permission the installation withholds", fmt.Errorf("mint installation token: %w", forge.ErrPermissionsNotGranted), false},
		{"an answer nobody classified", errors.New("forge: validation failed"), false},
		{"a cancelled call", fmt.Errorf("post status: %w", context.Canceled), true},
		{"a timed-out call", fmt.Errorf("post status: %w", context.DeadlineExceeded), true},
		{"a cut connection", &url.Error{Op: "Post", URL: "https://api.github.com/repos/o/r/statuses/x", Err: io.EOF}, true},
		{"a body cut short", fmt.Errorf("decode: %w", io.ErrUnexpectedEOF), true},
	} {
		var out gateOutcome
		out.noteTransient(tc.err)
		if out.transient != tc.want {
			t.Errorf("%s: transient=%v, want %v", tc.name, out.transient, tc.want)
		}
	}
}

// A verdict that lands retires the grant's deferral only if that one was not
// decided later: replaying an older verdict must not throw away a newer one
// deferred on the same grant meanwhile (a fork's, on the shared grant).
func TestALandedVerdictKeepsANewerDeferral(t *testing.T) {
	w := newDeferredWorld(t)
	newer := &gateDeferral{Gate: publishReviewGate{Enabled: true, Context: "iterion/review", AuditedSHA: "deadbeef", BlockingCount: 3},
		Repo: "o/r", Number: 42, Decision: gateDecision{ID: "newer", At: w.now.Add(time.Minute)}, RetryAt: w.now.Add(time.Hour), Attempts: 1}
	if ok, err := w.s.forgePublishTokens.update("tok-gate", func(g *ForgePublishGrant) { g.Deferred = newer }); err != nil || !ok {
		t.Fatal(ok, err)
	}
	landed := gateOutcome{posted: true, sha: "deadbeef", context: "iterion/review", state: string(forge.CommitStateSuccess)}
	w.s.recordGateVerdict("tok-gate", landed, gateDecision{ID: "older", At: *w.now})
	if g := mustGrant(t, w.s, "tok-gate"); g.Deferred == nil || g.Deferred.Decision.ID != "newer" {
		t.Errorf("an older verdict landing threw away the newer deferral: %+v", g.Deferred)
	}
}
