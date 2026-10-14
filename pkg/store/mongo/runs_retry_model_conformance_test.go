package mongo

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/retryprobetest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The probe's guarantee is asserted over random event sequences in pkg/runner
// and pkg/server, against the in-memory twin of this row
// (internal/retryprobetest) — because forty steps have to run in
// milliseconds, on a machine with no database. That twin is only worth its
// verdict while it answers what THIS store answers, and a double kinder than
// the real thing has already hidden a blind lane here for seven review rounds.
//
// So the pairing is a property of its own: the same generated sequences of
// store operations, applied to the real replica set and to the model, with
// the row compared after EVERY one — the returned values, the armed instant,
// the marker, the ledger, the attempt count, the continuation, and what each
// of the two scans lists.
//
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27077/?replicaSet=rs0' \
//	    devbox run -- go test ./pkg/store/mongo/ -run TestRunRetryProbeModel

type retryOp string

const (
	opSchedule  retryOp = "ScheduleRunRetry"
	opDelay     retryOp = "DelayRunRetry"
	opPull      retryOp = "PullRunRetry"
	opRearm     retryOp = "RearmProbedRunRetry"
	opClaim     retryOp = "ClaimRunRetry"
	opClear     retryOp = "ClearRunRetry"
	opAbandon   retryOp = "AbandonRunRetry"
	opScanDue   retryOp = "ListRunsDueForRetry"
	opScanProbe retryOp = "ListRunsForRetryProbe"
	// conformBudget is the ATTEMPT budget an arming is refused past.
	conformBudget = 4
	// conformProbeBudget is the probe budget the scan's own bound uses. TWO,
	// not the shipped 24 and not one: with 24 no sequence ever landed the
	// pulls one wait needs to reach the bound, and with one the bound would
	// shadow the PACE term for ever (a row under the budget with a pull stamp
	// is exactly what the pace is about).
	conformProbeBudget = 2
	conformFloor       = time.Hour
)

var retryOps = []retryOp{opSchedule, opDelay, opPull, opRearm, opClaim, opClear, opAbandon, opScanDue, opScanProbe}

// conformScripts are the DIRECTED sequences: the two filter terms a uniform
// draw reaches once in a thousand steps need a specific prefix, and a coverage
// assertion that depends on luck is a flake, not a guard.
//
//   - the probe BUDGET excludes a row with no marker whose ledger is at the
//     bound: two landed pull/re-arm cycles on one wait, nothing in between;
//   - a scan at limit 0 must LIST (both scans coerce it to 100), which needs a
//     row that passes every other term at the moment the limit is 0.
var conformScripts = map[string][]retryOp{
	"budget":  {opSchedule, opPull, opRearm, opPull, opRearm, opScanProbe},
	"limit-0": {opSchedule, opScanProbe, opScanDue},
}

// TestRunRetryProbeModel_AnswersWhatTheStoreAnswers walks 30 seeded random
// sequences of 30 operations, plus the directed scripts, through both
// implementations.
func TestRunRetryProbeModel_AnswersWhatTheStoreAnswers(t *testing.T) {
	seen := map[string]int{}
	for seed := uint64(1); seed <= 30; seed++ {
		conformSequence(t, seed, nil, seen)
	}
	for name, script := range conformScripts {
		conformSequence(t, 1000+uint64(len(name)), script, seen)
	}
	// A pairing whose sequences never reach the states the filters are about
	// agrees with the store by never asking it anything.
	want := []string{
		"pull landed",
		"pull refused over a live marker",
		"re-arm landed",
		"arming refused",
		"claim landed",
		"due scan listed the row",
		"probe scan listed the row",
		"probe scan at limit 0 listed the row",
	}
	// ListRunsForRetryProbe is the function this change adds, and its filter
	// has six terms beyond the status. "The scan refused the row" is not
	// coverage of them: five of the six could be dropped from the model while
	// the pairing stayed green, because no sequence ever reached the state
	// they are about. Each must have excluded the row at least once.
	for _, term := range probeScanTerms {
		want = append(want, "probe scan refused the row on the "+term)
	}
	for _, w := range want {
		if seen[w] == 0 {
			t.Errorf("no conformance sequence ever reached %q — the pairing is not compared there\nwhat they did reach: %v", w, seen)
		}
	}
}

// conformSequence runs ONE sequence against the real replica set and the
// model, comparing the returned values and the whole row after every step. A
// nil script means the ops are drawn at random; a script fixes them, and its
// arguments are then chosen to LAND (the live CAS instant, a far arming, a
// scan at limit 0) so the state it is written for is reached every run.
func conformSequence(t *testing.T, seed uint64, script []retryOp, seen map[string]int) {
	t.Helper()
	runID := fmt.Sprintf("run-conform-%d", seed)
	real := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, real, runID)

	// The model starts as a COPY of the seeded row, so the first
	// divergence the comparison reports is one of the operations and
	// never of the fixture.
	seeded, err := real.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("seed %d: LoadRun: %v", seed, err)
	}
	model := retryprobetest.New(&store.Run{
		ID:                seeded.ID,
		Status:            seeded.Status,
		ContinuationState: seeded.ContinuationState,
		RetryState:        seeded.RetryState,
	})

	r := rand.New(rand.NewPCG(seed, 0x2247))
	// NOT truncated: BSON stores milliseconds, so an instant with
	// nanoseconds is exactly what makes a model's CAS answer what the
	// store's answers — or not. A generator that only ever produced
	// millisecond-aligned instants never asked the question, and the one
	// unfaithfulness class this pairing is credited with catching was
	// out of its reach.
	base := time.Now().UTC()
	instants := []time.Time{
		base.Add(-time.Hour), base.Add(5 * time.Minute), base.Add(95 * time.Minute), base.Add(4 * 24 * time.Hour),
	}
	// An ARMING draws from the far end of the palette most of the time.
	// A row armed a few minutes out is excluded by the scan's floor
	// before any other term is even evaluated, so a uniform draw spent
	// most of its steps re-proving that one.
	armingInstants := []time.Time{
		base.Add(95 * time.Minute), base.Add(4 * 24 * time.Hour), base.Add(7 * 24 * time.Hour), base.Add(5 * time.Minute),
	}
	var history []string
	var prev retryOp
	steps := 30
	if script != nil {
		steps = len(script)
	}
	for step := 0; step < steps; step++ {
		op := retryOps[r.IntN(len(retryOps))]
		if script != nil {
			op = script[step]
		}
		// The probe scan's narrow terms (the pace, a live lease, a marker
		// already held) are about a row the PREVIOUS op just left in that
		// state, and a uniform draw over nine ops almost never lines one
		// up: 600 steps landed three re-arms and ten claims and never
		// scanned right after one.
		if script == nil && r.IntN(2) == 0 {
			switch prev {
			case opPull, opRearm, opClaim:
				op = opScanProbe
			}
		}
		prev = op
		at := instants[r.IntN(len(instants))]
		switch op {
		case opSchedule, opRearm, opDelay:
			at = armingInstants[r.IntN(len(armingInstants))]
		}
		// The limit palette includes 0, which both scans COERCE to 100
		// rather than treating as "list nothing" — a divergence the
		// pairing could not see while it always passed 10.
		limit := []int{0, 1, 10}[r.IntN(3)]
		// And the reason: nothing but a usage-window wait is probe
		// eligible, so a pairing that only ever writes that reason never
		// compares the term that says so.
		reason := "usage_window"
		if r.IntN(4) == 0 {
			reason = "transient"
		}
		rowBefore, berr := real.LoadRun(ctx, runID)
		if berr != nil {
			t.Fatalf("seed %d step %d: LoadRun: %v", seed, step, berr)
		}
		// The CAS argument is usually the instant the row really holds
		// (the caller read it from a scan) and sometimes a stale one (an
		// operator or another replica moved the row since). Biased
		// towards the live instant: a CAS that never matches exercises
		// one branch of every conditional update.
		expected := at
		if r.IntN(4) != 0 && rowBefore.RetryState != nil && rowBefore.RetryState.RetryAfter != nil {
			expected = *rowBefore.RetryState.RetryAfter
		}
		if script != nil {
			// A scripted step must LAND: the far arming instant, the live
			// CAS argument, a usage-window reason, and limit 0 on the scans
			// (the value both stores coerce, and the one a model answering
			// "nothing" would get wrong).
			at, limit, reason = base.Add(7*24*time.Hour), 0, "usage_window"
			switch op {
			case opScanDue:
				at = base.Add(8 * 24 * time.Hour)
			case opScanProbe:
				// Four days out: past the PACE (the pull stamps the clock,
				// and at-floor is days after it) so the term under it — the
				// probe budget — is the one that excludes the row. A scan
				// minutes from now is excluded by the pace first, and the
				// budget term is then compared against nothing.
				at = base.Add(4 * 24 * time.Hour)
			}
			expected = at
			if rowBefore.RetryState != nil && rowBefore.RetryState.RetryAfter != nil {
				expected = *rowBefore.RetryState.RetryAfter
			}
		}
		args := retryOpArgs{at: at, expected: expected, limit: limit, reason: reason}
		gotReal, errReal := applyRetryOp(ctx, real, model, runID, op, args, true)
		gotModel, errModel := applyRetryOp(ctx, real, model, runID, op, args, false)
		history = append(history, fmt.Sprintf("%s(at=%s expected=%s limit=%d reason=%s)",
			op, at.Format(time.RFC3339Nano), expected.Format(time.RFC3339Nano), limit, reason))
		if errReal != nil || errModel != nil {
			t.Fatalf("seed %d step %d %s: store err=%v model err=%v\nhistory: %s", seed, step, op, errReal, errModel, strings.Join(history, " → "))
		}
		noteConformance(seen, op, gotReal, rowBefore.RetryState, args)
		if gotReal != gotModel {
			t.Fatalf("seed %d step %d: %s returned %q from the store and %q from the model\nhistory: %s",
				seed, step, op, gotReal, gotModel, strings.Join(history, " → "))
		}
		realRow, lerr := real.LoadRun(ctx, runID)
		if lerr != nil {
			t.Fatalf("seed %d step %d: LoadRun: %v", seed, step, lerr)
		}
		if a, b := describeConformRow(realRow.Status, realRow.ContinuationState, realRow.RetryState), describeConformRow(model.Doc().Status, model.Doc().ContinuationState, model.Row()); a != b {
			t.Fatalf("seed %d step %d: the row diverged after %s\n  store: %s\n  model: %s\nhistory: %s",
				seed, step, op, a, b, strings.Join(history, " → "))
		}
	}
}

// probeScanTerms are the filter terms of ListRunsForRetryProbe beyond the
// status, in the order the scan applies them.
var probeScanTerms = []string{"reason", "floor", "marker", "budget", "pace", "lease"}

// probeScanExclusion names the FIRST term that keeps this row out of the probe
// scan, or "" when the scan should list it. Stated here from the row and the
// instant — independently of the model, which is the thing being compared.
func probeScanExclusion(row *store.RunRetryState, at time.Time) string {
	at = at.UTC().Truncate(time.Millisecond)
	if row == nil || row.RetryAfter == nil {
		return "armed"
	}
	switch {
	case row.Reason != "usage_window":
		return "reason"
	case !row.RetryAfter.After(at.Add(conformFloor)):
		return "floor"
	case row.ProbeFrom != nil:
		return "marker"
	case row.ProbeCount >= conformProbeBudget:
		return "budget"
	case row.LastProbeAt != nil && row.LastProbeAt.After(at.Add(-conformFloor)):
		return "pace"
	case row.ClaimedAt != nil && !row.ClaimedAt.Before(at.Add(-retryprobetest.ClaimLease)):
		return "lease"
	}
	return ""
}

// noteConformance records which of the store's own branches the sequences
// actually took, read from what the STORE answered.
func noteConformance(seen map[string]int, op retryOp, got string, before *store.RunRetryState, args retryOpArgs) {
	if op == opScanProbe {
		if got == "listed=0" {
			if term := probeScanExclusion(before, args.at); term != "" {
				seen["probe scan refused the row on the "+term]++
			}
		} else if args.limit == 0 {
			seen["probe scan at limit 0 listed the row"]++
		}
	}
	expected := args.expected
	switch {
	case op == opPull && got == "true":
		seen["pull landed"]++
	case op == opPull && got == "false" && before != nil && before.ProbeFrom != nil &&
		before.RetryAfter != nil && before.RetryAfter.Truncate(time.Millisecond).Equal(expected.UTC().Truncate(time.Millisecond)):
		// The exact state the pull's third filter term exists for: the CAS
		// matches and a marker is already held aside.
		seen["pull refused over a live marker"]++
	case op == opRearm && got == "true":
		seen["re-arm landed"]++
	case op == opSchedule && strings.HasPrefix(got, "false"):
		seen["arming refused"]++
	case op == opClaim && got == "true":
		seen["claim landed"]++
	case op == opScanDue && got == "listed=1":
		seen["due scan listed the row"]++
	case op == opScanProbe && got == "listed=1":
		seen["probe scan listed the row"]++
	}
}

// retryOpArgs is what one generated operation carries: the instant, the CAS
// argument, the scan limit and the reason an arming writes.
type retryOpArgs struct {
	at       time.Time
	expected time.Time
	limit    int
	reason   string
}

// applyRetryOp runs one operation against the store (real=true) or the model
// and renders what it returned, so the two can be compared as strings.
func applyRetryOp(ctx context.Context, real *Store, model *retryprobetest.Store, runID string, op retryOp, args retryOpArgs, onStore bool) (string, error) {
	at, expected := args.at, args.expected
	switch op {
	case opSchedule:
		if onStore {
			ok, attempt, err := real.ScheduleRunRetry(ctx, runID, at, args.reason, "USAGE_LIMIT_BLOCKED", conformBudget)
			return fmt.Sprintf("%v/%d", ok, attempt), err
		}
		ok, attempt, err := model.ScheduleRunRetry(ctx, runID, at, args.reason, "USAGE_LIMIT_BLOCKED", conformBudget)
		return fmt.Sprintf("%v/%d", ok, attempt), err
	case opDelay:
		if onStore {
			ok, err := real.DelayRunRetry(ctx, runID, expected, at)
			return fmt.Sprintf("%v", ok), err
		}
		ok, err := model.DelayRunRetry(ctx, runID, expected, at)
		return fmt.Sprintf("%v", ok), err
	case opPull:
		if onStore {
			ok, err := real.PullRunRetry(ctx, runID, expected, at)
			return fmt.Sprintf("%v", ok), err
		}
		ok, err := model.PullRunRetry(ctx, runID, expected, at)
		return fmt.Sprintf("%v", ok), err
	case opRearm:
		if onStore {
			ok, err := real.RearmProbedRunRetry(ctx, runID, at)
			return fmt.Sprintf("%v", ok), err
		}
		ok, err := model.RearmProbedRunRetry(ctx, runID, at)
		return fmt.Sprintf("%v", ok), err
	case opClaim:
		if onStore {
			ok, err := real.ClaimRunRetry(ctx, runID, expected)
			return fmt.Sprintf("%v", ok), err
		}
		ok, err := model.ClaimRunRetry(ctx, runID, expected)
		return fmt.Sprintf("%v", ok), err
	case opClear:
		if onStore {
			return "", real.ClearRunRetry(ctx, runID)
		}
		return "", model.ClearRunRetry(ctx, runID)
	case opAbandon:
		const reason = "auto-retry abandoned: PROBE conformance"
		if onStore {
			return "", real.AbandonRunRetry(ctx, runID, reason)
		}
		return "", model.AbandonRunRetry(ctx, runID, reason)
	case opScanDue:
		if onStore {
			rows, err := real.ListRunsDueForRetry(store.WithoutTenantFilter(ctx), at, args.limit)
			return fmt.Sprintf("listed=%d", len(rows)), err
		}
		return fmt.Sprintf("listed=%d", len(model.DueRows(at, args.limit))), nil
	case opScanProbe:
		if onStore {
			rows, err := real.ListRunsForRetryProbe(store.WithoutTenantFilter(ctx), at, conformFloor, conformProbeBudget, args.limit)
			return fmt.Sprintf("listed=%d", len(rows)), err
		}
		return fmt.Sprintf("listed=%d", len(model.ProbeRows(at, conformFloor, conformProbeBudget, args.limit))), nil
	}
	return "", fmt.Errorf("unknown op %s", op)
}

// describeConformRow renders everything the probe's readers read. The
// instants a store stamps ITSELF (scheduled_at, claimed_at, last_probe_at)
// are compared by presence: both sides stamp their own clock, so an equality
// there would be a test of the clock.
func describeConformRow(status store.RunStatus, continuation store.ContinuationState, row *store.RunRetryState) string {
	if row == nil {
		return fmt.Sprintf("status=%s continuation=%s retry_state=absent", status, continuation)
	}
	return fmt.Sprintf("status=%s continuation=%s retry_after=%s probe_from=%s probe_count=%d attempts=%d reason=%s code=%s last_error=%q scheduled_at=%v claimed_at=%v last_probe_at=%v",
		status, continuation,
		conformInstant(row.RetryAfter), conformInstant(row.ProbeFrom), row.ProbeCount, row.Attempts,
		row.Reason, row.Code, row.LastError,
		row.ScheduledAt != nil, row.ClaimedAt != nil, row.LastProbeAt != nil)
}

func conformInstant(t *time.Time) string {
	if t == nil {
		return "absent"
	}
	return t.UTC().Truncate(time.Millisecond).Format(time.RFC3339Nano)
}
