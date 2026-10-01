package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// forEachSettleStore runs one row against both twins; advance moves the
// store's clock (miniredis's for Valkey).
func forEachSettleStore(t *testing.T, row func(t *testing.T, st gateSettleStore, advance func(time.Duration))) {
	t.Run("memory", func(t *testing.T) {
		clock := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
		m := newMemoryGateSettleStore(nil)
		m.now = func() time.Time { return clock }
		row(t, m, func(d time.Duration) { clock = clock.Add(d) })
	})
	t.Run("valkey", func(t *testing.T) {
		mr, rdb := newTestRedis(t)
		row(t, newValkeyGateSettleStore(rdb, nil), mr.FastForward)
	})
}

// A mark holds for its TTL, carries what it was written with, and exists for
// the runs it names only.
func TestGateSettleStore_AMarkHoldsForItsTTL(t *testing.T) {
	forEachSettleStore(t, func(t *testing.T, st gateSettleStore, advance func(time.Duration)) {
		want := gateSettlement{Reason: gateSettledVerdictSuccess, SHA: "deadbeef", Episode: 1727700000123}
		st.settle("run-a", want, time.Hour)
		marks, err := st.settled(context.Background(), []string{"run-a", "run-b"})
		if err != nil {
			t.Fatal(err)
		}
		if len(marks) != 1 || marks["run-a"] != want {
			t.Fatalf("marks = %+v, want only run-a = %+v", marks, want)
		}
		advance(59 * time.Minute)
		if marks, _ := st.settled(context.Background(), []string{"run-a"}); len(marks) != 1 {
			t.Fatal("the mark lapsed before its TTL")
		}
		advance(2 * time.Minute)
		if marks, _ := st.settled(context.Background(), []string{"run-a"}); len(marks) != 0 {
			t.Fatalf("the mark outlived its TTL: %+v", marks)
		}
	})
}

// A mark is written for one terminal episode: a resumed run that ends again
// is a new episode, and must be looked at again.
func TestGateSettlement_AppliesToItsEpisodeOnly(t *testing.T) {
	ended := time.Date(2026, 9, 30, 12, 0, 0, 123_000_000, time.UTC)
	g := gateSettlement{Reason: gateSettledMerged, Episode: ended.UnixMilli()}
	if !g.appliesTo(ended) {
		t.Error("a mark does not apply to the episode it was written for")
	}
	if g.appliesTo(ended.Add(time.Second)) {
		t.Error("a mark applies to a later episode of the run")
	}
	zero := gateSettlement{Reason: gateSettledMerged, Episode: time.Time{}.UnixMilli()}
	if zero.appliesTo(time.Time{}) {
		t.Error("a mark applies to a run with no updated_at")
	}
}

// settledRun seeds a gating run in the reconcile fixture and returns it with
// its episode instant, as the sweep's lister would report it.
func settledFixture(t *testing.T, inputs map[string]any, gc forgeGateClient) (*Server, *store.Run) {
	t.Helper()
	s, runID := gateReconcileFixture(t, inputs, gc)
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return s, run
}

// A run the reconciler found settled costs the sweep nothing: not a forge read
// — and a mark written for an EARLIER episode of the run settles nothing.
func TestGateSweep_ASettledRunIsNotOfferedAgain(t *testing.T) {
	gc := &readCountingGateClient{listingGateClient: &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}}
	s, run := settledFixture(t, gatingInputs(), gc)
	lister := &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: run.ID, UpdatedAt: run.UpdatedAt}}}

	s.gateSettles.settle(run.ID, gateSettlement{Reason: gateSettledMerged, Episode: run.UpdatedAt.UnixMilli()}, time.Hour)
	for i := 0; i < 3; i++ {
		s.sweepGates(context.Background(), lister, time.Now().UTC(), gateSweepLookback, time.Time{})
	}
	if gc.gets != 0 || gc.setCalls != 0 {
		t.Fatalf("a settled run cost %d reads and %d writes over three passes, want none", gc.gets, gc.setCalls)
	}

	s.gateSettles.settle(run.ID, gateSettlement{Reason: gateSettledMerged, Episode: run.UpdatedAt.Add(-time.Minute).UnixMilli()}, time.Hour)
	s.sweepGates(context.Background(), lister, time.Now().UTC(), gateSweepLookback, time.Time{})
	if gc.gets == 0 {
		t.Error("a mark written for an earlier episode kept the run's new episode from the reconciler")
	}
}

// failingSettleStore cannot read its marks.
type failingSettleStore struct {
	failing atomic.Bool
	inner   gateSettleStore
}

func (f *failingSettleStore) settle(id string, g gateSettlement, ttl time.Duration) {
	f.inner.settle(id, g, ttl)
}

func (f *failingSettleStore) settled(ctx context.Context, ids []string) (map[string]gateSettlement, error) {
	if f.failing.Load() {
		return nil, errors.New("valkey: connection refused")
	}
	return f.inner.settled(ctx, ids)
}

// Marks the sweep cannot read settle nothing: every run is offered, as before
// the marks existed — the net's reach is never traded for its cost — and the
// episode is reported once, with its recovery.
func TestGateSweep_UnreadableMarksSettleNothingAndSaySoOnce(t *testing.T) {
	gc := &readCountingGateClient{listingGateClient: &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}}
	s, run := settledFixture(t, gatingInputs(), gc)
	out := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelInfo, out)
	fs := &failingSettleStore{inner: newMemoryGateSettleStore(nil)}
	s.gateSettles = fs
	fs.settle(run.ID, gateSettlement{Reason: gateSettledMerged, Episode: run.UpdatedAt.UnixMilli()}, time.Hour)
	fs.failing.Store(true)
	lister := &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: run.ID, UpdatedAt: run.UpdatedAt}}}

	for i := 0; i < 3; i++ {
		s.sweepGates(context.Background(), lister, time.Now().UTC(), gateSweepLookback, time.Time{})
	}
	if gc.gets != 3 {
		t.Errorf("with unreadable marks the run was read %d times over three passes, want 3 — it must be offered as if unsettled", gc.gets)
	}
	if n := strings.Count(out.String(), "settled marks are unreadable"); n != 1 {
		t.Errorf("the read failure was reported %d times, want once; log:\n%s", n, out.String())
	}
	fs.failing.Store(false)
	s.sweepGates(context.Background(), lister, time.Now().UTC(), gateSweepLookback, time.Time{})
	if !strings.Contains(out.String(), "readable again") {
		t.Errorf("the recovery was not reported; log:\n%s", out.String())
	}
}

// The mark the reconciler writes names what it found, and lasts as long as
// that fact can be trusted: a merge and a real verdict for the rest of the
// horizon, a closed pull request and a moved head only until they are worth
// re-reading — both can change back. A merge also retires the run's grant to
// the post-run grace: nothing will post for a merged pull request.
func TestGateReconcile_WritesTheSettlementItFound(t *testing.T) {
	realVerdict := forge.CommitStatus{Context: "iterion/review", State: forge.CommitStateFailure, Description: "2 blocking findings", TargetURL: "https://github.com/o/r/pull/42#pullrequestreview-7"}
	withInputs := func(edit func(map[string]any)) map[string]any {
		in := gatingInputs()
		if edit != nil {
			edit(in)
		}
		return in
	}
	for _, tc := range []struct {
		name       string
		inputs     map[string]any
		pr         fakeGateClient
		statuses   []forge.CommitStatus
		endReason  store.RunEndReason
		wantReason string
		permanent  bool
		wantReads  bool
		cutBack    bool
	}{
		{name: "merged", inputs: withInputs(nil), pr: fakeGateClient{headSHA: "deadbeef", state: "merged"}, wantReason: gateSettledMerged, permanent: true, wantReads: true, cutBack: true},
		{name: "closed", inputs: withInputs(nil), pr: fakeGateClient{headSHA: "deadbeef", state: "closed"}, wantReason: gateSettledClosed, wantReads: true},
		{name: "head moved", inputs: withInputs(nil), pr: fakeGateClient{headSHA: "0ther5ha"}, wantReason: gateSettledHeadMoved, wantReads: true},
		{name: "a real verdict on the reviewed head", inputs: withInputs(nil), pr: fakeGateClient{headSHA: "deadbeef"}, statuses: []forge.CommitStatus{realVerdict}, wantReason: gateSettledVerdictFailure, permanent: true, wantReads: true},
		{name: "no reviewed revision", inputs: withInputs(func(in map[string]any) { delete(in, "head_sha") }), pr: fakeGateClient{headSHA: "deadbeef"}, wantReason: gateSettledUnpinned, permanent: true},
		{name: "cancelled when its pull request closed", inputs: withInputs(nil), pr: fakeGateClient{headSHA: "deadbeef", state: "closed"}, endReason: store.RunEndReasonPRClosed, wantReason: gateSettledClosed, wantReads: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gc := &readCountingGateClient{listingGateClient: &listingGateClient{fakeGateClient: tc.pr, statuses: tc.statuses}}
			s, run := settledFixture(t, tc.inputs, gc)
			run = finishRun(t, s, run.ID)
			if tc.endReason != "" {
				// The stop-on-close lane cancels the run and says why; the
				// store keeps an end reason only on such a status.
				run.Status = store.RunStatusCancelled
				run.EndReason = tc.endReason
				if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
					t.Fatal(err)
				}
				if run, _ = s.cfg.Store.LoadRun(context.Background(), run.ID); run == nil {
					t.Fatal("run vanished")
				}
			}
			if err := s.reconcileGateForRun(context.Background(), terminalEvent(run.ID)); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := gc.gets > 0; got != tc.wantReads {
				t.Errorf("the reconcile read the pull request %d times, want reads=%v", gc.gets, tc.wantReads)
			}
			m := s.gateSettles.(*memoryGateSettleStore)
			m.mu.Lock()
			mark, ok := m.marks[run.ID]
			m.mu.Unlock()
			if !ok || mark.g.Reason != tc.wantReason || !mark.g.appliesTo(run.UpdatedAt) {
				t.Fatalf("mark = %+v (present=%v), want %q for the run's episode", mark.g, ok, tc.wantReason)
			}
			ttl := time.Until(mark.expires)
			if tc.permanent && ttl < gateSweepHorizon-time.Hour {
				t.Errorf("a permanent settlement (%s) lasts %s, want the rest of the %s horizon", tc.wantReason, ttl.Round(time.Minute), gateSweepHorizon)
			}
			if !tc.permanent && (ttl > gateSettleRecheck || ttl < gateSettleRecheck-time.Minute) {
				t.Errorf("a reversible settlement (%s) lasts %s, want the %s re-check", tc.wantReason, ttl.Round(time.Minute), gateSettleRecheck)
			}
			g := mustGrant(t, s, "tok-gate")
			if cut := g.CutBack && time.Until(g.ExpiresAt) <= s.postRunGrace()+time.Minute; cut != tc.cutBack {
				t.Errorf("settled %s: the grant cut back=%v (lives %s), want %v", tc.wantReason, cut, time.Until(g.ExpiresAt).Round(time.Minute), tc.cutBack)
			}
		})
	}
}

// autofixSweepWorld is a gating run on a repo with the auto-fix lane on, a red
// verdict on its reviewed head, and a fixer launch the test can count.
type autofixSweepWorld struct {
	s        *Server
	run      *store.Run
	gc       *laneTaggingStub
	launched *atomic.Int32
}

func newAutofixSweepWorld(t *testing.T) autofixSweepWorld {
	t.Helper()
	const (
		team  = "t1"
		repo  = "acme/widgets"
		prURL = "https://github.com/acme/widgets/pull/7"
		head  = "cafe1234cafe1234cafe1234cafe1234cafe1234"
	)
	s := newWebhookTestServer(t)
	s.cfg.WorkDir = writeConsumerBotFixture(t, "fixer-bot", "prior_review")
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "c1", TenantID: team, Provider: forge.ProviderGitHub}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	s.forgePublishTokens = NewForgePublishTokenRegistry()
	s.gateSettles = newMemoryGateSettleStore(nil)
	if err := s.forgePublishTokens.Register("run-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	ints := forge.NewMemoryRepoIntegrationStore()
	if err := ints.Create(context.Background(), forge.RepoIntegration{
		ID: "i1", TenantID: team, ConnectionID: "c1", RepoFullName: repo,
		BotIDs: []string{"fixer-bot"}, WebhookID: "w1", AutoFixOnGateFailure: true,
		LaunchVars: map[string]string{gateContextVar: "iterion/review"},
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeIntegrations = ints
	if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
		ID: "w1", TenantID: team, BotIDs: []string{"fixer-bot"},
	}); err != nil {
		t.Fatal(err)
	}
	gc := &laneTaggingStub{stubGateClient: stubGateClient{head: head, state: forge.CommitStateFailure, ctxName: "iterion/review"}}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	var launched atomic.Int32
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched.Add(1)
		return "run-fixer", nil
	}
	id, err := store.GenerateRunID()
	if err != nil {
		t.Fatal(err)
	}
	run, err := rs.CreateRun(context.Background(), id, "reviewer-bot", map[string]any{
		"pr_url": prURL, "gate_context": "iterion/review", "head_sha": head,
		forgePublishVarToken: "run-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	run.BotID = "reviewer-bot"
	run.Status = store.RunStatusFinished
	if err := rs.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if run, err = rs.LoadRun(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return autofixSweepWorld{s: s, run: run, gc: gc, launched: &launched}
}

// A failure verdict settles the gate, but it is the auto-fix lane's own
// trigger: the sweep still offers such a run to that lane, and only to it.
// Any other settlement closes both lanes.
func TestGateSweep_AFailureVerdictStillReachesTheAutofixLane(t *testing.T) {
	for _, tc := range []struct {
		reason       string
		wantLaunched int32
	}{
		{gateSettledVerdictFailure, 1},
		{gateSettledMerged, 0},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			w := newAutofixSweepWorld(t)
			w.s.gateSettles.settle(w.run.ID, gateSettlement{Reason: tc.reason, Episode: w.run.UpdatedAt.UnixMilli()}, time.Hour)
			w.s.sweepGates(context.Background(), &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: w.run.ID, UpdatedAt: w.run.UpdatedAt}}}, time.Now().UTC(), gateSweepLookback, time.Time{})
			if got := w.launched.Load(); got != tc.wantLaunched {
				t.Errorf("a run settled %q launched %d fixers, want %d", tc.reason, got, tc.wantLaunched)
			}
			w.gc.mu.Lock()
			reads := len(w.gc.lanes)
			w.gc.mu.Unlock()
			if tc.wantLaunched == 1 && reads != 1 {
				t.Errorf("a run settled on a failure verdict read its pull request %d times, want 1 — the fix lane's, not the reconciler's", reads)
			}
		})
	}
}

// failingGrantUpdateStore refuses every grant update.
type failingGrantUpdateStore struct{ ForgePublishTokenStore }

func (failingGrantUpdateStore) update(string, func(*ForgePublishGrant)) (bool, error) {
	return false, errors.New("valkey: connection refused")
}

// A second launch that pins an existing grant makes it shared, and a grant
// that cannot be marked is refused like one that cannot be minted: shortened
// under the other run, it would leave that run's verdict unpostable.
func TestAPinnedGrantIsMarkedSharedOrRefused(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.test"
	registerPublishToken(t, s, "tok-pinned", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	vars := map[string]string{"pr_url": "https://github.com/o/r/pull/42", forgePublishVarToken: "tok-pinned"}
	if _, err := s.injectForgePublishVars(context.Background(), "team1", "conn1", "review-pr", vars, nil, store.RunTrustDefault); err != nil {
		t.Fatalf("pinned launch: %v", err)
	}
	if g, ok := s.forgePublishTokens.lookup("tok-pinned"); !ok || !g.Shared {
		t.Fatalf("the pinned grant = %+v (present=%v), want it marked shared", g, ok)
	}

	out := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelWarn, out)
	if _, err := s.injectForgePublishVars(context.Background(), "team1", "conn1", "review-pr", map[string]string{"pr_url": "https://github.com/o/r/pull/42", forgePublishVarToken: "tok-gone"}, nil, store.RunTrustDefault); err != nil {
		t.Fatalf("pinned launch on a grant that is gone: %v", err)
	}
	if !strings.Contains(out.String(), "expired or revoked") {
		t.Errorf("a launch pinning a grant that is gone was not reported — its publish is refused with no warning; log:\n%s", out.String())
	}

	s.forgePublishTokens = failingGrantUpdateStore{ForgePublishTokenStore: s.forgePublishTokens}
	_, err := s.injectForgePublishVars(context.Background(), "team1", "conn1", "review-pr", map[string]string{"pr_url": "https://github.com/o/r/pull/42", forgePublishVarToken: "tok-pinned"}, nil, store.RunTrustDefault)
	if !errors.Is(err, errForgePublishGrantUnavailable) {
		t.Fatalf("a pinned grant that cannot be marked shared launched anyway (err=%v)", err)
	}
}

// update rewrites a live grant in place: its expiry stays where it was — a
// shortened grant is not lengthened back — and a grant that is gone is not
// re-created.
func TestForgePublishGrantUpdate_KeepsTheExpiryAndNeverResurrects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(t *testing.T) (ForgePublishTokenStore, func(token string) time.Duration)
	}{
		{"memory", func(t *testing.T) (ForgePublishTokenStore, func(string) time.Duration) {
			r := NewForgePublishTokenRegistry()
			return r, func(token string) time.Duration {
				r.mu.RLock()
				defer r.mu.RUnlock()
				return time.Until(r.tokens[token].ExpiresAt)
			}
		}},
		{"valkey", func(t *testing.T) (ForgePublishTokenStore, func(string) time.Duration) {
			mr, rdb := newTestRedis(t)
			return newValkeyForgePublishTokenStore(rdb, nil), func(token string) time.Duration {
				return mr.TTL(forgePublishTokenKeyPrefix + token)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ttlOf := tc.store(t)
			if ok, err := st.update("unknown", func(*ForgePublishGrant) {}); err != nil || ok {
				t.Fatalf("update of an unknown token = %v, %v; want false, nil", ok, err)
			}
			if err := st.Register("tok", ForgePublishGrant{TeamID: "t", ConnectionID: "c", Repo: "o/r"}); err != nil {
				t.Fatal(err)
			}
			st.expireIn("tok", time.Hour)
			ok, err := st.update("tok", func(g *ForgePublishGrant) {
				g.Verdict = &gateVerdict{SHA: "deadbeef", Context: "iterion/review", State: "success"}
			})
			if err != nil || !ok {
				t.Fatalf("update = %v, %v", ok, err)
			}
			if ttl := ttlOf("tok"); ttl > time.Hour || ttl < 59*time.Minute {
				t.Errorf("after the update the grant lives %s, want the hour it was shortened to", ttl)
			}
			if g, ok := st.lookup("tok"); !ok || g.Verdict == nil || g.Verdict.SHA != "deadbeef" {
				t.Errorf("the update did not persist: %+v (present=%v)", g, ok)
			}
			st.Revoke("tok")
			if ok, err := st.update("tok", func(g *ForgePublishGrant) { g.Shared = true }); err != nil || ok {
				t.Fatalf("update of a revoked grant = %v, %v; want false, nil", ok, err)
			}
			if _, ok := st.lookup("tok"); ok {
				t.Error("the update re-created a revoked grant")
			}
		})
	}
}

// The sweep's cadence is the operator's to set; a value that would open a gap
// between two windows, or does not parse, keeps its default and says so.
func TestGateSweepSettingsFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       map[string]string
		want      gateSweepSettings
		wantWarns int
	}{
		{"defaults", nil, defaultGateSweepSettings(), 0},
		{"slower sweep", map[string]string{"ITERION_GATE_SWEEP_INTERVAL": "5m", "ITERION_GATE_SWEEP_LOOKBACK": "2h", "ITERION_GATE_SWEEP_DEEP_EVERY": "12"},
			gateSweepSettings{interval: 5 * time.Minute, lookback: 2 * time.Hour, deepEvery: 12}, 0},
		{"unparsable values", map[string]string{"ITERION_GATE_SWEEP_INTERVAL": "soon", "ITERION_GATE_SWEEP_DEEP_EVERY": "0"}, defaultGateSweepSettings(), 2},
		{"a lookback that opens a gap", map[string]string{"ITERION_GATE_SWEEP_LOOKBACK": "2m"}, defaultGateSweepSettings(), 1},
		{"an interval no lookback covers", map[string]string{"ITERION_GATE_SWEEP_INTERVAL": "2h"}, defaultGateSweepSettings(), 1},
		{"a lookback exactly interval plus grace", map[string]string{"ITERION_GATE_SWEEP_LOOKBACK": "4m"}, defaultGateSweepSettings(), 1},
		{"non-positive durations", map[string]string{"ITERION_GATE_SWEEP_INTERVAL": "0s", "ITERION_GATE_SWEEP_LOOKBACK": "-1m"}, defaultGateSweepSettings(), 2},
		{"an interval under the floor", map[string]string{"ITERION_GATE_SWEEP_INTERVAL": "500ms"}, defaultGateSweepSettings(), 1},
		{"a lookback reaching the horizon", map[string]string{"ITERION_GATE_SWEEP_LOOKBACK": "192h"}, defaultGateSweepSettings(), 1},
		{"deep passes too sparse", map[string]string{"ITERION_GATE_SWEEP_INTERVAL": "10m", "ITERION_GATE_SWEEP_LOOKBACK": "11h", "ITERION_GATE_SWEEP_DEEP_EVERY": "1008"},
			gateSweepSettings{interval: 10 * time.Minute, lookback: 11 * time.Hour, deepEvery: gateDeepSweepEvery}, 1},
		{"a deep cadence too sparse at any count", map[string]string{"ITERION_GATE_SWEEP_INTERVAL": "4h", "ITERION_GATE_SWEEP_LOOKBACK": "5h"},
			gateSweepSettings{interval: gateSweepInterval, lookback: 5 * time.Hour, deepEvery: gateDeepSweepEvery}, 1},
		{"deep passes exactly half the horizon apart", map[string]string{"ITERION_GATE_SWEEP_INTERVAL": "1h", "ITERION_GATE_SWEEP_LOOKBACK": "2h", "ITERION_GATE_SWEEP_DEEP_EVERY": "96"},
			gateSweepSettings{interval: time.Hour, lookback: 2 * time.Hour, deepEvery: gateDeepSweepEvery}, 1},
		{"a pass count whose product with the interval overflows", map[string]string{"ITERION_GATE_SWEEP_DEEP_EVERY": "200000000"}, defaultGateSweepSettings(), 1},
		{"an interval of half the horizon or more", map[string]string{"ITERION_GATE_SWEEP_INTERVAL": "2562047h45m"}, defaultGateSweepSettings(), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var warns []string
			got := gateSweepSettingsFromEnv(func(k string) string { return tc.env[k] }, func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) })
			if got != tc.want {
				t.Errorf("settings = %+v, want %+v", got, tc.want)
			}
			if len(warns) != tc.wantWarns {
				t.Errorf("%d warnings, want %d: %q", len(warns), tc.wantWarns, warns)
			}
			for _, w := range warns {
				if !strings.Contains(w, "ITERION_GATE_SWEEP_") {
					t.Errorf("warning %q does not name the variable it is about", w)
				}
			}
		})
	}
}

// The configured cadence is the one the sweep runs, and the ordinary grant
// outlives the configured lookback. Read so that a spurious re-election — a
// new term, opening deep again — cannot fail it: within any term the passes
// alternate deep/ordinary at a deep cadence of 2, so an ordinary pass is never
// followed by another; ignoring the cadence would give runs of them.
func TestGateSweep_RunsTheConfiguredCadence(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	s.gateSweep = gateSweepSettings{interval: 200 * time.Millisecond, lookback: 3 * time.Hour, deepEvery: 2}
	rec := &windowRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runElectedGateSweeper(ctx, rec)
	}()
	waitForCond(t, 10*time.Second, "six passes", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.windows) >= 6
	})
	cancel()
	<-done
	rec.mu.Lock()
	got := append([]time.Duration(nil), rec.windows...)
	rec.mu.Unlock()
	ordinary := 0
	for i, w := range got {
		switch w {
		case gateSweepHorizon:
		case 3 * time.Hour:
			ordinary++
			if i > 0 && got[i-1] == 3*time.Hour {
				t.Fatalf("pass windows = %v: two ordinary passes in a row at a deep cadence of 2", got)
			}
		default:
			t.Fatalf("pass windows = %v: %s is neither the horizon nor the configured lookback", got, w)
		}
	}
	if ordinary == 0 {
		t.Fatalf("pass windows = %v: no pass reached the configured 3h lookback", got)
	}
	if g := s.postRunGrace(); g != 3*time.Hour+30*time.Minute {
		t.Errorf("post-run grace = %s, want the configured lookback plus 30m", g)
	}
}

// A term's first pass runs at once: with an interval an operator stretched to
// spare the forge, waiting one before the first pass would make every
// hand-over a blind window that long.
func TestGateSweeper_ATermSweepsAtOnce(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	s.gateSweep = gateSweepSettings{interval: time.Hour, lookback: 2 * time.Hour, deepEvery: 2}
	rec := &windowRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runElectedGateSweeper(ctx, rec)
	}()
	waitForCond(t, 10*time.Second, "the term's first pass", func() bool { _, ok := rec.first(); return ok })
	cancel()
	<-done
	if w, _ := rec.first(); w != gateSweepHorizon {
		t.Errorf("the term's first pass reached %s, want the horizon", w)
	}
}

// A closed pull request reopened on the same head launches no fresh review —
// the reopen's delivery shares the original launch's per-head key — so the
// dead run's own claim is left on the head. The closed settlement is
// re-checked, and the re-check is what repairs it.
func TestGateReconcile_AReopenedPullRequestIsRepairedAtTheRecheck(t *testing.T) {
	gc := &readCountingGateClient{listingGateClient: &listingGateClient{
		fakeGateClient: fakeGateClient{headSHA: "deadbeef", state: "closed"},
		statuses: []forge.CommitStatus{{
			Context: "iterion/review", State: forge.CommitStatePending, Description: gateInFlightDescription,
			TargetURL: gateRunTargetFor("https://iterion.test", "run-gating").url,
		}},
	}}
	s, run := settledFixture(t, gatingInputs(), gc)
	run.Status, run.EndReason = store.RunStatusCancelled, store.RunEndReasonPRClosed
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	run, _ = s.cfg.Store.LoadRun(context.Background(), run.ID)
	clock := time.Now()
	marks := newMemoryGateSettleStore(nil)
	marks.now = func() time.Time { return clock }
	s.gateSettles = marks
	lister := &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: run.ID, UpdatedAt: run.UpdatedAt}}}

	if err := s.reconcileGateForRun(context.Background(), terminalEvent(run.ID)); err != nil {
		t.Fatal(err)
	}
	gc.state = "" // reopened, same head: nothing new launched
	s.sweepGates(context.Background(), lister, time.Now().UTC(), gateSweepLookback, time.Time{})
	if gc.setCalls != 0 {
		t.Fatalf("the reopen was repaired before the re-check (%d writes) — the settlement was not read", gc.setCalls)
	}
	clock = clock.Add(gateSettleRecheck + time.Minute)
	s.sweepGates(context.Background(), lister, time.Now().UTC(), gateSweepLookback, time.Time{})
	if gc.setCalls != 1 || gc.last.State != forge.CommitStateFailure {
		t.Fatalf("after the re-check the dead run's claim was not answered (%d writes, last %+v) — the reopened pull request waits forever", gc.setCalls, gc.last)
	}
}

// The record settles a run only for the verdict it OWED: its reviewed head,
// its pinned check, on a grant no other run publishes with. A verdict recorded
// for another head (an unpinned publish that landed on a newer one), another
// check, or on a shared grant (another run's verdict, posted before this run
// claimed the head) leaves the run's own claim unanswered — the reconciler
// reads the forge and repairs it.
func TestGateReconcile_ARecordForAnotherHeadOrCheckSettlesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		v      *gateVerdict
		shared bool
	}{
		{name: "another head", v: &gateVerdict{SHA: "0ther5ha", Context: "iterion/review", State: "success"}},
		{name: "another check", v: &gateVerdict{SHA: "deadbeef", Context: "revi/review", State: "success"}},
		{name: "a shared grant", v: &gateVerdict{SHA: "deadbeef", Context: "iterion/review", State: "success"}, shared: true},
	} {
		name, v := tc.name, tc.v
		t.Run(name, func(t *testing.T) {
			gc := &readCountingGateClient{listingGateClient: &listingGateClient{
				fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
				statuses: []forge.CommitStatus{{
					Context: "iterion/review", State: forge.CommitStatePending, Description: gateInFlightDescription,
					TargetURL: gateRunTargetFor("https://iterion.test", "run-gating").url,
				}},
			}}
			s, run := settledFixture(t, gatingInputs(), gc)
			if ok, err := s.forgePublishTokens.update("tok-gate", func(g *ForgePublishGrant) { g.Verdict, g.Shared = v, tc.shared }); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if err := s.reconcileGateForRun(context.Background(), terminalEvent(run.ID)); err != nil {
				t.Fatal(err)
			}
			if gc.gets == 0 || gc.setCalls != 1 {
				t.Errorf("%d reads, %d writes: the run's own claim was left unanswered on a record that was not its verdict", gc.gets, gc.setCalls)
			}
		})
	}
}

// A reversible mark lapses before the run leaves the window, so its re-read
// still happens inside it; too close to the edge, no mark is written.
func TestSettleGateRun_AReversibleMarkEndsInsideTheWindow(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	marks := newMemoryGateSettleStore(nil)
	marks.now = func() time.Time { return now }
	s := &Server{gateSettles: marks, gateClock: func() time.Time { return now }, gateSweep: defaultGateSweepSettings()}
	deepInterval := time.Duration(gateDeepSweepEvery) * gateSweepInterval
	for _, tc := range []struct {
		age     time.Duration
		wantTTL time.Duration // 0: no mark
	}{
		{age: time.Hour, wantTTL: gateSettleRecheck},
		{age: gateSweepHorizon - 3*time.Hour, wantTTL: 3*time.Hour - deepInterval},
		{age: gateSweepHorizon - deepInterval/2, wantTTL: 0},
	} {
		run := &store.Run{ID: fmt.Sprintf("run-%s", tc.age), UpdatedAt: now.Add(-tc.age)}
		s.settleGateRun(run, gateSettledClosed, "")
		mark, ok := marks.marks[run.ID]
		switch {
		case tc.wantTTL == 0 && ok:
			t.Errorf("age %s: a mark was written %s before the window ends, past its last deep pass", tc.age, gateSweepHorizon-tc.age)
		case tc.wantTTL != 0 && (!ok || mark.expires.Sub(now) != tc.wantTTL):
			t.Errorf("age %s: mark present=%v lasting %s, want %s", tc.age, ok, mark.expires.Sub(now), tc.wantTTL)
		}
	}
}

// A full in-memory store says so once per saturation episode, not once per
// slot that frees up and fills again — and a new episode, once the store has
// drained below 90 %, is reported again.
func TestMemoryGateSettleStore_SaturationIsReportedOncePerEpisode(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	out := &lockedBuffer{}
	m := newMemoryGateSettleStore(iterlog.New(iterlog.LevelWarn, out))
	m.now = func() time.Time { return now }
	for i := 0; i < gateSettleMaxMarks; i++ {
		ttl := 48 * time.Hour
		if i%100 < 5 {
			ttl = time.Hour // 5 % expire early
		}
		m.settle(fmt.Sprintf("run-%d", i), gateSettlement{Reason: gateSettledMerged}, ttl)
	}
	for i := 0; i < 3; i++ {
		m.settle(fmt.Sprintf("extra-%d", i), gateSettlement{Reason: gateSettledMerged}, time.Hour)
	}
	now = now.Add(2 * time.Hour) // 5 % expired: a slot frees, the store stays above 90 %
	for i := 0; i < 20; i++ {
		m.settle(fmt.Sprintf("later-%d", i), gateSettlement{Reason: gateSettledMerged}, time.Hour)
	}
	if n := strings.Count(out.String(), "settle marks are full"); n != 1 {
		t.Errorf("saturation reported %d times in one episode, want once", n)
	}

	now = now.Add(49 * time.Hour) // every mark expired: the next write at full drains the store
	for i := 0; i <= gateSettleMaxMarks; i++ {
		m.settle(fmt.Sprintf("again-%d", i), gateSettlement{Reason: gateSettledMerged}, time.Hour)
	}
	if n := strings.Count(out.String(), "settle marks are full"); n != 2 {
		t.Errorf("saturation reported %d times over two episodes, want twice", n)
	}
}

// The configured cadence is read by New, from the environment.
func TestNew_ReadsTheSweepCadenceFromTheEnvironment(t *testing.T) {
	t.Setenv("ITERION_GATE_SWEEP_INTERVAL", "5m")
	t.Setenv("ITERION_GATE_SWEEP_LOOKBACK", "2h")
	t.Setenv("ITERION_GATE_SWEEP_DEEP_EVERY", "12")
	s := New(Config{}, iterlog.New(iterlog.LevelError, nil))
	if want := (gateSweepSettings{interval: 5 * time.Minute, lookback: 2 * time.Hour, deepEvery: 12}); s.gateSweep != want {
		t.Errorf("New read %+v, want %+v", s.gateSweep, want)
	}
}

// The last-pass warning is measured in the configured deep intervals: with
// deep passes 20 minutes apart, the last two of them come 40 minutes before
// the horizon — not the default cadence's hour.
func TestGateSweepIsLastPass_FollowsTheConfiguredCadence(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	s := &Server{gateClock: func() time.Time { return now }, gateSweep: gateSweepSettings{interval: 2 * time.Minute, lookback: time.Hour, deepEvery: 10}}
	if s.gateSweepIsLastPass(&store.Run{UpdatedAt: now.Add(-(gateSweepHorizon - 50*time.Minute))}) {
		t.Error("50 minutes before the horizon is not among the last two deep passes at a 20-minute deep cadence")
	}
	if !s.gateSweepIsLastPass(&store.Run{UpdatedAt: now.Add(-(gateSweepHorizon - 30*time.Minute))}) {
		t.Error("30 minutes before the horizon is among the last two deep passes at a 20-minute deep cadence")
	}
}

// A duration that is not positive is named as such — not reported through
// whichever bound it happens to break downstream, which would tell the
// operator about a gap instead of their typo.
func TestGateSweepSettingsFromEnv_NamesANonPositiveDuration(t *testing.T) {
	var warns []string
	env := map[string]string{"ITERION_GATE_SWEEP_INTERVAL": "0s", "ITERION_GATE_SWEEP_LOOKBACK": "-1m"}
	gateSweepSettingsFromEnv(func(k string) string { return env[k] }, func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) })
	if len(warns) != 2 {
		t.Fatalf("%d warnings, want 2: %q", len(warns), warns)
	}
	for _, w := range warns {
		if !strings.Contains(w, "is not a positive duration") {
			t.Errorf("warning %q does not name the non-positive value", w)
		}
	}
}

// pagedWindowLister pages runs newest-first with ListNotifiableRuns' own
// filter: a terminal run since `since`, a run waiting on a human however old,
// both before `before`. It records the runs deep listings returned — a deep
// listing is one whose window reaches past the lookback — counts both kinds,
// and fails the deep listings at or past failAt when it is set.
type pagedWindowLister struct {
	mu        sync.Mutex
	rows      []mongostore.NotifiableRunRef // newest first
	lookback  time.Duration
	failAt    time.Time
	failCalls map[int]bool // deep listings (1-based) that fail once
	deepSeen  map[string]bool
	fast      int
	deepCalls int
	deepFails int
}

func (l *pagedWindowLister) ListNotifiableRuns(_ context.Context, since, before time.Time, limit int) ([]mongostore.NotifiableRunRef, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	deep := time.Since(since) > l.lookback+time.Hour
	if deep {
		l.deepCalls++
		if l.failCalls[l.deepCalls] || (!l.failAt.IsZero() && !before.After(l.failAt)) {
			l.deepFails++
			return nil, errors.New("mongo: cannot decode document")
		}
	}
	var out []mongostore.NotifiableRunRef
	for _, r := range l.rows {
		paused := r.Status == string(store.RunStatusPausedWaitingHuman)
		if !r.UpdatedAt.Before(before) || (!paused && r.UpdatedAt.Before(since)) {
			continue
		}
		out = append(out, r)
		if deep {
			l.deepSeen[r.ID] = true
		}
		if len(out) == limit {
			break
		}
	}
	if !deep {
		l.fast++
	}
	return out, nil
}

// A deep traversal the page budget cuts short continues at the very next
// pass, beside that pass's fast one. Waiting for the next deep pass instead
// would let a traversal outlast the runs it walks once deep passes are spaced
// out — a thousand passes apart in the first row; in the second every pass is
// deep, and the fast pass must still run beside each continuation.
func TestGateSweep_APagedDeepTraversalContinuesAtTheNextPass(t *testing.T) {
	for _, deepEvery := range []int{1000, 1} {
		t.Run(fmt.Sprintf("deep every %d", deepEvery), func(t *testing.T) {
			testAPagedDeepTraversalContinues(t, deepEvery)
		})
	}
}

func testAPagedDeepTraversalContinues(t *testing.T, deepEvery int) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	s.gateSweep = gateSweepSettings{interval: time.Minute, lookback: time.Hour, deepEvery: deepEvery}
	s.gateSweepTick = 50 * time.Millisecond
	now := time.Now().UTC()
	const n = 2*gateSweepMaxPages*gateSweepBatch + 500 // three passes' page budget
	span := gateSweepHorizon - 3*time.Hour
	l := &pagedWindowLister{lookback: time.Hour, deepSeen: map[string]bool{}}
	for i := 0; i < n; i++ {
		l.rows = append(l.rows, mongostore.NotifiableRunRef{
			ID:        fmt.Sprintf("run-%05d", i),
			Status:    string(store.RunStatusFinished),
			UpdatedAt: now.Add(-gateSweepGrace - time.Hour - time.Duration(i)*(span/n)),
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runGateSweeper(ctx, l)
	}()
	waitForCond(t, 20*time.Second, "a deep traversal of the whole window", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.deepSeen) == n
	})
	cancel()
	<-done
	l.mu.Lock()
	fast := l.fast
	l.mu.Unlock()
	if fast < 2 {
		t.Errorf("%d ordinary listings while the traversal continued, want one beside each continuation — a fresh death would wait for the traversal to end", fast)
	}
}

// The marks a page reads in one round-trip are mapped back to THEIR runs: on
// the production backend, a settled run second in its page is not offered.
func TestGateSweep_AValkeyMarkSettlesItsOwnRun(t *testing.T) {
	gc := &readCountingGateClient{listingGateClient: &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}}
	s, run := settledFixture(t, gatingInputs(), gc)
	_, rdb := newTestRedis(t)
	s.gateSettles = newValkeyGateSettleStore(rdb, nil)
	plain, err := s.cfg.Store.CreateRun(context.Background(), "run-plain", "brancher", map[string]any{"pr_url": "https://github.com/o/r/pull/7"})
	if err != nil {
		t.Fatal(err)
	}
	s.gateSettles.settle(run.ID, gateSettlement{Reason: gateSettledMerged, Episode: run.UpdatedAt.UnixMilli()}, time.Hour)
	lister := &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{
		{ID: plain.ID, UpdatedAt: plain.UpdatedAt},
		{ID: run.ID, UpdatedAt: run.UpdatedAt},
	}}
	for i := 0; i < 3; i++ {
		s.sweepGates(context.Background(), lister, time.Now().UTC(), gateSweepLookback, time.Time{})
	}
	if gc.gets != 0 {
		t.Errorf("a run settled in Valkey, second in its page, was read %d times over three passes, want 0", gc.gets)
	}
}

// recordingSettleStore counts the marks it was asked to write.
type recordingSettleStore struct {
	gateSettleStore
	settles int
}

func (r *recordingSettleStore) settle(id string, g gateSettlement, ttl time.Duration) {
	r.settles++
	r.gateSettleStore.settle(id, g, ttl)
}

// A run already past the horizon is offered by no pass, so it gets no mark —
// and a store asked for a mark with no lifetime writes none: Valkey would keep
// it forever. Two guards, each witnessed alone.
func TestSettleGateRun_PastTheHorizonWritesNoMark(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	rec := &recordingSettleStore{gateSettleStore: newMemoryGateSettleStore(nil)}
	s := &Server{gateSettles: rec, gateClock: func() time.Time { return now }, gateSweep: defaultGateSweepSettings()}
	for _, reason := range []string{gateSettledMerged, gateSettledClosed} {
		s.settleGateRun(&store.Run{ID: "run-old", UpdatedAt: now.Add(-gateSweepHorizon - 2*time.Hour)}, reason, "")
	}
	if rec.settles != 0 {
		t.Errorf("%d marks written for a run past the horizon, want none", rec.settles)
	}
}

func TestGateSettleStores_RefuseAMarkWithNoLifetime(t *testing.T) {
	for _, backend := range []string{"memory", "valkey"} {
		t.Run(backend, func(t *testing.T) {
			out := &lockedBuffer{}
			logger := iterlog.New(iterlog.LevelWarn, out)
			var st gateSettleStore
			var held func() int
			if backend == "memory" {
				m := newMemoryGateSettleStore(logger)
				st, held = m, func() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.marks) }
			} else {
				mr, rdb := newTestRedis(t)
				st, held = newValkeyGateSettleStore(rdb, logger), func() int { return len(mr.Keys()) }
			}
			for _, ttl := range []time.Duration{0, -time.Hour} {
				st.settle("run-a", gateSettlement{Reason: gateSettledMerged, Episode: 1}, ttl)
			}
			if n := held(); n != 0 {
				t.Errorf("%d marks with no lifetime held by the store, want none", n)
			}
			if n := strings.Count(out.String(), "with no lifetime"); n != 2 {
				t.Errorf("%d refusals reported, want 2; log:\n%s", n, out.String())
			}
		})
	}
}

// The sweep's lease is paced by the interval, capped at the default one: a
// long interval must not stretch the TTL a dead holder is outlived by, nor the
// retry a successor campaigns at.
func TestGateSweepLeaseSpec_IsPacedByTheIntervalCappedAtTheDefault(t *testing.T) {
	s := &Server{replicaID: "r1", gateSweep: gateSweepSettings{interval: time.Hour, lookback: 2 * time.Hour, deepEvery: 2}}
	if spec := s.gateSweepLeaseSpec(); spec.TTL != 3*time.Minute || spec.Retry != time.Minute || spec.Name != leaseMergeGateSweeper || spec.Owner != "r1" {
		t.Errorf("under a 1h interval the lease spec is TTL %s, retry %s (%s/%s), want 3m and 1m", spec.TTL, spec.Retry, spec.Name, spec.Owner)
	}
	s.gateSweepTick = 100 * time.Millisecond
	if spec := s.gateSweepLeaseSpec(); spec.TTL != 300*time.Millisecond || spec.Retry != 100*time.Millisecond {
		t.Errorf("under a 100ms interval the lease spec is TTL %s, retry %s, want 300ms and 100ms", spec.TTL, spec.Retry)
	}
}

// sweepAWhile runs the sweeper on l until it has made `passes` ordinary
// listings, then stops it.
func sweepAWhile(t *testing.T, s *Server, l *pagedWindowLister, passes int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runGateSweeper(ctx, l)
	}()
	waitForCond(t, 30*time.Second, fmt.Sprintf("%d ordinary listings", passes), func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.fast >= passes
	})
	cancel()
	<-done
}

// windowRows is n terminal runs spread over the window, newest first, then
// `paused` runs waiting on a human since before the horizon — the rows
// ListNotifiableRuns returns past `since`.
func windowRows(now time.Time, n, paused int) []mongostore.NotifiableRunRef {
	var rows []mongostore.NotifiableRunRef
	span := gateSweepHorizon - 3*time.Hour
	for i := 0; i < n; i++ {
		rows = append(rows, mongostore.NotifiableRunRef{
			ID: fmt.Sprintf("run-%05d", i), Status: string(store.RunStatusFinished),
			UpdatedAt: now.Add(-gateSweepGrace - time.Hour - time.Duration(i)*(span/time.Duration(n))),
		})
	}
	for i := 0; i < paused; i++ {
		rows = append(rows, mongostore.NotifiableRunRef{
			ID: fmt.Sprintf("paused-%05d", i), Status: string(store.RunStatusPausedWaitingHuman),
			UpdatedAt: now.Add(-gateSweepHorizon - time.Hour - time.Duration(i)*time.Minute),
		})
	}
	return rows
}

// Runs waiting on a human come back however old they are, so a deep
// traversal reaches past the window's far edge on them. That is the window
// exhausted — not a cursor to resume from every pass.
func TestGateSweep_OldPausedRunsDoNotKeepADeepTraversalAlive(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	s.gateSweep = gateSweepSettings{interval: time.Minute, lookback: time.Hour, deepEvery: 1000}
	s.gateSweepTick = 20 * time.Millisecond
	l := &pagedWindowLister{rows: windowRows(time.Now().UTC(), 1900, 300), lookback: time.Hour, deepSeen: map[string]bool{}}
	sweepAWhile(t, s, l, 20)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.deepCalls > gateSweepMaxPages {
		t.Errorf("%d deep listings over 20 passes, want at most one traversal's %d — old paused runs kept the traversal resuming", l.deepCalls, gateSweepMaxPages)
	}
}

// A deep page that keeps failing parks the traversal until the next deep
// pass, instead of failing again at every tick.
func TestGateSweep_AStuckDeepPageWaitsForTheNextDeepPass(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	s.gateSweep = gateSweepSettings{interval: time.Minute, lookback: time.Hour, deepEvery: 1000}
	s.gateSweepTick = 20 * time.Millisecond
	rows := windowRows(time.Now().UTC(), 2*gateSweepMaxPages*gateSweepBatch+500, 0)
	l := &pagedWindowLister{rows: rows, lookback: time.Hour, deepSeen: map[string]bool{},
		failAt: rows[gateSweepMaxPages*gateSweepBatch-1].UpdatedAt}
	sweepAWhile(t, s, l, 15)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.deepFails > 2 {
		t.Errorf("a failing deep page was retried %d times over 15 passes, want it parked until the next deep pass", l.deepFails)
	}
}

// Each fallback blames the variable it resets: an operator who set only the
// interval is told about the interval, not about a lookback or deep cadence
// they never set and that stays as it was.
func TestGateSweepSettingsFromEnv_BlamesTheVariableTheOperatorSet(t *testing.T) {
	for _, env := range []map[string]string{
		{"ITERION_GATE_SWEEP_INTERVAL": "2h"},
		{"ITERION_GATE_SWEEP_INTERVAL": "4h", "ITERION_GATE_SWEEP_LOOKBACK": "5h"},
	} {
		var warns []string
		gateSweepSettingsFromEnv(func(k string) string { return env[k] }, func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) })
		if len(warns) != 1 || !strings.HasPrefix(warns[0], "merge-gate sweeper: ITERION_GATE_SWEEP_INTERVAL=") {
			t.Errorf("env %v: warnings %q, want one, about the interval", env, warns)
		}
	}
}

// One failed deep page is a blip: the next pass retries it and the traversal
// goes on, instead of waiting a deep interval for its next chance.
func TestGateSweep_OneTransientDeepFailureDoesNotParkTheTraversal(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	s.gateSweep = gateSweepSettings{interval: time.Minute, lookback: time.Hour, deepEvery: 1000}
	s.gateSweepTick = 20 * time.Millisecond
	n := 3*gateSweepMaxPages*gateSweepBatch + 500
	l := &pagedWindowLister{rows: windowRows(time.Now().UTC(), n, 0), lookback: time.Hour, deepSeen: map[string]bool{},
		failCalls: map[int]bool{gateSweepMaxPages + 1: true}} // the first continuation's first page
	sweepAWhile(t, s, l, 12)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.deepSeen) != n {
		t.Errorf("one transient failure left the deep traversal at %d of %d runs for 12 passes — parked until the next deep pass", len(l.deepSeen), n)
	}
}

// A traversal parked on a page that failed twice resumes at the next deep
// pass — and once that page moves, continues at every pass again.
func TestGateSweep_AParkedTraversalResumesEveryPassOnceItMoves(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	s.gateSweep = gateSweepSettings{interval: time.Minute, lookback: time.Hour, deepEvery: 5}
	s.gateSweepTick = 20 * time.Millisecond
	n := 5*gateSweepMaxPages*gateSweepBatch + 500
	l := &pagedWindowLister{rows: windowRows(time.Now().UTC(), n, 0), lookback: time.Hour, deepSeen: map[string]bool{},
		failCalls: map[int]bool{gateSweepMaxPages + 1: true, gateSweepMaxPages + 2: true}}
	sweepAWhile(t, s, l, 14)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.deepSeen) != n {
		t.Errorf("the traversal unparked at the deep pass but covered %d of %d runs by pass 14 — it went on waiting for deep passes", len(l.deepSeen), n)
	}
}
