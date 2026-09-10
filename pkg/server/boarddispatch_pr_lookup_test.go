package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

func boardPRLookupFixture(t *testing.T, gc forgeGateClient) (*Server, *countingPublisher, *fakeBoardCoord) {
	t.Helper()
	s := prLaunchGuardFixture(t, gc)
	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "probe.bot"), []byte(boardGateProbeBot), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub := &countingPublisher{onLaunch: func(id string) { finishRunAs(t, rs, id, store.RunStatusFinished) }}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	card := readyCard("native:pr", "probe")
	card.Tenant = "team1"
	card.Issue.BotArgs = map[string]string{"pr_url": "https://github.com/o/r/pull/7"}
	return s, pub, newFakeBoardCoord(card)
}

// Exercise the real forge guard and dispatcher together: a never-launched
// card must be eligible again after the forge recovers, with the existing
// durable backoff between attempts rather than a terminal blocked verdict.
func TestBoardDispatcher_PRLookupFailureRetriesAfterRecovery(t *testing.T) {
	gc := &fakeGateClient{getErr: errors.New("forge unavailable: 502")}
	s, pub, f := boardPRLookupFixture(t, gc)
	d := newBoardDispatcher(f, s.processBoardCard, "replica-A", 1, iterlog.Nop())
	ctx := boundedCtx(t)
	d.tick(ctx)
	d.wg.Wait()
	if got := f.states["native:pr"]; got != native.StateReady {
		t.Fatalf("failed PR lookup left card %q, want ready", got)
	}
	if pub.count() != 0 || len(f.claimed) != 0 {
		t.Fatalf("failed lookup: launches=%d claims=%v, want no launch and released claim", pub.count(), f.claimed)
	}
	ledger := f.refusals["native:pr"]
	if ledger == nil || ledger.Attempts != 1 || !ledger.NotBefore.After(time.Now()) || !strings.Contains(ledger.LastReason, "forge unavailable") {
		t.Fatalf("lookup retry ledger = %+v, want one delayed attempt naming the failure", ledger)
	}
	if f.reasons["native:pr"] != tracker.ReasonLaunchRefused {
		t.Fatalf("give-back reason = %q, want launch_refused", f.reasons["native:pr"])
	}
	if n := d.tick(ctx); n != 0 {
		d.wg.Wait()
		t.Fatalf("claimed %d cards during the lookup backoff", n)
	}

	gc.getErr = nil
	f.mu.Lock()
	f.refusals["native:pr"].NotBefore = time.Now().Add(-time.Second)
	f.mu.Unlock()
	d.tick(ctx)
	d.wg.Wait()
	if pub.count() != 1 || f.states["native:pr"] != native.StateDone || len(f.claimed) != 0 {
		t.Fatalf("recovered lookup: launches=%d state=%q claims=%v, want one finished run", pub.count(), f.states["native:pr"], f.claimed)
	}
}

func TestBoardDispatcher_PRLookupForkRemainsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		gc   *fakeGateClient
	}{
		{"fork", &fakeGateClient{headRepo: "someone/r"}},
		{"withheld head", &fakeGateClient{noHeadRepo: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, pub, f := boardPRLookupFixture(t, tc.gc)
			d := newBoardDispatcher(f, s.processBoardCard, "replica-A", 1, iterlog.Nop())
			d.tick(boundedCtx(t))
			d.wg.Wait()
			if f.states["native:pr"] != native.StateBlocked || pub.count() != 0 || len(f.claimed) != 0 {
				t.Fatalf("fork refusal: state=%q launches=%d claims=%v", f.states["native:pr"], pub.count(), f.claimed)
			}
			if f.refusals["native:pr"] != nil {
				t.Fatal("a proven fork must not consume a transient retry budget")
			}
		})
	}
}

func TestBoardDispatcher_PRLookupFailureHonoursAttemptCap(t *testing.T) {
	s, pub, f := boardPRLookupFixture(t, &fakeGateClient{getErr: errors.New("forge unavailable")})
	f.cands[0].Issue.LaunchRefusal = &native.LaunchRefusal{Attempts: 2}
	d := newBoardDispatcher(f, s.processBoardCard, "replica-A", 1, iterlog.Nop())
	d.launchAttemptCap = 3
	d.tick(boundedCtx(t))
	d.wg.Wait()
	g := f.gaveUps["native:pr"]
	if f.states["native:pr"] != native.StateBlocked || f.reasons["native:pr"] != tracker.ReasonLaunchGivenUp || g == nil || g.Attempts != 3 || !g.Launch {
		t.Fatalf("lookup attempt cap: state=%q reason=%q give-up=%+v", f.states["native:pr"], f.reasons["native:pr"], g)
	}
	if pub.count() != 0 || len(f.claimed) != 0 {
		t.Fatalf("lookup give-up: launches=%d claims=%v, want neither", pub.count(), f.claimed)
	}
}

func TestBoardDispatcher_PRLookupForeignGrantRemainsTerminal(t *testing.T) {
	s, pub, f := boardPRLookupFixture(t, &fakeGateClient{})
	registerPublishToken(t, s, "foreign", ForgePublishGrant{TeamID: "another-team", ConnectionID: "conn1", Repo: "o/r"})
	f.cands[0].Issue.BotArgs[forgePublishVarToken] = "foreign"
	d := newBoardDispatcher(f, s.processBoardCard, "replica-A", 1, iterlog.Nop())
	d.tick(boundedCtx(t))
	d.wg.Wait()
	if f.states["native:pr"] != native.StateBlocked || pub.count() != 0 || f.refusals["native:pr"] != nil {
		t.Fatalf("foreign grant: state=%q launches=%d ledger=%+v, want terminal refusal", f.states["native:pr"], pub.count(), f.refusals["native:pr"])
	}
}

type drainingPRLookupClient struct {
	fakeGateClient
	cancel context.CancelFunc
}

func (g *drainingPRLookupClient) GetPullRequest(ctx context.Context, _ string, _ int) (forge.PullRef, error) {
	g.cancel()
	<-ctx.Done()
	return forge.PullRef{}, ctx.Err()
}

func TestBoardDispatcher_PRLookupDrainReturnsUnlaunchedCard(t *testing.T) {
	ctx, cancel := context.WithCancel(boundedCtx(t))
	defer cancel()
	s, pub, f := boardPRLookupFixture(t, &drainingPRLookupClient{cancel: cancel})
	d := newBoardDispatcher(f, s.processBoardCard, "replica-A", 1, iterlog.Nop())
	d.tick(ctx)
	d.wg.Wait()
	if f.states["native:pr"] != native.StateReady || pub.count() != 0 || len(f.claimed) != 0 {
		t.Fatalf("drained lookup: state=%q launches=%d claims=%v, want ready without a run or claim", f.states["native:pr"], pub.count(), f.claimed)
	}
	if f.refusals["native:pr"] != nil {
		t.Fatal("a replica drain before launch must consume no attempt")
	}
}
