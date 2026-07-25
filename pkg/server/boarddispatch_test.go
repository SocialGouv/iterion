package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
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
