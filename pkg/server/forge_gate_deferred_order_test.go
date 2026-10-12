package server

// Verdicts deferred by a forge limit, against a forge that remembers (#2002
// lot 3): what lands on the head, in what order, and what the wait spends.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// liveForge is a forge that REMEMBERS: the latest status per (sha, context) is
// what a list returns, every call is refused while the budget is spent (the
// GET pull first — what a real exhausted installation refuses first), and
// individual calls can be failed on demand.
type liveForge struct {
	mu           sync.Mutex
	clock        func() time.Time
	head         string
	state        string
	limitedUntil time.Time
	statuses     map[string]forge.CommitStatus
	posted       []string
	getErrs      []error
	setErrs      []error
	gets         int
	sets         int
	lists        int
}

func (f *liveForge) limited(op string) error {
	if !f.limitedUntil.IsZero() && f.clock().Before(f.limitedUntil) {
		return &forge.StatusError{Provider: forge.ProviderGitHub, Op: op, Code: http.StatusForbidden, Limit: true, ResetAt: f.limitedUntil}
	}
	return nil
}

func (f *liveForge) GetPullRequest(_ context.Context, repo string, _ int) (forge.PullRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if err := f.limited("GET pull"); err != nil {
		return forge.PullRef{}, err
	}
	if len(f.getErrs) > 0 {
		e := f.getErrs[0]
		f.getErrs = f.getErrs[1:]
		if e != nil {
			return forge.PullRef{}, e
		}
	}
	return forge.PullRef{HeadSHA: f.head, HeadRepoFullName: repo, State: f.state}, nil
}

func (f *liveForge) SetCommitStatus(_ context.Context, _, sha string, st forge.CommitStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	if err := f.limited("POST statuses"); err != nil {
		return err
	}
	if len(f.setErrs) > 0 {
		e := f.setErrs[0]
		f.setErrs = f.setErrs[1:]
		if e != nil {
			return e
		}
	}
	f.statuses[strings.ToLower(sha)+"|"+st.Context] = st
	f.posted = append(f.posted, fmt.Sprintf("%s@%s %s %q", st.Context, sha, st.State, st.Description))
	return nil
}

func (f *liveForge) ListCommitStatuses(_ context.Context, _, sha string) ([]forge.CommitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if err := f.limited("GET statuses"); err != nil {
		return nil, err
	}
	var out []forge.CommitStatus
	for k, st := range f.statuses {
		if strings.HasPrefix(k, strings.ToLower(sha)+"|") {
			out = append(out, st)
		}
	}
	return out, nil
}

func (f *liveForge) on(sha, ctxName string) forge.CommitStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses[strings.ToLower(sha)+"|"+ctxName]
}

type pWorld struct {
	s   *Server
	f   *liveForge
	now *time.Time
}

const pCtx = "iterion/review"

func newPWorld(t *testing.T, inputs map[string]any, head string) pWorld {
	t.Helper()
	now := time.Now().UTC()
	f := &liveForge{head: head, statuses: map[string]forge.CommitStatus{}}
	f.clock = func() time.Time { return now }
	s, runID := gateReconcileFixture(t, inputs, f)
	s.gateClock = func() time.Time { return now }
	// The run's own in-flight claim, as markGateInFlight left it at launch.
	f.statuses[strings.ToLower(head)+"|"+pCtx] = forge.CommitStatus{
		State: forge.CommitStatePending, Context: pCtx, Description: gateInFlightDescription,
		TargetURL: gateRunURL("https://iterion.test", runID),
	}
	return pWorld{s: s, f: f, now: &now}
}

func (w pWorld) publish(t *testing.T, token, gate string) publishReviewResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	w.s.handleForgePublishReview(rec, publishReq(token, publishBodyWithGate(gate)))
	var resp publishReviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("publish answered HTTP %d: %s", rec.Code, rec.Body.String())
	}
	return resp
}

func pGate(blocking int, audited string) string {
	return fmt.Sprintf(`{"enabled":true,"context":%q,"blocking_count":%d,"threshold":"high","total_findings":%d,"audited_sha":%q}`, pCtx, blocking, blocking, audited)
}

func addGatingRun(t *testing.T, w pWorld, runID, token, head string) {
	t.Helper()
	in := gatingInputs()
	in[forgePublishVarToken] = token
	in["head_sha"] = head
	if _, err := w.s.cfg.Store.CreateRun(context.Background(), runID, "review_pr", in); err != nil {
		t.Fatal(err)
	}
	registerPublishToken(t, w.s, token, ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r", Bot: "review-pr"})
}

// A deferral replayed after a NEWER verdict landed on its head stands down:
// an older green never overwrites a newer red. The control is the same world
// with no deferral — the ordinary repair.
func TestAReplayNeverOverwritesANewerVerdict(t *testing.T) {
	for _, keepDeferral := range []bool{true, false} {
		t.Run(fmt.Sprintf("deferral_kept=%v", keepDeferral), func(t *testing.T) {
			w := newPWorld(t, gatingInputs(), "deadbeef")
			reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
			w.f.limitedUntil = reset
			// Run A (older review): GREEN, refused for the limit -> deferred.
			if resp := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(resp.GateError, "deferred until") {
				t.Fatalf("A not deferred: %+v", resp)
			}
			finishRun(t, w.s, "run-gating")
			// The limit lifts; run B (a newer review of the SAME head, e.g. /revi)
			// publishes RED and it lands.
			*w.now = reset.Add(time.Minute)
			addGatingRun(t, w, "run-b", "tok-b", "deadbeef")
			if resp := w.publish(t, "tok-b", pGate(2, "deadbeef")); !resp.GatePosted {
				t.Fatalf("B not posted: %+v", resp)
			}
			t.Logf("after B's publish the head shows: %s %q", w.f.on("deadbeef", pCtx).State, w.f.on("deadbeef", pCtx).Description)
			if !keepDeferral {
				// Control: the same world with no deferral = the ordinary repair.
				if _, err := w.s.forgePublishTokens.update("tok-gate", func(g *ForgePublishGrant) { g.Deferred = nil }); err != nil {
					t.Fatal(err)
				}
			}
			*w.now = reset.Add(2 * time.Minute)
			if err := w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating")); err != nil {
				t.Fatal(err)
			}
			final := w.f.on("deadbeef", pCtx)
			t.Logf("posted sequence: %v", w.f.posted)
			t.Logf("FINAL required check on deadbeef: %s %q", final.State, final.Description)
			if final.State != forge.CommitStateFailure {
				t.Errorf("the newer RED verdict (run B) was overwritten by %s %q", final.State, final.Description)
			}
		})
	}
}

// Two deferrals on one head, due at the same reset: whichever the sweep
// replays first — it lists newest-first, but nothing guarantees it — the
// newest verdict ends on the head.
func TestTwoDeferralsOnOneHeadTheNewestWins(t *testing.T) {
	for _, newestFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("newest replayed first=%v", newestFirst), func(t *testing.T) {
			twoDeferralsOnOneHead(t, newestFirst)
		})
	}
}

func twoDeferralsOnOneHead(t *testing.T, newestFirst bool) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	addGatingRun(t, w, "run-b", "tok-b", "deadbeef")
	// A (older) GREEN, then B (newer) RED, both during the limit.
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("A: %+v", r)
	}
	runA := finishRun(t, w.s, "run-gating")
	time.Sleep(5 * time.Millisecond)
	*w.now = w.now.Add(5 * time.Minute)
	if r := w.publish(t, "tok-b", pGate(3, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("B: %+v", r)
	}
	runB := finishRun(t, w.s, "run-b")
	ga, gb := mustGrant(t, w.s, "tok-gate"), mustGrant(t, w.s, "tok-b")
	t.Logf("A deferred until %s, B deferred until %s", ga.Deferred.RetryAt, gb.Deferred.RetryAt)
	*w.now = reset.Add(time.Minute)
	refs := []mongostore.NotifiableRunRef{
		{ID: runB.ID, Status: string(runB.Status), UpdatedAt: runB.UpdatedAt},
		{ID: runA.ID, Status: string(runA.Status), UpdatedAt: runA.UpdatedAt},
	}
	if !newestFirst {
		refs[0], refs[1] = refs[1], refs[0]
	}
	lister := &fakeGateSweepLister{refs: refs}
	w.s.sweepGates(context.Background(), lister, *w.now, gateSweepLookback, time.Time{})
	final := w.f.on("deadbeef", pCtx)
	t.Logf("posted sequence: %v", w.f.posted)
	t.Logf("FINAL required check: %s %q (newest verdict was B's RED)", final.State, final.Description)
	if final.State != forge.CommitStateFailure {
		t.Errorf("the newest verdict (B, red) lost to the older one (A): final %s", final.State)
	}
}

// The endpoint pins on a 7–40 hex prefix; a deferral pinned on a short form
// is replayed all the same, and the wait reads nothing.
func TestAnAbbreviatedPinIsReplayed(t *testing.T) {
	full := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	in := gatingInputs()
	in["head_sha"] = full
	w := newPWorld(t, in, full)
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	resp := w.publish(t, "tok-gate", pGate(0, full[:12]))
	t.Logf("publish answered gate_error=%q", resp.GateError)
	finishRun(t, w.s, "run-gating")
	// During the wait: is the run silent?
	gets0 := w.f.gets
	*w.now = reset.Add(-10 * time.Minute)
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	t.Logf("during the limit the reconciler made %d GET pull call(s) on the exhausted budget", w.f.gets-gets0)
	*w.now = reset.Add(time.Minute)
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	final := w.f.on(full, pCtx)
	g := mustGrant(t, w.s, "tok-gate")
	t.Logf("posted sequence: %v", w.f.posted)
	t.Logf("FINAL check: %s %q; deferral still on grant: %v", final.State, final.Description, g.Deferred != nil)
	if final.State != forge.CommitStateSuccess {
		t.Errorf("the deferred GREEN verdict was never posted; the head shows %s %q", final.State, final.Description)
	}
}

// A bot whose verdict covers the commit it PUSHED (Vetty's align-and-commit
// path: audited_sha = outputs.commit.sha) pins a sha that is not the run's
// head_sha input; the deferral is replayed by its own pin.
func TestAVerdictOnThePushedCommitIsReplayed(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef") // run reviewed deadbeef
	w.f.head = "c0ffee12"                         // ...and pushed c0ffee12
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	resp := w.publish(t, "tok-gate", pGate(0, "c0ffee12"))
	t.Logf("publish answered gate_error=%q", resp.GateError)
	finishRun(t, w.s, "run-gating")
	for i := 1; i <= 3; i++ {
		*w.now = reset.Add(time.Duration(i) * 7 * time.Hour) // past every recheck
		_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	}
	g := mustGrant(t, w.s, "tok-gate")
	final := w.f.on("c0ffee12", pCtx)
	marks, _ := w.s.gateSettles.settled(context.Background(), []string{"run-gating"})
	t.Logf("check on the pushed commit c0ffee12: %q %q; deferral still on grant: %+v; settle mark: %+v", final.State, final.Description, g.Deferred != nil, marks["run-gating"])
	if final.State == "" {
		t.Errorf("a verdict answered as 'deferred until …' was never posted")
	}
}

// A transient refusal at replay — a 502 on the forge's side — re-arms the
// deferral; it never throws the verdict away for a "review died".
func TestATransientFailureAtReplayIsRetriedNotPaintedDead(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	w.publish(t, "tok-gate", pGate(0, "deadbeef"))
	finishRun(t, w.s, "run-gating")
	*w.now = reset.Add(time.Minute)
	// What the real github client returns for a 502 on POST statuses:
	// refusal -> forge.StatusErrNeeding -> *StatusError{Code: 502}.
	w.f.setErrs = []error{&forge.StatusError{Provider: forge.ProviderGitHub, Op: "set commit status", Code: http.StatusBadGateway}}
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	final := w.f.on("deadbeef", pCtx)
	t.Logf("posted sequence: %v", w.f.posted)
	t.Logf("FINAL: %s %q", final.State, final.Description)
	if isSyntheticGateInterruption(final.Description) {
		t.Errorf("a transient 502 on the replay dropped the computed verdict and painted a synthetic failure")
	}
}

// The wait silences the auto-fix lane too: the sweep offers it the run at
// every pass, and every read it would make spends the exhausted budget.
func TestTheAutofixLaneReadsNothingDuringTheWait(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	ints := forge.NewMemoryRepoIntegrationStore()
	if err := ints.Create(context.Background(), forge.RepoIntegration{
		ID: "i1", TenantID: "team1", ConnectionID: "conn1", RepoFullName: "o/r", WebhookID: "w1",
		AutoFixOnGateFailure: true, LaunchVars: map[string]string{gateContextVar: pCtx},
	}); err != nil {
		t.Fatal(err)
	}
	w.s.forgeIntegrations = ints
	w.s.webhookConfigs = webhooks.NewMemoryConfigStore()
	w.s.webhookDeliveries = webhooks.NewMemoryDeliveryStore()
	reset := w.now.Add(50 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	w.publish(t, "tok-gate", pGate(2, "deadbeef"))
	run := finishRun(t, w.s, "run-gating")
	lister := &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: run.ID, Status: string(run.Status), UpdatedAt: run.UpdatedAt}}}
	g0, l0 := w.f.gets, w.f.lists
	for pass := 0; pass < 45; pass++ { // 45 fast passes, one a minute, all inside the wait
		*w.now = w.now.Add(time.Minute)
		if !w.now.Before(reset) {
			t.Fatal("left the wait")
		}
		w.s.sweepGates(context.Background(), lister, *w.now, gateSweepLookback, time.Time{})
	}
	t.Logf("during the wait: %d GET pull + %d list-status calls refused by the exhausted budget", w.f.gets-g0, w.f.lists-l0)
	if w.f.gets-g0 > 0 {
		t.Errorf("the wait spent %d reads on the exhausted budget", w.f.gets-g0)
	}
}

// A newer deferral replaces the grant's record of an older posted verdict:
// the reconciler replays the newer, instead of settling on the older.
func TestANewerDeferralReplacesTheRecordedVerdict(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !r.GatePosted {
		t.Fatalf("first publish: %+v", r)
	}
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	// The two decisions are causally ordered — give them the distinct
	// instants production's clock would: the decision authority breaks
	// millisecond ties toward the incumbent, and a frozen fixture clock
	// manufactures one.
	*w.now = w.now.Add(time.Minute)
	if r := w.publish(t, "tok-gate", pGate(4, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("second publish: %+v", r)
	}
	finishRun(t, w.s, "run-gating")
	*w.now = reset.Add(time.Minute)
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	g := mustGrant(t, w.s, "tok-gate")
	final := w.f.on("deadbeef", pCtx)
	t.Logf("posted: %v; final %s; grant deferred=%v cut_back=%v", w.f.posted, final.State, g.Deferred != nil, g.CutBack)
	if final.State != forge.CommitStateFailure {
		t.Errorf("the newer (red) deferred verdict was never replayed; the head keeps the older %s", final.State)
	}
}

// The REAL github client, fed GitHub's own rate-limit answer: the GET pull —
// the first call an exhausted installation refuses — is deferred until the
// forge's reset.
func TestTheRealGitHubClientsLimitIsDeferred(t *testing.T) {
	reset := time.Now().Add(40 * time.Minute).Truncate(time.Second)
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		rw.Header().Set("X-RateLimit-Limit", "5000")
		rw.Header().Set("X-RateLimit-Remaining", "0")
		rw.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		rw.Header().Set("X-RateLimit-Resource", "core")
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusForbidden)
		_, _ = rw.Write([]byte(`{"message":"API rate limit exceeded for installation ID 4242.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`))
	}))
	defer srv.Close()
	gh := forgegithub.New(srv.Client(), srv.URL, "t")
	_, getErr := gh.GetPullRequest(context.Background(), "o/r", 42)
	setErr := gh.SetCommitStatus(context.Background(), "o/r", "deadbeef", forge.CommitStatus{State: forge.CommitStateSuccess, Context: pCtx})
	for name, err := range map[string]error{"GET pull": getErr, "POST status": setErr} {
		var o gateOutcome
		o.noteRateLimit(err)
		t.Logf("%s -> %T %v | noteRateLimit: limited=%v reset=%s", name, err, err, o.rateLimited, o.resetAt)
	}
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, _ := gateReconcileFixture(t, gatingInputs(), gc)
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gh, nil }
	rec := httptest.NewRecorder()
	s.handleForgePublishReview(rec, publishReq("tok-gate", publishBodyWithGate(pGate(0, "deadbeef"))))
	var resp publishReviewResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	g := mustGrant(t, s, "tok-gate")
	t.Logf("calls=%v gate_error=%q deferred=%+v", calls, resp.GateError, g.Deferred)
	if g.Deferred == nil || !g.Deferred.RetryAt.Equal(reset) {
		t.Errorf("the real client's rate-limited GET pull was not deferred until the forge's reset %s", reset)
	}
}

// A deferral is replayed by its own pin and check, whatever the run's inputs
// lack.
func TestADeferralIsReplayedWhateverTheRunsInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop string
	}{
		{"a run launched without head_sha", "head_sha"},
		{"a run launched without gate_context", "gate_context"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gatingInputs()
			delete(in, tc.drop)
			w := newPWorld(t, in, "deadbeef")
			reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
			w.f.limitedUntil = reset
			resp := w.publish(t, "tok-gate", pGate(0, "deadbeef"))
			finishRun(t, w.s, "run-gating")
			for i := 1; i <= 3; i++ {
				*w.now = reset.Add(time.Duration(i) * time.Hour)
				_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
			}
			g := mustGrant(t, w.s, "tok-gate")
			final := w.f.on("deadbeef", pCtx)
			t.Logf("gate_error=%q | 3h after the reset: check %s %q, deferral on grant=%v, posts=%d", resp.GateError, final.State, final.Description, g.Deferred != nil, len(w.f.posted))
			if final.State != forge.CommitStateSuccess {
				t.Errorf("deferred verdict never replayed")
			}
		})
	}
}

// raceForge refuses ONE status post for a (secondary) rate limit while a
// concurrent publish on the same grant lands — GitHub's secondary limit
// refuses concurrent writes exactly so.
type raceForge struct {
	*liveForge
	hook          func()
	failAfterHook error
}

func (r *raceForge) SetCommitStatus(ctx context.Context, repo, sha string, st forge.CommitStatus) error {
	if r.hook != nil {
		h := r.hook
		r.hook = nil
		h()
		return r.failAfterHook
	}
	return r.liveForge.SetCommitStatus(ctx, repo, sha, st)
}

// A re-arm replaces only the deferral it replayed: a newer verdict that
// cleared it meanwhile (here a fork's, on the shared grant) stays cleared,
// and stays on the head.
func TestARearmNeverResurrectsAClearedDeferral(t *testing.T) {
	now := time.Now().UTC()
	lf := &liveForge{head: "deadbeef", statuses: map[string]forge.CommitStatus{}}
	lf.clock = func() time.Time { return now }
	rf := &raceForge{liveForge: lf}
	s, _ := gateReconcileFixture(t, gatingInputs(), rf)
	s.gateClock = func() time.Time { return now }
	w := pWorld{s: s, f: lf, now: &now}
	if _, _, err := s.shareGrant("tok-gate"); err != nil { // a fork publishes with it too
		t.Fatal(err)
	}
	reset := now.Add(30 * time.Minute).Truncate(time.Second)
	lf.limitedUntil = reset
	w.publish(t, "tok-gate", pGate(0, "deadbeef")) // A: green, deferred
	finishRun(t, s, "run-gating")
	now = reset.Add(time.Minute)
	rf.hook = func() { // while A's replay posts, the fork's newer RED lands
		if r := w.publish(t, "tok-gate", pGate(5, "deadbeef")); !r.GatePosted {
			t.Fatalf("fork publish: %+v", r)
		}
		t.Logf("fork's newer verdict landed; grant now deferred=%v", mustGrant(t, s, "tok-gate").Deferred != nil)
	}
	rf.failAfterHook = &forge.StatusError{Provider: forge.ProviderGitHub, Op: "POST statuses", Code: http.StatusForbidden, Limit: true}
	_ = s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	if g := mustGrant(t, s, "tok-gate"); g.Deferred != nil {
		t.Errorf("A's refused replay resurrected its deferral over the fork's newer verdict: %+v", g.Deferred)
		now = g.Deferred.RetryAt.Add(time.Minute)
		_ = s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	}
	final := lf.on("deadbeef", pCtx)
	t.Logf("posted: %v", lf.posted)
	t.Logf("FINAL: %s %q", final.State, final.Description)
	if final.State != forge.CommitStateFailure {
		t.Errorf("the fork's newer RED verdict was overwritten by the resurrected older one")
	}
}

// A forge reset is waited out an hour at most: one named days out (a typed
// answer may carry one a month away) would outlive the grant and the sweep.
func TestAForgeResetIsWaitedAnHourAtMost(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	at := gateRetryAt(now, now.Add(9*24*time.Hour), 1)
	t.Logf("retry_at for a forge reset 9 days out: %s (+%s); grant gate grace %s; sweep horizon %s", at, at.Sub(now), forgePublishGateGrace, gateSweepHorizon)
	w := newPWorld(t, gatingInputs(), "deadbeef")
	w.f.limitedUntil = w.now.Add(9 * 24 * time.Hour)
	w.publish(t, "tok-gate", pGate(0, "deadbeef"))
	finishRun(t, w.s, "run-gating")
	g0 := w.f.gets
	*w.now = w.now.Add(gateSweepHorizon - time.Hour) // the sweep's last passes
	w.f.limitedUntil = time.Time{}                   // the forge is long fine again
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	t.Logf("at the horizon's edge (limit long lifted): reads=%d posts=%d deferral left=%v", w.f.gets-g0, len(w.f.posted), mustGrant(t, w.s, "tok-gate").Deferred != nil)
	if at.After(now.Add(forgePublishGateGrace)) {
		t.Errorf("a deferral may wait past the grant (%s > %s): nothing will ever replay it", at.Sub(now), forgePublishGateGrace)
	}
}

// The reaper never cuts back a grant still carrying a deferral: the replay
// needs it.
func TestTheReaperKeepsAGrantCarryingADeferral(t *testing.T) {
	in := gatingInputs()
	delete(in, "head_sha")
	w := newPWorld(t, in, "deadbeef")
	w.f.limitedUntil = w.now.Add(50 * time.Minute)
	w.publish(t, "tok-gate", pGate(0, "deadbeef"))
	finishRun(t, w.s, "run-gating")
	if err := w.s.expireForgePublishGrantForRun(context.Background(), "run-gating"); err != nil {
		t.Fatal(err)
	}
	g := mustGrant(t, w.s, "tok-gate")
	t.Logf("after the reaper: deferred=%v cut_back=%v grant left %s (post-run grace %s)", g.Deferred != nil, g.CutBack, time.Until(g.ExpiresAt).Round(time.Minute), w.s.postRunGrace())
	if g.Deferred == nil || g.CutBack {
		t.Errorf("a grant still carrying a deferral was cut back")
	}
}

// Verdicts decided A then B, replayed A then B across two limit windows: the
// newest (B) ends on the head — A's landing first does not make it newer.
func TestTwoDeferralsInTwoWindowsTheNewestWins(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	r1 := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = r1
	addGatingRun(t, w, "run-b", "tok-b", "deadbeef")
	w.publish(t, "tok-gate", pGate(0, "deadbeef")) // A green, deferred to r1
	finishRun(t, w.s, "run-gating")
	*w.now = r1.Add(time.Minute)
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating")) // A replays: green lands
	r2 := w.now.Add(time.Hour)
	w.f.limitedUntil = r2 // the budget is spent again at once
	*w.now = w.now.Add(10 * time.Minute)
	if r := w.publish(t, "tok-b", pGate(3, "deadbeef")); !strings.Contains(r.GateError, "deferred") { // B red, newer
		t.Fatalf("B: %+v", r)
	}
	finishRun(t, w.s, "run-b")
	*w.now = r2.Add(time.Minute)
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-b"))
	final := w.f.on("deadbeef", pCtx)
	t.Logf("posted: %v; FINAL %s", w.f.posted, final.State)
	if final.State != forge.CommitStateFailure {
		t.Errorf("the newest verdict (B red) did not end on the head: %s", final.State)
	}
}

// A newer verdict deferred, then refused for good at replay (the head moved),
// must not settle on the grant's record of the OLDER verdict the deferral
// replaced — that record was erased when the deferral was kept. What answers
// the run is the refusal itself, on the CURRENT head, in the same pass
// (#1632): leaving the new head bare on the strength of an erased green is
// exactly the deadlock the refusal repair exists to close.
func TestARefusedReplayDoesNotFallBackOnAnOlderRecord(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !r.GatePosted {
		t.Fatalf("first publish: %+v", r)
	}
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	// Same frozen-clock tie as above: causally ordered, so distinctly
	// timestamped (the authority breaks ms ties toward the incumbent).
	*w.now = w.now.Add(time.Minute)
	if r := w.publish(t, "tok-gate", pGate(4, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("second publish: %+v", r)
	}
	finishRun(t, w.s, "run-gating")
	w.f.head = "0ther5ha" // a push lands before the replay
	*w.now = reset.Add(time.Minute)
	postsBefore := w.f.sets
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	marks, _ := w.s.gateSettles.settled(context.Background(), []string{"run-gating"})
	if m := marks["run-gating"]; m.Reason != gateSettledRefused {
		t.Errorf("after a refused replay the run was settled %q, want refused — neither the erased green record (verdict_success) nor a bare head (head_moved)", m.Reason)
	}
	if w.f.sets != postsBefore+1 {
		t.Fatalf("the refused replay was not answered (%d new writes, want 1)", w.f.sets-postsBefore)
	}
	if last := w.f.posted[len(w.f.posted)-1]; !strings.Contains(last, "review refused to certify") {
		t.Errorf("the answer must carry the refusal's diagnosis, got %q", last)
	}
}
