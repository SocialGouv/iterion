package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// finishRun marks the fixture's run ended, as the event path and the sweep
// only ever see it.
func finishRun(t *testing.T, s *Server, runID string) *store.Run {
	t.Helper()
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFinished
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	run, err = s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// withAutofix gives the fixture's repo an integration, with the auto-fix lane
// on or off.
func withAutofix(t *testing.T, s *Server, on bool) {
	t.Helper()
	ints := forge.NewMemoryRepoIntegrationStore()
	if err := ints.Create(context.Background(), forge.RepoIntegration{
		ID: "i1", TenantID: "team1", ConnectionID: "conn1", RepoFullName: "o/r", AutoFixOnGateFailure: on,
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeIntegrations = ints
}

// readCountingGateClient counts the pull-request reads a reconcile makes.
type readCountingGateClient struct {
	*listingGateClient
	gets int
}

func (c *readCountingGateClient) GetPullRequest(ctx context.Context, repo string, n int) (forge.PullRef, error) {
	c.gets++
	return c.listingGateClient.GetPullRequest(ctx, repo, n)
}

// A gating run keeps its publish grant for the whole sweep horizon, because a
// repair may still owe a verdict on its behalf. That is eight days of a
// forge-write bearer held by an agent that reads untrusted pull-request content
// and can post a review AND a commit status — including a green one on the
// required check.
//
// So the grant is cut back to the ordinary post-run grace the moment it is
// PROVABLY without a reader: when the verdict this run owed is posted — which
// the forge's status cannot say (its target URL is the review, where reviewers
// land), so the publish endpoint that posts it records it on the grant. The
// cases pin what that record allows, driven through the real producer:
//
//   - its own green verdict: settled with no forge read, grant cut back;
//   - its own red verdict on a repo with the auto-fix lane on: settled, grant
//     KEPT — the lane reads it to launch a fixer;
//   - its own red verdict with the lane off: nothing will read it, cut back;
//   - its own green verdict on a SHARED grant (a second run publishes with
//     it): read from the forge — the record may be the other run's — and
//     kept, since the other run may still be publishing;
//   - another run's verdict, only on the forge: settled after reading it, grant
//     kept — a repo's gate context is shared between bots, and that verdict
//     says nothing about what this run's grant is still for.
func TestTheGrantIsCutBackOnlyAfterThisRunsOwnGreenVerdict(t *testing.T) {
	green := `{"enabled":true,"context":"iterion/review","blocking_count":0,"threshold":"high","total_findings":0,"audited_sha":"deadbeef"}`
	red := `{"enabled":true,"context":"iterion/review","blocking_count":2,"threshold":"high","total_findings":2,"audited_sha":"deadbeef"}`
	anotherRuns := forge.CommitStatus{
		Context:     "iterion/review",
		State:       forge.CommitStateSuccess,
		Description: "no blocking findings (≥high); 0 total",
		TargetURL:   "https://github.com/o/r/pull/42#pullrequestreview-9",
	}
	for _, tc := range []struct {
		name           string
		publish        string // the gate this run's own publish posts; empty: it posts none
		onForge        []forge.CommitStatus
		shared         bool
		autofix        bool
		wantCutBack    bool
		wantForgeReads bool
		wantReason     string
	}{
		{name: "its own green verdict", publish: green, wantCutBack: true, wantReason: gateSettledVerdictSuccess},
		{name: "its own green verdict, auto-fix on", publish: green, autofix: true, wantCutBack: true, wantReason: gateSettledVerdictSuccess},
		{name: "its own red verdict, auto-fix on", publish: red, autofix: true, wantReason: gateSettledVerdictFailure},
		{name: "its own red verdict, auto-fix off", publish: red, wantCutBack: true, wantReason: gateSettledVerdictFailure},
		{name: "its own green verdict on a shared grant", publish: green, shared: true, wantForgeReads: true, wantReason: gateSettledVerdictSuccess},
		{name: "another run's verdict, only on the forge", onForge: []forge.CommitStatus{anotherRuns}, wantForgeReads: true, wantReason: gateSettledVerdictSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gc := &readCountingGateClient{listingGateClient: &listingGateClient{
				fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
				statuses:       tc.onForge,
			}}
			s, runID := gateReconcileFixture(t, gatingInputs(), gc)
			finishRun(t, s, runID)
			withAutofix(t, s, tc.autofix)
			if tc.shared {
				if ok, err := s.forgePublishTokens.update("tok-gate", func(g *ForgePublishGrant) { g.Shared = true }); err != nil || !ok {
					t.Fatalf("marking the grant shared: %v, %v", ok, err)
				}
			}
			if tc.publish != "" {
				w := httptest.NewRecorder()
				s.handleForgePublishReview(w, publishReq("tok-gate", publishBodyWithGate(tc.publish)))
				if w.Code != http.StatusOK || gc.setCalls != 1 {
					t.Fatalf("the run's publish did not post its verdict: HTTP %d, %d status writes: %s", w.Code, gc.setCalls, w.Body.String())
				}
				if g, _ := s.forgePublishTokens.lookup("tok-gate"); g.Verdict == nil {
					t.Fatal("the publish endpoint posted a verdict and recorded none on the grant")
				}
				gc.statuses = append(gc.statuses, gc.posted...) // the forge shows what was posted
			}

			gc.gets, gc.setCalls = 0, 0
			if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if gc.setCalls != 0 {
				t.Fatalf("posted %d statuses over a real verdict, want 0", gc.setCalls)
			}
			if got := gc.gets > 0; got != tc.wantForgeReads {
				t.Errorf("the reconcile read the pull request %d times, want reads=%v — its own recorded verdict needs none", gc.gets, tc.wantForgeReads)
			}

			grant, ok := s.forgePublishTokens.lookup("tok-gate")
			if !ok {
				t.Fatal("the grant is gone: a verdict shortens it, never revokes it outright")
			}
			cutBack := time.Until(grant.ExpiresAt) <= s.postRunGrace()+time.Minute
			if cutBack && time.Until(grant.ExpiresAt) < s.postRunGrace()-time.Minute {
				t.Errorf("the grant was cut to %s, under the %s post-run grace the event path and the fast sweep still need", time.Until(grant.ExpiresAt).Round(time.Minute), s.postRunGrace())
			}
			switch {
			case tc.wantCutBack && !cutBack:
				t.Errorf("the grant still lives %s after the run's own green verdict — a forge-write bearer kept for days past its use", time.Until(grant.ExpiresAt).Round(time.Minute))
			case !tc.wantCutBack && cutBack:
				t.Errorf("the grant was cut back to %s, but its reader is still ahead (the auto-fix lane, or another run on a shared grant)", time.Until(grant.ExpiresAt).Round(time.Minute))
			}

			run, err := s.cfg.Store.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			marks, err := s.gateSettles.settled(context.Background(), []string{runID})
			if err != nil {
				t.Fatal(err)
			}
			if m, ok := marks[runID]; !ok || m.Reason != tc.wantReason || !m.appliesTo(run.UpdatedAt) {
				t.Errorf("settle mark = %+v (present=%v), want reason %q for the run's current episode", m, ok, tc.wantReason)
			}
		})
	}
}

// A share and a cut-back decide on the SAME grant record, so a launch pinning
// the token and the verdict that would cut the grant back cannot both win:
// shared first, the grant is kept — flags AND expiry; cut back first, the
// share is refused and the pinned launch with it.
func TestTheShareAndTheCutBackAreExclusive(t *testing.T) {
	for _, backend := range []string{"memory", "valkey"} {
		t.Run(backend, func(t *testing.T) {
			build := func(t *testing.T) (*Server, *store.Run, func() time.Duration) {
				gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
				s, runID := gateReconcileFixture(t, gatingInputs(), gc)
				lifetime := func() time.Duration { return time.Until(mustGrant(t, s, "tok-gate").ExpiresAt) }
				if backend == "valkey" {
					mr, rdb := newTestRedis(t)
					st := newValkeyForgePublishTokenStore(rdb, nil)
					if err := st.Register("tok-gate", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"}); err != nil {
						t.Fatal(err)
					}
					s.forgePublishTokens = st
					lifetime = func() time.Duration { return mr.TTL(forgePublishTokenKeyPrefix + "tok-gate") }
				}
				return s, finishRun(t, s, runID), lifetime
			}
			t.Run("cut back first", func(t *testing.T) {
				s, run, lifetime := build(t)
				s.cutBackGrant(run, "tok-gate")
				if left := lifetime(); left > s.postRunGrace()+time.Minute {
					t.Fatalf("the cut-back left the grant %s, want the %s post-run grace", left, s.postRunGrace())
				}
				cutBack, found, err := s.shareGrant("tok-gate")
				if err != nil || !found || !cutBack {
					t.Fatalf("shareGrant after a cut-back = cutBack %v, found %v, %v; want it refused", cutBack, found, err)
				}
				if g, _ := s.forgePublishTokens.lookup("tok-gate"); g.Shared {
					t.Error("a cut-back grant was marked shared")
				}
				s.cfg.PublicURL = "https://iterion.test"
				_, err = s.injectForgePublishVars(context.Background(), "team1", "conn1", "review-pr",
					map[string]string{"pr_url": "https://github.com/o/r/pull/42", forgePublishVarToken: "tok-gate"}, nil, store.RunTrustDefault)
				if !errors.Is(err, errForgePublishGrantUnavailable) {
					t.Errorf("a launch pinning a cut-back grant was accepted (err=%v) — its verdict would be unpostable after the grace", err)
				}
			})
			t.Run("shared first", func(t *testing.T) {
				s, run, lifetime := build(t)
				if cutBack, found, err := s.shareGrant("tok-gate"); err != nil || !found || cutBack {
					t.Fatalf("shareGrant = cutBack %v, found %v, %v", cutBack, found, err)
				}
				s.cutBackGrant(run, "tok-gate")
				g, ok := s.forgePublishTokens.lookup("tok-gate")
				if !ok || !g.Shared || g.CutBack {
					t.Errorf("after a share then a cut-back: %+v (present=%v), want shared and not cut back", g, ok)
				}
				if left := lifetime(); left < 24*time.Hour {
					t.Errorf("a shared grant was shortened to %s by a cut-back — its second run's verdict becomes unpostable", left.Round(time.Minute))
				}
			})
		})
	}
}

// A fork carries its parent's token: a second run on one grant, marked shared
// so the parent's verdict cannot cut it back under the fork.
func TestAForkSharesItsInheritedGrant(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, _ := gateReconcileFixture(t, gatingInputs(), gc)
	if _, err := s.cfg.Store.CreateRun(context.Background(), "run-fork", "review_pr", gatingInputs()); err != nil {
		t.Fatal(err)
	}
	s.shareForkGrant(context.Background(), "run-fork")
	if g, ok := s.forgePublishTokens.lookup("tok-gate"); !ok || !g.Shared {
		t.Fatalf("the fork's inherited grant = %+v (present=%v), want it shared", g, ok)
	}

	out := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelWarn, out)
	if ok, err := s.forgePublishTokens.update("tok-gate", func(g *ForgePublishGrant) { g.Shared, g.CutBack = false, true }); err != nil || !ok {
		t.Fatal(ok, err)
	}
	s.shareForkGrant(context.Background(), "run-fork")
	if !strings.Contains(out.String(), "already cut back") {
		t.Errorf("a fork of a cut-back grant was not reported; log:\n%s", out.String())
	}

	s.forgePublishTokens.Revoke("tok-gate")
	s.shareForkGrant(context.Background(), "run-fork")
	if !strings.Contains(out.String(), "expired or revoked") {
		t.Errorf("a fork of a grant that is gone was not reported — its publish is refused with no warning; log:\n%s", out.String())
	}
}

// A run that is not over — resumed after the sweep listed it — keeps its grant
// whatever its record says: it is about to publish again.
func TestARunNotOverKeepsItsGrant(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusRunning
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	run, _ = s.cfg.Store.LoadRun(context.Background(), runID)
	s.cutBackGrant(run, "tok-gate")
	if g, _ := s.forgePublishTokens.lookup("tok-gate"); time.Until(g.ExpiresAt) < 24*time.Hour {
		t.Errorf("a running run's grant was cut back to %s", time.Until(g.ExpiresAt).Round(time.Minute))
	}
}

// A gating run that names no reviewed revision can never be repaired for: its
// grant gets the ordinary post-run grace, not the gate's eight days.
func TestAnUnpinnedGatingRunGetsTheOrdinaryGrace(t *testing.T) {
	in := gatingInputs()
	delete(in, "head_sha")
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, in, gc)
	finishRun(t, s, runID)
	if err := s.expireForgePublishGrantForRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.forgePublishTokens.lookup("tok-gate"); time.Until(g.ExpiresAt) > s.postRunGrace()+time.Minute {
		t.Errorf("an unpinned gating run's grant lives %s, want the %s post-run grace", time.Until(g.ExpiresAt).Round(time.Minute), s.postRunGrace())
	}
}

// update keeps the keys this build does not know, so a newer build's field
// survives an older build's write; the keys it owns follow the struct.
func TestMergeGrantJSON_KeepsTheKeysItDoesNotKnow(t *testing.T) {
	raw := []byte(`{"team_id":"t","connection_id":"c","repo":"o/r","verdict":{"sha":"a"},"pending_verdict":{"retry_at":"soon"}}`)
	out, err := mergeGrantJSON(raw, ForgePublishGrant{TeamID: "t", ConnectionID: "c", Repo: "o/r", Shared: true})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["pending_verdict"]; !ok {
		t.Errorf("a newer build's field was erased: %s", out)
	}
	if _, ok := got["verdict"]; ok {
		t.Errorf("a field this build cleared survived: %s", out)
	}
	if string(got["shared"]) != "true" {
		t.Errorf("a field this build set is missing: %s", out)
	}
}

// forgePublishGrantKeys is the list mergeGrantJSON trusts: it must be exactly
// the struct's JSON keys, or a new field would be dropped, or kept stale.
func TestForgePublishGrantKeysAreTheStructs(t *testing.T) {
	var want []string
	rt := reflect.TypeOf(ForgePublishGrant{})
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			want = append(want, tag)
		}
	}
	got := append([]string(nil), forgePublishGrantKeys...)
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("forgePublishGrantKeys = %v, want the struct's %v", got, want)
	}
}

// unansweringIntegrations is an integration store that cannot answer.
type unansweringIntegrations struct{ forge.RepoIntegrationStore }

func (unansweringIntegrations) GetByConnRepo(context.Context, string, string, string) (forge.RepoIntegration, error) {
	return forge.RepoIntegration{}, errors.New("mongo: server selection timeout")
}

// Whether the auto-fix lane will read a red verdict's grant is the integration
// store's answer. A repo with no integration has no lane, and its grant is cut
// back; a store that cannot answer keeps it — a grant kept longer is the safe
// error, a fixer launched without one is not.
func TestARedVerdictsGrantFollowsTheLanesAnswer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ints        forge.RepoIntegrationStore
		wantCutBack bool
	}{
		{name: "the store cannot answer", ints: unansweringIntegrations{RepoIntegrationStore: forge.NewMemoryRepoIntegrationStore()}},
		{name: "no integration for the repo", ints: forge.NewMemoryRepoIntegrationStore(), wantCutBack: true},
		{name: "no integration store", wantCutBack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
			s, runID := gateReconcileFixture(t, gatingInputs(), gc)
			run := finishRun(t, s, runID)
			s.forgeIntegrations = tc.ints
			grant, _ := s.forgePublishTokens.lookup("tok-gate")
			s.settleOwnVerdict(context.Background(), run, "tok-gate", grant, &gateVerdict{SHA: "deadbeef", Context: "iterion/review", State: string(forge.CommitStateFailure)})
			g, _ := s.forgePublishTokens.lookup("tok-gate")
			if cutBack := g.CutBack && time.Until(g.ExpiresAt) <= s.postRunGrace()+time.Minute; cutBack != tc.wantCutBack {
				t.Errorf("a red verdict's grant: cut back=%v (lives %s), want %v", cutBack, time.Until(g.ExpiresAt).Round(time.Minute), tc.wantCutBack)
			}
		})
	}
}

// updateFailsOnce refuses the first grant update and lets the next through.
type updateFailsOnce struct {
	ForgePublishTokenStore
	failed bool
}

func (u *updateFailsOnce) update(token string, mutate func(*ForgePublishGrant)) (bool, error) {
	if !u.failed {
		u.failed = true
		return false, errors.New("valkey: connection reset")
	}
	return u.ForgePublishTokenStore.update(token, mutate)
}

// A verdict the grant could not record must not leave an EARLIER record
// standing: the reconciler would trust a verdict the forge no longer shows,
// settle the run on it, and cut its grant back.
func TestAVerdictThatCannotBeRecordedClearsTheEarlierRecord(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, _ := gateReconcileFixture(t, gatingInputs(), gc)
	earlier := &gateVerdict{SHA: "deadbeef", Context: "iterion/review", State: string(forge.CommitStateSuccess), At: time.Now().UTC()}
	if ok, err := s.forgePublishTokens.update("tok-gate", func(g *ForgePublishGrant) { g.Verdict = earlier }); err != nil || !ok {
		t.Fatal(ok, err)
	}
	s.forgePublishTokens = &updateFailsOnce{ForgePublishTokenStore: s.forgePublishTokens}

	red := `{"enabled":true,"context":"iterion/review","blocking_count":2,"threshold":"high","total_findings":2,"audited_sha":"deadbeef"}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok-gate", publishBodyWithGate(red)))
	if w.Code != http.StatusOK || gc.setCalls != 1 {
		t.Fatalf("the publish did not post its verdict: HTTP %d, %d status writes: %s", w.Code, gc.setCalls, w.Body.String())
	}
	if g, _ := s.forgePublishTokens.lookup("tok-gate"); g.Verdict != nil {
		t.Errorf("the grant still records %+v after a red verdict it could not record — the reconciler would settle the run green", *g.Verdict)
	}
}

// The ordinary post-run grace outlives the fast sweep's lookback, whatever an
// operator set it to: the reaper and the cut-back both give that much.
func TestTheGraceFollowsTheConfiguredLookback(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	s.gateSweep = gateSweepSettings{interval: time.Minute, lookback: 5 * time.Hour, deepEvery: 30}
	const want = 5*time.Hour + 30*time.Minute
	if got := s.postRunGrace(); got != want {
		t.Fatalf("postRunGrace = %s under a 5h lookback, want %s", got, want)
	}
	run := finishRun(t, s, runID)
	s.cutBackGrant(run, "tok-gate")
	if left := time.Until(mustGrant(t, s, "tok-gate").ExpiresAt); left < want-time.Minute || left > want+time.Minute {
		t.Errorf("the cut-back grant lives %s, want the %s grace a 5h lookback needs", left.Round(time.Minute), want)
	}

	registerPublishToken(t, s, "tok-plain", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	plain, err := s.cfg.Store.CreateRun(context.Background(), "run-plain", "brancher", map[string]any{forgePublishVarToken: "tok-plain"})
	if err != nil {
		t.Fatal(err)
	}
	finishRun(t, s, plain.ID)
	if err := s.expireForgePublishGrantForRun(context.Background(), plain.ID); err != nil {
		t.Fatal(err)
	}
	if left := time.Until(mustGrant(t, s, "tok-plain").ExpiresAt); left < want-time.Minute || left > want+time.Minute {
		t.Errorf("an ordinary run's grant lives %s after it ended, want the %s grace a 5h lookback needs", left.Round(time.Minute), want)
	}
}

func mustGrant(t *testing.T, s *Server, token string) ForgePublishGrant {
	t.Helper()
	g, ok := s.forgePublishTokens.lookup(token)
	if !ok {
		t.Fatalf("grant %s is gone", token)
	}
	return g
}

// The share is only as good as the call that makes it, so the fork endpoint
// itself is driven: its child inherits the parent's token and must find the
// grant shared.
func TestTheForkEndpointSharesTheInheritedGrant(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	registerPublishToken(t, s, "tok-gate", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r", Bot: "review-pr"})
	s.runs = newTestRunviewService(t, dir)

	ctx := context.Background()
	const parentID = "run-parent"
	if _, err := st.CreateRun(ctx, parentID, "review_pr", gatingInputs()); err != nil {
		t.Fatal(err)
	}
	parent, err := st.LoadRun(ctx, parentID)
	if err != nil {
		t.Fatal(err)
	}
	parent.Status = store.RunStatusCancelled
	parent.Checkpoint = &store.Checkpoint{NodeID: "review"}
	if err := st.SaveRun(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteTurn(ctx, &store.TurnCheckpoint{
		RunID: parentID, NodeID: "review", TurnIndex: 0, Backend: "claw", FinishReason: "tool_use",
		MessagesRef: "review/0/0.messages.json",
		Messages:    json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]`),
		WrittenAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+parentID+"/fork", strings.NewReader(`{"node_id":"review"}`))
	req.SetPathValue("id", parentID)
	w := httptest.NewRecorder()
	s.handleForkRun(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: HTTP %d: %s", w.Code, w.Body.String())
	}
	if g, ok := s.forgePublishTokens.lookup("tok-gate"); !ok || !g.Shared {
		t.Errorf("after a fork the inherited grant = %+v (present=%v), want it shared — the parent's verdict could cut it back under the fork", g, ok)
	}
}

// A verdict posted while the review itself failed is still the run's verdict
// on the forge, and is recorded like one: both of the endpoint's posting
// paths write the record.
func TestAVerdictPostedWhileTheReviewFailedIsRecorded(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, _ := gateReconcileFixture(t, gatingInputs(), gc)
	s.forgeReviewClientFor = func(context.Context, forge.Connection) (forge.ReviewClient, error) {
		return &fakeReviewClient{err: errors.New("github: 502 bad gateway")}, nil
	}
	green := `{"enabled":true,"context":"iterion/review","blocking_count":0,"threshold":"high","total_findings":0,"audited_sha":"deadbeef"}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok-gate", publishBodyWithGate(green)))
	if w.Code != http.StatusBadGateway || gc.setCalls != 1 {
		t.Fatalf("want the review to fail and the gate to post: HTTP %d, %d status writes: %s", w.Code, gc.setCalls, w.Body.String())
	}
	g := mustGrant(t, s, "tok-gate")
	if g.Verdict == nil || g.Verdict.SHA != "deadbeef" || g.Verdict.State != string(forge.CommitStateSuccess) || g.Verdict.Context != "iterion/review" {
		t.Errorf("the grant records %+v after a verdict posted on the review-failed path, want the posted verdict", g.Verdict)
	}
}

// Only a verdict that LANDED is recorded: a record of one the forge refused
// would settle the run on a check nobody can see, and cut its grant back
// before the repair that check still needs.
func TestAVerdictThatDidNotLandIsNotRecorded(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef", setErr: errors.New("github: 403 resource not accessible by integration")}}
	s, _ := gateReconcileFixture(t, gatingInputs(), gc)
	green := `{"enabled":true,"context":"iterion/review","blocking_count":0,"threshold":"high","total_findings":0,"audited_sha":"deadbeef"}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok-gate", publishBodyWithGate(green)))
	if gc.setCalls != 1 {
		t.Fatalf("want one refused status write, got %d: HTTP %d %s", gc.setCalls, w.Code, w.Body.String())
	}
	if g := mustGrant(t, s, "tok-gate"); g.Verdict != nil {
		t.Errorf("the grant records %+v for a verdict the forge refused", *g.Verdict)
	}
}

// The end of a run that leaves its grant no reader cuts it back to the
// post-run grace — the same decision a verdict makes, so a grant a second run
// shares is kept for it. A run pinning the token of a gating run, gating too
// but naming no reviewed revision (a launch outside the webhook lanes), or
// gating nothing at all, ends: the grant must outlive it for the first run.
func TestTheReaperKeepsASharedGrantForItsOtherRun(t *testing.T) {
	for name, inputs := range map[string]map[string]any{
		"a pinned gating run naming no revision": {"pr_url": "https://github.com/o/r/pull/42", "gate_context": "iterion/review", forgePublishVarToken: "tok-gate"},
		"a pinned run gating nothing":            {"pr_url": "https://github.com/o/r/pull/42", forgePublishVarToken: "tok-gate"},
	} {
		t.Run(name, func(t *testing.T) {
			gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
			s, _ := gateReconcileFixture(t, gatingInputs(), gc)
			if _, err := s.injectForgePublishVars(context.Background(), "team1", "conn1", "review-pr",
				map[string]string{"pr_url": "https://github.com/o/r/pull/42", forgePublishVarToken: "tok-gate"}, nil, store.RunTrustDefault); err != nil {
				t.Fatalf("pinned launch: %v", err)
			}
			if _, err := s.cfg.Store.CreateRun(context.Background(), "run-pinned", "review_pr", inputs); err != nil {
				t.Fatal(err)
			}
			finishRun(t, s, "run-pinned")
			if err := s.expireForgePublishGrantForRun(context.Background(), "run-pinned"); err != nil {
				t.Fatal(err)
			}
			g := mustGrant(t, s, "tok-gate")
			if left := time.Until(g.ExpiresAt); left < 24*time.Hour || g.CutBack {
				t.Errorf("the end of the pinning run cut the shared grant to %s (cut_back=%v) — the first run's verdict becomes unpostable", left.Round(time.Minute), g.CutBack)
			}
		})
	}
}

// A grant the reaper retired is cut back like one a verdict retired: a later
// launch pinning it is refused, instead of starting a run whose publish dies
// with the grant's short grace.
func TestAGrantTheReaperRetiredCannotBeShared(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, _ := gateReconcileFixture(t, gatingInputs(), gc)
	registerPublishToken(t, s, "tok-plain", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	if _, err := s.cfg.Store.CreateRun(context.Background(), "run-plain", "brancher", map[string]any{forgePublishVarToken: "tok-plain"}); err != nil {
		t.Fatal(err)
	}
	finishRun(t, s, "run-plain")
	if err := s.expireForgePublishGrantForRun(context.Background(), "run-plain"); err != nil {
		t.Fatal(err)
	}
	if g := mustGrant(t, s, "tok-plain"); !g.CutBack || time.Until(g.ExpiresAt) > s.postRunGrace()+time.Minute {
		t.Fatalf("the reaper left the ordinary grant %+v living %s, want it cut back to %s", g, time.Until(g.ExpiresAt).Round(time.Minute), s.postRunGrace())
	}
	_, err := s.injectForgePublishVars(context.Background(), "team1", "conn1", "review-pr",
		map[string]string{"pr_url": "https://github.com/o/r/pull/42", forgePublishVarToken: "tok-plain"}, nil, store.RunTrustDefault)
	if !errors.Is(err, errForgePublishGrantUnavailable) {
		t.Errorf("a launch pinning a grant the reaper retired was accepted (err=%v)", err)
	}
}

// A late outcome event for a run that was resumed meanwhile — or one paused —
// leaves its grant alone: the run will publish again.
func TestTheReaperLeavesARunThatIsNotOverAlone(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunStatusRunning, store.RunStatusQueued, store.RunStatusPausedWaitingHuman} {
		t.Run(string(status), func(t *testing.T) {
			gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
			s, _ := gateReconcileFixture(t, gatingInputs(), gc)
			registerPublishToken(t, s, "tok-plain", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
			run, err := s.cfg.Store.CreateRun(context.Background(), "run-plain", "brancher", map[string]any{forgePublishVarToken: "tok-plain"})
			if err != nil {
				t.Fatal(err)
			}
			run.Status = status
			if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
				t.Fatal(err)
			}
			if err := s.expireForgePublishGrantForRun(context.Background(), "run-plain"); err != nil {
				t.Fatal(err)
			}
			if g := mustGrant(t, s, "tok-plain"); g.CutBack || time.Until(g.ExpiresAt) < 24*time.Hour {
				t.Errorf("a %s run's grant was retired (cut_back=%v, lives %s) — it will publish again", status, g.CutBack, time.Until(g.ExpiresAt).Round(time.Minute))
			}
		})
	}
}

// A grant the reaper could not retire is reported, not passed over: it keeps
// its TTL, and the event bus logs the error.
func TestTheReaperReportsAGrantItCouldNotRetire(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, _ := gateReconcileFixture(t, gatingInputs(), gc)
	registerPublishToken(t, s, "tok-plain", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	if _, err := s.cfg.Store.CreateRun(context.Background(), "run-plain", "brancher", map[string]any{forgePublishVarToken: "tok-plain"}); err != nil {
		t.Fatal(err)
	}
	finishRun(t, s, "run-plain")
	s.forgePublishTokens = failingGrantUpdateStore{ForgePublishTokenStore: s.forgePublishTokens}
	if err := s.expireForgePublishGrantForRun(context.Background(), "run-plain"); err == nil {
		t.Error("the reaper returned no error for a grant it could not retire")
	}
	if g := mustGrant(t, s, "tok-plain"); time.Until(g.ExpiresAt) < 24*time.Hour {
		t.Errorf("a grant the reaper could not decide on was shortened to %s anyway", time.Until(g.ExpiresAt).Round(time.Minute))
	}
}
