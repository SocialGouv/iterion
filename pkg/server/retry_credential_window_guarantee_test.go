package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The guarantee of the credential window (#3127, #3032), as the bko shape at a
// weekly wall: seven runs of one tenant parked on ONE credential's weekly wall
// whose reset the provider announced four days out, and one run parked on
// ANOTHER credential's wall as the control. The sweeper's real due and probe
// passes run on TWO replicas over one store — the twin fleet, one row per run
// — at the sweeper's own interval (retrySweepInterval) and at the quarter hour
// of the re-enqueue backoff, and each replica is replaced by a fresh instance
// mid-wait. Every assertion is read after every tick. The runner's half of
// every resume is the REAL engine (pkg/runtime) on a store of its own, its
// duration budget timed by the simulation's clock, and the two writes the
// runner makes on the retry row: the free re-arm of a park that followed a
// pull, and the probe's answer when a call on the wall's credential is served.
// The gate reconciler is offered every parked run on every tick, and each
// re-park on its own event.
//
// The wall falls early (a hand top-up) ten and a half hours in; two runs of
// the tenant belong to a bot redeployed under them half an hour later, so
// their resume fails whatever the wall does — one with the generic Resume
// error (the workflow hash moved), one with a publish that may have landed
// (runview.RunMayHaveStarted).

const (
	// guaranteeWork is the work each run did before it met the wall: the
	// minutes max_duration keeps counting.
	guaranteeWork = 30 * time.Minute
	// guaranteeAttempt is an attempt's approach to the wall: from its pod's
	// start to its first call's answer.
	guaranteeAttempt = 5 * time.Minute
	// guaranteeReplicaOffset is where the second replica's tick falls inside
	// the first one's interval.
	guaranteeReplicaOffset = 20 * time.Second
)

var (
	// guaranteeStart is the instant the scenario opens on, the bko shape: z.ai's
	// weekly [1310] wall met on 10/11, announced to reset on 14/11 at 17:24Z.
	guaranteeStart    = time.Date(2026, 11, 10, 13, 0, 0, 0, time.UTC)
	guaranteeZaiWall  = time.Date(2026, 11, 14, 17, 24, 0, 0, time.UTC)
	guaranteeKimiWall = time.Date(2026, 11, 14, 20, 0, 0, 0, time.UTC)
	// guaranteeTopUp is when the weekly wall falls by hand, and
	// guaranteeRedeploy when two runs' bot is redeployed under them.
	guaranteeTopUp    = guaranteeStart.Add(10*time.Hour + 30*time.Minute)
	guaranteeRedeploy = guaranteeStart.Add(11 * time.Hour)
)

// wallResume is one resume the run service was asked for.
type wallResume struct {
	runID string
	at    time.Time
	err   error
}

// wallResumer is the run service: the fleet's resumer, except for the runs
// whose bot was redeployed under them — their resume fails from that instant
// on, with the cause it would fail with.
type wallResumer struct {
	inner      *credFleetResumer
	clock      func() time.Time
	broken     map[string]error
	brokenFrom time.Time
	calls      []wallResume
}

func (r *wallResumer) Resume(ctx context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
	now := r.clock()
	if cause, ok := r.broken[spec.RunID]; ok && !now.Before(r.brokenFrom) {
		r.calls = append(r.calls, wallResume{runID: spec.RunID, at: now, err: cause})
		return nil, cause
	}
	res, err := r.inner.Resume(ctx, spec)
	r.calls = append(r.calls, wallResume{runID: spec.RunID, at: now, err: err})
	return res, err
}

// wallRunner executes each attempt with the real engine. Its executor is the
// provider: "prepare" works guaranteeWork, "review" makes the run's first call
// guaranteeAttempt after the pod starts, refused while the run's wall stands.
type wallRunner struct {
	t       *testing.T
	st      store.RunStore
	eng     *runtime.Engine
	now     time.Time
	current string
	serves  func(runID string, at time.Time) bool
	walls   map[string]time.Time
	// approach is how long the attempt running now takes to reach its call:
	// guaranteeAttempt for a resume, nothing for the attempt that parked the
	// run first (it met the wall right after its work, so whatever the
	// scenario's attempts do to the budget is theirs).
	approach time.Duration
	// walled counts the resumes checked parked on their work alone.
	walled int
}

func newWallRunner(t *testing.T, serves func(string, time.Time) bool, walls map[string]time.Time) *wallRunner {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("engine store: %v", err)
	}
	r := &wallRunner{t: t, st: st, serves: serves, walls: walls}
	wf := &ir.Workflow{
		Name: "weekly_wall", Entry: "prepare", Worktree: "none",
		Nodes: map[string]ir.Node{
			"prepare": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "prepare"}},
			"review":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "prepare", To: "review"}, {From: "review", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		// Two hours of active work allowed, against four days parked.
		Budget: &ir.Budget{MaxDuration: "120m"},
	}
	r.eng = runtime.New(wf, st, r, runtime.WithBudgetClock(func() time.Time { return r.now }))
	return r
}

func (r *wallRunner) Execute(_ context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	switch node.NodeID() {
	case "prepare":
		r.now = r.now.Add(guaranteeWork)
		return map[string]any{"ok": true, "_tokens": 1200}, nil
	case "review":
		r.now = r.now.Add(r.approach)
		if !r.serves(r.current, r.now) {
			return nil, &delegate.ErrRateLimited{
				Provider: "zai", Kind: delegate.RateLimitKindUsageWindow,
				Detail:  "429 · [1310] Weekly Limit Exhausted",
				ResetAt: r.walls[r.current],
			}
		}
		return map[string]any{"ok": true, "_tokens": 800}, nil
	}
	return map[string]any{"ok": true}, nil
}

// attempt runs one attempt of runID from its pod's start and reports the
// instant its call was answered and whether the wall refused it. A refused
// attempt is checked parked on its work alone.
func (r *wallRunner) attempt(runID string, start time.Time, first bool) (time.Time, bool) {
	r.t.Helper()
	r.current, r.now, r.approach = runID, start, guaranteeAttempt
	var err error
	if first {
		r.approach = 0
		err = r.eng.Run(context.Background(), runID, nil)
	} else {
		err = r.eng.Resume(context.Background(), runID, nil)
	}
	if err == nil {
		return r.now, false
	}
	var rt *runtime.RuntimeError
	if !errors.As(err, &rt) || rt.Code != runtime.ErrCodeUsageLimitBlocked {
		r.t.Fatalf("run %s: the attempt started at %s ended on %v, want its wall or its end", runID, start.Format(time.RFC3339), err)
	}
	r.parkedOnWorkAlone(runID, start, r.approach)
	return r.now, true
}

// parkedOnWorkAlone is guarantee 4 on the engine's record: the run's duration
// budget carries its work and nothing else — not the days it has been parked,
// not the attempts that only met the wall — and the attempt that just met it
// says so on the timeline (duration_refunded_ms, absent when it spent nothing).
func (r *wallRunner) parkedOnWorkAlone(runID string, start time.Time, spent time.Duration) {
	r.t.Helper()
	run, err := r.st.LoadRun(context.Background(), runID)
	if err != nil || run == nil || run.Checkpoint == nil {
		r.t.Fatalf("run %s parked with no checkpoint (%v)", runID, err)
	}
	if got := time.Duration(run.Checkpoint.BudgetElapsedNS); got != guaranteeWork {
		r.t.Fatalf("guarantee 4: run %s, parked again by its attempt of %s, carries %s on its duration budget, want the %s of work alone — the parked days and the attempts that only met the wall must not count against max_duration",
			runID, start.Format(time.RFC3339), got, guaranteeWork)
	}
	events, err := r.st.LoadEvents(context.Background(), runID)
	if err != nil {
		r.t.Fatal(err)
	}
	var refunded time.Duration
	for _, ev := range events {
		if ev.Type == store.EventRunFailed {
			refunded = 0
			if ms, ok := ev.Data["duration_refunded_ms"].(float64); ok {
				refunded = time.Duration(ms) * time.Millisecond
			}
		}
	}
	if refunded != spent {
		r.t.Fatalf("guarantee 4: run %s's attempt of %s met the wall and its run_failed event refunds %s, want duration_refunded_ms of the %s it spent meeting the wall",
			runID, start.Format(time.RFC3339), refunded, spent)
	}
	if spent > 0 {
		r.walled++
	}
}

// parkedGateForge is the forge the gate reconciler reads for the fleet's pull
// requests: each open, its head the revision its run reviews, no status on it
// — the shape a run judged dead gets a synthetic failure on. A parked run owes
// it no read and no write.
type parkedGateForge struct {
	t     *testing.T
	heads map[int]string
	reads []string
}

func (f *parkedGateForge) GetPullRequest(_ context.Context, repo string, number int) (forge.PullRef, error) {
	f.reads = append(f.reads, fmt.Sprintf("%s#%d", repo, number))
	return forge.PullRef{HeadSHA: f.heads[number], HeadRepoFullName: repo, State: "open"}, nil
}

func (f *parkedGateForge) ListCommitStatuses(context.Context, string, string) ([]forge.CommitStatus, error) {
	return nil, nil
}

func (f *parkedGateForge) SetCommitStatus(_ context.Context, repo, sha string, st forge.CommitStatus) error {
	f.t.Fatalf("guarantee 5: the gate reconciler posted %s %q on %s@%s — a run its retry still parks is not dead",
		st.State, st.Description, repo, sha)
	return nil
}

// weeklyWall is the world: the fleet, the two replicas, the run service, the
// runner, the gate reconciler, the clock, and what the assertions read.
type weeklyWall struct {
	t        *testing.T
	f        *credFleet
	replicas [2]*Server
	restarts int
	gate     *Server
	forge    *parkedGateForge
	resumer  *wallResumer
	runner   *wallRunner
	pending  []fleetAction

	runs      []string
	window    map[string]string
	announced map[string]time.Time

	seenPulls, seenResumes int
	// pullsOwed counts, per run, the pulls not yet followed by a resume:
	// before its announced instant a run is relaunched once per pull.
	pullsOwed map[string]int
	// lastCanary is each window's latest canary pull.
	lastCanary map[string]time.Time
	canaries   map[string]int
	// answered is the instant a window's canary answer landed on its row,
	// and the canary that gave it.
	answered   map[string]time.Time
	answeredBy map[string]string
	releases   []fleetPull
	// lastPull is each run's latest pull.
	lastPull map[string]fleetPull
	// failedAt is each broken run's latest failed resume before its date,
	// firstFailure its first; attempts every resume asked for each run.
	failedAt, firstFailure map[string]time.Time
	attempts               map[string][]wallResume
	// restartBeforeB asks for the second replica's replacement before its
	// next pass.
	restartBeforeB bool
}

func (w *weeklyWall) newReplica() *Server {
	s := newRetrySweeperServer(w.t, w.f, nil)
	s.retryClock = w.f.clock
	return s
}

// later applies do in the first pass at or after at.
func (w *weeklyWall) later(at time.Time, do func()) {
	w.pending = append(w.pending, fleetAction{at: at, do: do})
}

func (w *weeklyWall) applyDue(now time.Time) {
	var keep []fleetAction
	for _, a := range w.pending {
		if a.at.After(now) {
			keep = append(keep, a)
			continue
		}
		a.do()
	}
	w.pending = keep
}

// pass is one replica's tick at now: the runner's writes that are due, the
// due pass, the runner's half of what it resumed, then the probe pass —
// runRetrySweeper's order.
func (w *weeklyWall) pass(i int, now time.Time) {
	w.t.Helper()
	w.f.setNow(now)
	w.applyDue(now)
	if i == 1 && w.restartBeforeB {
		w.restart(1)
		w.restartBeforeB = false
	}
	w.replicas[i].sweepDueRetries(context.Background(), w.f, w.resumer, now)
	w.observeResumes(now)
	w.replicas[i].sweepRetryProbes(context.Background(), w.f, now)
	w.observePulls(now)
}

func (w *weeklyWall) restart(i int) {
	w.replicas[i] = w.newReplica()
	w.restarts++
}

// resumed is the runner's half of one resume at now: the engine runs the
// attempt; its call is answered guaranteeAttempt later, when the runner writes
// the retry row — the free re-arm of a refused call that followed a pull, or
// the probe's answer of a served call on an attempt that followed a pull whose
// wall is still ahead, and the run's end.
func (w *weeklyWall) resumed(id string, now time.Time) {
	w.t.Helper()
	watch := w.f.twin(id).Doc().RetryState.AnnouncedProbeFrom(now) != nil
	servedAt, walled := w.runner.attempt(id, now, false)
	if walled {
		w.later(servedAt, func() {
			w.f.meetsTheWallAgain(w.t, id)
			// The runner parks first, then fires the run's outcome event.
			if err := w.gate.reconcileGateForRunID(context.Background(), id, gateTriggerEvent); err != nil {
				w.t.Fatalf("gate reconcile of %s on its own event: %v", id, err)
			}
		})
		return
	}
	w.later(servedAt, func() {
		tw := w.f.twin(id)
		if watch {
			ok, err := tw.AnswerRunProbe(context.Background(), id, servedAt)
			if err != nil {
				w.t.Fatalf("answer of %s: %v", id, err)
			}
			if key := w.window[id]; ok && w.answered[key].IsZero() {
				w.answered[key], w.answeredBy[key] = w.f.clock(), id
				w.restartBeforeB = true
			}
		}
		tw.Mutate(func(run *store.Run) { run.Status = store.RunStatusFinished })
	})
}

// observeResumes reads the resumes the last due pass asked for.
func (w *weeklyWall) observeResumes(now time.Time) {
	w.t.Helper()
	for _, c := range w.resumer.calls[w.seenResumes:] {
		w.attempts[c.runID] = append(w.attempts[c.runID], c)
		before := c.at.Before(w.announced[c.runID])
		if before {
			// Guarantee 2: no relaunch before the date but the one a pull
			// bought — the window's canary, or a release on its answer.
			if w.pullsOwed[c.runID] == 0 {
				w.t.Fatalf("guarantee 2: run %s was relaunched at %s, before its announced %s, with no pull of its window behind it\nresumes: %v",
					c.runID, c.at.Format(time.RFC3339), w.announced[c.runID].Format(time.RFC3339), w.attempts[c.runID])
			}
			w.pullsOwed[c.runID] = 0
			// Guarantee 6: once a broken run's resume failed, nothing but its
			// window's canary pull relaunches it before its date.
			if failed := w.firstFailure[c.runID]; !failed.IsZero() && w.lastPull[c.runID].released {
				w.t.Fatalf("guarantee 6: run %s, whose resume failed at %s, was relaunched at %s on a release — after that failure only its window's canary may relaunch it before its date",
					c.runID, failed.Format(time.RFC3339), c.at.Format(time.RFC3339))
			}
		}
		if c.err == nil {
			w.resumed(c.runID, now)
			continue
		}
		if !before {
			continue
		}
		// Guarantee 6: a released run whose resume failed for a cause that
		// is no transient refusal stands down to the announced instant after
		// that ONE failure, free, its release consumed.
		row := w.f.twin(c.runID).Row()
		if row.RetryAfter == nil || !row.RetryAfter.Equal(w.announced[c.runID]) || row.ProbeFrom != nil || row.ReleasedBy != "" || row.Attempts != 1 {
			w.t.Fatalf("guarantee 6: run %s's resume failed at %s (%v) and its row is %s, want it stood down to its announced %s after that one failure, free of charge — not re-armed on the backoff",
				c.runID, c.at.Format(time.RFC3339), c.err, describeRow(row), w.announced[c.runID].Format(time.RFC3339))
		}
		if prev, ok := w.failedAt[c.runID]; ok && c.at.Sub(prev) < defaultRetryProbeFloor {
			w.t.Fatalf("guarantee 6: run %s's resume failed at %s and again at %s, within the probe floor — a cause that is no transient refusal must not loop on the backoff before the date",
				c.runID, prev.Format(time.RFC3339), c.at.Format(time.RFC3339))
		}
		w.failedAt[c.runID] = c.at
		if w.firstFailure[c.runID].IsZero() {
			w.firstFailure[c.runID] = c.at
		}
	}
	w.seenResumes = len(w.resumer.calls)
}

// observePulls reads the pulls the last probe pass landed.
func (w *weeklyWall) observePulls(now time.Time) {
	w.t.Helper()
	var released []string
	for _, p := range w.f.pulled[w.seenPulls:] {
		key := w.window[p.runID]
		w.pullsOwed[p.runID]++
		w.lastPull[p.runID] = p
		if !p.released {
			// Guarantee 1: one canary per window per floor, whichever replica
			// pulls it and across the restarts.
			if last, ok := w.lastCanary[key]; ok && p.at.Sub(last) < defaultRetryProbeFloor {
				w.t.Fatalf("guarantee 1: window %s pulled %s as its canary at %s, %s after the last canary at %s — at most one per hour",
					key, p.runID, p.at.Format(time.RFC3339), p.at.Sub(last), last.Format(time.RFC3339))
			}
			w.lastCanary[key] = p.at
			w.canaries[key]++
			continue
		}
		// Guarantee 3: a release is its own window's, never another's — the
		// control stays parked whatever the tenant's canary answered.
		if by := w.f.twin(p.runID).Row().ReleasedBy; w.window[by] != key {
			w.t.Fatalf("guarantee 3: run %s of window %s was released at %s on the answer of %s, of window %q — an answer says nothing about another credential's wall",
				p.runID, key, p.at.Format(time.RFC3339), by, w.window[by])
		}
		// Guarantee 2: no release while the window has no answer recorded.
		if w.answered[key].IsZero() {
			w.t.Fatalf("guarantee 2: run %s was released at %s on window %s, which has no answer recorded — the canary has not got past the wall",
				p.runID, p.at.Format(time.RFC3339), key)
		}
		released = append(released, p.runID)
		w.releases = append(w.releases, p)
	}
	w.seenPulls = len(w.f.pulled)
	if len(released) == 0 {
		return
	}
	// Guarantee 3: the pass right after the answer releases every OTHER run
	// of the window, in that one pass, and nothing of another window.
	key := w.window[released[0]]
	want := []string{}
	for _, id := range w.runs {
		if w.window[id] == key && id != w.answeredBy[key] {
			want = append(want, id)
		}
	}
	slices.Sort(released)
	if !slices.Equal(released, want) || !now.Equal(w.answered[key]) || len(w.releases) != len(want) {
		w.t.Fatalf("guarantee 3: the pass at %s released %v (all releases %v), want %v in the one pass after %s's answer landed at %s",
			now.Format(time.RFC3339), released, w.releases, want, w.answeredBy[key], w.answered[key].Format(time.RFC3339))
	}
}

// offerGates offers every parked run to the gate reconciler, as its sweep does.
func (w *weeklyWall) offerGates(now time.Time) {
	w.t.Helper()
	for _, id := range w.runs {
		doc := w.f.twin(id).Doc()
		if doc.Status != store.RunStatusFailedResumable || doc.ContinuationState != store.ContinuationRetryArmed {
			continue
		}
		if err := w.gate.reconcileGateForRunID(context.Background(), id, gateTriggerSweep); err != nil {
			w.t.Fatalf("gate reconcile of %s: %v", id, err)
		}
	}
	// Guarantee 5: a parked run is alive to the reconciler — not even a read.
	if len(w.forge.reads) > 0 {
		w.t.Fatalf("guarantee 5: at %s the gate reconciler read %v from the forge to repair a run its retry still parks", now.Format(time.RFC3339), w.forge.reads)
	}
}

// check is the state every run must hold after a tick.
func (w *weeklyWall) check(now time.Time) {
	w.t.Helper()
	for _, id := range w.runs {
		tw := w.f.twin(id)
		doc := tw.Doc()
		rs := doc.RetryState
		if now.Before(w.announced[id]) {
			// Guarantee 4: a parked run's attempt budget is its park's — the
			// canary's free re-arms, the holds, the releases and the
			// stand-downs spend none.
			if rs.Attempts != 1 || tw.PaidArmings != 0 {
				w.t.Fatalf("guarantee 4: at %s run %s has spent %d attempts (%d paid armings), want the 1 its park spent — nothing before its date reached past the wall on its account",
					now.Format(time.RFC3339), id, rs.Attempts, tw.PaidArmings)
			}
		}
		key := w.window[id]
		if w.answered[key].IsZero() && doc.Status == store.RunStatusFailedResumable && rs.ProbeFrom == nil {
			// Guarantee 2: while no answer is recorded every run of the
			// window that is not the canary in flight stays parked on its
			// announced instant.
			if rs.RetryAfter == nil || !rs.RetryAfter.Equal(w.announced[id]) || rs.ReleasedBy != "" {
				w.t.Fatalf("guarantee 2: at %s, with no answer recorded on window %s, run %s's row is %s, want it parked on its announced %s",
					now.Format(time.RFC3339), key, id, describeRow(rs), w.announced[id].Format(time.RFC3339))
			}
		}
		if key == w.window["ctl-1"] && rs.ReleasedBy != "" {
			w.t.Fatalf("guarantee 3: at %s the control %s carries a release by %s — another window's answer says nothing about its wall", now.Format(time.RFC3339), id, rs.ReleasedBy)
		}
	}
}

func describeRow(rs *store.RunRetryState) string {
	if rs == nil {
		return "<no retry state>"
	}
	at := func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("{retry_after:%s probe_from:%s released_by:%q last_probe_at:%s probe_count:%d attempts:%d}",
		at(rs.RetryAfter), at(rs.ProbeFrom), rs.ReleasedBy, at(rs.LastProbeAt), rs.ProbeCount, rs.Attempts)
}

func TestCredentialWindowGuarantee_SevenRunsOnAWeeklyWall(t *testing.T) {
	// The deployment's own bounds, whatever the shell exports.
	t.Setenv(EnvRetryProbeFloor, "1h")
	t.Setenv(EnvRetryProbeBudget, "24")
	t.Setenv(EnvRetryProbeBeforeWindow, "false")
	t.Setenv(EnvRetryCredentialProbeScan, "")
	t.Setenv(EnvUsageParkSettleWindow, "")
	// Every minute is the sweeper's own interval; every quarter hour is the
	// cadence of the backoff a failed resume used to loop on.
	for _, cadence := range []time.Duration{retrySweepInterval, retryReenqueueBackoff} {
		t.Run("ticks every "+cadence.String(), func(t *testing.T) { runWeeklyWall(t, cadence) })
	}
}

// guaranteeBudget is the deployment's probe budget the scenario runs under.
const guaranteeBudget = 24

func runWeeklyWall(t *testing.T, cadence time.Duration) {
	zai, kimi := []string{"fp-zai-weekly"}, []string{"fp-kimi"}
	runs := []string{"bko-1", "bko-2", "bko-3", "bko-4", "bko-5", "bko-6", "bko-7", "ctl-1"}
	creds := map[string][]string{"ctl-1": kimi}
	walls := map[string]time.Time{"ctl-1": guaranteeKimiWall}
	for _, id := range runs[:7] {
		creds[id], walls[id] = zai, guaranteeZaiWall
	}
	hashMoved := errors.New("resume refused: the bot was redeployed and the workflow hash moved since the run's checkpoint — resume with force to run the new revision")
	mayHaveLanded := &runview.QueueUnavailableError{Cause: errors.New("publish acknowledged after its deadline — the message may have landed")}
	if runview.RunMayHaveStarted(hashMoved) || !runview.RunMayHaveStarted(mayHaveLanded) {
		t.Fatal("premise: the generic Resume error starts nothing, the late publish may have started the run")
	}
	broken := map[string]error{"bko-3": hashMoved, "bko-6": mayHaveLanded}

	f := newCredFleet(guaranteeStart)
	w := &weeklyWall{
		t: t, f: f, runs: runs,
		window: map[string]string{}, announced: map[string]time.Time{},
		pullsOwed: map[string]int{}, lastPull: map[string]fleetPull{},
		lastCanary: map[string]time.Time{}, canaries: map[string]int{},
		answered: map[string]time.Time{}, answeredBy: map[string]string{},
		failedAt: map[string]time.Time{}, firstFailure: map[string]time.Time{},
		attempts: map[string][]wallResume{},
	}
	inner := &credFleetResumer{f: f, restamp: func(id string) []string { return creds[id] }}
	w.resumer = &wallResumer{inner: inner, clock: f.clock, broken: broken, brokenFrom: guaranteeRedeploy}
	w.runner = newWallRunner(t, func(id string, at time.Time) bool {
		if creds[id][0] == zai[0] && !at.Before(guaranteeTopUp) {
			return true
		}
		return !at.Before(walls[id])
	}, walls)

	// Each run worked, then met its wall: the engine's first attempt, then the
	// runner's paid park on the provider's announced instant — earliest-parked
	// first, bko-1 seventy minutes before the scenario opens.
	for i, id := range runs {
		parkedAt := guaranteeStart.Add(-time.Duration(70-10*i) * time.Minute)
		if _, walled := w.runner.attempt(id, parkedAt.Add(-guaranteeWork), true); !walled {
			t.Fatalf("premise: run %s's first attempt did not meet its wall", id)
		}
		f.park(id, creds[id], parkedAt, walls[id])
		w.window[id] = f.twin(id).Row().CredentialWindow()
		w.announced[id] = *f.twin(id).Row().RetryAfter
	}
	if w.window["bko-1"] == w.window["ctl-1"] || w.window["bko-1"] == "" {
		t.Fatalf("premise: the control's window %q must differ from the tenant's %q", w.window["ctl-1"], w.window["bko-1"])
	}
	w.gate, w.forge = newParkedGate(t, f, runs)
	w.replicas = [2]*Server{w.newReplica(), w.newReplica()}

	// A replica is replaced by a fresh instance after the first pass at or
	// past each of these instants (and the second one again in the pass the
	// canary's answer lands in).
	restarts := []struct {
		at      time.Time
		replica int
	}{
		{guaranteeStart.Add(3 * time.Hour), 1}, // a canary just pulled, its probe in flight
		{guaranteeStart.Add(48 * time.Hour), 0},
		{w.announced["bko-1"], 1},
	}
	end := w.announced["ctl-1"].Add(2 * time.Hour)
	for tick := guaranteeStart; !tick.After(end); tick = tick.Add(cadence) {
		w.pass(0, tick)
		if len(restarts) > 0 && !tick.Before(restarts[0].at) {
			w.restart(restarts[0].replica)
			restarts = restarts[1:]
		}
		w.pass(1, tick.Add(guaranteeReplicaOffset))
		w.offerGates(tick.Add(guaranteeReplicaOffset))
		w.check(tick.Add(guaranteeReplicaOffset))
	}

	// The scenario happened as staged.
	zaiKey := w.window["bko-1"]
	canary := w.answeredBy[zaiKey]
	if canary == "" || broken[canary] != nil {
		t.Fatalf("premise: the weekly wall's canary that got through = %q, want one of the runs whose resume works", canary)
	}
	if len(w.releases) != 6 {
		t.Fatalf("guarantee 3: %d runs released over the scenario (%v), want the 6 others of the canary's window", len(w.releases), w.releases)
	}
	if w.restarts < 4 {
		t.Fatalf("premise: %d replica restarts, want the 4 staged", w.restarts)
	}
	// Guarantee 6, bounded: after its one failure a broken run is resumed
	// before its date only by its window's canary pulls — one floor apart at
	// least (observeResumes), so never on the backoff — and the window stops
	// probing at its budget.
	var afterFailure []wallResume
	for id := range broken {
		if w.firstFailure[id].IsZero() {
			t.Fatalf("premise: run %s's resume never failed", id)
		}
		for _, a := range w.attempts[id] {
			if a.at.After(w.firstFailure[id]) && a.at.Before(w.announced[id]) {
				afterFailure = append(afterFailure, a)
			}
		}
	}
	if len(afterFailure) > guaranteeBudget {
		t.Fatalf("guarantee 6: the broken runs were resumed %d times between their failure and their date (%v), want at most the window's %d probes",
			len(afterFailure), afterFailure, guaranteeBudget)
	}
	for _, id := range runs {
		doc := f.twin(id).Doc()
		switch {
		case broken[id] != nil:
			// Past its date a resume that keeps failing is an ordinary due
			// retry: charged, and retired at the attempt budget.
			if doc.ContinuationState != store.ContinuationFinal || doc.RetryState.Attempts != 5 || len(f.twin(id).Abandons) != 1 {
				t.Fatalf("run %s, whose resume fails for good, ends %s/%s with %d attempts and abandons %v, want retired at its budget of 5 past its date",
					id, doc.Status, doc.ContinuationState, doc.RetryState.Attempts, f.twin(id).Abandons)
			}
		default:
			if doc.Status != store.RunStatusFinished {
				t.Fatalf("run %s ends %s, want finished", id, doc.Status)
			}
			run, err := w.runner.st.LoadRun(context.Background(), id)
			if err != nil || run.Status != store.RunStatusFinished {
				t.Fatalf("run %s's engine record ends %v (%v), want finished under its max_duration", id, run, err)
			}
		}
	}
	var ctlAtDate []wallResume
	for _, a := range w.attempts["ctl-1"] {
		if !a.at.Before(w.announced["ctl-1"]) {
			ctlAtDate = append(ctlAtDate, a)
		}
	}
	if len(ctlAtDate) != 1 {
		t.Fatalf("the control resumed %v at or after its announced %s, want once", ctlAtDate, w.announced["ctl-1"].Format(time.RFC3339))
	}
	t.Logf("canaries per window: %v; walled attempts parked on their work alone: %d; released in one pass: %v; broken runs' resumes between their failure and their date: %d (each a canary pull of their window, one floor apart); replica restarts: %d",
		w.canaries, w.runner.walled, w.releases, len(afterFailure), w.restarts)
}

// newParkedGate is the gate reconciler over the fleet: each run gates its own
// pull request through a grant of its tenant, on a connection whose forge is
// parkedGateForge.
func newParkedGate(t *testing.T, f *credFleet, runs []string) (*Server, *parkedGateForge) {
	t.Helper()
	s := newForgeGateTestServer(t, f)
	s.gateClock = f.clock
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "conn-team-1", TenantID: "team-1", Provider: forge.ProviderGitHub,
	}); err != nil {
		t.Fatal(err)
	}
	fg := &parkedGateForge{t: t, heads: map[int]string{}}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return fg, nil }
	for i, id := range runs {
		n, tok := 100+i, "tok-"+id
		registerPublishToken(t, s, tok, ForgePublishGrant{TeamID: "team-1", ConnectionID: "conn-team-1", Repo: "o/r", Bot: "review-pr"})
		head := fmt.Sprintf("%040x", n)
		fg.heads[n] = head
		f.twin(id).Mutate(func(run *store.Run) {
			run.Inputs = map[string]any{
				"pr_url":             fmt.Sprintf("https://github.com/o/r/pull/%d", n),
				forgePublishVarToken: tok,
				"gate_context":       "iterion/review",
				"head_sha":           head,
				"forge_publish_url":  "https://iterion.test/api/v1/forge/publish-review",
			}
		})
	}
	return s, fg
}
