package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/retryprobetest"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// The sweeper's half of the probe's guarantee, over random sequences.
//
// Same shape as the runner's property next door (pkg/runner), on the other
// three actors of the same row: the pull-forward pass, the due pass with its
// settlement branches, and the pull request's pause notice. Every step is the
// REAL pass over the modelled row, with the invariants of
// internal/retryprobetest asserted after each one — including the two the
// sweeper alone can break: a probe's resume spending the org's month or the
// minute's launch token, and a settlement retiring a run days before the wall
// it waits on falls.

const (
	probeSequenceTenant = "team-1"
	probeSequenceRunID  = "run-2247"
	probeSequenceBot    = "bots/feed-watch/main.bot"
	probeSequenceBudget = 3
)

// modelRetryLister is the scan over the modelled row: the same two filters
// the Mongo store applies, so a row the real scan would not list is not
// handed to the pass here either.
type modelRetryLister struct{ st *retryprobetest.Store }

func (l *modelRetryLister) ListRunsDueForRetry(_ context.Context, before time.Time, limit int) ([]mongostore.RetryDueRef, error) {
	return probeRefs(l.st.DueRows(before, limit)), nil
}

func (l *modelRetryLister) ListRunsForRetryProbe(_ context.Context, now time.Time, floor time.Duration, maxProbes, limit int) ([]mongostore.RetryDueRef, error) {
	return probeRefs(l.st.ProbeRows(now, floor, maxProbes, limit)), nil
}

func probeRefs(rows []*store.RunRetryState) []mongostore.RetryDueRef {
	out := make([]mongostore.RetryDueRef, 0, len(rows))
	for _, row := range rows {
		out = append(out, mongostore.RetryDueRef{
			ID:         probeSequenceRunID,
			TenantID:   probeSequenceTenant,
			OwnerID:    "user-1",
			FilePath:   probeSequenceBot,
			RetryState: row,
		})
	}
	return out
}

// probeSequenceWorld is one sequence's world: the modelled row, the real
// Server whose passes act on it, the admission counter the resume is metered
// against, and the pull request that hears about the pause.
type probeSequenceWorld struct {
	st       *retryprobetest.Store
	s        *Server
	lister   *modelRetryLister
	resumer  *fakeResumer
	counter  *orgusage.MemoryCounter
	notice   *Server
	comments *stubCommenter
	noticed  *store.Run
	t        *testing.T
	// armedWhenNoticed is the number of armings the row had taken when the
	// pause notice last fired. The notice is driven by the run-outcome
	// EVENT, so one park is one notice: a second notice with no arming in
	// between is an event production never emits.
	armedWhenNoticed int
}

// newProbeSequenceServer is newMeteredRetrySweeperServer with the launch RATE
// bounded to one a minute.
//
// That bound is how the third thing a probe must not spend becomes
// observable: the token is internal to the gate, but a token a probe took and
// never gave back denies the NEXT launch of the same minute, and the sequence
// then shows a due row that nothing resumed.
func newProbeSequenceServer(t *testing.T, st store.RunStore) (*Server, *orgusage.MemoryCounter) {
	t.Helper()
	s := newOrgTestServer(t)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	seedGate(t, s, gateSpec{id: probeSequenceTenant, launchRatePerMin: 1})
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bots", "feed-watch"), 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bots", "feed-watch", "main.bot"), []byte("workflow w:\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	s.cfg.Store, s.cfg.WorkDir, s.cfg.Metrics = st, dir, metrics.New()
	return s, counter
}

// newProbeNoticeServer wires the forge side the pause notice needs: a
// connection, a publish grant for the run's repo, the bot catalogue the role
// is derived from, and a commenter that keeps what was said.
//
// Shared across sequences (it holds no row state) with the comments reset
// between them — standing it up walks the real bot catalogue.
func newProbeNoticeServer(t *testing.T) (*Server, *stubCommenter, *store.Run) {
	t.Helper()
	const repo = "acme/widgets"
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{
		ID: "c1", TenantID: probeSequenceTenant, Provider: forge.ProviderGitHub,
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	s.forgePublishTokens = NewForgePublishTokenRegistry()
	s.forgePublishTokens.Register("run-token", ForgePublishGrant{
		TeamID: probeSequenceTenant, ConnectionID: "c1", Repo: repo,
	})
	c := &stubCommenter{}
	s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) {
		return c, nil
	}
	id, err := store.GenerateRunID()
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.cfg.Store.CreateRun(context.Background(), id, "review-pr", map[string]any{
		"pr_url": "https://github.com/acme/widgets/pull/7", forgePublishVarToken: "run-token", "gate_context": "revi/review",
	})
	if err != nil {
		t.Fatal(err)
	}
	run.BotID = "review-pr"
	run.Status = store.RunStatusFailedResumable
	run.Error = "node \"campaign\": rate_limited: You've hit your weekly limit · resets 9pm"
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return s, c, run
}

// newProbeSequenceWorld seeds the row every sequence starts from: a run
// parked on a provider wall four days out, one attempt spent, no probe taken
// yet — the shape the ticket measured on six reviews.
func newProbeSequenceWorld(t *testing.T, notice *Server, comments *stubCommenter, noticed *store.Run, base time.Time) *probeSequenceWorld {
	t.Helper()
	// Truncated to the millisecond the database stores: a row seeded with
	// more precision than Mongo can hold would answer a CAS the real one
	// refuses.
	wall := base.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	run := &store.Run{
		ID:           probeSequenceRunID,
		Status:       store.RunStatusFailedResumable,
		WorkflowHash: "wf-hash-1",
		FilePath:     probeSequenceBot,
		RetryPolicy: &store.RunRetryPolicy{
			UsageWindow: "resume", MaxAttempts: probeSequenceBudget, MaxWait: "192h", Jitter: "0s",
		},
		ContinuationState: store.ContinuationRetryArmed,
		RetryState: &store.RunRetryState{
			RetryAfter: &wall,
			Reason:     "usage_window",
			Code:       "USAGE_LIMIT_BLOCKED",
			Attempts:   1,
		},
	}
	st := retryprobetest.New(run)
	s, counter := newProbeSequenceServer(t, st)
	comments.bodies = nil
	return &probeSequenceWorld{
		st: st, s: s, lister: &modelRetryLister{st: st}, resumer: &fakeResumer{},
		counter: counter, notice: notice, comments: comments, noticed: noticed, t: t,
		// The seeded park is an arming no counter saw, and the pull request
		// may legitimately be told about it once.
		armedWhenNoticed: -1,
	}
}

func (w *probeSequenceWorld) snapshot() retryprobetest.Snapshot {
	doc := w.st.Doc()
	return retryprobetest.Snapshot{
		Row:          doc.RetryState,
		Status:       doc.Status,
		Continuation: doc.ContinuationState,
		Metered:      meteredRetryRuns(w.t, w.counter),
		Resumes:      len(w.resumer.calls),
		Armings:      w.st.PaidArmings + w.st.FreeRearms,
		Circuit:      w.st.CircuitFailures,
		Notices:      len(w.comments.bodies),
		Abandons:     len(w.st.Abandons),
	}
}

// apply drives one step through the real pass. It reports false when the row
// is in a state that step cannot reach in production.
func (w *probeSequenceWorld) apply(step retryprobetest.Step, now time.Time) bool {
	ctx := context.Background()
	switch step.Kind {
	case retryprobetest.StepProbeSweep:
		w.s.sweepRetryProbes(ctx, w.lister, now)
	case retryprobetest.StepDueSweep:
		w.resumer.failing = nil
		w.s.sweepDueRetries(ctx, w.lister, w.resumer, now)
	case retryprobetest.StepDueSweepResumeFails:
		// A publish that never landed: the slot goes back and the retry is
		// re-armed inside its budget — the path that overwrote the announced
		// instant with now+15m before the stand-down.
		w.resumer.failing = errors.New("PROBE: the queue refused the publish")
		w.s.sweepDueRetries(ctx, w.lister, w.resumer, now)
	case retryprobetest.StepDueSweepPoolRemap:
		// A permanent refusal: the sweeper ABANDONS, which on a probed row
		// would retire the run days before its wall falls.
		w.resumer.failing = fmt.Errorf("%w: run %s is bound to runner pool %q but its team is now mapped to %q",
			cloudpublisher.ErrPoolRemapped, probeSequenceRunID, "old-pool", "new-pool")
		w.s.sweepDueRetries(ctx, w.lister, w.resumer, now)
	case retryprobetest.StepNotice:
		// The park speaks on the pull request, ONCE per park: the notice
		// fires from the run-outcome event, and an arming is what produces
		// one. The row it reads is the live one; the rest of the document is
		// the parked review the notice fixture builds.
		if armed := w.armings(); armed == w.armedWhenNoticed {
			return false
		} else {
			w.armedWhenNoticed = armed
		}
		w.noticed.RetryState = w.st.Row()
		w.notice.noticeGatePausedForRetry(ctx, w.noticed)
	case retryprobetest.StepPaidPark:
		// What the runner's PAID arming does to the row — one attempt, a new
		// wall, the probe ledger closed. Who pays for an arming is decided
		// in pkg/runner and asserted by its own property; here it is the
		// precondition that opens a wall the pull request has not heard of.
		if !w.aPodCouldBeRunning(now) {
			return false
		}
		if _, _, err := w.st.ScheduleRunRetry(context.Background(), probeSequenceRunID,
			step.At, "usage_window", "USAGE_LIMIT_BLOCKED", probeSequenceBudget); err != nil {
			w.t.Fatalf("model arming: %v", err)
		}
	case retryprobetest.StepAgeMarker:
		row := w.st.Row()
		if row == nil || row.ProbeFrom == nil {
			return false
		}
		past := now.Add(-time.Hour)
		w.st.Mutate(func(r *store.Run) { r.RetryState.ProbeFrom = &past })
	case retryprobetest.StepAgePull:
		// The pull recedes while the instant it set aside stays ahead: the
		// refund and the stand-down read that row differently on purpose, and
		// nothing else in this driver reaches it.
		row := w.st.Row()
		if row == nil || row.LastProbeAt == nil {
			return false
		}
		stale := now.Add(-2 * store.ProbeMarkerTTL).Truncate(time.Millisecond)
		w.st.Mutate(func(r *store.Run) { r.RetryState.LastProbeAt = &stale })
	case retryprobetest.StepDue:
		// The armed instant arrives, which is what the due pass needs to see
		// a row a previous step armed for days out.
		row := w.st.Row()
		if row == nil || row.RetryAfter == nil || !row.RetryAfter.After(now) {
			return false
		}
		due := now.Truncate(time.Millisecond)
		w.st.Mutate(func(r *store.Run) { r.RetryState.RetryAfter = &due })
	default:
		w.t.Fatalf("server property: unsupported step %s", step.Kind)
	}
	return true
}

// armings counts the writes that armed this row, whoever paid for them: the
// paid park and the free re-arm a probe's stand-down performs.
func (w *probeSequenceWorld) armings() int { return w.st.PaidArmings + w.st.FreeRearms }

// aPodCouldBeRunning reports that something could legitimately be executing
// this run, which is the precondition of a park: its resume cleared the armed
// instant, or a probe pulled the row forward, or the instant it waited for
// has arrived.
func (w *probeSequenceWorld) aPodCouldBeRunning(now time.Time) bool {
	row := w.st.Row()
	if row == nil || row.RetryAfter == nil {
		return true
	}
	return row.ProbeFrom != nil || !row.RetryAfter.After(now)
}

// sweeperProbeSteps is the alphabet this driver executes for real.
func sweeperProbeSteps() []retryprobetest.StepKind {
	return []retryprobetest.StepKind{
		retryprobetest.StepProbeSweep,
		retryprobetest.StepDueSweep,
		retryprobetest.StepDueSweepResumeFails,
		retryprobetest.StepDueSweepPoolRemap,
		retryprobetest.StepNotice,
		retryprobetest.StepPaidPark,
		retryprobetest.StepAgeMarker,
		retryprobetest.StepAgePull,
		retryprobetest.StepDue,
	}
}

func runSweeperSequence(t *testing.T, notice *Server, comments *stubCommenter, noticed *store.Run, base time.Time, seq []retryprobetest.Step, seen map[string]int) []string {
	t.Helper()
	w := newProbeSequenceWorld(t, notice, comments, noticed, base)
	// The seeded park is a wall the pull request may legitimately be told
	// about once.
	ledger := &retryprobetest.Ledger{MaxAttempts: probeSequenceBudget, Wall: base.Add(4 * 24 * time.Hour), WallsOpened: 1}
	var bad []string
	for _, step := range seq {
		now := time.Now().UTC()
		before := w.snapshot()
		if !w.apply(step, now) {
			continue
		}
		after := w.snapshot()
		noteSweeper(seen, step, before, after, now, w.st)
		bad = append(bad, ledger.Observe(step, before, after, now)...)
		if len(bad) > 0 {
			return bad
		}
	}
	return bad
}

// noteSweeper records which states the sequences actually reached: a
// generator that never pulls a row forward would make every invariant below
// vacuous, and nothing else in the suite would say so.
func noteSweeper(seen map[string]int, step retryprobetest.Step, before, after retryprobetest.Snapshot, now time.Time, st *retryprobetest.Store) {
	if seen == nil {
		return
	}
	seen["step:"+string(step.Kind)]++
	if after.Resumes > before.Resumes {
		if before.LiveMarker(now) {
			seen["probe-driven resume"]++
		} else {
			seen["ordinary billable resume"]++
		}
	}
	if st.Pulls > 0 {
		seen["row pulled forward"]++
	}
	if st.FreeRearms > 0 {
		seen["stand-down restored the wall"]++
	}
	if after.Notices > before.Notices {
		seen["pause notice posted"]++
	}
	if before.LiveMarker(now) && (step.Kind == retryprobetest.StepDueSweepPoolRemap || step.Kind == retryprobetest.StepDueSweepResumeFails) {
		seen["settlement attempted on a probed row"]++
	}
	if after.Attempts() == probeSequenceBudget {
		seen["attempt budget spent"]++
	}
	if before.StaleMarker(now) {
		seen["pass over a marker whose pull is too old"]++
	}
	if step.Kind == retryprobetest.StepDue && after.Row != nil && after.Row.RetryAfter != nil {
		seen["armed instant arrived"]++
	}
}

// TestProbeGuarantee_TheSweepersPassesKeepItOverRandomSequences drives the
// three real passes over 60 seeded sequences, asserting the guarantee after
// every step.
func TestProbeGuarantee_TheSweepersPassesKeepItOverRandomSequences(t *testing.T) {
	notice, comments, noticed := newProbeNoticeServer(t)
	kinds := sweeperProbeSteps()
	seen := map[string]int{}
	for seed := uint64(1); seed <= 60; seed++ {
		base := time.Now().UTC()
		seq := retryprobetest.Generate(seed, kinds, base)
		bad := runSweeperSequence(t, notice, comments, noticed, base, seq, seen)
		if len(bad) == 0 {
			continue
		}
		shortest, at := bad, len(seq)
		for n := 1; n < len(seq); n++ {
			if b := runSweeperSequence(t, notice, comments, noticed, base, seq[:n], nil); len(b) > 0 {
				shortest, at = b, n
				break
			}
		}
		t.Fatalf("seed %d: the probe's guarantee broke within the first %d of %d steps\n%s",
			seed, at, len(seq), strings.Join(shortest, "\n"))
	}
	for _, want := range []string{
		"row pulled forward",
		"probe-driven resume",
		"ordinary billable resume",
		"stand-down restored the wall",
		"settlement attempted on a probed row",
		"pause notice posted",
		"attempt budget spent",
		"pass over a marker whose pull is too old",
		"armed instant arrived",
	} {
		if seen[want] == 0 {
			t.Errorf("no generated sequence ever reached %q — the property is asserting nothing about it\nwhat they did reach: %s",
				want, describeSweeperCoverage(seen))
		}
	}
}

func describeSweeperCoverage(seen map[string]int) string {
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, seen[k]))
	}
	return strings.Join(parts, " ")
}
