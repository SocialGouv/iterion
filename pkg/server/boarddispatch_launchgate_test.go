package server

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The board dispatcher launches through the SAME admission gate as every
// other launch surface (#841): suspend → concurrency → launch rate → monthly
// caps, metered like an HTTP launch and rolled back when the run service then
// refuses. A denial is a launch refusal of the ordinary class (#814): the card
// goes back to its column under machine provenance, the rule that refused it
// on the ledger, and the retry waits out the backoff.

// countingPublisher accepts or refuses every launch and counts the attempts —
// the oracle for "the run service was never asked" on a gate denial. onLaunch
// runs with the run id the service chose, so a test can materialise the run
// the way a runner pod would.
type countingPublisher struct {
	mu       sync.Mutex
	launches int
	err      error
	onLaunch func(runID string)
}

func (p *countingPublisher) SubmitLaunch(_ context.Context, runID string, _ runview.LaunchSpec, _ *ir.Workflow, _ string) (int, error) {
	p.mu.Lock()
	p.launches++
	p.mu.Unlock()
	if p.onLaunch != nil {
		p.onLaunch(runID)
	}
	return 0, p.err
}
func (p *countingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.launches
}
func (*countingPublisher) CancelRun(context.Context, string) error { return nil }
func (*countingPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (*countingPublisher) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, string) error {
	return nil
}

const boardGateProbeBot = "schema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"yes\"}'`\n  output: probe_out\n\nworkflow board_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n"

// newGatedBoardServer is a cloud-shaped server: an identity store with one
// org+team seeded per spec (so the gate has a team to read), a bot catalog
// holding the tool-only probe bot, and a run service whose publisher is the
// caller's. The run store is returned so a test can play the runner pod.
func newGatedBoardServer(t *testing.T, spec gateSpec, pub *countingPublisher) (*Server, *store.FilesystemRunStore) {
	t.Helper()
	s := newOrgTestServer(t)
	seedGate(t, s, spec)
	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "probe.bot"), []byte(boardGateProbeBot), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := runview.NewService("", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	if err != nil {
		t.Fatal(err)
	}
	s.runs = svc
	return s, rs
}

// TestProcessBoardCard_GateDenialIsALaunchRefusal: an org at its concurrency
// cap — the run service is never asked, the error is the launch-refusal class
// carrying the gate's typed denial, and the reason that reaches the card's
// ledger names the rule.
func TestProcessBoardCard_GateDenialIsALaunchRefusal(t *testing.T) {
	pub := &countingPublisher{}
	s, rs := newGatedBoardServer(t, gateSpec{id: "t1", maxConcurrentRuns: 1}, pub)
	s.cfg.Store = fakeActiveStore{RunStore: rs, active: 1}

	err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{ID: "native:1", Bot: "probe", State: native.StateReady})
	if !errors.Is(err, errCardLaunchRefused) {
		t.Fatalf("processBoardCard = %v, want errCardLaunchRefused — a gate denial launches nothing, so no verdict belongs on the card", err)
	}
	var deny *launchDeniedError
	if !errors.As(err, &deny) || deny.Reason != denyConcurrencyCap {
		t.Fatalf("the refusal does not carry the gate's typed denial: %v", err)
	}
	if got := pub.count(); got != 0 {
		t.Errorf("the run service was asked %d time(s) after a gate denial, want 0", got)
	}
	if reason := launchRefusalReason(err); !strings.Contains(reason, denyConcurrencyCap) {
		t.Errorf("ledger reason = %q, want it to name the gate rule %q", reason, denyConcurrencyCap)
	}
}

// TestProcessBoardCard_AdmittedLaunchIsMetered: the board's launch consumes
// a monthly run slot exactly like an HTTP launch — and only when the run
// service accepted it; a refused launch hands the slot back (the HTTP
// handler's rollback, on this surface too).
func TestProcessBoardCard_AdmittedLaunchIsMetered(t *testing.T) {
	t.Run("refused launch releases the slot", func(t *testing.T) {
		pub := &countingPublisher{err: errors.New("cloudpublisher: seal run bundle: kms unavailable")}
		s, _ := newGatedBoardServer(t, gateSpec{id: "t1", orgRunQuota: 3}, pub)
		counter := orgusage.NewMemoryCounter()
		s.orgUsage = counter
		err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{ID: "native:1", Bot: "probe", State: native.StateReady})
		if !errors.Is(err, errCardLaunchRefused) {
			t.Fatalf("processBoardCard = %v, want errCardLaunchRefused", err)
		}
		if got := pub.count(); got != 1 {
			t.Fatalf("the run service was asked %d time(s), want 1 — the gate admitted this launch", got)
		}
		u, _ := counter.Usage(context.Background(), "t1", time.Now().UTC())
		if u.Runs != 0 {
			t.Errorf("monthly runs = %d after a refused launch, want 0 — a run that never started must not consume a slot", u.Runs)
		}
	})
	t.Run("accepted launch keeps the slot", func(t *testing.T) {
		pub := &countingPublisher{}
		s, rs := newGatedBoardServer(t, gateSpec{id: "t1", orgRunQuota: 3}, pub)
		counter := orgusage.NewMemoryCounter()
		s.orgUsage = counter
		pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
		if err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{ID: "native:1", Bot: "probe", State: native.StateReady}); err != nil {
			t.Fatalf("processBoardCard = %v, want nil for a finished run", err)
		}
		u, _ := counter.Usage(context.Background(), "t1", time.Now().UTC())
		if u.Runs != 1 {
			t.Errorf("monthly runs = %d after an accepted launch, want 1", u.Runs)
		}
	})
}

// boundedCtx keeps a launch that WAS admitted from polling a run nobody
// finishes for the test's whole timeout: a pre-fix run fails on its
// assertion instead of hanging.
func boundedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// finishRunAs plays the runner pod for a cloud launch: the publisher owns the
// run record in production, so the test materialises it in the service's
// store at the given terminal status and the dispatcher's poll sees it end.
func finishRunAs(t *testing.T, rs *store.FilesystemRunStore, runID string, st store.RunStatus) {
	t.Helper()
	ctx := context.Background()
	if _, err := rs.CreateRun(ctx, runID, "board_probe", nil); err != nil {
		t.Fatalf("CreateRun(%s): %v", runID, err)
	}
	run, err := rs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun(%s): %v", runID, err)
	}
	run.Status = st
	if err := rs.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun(%s): %v", runID, err)
	}
}

// TestBoardDispatcher_GateDenialKeepsTheCardReadyUntilTheCapFrees, end to
// end through the dispatcher: an org at its concurrency cap → the card stays
// `ready`, no run, machine provenance, the rule on its ledger, one Warn; the
// cap frees → the next eligible tick launches it.
func TestBoardDispatcher_GateDenialKeepsTheCardReadyUntilTheCapFrees(t *testing.T) {
	pub := &countingPublisher{}
	s, rs := newGatedBoardServer(t, gateSpec{id: "t1", maxConcurrentRuns: 1}, pub)
	s.cfg.Store = fakeActiveStore{RunStore: rs, active: 1}
	f := newFakeBoardCoord(readyCard("native:1", "probe"))
	var buf bytes.Buffer
	d := newBoardDispatcher(f, s.processBoardCard, "replica-A", 4, iterlog.New(iterlog.LevelWarn, &buf))

	before := time.Now()
	d.tick(boundedCtx(t))
	d.wg.Wait()
	if got := f.states["native:1"]; got != native.StateReady {
		t.Errorf("a gate denial ended the card in %q, want %q — the column it was taken from", got, native.StateReady)
	}
	if got := f.reasons["native:1"]; got != tracker.ReasonLaunchRefused {
		t.Errorf("the give-back carried provenance %q, want %q", got, tracker.ReasonLaunchRefused)
	}
	rec := f.refusals["native:1"]
	if rec == nil || rec.Attempts != 1 || !rec.NotBefore.After(before) || !strings.Contains(rec.LastReason, denyConcurrencyCap) {
		t.Fatalf("launch-refusal ledger = %+v, want attempts=1, a NotBefore in the future and the gate rule as reason", rec)
	}
	if got := pub.count(); got != 0 {
		t.Errorf("the run service was asked %d time(s) while the org was at its cap, want 0", got)
	}
	if len(f.claimed) != 0 {
		t.Errorf("the claim must be freed after the give-back: %v", f.claimed)
	}
	if _, stamped := f.gaveUps["native:1"]; stamped {
		t.Error("a single denial stamped a give-up — that is the attempt cap's job")
	}
	if got := linesMentioning(buf.String(), "native:1"); got != 1 {
		t.Errorf("the denial was logged %d time(s), want exactly 1:\n%s", got, buf.String())
	}

	// The cap frees and the backoff elapses: the next tick claims the card
	// again and this time the run service is asked.
	s.cfg.Store = fakeActiveStore{RunStore: rs, active: 0}
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	f.mu.Lock()
	f.refusals["native:1"].NotBefore = time.Now().Add(-time.Second)
	f.mu.Unlock()
	d.tick(boundedCtx(t))
	d.wg.Wait()
	if got := pub.count(); got != 1 {
		t.Fatalf("the run service was asked %d time(s) once the cap freed, want 1", got)
	}
	if f.claimCalls != 2 {
		t.Errorf("Claim was attempted %d time(s) over the two ticks, want 2", f.claimCalls)
	}
	if got := f.states["native:1"]; got != d.doneState {
		t.Errorf("after the launch the card is %q, want %q", got, d.doneState)
	}
}
