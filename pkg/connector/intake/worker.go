package intake

import (
	"context"
	"errors"
	"fmt"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/schedgate"
)

// Verdict is one candidate subscription's answer to an intake.
type Verdict struct {
	SubscriptionID string
	// Matched: the subscription's filters and `when` accept the event.
	Matched bool
	// Refusal, when set, is a verdict no retry changes — an expression past
	// its budget, a mapping that cannot be evaluated — and wins over
	// Matched. Recorded on a refused pair, visible and replayable.
	Refusal string
	// Reason says why a candidate did not match, for the record.
	Reason string
}

// Matcher lists an intake's candidate subscriptions and judges each one. Its
// error is a failure of the whole matching — a store that cannot answer —
// and the intake is retried; a candidate's own failure is its Verdict.
type Matcher interface {
	Match(ctx context.Context, in Intake) ([]Verdict, error)
}

// Submitter launches what a pair owes, as the idempotent submission
// p.SubmissionKey() names, and returns the run id once the submission is
// acknowledged. Repeating a call for the same key never produces a second
// run (runview.LaunchSpec.SubmissionKey).
//
// Its error is read for four classes, by method so the launch path need not
// import this package: SubmissionSpent() true — the key's run never left the
// launch path, the pair moves to its next epoch; RefusalReason() — no retry
// of this pair can succeed; PostponeFor() true — a transient denial, the
// pair waits the duration with its attempts whole; schedgate.ErrMaintenanceHold — postponed,
// without spending an attempt. Anything else is a transient failure.
type Submitter interface {
	Submit(ctx context.Context, in Intake, p Pair) (runID string, err error)
}

// Refusal is a permanent failure a Matcher or Submitter reports.
type Refusal struct {
	Reason string
	Err    error
}

// Refuse wraps err as a permanent refusal saying reason.
func Refuse(reason string, err error) error { return &Refusal{Reason: reason, Err: err} }

func (r *Refusal) Error() string {
	if r.Err == nil {
		return r.Reason
	}
	return r.Reason + ": " + r.Err.Error()
}

func (r *Refusal) Unwrap() error { return r.Err }

// RefusalReason marks the error as permanent.
func (r *Refusal) RefusalReason() string { return r.Reason }

// refusalReason reads a permanent refusal off err.
func refusalReason(err error) (string, bool) {
	var r interface{ RefusalReason() string }
	if errors.As(err, &r) {
		return r.RefusalReason(), true
	}
	return "", false
}

// postponeFor reads a transient denial off err: how long the pair waits
// before it is tried again, without spending an attempt (ADR-168 Q3).
func postponeFor(err error) (time.Duration, bool) {
	var p interface{ PostponeFor() (time.Duration, bool) }
	if errors.As(err, &p) {
		return p.PostponeFor()
	}
	return 0, false
}

// submissionSpent reads a spent submission off err.
func submissionSpent(err error) bool {
	var s interface{ SubmissionSpent() bool }
	return errors.As(err, &s) && s.SubmissionSpent()
}

// DefaultBatch is how many rows one tick claims.
const DefaultBatch = 20

// Worker drives intakes to their pairs and pairs to their launches. Every
// replica may run one: the store's claims keep two workers off one row, and
// its generations make a late worker's writes fail instead of land.
type Worker struct {
	Store     Store
	Matcher   Matcher
	Submitter Submitter
	// Owner names this worker on the rows it claims; unique per process.
	Owner   string
	Intakes Policy
	Pairs   Policy
	// Batch bounds the rows one tick claims; 0 is DefaultBatch.
	Batch  int
	Logger *iterlog.Logger
	// Now is the clock; nil reads the wall clock.
	Now func() time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) batch() int {
	if w.Batch > 0 {
		return w.Batch
	}
	return DefaultBatch
}

func (w *Worker) warn(format string, args ...any) {
	if w.Logger != nil {
		w.Logger.Warn(format, args...)
	}
}

// Run ticks every interval until ctx is done: matching first, so an intake
// accepted before a tick can be launched by it.
func (w *Worker) Run(ctx context.Context, every time.Duration) error {
	if err := w.validate(); err != nil {
		return err
	}
	if every <= 0 {
		return fmt.Errorf("intake: worker interval %s", every)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := w.MatchTick(ctx); err != nil {
			w.warn("intake: match tick: %v", err)
		}
		if _, err := w.LaunchTick(ctx); err != nil {
			w.warn("intake: launch tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (w *Worker) validate() error {
	switch {
	case w.Store == nil || w.Matcher == nil || w.Submitter == nil:
		return errors.New("intake: worker needs a store, a matcher and a submitter")
	case w.Owner == "":
		return errors.New("intake: worker needs an owner")
	}
	if err := w.Intakes.Validate(); err != nil {
		return fmt.Errorf("intake: intake policy: %w", err)
	}
	if err := w.Pairs.Validate(); err != nil {
		return fmt.Errorf("intake: pair policy: %w", err)
	}
	return nil
}

// window is the share of a claim the work under it may spend: half its TTL,
// so a worker that overruns returns — and the launch path's own rollback
// lands — before the claim expires and another worker can take the row.
func window(claimedUntil time.Time, ttl time.Duration) time.Time {
	return claimedUntil.Add(-ttl / 2)
}

// MatchTick claims due intakes and writes their pairs. It returns how many
// intakes it claimed.
func (w *Worker) MatchTick(ctx context.Context) (int, error) {
	if err := w.validate(); err != nil {
		return 0, err
	}
	rows, err := w.Store.ClaimIntakes(ctx, w.Owner, w.now(), w.Intakes.ClaimTTL, w.batch())
	if err != nil {
		return 0, fmt.Errorf("intake: claim intakes: %w", err)
	}
	for _, in := range rows {
		w.matchOne(ctx, in)
	}
	return len(rows), nil
}

func (w *Worker) matchOne(ctx context.Context, in Intake) {
	deadline := window(in.NotBefore, w.Intakes.ClaimTTL)
	if !w.now().Before(deadline) {
		// The batch outlived this row's share of its claim: give it back
		// unspent rather than race the worker that may take it.
		w.releaseIntake(ctx, in, "released unattempted: the claim's working window had passed")
		return
	}
	if w.Intakes.exhausted(in.Attempts) {
		w.buryIntake(ctx, in, in.Attempts, fmt.Sprintf("reclaimed %d times without completing", in.Attempts))
		return
	}
	mctx, cancel := context.WithTimeout(ctx, deadline.Sub(w.now()))
	verdicts, err := w.Matcher.Match(mctx, in)
	cancel()
	if err == nil {
		err = checkVerdicts(verdicts)
	}
	if err != nil {
		w.failIntake(ctx, in, err)
		return
	}
	// Each pair is keyed by (intake, subscription) and written once, so the
	// record never depends on the order the matcher answered in.
	now := w.now()
	pairs := PairsOf(in, verdicts, now)
	if err := w.Store.WritePairs(ctx, pairs); err != nil {
		w.failIntake(ctx, in, fmt.Errorf("write pairs: %w", err))
		return
	}
	if err := w.Store.MarkIntakeMatched(ctx, in.ID, in.Generation, now); err != nil {
		// Lost or unwritten, the pairs stand: a matcher that takes the
		// intake again writes nothing they do not already say.
		w.warn("intake: intake %s matched, but the mark did not land: %v", in.ID, err)
	}
}

// PairsOf is what an intake owes the candidates that judged it: a pending
// pair where a subscription matched, a refused one where its verdict could
// not be reached, a not_matched one otherwise. The matching worker writes
// them, and so does a poller's backfill, which judges primed and earlier
// intakes for one subscription.
func PairsOf(in Intake, verdicts []Verdict, now time.Time) []Pair {
	pairs := make([]Pair, 0, len(verdicts))
	for _, v := range verdicts {
		p := Pair{
			TenantID: in.TenantID, IntakeID: in.ID, SubscriptionID: v.SubscriptionID,
			ReceivedAt: in.ReceivedAt, ExpiresAt: in.ExpiresAt, CreatedAt: now,
		}
		switch {
		case v.Refusal != "":
			p.State, p.Reason = PairRefused, v.Refusal
		case v.Matched:
			p.State = PairPending
		default:
			p.State, p.Reason = PairNotMatched, v.Reason
			if p.Reason == "" {
				p.Reason = "the subscription's filters did not match"
			}
		}
		pairs = append(pairs, p)
	}
	return pairs
}

// checkVerdicts refuses an answer no pair set can record.
func checkVerdicts(vs []Verdict) error {
	seen := make(map[string]bool, len(vs))
	for _, v := range vs {
		if v.SubscriptionID == "" {
			return errors.New("matcher answered a verdict without a subscription")
		}
		if seen[v.SubscriptionID] {
			return fmt.Errorf("matcher answered subscription %s twice", v.SubscriptionID)
		}
		seen[v.SubscriptionID] = true
	}
	return nil
}

func (w *Worker) failIntake(ctx context.Context, in Intake, cause error) {
	attempts := in.Attempts + 1
	if w.Intakes.exhausted(attempts) {
		w.buryIntake(ctx, in, attempts, fmt.Sprintf("matching failed %d times: %v", attempts, cause))
		return
	}
	delay := w.Intakes.Delay(attempts)
	w.warn("intake: intake %s matching failed (attempt %d), retrying in %s: %v", in.ID, attempts, delay, cause)
	if err := w.Store.RetryIntake(ctx, in.ID, in.Generation, attempts, w.now().Add(delay), cause.Error()); err != nil {
		w.warn("intake: intake %s retry write: %v", in.ID, err)
	}
}

func (w *Worker) releaseIntake(ctx context.Context, in Intake, why string) {
	if err := w.Store.RetryIntake(ctx, in.ID, in.Generation, in.Attempts, w.now(), why); err != nil {
		w.warn("intake: intake %s release: %v", in.ID, err)
	}
}

func (w *Worker) buryIntake(ctx context.Context, in Intake, attempts int, why string) {
	w.warn("intake: intake %s parked as a dead letter: %s", in.ID, why)
	if err := w.Store.BuryIntake(ctx, in.ID, in.Generation, attempts, why); err != nil {
		w.warn("intake: intake %s dead-letter write: %v", in.ID, err)
	}
}

// LaunchTick claims due pairs and submits their launches. It returns how many
// pairs it claimed.
func (w *Worker) LaunchTick(ctx context.Context) (int, error) {
	if err := w.validate(); err != nil {
		return 0, err
	}
	rows, err := w.Store.ClaimPairs(ctx, w.Owner, w.now(), w.Pairs.ClaimTTL, w.batch())
	if err != nil {
		return 0, fmt.Errorf("intake: claim pairs: %w", err)
	}
	for _, p := range rows {
		w.launchOne(ctx, p)
	}
	return len(rows), nil
}

func (w *Worker) launchOne(ctx context.Context, p Pair) {
	deadline := window(p.NotBefore, w.Pairs.ClaimTTL)
	release := func() {
		w.retryPair(ctx, p, PairRetry{Attempts: p.Attempts, NotBefore: w.now(), LastError: "released unattempted: the claim's working window had passed", Epoch: p.Epoch})
	}
	if !w.now().Before(deadline) {
		release()
		return
	}
	if w.Pairs.exhausted(p.Attempts) {
		w.buryPair(ctx, p, p.Attempts, fmt.Sprintf("reclaimed %d times without completing", p.Attempts))
		return
	}
	// Everything under the claim spends its window: the read too, and the
	// window is judged again before the launch, never handed a spent one.
	rctx, rcancel := context.WithTimeout(ctx, deadline.Sub(w.now()))
	in, err := w.Store.GetIntake(rctx, p.TenantID, p.IntakeID)
	rcancel()
	if errors.Is(err, ErrNotFound) {
		w.refusePair(ctx, p, "its intake is gone (expired or removed)")
		return
	}
	if !w.now().Before(deadline) {
		release()
		return
	}
	if err != nil {
		w.failPair(ctx, p, fmt.Errorf("read intake %s: %w", p.IntakeID, err))
		return
	}
	// The remaining share of the window, as a duration: the worker's clock
	// decides the window, the context's timer only spends it.
	sctx, cancel := context.WithTimeout(ctx, deadline.Sub(w.now()))
	runID, err := w.Submitter.Submit(sctx, in, p)
	cancel()
	if err == nil && runID == "" {
		err = errors.New("the submission was acknowledged without a run id")
	}
	if err == nil {
		if merr := w.Store.MarkPairLaunched(ctx, p.ID, p.Generation, runID, w.now()); merr != nil {
			// The submission is acknowledged: a worker that takes the pair
			// again submits the same key and is answered with this run.
			w.warn("intake: pair %s launched run %s, but the mark did not land: %v", p.ID, runID, merr)
		}
		return
	}
	delay, postponed := postponeFor(err)
	switch reason, refused := refusalReason(err); {
	case refused:
		w.refusePair(ctx, p, reason)
	case errors.Is(err, schedgate.ErrMaintenanceHold):
		// Postponed, not attempted: the attempt budget stays whole.
		w.retryPair(ctx, p, PairRetry{Attempts: p.Attempts, NotBefore: w.now().Add(w.Pairs.Backoff), LastError: err.Error(), Epoch: p.Epoch})
	case postponed:
		// A transient denial — the team's concurrency cap, its launch rate —
		// is lifted by waiting, not by trying: the budget stays whole too.
		w.retryPair(ctx, p, PairRetry{Attempts: p.Attempts, NotBefore: w.now().Add(delay), LastError: err.Error(), Epoch: p.Epoch})
	case submissionSpent(err):
		// The run of this epoch's key never left the launch path and stays
		// as it is; the next attempt submits under the next key.
		w.failPairAt(ctx, p, err, p.Epoch+1)
	default:
		w.failPair(ctx, p, err)
	}
}

func (w *Worker) failPair(ctx context.Context, p Pair, cause error) {
	w.failPairAt(ctx, p, cause, p.Epoch)
}

func (w *Worker) failPairAt(ctx context.Context, p Pair, cause error, epoch int) {
	attempts := p.Attempts + 1
	if w.Pairs.exhausted(attempts) {
		w.buryPair(ctx, p, attempts, fmt.Sprintf("launch failed %d times: %v", attempts, cause))
		return
	}
	delay := w.Pairs.Delay(attempts)
	w.warn("intake: pair %s launch failed (attempt %d), retrying in %s: %v", p.ID, attempts, delay, cause)
	w.retryPair(ctx, p, PairRetry{Attempts: attempts, NotBefore: w.now().Add(delay), LastError: cause.Error(), Epoch: epoch})
}

func (w *Worker) retryPair(ctx context.Context, p Pair, r PairRetry) {
	if err := w.Store.RetryPair(ctx, p.ID, p.Generation, r); err != nil {
		w.warn("intake: pair %s retry write: %v", p.ID, err)
	}
}

func (w *Worker) refusePair(ctx context.Context, p Pair, reason string) {
	w.warn("intake: pair %s refused: %s", p.ID, reason)
	if err := w.Store.RefusePair(ctx, p.ID, p.Generation, reason); err != nil {
		w.warn("intake: pair %s refusal write: %v", p.ID, err)
	}
}

func (w *Worker) buryPair(ctx context.Context, p Pair, attempts int, why string) {
	w.warn("intake: pair %s parked as a dead letter: %s", p.ID, why)
	if err := w.Store.BuryPair(ctx, p.ID, p.Generation, attempts, why); err != nil {
		w.warn("intake: pair %s dead-letter write: %v", p.ID, err)
	}
}
