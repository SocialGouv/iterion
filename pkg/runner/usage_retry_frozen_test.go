package runner

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A usage-window park of a started run records the program its delivery
// carried (ADR-167 T4a), before whichever arming lands, bound to the attempt
// the delivery belongs to: the retry sweeper resumes the bot's current
// program, and this one only when the current one is refused because its
// source changed. A run that never started records nothing; a record that
// cannot be written is said and counted and never stops the arming.

// usageMsg is a delivery naming runID and carrying no program: what the
// arming tests that do not look at the freeze hand the park.
func usageMsg(runID string) *queue.RunMessage { return &queue.RunMessage{RunID: runID} }

// usageRecordWrite is one SetRunFrozenProgram the park asked for.
type usageRecordWrite struct {
	runID    string
	queuedAt time.Time
	fp       store.FrozenProgram
}

// frozenUsageStore is the arming store with an IR blob store and a
// frozen-program writer, logging the record write and the armings in the
// order the park asked for them.
type frozenUsageStore struct {
	*cancelAwareStore
	*runnerIRBlobs
	mu       sync.Mutex
	order    []string
	writes   []usageRecordWrite
	setErr   error
	setMoved bool
	// setDelay holds the record write that long, a slow store; armBudget is
	// what was left of the arming's context when it asked to arm.
	setDelay  time.Duration
	armBudget time.Duration
}

func (s *frozenUsageStore) Unwrap() store.RunStore { return s.cancelAwareStore }

func (s *frozenUsageStore) note(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, step)
}

func (s *frozenUsageStore) steps() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func (s *frozenUsageStore) SetRunFrozenProgram(ctx context.Context, runID string, queuedAt time.Time, fp *store.FrozenProgram) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.note("record")
	time.Sleep(s.setDelay)
	if s.setErr != nil {
		return false, s.setErr
	}
	if s.setMoved {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := *fp
	rec.QueuedAt = queuedAt
	s.writes = append(s.writes, usageRecordWrite{runID: runID, queuedAt: queuedAt, fp: rec})
	return true, nil
}

func (s *frozenUsageStore) ScheduleRunRetry(ctx context.Context, runID string, at, wall time.Time, reason, code string, maxAttempts int) (bool, int, error) {
	s.note("schedule")
	if deadline, ok := ctx.Deadline(); ok {
		s.mu.Lock()
		s.armBudget = time.Until(deadline)
		s.mu.Unlock()
	}
	return s.cancelAwareStore.ScheduleRunRetry(ctx, runID, at, wall, reason, code, maxAttempts)
}

func (s *frozenUsageStore) RearmProbedRunRetry(ctx context.Context, runID string, at, wall time.Time) (bool, error) {
	s.note("rearm")
	return s.cancelAwareStore.RearmProbedRunRetry(ctx, runID, at, wall)
}

func (s *frozenUsageStore) recordWrites() []usageRecordWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usageRecordWrite(nil), s.writes...)
}

// usageFrozenFixture is a started run (a checkpoint, the transport's hash, an
// attempt marker) parked on a usage window by a delivery that carries the
// transport IR and belongs to that attempt. shape adjusts the run before the
// park (probe marker, attempts, started or not).
type usageFrozenFixture struct {
	st    *frozenUsageStore
	blobs *runnerIRBlobs
	r     *Runner
	reg   *metrics.Registry
	msg   *queue.RunMessage
	body  []byte
	q1    time.Time
}

func newUsageFrozenFixture(t *testing.T, shape func(run *store.Run)) *usageFrozenFixture {
	t.Helper()
	body, hash := frozenTransport(t)
	q1 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	run := &store.Run{
		ID: "run-usage", TenantID: "team-1", Status: store.RunStatusFailedResumable,
		WorkflowHash: hash, QueuedAt: &q1, Checkpoint: &store.Checkpoint{NodeID: "one"},
		RetryPolicy: &store.RunRetryPolicy{UsageWindow: "resume", MaxAttempts: 5, MaxWait: "192h", Jitter: "0s"},
	}
	if shape != nil {
		shape(run)
	}
	blobs := &runnerIRBlobs{}
	st := &frozenUsageStore{cancelAwareStore: &cancelAwareStore{run: run}, runnerIRBlobs: blobs}
	reg := metrics.New()
	return &usageFrozenFixture{
		st: st, blobs: blobs, reg: reg, body: body, q1: q1,
		r: &Runner{cfg: Config{Store: st, Metrics: reg}},
		msg: &queue.RunMessage{
			RunID: "run-usage", TenantID: "team-1", OwnerID: "u1",
			WorkflowName: "frozen", WorkflowHash: hash, IRCompiled: body,
			PublishedAtRFC: q1.Add(time.Second).Format(time.RFC3339Nano),
		},
	}
}

func (f *usageFrozenFixture) park(t *testing.T, spent bool) usageRetryOutcome {
	t.Helper()
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	return f.r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), f.msg, spent, iterlog.New(iterlog.LevelError, io.Discard))
}

func (f *usageFrozenFixture) count(t *testing.T, outcome string) float64 {
	t.Helper()
	return counterValue(t, f.reg.CapacityFrozenProgramTotal.WithLabelValues(store.FrozenProgramSourceUsageWindow, outcome))
}

// probedShape is a row a pull-forward probe moved: the announced instant set
// aside, the pull just made, attempts spent as given.
func probedShape(attempts int) func(*store.Run) {
	return func(run *store.Run) {
		announced := time.Now().UTC().Add(4 * 24 * time.Hour)
		pulled := time.Now().UTC()
		run.RetryState = &store.RunRetryState{
			RetryAfter: &pulled, ProbeFrom: &announced, Wall: &announced, LastProbeAt: &pulled,
			ProbeCount: 1, Attempts: attempts, Reason: store.RetryReasonUsageWindow, Code: "USAGE_LIMIT_BLOCKED",
		}
	}
}

// Every arming a usage park can land on is preceded by the record of the
// delivered program: the paid arming, the free re-arm that follows a probe,
// and the free restore of the announced instant after a refused paid arming.
// The record binds the attempt the delivery belongs to (the run's queued_at),
// names the usage-window source and an envelope holding the delivered IR, and
// is counted. Red when the freeze is dropped, or when it follows the armings
// (or only one of them).
func TestUsagePark_RecordsTheProgramBeforeEveryArming(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(*store.Run)
		spent bool
		order []string
	}{
		{"paid arming", nil, spentNothing, []string{"record", "schedule"}},
		{"free re-arm after a probe", probedShape(2), spentNothing, []string{"record", "rearm"}},
		{"restored instant after a refused paid arming", probedShape(5), spentTokens, []string{"record", "schedule", "rearm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUsageFrozenFixture(t, tc.shape)
			if got := f.park(t, tc.spent); got != usageRetryArmed {
				t.Fatalf("outcome %v, want armed", got)
			}
			if got := f.st.steps(); len(got) != len(tc.order) || !equalSteps(got, tc.order) {
				t.Fatalf("the park wrote %v, want %v — the program recorded before the arming", got, tc.order)
			}
			writes := f.st.recordWrites()
			if len(writes) != 1 {
				t.Fatalf("record writes %+v, want one", writes)
			}
			w := writes[0]
			if w.runID != "run-usage" || !w.queuedAt.Equal(f.q1) || w.fp.Source != store.FrozenProgramSourceUsageWindow {
				t.Fatalf("record %+v, want run-usage bound to the delivery's attempt %v, source usage_window", w, f.q1)
			}
			env, err := runview.LoadFrozenProgram(context.Background(), f.blobs, "run-usage", &w.fp)
			if err != nil {
				t.Fatalf("the record names no readable envelope: %v", err)
			}
			if string(env.IR) != string(f.body) || env.WorkflowHash != f.msg.WorkflowHash {
				t.Fatalf("envelope hash %s, want the delivered program %s", env.WorkflowHash, f.msg.WorkflowHash)
			}
			if n := f.count(t, metrics.FrozenProgramWritten); n != 1 {
				t.Fatalf("written count %v, want 1", n)
			}
		})
	}
}

func equalSteps(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return len(a) == len(b)
}

// A run that never started — no checkpoint, no recorded attempt — has no
// program to keep: its usage park records nothing, is counted not_started,
// and arms as before. Red when every usage park freezes.
func TestUsagePark_ARunThatNeverStartedRecordsNothing(t *testing.T) {
	f := newUsageFrozenFixture(t, func(run *store.Run) { run.Checkpoint, run.Attempts = nil, nil })
	if got := f.park(t, spentNothing); got != usageRetryArmed {
		t.Fatalf("outcome %v, want armed", got)
	}
	if writes := f.st.recordWrites(); len(writes) != 0 {
		t.Fatalf("a run that never started recorded %+v", writes)
	}
	if n := f.count(t, metrics.FrozenProgramNotStarted); n != 1 {
		t.Fatalf("not_started count %v, want 1", n)
	}
}

// A record that does not land never holds the arming back: a legacy bundle
// with no snapshot digest, a blob store that refuses the envelope, a
// delivered program that is not the run's, a record write the store refuses
// or finds the run moved for, a store with no frozen-program writer, a
// delivery whose attempt is no longer the run's, a run with no attempt marker
// to bind to. Each arms the retry and is counted. Red when a freeze failure
// stops the arming, or when one of these is passed over without a count.
func TestUsagePark_ARecordThatDoesNotLandStillArms(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome string
		setup   func(t *testing.T, f *usageFrozenFixture)
	}{
		{"legacy digest-less bundle", metrics.FrozenProgramFreezeFailed, func(_ *testing.T, f *usageFrozenFixture) {
			f.msg.BotBundle = &queue.BotBundleRef{TenantID: "team-1", Slug: "bot", Version: 2}
		}},
		{"envelope write refused", metrics.FrozenProgramFreezeFailed, func(_ *testing.T, f *usageFrozenFixture) {
			f.blobs.putErr = errors.New("s3: access denied")
		}},
		{"a delivered program that is not the run's", metrics.FrozenProgramHashMismatch, func(_ *testing.T, f *usageFrozenFixture) {
			f.st.run.WorkflowHash = "sha256:another-revision"
		}},
		{"record write refused", metrics.FrozenProgramFreezeFailed, func(_ *testing.T, f *usageFrozenFixture) {
			f.st.setErr = errors.New("mongo: not primary")
		}},
		{"record write finds the run moved", metrics.FrozenProgramFreezeFailed, func(_ *testing.T, f *usageFrozenFixture) {
			f.st.setMoved = true
		}},
		{"no frozen-program writer", metrics.FrozenProgramFreezeFailed, func(_ *testing.T, f *usageFrozenFixture) {
			f.r.cfg.Store = &blobArmingStore{cancelAwareStore: f.st.cancelAwareStore, runnerIRBlobs: f.blobs}
		}},
		{"a delivery older than the run's attempt", metrics.FrozenProgramFreezeFailed, func(_ *testing.T, f *usageFrozenFixture) {
			f.msg.PublishedAtRFC = f.q1.Add(-time.Second).Format(time.RFC3339Nano)
		}},
		{"a run with no attempt marker", metrics.FrozenProgramFreezeFailed, func(_ *testing.T, f *usageFrozenFixture) {
			f.st.run.QueuedAt = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUsageFrozenFixture(t, nil)
			tc.setup(t, f)
			if got := f.park(t, spentNothing); got != usageRetryArmed {
				t.Fatalf("outcome %v, want armed: a record that did not land stopped the arming", got)
			}
			if !f.st.armed {
				t.Fatal("the retry was not armed")
			}
			if writes := f.st.recordWrites(); len(writes) != 0 {
				t.Fatalf("records %+v, want none", writes)
			}
			if n := f.count(t, tc.outcome); n != 1 {
				t.Fatalf("%s count %v, want 1", tc.outcome, n)
			}
		})
	}
}

// blobArmingStore is the arming store with IR blobs and no frozen-program
// writer.
type blobArmingStore struct {
	*cancelAwareStore
	*runnerIRBlobs
}

func (s *blobArmingStore) Unwrap() store.RunStore { return s.cancelAwareStore }

// The record's writes run on a budget of their own and the arming gets its
// own afresh: a slow record write leaves the arming its whole store budget,
// never what the freeze left of it — an arming on an expired context fails,
// and the park falls back to a redelivery. Red when the freeze and the arming
// share one budget.
func TestUsagePark_TheArmingKeepsItsWholeBudgetAfterTheFreeze(t *testing.T) {
	f := newUsageFrozenFixture(t, nil)
	f.st.setDelay = 1500 * time.Millisecond
	if got := f.park(t, spentNothing); got != usageRetryArmed {
		t.Fatalf("outcome %v, want armed", got)
	}
	f.st.mu.Lock()
	left := f.st.armBudget
	f.st.mu.Unlock()
	if floor := usageRetryStoreTimeout - time.Second; left < floor {
		t.Fatalf("the arming had %s of its store budget left after a %s record write, want at least %s", left, f.st.setDelay, floor)
	}
}
