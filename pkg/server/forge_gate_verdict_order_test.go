package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// hookForge runs a hook just before a status write lands: the window between
// a decision's claim and its post.
type hookForge struct {
	*liveForge
	mu    sync.Mutex
	onSet func(st forge.CommitStatus)
}

func (h *hookForge) SetCommitStatus(ctx context.Context, repo, sha string, st forge.CommitStatus) error {
	h.mu.Lock()
	fn := h.onSet
	h.onSet = nil
	h.mu.Unlock()
	if fn != nil {
		fn(st)
	}
	return h.liveForge.SetCommitStatus(ctx, repo, sha, st)
}

// An older verdict whose post was in flight when a newer one claimed and
// posted lands on top of it — and is put back under it at once: the head
// ends on the newer verdict, on both twins of the order store.
func TestAVerdictThatLandsLateIsPutUnderTheNewerOne(t *testing.T) {
	for _, backend := range []string{"memory", "valkey"} {
		t.Run(backend, func(t *testing.T) {
			w := newPWorld(t, gatingInputs(), "deadbeef")
			if backend == "valkey" {
				_, rdb := newTestRedis(t)
				w.s.gateDecisions = newValkeyGateDecisionStore(rdb)
			}
			h := &hookForge{liveForge: w.f}
			w.s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return h, nil }
			reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
			w.f.limitedUntil = reset
			if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred until") {
				t.Fatalf("the green verdict was not deferred: %+v", r)
			}
			finishRun(t, w.s, "run-gating")
			*w.now = reset.Add(time.Minute)
			addGatingRun(t, w, "run-b", "tok-b", "deadbeef")
			h.onSet = func(forge.CommitStatus) {
				// The green replay has claimed the head and is posting: a newer
				// review's red is decided, claims and lands in that window.
				*w.now = w.now.Add(time.Minute)
				if r := w.publish(t, "tok-b", pGate(2, "deadbeef")); !r.GatePosted {
					t.Fatalf("the newer red verdict was not posted: %+v", r)
				}
			}
			if err := w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating")); err != nil {
				t.Fatal(err)
			}
			if final := w.f.on("deadbeef", pCtx); final.State != forge.CommitStateFailure {
				t.Errorf("the newer red verdict was left under the older green: final %s %q (posted %v)", final.State, final.Description, w.f.posted)
			}
		})
	}
}

// Two forges carry two status streams: another tenant's verdict on its own
// forge, for the same repo path, commit and check, does not supersede this
// tenant's deferred verdict.
func TestTheVerdictOrderIsPerForge(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	other := &liveForge{head: "deadbeef", statuses: map[string]forge.CommitStatus{}}
	other.clock = func() time.Time { return *w.now }
	w.s.forgeGateClientFor = func(_ context.Context, conn forge.Connection) (forgeGateClient, error) {
		if conn.ID == "conn2" {
			return other, nil
		}
		return w.f, nil
	}
	if err := w.s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "conn2", TenantID: "team2", Provider: forge.ProviderGitHub, ForgeBaseURL: "https://ghe.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	registerPublishToken(t, w.s, "tok-2", ForgePublishGrant{TeamID: "team2", ConnectionID: "conn2", Repo: "o/r", Bot: "review-pr"})
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("tenant 1's green was not deferred: %+v", r)
	}
	finishRun(t, w.s, "run-gating")
	*w.now = w.now.Add(5 * time.Minute)
	rec := httptest.NewRecorder()
	w.s.handleForgePublishReview(rec, publishReq("tok-2", `{"pr_url":"https://ghe.example.com/o/r/pull/42","summary":"s","comments":[],"gate":`+pGate(2, "deadbeef")+`}`))
	if st := other.on("deadbeef", pCtx); st.State != forge.CommitStateFailure {
		t.Fatalf("tenant 2's red never landed on its forge (HTTP %d %s)", rec.Code, rec.Body.String())
	}
	*w.now = reset.Add(time.Minute)
	if err := w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating")); err != nil {
		t.Fatal(err)
	}
	if final := w.f.on("deadbeef", pCtx); final.State != forge.CommitStateSuccess {
		t.Errorf("tenant 1's deferred green never landed on its own forge: %s %q", final.State, final.Description)
	}
}

// A post the forge failed on its own side, and a verdict the order store
// could not place, are deferred like a rate limit — then posted.
func TestATransientRefusalIsDeferredAtTheEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(w pWorld)
	}{
		{"a 502 on the status write", func(w pWorld) {
			w.f.setErrs = []error{&forge.StatusError{Provider: forge.ProviderGitHub, Op: "POST statuses", Code: http.StatusBadGateway}}
		}},
		{"the order store unreachable", func(w pWorld) {
			w.s.gateDecisions = &failingOnceMarks{gateDecisionStore: w.s.gateDecisions}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPWorld(t, gatingInputs(), "deadbeef")
			tc.breakIt(w)
			if r := w.publish(t, "tok-gate", pGate(2, "deadbeef")); r.GatePosted || !strings.Contains(r.GateError, "deferred until") {
				t.Fatalf("the red verdict was not deferred: %+v", r)
			}
			finishRun(t, w.s, "run-gating")
			*w.now = mustGrant(t, w.s, "tok-gate").Deferred.RetryAt.Add(time.Minute)
			if err := w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating")); err != nil {
				t.Fatal(err)
			}
			if final := w.f.on("deadbeef", pCtx); final.State != forge.CommitStateFailure || isSyntheticGateInterruption(final.Description) {
				t.Errorf("the deferred red verdict was not posted: %s %q", final.State, final.Description)
			}
		})
	}
}

// failingOnceMarks fails its first claim, as a store does through a failover.
type failingOnceMarks struct {
	gateDecisionStore
	mu     sync.Mutex
	failed bool
}

func (f *failingOnceMarks) claim(ctx context.Context, key string, m gateMark, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	first := !f.failed
	f.failed = true
	f.mu.Unlock()
	if first {
		return false, errors.New("claim the gate decision: i/o timeout")
	}
	return f.gateDecisionStore.claim(ctx, key, m, ttl)
}

// A deferred verdict that meets a newer decision on the check its run owes
// settles the run: the newer one answers the head, posted or still waiting —
// the ordinary repair would read that wait as this run's death and paint the
// head "review died".
func TestASupersededReplaySettlesItsRun(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("A not deferred: %+v", r)
	}
	finishRun(t, w.s, "run-gating")
	*w.now = reset.Add(time.Minute)
	addGatingRun(t, w, "run-b", "tok-b", "deadbeef")
	w.f.setErrs = []error{&forge.StatusError{Provider: forge.ProviderGitHub, Op: "POST statuses", Code: http.StatusForbidden, Limit: true}}
	if r := w.publish(t, "tok-b", pGate(2, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("B not deferred: %+v", r)
	}
	*w.now = w.now.Add(time.Minute)
	if err := w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating")); err != nil {
		t.Fatal(err)
	}
	marks, err := w.s.gateSettles.settled(context.Background(), []string{"run-gating"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range w.f.posted {
		if strings.Contains(p, gateInterruptedDescription) {
			t.Fatalf("a newer verdict waiting to be posted was read as the run's death: %v", w.f.posted)
		}
	}
	if marks["run-gating"].Reason != gateSettledSuperseded {
		t.Errorf("the superseded run is not settled: %+v", marks["run-gating"])
	}
	finishRun(t, w.s, "run-b")
	*w.now = mustGrant(t, w.s, "tok-b").Deferred.RetryAt.Add(time.Minute)
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-b"))
	if final := w.f.on("deadbeef", pCtx); final.State != forge.CommitStateFailure {
		t.Errorf("the newer red verdict never landed: %s %q", final.State, final.Description)
	}
}

// A deferral replayed from another run's offer on a SHARED grant answers that
// other run's check: the offered run is not settled on it, and its own owed
// check is repaired.
func TestAReplaySettlesOnlyTheRunItAnswers(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	inB := gatingInputs()
	inB["head_sha"] = "deadbeef"
	if _, err := w.s.cfg.Store.CreateRun(context.Background(), "run-b", "other_bot", inB); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.s.shareGrant("tok-gate"); err != nil {
		t.Fatal(err)
	}
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	gate := `{"enabled":true,"context":"other/check","blocking_count":0,"threshold":"high","total_findings":0,"audited_sha":"deadbeef"}`
	if r := w.publish(t, "tok-gate", gate); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("the other check's verdict was not deferred: %+v", r)
	}
	finishRun(t, w.s, "run-b")
	finishRun(t, w.s, "run-gating")
	runA, _ := w.s.cfg.Store.LoadRun(context.Background(), "run-gating")
	runB, _ := w.s.cfg.Store.LoadRun(context.Background(), "run-b")
	lister := &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{
		{ID: runA.ID, Status: string(runA.Status), UpdatedAt: runA.UpdatedAt},
		{ID: runB.ID, Status: string(runB.Status), UpdatedAt: runB.UpdatedAt},
	}}
	for i := 0; i < 90; i++ {
		*w.now = reset.Add(time.Duration(i+1) * 10 * time.Minute)
		w.s.sweepGates(context.Background(), lister, *w.now, gateSweepHorizon, time.Time{})
	}
	if mine := w.f.on("deadbeef", pCtx); isGateInFlight(mine) {
		t.Errorf("run A's owed %s is still on its in-flight claim after 15 h of sweeps: settled on another check's verdict (posted %v)", pCtx, w.f.posted)
	}
}

// Two deferrals on one grant, decided older then newer, written newer first:
// the grant keeps the newer, and the head ends on it.
func TestADeferralKeepsTheNewerOfTwoDecisions(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	gate := func(blocking int) *publishReviewGate {
		return &publishReviewGate{Enabled: true, Context: pCtx, BlockingCount: blocking, TotalFindings: blocking, AuditedSHA: "deadbeef"}
	}
	older := gateDecision{ID: "older", At: w.now.Add(-2 * time.Second)}
	newer := gateDecision{ID: "newer", At: w.now.Add(-time.Second)}
	limited := func() *gateOutcome {
		return &gateOutcome{requested: true, rateLimited: true, resetAt: reset, errText: "limited"}
	}
	w.s.deferGateVerdict("tok-gate", "o/r", 42, gate(3), "", newer, limited())
	out := limited()
	w.s.deferGateVerdict("tok-gate", "o/r", 42, gate(0), "", older, out)
	if g := mustGrant(t, w.s, "tok-gate"); g.Deferred == nil || g.Deferred.Decision.ID != "newer" {
		t.Fatalf("the grant keeps %+v, want the newer decision", g.Deferred)
	}
	if strings.Contains(out.errText, "deferred until") {
		t.Errorf("the older verdict reports itself deferred: %q", out.errText)
	}
	finishRun(t, w.s, "run-gating")
	*w.now = reset.Add(time.Minute)
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	if final := w.f.on("deadbeef", pCtx); final.State != forge.CommitStateFailure {
		t.Errorf("the head shows %s, want the newer decision's failure", final.State)
	}
}

// A verdict landing on ANOTHER check of the same grant leaves this check's
// deferral alone: it is another check's answer.
func TestAVerdictOnAnotherCheckLeavesTheDeferral(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("not deferred: %+v", r)
	}
	*w.now = reset.Add(time.Minute)
	other := `{"enabled":true,"context":"iterion/security","blocking_count":0,"threshold":"high","total_findings":0,"audited_sha":"deadbeef"}`
	if r := w.publish(t, "tok-gate", other); !r.GatePosted {
		t.Fatalf("the second check: %+v", r)
	}
	if g := mustGrant(t, w.s, "tok-gate"); g.Deferred == nil {
		t.Error("a verdict on iterion/security retired the deferred iterion/review verdict")
	}
}

// A refusal a retry would repeat — a permission the installation withholds, a
// connection deleted, a typed permission error — is replayed once, not
// twelve times over ten hours.
func TestARefusalARetryRepeatsIsReplayedOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a permission the installation withholds", fmt.Errorf("mint installation token: %w", forge.ErrPermissionsNotGranted)},
		{"a typed permission error", &forge.PermissionError{Provider: forge.ProviderGitHub, Op: "POST statuses", Missing: []string{"statuses:write"}, Cause: forge.ErrForbidden}},
		{"a deleted connection", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPWorld(t, gatingInputs(), "deadbeef")
			reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
			w.f.limitedUntil = reset
			if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
				t.Fatalf("not deferred: %+v", r)
			}
			finishRun(t, w.s, "run-gating")
			w.f.limitedUntil = time.Time{}
			if tc.err != nil {
				w.s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return nil, tc.err }
			} else if err := w.s.forgeConnections.Delete(context.Background(), "conn1"); err != nil {
				t.Fatal(err)
			}
			replays := 0
			for i := 0; i < 40; i++ {
				g := mustGrant(t, w.s, "tok-gate")
				if g.Deferred == nil {
					break
				}
				*w.now = g.Deferred.RetryAt.Add(time.Minute)
				replays++
				_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
			}
			if replays != 1 {
				t.Errorf("%d replays before the deferral was dropped, want 1", replays)
			}
		})
	}
}

// A run that owes no gate repair keeps its grant only while a deferral needs
// it: once the deferral is refused for good, the grant is cut back like the
// reaper would have.
func TestTheGrantIsCutBackOnceItsDeferralIsGone(t *testing.T) {
	in := gatingInputs()
	delete(in, "gate_context")
	w := newPWorld(t, in, "deadbeef")
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("not deferred: %+v", r)
	}
	finishRun(t, w.s, "run-gating")
	if err := w.s.expireForgePublishGrantForRun(context.Background(), "run-gating"); err != nil {
		t.Fatal(err)
	}
	w.f.head = "0ther5ha" // the head moved: the replay is refused for good
	*w.now = reset.Add(time.Minute)
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	if g := mustGrant(t, w.s, "tok-gate"); !g.CutBack {
		t.Errorf("the grant of a run that owes nothing outlives its cleared deferral: deferred=%v, lives %s more", g.Deferred != nil, time.Until(g.ExpiresAt).Round(time.Hour))
	}
}

// A mark outlives every deferral that could still replay an older decision:
// a day, or a week, after a newer decision claimed the head, an older one
// still may not post over it.
func TestTheMarkOutlivesEveryDeferral(t *testing.T) {
	mr, rdb := newTestRedis(t)
	st := newValkeyGateDecisionStore(rdb)
	key := gateDecisionKey(forge.Connection{Provider: forge.ProviderGitHub}, "o/r", "deadbeef", pCtx)
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	newer, older := gateMark{Decision: gateDecision{ID: "newer", At: t0.Add(time.Minute)}}, gateMark{Decision: gateDecision{ID: "older", At: t0}}
	if ok, err := st.claim(context.Background(), key, newer, gateDecisionTTL); !ok || err != nil {
		t.Fatal(ok, err)
	}
	elapsed := time.Duration(0)
	for _, later := range []time.Duration{25 * time.Hour, forgePublishDefaultTTL + forgePublishGateGrace - time.Hour} {
		mr.FastForward(later - elapsed)
		elapsed = later
		if ok, _ := st.claim(context.Background(), key, older, gateDecisionTTL); ok {
			t.Errorf("%s after the newer decision, the older one may post over it", later)
		}
	}
}

// Full, the in-memory order store says so once per saturation, however claims
// on held and fresh heads alternate.
func TestTheMemoryMarksSaySaturationOnce(t *testing.T) {
	var buf bytes.Buffer
	st := newMemoryGateDecisionStore(iterlog.New(iterlog.LevelWarn, &buf))
	ctx := context.Background()
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < gateDecisionMaxMarks; i++ {
		_, _ = st.claim(ctx, fmt.Sprintf("k%d", i), gateMark{Decision: gateDecision{ID: "x", At: t0}}, time.Hour)
	}
	for i := 0; i < 5; i++ {
		_, _ = st.claim(ctx, "k0", gateMark{Decision: gateDecision{ID: "y", At: t0.Add(time.Duration(i) * time.Millisecond)}}, time.Hour)
		_, _ = st.claim(ctx, fmt.Sprintf("new%d", i), gateMark{Decision: gateDecision{ID: "z", At: t0}}, time.Hour)
	}
	if n := strings.Count(buf.String(), "verdict-order marks are full"); n != 1 {
		t.Errorf("the saturation was said %d times, want once", n)
	}
}

type readOnlyGrants struct{ ForgePublishTokenStore }

func (readOnlyGrants) update(string, func(*ForgePublishGrant)) (bool, error) {
	return false, errors.New("READONLY You can't write against a read only replica")
}

// While the grant cannot be written, a due deferral is not posted: its attempt
// could not be booked, and every pass would otherwise spend the limited
// budget again.
func TestAReplayIsBookedBeforeItIsPosted(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("not deferred: %+v", r)
	}
	finishRun(t, w.s, "run-gating")
	w.f.limitedUntil = reset.Add(6 * time.Hour)
	w.s.forgePublishTokens = readOnlyGrants{w.s.forgePublishTokens}
	*w.now = reset.Add(time.Minute)
	gets, sets := w.f.gets, w.f.sets
	for i := 0; i < 60; i++ {
		*w.now = w.now.Add(time.Minute)
		_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	}
	if w.f.gets != gets || w.f.sets != sets {
		t.Errorf("an hour of passes with an unwritable grant spent %d reads and %d writes on the limited budget", w.f.gets-gets, w.f.sets-sets)
	}
}

// A verdict the forge keeps refusing gets twelve attempts in all, the refused
// original included; the next pass drops it without trying again, and the run
// is answered like any that left no verdict.
func TestADeferredVerdictGetsTwelveAttemptsInAll(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	w.f.limitedUntil = w.now.Add(30 * 24 * time.Hour)
	tries := w.f.gets // each attempt resolves the head first, and is refused there
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("not deferred: %+v", r)
	}
	finishRun(t, w.s, "run-gating")
	for i := 0; i < 40; i++ {
		g := mustGrant(t, w.s, "tok-gate")
		if g.Deferred == nil || g.Deferred.Attempts >= gateDeferMaxAttempts {
			break
		}
		*w.now = g.Deferred.RetryAt.Add(time.Minute)
		_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	}
	if n := w.f.gets - tries; n != gateDeferMaxAttempts {
		t.Fatalf("%d attempts, want %d", n, gateDeferMaxAttempts)
	}
	g := mustGrant(t, w.s, "tok-gate")
	if g.Deferred == nil {
		t.Fatal("the deferral is gone before its last attempt was answered")
	}
	*w.now = g.Deferred.RetryAt.Add(time.Minute)
	_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating"))
	if g := mustGrant(t, w.s, "tok-gate"); g.Deferred != nil {
		t.Errorf("the deferral outlived its %d attempts: %+v", gateDeferMaxAttempts, g.Deferred)
	}
}

// A decision that will neither post nor wait — its post refused for good —
// lets go of the check: the older deferred verdict then lands, where the
// ghost would have superseded it forever, leaving the required check on its
// in-flight claim with nobody to answer it.
func TestAGhostClaimIsReleased(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("A not deferred: %+v", r)
	}
	finishRun(t, w.s, "run-gating")
	*w.now = reset.Add(time.Minute)
	addGatingRun(t, w, "run-b", "tok-b", "deadbeef")
	w.f.setErrs = []error{&forge.StatusError{Provider: forge.ProviderGitHub, Op: "POST statuses", Code: http.StatusUnprocessableEntity}}
	if r := w.publish(t, "tok-b", pGate(2, "deadbeef")); r.GatePosted || strings.Contains(r.GateError, "deferred") {
		t.Fatalf("B: %+v", r)
	}
	*w.now = mustGrant(t, w.s, "tok-gate").Deferred.RetryAt.Add(time.Minute)
	if err := w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating")); err != nil {
		t.Fatal(err)
	}
	if final := w.f.on("deadbeef", pCtx); final.State != forge.CommitStateSuccess || isSyntheticGateInterruption(final.Description) {
		t.Errorf("the older green never landed behind a ghost claim: %s %q", final.State, final.Description)
	}
}

// failingOnceGrants refuses its first grant write, as a store does through a
// failover.
type failingOnceGrants struct {
	ForgePublishTokenStore
	mu     sync.Mutex
	failed bool
}

func (f *failingOnceGrants) update(token string, fn func(*ForgePublishGrant)) (bool, error) {
	f.mu.Lock()
	first := !f.failed
	f.failed = true
	f.mu.Unlock()
	if first {
		return false, errors.New("READONLY You can't write against a read only replica")
	}
	return f.ForgePublishTokenStore.update(token, fn)
}

// A verdict the forge limited, whose deferral could not be written, also lets
// go of the check — as does one no bot pinned, and one the forge refused for
// good: every verdict nobody will post or wait for.
func TestAClaimNobodyTakesInIsReleased(t *testing.T) {
	for _, tc := range []struct {
		name    string
		publish func(w pWorld, reset time.Time)
	}{
		{"a deferral that could not be written", func(w pWorld, reset time.Time) {
			w.s.forgePublishTokens = &failingOnceGrants{ForgePublishTokenStore: w.s.forgePublishTokens}
			addGatingRun(t, w, "run-b", "tok-b", "deadbeef")
			// The forge limits again — at the status write this time.
			w.f.limitedUntil = time.Time{}
			w.f.setErrs = []error{&forge.StatusError{Provider: forge.ProviderGitHub, Op: "POST statuses", Code: http.StatusForbidden, Limit: true, ResetAt: reset.Add(time.Hour)}}
			if r := w.publish(t, "tok-b", pGate(2, "deadbeef")); strings.Contains(r.GateError, "deferred") {
				t.Fatalf("B reports itself deferred: %+v", r)
			}
		}},
		{"a verdict no bot pinned", func(w pWorld, reset time.Time) {
			addGatingRun(t, w, "run-b", "tok-b", "deadbeef")
			w.f.limitedUntil = time.Time{}
			w.f.setErrs = []error{&forge.StatusError{Provider: forge.ProviderGitHub, Op: "POST statuses", Code: http.StatusForbidden, Limit: true, ResetAt: reset.Add(time.Hour)}}
			unpinned := `{"enabled":true,"context":"` + pCtx + `","blocking_count":2,"threshold":"high","total_findings":2}`
			if r := w.publish(t, "tok-b", unpinned); strings.Contains(r.GateError, "deferred") {
				t.Fatalf("an unpinned verdict reports itself deferred: %+v", r)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPWorld(t, gatingInputs(), "deadbeef")
			reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
			w.f.limitedUntil = reset
			if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
				t.Fatalf("A not deferred: %+v", r)
			}
			finishRun(t, w.s, "run-gating")
			*w.now = reset.Add(time.Minute)
			tc.publish(w, reset)
			*w.now = mustGrant(t, w.s, "tok-gate").Deferred.RetryAt.Add(time.Minute)
			if err := w.s.reconcileGateForRun(context.Background(), terminalEvent("run-gating")); err != nil {
				t.Fatal(err)
			}
			if final := w.f.on("deadbeef", pCtx); final.State != forge.CommitStateSuccess || isSyntheticGateInterruption(final.Description) {
				t.Errorf("the older green never landed behind a claim nobody took in: %s %q", final.State, final.Description)
			}
		})
	}
}

// A released claim stands for its predecessor: the newest live decision, the
// one that posted before it or waits to — on both twins.
func TestAReleasedClaimStandsForItsPredecessor(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	a, b, older := gateMark{Decision: gateDecision{ID: "a", At: t0}}, gateMark{Decision: gateDecision{ID: "b", At: t0.Add(time.Minute)}}, gateMark{Decision: gateDecision{ID: "z", At: t0.Add(-time.Minute)}}
	for _, backend := range []string{"memory", "valkey"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var st gateDecisionStore = newMemoryGateDecisionStore(nil)
			if backend == "valkey" {
				_, rdb := newTestRedis(t)
				st = newValkeyGateDecisionStore(rdb)
			}
			github := forge.Connection{Provider: forge.ProviderGitHub}
			key := gateDecisionKey(github, "o/r", "deadbeef", pCtx)
			if ok, err := st.claim(ctx, key, a, time.Hour); !ok || err != nil {
				t.Fatal(ok, err)
			}
			if ok, err := st.claim(ctx, key, b, time.Hour); !ok || err != nil {
				t.Fatal(ok, err)
			}
			if err := st.release(ctx, key, b.Decision); err != nil {
				t.Fatal(err)
			}
			if m, found, _ := st.newest(ctx, key); !found || m.Decision.ID != "a" {
				t.Errorf("the released claim still stands: %+v (found %v)", m, found)
			}
			if ok, _ := st.claim(ctx, key, older, time.Hour); ok {
				t.Error("a decision older than the predecessor may post over it")
			}
			if ok, err := st.claim(ctx, key, a, time.Hour); !ok || err != nil {
				t.Errorf("the predecessor re-claiming its own mark: %v (err %v)", ok, err)
			}
			// Releasing a decision that is not the claim's top — a's own
			// claim is b's predecessor, not the top — is a no-op: only the
			// top can let go.
			if err := st.release(ctx, key, a.Decision); err != nil {
				t.Fatal(err)
			}
			if m, found, _ := st.newest(ctx, key); !found || m.Decision.ID != "a" {
				t.Errorf("after the no-op release the predecessor no longer stands: %+v (found %v)", m, found)
			}
		})
	}
}

// escalatingMarks names a newer decision on every read, as a claim landing
// inside each write does; the re-assert stops at its bound and SAYS what it
// could not put back.
func TestTheReassertSaysWhatItCouldNotPutBack(t *testing.T) {
	var buf bytes.Buffer
	s := newWebhookTestServer(t)
	s.logger = iterlog.New(iterlog.LevelWarn, &buf)
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	marks := &escalatingMarks{gateDecisionStore: newMemoryGateDecisionStore(nil), base: base}
	s.gateDecisions = marks
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s.reassertNewerVerdict(context.Background(), gc, "k", "o/r", "deadbeef", pCtx, gateDecision{ID: "start", At: base})
	if n := len(gc.posted); n != gateReassertMax {
		t.Errorf("%d re-assert writes, want the bound %d", n, gateReassertMax)
	}
	if !strings.Contains(buf.String(), "during the re-assert") {
		t.Errorf("the bounded re-assert stayed silent about a newer verdict still under; log:\n%s", buf.String())
	}
}

// escalatingMarks is a decision store whose marks read newer on every read.
type escalatingMarks struct {
	gateDecisionStore
	base  time.Time
	calls int
}

func (e *escalatingMarks) newest(context.Context, string) (gateMark, bool, error) {
	e.calls++
	d := gateDecision{ID: fmt.Sprintf("e%d", e.calls), At: e.base.Add(time.Duration(e.calls) * time.Minute)}
	return gateMark{Decision: d, Status: gateMarkStatus{State: "failure", Description: "verdict " + d.ID}}, true, nil
}

// Two replays of one deferral read before either booked it — the event path
// beside the sweep — spend one attempt: the second finds it booked and posts
// nothing.
func TestAnAttemptIsBookedOnce(t *testing.T) {
	ctx := context.Background()
	w := newPWorld(t, gatingInputs(), "deadbeef")
	reset := w.now.Add(30 * time.Minute).Truncate(time.Second)
	w.f.limitedUntil = reset
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !strings.Contains(r.GateError, "deferred") {
		t.Fatalf("not deferred: %+v", r)
	}
	finishRun(t, w.s, "run-gating")
	w.f.limitedUntil = reset.Add(6 * time.Hour) // still refusing: the attempt is re-armed
	*w.now = reset.Add(time.Minute)
	g := mustGrant(t, w.s, "tok-gate")
	run, err := w.s.cfg.Store.LoadRun(ctx, "run-gating")
	if err != nil {
		t.Fatal(err)
	}
	gets := w.f.gets
	w.s.replayGateDeferral(ctx, run, "tok-gate", g, g.Deferred)
	w.s.replayGateDeferral(ctx, run, "tok-gate", g, g.Deferred) // the same read, taken before the first booked
	if n := w.f.gets - gets; n != 1 {
		t.Errorf("one attempt booked twice: %d attempts", n)
	}
}

// A run parked on an armed retry is not over: its grant is not cut back, as
// the reaper already keeps it — it will publish again.
func TestAGrantIsKeptForARunParkedOnAnArmedRetry(t *testing.T) {
	w := newPWorld(t, gatingInputs(), "deadbeef")
	run, err := w.s.cfg.Store.LoadRun(context.Background(), "run-gating")
	if err != nil {
		t.Fatal(err)
	}
	after := w.now.Add(time.Hour)
	run.Status = store.RunStatusFailedResumable
	run.RetryState = &store.RunRetryState{RetryAfter: &after, Reason: "usage_window", Attempts: 1}
	if err := w.s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	w.s.cutBackGrant(run, "tok-gate")
	if g := mustGrant(t, w.s, "tok-gate"); g.CutBack {
		t.Error("the grant of a run parked on an armed retry was cut back")
	}
}
