package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A launch the run service REFUSES before any run exists — a sealing
// failure, a queue outage, a bot that does not compile — is not a verdict on
// the card (#814). The card goes back to its column under machine
// provenance, its retry is bounded by a backoff + an attempt cap, and only a
// permanently refused card is filed blocked — with the reason, and reflected
// on purpose.

func refusedLaunch(iss native.Issue) error {
	return &launchRefusal{cardID: iss.ID, cause: errors.New("cloudpublisher: seal run bundle: kms unavailable")}
}

// TestBoardDispatcher_LaunchRefusalGivesTheCardBack: one refusal → the card
// is back in `ready` under ReasonLaunchRefused (machine: no chain fires, no
// reflect), its ledger reads one attempt with a NotBefore in the future, the
// claim is freed, the next tick does NOT re-claim it, and the refusal is
// said once and counted.
func TestBoardDispatcher_LaunchRefusalGivesTheCardBack(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	var buf bytes.Buffer
	d := newBoardDispatcher(f, func(_ context.Context, _ string, iss native.Issue) error {
		return refusedLaunch(iss)
	}, "replica-A", 4, iterlog.New(iterlog.LevelWarn, &buf))
	d.launchAttemptCap = 3

	before := time.Now()
	for i := 0; i < 2; i++ {
		d.tick(context.Background())
		d.wg.Wait()
	}
	if got := f.states["native:1"]; got != native.StateReady {
		t.Errorf("a refused launch ended the card in %q, want %q — the column it was taken from", got, native.StateReady)
	}
	if got := f.reasons["native:1"]; got != tracker.ReasonLaunchRefused {
		t.Errorf("the give-back carried provenance %q, want %q", got, tracker.ReasonLaunchRefused)
	}
	rec := f.refusals["native:1"]
	if rec == nil || rec.Attempts != 1 || !rec.NotBefore.After(before) || !strings.Contains(rec.LastReason, "kms unavailable") {
		t.Fatalf("launch-refusal ledger = %+v, want attempts=1, a NotBefore in the future and the refusal as reason", rec)
	}
	if f.claimCalls != 1 {
		t.Errorf("Claim was attempted %d time(s) over 2 ticks, want 1 — a card inside its backoff is not listed", f.claimCalls)
	}
	if len(f.claimed) != 0 {
		t.Errorf("the claim must be freed after the give-back: %v", f.claimed)
	}
	if got := d.unlaunchableCounts(); got.launchRefused != 1 || got.launchGivenUp != 0 {
		t.Errorf("tally = %+v, want 1 launch refused, 0 given up", got)
	}
	if got := linesMentioning(buf.String(), "native:1"); got != 1 {
		t.Errorf("the refusal was logged %d time(s), want exactly 1:\n%s", got, buf.String())
	}
	if _, stamped := f.gaveUps["native:1"]; stamped {
		t.Error("a single refusal stamped a give-up — that is the attempt cap's job")
	}
}

// TestBoardDispatcher_LaunchRefusalIsBounded: the refusal that reaches the
// attempt cap files the card blocked under the DESCRIPTIVE
// ReasonLaunchGivenUp (reflected on purpose — a human decides now), with the
// last refusal readable on the card and a give-up stamp naming it.
func TestBoardDispatcher_LaunchRefusalIsBounded(t *testing.T) {
	card := readyCard("native:1", "feature-dev")
	card.Issue.LaunchRefusal = &native.LaunchRefusal{
		Attempts: 2, LastAt: time.Now().Add(-time.Hour), NotBefore: time.Now().Add(-time.Minute), LastReason: "earlier refusal",
	}
	f := newFakeBoardCoord(card)
	d := newBoardDispatcher(f, func(_ context.Context, _ string, iss native.Issue) error {
		return refusedLaunch(iss)
	}, "replica-A", 4, iterlog.Nop())
	d.launchAttemptCap = 3

	d.tick(context.Background())
	d.wg.Wait()
	if got := f.states["native:1"]; got != native.StateBlocked {
		t.Fatalf("after the attempt cap the card is %q, want %q — a permanently refused launch has to surface", got, native.StateBlocked)
	}
	if got := f.reasons["native:1"]; got != tracker.ReasonLaunchGivenUp {
		t.Errorf("the filing carried provenance %q, want %q (descriptive — the roadmap must show it)", got, tracker.ReasonLaunchGivenUp)
	}
	if tracker.IsMachineReason(tracker.ReasonLaunchGivenUp) {
		t.Error("launch_given_up must not be machine provenance — the operator's board must show the card")
	}
	rec := f.refusals["native:1"]
	if rec == nil || rec.Attempts != 3 || !strings.Contains(rec.LastReason, "kms unavailable") {
		t.Fatalf("ledger = %+v, want attempts=3 with the last refusal", rec)
	}
	g := f.gaveUps["native:1"]
	if g == nil || g.State != native.StateBlocked || g.Attempts != 3 || !strings.Contains(g.Reason, "kms unavailable") {
		t.Fatalf("give-up stamp = %+v, want blocked/3 attempts/the last refusal", g)
	}
	if len(f.claimed) != 0 {
		t.Errorf("the claim must be freed after the filing: %v", f.claimed)
	}
	if got := d.unlaunchableCounts(); got.launchGivenUp != 1 {
		t.Errorf("tally = %+v, want 1 given up", got)
	}
}

// TestBoardDispatcher_DrainingLaunchRefusalConsumesNoAttempt: the server
// draining refuses every launch, which says nothing about the card — it is
// given back with no ledger advance, so another replica claims it on its
// next tick with a clean count.
func TestBoardDispatcher_DrainingLaunchRefusalConsumesNoAttempt(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	d := newBoardDispatcher(f, func(_ context.Context, _ string, iss native.Issue) error {
		return &launchRefusal{cardID: iss.ID, cause: fmt.Errorf("runview: %w", runtime.ErrServerDraining)}
	}, "replica-A", 4, iterlog.Nop())

	d.tick(context.Background())
	d.wg.Wait()
	if got := f.states["native:1"]; got != native.StateReady {
		t.Errorf("state = %q, want %q", got, native.StateReady)
	}
	if got := f.reasons["native:1"]; got != tracker.ReasonLaunchRefused {
		t.Errorf("provenance = %q, want %q", got, tracker.ReasonLaunchRefused)
	}
	if rec := f.refusals["native:1"]; rec != nil {
		t.Errorf("a draining refusal advanced the ledger to %+v — it must consume no attempt", rec)
	}
}

// TestBoardDispatcher_RunFailureStillFilesBlocked pins what did NOT change:
// a run that started and failed is a verdict on the card — blocked, under
// no machine reason, reflected as before.
func TestBoardDispatcher_RunFailureStillFilesBlocked(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		return errors.New("run r1 ended failed")
	}, "replica-A", 4, iterlog.Nop())
	d.tick(context.Background())
	d.wg.Wait()
	if got := f.states["native:1"]; got != native.StateBlocked {
		t.Errorf("a failed run's card is %q, want %q", got, native.StateBlocked)
	}
	if got := f.reasons["native:1"]; got != "" {
		t.Errorf("a run failure carried provenance %q, want none — the roadmap follows a run's verdict", got)
	}
	if rec := f.refusals["native:1"]; rec != nil {
		t.Errorf("a run failure touched the launch-refusal ledger: %+v", rec)
	}
}

// TestBoardDispatcher_DispatchListingHonoursTheBackoff: a card inside its
// NotBefore is not a candidate; once the instant has passed it is listed
// again with its ledger on the candidate (the fake mirrors the Mongo query;
// the real one is pinned by the conformance suite).
func TestBoardDispatcher_DispatchListingHonoursTheBackoff(t *testing.T) {
	card := readyCard("native:1", "feature-dev")
	card.Issue.LaunchRefusal = &native.LaunchRefusal{Attempts: 1, NotBefore: time.Now().Add(time.Hour)}
	f := newFakeBoardCoord(card)
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		t.Error("a card inside its backoff must not be dispatched")
		return nil
	}, "replica-A", 4, iterlog.Nop())
	if n := d.tick(context.Background()); n != 0 {
		t.Fatalf("tick claimed %d card(s) inside the backoff, want 0", n)
	}
	d.wg.Wait()
}

// refusingPublisher is a cloud publisher whose every SubmitLaunch is refused
// — the shape a sealing failure or a queue outage takes at the run service.
type refusingPublisher struct{ err error }

func (p refusingPublisher) SubmitLaunch(context.Context, string, runview.LaunchSpec, *ir.Workflow, string) (int, error) {
	return 0, p.err
}
func (refusingPublisher) CancelRun(context.Context, string) error { return nil }
func (refusingPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (refusingPublisher) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, string) error {
	return nil
}

// TestProcessBoardCard_LaunchRefusalIsTyped pins the taxonomy at its source:
// an error out of runs.Launch — here the publisher refusing the launch — is
// errCardLaunchRefused, never a run failure, so processCard gives the card
// back instead of parking it.
func TestProcessBoardCard_LaunchRefusalIsTyped(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "probe.bot"), []byte(
		"schema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"yes\"}'`\n  output: probe_out\n\nworkflow board_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := runview.NewService("", runview.WithStore(rs),
		runview.WithLaunchPublisher(refusingPublisher{err: errors.New("cloudpublisher: seal run bundle: kms unavailable")}))
	if err != nil {
		t.Fatal(err)
	}
	s.runs = svc
	err = s.processBoardCard(context.Background(), "team1", native.Issue{ID: "native:1", Bot: "probe"})
	if !errors.Is(err, errCardLaunchRefused) {
		t.Fatalf("processBoardCard = %v, want errCardLaunchRefused — no run started, no verdict belongs on the card", err)
	}
	if errors.Is(err, errCardUnlaunchable) {
		t.Fatal("a launch refusal must not read as a precondition failure: it is transient and retried with a backoff")
	}
	if !strings.Contains(err.Error(), "kms unavailable") {
		t.Fatalf("the refusal lost the publisher's reason: %v", err)
	}
	_ = boardmongo.Candidate{}
}
