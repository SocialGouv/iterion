package server

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// pausedWorld is a pWorld whose run holds its launch claim and then paused for
// a human, with a commenter recording the pull request's comments.
type pausedWorld struct {
	pWorld
	c        *stubCommenter
	runID    string
	pausedAt time.Time
}

func newPausedWorld(t *testing.T, status store.RunStatus, cp *store.Checkpoint) pausedWorld {
	t.Helper()
	w := newPWorld(t, gatingInputs(), "deadbeef")
	c := &stubCommenter{}
	w.s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) {
		return c, nil
	}
	const runID = "run-gating"
	ctx := context.Background()
	run, err := w.s.cfg.Store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = status
	run.Checkpoint = cp
	if err := w.s.cfg.Store.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	run, err = w.s.cfg.Store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.UpdatedAt.IsZero() {
		t.Fatal("the paused run carries no updated_at — the pause cannot be dated")
	}
	return pausedWorld{pWorld: w, c: c, runID: runID, pausedAt: run.UpdatedAt}
}

// at moves the gate clock to d after the pause.
func (w pausedWorld) at(d time.Duration) { *w.now = w.pausedAt.Add(d) }

func (w pausedWorld) offer(t *testing.T) {
	t.Helper()
	if err := w.s.reconcileGateForRunID(context.Background(), w.runID, gateTriggerSweep); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func (w pausedWorld) head() forge.CommitStatus { return w.f.on("deadbeef", pCtx) }

func authFailedPause() *store.Checkpoint {
	return &store.Checkpoint{NodeID: "audit", RecoveryPause: true, RecoveryCode: "AUTH_FAILED"}
}

// The #3768 shape: a gating run paused on a recovery pause after AUTH_FAILED
// held the required check pending until its pull request closed. Past the
// bound its claim is answered with a failure that names the pause and its
// cure, the pull request is told once, and the run is left paused — not
// cancelled, not relaunched.
func TestGatePause_PastTheBoundTheClaimIsAnsweredAndThePRToldOnce(t *testing.T) {
	w := newPausedWorld(t, store.RunStatusPausedWaitingHuman, authFailedPause())
	w.at(defaultGatePauseAnswerAfter + time.Minute)
	w.offer(t)

	got := w.head()
	if got.State != forge.CommitStateFailure {
		t.Fatalf("head = %s %q, want the synthetic failure — a paused run must not hold the required check pending", got.State, got.Description)
	}
	want := "review paused (AUTH_FAILED at audit" + gatePausedRecoveryRemedy
	if got.Description != want {
		t.Errorf("description = %q, want %q", got.Description, want)
	}
	if !isSyntheticGateInterruption(got.Description) {
		t.Errorf("%q is not recognised as synthetic — the auto-fix lane would read findings into it", got.Description)
	}
	if got.TargetURL != gateRunURL("https://iterion.test", w.runID) {
		t.Errorf("target = %q, want the paused run", got.TargetURL)
	}
	if len(w.c.bodies) != 1 {
		t.Fatalf("posted %d comments, want exactly one notice", len(w.c.bodies))
	}
	body := w.c.bodies[0]
	for _, part := range []string{gatePausedNoticeMarker, "AUTH_FAILED", "`audit`", "marked it **failed**", "not relaunched",
		"recovery prompt", gateRunURL("https://iterion.test", w.runID), "its verdict replaces this failure"} {
		if !strings.Contains(body, part) {
			t.Errorf("notice lacks %q:\n%s", part, body)
		}
	}
	run, err := w.s.cfg.Store.LoadRun(context.Background(), w.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusPausedWaitingHuman {
		t.Errorf("run status = %s, want it still paused for its human", run.Status)
	}
	marks, err := w.s.gateSettles.settled(context.Background(), []string{w.runID})
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := marks[w.runID]; !ok || m.Reason != gateSettledPaused || !m.appliesTo(run.UpdatedAt) {
		t.Errorf("settle mark = %+v (found %v), want %q for this pause's episode — the sweep would re-read the forge every pass", m, ok, gateSettledPaused)
	}

	// The sweep keeps offering a paused run on every pass: the forge already
	// carries this pause's answer, so nothing is written or said again.
	sets := w.f.sets
	w.at(defaultGatePauseAnswerAfter + 2*time.Hour)
	w.offer(t)
	if w.f.sets != sets {
		t.Errorf("the second offer wrote %d more statuses, want none", w.f.sets-sets)
	}
	if len(w.c.bodies) != 1 {
		t.Errorf("the second offer posted another notice (%d in all) — one pause, one notice", len(w.c.bodies))
	}
}

// Before the bound the pause is not news: no forge read, no write, no comment.
func TestGatePause_BeforeTheBoundNothingHappens(t *testing.T) {
	w := newPausedWorld(t, store.RunStatusPausedWaitingHuman, authFailedPause())
	w.at(defaultGatePauseAnswerAfter - time.Minute)
	w.offer(t)
	if w.f.sets != 0 || w.f.gets != 0 || len(w.c.bodies) != 0 {
		t.Fatalf("acted before the bound: %d writes, %d reads, %d comments", w.f.sets, w.f.gets, len(w.c.bodies))
	}
	if got := w.head(); !isGateInFlight(got) {
		t.Fatalf("head = %s %q, want the run's claim untouched", got.State, got.Description)
	}
}

// A pause at a human node names the node and asks for the answer.
func TestGatePause_AHumanNodeAsksForTheAnswer(t *testing.T) {
	w := newPausedWorld(t, store.RunStatusPausedWaitingHuman, &store.Checkpoint{NodeID: "escalate"})
	w.at(defaultGatePauseAnswerAfter)
	w.offer(t)
	if got, want := w.head().Description, "review paused (waiting for a human at escalate"+gatePausedHumanRemedy; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
	if len(w.c.bodies) != 1 || !strings.Contains(w.c.bodies[0], "answer the run's question") || strings.Contains(w.c.bodies[0], "recovery prompt") {
		t.Fatalf("notice does not ask for the human's answer: %v", w.c.bodies)
	}
}

// A check already answered — a real verdict, or another run's live claim —
// is left alone whatever the pause's age.
func TestGatePause_AnAnsweredCheckIsLeftAlone(t *testing.T) {
	for name, st := range map[string]forge.CommitStatus{
		"a real verdict": {State: forge.CommitStateSuccess, Context: pCtx, Description: "0 blocking findings"},
		"another run's claim": {State: forge.CommitStatePending, Context: pCtx, Description: gateInFlightDescription,
			TargetURL: gateRunURL("https://iterion.test", "run-other")},
	} {
		t.Run(name, func(t *testing.T) {
			w := newPausedWorld(t, store.RunStatusPausedWaitingHuman, authFailedPause())
			w.f.statuses["deadbeef|"+pCtx] = st
			w.at(defaultGatePauseAnswerAfter + time.Hour)
			w.offer(t)
			if w.f.sets != 0 || len(w.c.bodies) != 0 {
				t.Fatalf("answered a check that was not this run's to answer: %d writes, %d comments (%v)", w.f.sets, len(w.c.bodies), w.f.posted)
			}
		})
	}
}

// An operator's own pause is left alone: the operator knows.
func TestGatePause_AnOperatorPauseIsLeftAlone(t *testing.T) {
	w := newPausedWorld(t, store.RunStatusPausedOperator, nil)
	w.at(100 * time.Hour)
	w.offer(t)
	if w.f.sets != 0 || w.f.gets != 0 || len(w.c.bodies) != 0 {
		t.Fatalf("acted on an operator's pause: %d writes, %d reads, %d comments", w.f.sets, w.f.gets, len(w.c.bodies))
	}
}

// The bound is the operator's: a shorter one answers sooner, and zero turns
// the answer off however long the pause lasts.
func TestGatePause_TheOverrideIsHonoured(t *testing.T) {
	t.Run("shorter", func(t *testing.T) {
		w := newPausedWorld(t, store.RunStatusPausedWaitingHuman, authFailedPause())
		w.s.gatePauseAnswer = gatePauseAnswerSetting{after: 10 * time.Minute, configured: true}
		w.at(9 * time.Minute)
		w.offer(t)
		if w.f.sets != 0 {
			t.Fatalf("answered before the configured 10m bound")
		}
		w.at(11 * time.Minute)
		w.offer(t)
		if got := w.head(); got.State != forge.CommitStateFailure {
			t.Fatalf("head = %s %q at 11m, want the answer under a 10m bound", got.State, got.Description)
		}
	})
	t.Run("off", func(t *testing.T) {
		w := newPausedWorld(t, store.RunStatusPausedWaitingHuman, authFailedPause())
		w.s.gatePauseAnswer = gatePauseAnswerSetting{after: 0, configured: true}
		w.at(100 * time.Hour)
		w.offer(t)
		if w.f.sets != 0 || len(w.c.bodies) != 0 {
			t.Fatalf("answered with the bound off: %d writes, %d comments", w.f.sets, len(w.c.bodies))
		}
	})
}

// The synthetic failure is anchored at the pause, so the verdict the run
// publishes once resumed is the newer decision and replaces it.
func TestGatePause_TheResumedRunsVerdictReplacesTheFailure(t *testing.T) {
	w := newPausedWorld(t, store.RunStatusPausedWaitingHuman, authFailedPause())
	w.at(defaultGatePauseAnswerAfter + time.Minute)
	w.offer(t)
	if got := w.head(); got.State != forge.CommitStateFailure {
		t.Fatalf("head = %s %q, want the pause's failure first", got.State, got.Description)
	}
	if err := w.s.cfg.Store.UpdateRunStatus(context.Background(), w.runID, store.RunStatusRunning, ""); err != nil {
		t.Fatal(err)
	}
	w.at(defaultGatePauseAnswerAfter + 30*time.Minute)
	if r := w.publish(t, "tok-gate", pGate(0, "deadbeef")); !r.GatePosted {
		t.Fatalf("the resumed run's verdict was not posted: %+v", r)
	}
	if got := w.head(); got.State != forge.CommitStateSuccess {
		t.Fatalf("head = %s %q, want the resumed run's green verdict over the pause's failure (posted %v)", got.State, got.Description, w.f.posted)
	}
}

func TestGatePauseAnswerFromEnv(t *testing.T) {
	cases := []struct {
		raw    string
		want   time.Duration
		warned bool
	}{
		{"", defaultGatePauseAnswerAfter, false},
		{"30m", 30 * time.Minute, false},
		{"0", 0, true},
		{"-5m", 0, true},
		{"off", 0, true},
		{"OFF", 0, true},
		{"soon", defaultGatePauseAnswerAfter, true},
		{"240h", 240 * time.Hour, true}, // kept, with a warning: the grant may be gone
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			var warns []string
			setting := gatePauseAnswerFromEnv(func(name string) string {
				if name == EnvGatePauseAnswerAfter {
					return tc.raw
				}
				return ""
			}, func(format string, _ ...any) { warns = append(warns, format) })
			s := &Server{gatePauseAnswer: setting}
			if got := s.gatePauseAnswerAfter(); got != tc.want {
				t.Errorf("bound = %s, want %s", got, tc.want)
			}
			if (len(warns) > 0) != tc.warned {
				t.Errorf("warnings = %v, want warned=%v", warns, tc.warned)
			}
		})
	}
}

// A long node id never pushes the remedy past the 140 runes a forge keeps.
func TestGatePausedDescriptionKeepsTheRemedy(t *testing.T) {
	run := &store.Run{Checkpoint: &store.Checkpoint{NodeID: strings.Repeat("é", 200), RecoveryPause: true, RecoveryCode: "AUTH_FAILED"}}
	d := gatePausedDescription(run)
	if n := utf8.RuneCountInString(d); n > 140 {
		t.Fatalf("description is %d runes, want ≤ 140: %q", n, d)
	}
	if !strings.HasSuffix(d, gatePausedRecoveryRemedy) || !utf8.ValidString(d) {
		t.Fatalf("description lost its remedy or its encoding: %q", d)
	}
}
