package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fixLaunchFixture is inFlightFixture plus the REAL bots/ catalog, because the
// role is derived from each bot's manifest produces:/consumes: and the engine
// names no bot. Without the catalog every role reads `unknown` and every
// assertion below would pass vacuously — which is exactly what the first draft
// of this file did.
func fixLaunchFixture(t *testing.T, gc forgeGateClient) *Server {
	t.Helper()
	s := inFlightFixture(t, gc)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	return s
}

// fixLaunchVars is a fixer-shaped launch: a pull request and the revision it
// is about to rewrite. Deliberately carries a gate_context too — a fixer
// launched through a repo that pins one must still not write there.
func fixLaunchVars() map[string]string {
	return map[string]string{
		"pr_url":       "https://github.com/o/r/pull/42",
		"head_sha":     "deadbeef",
		"gate_context": "iterion/review",
	}
}

// The window this closes: a fixer works for tens of minutes and holds no
// required check, so until it reported, NOTHING on the pull request said it
// was there. A second writer pushing in that window collides with the
// push-back, the auto-rebase conflicts, and the pass is banked instead of
// landed. Measured twice on one PR on 2026-09-09.
func TestMarkFixInFlight_ClaimsForAFixer(t *testing.T) {
	gc := &listingGateClient{}
	s := fixLaunchFixture(t, gc)

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — the fixer stays invisible for its whole run", gc.setCalls)
	}
	if gc.last.State != forge.CommitStatePending {
		t.Errorf("state = %q, want pending — the fixer has not reported yet", gc.last.State)
	}
	if !isFixInFlight(gc.last) {
		t.Errorf("status %q is not recognisable as the fixer claim — the terminal clear would leave it pending forever", gc.last.Description)
	}
	if gc.last.TargetURL != "https://iterion.test/runs/run-77" {
		t.Errorf("target url = %q, want the live run console — an unattributable claim can never be resolved", gc.last.TargetURL)
	}
	if gc.lastSHA != "deadbeef" {
		t.Errorf("posted on %q, want the revision the run was handed", gc.lastSHA)
	}
}

// THE load-bearing separation. The fixer must never occupy the context branch
// protection may require: writing there could blank a reviewer's verdict back
// to "running" — the exact harm markGateInFlight is written to avoid — and
// could block a merge on a signal that is meant to be advisory.
func TestMarkFixInFlight_NeverWritesOnTheGateContext(t *testing.T) {
	gc := &listingGateClient{}
	s := fixLaunchFixture(t, gc)
	vars := fixLaunchVars()

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", vars, "run-77")

	if gc.last.Context == vars["gate_context"] {
		t.Fatalf("the fixer claimed %q, the repo's REQUIRED gate — a reviewer verdict there would be blanked to running", gc.last.Context)
	}
	if gc.last.Context != fixInFlightContext {
		t.Errorf("context = %q, want %q", gc.last.Context, fixInFlightContext)
	}
}

// The role decides, and it comes from the manifest — never a bot id. A
// REVIEWER already claims the gate context; giving it a second claim would put
// a pending status on every reviewed head that nothing in the reviewer lane
// ever resolves.
func TestMarkFixInFlight_SilentForANonFixer(t *testing.T) {
	for _, bot := range []string{"review-pr", "", "a-bot-no-catalog-knows"} {
		gc := &listingGateClient{}
		s := fixLaunchFixture(t, gc)

		s.markFixInFlight(context.Background(), "team1", "", bot, fixLaunchVars(), "run-77")

		if gc.setCalls != 0 {
			t.Errorf("bot %q claimed the fixer context (%d posts) — only a bot that CONSUMES a review is a fixer", bot, gc.setCalls)
		}
	}
}

// A launch that names no pull request, or no revision, has nothing to claim
// on. Silence, not a guess.
func TestMarkFixInFlight_NeedsAPRAndARevision(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"no pr_url":   func(v map[string]string) { delete(v, "pr_url") },
		"no head_sha": func(v map[string]string) { delete(v, "head_sha") },
	} {
		gc := &listingGateClient{}
		s := fixLaunchFixture(t, gc)
		vars := fixLaunchVars()
		mutate(vars)

		s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", vars, "run-77")

		if gc.setCalls != 0 {
			t.Errorf("%s: posted anyway (%d)", name, gc.setCalls)
		}
	}
}

// Read before write. A status somebody else owns on this context is not ours
// to blank — the same discipline the gate claim keeps.
func TestMarkFixInFlight_NeverOverwritesAForeignStatus(t *testing.T) {
	for _, st := range []forge.CommitState{forge.CommitStateSuccess, forge.CommitStateFailure, forge.CommitStatePending} {
		gc := &listingGateClient{statuses: []forge.CommitStatus{
			{Context: fixInFlightContext, State: st, Description: "something another tool posted"},
		}}
		s := fixLaunchFixture(t, gc)

		s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

		if gc.setCalls != 0 {
			t.Errorf("state %q: overwrote a status this server does not own", st)
		}
	}
}

// fixRunFixture builds a terminal fixer run holding the inputs the clear reads.
func fixRunFixture(t *testing.T, gc forgeGateClient, status store.RunStatus) (*Server, *store.Run) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := newForgeGateTestServer(t, st)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	s.cfg.PublicURL = "https://iterion.test"
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return gc, nil
	}
	run, err := st.CreateRun(context.Background(), "run-77", "branch-improve-loop", map[string]any{
		"pr_url": "https://github.com/o/r/pull/42", "head_sha": "deadbeef",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// The clear resolves its connection off the RUN's tenant (a terminal
	// sweep has no ambient one), so the fixture must carry the tenant the
	// harness registered its connection under — as a real run always does.
	run.TenantID = "team1"
	// And its BotID, which CreateRun's third argument does NOT set: the role
	// is derived from it, so leaving it empty makes every role read `unknown`
	// and every "must not touch" assertion below pass without exercising
	// anything. Measured: three of them did exactly that.
	run.BotID = "branch-improve-loop"
	run.Status = status
	return s, run
}

// A claim that is never resolved is worse than none: it would tell every later
// reader that a run is working when none is. The terminal clear RESOLVES it
// rather than deleting it — a status that vanishes reads as "never claimed",
// the ambiguity this file exists to remove.
func TestClearFixInFlight_ResolvesOurOwnClaim(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunStatusFinished, store.RunStatusFailed, store.RunStatusCancelled} {
		gc := &listingGateClient{statuses: []forge.CommitStatus{
			{Context: fixInFlightContext, State: forge.CommitStatePending, Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
		}}
		s, run := fixRunFixture(t, gc, status)

		s.clearFixInFlight(context.Background(), run)

		if gc.setCalls != 1 {
			t.Fatalf("%s: posted %d, want 1 — the claim stays pending on a run that is over", status, gc.setCalls)
		}
		if gc.last.State != forge.CommitStateSuccess {
			t.Errorf("%s: state = %q, want success", status, gc.last.State)
		}
		if isFixInFlight(gc.last) {
			t.Errorf("%s: still reads as in-flight after the run ended", status)
		}
	}
}

// While the run is alive the claim is exactly right, and clearing it would
// re-open the window it exists to close.
func TestClearFixInFlight_SilentWhileTheRunIsAlive(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunStatusRunning, store.RunStatusQueued, store.RunStatusPausedWaitingHuman} {
		gc := &listingGateClient{statuses: []forge.CommitStatus{
			{Context: fixInFlightContext, State: forge.CommitStatePending, Description: fixInFlightDescription},
		}}
		s, run := fixRunFixture(t, gc, status)

		s.clearFixInFlight(context.Background(), run)

		if gc.setCalls != 0 {
			t.Errorf("%s: released the claim on a run still in flight", status)
		}
	}
}

// R51e567 — `failed_resumable` is terminal to Status.IsTerminal, but a run the
// PLATFORM still owns is parked, not done: the queue redelivers it (the NAK
// family — sandbox setup timeout, capacity, a drained runner) or the retry
// sweeper resumes it (the usage-window park). It then goes on rewriting the
// branch, and neither wake-up passes through a launch surface, so nothing
// re-claims: the status would stay green for that whole second pass.
//
// The NAK family fires no outcome event, which is why the guard cannot live on
// the event path — the gate SWEEP offers the same run (ListNotifiableRuns
// includes failed_resumable) and calls the clear from there.
func TestClearFixInFlight_SilentWhileThePlatformOwnsTheRun(t *testing.T) {
	retryAfter := time.Now().Add(30 * time.Minute)
	for name, park := range map[string]func(*store.Run){
		// A Nak'd redelivery: the runner promotes the continuation and writes
		// NO RetryState at all, so a RetryAfter-only guard misses it entirely.
		"queue redelivery": func(r *store.Run) {
			r.ContinuationState = store.ContinuationRedeliveryPending
		},
		"armed retry": func(r *store.Run) {
			r.ContinuationState = store.ContinuationRetryArmed
			r.RetryState = &store.RunRetryState{RetryAfter: &retryAfter}
		},
		// A doc written before the continuation promote, or by a path that
		// bypassed it: the timestamp is all the ownership there is.
		"legacy armed retry, no continuation": func(r *store.Run) {
			r.RetryState = &store.RunRetryState{RetryAfter: &retryAfter}
		},
	} {
		gc := &listingGateClient{statuses: []forge.CommitStatus{
			{Context: fixInFlightContext, State: forge.CommitStatePending,
				Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
		}}
		s, run := fixRunFixture(t, gc, store.RunStatusFailedResumable)
		park(run)

		s.clearFixInFlight(context.Background(), run)

		if gc.setCalls != 0 {
			t.Errorf("%s: posted the all-clear (%d) on a run the platform will resume — it goes on rewriting the branch with the PR saying pushing is safe", name, gc.setCalls)
		}
	}
}

// The other half of that guard, and the one a later simplification would
// delete: a DLQ park is FINAL for automation whatever the markers still say —
// the queue exhausted its deliveries and only an operator's replay wakes it.
// Its claim must resolve, or it sits pending on a run nothing will ever
// continue. (The retry sweeper disarms exactly such a stale armed retry.)
func TestClearFixInFlight_ResolvesADLQPark(t *testing.T) {
	retryAfter := time.Now().Add(30 * time.Minute)
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFailedResumable)
	run.FailureCode = store.FailureDLQParked
	// Markers that survived the park: the guard must not read them as ownership.
	run.ContinuationState = store.ContinuationRetryArmed
	run.RetryState = &store.RunRetryState{RetryAfter: &retryAfter}

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — a DLQ-parked fixer's claim would sit pending forever", gc.setCalls)
	}
	if gc.last.State != forge.CommitStateSuccess {
		t.Errorf("state = %q, want success", gc.last.State)
	}
}

// R65a408 — THE one the first draft of this file could not see. Its
// "foreign status" case used a different DESCRIPTION, so it only ever proved
// that another tool's verdict survives. It never proved that another RUN's
// claim does, and that is the case production actually produces:
//
// the auto-fix lane launches the fixer on the REVIEWER's own head_sha, and two
// consecutive fixer passes reuse it as well. The sweeper re-offers every
// terminal run for the whole lookback — so the reviewer's own reconcile pass,
// terminal and on the same sha, would read the LIVE fixer's claim, match the
// description, and post "the fix run is done" while the branch is still being
// rewritten. A false all-clear, in the exact lane this feature exists for.
//
// Ownership is the target URL, never the shape of the text.
func TestClearFixInFlight_LeavesAnotherRunsClaimAlone(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		// Byte-identical to what this run would post — except whose it is.
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription,
			TargetURL:   "https://iterion.test/runs/some-other-run"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFinished)

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 0 {
		t.Fatalf("resolved a claim posted by ANOTHER run (%d posts) — a live fixer would be announced done while it is still rewriting the branch", gc.setCalls)
	}
}

// R4e92c6 — `pr_url` + `head_sha` are set on EVERY forge-launched run, so
// without a role check before the network the clear pays a live
// ListCommitStatuses for reviewers, branchers, implementers and docs-amenders
// too — on every sweep pass, for the whole lookback, on a path that used to
// exit on a local field read. The assertion is on the READ, not on the write:
// a guard placed after the round trip would still cost it.
func TestClearFixInFlight_NoForgeTrafficForANonFixer(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending, Description: fixInFlightDescription},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFinished)
	run.BotID = "review-pr" // a reviewer: it never claimed, so it owes no clear

	s.clearFixInFlight(context.Background(), run)

	if gc.listCalls != 0 {
		t.Errorf("read the forge %d time(s) for a non-fixer — this path must cost no network at all", gc.listCalls)
	}
	if gc.setCalls != 0 {
		t.Errorf("a non-fixer posted %d status(es)", gc.setCalls)
	}
}

// Symmetric to the claim: only ever OUR marker. A verdict another tool posted
// on this context survives the run ending.
func TestClearFixInFlight_LeavesAForeignStatusAlone(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStateFailure, Description: "another tool's verdict"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFinished)

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 0 {
		t.Fatalf("overwrote a status this server does not own (%d posts)", gc.setCalls)
	}
}

// recordingGateClient is listingGateClient with a lock, because the launch
// surfaces below post from a goroutine the test does not own.
type recordingGateClient struct {
	fakeGateClient
	mu     sync.Mutex
	posted []forge.CommitStatus
}

func (f *recordingGateClient) ListCommitStatuses(context.Context, string, string) ([]forge.CommitStatus, error) {
	return nil, nil
}

func (f *recordingGateClient) SetCommitStatus(_ context.Context, _, _ string, st forge.CommitStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posted = append(f.posted, st)
	return nil
}

func (f *recordingGateClient) claim() (forge.CommitStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, st := range f.posted {
		if st.Context == fixInFlightContext {
			return st, true
		}
	}
	return forge.CommitStatus{}, false
}

// writeFixerBundle is a launchable fixer: a manifest that CONSUMES a review
// (which is the whole definition of the role — the engine names no bot) plus a
// workflow trivial enough to run for real in a test. `worktree: none` because
// the IR default would fork one off the live checkout and provision its devbox
// for a fixture that only asserts on what got posted.
func writeFixerBundle(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"),
		[]byte("name: "+name+"\nschema_version: 1\nconsumes:\n  - kind: review\n    var: prior_review\n    scope: pr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(
		"schema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"yes\"}'`\n  output: probe_out\n\nworkflow board_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(dir)
}

// A `/billy` on a cloud deployment is a BOARD-mode command
// (bots/branch-improve-loop/manifest.yaml declares `mode: board`), so with a
// coordinator wired the card IS the launch: dispatchInvocation returns
// "carded" and the webhook launch tail — which holds the other claim call — is
// never reached. Claiming only there would leave the primary human `/billy`
// invisible while the zero-touch auto-fix lane (which does go through the
// tail) signalled: the worse half to lose, and the exact lane the measured
// collision happened on.
//
// Drives the real processBoardCard rather than markFixInFlight alone: a
// correct claim is worthless if the launch path never calls it.
func TestProcessBoardCard_ClaimsTheFixInFlightContext(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.test"
	s.cfg.Bots.Paths = []string{writeFixerBundle(t, "fixer")}
	gc := &recordingGateClient{fakeGateClient: fakeGateClient{headSHA: "abc"}}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return gc, nil
	}

	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.processBoardCard(ctx, "team1", native.Issue{
			ID: "card1", Bot: "fixer", State: native.StateReady,
			// The two vars ensureBoardCard carries onto a command card, and
			// the two the claim needs.
			BotArgs: map[string]string{
				"pr_url":   "https://github.com/o/r/pull/7",
				"head_sha": "deadbeef",
			},
		})
	}()
	t.Cleanup(func() { cancel(); <-done })

	// Settling the run is the sync point: the claim is posted before the card
	// poll begins, so a terminal run means it has been posted or never will be.
	var settled *store.Run
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if ids, lerr := rs.ListRuns(ctx); lerr == nil && len(ids) > 0 {
			if run, rerr := rs.LoadRun(ctx, ids[0]); rerr == nil && run.Status.IsTerminal() {
				settled = run
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if settled == nil {
		t.Fatal("no run settled from the card")
	}

	st, ok := gc.claim()
	if !ok {
		t.Fatal("the board lane launched a fixer onto a pull request and said nothing on it — the primary /billy path is exactly the one that must claim")
	}
	if !isFixInFlight(st) {
		t.Errorf("posted %q on %s, which the terminal clear will not recognise as its own claim", st.Description, st.Context)
	}
	if st.TargetURL != "https://iterion.test/runs/"+settled.ID {
		t.Errorf("target url = %q, want the launched run's console — ownership is the target URL", st.TargetURL)
	}
}

// The third surface, and the one no webhook covers: a fixer picked by hand in
// the studio (or over the HTTP API) on a pull request. It composes the same PR
// launch context as the other two and rewrites the branch the same way, so
// leaving it unclaimed would keep a whole way of starting a fixer invisible.
func TestHandleLaunchRun_ClaimsTheFixInFlightContext(t *testing.T) {
	s := launchPRContextServer(t, "team1")
	s.cfg.Bots.Paths = []string{writeFixerBundle(t, "fixer")}
	gc := &recordingGateClient{fakeGateClient: fakeGateClient{headSHA: "abc"}}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return gc, nil
	}

	body, err := json.Marshal(map[string]any{
		// The bot id is what carries the ROLE — the manifest behind it is
		// where `consumes: review` lives. The source rides along because a
		// local studio launches from bytes, not from the catalog path.
		"bot_id":    "fixer",
		"file_path": "pr.bot",
		"source":    "workflow pr:\n  entry: done\n",
		"vars": map[string]string{
			"pr_url":   "https://github.com/o/r/pull/42",
			"head_sha": "deadbeef",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{UserID: "u1", TeamID: "team1"}))
	rec := httptest.NewRecorder()
	s.handleLaunchRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("launch status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}

	// Posted synchronously by the handler, before it answers: no polling.
	st, ok := gc.claim()
	if !ok {
		t.Fatal("a hand-launched fixer said nothing on the pull request it is about to rewrite")
	}
	if !isFixInFlight(st) {
		t.Errorf("posted %q on %s, which the terminal clear will not recognise as its own claim", st.Description, st.Context)
	}
	var launched launchRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &launched); err != nil {
		t.Fatalf("launch response: %v", err)
	}
	if st.TargetURL != "https://iterion.test/runs/"+launched.RunID {
		t.Errorf("target url = %q, want the launched run's console — ownership is the target URL", st.TargetURL)
	}
}
