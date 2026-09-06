package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The cloud board dispatcher must not claim what it cannot launch (#798).
//
// A `ready` card with no bot is roadmap content, not dispatchable work — the
// cloud dispatcher has no default bot — and a project-board sync lands every
// "Planned" ticket in `ready`. The prod shape: 36 such cards were claimed,
// each failed "card has no bot", each was parked `blocked`, and the reflect
// pushed Blocked onto 30 roadmap tickets as if a human had moved them.

// TestBoardDispatcher_NeverClaimsACardWithoutABot pins the choke point: the
// tick's candidate query excludes a bot-less card, so it is never claimed,
// launched or moved.
func TestBoardDispatcher_NeverClaimsACardWithoutABot(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:roadmap", ""), readyCard("native:work", "feature-dev"))
	var pmu sync.Mutex
	processed := map[string]int{}
	d := newBoardDispatcher(f, func(_ context.Context, _ string, iss native.Issue) error {
		pmu.Lock()
		processed[iss.ID]++
		pmu.Unlock()
		if iss.Bot == "" {
			return fmt.Errorf("card %s has no bot", iss.ID)
		}
		return nil
	}, "replica-A", 4, iterlog.Nop())

	if n := d.tick(context.Background()); n != 1 {
		t.Fatalf("tick claimed %d card(s), want 1 — only the card that names a bot is dispatchable", n)
	}
	d.wg.Wait()
	if f.claimCalls != 1 {
		t.Errorf("Claim was attempted %d time(s), want 1 — a bot-less card must never be claimed", f.claimCalls)
	}
	if processed["native:roadmap"] != 0 {
		t.Errorf("the bot-less card was processed %d time(s), want 0", processed["native:roadmap"])
	}
	if got, moved := f.states["native:roadmap"]; moved {
		t.Errorf("the bot-less card was moved to %q, want it left in %q untouched", got, native.StateReady)
	}
	if f.states["native:work"] != native.StateDone {
		t.Errorf("the launchable card ended %q, want %q", f.states["native:work"], native.StateDone)
	}
}

// TestBoardDispatcher_AdmissionRefusesBeforeTheClaim: the launch
// preconditions are checked BEFORE the claim (the local dispatcher's
// resolveExplicitBot shape), so a card that cannot be launched is skipped in
// its column — no claim, no in_progress move, no give-back — and the refusal
// is said once per (card, reason), not once per 5s tick.
func TestBoardDispatcher_AdmissionRefusesBeforeTheClaim(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:ghost", "ghost-bot"))
	var buf bytes.Buffer
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		t.Error("a card refused at admission must never reach process")
		return nil
	}, "replica-A", 4, iterlog.New(iterlog.LevelWarn, &buf))
	d.admit = func(_ context.Context, _ string, iss native.Issue) error {
		return fmt.Errorf("card %s names bot %q which cannot be resolved: %w", iss.ID, iss.Bot, errCardUnlaunchable)
	}

	for i := 0; i < 3; i++ {
		if n := d.tick(context.Background()); n != 0 {
			t.Fatalf("tick %d claimed %d card(s), want 0", i, n)
		}
	}
	d.wg.Wait()
	if f.claimCalls != 0 {
		t.Errorf("Claim was attempted %d time(s), want 0 — admission runs before the claim", f.claimCalls)
	}
	if got, moved := f.states["native:ghost"]; moved {
		t.Errorf("a card refused at admission was moved to %q — it must keep its column", got)
	}
	if got := linesMentioning(buf.String(), "native:ghost"); got != 1 {
		t.Errorf("the refusal was logged %d time(s) over 3 ticks, want exactly 1 (once per card+reason edge):\n%s", got, buf.String())
	}
	if got := d.unlaunchableCounts(); got.refused != 3 || got.released != 0 {
		t.Errorf("unlaunchable counts = %+v, want 3 refused at admission, 0 released after claim", got)
	}
}

// TestBoardDispatcher_UnlaunchableCardKeepsItsColumn is the belt under the
// choke point: when a precondition still fails AFTER the claim (the card
// changed between the listing and the launch), nothing ran, so no verdict
// belongs on the card. It goes back to the column the tick took it from —
// under machine provenance, so the return re-fires no subscription and is
// not projected as a move — and the claim is freed. Never `blocked`.
func TestBoardDispatcher_UnlaunchableCardKeepsItsColumn(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	var buf bytes.Buffer
	d := newBoardDispatcher(f, func(_ context.Context, _ string, iss native.Issue) error {
		return fmt.Errorf("card %s has no bot: %w", iss.ID, errCardUnlaunchable)
	}, "replica-A", 4, iterlog.New(iterlog.LevelWarn, &buf))

	for i := 0; i < 2; i++ {
		d.tick(context.Background())
		d.wg.Wait()
	}
	if got := f.states["native:1"]; got != native.StateReady {
		t.Errorf("an unlaunchable card ended in %q, want %q — the column it was taken from, never blocked", got, native.StateReady)
	}
	if got := f.reasons["native:1"]; got != tracker.ReasonUnlaunchable {
		t.Errorf("the give-back carried provenance %q, want %q (machine — no chain fires, no reflect)", got, tracker.ReasonUnlaunchable)
	}
	if len(f.claimed) != 0 {
		t.Errorf("the claim must be freed after the give-back: %v", f.claimed)
	}
	if got := linesMentioning(buf.String(), "native:1"); got != 1 {
		t.Errorf("the give-back was logged %d time(s) over 2 ticks, want exactly 1:\n%s", got, buf.String())
	}
	if got := d.unlaunchableCounts(); got.released != 2 {
		t.Errorf("unlaunchable counts = %+v, want 2 released after claim", got)
	}
}

// TestProcessBoardCard_PreconditionsAreUnlaunchable pins the taxonomy at its
// source: every failure that happens BEFORE a run exists — no bot, an
// unresolvable bot, malformed reserved bot-args, no run service — is
// errCardUnlaunchable, so processCard gives the card back instead of parking
// it. The publisher refusing an actual launch is deliberately NOT in this
// class: that is a verdict on the launch, and keeps its filing.
func TestProcessBoardCard_PreconditionsAreUnlaunchable(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.Bots.Paths = []string{t.TempDir()}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := runview.NewService("", runview.WithStore(rs))
	if err != nil {
		t.Fatal(err)
	}
	s.runs = svc
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		iss  native.Issue
	}{
		{"no bot", native.Issue{ID: "native:nobot"}},
		{"unresolvable bot", native.Issue{ID: "native:ghost", Bot: "no-such-bot-anywhere"}},
		{"malformed reserved bot-args", native.Issue{ID: "native:bad", Bot: "no-such-bot-anywhere",
			BotArgs: map[string]string{boardKeyOverridesKey: "{not json"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.processBoardCard(ctx, "team1", tc.iss)
			if !errors.Is(err, errCardUnlaunchable) {
				t.Fatalf("processBoardCard = %v, want errCardUnlaunchable — nothing ran, no verdict belongs on the card", err)
			}
		})
	}
	t.Run("run service unwired", func(t *testing.T) {
		s.runs = nil
		if err := s.processBoardCard(ctx, "team1", native.Issue{ID: "native:x", Bot: "b"}); !errors.Is(err, errCardUnlaunchable) {
			t.Fatalf("processBoardCard = %v, want errCardUnlaunchable", err)
		}
	})
}

// linesMentioning counts the LOG LINES naming a card — one line names it
// twice (the dispatcher's own prefix and the wrapped error), so a substring
// count would read one warn as two.
func linesMentioning(log, needle string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

// TestServerAdmitBoardCard: the pre-claim admission shares processBoardCard's
// precondition set — one helper, so the two cannot drift — and refuses with
// the same sentinel.
func TestServerAdmitBoardCard(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.Bots.Paths = []string{t.TempDir()}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := runview.NewService("", runview.WithStore(rs))
	if err != nil {
		t.Fatal(err)
	}
	s.runs = svc
	if err := s.admitBoardCard(context.Background(), "team1", native.Issue{ID: "native:nobot"}); !errors.Is(err, errCardUnlaunchable) {
		t.Fatalf("admit(no bot) = %v, want errCardUnlaunchable", err)
	}
	if err := s.admitBoardCard(context.Background(), "team1", native.Issue{ID: "native:ghost", Bot: "no-such-bot"}); !errors.Is(err, errCardUnlaunchable) {
		t.Fatalf("admit(unresolvable bot) = %v, want errCardUnlaunchable", err)
	}
}
