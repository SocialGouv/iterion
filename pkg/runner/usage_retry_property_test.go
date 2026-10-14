package runner

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/retryprobetest"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The runner's half of the probe's guarantee, over random sequences.
//
// The example tests next door each pin ONE interleaving that cost a review
// round to find. This one generates them: seeded sequences of up to forty
// events — a dated park, a park naming no instant, a park after real work, a
// pull-forward probe, a resume's clear, a skipped credential reopening sooner
// or later, a wall that passes while a probe is in flight — driven through
// the REAL arming, with the invariants of internal/retryprobetest asserted
// after every single step.
//
// What it is here to catch is the class the three rounds kept producing: a
// fix whose condition is right on the state the report named and wrong on the
// state two steps later.

// probeWorld is one sequence's world: the modelled row, the real Runner that
// decides, and the counters an operator reads.
type probeWorld struct {
	st    *retryprobetest.Store
	r     *Runner
	reg   *metrics.Registry
	runID string
}

const (
	probePropertyMaxAttempts = 3
	// probePropertyFloor is the probe pace the scan applies, the shipped
	// default (pkg/server.retryProbeFloorDefault).
	probePropertyFloor = time.Hour
	// probePropertyProbeBudget is deliberately SMALL, not the shipped 24: a
	// sequence of forty steps can spend three probes on one wait, and the
	// scan's budget term then bites inside the property instead of being a
	// filter nothing ever reaches.
	probePropertyProbeBudget = 3
)

func newProbeWorld() *probeWorld {
	run := &store.Run{
		ID:     "run-2247",
		Status: store.RunStatusFailedResumable,
		// retrycoord keys the shared circuit on the workflow hash: without
		// one, I3's "fed to the circuit" could not be observed at all.
		WorkflowHash: "wf-hash-1",
		// MaxWait is deliberately longer than the furthest wall the
		// generator draws (7d), so no instant below is clamped by the
		// horizon — the property is about the probe, not about max_wait.
		RetryPolicy: &store.RunRetryPolicy{
			UsageWindow: "resume", MaxAttempts: probePropertyMaxAttempts, MaxWait: "192h", Jitter: "0s",
		},
	}
	st := retryprobetest.New(run)
	reg := metrics.New()
	return &probeWorld{st: st, r: &Runner{cfg: Config{Store: st, Metrics: reg}}, reg: reg, runID: run.ID}
}

func (w *probeWorld) snapshot(t *testing.T) retryprobetest.Snapshot {
	t.Helper()
	doc := w.st.Doc()
	return retryprobetest.Snapshot{
		Row:          doc.RetryState,
		Status:       doc.Status,
		Continuation: doc.ContinuationState,
		Blocked:      int(counterValue(t, w.reg.RunsUsageWindowBlocked)),
		Scheduled:    int(counterValue(t, w.reg.RunsRetryScheduled)),
		Armings:      w.st.PaidArmings + w.st.FreeRearms,
		Circuit:      w.st.CircuitFailures,
		Abandons:     len(w.st.Abandons),
	}
}

// apply drives ONE step through the real code (or, for the row-level steps,
// through the store the real code shares with the sweeper). It reports false
// when the row is in a state that step cannot reach in production, and then
// changes nothing — the invariants are about states the system produces, and
// a model that invents others proves nothing about it.
func (w *probeWorld) apply(step retryprobetest.Step, now time.Time) bool {
	ctx := context.Background()
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	if step.IsPark() && !w.aPodCouldBeRunning(now) {
		// A park is what an EXECUTION does, and no execution exists for a
		// run whose retry is still armed for a future instant with no probe
		// holding it: the pod that armed it is gone and nothing has resumed
		// it yet. Parking such a row is a state no pass can produce, and
		// modelling it would invent a wall that production never loses.
		return false
	}
	switch step.Kind {
	case retryprobetest.StepParkDated, retryprobetest.StepParkAfterWorkDated:
		w.r.armUsageWindowRetry(ctx, weeklyWindowErr(step.At), w.runID, step.Spend, quiet)
	case retryprobetest.StepParkBlind, retryprobetest.StepParkAfterWorkBlind:
		w.r.armUsageWindowRetry(ctx, blindUsageWindowErr(), w.runID, step.Spend, quiet)
	case retryprobetest.StepSkippedSooner:
		at := now.Add(45 * time.Minute)
		w.st.Mutate(func(r *store.Run) { r.SkippedCredReopensAt = &at })
	case retryprobetest.StepSkippedLater:
		at := now.Add(10 * 24 * time.Hour)
		w.st.Mutate(func(r *store.Run) { r.SkippedCredReopensAt = &at })
	case retryprobetest.StepSkippedCleared:
		w.st.Mutate(func(r *store.Run) { r.SkippedCredReopensAt = nil })
	case retryprobetest.StepProbePull:
		// What the sweeper's probe pass does to the row, through the SCAN
		// that decides which rows it may touch: a pull the floor, the pace,
		// the budget or a live lease would have refused is a state production
		// cannot produce, and inventing one manufactures markers on rows the
		// real pass leaves frozen — the kinder half of the world is where a
		// guarantee goes to hide.
		rows := w.st.ProbeRows(now, probePropertyFloor, probePropertyProbeBudget, 2)
		if len(rows) == 0 || rows[0].RetryAfter == nil {
			return false
		}
		if _, err := w.st.PullRunRetry(ctx, w.runID, *rows[0].RetryAfter, now); err != nil {
			panic(err)
		}
	case retryprobetest.StepResumeClear:
		// What a resume does: the lease and the armed instant go, the probe
		// ledger stays — the pod has not reached the preflight yet.
		//
		// Only a row a sweeper could actually have claimed is cleared: one
		// that is DUE, or one a probe pulled forward. Clearing a row still
		// armed for a future instant is a state no pass can produce (the
		// claim CASes on the instant a scan listed), and modelling it would
		// invent a lost wall nothing in production loses.
		row := w.st.Row()
		if row == nil || row.RetryAfter == nil {
			return false
		}
		if row.RetryAfter.After(now) && row.ProbeFrom == nil {
			return false
		}
		if err := w.st.ClearRunRetry(ctx, w.runID); err != nil {
			panic(err)
		}
	case retryprobetest.StepAgeMarker:
		// The wall a probe is holding passes while the probe is in flight.
		row := w.st.Row()
		if row == nil || row.ProbeFrom == nil {
			return false
		}
		past := now.Add(-time.Hour)
		w.st.Mutate(func(r *store.Run) { r.RetryState.ProbeFrom = &past })
	case retryprobetest.StepAgePull:
		// The PULL recedes while the instant it set aside stays ahead: the
		// row a probe-driven resume that never parked leaves behind. Nothing
		// in production rewrites last_probe_at backwards — time does it, and
		// a sequence that runs in microseconds has to say so by hand.
		row := w.st.Row()
		if row == nil || row.LastProbeAt == nil {
			return false
		}
		stale := now.Add(-2 * store.ProbeMarkerTTL).Truncate(time.Millisecond)
		w.st.Mutate(func(r *store.Run) { r.RetryState.LastProbeAt = &stale })
	case retryprobetest.StepDue:
		// The instant the row was armed for ARRIVES. Modelled on the row
		// (retry_after = now) rather than by moving the clock, which is what
		// the due scan reads; the ledger drops the wall for the same reason.
		// Only a row armed for a future instant has an instant to wait for.
		row := w.st.Row()
		if row == nil || row.RetryAfter == nil || !row.RetryAfter.After(now) {
			return false
		}
		due := now.Truncate(time.Millisecond)
		w.st.Mutate(func(r *store.Run) { r.RetryState.RetryAfter = &due })
	default:
		panic("runner property: unsupported step " + string(step.Kind))
	}
	return true
}

// aPodCouldBeRunning reports that something could legitimately be executing
// this run: its resume cleared the armed instant, or a probe pulled the row
// forward, or the instant it waited for has arrived.
func (w *probeWorld) aPodCouldBeRunning(now time.Time) bool {
	row := w.st.Row()
	if row == nil || row.RetryAfter == nil {
		return true
	}
	return row.ProbeFrom != nil || !row.RetryAfter.After(now)
}

// runnerProbeSteps is the alphabet this driver can execute for REAL.
//
// Deliberately out: a store blip on the free re-arm (the fall-through then
// charges an attempt by design — the price of never losing a durable retry,
// pinned by its own example tests), and an operator's own resume (which takes
// the run out of failed_resumable and makes every later step inert).
func runnerProbeSteps() []retryprobetest.StepKind {
	return []retryprobetest.StepKind{
		retryprobetest.StepParkDated,
		retryprobetest.StepParkBlind,
		retryprobetest.StepParkAfterWorkDated,
		retryprobetest.StepParkAfterWorkBlind,
		retryprobetest.StepSkippedSooner,
		retryprobetest.StepSkippedLater,
		retryprobetest.StepSkippedCleared,
		retryprobetest.StepProbePull,
		retryprobetest.StepResumeClear,
		retryprobetest.StepAgeMarker,
		retryprobetest.StepAgePull,
		retryprobetest.StepDue,
	}
}

// runRunnerSequence executes one sequence and returns the invariants it
// violated. It never fails the test itself, so the caller can replay prefixes
// to find the shortest violating one.
func runRunnerSequence(t *testing.T, seq []retryprobetest.Step, seen map[string]int) []string {
	t.Helper()
	w := newProbeWorld()
	ledger := &retryprobetest.Ledger{MaxAttempts: probePropertyMaxAttempts}
	var bad []string
	for _, step := range seq {
		now := time.Now().UTC()
		before := w.snapshot(t)
		// The skipped credential's reopening is a fact of the ROW, not of
		// the step: whatever an earlier step recorded is what this arming
		// weighs.
		if doc := w.st.Doc(); doc.SkippedCredReopensAt != nil {
			step.Skipped = *doc.SkippedCredReopensAt
		}
		if !w.apply(step, now) {
			continue
		}
		after := w.snapshot(t)
		note(seen, step, before, after, now, w.st)
		bad = append(bad, ledger.Observe(step, before, after, now)...)
		if len(bad) > 0 {
			return bad
		}
	}
	return bad
}

// note records which STATES the sequences actually reached. A property whose
// generator never produces the interleaving it exists for is a decoration,
// and nothing else in the suite would say so.
func note(seen map[string]int, step retryprobetest.Step, before, after retryprobetest.Snapshot, now time.Time, st *retryprobetest.Store) {
	if seen == nil {
		return
	}
	seen["step:"+string(step.Kind)]++
	switch {
	case step.IsPark() && before.LiveMarker(now):
		seen["park on a live probe marker"]++
	case step.IsPark() && before.StaleMarker(now):
		seen["park on a marker whose pull is too old"]++
	case step.IsPark() && before.ExpiredMarker(now):
		seen["park on an expired probe marker"]++
	}
	if step.IsPark() && !step.Skipped.IsZero() && before.AnnouncedMarker(now) {
		seen["park weighing a skipped credential on a row holding the wall"]++
	}
	if step.IsPark() && step.Spend {
		seen["park charged after real work"]++
	}
	if after.Attempts() == probePropertyMaxAttempts {
		seen["attempt budget spent"]++
	}
	if st.FreeRearms > 0 {
		seen["free re-arm taken"]++
	}
	if st.RefusedArmings > 0 {
		seen["paid arming refused"]++
	}
	if before.Row != nil && before.Row.ProbeCount > 0 {
		seen["probe ledger open"]++
	}
	// The treadmill's precondition: a row whose own armed instant arrived,
	// with no probe holding it — the state every free re-arm leaves, and the
	// one no step could reach before StepDue existed.
	if step.Kind == retryprobetest.StepDue && after.Row != nil && after.Row.ProbeFrom == nil {
		seen["row resumed after its own instant arrived"]++
	}
}

// TestProbeGuarantee_TheRunnersArmingKeepsItOverRandomSequences is the
// guarantee test the review rounds could not reach by reading: 200 seeded
// sequences, every invariant after every step.
func TestProbeGuarantee_TheRunnersArmingKeepsItOverRandomSequences(t *testing.T) {
	kinds := runnerProbeSteps()
	seen := map[string]int{}
	for seed := uint64(1); seed <= 200; seed++ {
		base := time.Now().UTC()
		seq := retryprobetest.Generate(seed, kinds, base)
		bad := runRunnerSequence(t, seq, seen)
		if len(bad) == 0 {
			continue
		}
		// Shrink by replaying prefixes: the shortest one that still
		// violates is the interleaving to read.
		shortest, at := bad, len(seq)
		for n := 1; n < len(seq); n++ {
			if b := runRunnerSequence(t, seq[:n], nil); len(b) > 0 {
				shortest, at = b, n
				break
			}
		}
		t.Fatalf("seed %d: the probe's guarantee broke within the first %d of %d steps\n%s",
			seed, at, len(seq), strings.Join(shortest, "\n"))
	}
	for _, want := range []string{
		"park on a live probe marker",
		"park on a marker whose pull is too old",
		"park on an expired probe marker",
		"park weighing a skipped credential on a row holding the wall",
		"park charged after real work",
		"attempt budget spent",
		"free re-arm taken",
		"paid arming refused",
		"probe ledger open",
		"row resumed after its own instant arrived",
		"step:" + string(retryprobetest.StepProbePull),
		"step:" + string(retryprobetest.StepResumeClear),
		"step:" + string(retryprobetest.StepAgeMarker),
		"step:" + string(retryprobetest.StepAgePull),
		"step:" + string(retryprobetest.StepDue),
	} {
		if seen[want] == 0 {
			t.Errorf("no generated sequence ever reached %q — the property is asserting nothing about it\nwhat they did reach: %s", want, describeCoverage(seen))
		}
	}
}

func describeCoverage(seen map[string]int) string {
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
