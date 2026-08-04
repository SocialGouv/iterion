package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

type fakeBoardCoord struct {
	mu       sync.Mutex
	cands    []boardmongo.Candidate
	claimed  map[string]string
	states   map[string]string
	claimErr map[string]error
}

func newFakeBoardCoord(cands ...boardmongo.Candidate) *fakeBoardCoord {
	return &fakeBoardCoord{cands: cands, claimed: map[string]string{}, states: map[string]string{}, claimErr: map[string]error{}}
}

func (f *fakeBoardCoord) ListEligible(_ context.Context, _ []string, _ int) ([]boardmongo.Candidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []boardmongo.Candidate
	for _, c := range f.cands {
		if f.claimed[c.Issue.ID] == "" {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeBoardCoord) Claim(_ context.Context, _, id, marker string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.claimErr[id]; err != nil {
		return err
	}
	if f.claimed[id] != "" {
		return errors.New("conflict")
	}
	f.claimed[id] = marker
	return nil
}

func (f *fakeBoardCoord) SetState(_ context.Context, _, id, state string) error {
	f.mu.Lock()
	f.states[id] = state
	f.mu.Unlock()
	return nil
}

func (f *fakeBoardCoord) Release(_ context.Context, _, id, _ string) error {
	f.mu.Lock()
	delete(f.claimed, id)
	f.mu.Unlock()
	return nil
}

func readyCard(id, bot string) boardmongo.Candidate {
	return boardmongo.Candidate{Tenant: "t1", Issue: native.Issue{ID: id, Bot: bot, State: native.StateReady}}
}

// TestStampCardLastRun: the cloud coordinator must stamp the launched run onto
// the card via the CloudBoardFor seam (the local dispatcher does this itself;
// the cloud path launches through runs.Launch and previously never stamped, so
// a running card had no link to its run). A missing seam / empty run id / store
// error is a silent no-op — stamping is best-effort and never fails the run.
func TestStampCardLastRun(t *testing.T) {
	boardStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := boardStore.Create(native.Issue{Title: "x", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return boardStore }

	s.stampCardLastRun("t1", iss.ID, "run-live-1")
	got, err := boardStore.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRunID != "run-live-1" {
		t.Fatalf("card LastRunID = %q, want run-live-1", got.LastRunID)
	}

	// Empty run id and a nil seam must be no-ops, not panics.
	s.stampCardLastRun("t1", iss.ID, "")
	if got, _ := boardStore.Get(iss.ID); got.LastRunID != "run-live-1" {
		t.Fatalf("empty run id must not clobber the stamp, got %q", got.LastRunID)
	}
	(&Server{}).stampCardLastRun("t1", iss.ID, "run-x") // CloudBoardFor nil → no-op
}

// TestSetCardAwaitingInput: the cloud coordinator denormalizes the pause hint
// onto the card via the same CloudBoardFor seam, so the board grid can badge a
// paused card without an N+1 run fetch. Set true on pause, clear false on
// dispatch; a nil seam is a silent no-op.
func TestSetCardAwaitingInput(t *testing.T) {
	boardStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := boardStore.Create(native.Issue{Title: "x", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return boardStore }

	s.setCardAwaitingInput("t1", iss.ID, true)
	if got, _ := boardStore.Get(iss.ID); !got.AwaitingInput {
		t.Fatalf("card AwaitingInput = false, want true")
	}
	s.setCardAwaitingInput("t1", iss.ID, false)
	if got, _ := boardStore.Get(iss.ID); got.AwaitingInput {
		t.Fatalf("card AwaitingInput = true, want cleared")
	}
	(&Server{}).setCardAwaitingInput("t1", iss.ID, true) // CloudBoardFor nil → no-op
}

func TestBoardDispatcher_ClaimsProcessesAndMoves(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"), readyCard("native:2", "sec-audit-source"))
	var pmu sync.Mutex
	processed := map[string]int{}
	d := newBoardDispatcher(f, func(_ context.Context, _ string, iss native.Issue) error {
		pmu.Lock()
		processed[iss.ID]++
		pmu.Unlock()
		return nil
	}, "replica-A", 4, nil)

	if n := d.tick(context.Background()); n != 2 {
		t.Fatalf("tick should claim 2, got %d", n)
	}
	d.wg.Wait()

	if processed["native:1"] != 1 || processed["native:2"] != 1 {
		t.Errorf("each card should process once: %v", processed)
	}
	if f.states["native:1"] != native.StateDone || f.states["native:2"] != native.StateDone {
		t.Errorf("cards should move to done: %v", f.states)
	}
	if len(f.claimed) != 0 {
		t.Errorf("cards should be released: %v", f.claimed)
	}
}

func TestBoardDispatcher_FailedRunMovesToBlocked(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		return errors.New("run failed")
	}, "replica-A", 4, nil)
	d.tick(context.Background())
	d.wg.Wait()
	if f.states["native:1"] != native.StateBlocked {
		t.Errorf("failed run should move card to blocked, got %q", f.states["native:1"])
	}
}

func TestBoardDispatcher_PausedRunMovesToAwaitingInput(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	// A process func that signals a pause (errCardPaused) rather than a failure.
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		return fmt.Errorf("run r1 paused (paused_waiting_human): %w", errCardPaused)
	}, "replica-A", 4, nil)
	d.tick(context.Background())
	d.wg.Wait()
	if f.states["native:1"] != native.StateAwaitingInput {
		t.Errorf("paused run should move card to awaiting_input, got %q", f.states["native:1"])
	}
	if len(f.claimed) != 0 {
		t.Errorf("card should be released after a pause: %v", f.claimed)
	}
}

// TestBoardDispatcher_SweepMovesParkedCards: a cloud card parks UNCLAIMED in
// awaiting_input and every resume surface completes the run outside the
// dispatcher's poll loop, so only sweepParked can move it on. finished →
// done, hard-failed → blocked, resumable/in-flight statuses stay parked; the
// denormalized ⏸ badge is cleared on every terminal move.
func TestBoardDispatcher_SweepMovesParkedCards(t *testing.T) {
	awaiting := func(id, runID string) boardmongo.Candidate {
		return boardmongo.Candidate{Tenant: "t1", Issue: native.Issue{ID: id, State: native.StateAwaitingInput, LastRunID: runID}}
	}
	f := newFakeBoardCoord(
		awaiting("native:done", "run-finished"),
		awaiting("native:dead", "run-failed"),
		awaiting("native:wait", "run-paused"),
		awaiting("native:redo", "run-resumable"),
		awaiting("native:none", ""), // never dispatched — must be left alone
	)
	statuses := map[string]store.RunStatus{
		"run-finished":  store.RunStatusFinished,
		"run-failed":    store.RunStatusFailed,
		"run-paused":    store.RunStatusPausedWaitingHuman,
		"run-resumable": store.RunStatusFailedResumable,
	}
	var bmu sync.Mutex
	badgeCleared := map[string]bool{}
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		t.Fatal("sweep must never dispatch a parked card")
		return nil
	}, "replica-A", 4, nil)
	d.statusFor = func(_ context.Context, _, runID string) (store.RunStatus, error) {
		return statuses[runID], nil
	}
	d.clearBadge = func(_, id string) {
		bmu.Lock()
		badgeCleared[id] = true
		bmu.Unlock()
	}

	d.sweepParked(context.Background())

	if got := f.states["native:done"]; got != native.StateDone {
		t.Errorf("finished card state = %q, want %q", got, native.StateDone)
	}
	if got := f.states["native:dead"]; got != native.StateBlocked {
		t.Errorf("hard-failed card state = %q, want %q", got, native.StateBlocked)
	}
	for _, id := range []string{"native:wait", "native:redo", "native:none"} {
		if got, moved := f.states[id]; moved {
			t.Errorf("card %s must stay parked, was moved to %q", id, got)
		}
	}
	if !badgeCleared["native:done"] || !badgeCleared["native:dead"] {
		t.Errorf("badge must be cleared on terminal moves: %v", badgeCleared)
	}
	if badgeCleared["native:wait"] || badgeCleared["native:redo"] {
		t.Errorf("badge must survive on parked cards: %v", badgeCleared)
	}

	// nil statusFor (unwired) must be a hard no-op.
	d2 := newBoardDispatcher(f, func(context.Context, string, native.Issue) error { return nil }, "replica-A", 4, nil)
	d2.sweepParked(context.Background())
}

func TestBoardDispatcher_ClaimConflictSkips(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	f.claimErr["native:1"] = errors.New("held by another replica")
	var processed int
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		processed++
		return nil
	}, "replica-A", 4, nil)
	if n := d.tick(context.Background()); n != 0 {
		t.Fatalf("claim conflict should yield 0 claims, got %d", n)
	}
	d.wg.Wait()
	if processed != 0 {
		t.Errorf("a card we couldn't claim must not be processed, got %d", processed)
	}
	// The semaphore slot must have been released on the failed claim.
	if len(d.sem) != 0 {
		t.Errorf("semaphore slot leaked on failed claim: %d held", len(d.sem))
	}
}

// liftBoardLaunchContext must round-trip the webhook launch context stamped by
// ensureBoardCard: repo + BYOK key/secret overrides come off the reserved keys
// into the LaunchSpec, the bot's own vars are preserved, and the reserved keys
// never leak into those vars. Without this the board-coordinator launch has no
// overrides (the webhook's never reach it).
func TestLiftBoardLaunchContext(t *testing.T) {
	botArgs := map[string]string{
		"feature_prompt":        "add X",
		boardRepoURLKey:         "https://github.com/acme/api.git",
		boardRepoRefKey:         "main",
		boardKeyOverridesKey:    `{"anthropic":"key-1"}`,
		boardSecretOverridesKey: `{"forge_token":"sec-1"}`,
	}
	lc, err := liftBoardLaunchContext(botArgs)
	if err != nil {
		t.Fatalf("liftBoardLaunchContext: %v", err)
	}
	if lc.RepoURL != "https://github.com/acme/api.git" || lc.RepoRef != "main" {
		t.Errorf("repo lifted wrong: %q %q", lc.RepoURL, lc.RepoRef)
	}
	if lc.KeyOverrides["anthropic"] != "key-1" {
		t.Errorf("key overrides = %v", lc.KeyOverrides)
	}
	if lc.SecretOverrides["forge_token"] != "sec-1" {
		t.Errorf("secret overrides = %v", lc.SecretOverrides)
	}
	if lc.Vars["feature_prompt"] != "add X" {
		t.Errorf("bot var dropped: %v", lc.Vars)
	}
	for _, reserved := range []string{boardRepoURLKey, boardRepoRefKey, boardKeyOverridesKey, boardSecretOverridesKey} {
		if _, leaked := lc.Vars[reserved]; leaked {
			t.Errorf("reserved key %q leaked into bot vars", reserved)
		}
	}
	// A malformed override blob must fail the lift: silently dropping it
	// would launch without the webhook's key/secret overrides.
	if _, err := liftBoardLaunchContext(map[string]string{boardKeyOverridesKey: "{not json"}); err == nil {
		t.Errorf("malformed key-override blob should error")
	} else if !strings.Contains(err.Error(), boardKeyOverridesKey) {
		t.Errorf("error should name the offending bot-arg key, got: %v", err)
	}
	if _, err := liftBoardLaunchContext(map[string]string{boardSecretOverridesKey: "{not json"}); err == nil {
		t.Errorf("malformed secret-override blob should error")
	} else if !strings.Contains(err.Error(), boardSecretOverridesKey) {
		t.Errorf("error should name the offending bot-arg key, got: %v", err)
	}
}

// TestProcessBoardCardCarriesPRLaunchContext: a card that targets a pull
// request must reach its run with the repo's launch policy AND a forge-publish
// grant. Neither can ride the card — ensureBoardCard copies a curated subset of
// vars, and a grant expires — so the cloud coordinator resolves both at claim
// time. Without them a board-mode fixer pushes its commits and then has no
// endpoint to post its verdict or its merge-gate status to, leaving the repo's
// required check stale on the head it just created.
//
// Drives the real processBoardCard rather than applyPRLaunchContext alone: a
// correct composition is worthless if the launch path never calls it.
func TestProcessBoardCardCarriesPRLaunchContext(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.example"
	s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	// The gate context arrives from the co-enabled bots' manifest union, not
	// from an operator pin — the layer a policy built from the integration
	// alone would drop, resolving the var on every webhook lane and nowhere
	// else.
	if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
		ID: "wh1", TenantID: "team1",
		LaunchVars:         map[string]string{"gate_context": "iterion/review", "post_to_board": "false"},
		OperatorLaunchVars: map[string]string{"gate_severity": "high"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{
		ID: "ri1", TenantID: "team1", ConnectionID: "conn1", RepoFullName: "o/r",
		WebhookID:  "wh1",
		LaunchVars: map[string]string{"gate_severity": "high"},
	}); err != nil {
		t.Fatal(err)
	}

	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "fixer.bot"), []byte(
		// worktree: none — isolation is the IR default, and here it would fork
		// a worktree off the live checkout and provision its devbox for a
		// fixture that only asserts on launch vars.
		"schema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"yes\"}'`\n  output: probe_out\n\nworkflow board_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}

	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := runview.NewService("", runview.WithStore(rs))
	if err != nil {
		t.Fatal(err)
	}
	s.runs = svc

	// processBoardCard blocks polling the run on a 3s ticker; cancelling
	// releases it (the loop selects on ctx.Done) so the goroutine is joined
	// before the temp store is torn down.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.processBoardCard(ctx, "team1", native.Issue{
			ID: "card1", Bot: "fixer", State: native.StateReady,
			BotArgs: map[string]string{"pr_url": "https://github.com/o/r/pull/7"},
		})
	}()
	t.Cleanup(func() { cancel(); <-done })

	// Wait for the run to SETTLE, not merely to exist: reading run.json the
	// moment it appears leaves the engine's goroutines writing into the temp
	// store while cleanup removes it.
	var inputs map[string]any
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if ids, lerr := rs.ListRuns(ctx); lerr == nil && len(ids) > 0 {
			if run, rerr := rs.LoadRun(ctx, ids[0]); rerr == nil && run.Status.IsTerminal() {
				inputs = run.Inputs
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if inputs == nil {
		t.Fatal("no run settled from the card")
	}

	if got, _ := inputs["gate_context"].(string); got != "iterion/review" {
		t.Errorf("the repo's gate_context did not reach the run: %q", got)
	}
	if got, _ := inputs["gate_severity"].(string); got != "high" {
		t.Errorf("the operator layer did not reach the run: %q", got)
	}
	if got, _ := inputs[forgePublishVarURL].(string); got != "https://iterion.example/api/v1/forge/publish-review" {
		t.Errorf("publish endpoint not injected: %q", got)
	}
	tok, _ := inputs[forgePublishVarToken].(string)
	if tok == "" {
		t.Fatal("no publish grant minted for the board launch")
	}
	g, ok := s.forgePublishTokens.lookup(tok)
	if !ok || g.TeamID != "team1" || g.ConnectionID != "conn1" || g.Repo != "o/r" {
		t.Fatalf("grant scoped wrong: ok=%v g=%+v", ok, g)
	}
}

// TestApplyPRLaunchContextPrecedence: the repo's policy fills gaps only — a
// value already on the launch is a deliberate per-run pin — and within that
// policy the operator's override beats the manifest union. A launch with no
// pull request gets nothing: neither the grant nor the gate has any meaning.
func TestApplyPRLaunchContextPrecedence(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.example"
	s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
		ID: "wh1", TenantID: "team1",
		// The union carries a co-enabled bot's value for post_to_board; the
		// per-bot rule is what keeps it from reaching the other bot.
		LaunchVars:         map[string]string{"gate_context": "revi/review", "gate_severity": "low", "post_to_board": "false"},
		OperatorLaunchVars: map[string]string{"gate_context": "iterion/review"},
		BotRules: []webhooks.BotRule{
			{BotID: "fixer", LaunchVars: map[string]string{"post_to_board": "true"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{
		ID: "ri1", TenantID: "team1", ConnectionID: "conn1", RepoFullName: "o/r",
		WebhookID:  "wh1",
		LaunchVars: map[string]string{"gate_context": "iterion/review"},
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{
		"pr_url":        "https://github.com/o/r/pull/7",
		"gate_severity": "medium", // pinned on the launch — must survive
		"gate_context":  "",       // cleared field: absent, not a decision
	}
	out := s.applyPRLaunchContext(context.Background(), "team1", "", "fixer", vars, nil)
	if out["gate_severity"] != "medium" {
		t.Errorf("repo policy overwrote a launch pin: %q", out["gate_severity"])
	}
	if out["gate_context"] != "iterion/review" {
		t.Errorf("a blank value must not suppress the repo's pin: %q", out["gate_context"])
	}
	if out["post_to_board"] != "true" {
		t.Errorf("the bot's own rule must beat the co-enabled union: %q", out["post_to_board"])
	}

	// A repo re-provisioned onto another connection leaves the first row
	// behind — the live shape on the e2e repo, where the stale row is a
	// personal-token connection and the current one a GitHub App. The newer
	// provisioning is the operator's intent, and it decides which identity the
	// verdict is posted under, so it must win regardless of store order.
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "conn2", TenantID: "team1", Provider: forge.ProviderGitHub,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{
		// Sorts BEFORE ri1 lexicographically, and is older.
		ID: "aa-stale", TenantID: "team1", ConnectionID: "conn2", RepoFullName: "o/r",
		CreatedAt: time.Now().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	out = s.applyPRLaunchContext(context.Background(), "team1", "", "fixer",
		map[string]string{"pr_url": "https://github.com/o/r/pull/7"}, nil)
	if g, ok := s.forgePublishTokens.lookup(out[forgePublishVarToken]); !ok || g.ConnectionID != "conn1" {
		t.Errorf("the stale provisioning won the grant: ok=%v g=%+v", ok, g)
	}
	if out["gate_context"] != "iterion/review" {
		t.Errorf("the stale provisioning won the policy: %q", out["gate_context"])
	}

	// Same slug on another forge is a different repo: no policy, no grant.
	out = s.applyPRLaunchContext(context.Background(), "team1", "", "fixer",
		map[string]string{"pr_url": "https://gitlab.example/o/r/-/merge_requests/7"}, nil)
	if _, ok := out["gate_context"]; ok {
		t.Errorf("policy applied across forge hosts: %v", out)
	}
	if _, ok := out[forgePublishVarToken]; ok {
		t.Error("grant minted for a repo on a host no connection covers")
	}

	out = s.applyPRLaunchContext(context.Background(), "team1", "", "fixer", map[string]string{"base_ref": "main"}, nil)
	if _, ok := out[forgePublishVarToken]; ok {
		t.Error("no pr_url: nothing to grant")
	}
	if _, ok := out["gate_context"]; ok {
		t.Error("no pr_url: no repo policy to apply")
	}
}
