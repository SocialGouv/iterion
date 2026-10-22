package runner

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/retrycoord"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The wall is an IDENTITY, not an estimate: four readers dedup on it (the run
// owner's notification, the operator's incident, the pull request's pause
// notice and the probe budget), so two parks on one provider window have to
// agree on it. Only the provider's own DATED word moves it.
//
// The shape that forced this rule: a refusal naming a window DURATION
// ("reached for 5 hour") resolves against the moment it is read
// (pkg/backend/delegate.resetWindowRe → now+N), so trusting it re-dated the
// wall on every park — a new episode, a new incident, a new comment and a
// fresh probe budget each time, for one window nobody had news about.
//
// usageRefusalWall answers "did the provider DATE this" by reading the same
// refusal twice a second apart and comparing, rather than by listing the
// shapes it knows: the list is open (four patterns today), and a pattern added
// to delegate.ParseResetHint tomorrow has to land on the safe side without
// anyone remembering this file.

// sweepTick mirrors pkg/server.retrySweepInterval: the probe pass runs once a
// minute, which is the cadence an unpaced row is pulled at.
const sweepTick = time.Minute

func TestUsageParkWall_OnlyTheProvidersOwnDateIsNewsAboutTheWall(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	dated := now.Add(96 * time.Hour)
	held := now.Add(72 * time.Hour)
	stale := now.Add(-2 * time.Hour)

	cases := []struct {
		name     string
		execErr  error
		at       time.Time
		held     time.Time
		restored bool
		want     time.Time
		why      string
	}{
		{
			name:    "a dated refusal is news, whatever the row holds",
			execErr: weeklyWindowErr(dated),
			at:      now.Add(time.Hour), held: held,
			want: dated,
			why:  "the provider named an instant and it is not the one the row is on: a different wait, probed afresh, and the pull request hears it — the wall is the refusal's own instant, never the come-back pad the wake carries",
		},
		{
			name:    "a window duration on a row already on a wall is no news",
			execErr: windowDurationErr(),
			at:      now.Add(5 * time.Hour), held: held,
			want: time.Time{},
			why:  "the instant it resolves to is recomputed from now, so believing it would re-date the wall on every park",
		},
		{
			name:    "the operator's OWN cap window is news too",
			execErr: selfImposedCapErr(dated),
			at:      now.Add(time.Hour), held: held,
			want: dated,
			why:  "a refusal iterion raises carries the cap window's own instant, and pkg/usagecap renders it into the reason — so the rule that reads the refusal's words must not mistake a real wall for a reading-relative guess",
		},
		{
			// The zai facade's captured sentence: duration AND absolute
			// instant, the instant within the tolerance of now+duration. Both
			// facts in one text is what a producer's own rendering of the
			// duration looks like — no news about where the wall is.
			name:    "the captured facade rendering (duration + coherent instant) is no news",
			execErr: facadeWindowErr(now),
			at:      now.Add(5 * time.Hour), held: held,
			want: time.Time{},
			why:  "the instant it names is the duration's own rendering — a reading a minute later answers an instant a minute later, and believing it would re-date the wall on every park",
		},
		{
			// The same sentence whose instant is NOT the rendering of the
			// duration beside it: a refusal hours INTO the window it names
			// reads the remaining time as strictly less than the duration.
			name:    "a captured-shaped refusal whose instant is far from now+duration is news",
			execErr: facadeWindowErrAt(now, now.Add(70*time.Hour)),
			at:      now.Add(time.Hour), held: held,
			want: now.Add(70 * time.Hour),
			why:  "the instant stands without being the duration's rendering: a real dated wall refused mid-window, with the stale 5-hour sentence still in the text",
		},
		{
			// The multi-day wall's capture: an absolute instant and NO
			// duration in the sentence. The provider's own word, and the
			// tolerance has nothing to check it against.
			name:    "the weekly/monthly wall's capture (absolute alone) is news",
			execErr: weeklyWallCaptureErr(now),
			at:      now.Add(time.Hour), held: held,
			want: now.Add(48 * time.Hour),
			why:  "an absolute instant with no duration beside it is the provider's own date — the sentence recomputes nothing",
		},
		{
			name:    "the same window duration, TYPED, is no news either",
			execErr: typedWindowDurationErr(now),
			at:      now.Add(5 * time.Hour), held: held,
			want: time.Time{},
			why:  "the claude_code path delivers that refusal as the typed error, whose ResetAt is the instant the classifier resolved from the same words at the moment it read them: frozen, so reading it twice cannot see what it was resolved FROM",
		},
		{
			name:    "a non-usage error brings no wall at all",
			execErr: errors.New("node \"review\" exited 1"),
			at:      now.Add(time.Hour),
			want:    time.Time{},
			why:     "both call sites have already established this error IS a usage refusal; a caller that has not gets the instant nobody named written as the identity four readers dedup on",
		},
		{
			name:    "a blind refusal on a row already on a wall is no news",
			execErr: blindUsageWindowErr(),
			at:      now.Add(time.Hour), held: held,
			want: time.Time{},
			why:  "the bounded guess is this park's, not the window's",
		},
		{
			name:     "a restore on a row already on a wall is no news",
			execErr:  blindUsageWindowErr(),
			at:       held,
			held:     held,
			restored: true,
			want:     time.Time{},
			why:      "putting the held-aside instant back re-states the wall the row is on",
		},
		{
			name:    "a window duration on a WALL-LESS row names the wait it opens",
			execErr: windowDurationErr(),
			at:      now.Add(5 * time.Hour).Add(7 * time.Minute), // the jitter the policy adds
			want:    now.Add(5 * time.Hour),
			why:     "a row with no wall must gain one or its owner keeps being told the run FAILED — and it is the refusal's own instant, not `at`, which also carries the jitter and a skipped credential's wake",
		},
		{
			name:     "a restore on a WALL-LESS row names the instant it puts back",
			execErr:  blindUsageWindowErr(),
			at:       held,
			restored: true,
			want:     held,
			why:      "the legacy row (armed before the wall was persisted): the held-aside instant is the only one it has, and the alternative leaves it on the per-park failure notification for the whole wait",
		},
		{
			name:    "a blind refusal on a row whose wall has PASSED takes this park's guess",
			execErr: blindUsageWindowErr(),
			at:      now.Add(time.Hour), held: stale,
			want: now.Add(usageWindowBlindWait),
			why:  "the window that wall named is over — the run may well have worked since — so it is spent news and this park is a new wait",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := usageParkWall(tc.execErr, tc.at, now, tc.held, tc.restored)
			if !got.Equal(tc.want) {
				t.Fatalf("usageParkWall = %s, want %s: %s", instantOrNone(got), instantOrNone(tc.want), tc.why)
			}
		})
	}
}

// redatingProbeLoop drives the sweeper's probe pass over `ticks` one-minute
// passes against a provider whose refusal names a window DURATION, so every
// reading of its window answers a later instant. It returns the pulls (one pod
// each), the attempts charged and the distinct walls the row was ever parked
// on — which is the number of notifications, incidents and pull-request
// comments one provider window produced.
// `refuse` builds that refusal as it is read at a given instant, so the loop
// drives the shape the provider sends. The two deliveries of ONE refusal —
// the flattened text and the typed error the claude_code backend builds —
// must come out as the same single wall (pinned beside), and the refusals
// this loop is for re-date themselves against the moment of READING, which is
// the decision's own clock. A refusal whose datedness is read against a
// different clock than the one it was rendered against is a different shape —
// an aged rendering — with its own witnesses; driving it here would test the
// loop, not the provider.
func redatingProbeLoop(t *testing.T, ticks int, refuse func(time.Time) error) (pulls, attempts, walls int) {
	t.Helper()
	ctx := context.Background()
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	w := newProbeWorld()
	now := time.Now().UTC()

	if out := w.r.armUsageWindowRetry(ctx, refuse(now), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the park that opens the wall: %v", out)
	}
	// The walls this window is ever recorded as. The arming reads the REAL
	// clock, while these passes advance a synthetic one, so the row alone
	// cannot tell a stable wall from a re-dated one in a test that runs in
	// microseconds: what each pass is credited with is the wall the
	// production decision returns AT THAT PASS'S INSTANT, for the row as it
	// then stands. Called, not modelled.
	seen := map[int64]bool{}
	if wall := w.st.Row().UsageWall(); !wall.IsZero() {
		seen[wall.Unix()] = true
	}
	decide := func(at, scan time.Time) {
		if wall := usageParkWall(refuse(scan), at, scan, w.st.Row().UsageWall(), false); !wall.IsZero() {
			seen[wall.Unix()] = true
		}
	}
	scan := now
	for i := 0; i < ticks; i++ {
		rows := w.st.ProbeRows(scan, probePropertyFloor, probePropertyProbeBudget, 2)
		if len(rows) == 1 && rows[0].RetryAfter != nil {
			pulled, err := w.st.PullRunRetry(ctx, w.runID, *rows[0].RetryAfter, scan)
			if err != nil || !pulled {
				t.Fatalf("tick %d: pull = %v, %v", i, pulled, err)
			}
			pulls++
			// PullRunRetry stamps last_probe_at from the REAL clock while
			// these passes advance a synthetic one: restamp it to the pass's
			// instant, which is what the store writes in production, so the
			// scan's pace term reads one coherent clock.
			w.st.Mutate(func(r *store.Run) {
				stamped := scan.Truncate(time.Millisecond)
				r.RetryState.LastProbeAt = &stamped
			})
			// The due pass claims and resumes it; the resume clears the armed
			// instant and keeps the marker, then the pod's preflight meets
			// the same window and says the same words.
			if err := w.st.ClearRunRetry(ctx, w.runID); err != nil {
				t.Fatalf("tick %d: the resume's clear: %v", i, err)
			}
			decide(*rows[0].RetryAfter, scan)
			if out := w.r.armUsageWindowRetry(ctx, refuse(scan), w.runID, spentNothing, quiet); out != usageRetryArmed {
				t.Fatalf("tick %d: the probed re-failure: %v", i, out)
			}
		}
		scan = scan.Add(sweepTick)
	}
	return pulls, w.st.Row().Attempts, len(seen)
}

// TestArmUsageWindowRetry_AWindowDurationWindowIsOneWall is the bound
// docs/scheduling.md states, over the refusal shape that used to escape it:
// `budget` probes per provider wall, one pod per probe floor, and one audience
// per wall.
//
// Two hours of one-minute passes: the bound allows two pods. Trusting the
// duration hint instead gave a pull on EVERY pass (the arming re-dated the
// wall, and a moved wall used to clear the pace with the budget) — 120 pods,
// 120 owner pushes and 120 pull-request comments in two hours, charged nothing
// and bounded by nothing, on a run that could never terminate.
// Both deliveries of that one refusal are driven: the flattened text, and the
// TYPED error the claude_code backend builds — whose ResetAt the classifier
// resolved from the same words at the moment it read them. A reader that asks
// the typed instant alone sees a value that never moves and calls it the
// provider's word, so the harm comes back on the shape production actually
// sends while the text shape stays green.
func TestArmUsageWindowRetry_AWindowDurationWindowIsOneWall(t *testing.T) {
	shapes := map[string]func(time.Time) error{
		"flattened text": func(time.Time) error { return windowDurationErr() },
		"typed error":    typedWindowDurationErr,
	}
	for name, refuse := range shapes {
		t.Run(name, func(t *testing.T) {
			pulls, attempts, walls := redatingProbeLoop(t, 120, refuse)
			t.Logf("two hours of one-minute passes: pulls=%d attempts=%d walls=%d (budget %d, floor %s)",
				pulls, attempts, walls, probePropertyProbeBudget, probePropertyFloor)
			if pulls > 2 {
				t.Errorf("the probe pass pulled the row %d times in two hours, want at most 2: one pod per probe floor (%s) per run",
					pulls, probePropertyFloor)
			}
			if pulls > probePropertyProbeBudget {
				t.Errorf("one wall spent %d pulls, past its budget of %d", pulls, probePropertyProbeBudget)
			}
			if walls != 1 {
				t.Errorf("one provider window was recorded as %d walls: each one is a notification to the run's owner, an operator incident and a comment on the pull request, for a window nobody had news about",
					walls)
			}
		})
	}
}

// The control that keeps the figure above about the DATES and not about the
// loop's shape: a provider that re-states one dated instant, driven through
// the same loop, rests a whole floor between pulls and stays on one wall.
func TestArmUsageWindowRetry_ADatedWindowKeepsItsPaceToo(t *testing.T) {
	ctx := context.Background()
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	w := newProbeWorld()
	now := time.Now().UTC()
	wall := now.Add(96 * time.Hour)

	if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the park that opens the wall: %v", out)
	}
	pulls := 0
	scan := now
	for i := 0; i < 20; i++ {
		if rows := w.st.ProbeRows(scan, probePropertyFloor, probePropertyProbeBudget, 2); len(rows) == 1 {
			if pulled, err := w.st.PullRunRetry(ctx, w.runID, *rows[0].RetryAfter, scan); err != nil || !pulled {
				t.Fatalf("tick %d: pull = %v, %v", i, pulled, err)
			}
			pulls++
			w.st.Mutate(func(r *store.Run) {
				stamped := scan.Truncate(time.Millisecond)
				r.RetryState.LastProbeAt = &stamped
			})
			if err := w.st.ClearRunRetry(ctx, w.runID); err != nil {
				t.Fatalf("tick %d: clear: %v", i, err)
			}
			if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
				t.Fatalf("tick %d: the probed re-failure: %v", i, out)
			}
		}
		scan = scan.Add(sweepTick)
	}
	if pulls != 1 {
		t.Errorf("a steady wall was pulled %d times in 20 minutes, want 1", pulls)
	}
	if got := w.st.Row().UsageWall(); !got.Equal(wall.Truncate(time.Millisecond)) {
		t.Errorf("the wall moved to %s, want the provider's own %s", got, wall.Truncate(time.Millisecond))
	}
}

// TestArmUsageWindowRetry_ALegacyRowGainsItsWallWithoutASecondBudget is
// #2450's legacy-row anchor on the path #2451 Q1 is about: a row armed before
// the wall was persisted, pulled forward by the probe pass, then re-parked by
// a refusal that names no instant of its own.
//
// It must gain the wall — until it has one, store.UsageParkWall reports zero
// and the run's owner keeps hearing "Run failed" once per park for the whole
// wait, which is the notification this cluster exists to remove — and it must
// NOT be handed a second probe budget: probe_count predates the wall field, so
// a row whose budget was already spent is reachable, and a row that never
// carried a wall is not a row whose wall MOVED.
func TestArmUsageWindowRetry_ALegacyRowGainsItsWallWithoutASecondBudget(t *testing.T) {
	ctx := context.Background()
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	now := time.Now().UTC()
	announced := now.Add(4 * 24 * time.Hour)

	w := newProbeWorld()
	pulledAt := now.Truncate(time.Millisecond)
	probeInstant := now.Truncate(time.Millisecond)
	w.st.Mutate(func(run *store.Run) {
		run.RetryState = &store.RunRetryState{
			// The pre-wall shape: a live marker and a spent ledger, no wall.
			ProbeFrom: &announced, ProbeCount: probePropertyProbeBudget, LastProbeAt: &pulledAt,
			RetryAfter: &probeInstant, Attempts: 1,
			Reason: store.RetryReasonUsageWindow, Code: "USAGE_LIMIT_BLOCKED",
		}
	})
	if got := store.UsageParkWall(store.RunStatusFailedResumable, w.st.Row()); !got.IsZero() {
		t.Fatalf("precondition: the legacy row already reports a wall (%s)", got)
	}

	if out := w.r.armUsageWindowRetry(ctx, blindUsageWindowErr(), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the probed re-failure: %v", out)
	}
	row := w.st.Row()
	if got := store.UsageParkWall(store.RunStatusFailedResumable, row); !got.Equal(announced.UTC().Truncate(time.Millisecond)) {
		t.Errorf("the row carries wall %s after the arming, want the held-aside instant %s: a wall-less row makes every reader fall back to \"the run failed\", once per park, for the whole wait",
			instantOrNone(got), announced.Format(time.RFC3339))
	}
	if row.ProbeCount != probePropertyProbeBudget {
		t.Errorf("probe_count = %d after the arming that NAMED this row's wall, want the spent budget kept at %d: labelling the wait a row is already on must not buy that window a second budget of pods",
			row.ProbeCount, probePropertyProbeBudget)
	}
	if rows := w.st.ProbeRows(now.Add(probePropertyFloor+time.Minute), probePropertyFloor, probePropertyProbeBudget, 2); len(rows) != 0 {
		t.Errorf("the probe scan listed %d row(s) a floor after the arming, want 0: this wall's probes are spent and the run simply waits", len(rows))
	}
}

// TestArmUsageWindowRetry_EveryParkThatFollowedAPullReportsWhatItFound is
// #2450's third point, over the parks the free re-arm is not: the operator
// reading iterion_runs_retry_probes_total computes "the probes whose run got
// PAST the preflight" as `pulled` minus the other two labels, so a park that
// followed a pull and refused must say so — or a wasted pod is credited as a
// probe that found the window reopened.
func TestArmUsageWindowRetry_EveryParkThatFollowedAPullReportsWhatItFound(t *testing.T) {
	quiet := iterlog.New(iterlog.LevelError, io.Discard)

	t.Run("a park whose pull is older than the marker's TTL", func(t *testing.T) {
		now := time.Now().UTC()
		announced := now.Add(7 * 24 * time.Hour)
		stale := now.Add(-2 * store.ProbeMarkerTTL).Truncate(time.Millisecond)
		probeInstant := now.Truncate(time.Millisecond)
		run := probedRunDoc("run-2450", announced, 1)
		run.RetryState.LastProbeAt = &stale
		run.RetryState.RetryAfter = &probeInstant
		st := &cancelAwareStore{run: run}
		reg := metrics.New()
		r := &Runner{cfg: Config{Store: st, Metrics: reg}}

		if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-2450", spentNothing, quiet); got != usageRetryArmed {
			t.Fatalf("outcome = %v, want usageRetryArmed", got)
		}
		if st.probeRearmed {
			t.Fatal("precondition: a marker older than the TTL must not buy the free re-arm")
		}
		if v := probeResult(t, reg, metrics.RetryProbeStillBlocked); v != 1 {
			t.Errorf("iterion_runs_retry_probes_total{result=%s} = %v, want 1: the pull is the case store.ProbeMarkerTTL prices, the preflight refused, and `pulled` minus the other labels reports this pod as a probe that found the window REOPENED",
				metrics.RetryProbeStillBlocked, v)
		}
	})

	t.Run("a park whose free re-arm hit a store error", func(t *testing.T) {
		now := time.Now().UTC()
		announced := now.Add(7 * 24 * time.Hour)
		run := probedRunDoc("run-2450", announced, 1)
		st := &cancelAwareStore{run: run, probeRearmErr: errors.New("mongo is down")}
		reg := metrics.New()
		r := &Runner{cfg: Config{Store: st, Metrics: reg}}

		if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-2450", spentNothing, quiet); got != usageRetryArmed {
			t.Fatalf("outcome = %v, want usageRetryArmed", got)
		}
		if v := probeResult(t, reg, metrics.RetryProbeStillBlocked); v != 1 {
			t.Errorf("iterion_runs_retry_probes_total{result=%s} = %v, want 1: the store blip changed who PAYS for the park, not what the probe found",
				metrics.RetryProbeStillBlocked, v)
		}
	})

	t.Run("a park that got past the preflight and spent tokens reports nothing", func(t *testing.T) {
		now := time.Now().UTC()
		announced := now.Add(7 * 24 * time.Hour)
		run := probedRunDoc("run-2450", announced, 1)
		run.WorkflowHash = "wf-hash-1"
		st := &cancelAwareStore{run: run}
		reg := metrics.New()
		r := &Runner{cfg: Config{Store: st, Metrics: reg}}

		if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(now.Add(90*time.Minute)), "run-2450", spentTokens, quiet); got != usageRetryArmed {
			t.Fatalf("outcome = %v, want usageRetryArmed", got)
		}
		if v := probeResult(t, reg, metrics.RetryProbeStillBlocked); v != 0 {
			t.Errorf("iterion_runs_retry_probes_total{result=%s} = %v, want 0: that pull's run got PAST the preflight, which is the one outcome a probe cannot observe from the row and is exactly the `pulled` difference the label set exists to expose",
				metrics.RetryProbeStillBlocked, v)
		}
	})

	t.Run("a park the store refused is counted once it LANDS, not once per delivery", func(t *testing.T) {
		now := time.Now().UTC()
		announced := now.Add(7 * 24 * time.Hour)
		stale := now.Add(-2 * store.ProbeMarkerTTL).Truncate(time.Millisecond)
		probeInstant := now.Truncate(time.Millisecond)
		run := probedRunDoc("run-2450", announced, 1)
		// The pull is older than the marker's TTL, so this park is the PAID
		// one — the path whose refusal hands the delivery back.
		run.RetryState.LastProbeAt = &stale
		run.RetryState.RetryAfter = &probeInstant
		st := &cancelAwareStore{run: run, scheduleErr: errors.New("mongo is down")}
		reg := metrics.New()
		r := &Runner{cfg: Config{Store: st, Metrics: reg}}

		// What JetStream does with a nak: the same message against a row no
		// write consumed. Production allows eight (queue.MaxDeliver).
		const redeliveries = 4
		for i := range redeliveries {
			if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-2450", spentNothing, quiet); got != usageRetryNotApplicable {
				t.Fatalf("delivery %d: outcome = %v, want usageRetryNotApplicable — the store refused, so the caller naks", i, got)
			}
		}
		if v := probeResult(t, reg, metrics.RetryProbeStillBlocked); v != 0 {
			t.Errorf("iterion_runs_retry_probes_total{result=%s} = %v after %d redeliveries of ONE pull, want 0: the pass counts `pulled` once, and \"the probes whose run got past the preflight\" is `pulled` minus the other labels — so this reports %v wasted pods that never existed and makes the difference negative",
				metrics.RetryProbeStillBlocked, v, redeliveries, v)
		}

		// The delivery that LANDS reports what that pull found, once.
		st.scheduleErr = nil
		if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-2450", spentNothing, quiet); got != usageRetryArmed {
			t.Fatalf("the delivery that lands: outcome = %v, want usageRetryArmed", got)
		}
		if v := probeResult(t, reg, metrics.RetryProbeStillBlocked); v != 1 {
			t.Errorf("iterion_runs_retry_probes_total{result=%s} = %v after the arming landed, want 1: the pull happened and the preflight refused, whatever the store did in between",
				metrics.RetryProbeStillBlocked, v)
		}
	})
}

// TestArmUsageWindowRetry_AProviderThatMovesItsWallIsStillPacedByTheFloor is
// the bound docs/scheduling.md states for its own residual, driven end to end
// through the real arming: a provider whose refusal names a NEW date on every
// refusal earns each date a fresh probe budget — that is what "a wall the
// provider moved is a new wait" means — and is still pulled at most once per
// probe floor, because the PACE is the row's and no arming clears it.
//
// Twenty one-minute passes allow one pod. Clearing the pace with the budget
// gave a pull on EVERY pass: the scan reads an absent stamp as "never probed",
// the free re-arm charges no attempt, and nothing was left to bound the loop
// or to retire the run.
func TestArmUsageWindowRetry_AProviderThatMovesItsWallIsStillPacedByTheFloor(t *testing.T) {
	ctx := context.Background()
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	w := newProbeWorld()
	now := time.Now().UTC()
	wall := now.Add(96 * time.Hour)

	if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the park that opens the wall: %v", out)
	}
	pulls, moves := 0, 0
	scan := now
	for i := 0; i < 20; i++ {
		rows := w.st.ProbeRows(scan, probePropertyFloor, probePropertyProbeBudget, 2)
		if len(rows) == 1 && rows[0].RetryAfter != nil {
			pulled, err := w.st.PullRunRetry(ctx, w.runID, *rows[0].RetryAfter, scan)
			if err != nil || !pulled {
				t.Fatalf("tick %d: pull = %v, %v", i, pulled, err)
			}
			pulls++
			w.st.Mutate(func(r *store.Run) {
				stamped := scan.Truncate(time.Millisecond)
				r.RetryState.LastProbeAt = &stamped
			})
			if err := w.st.ClearRunRetry(ctx, w.runID); err != nil {
				t.Fatalf("tick %d: the resume's clear: %v", i, err)
			}
			// The provider's fresh refusal names a date an hour later than
			// the one before: genuine news, and a new wait.
			before := w.st.Row().UsageWall()
			wall = wall.Add(time.Hour)
			if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
				t.Fatalf("tick %d: the probed re-failure: %v", i, out)
			}
			if after := w.st.Row().UsageWall(); !after.Equal(before) {
				moves++
			}
		}
		scan = scan.Add(sweepTick)
	}
	t.Logf("twenty one-minute passes on a provider moving its date: pulls=%d wall moves=%d attempts=%d",
		pulls, moves, w.st.Row().Attempts)
	if moves == 0 {
		t.Fatal("no arming moved the wall — the shape under test was not reached")
	}
	if pulls > 1 {
		t.Errorf("the probe pass pulled the row %d times in twenty minutes, want at most 1: one pod per probe floor (%s) per run, whatever the provider does to its dates",
			pulls, probePropertyFloor)
	}
}

// TestArmUsageWindowRetry_TheCapturedFacadeWindowIsOneWall drives the shape
// the tree CAPTURES for the facade that prints the 5h sentence — the duration
// AND an absolute instant the facade rendered — through the probe pass over
// ten modelled hours, and holds BOTH producer hypotheses:
//
//   - the facade recomputes the duration against its own clock at every
//     refusal: each park reads a text freshly rendered at the moment the
//     decision reads it, so the instant is the duration's own rendering and
//     is no news — the row keeps its wall and the ledger that bounds this
//     window's pods;
//   - the facade anchors the instant on the window's start: the text never
//     changes, and whether a reading calls it the rendering (a refusal at the
//     window's start) or the provider's own date, the wall it names is the
//     one the row already holds, so an arming that re-states it moves
//     nothing.
//
// The row is read for what it IS after every arming. The loop beside
// re-derives the decision at each pass's instant, which answers a different
// question for a shape whose datedness depends on the reading clock — the
// only way to feed it a text rendered for that clock is to render at the
// reading, which is what the two recompute-per-refusal shapes here do.
//
// Trusting the instant gave one wall per park: ten hours of one-minute passes
// pulled the row ten times (the probe BUDGET never bit — every pull opened a
// fresh one), pushed ten notification episodes and spent the attempt budget
// hourly. The bound docs/scheduling.md states is per WALL.
func TestArmUsageWindowRetry_TheCapturedFacadeWindowIsOneWall(t *testing.T) {
	shapes := map[string]func(time.Time) error{
		"the facade recomputes per refusal (flattened text)": func(time.Time) error {
			return facadeWindowErr(time.Now().UTC())
		},
		"the facade recomputes per refusal (typed error)": func(time.Time) error {
			return facadeWindowTypedErr(time.Now().UTC())
		},
		"the facade anchors the window (frozen text)": func(time.Time) error {
			return facadeWindowErrAt(time.Now().UTC(), wallAnchor())
		},
	}
	for name, refuse := range shapes {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			quiet := iterlog.New(iterlog.LevelError, io.Discard)
			w := newProbeWorld()
			now := time.Now().UTC()

			if out := w.r.armUsageWindowRetry(ctx, refuse(now), w.runID, spentNothing, quiet); out != usageRetryArmed {
				t.Fatalf("the park that opens the wall: %v", out)
			}
			pulls, wallsSeen := 0, map[string]bool{}
			if wall := w.st.Row().UsageWall(); !wall.IsZero() {
				wallsSeen[wall.Format(time.RFC3339)] = true
			}
			scan := now
			for i := 0; i < 600; i++ {
				if rows := w.st.ProbeRows(scan, probePropertyFloor, probePropertyProbeBudget, 2); len(rows) == 1 && rows[0].RetryAfter != nil {
					if pulled, err := w.st.PullRunRetry(ctx, w.runID, *rows[0].RetryAfter, scan); err != nil || !pulled {
						t.Fatalf("tick %d: pull = %v, %v", i, pulled, err)
					}
					pulls++
					w.st.Mutate(func(r *store.Run) {
						stamped := scan.Truncate(time.Millisecond)
						r.RetryState.LastProbeAt = &stamped
					})
					if err := w.st.ClearRunRetry(ctx, w.runID); err != nil {
						t.Fatalf("tick %d: the resume's clear: %v", i, err)
					}
					if out := w.r.armUsageWindowRetry(ctx, refuse(time.Now().UTC()), w.runID, spentNothing, quiet); out != usageRetryArmed {
						t.Fatalf("tick %d: the probed re-failure: %v", i, out)
					}
					wall := w.st.Row().UsageWall()
					if wall.IsZero() {
						t.Fatalf("tick %d: the row lost its wall", i)
					}
					wallsSeen[wall.Format(time.RFC3339)] = true
				}
				scan = scan.Add(sweepTick)
			}
			t.Logf("ten hours of one-minute passes, %s: pulls=%d walls=%d (budget %d, floor %s)",
				name, pulls, len(wallsSeen), probePropertyProbeBudget, probePropertyFloor)
			if len(wallsSeen) != 1 {
				t.Errorf("one provider window was recorded as %d walls (%v): each one is a notification to the run's owner, an operator incident, a comment on the pull request and a fresh probe budget, for a window nobody had news about",
					len(wallsSeen), wallsSeen)
			}
			if pulls > probePropertyProbeBudget {
				t.Errorf("one wall spent %d pulls past its budget of %d", pulls, probePropertyProbeBudget)
			}
		})
	}
}

// wallAnchor is the instant a window-anchored facade built its wall from: the
// start of the 5h window the test loop runs in. The refusals of that window
// render an instant within the tolerance of reading+5h, so the oracle answers
// the rendering — and the row's wall is that same instant either way.
func wallAnchor() time.Time {
	return time.Now().UTC().Add(-time.Second)
}

// TestArmUsageWindowRetry_OneDatedWindowIsOneWallWhateverCarriedIt is the
// identity the wall-identity comment states, on a DATED window: the typed
// error the backend builds and the flattened text a lost hop leaves answer
// the same wall. The typed delivery used to carry the come-back pad into the
// wall (+1m over the flattened one), so the same provider window was two
// notification episodes, two probe budgets and two pull-request notices
// whenever the delivery shape changed between parks — a store round-trip, a
// %v flatten, a hop to another pod.
func TestArmUsageWindowRetry_OneDatedWindowIsOneWallWhateverCarriedIt(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(4 * 24 * time.Hour).Truncate(time.Second)
	typed := weeklyWindowErr(reset)
	flattened := &runtime.RuntimeError{
		Code:    runtime.ErrCodeUsageLimitBlocked,
		Message: `node "synthesize" execution failed: rate_limited (claude_code): You've hit your weekly limit · reset at ` + reset.Format("2006-01-02 15:04:05"),
	}
	wallTyped, datedTyped, okTyped := usageRefusalWall(typed, now)
	wallText, datedText, okText := usageRefusalWall(flattened, now)
	if !okTyped || !okText || !datedTyped || !datedText {
		t.Fatalf("one dated window answered ok=%v/%v dated=%v/%v across its two deliveries", okTyped, okText, datedTyped, datedText)
	}
	if !wallTyped.Equal(wallText) {
		t.Errorf("the two deliveries of ONE dated window answer walls %s apart (%s vs %s): each is a notification episode, an operator incident, a pull-request notice and a probe budget of its own",
			wallTyped.Sub(wallText), wallTyped.Format(time.RFC3339), wallText.Format(time.RFC3339))
	}
	if want := reset; !wallTyped.Equal(want) {
		t.Errorf("the wall is %s, want the refusal's own instant %s — the come-back pad is the wake's", wallTyped.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	// And through the store, where the identity becomes the row's fact: both
	// deliveries arm the same wall on a fresh row.
	for name, err := range map[string]error{"typed": typed, "flattened": flattened} {
		w := newProbeWorld()
		if out := w.r.armUsageWindowRetry(context.Background(), err, w.runID, spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); out != usageRetryArmed {
			t.Fatalf("%s delivery: outcome = %v", name, out)
		}
		if got := w.st.Row().UsageWall().Truncate(time.Millisecond); !got.Equal(reset.Truncate(time.Millisecond)) {
			t.Errorf("%s delivery armed wall %s, want %s", name, got.Format(time.RFC3339), reset.Truncate(time.Millisecond).Format(time.RFC3339))
		}
	}
}

// TestArmUsageWindowRetry_ARefusedParkFeedsTheSharedCircuitOnceItLands is the
// behaviour half of the class round 2 folded for the probe count: the blocked
// counter and the shared retry circuit used to charge on the way IN, so ONE
// park the store refused — JetStream then redelivers it up to MaxDeliver
// times — crossed the default threshold of 3 on its own and opened a breaker
// every sibling run of the workflow then waits behind.
func TestArmUsageWindowRetry_ARefusedParkFeedsTheSharedCircuitOnceItLands(t *testing.T) {
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	now := time.Now().UTC()
	announced := now.Add(7 * 24 * time.Hour)
	stale := now.Add(-2 * store.ProbeMarkerTTL).Truncate(time.Millisecond)
	probeInstant := now.Truncate(time.Millisecond)
	run := probedRunDoc("run-r3-circuit", announced, 1)
	run.RetryState.LastProbeAt = &stale
	run.RetryState.RetryAfter = &probeInstant
	st := &cancelAwareStore{run: run, scheduleErr: errors.New("mongo is down")}
	reg := metrics.New()
	r := &Runner{cfg: Config{Store: st, Metrics: reg}}

	// What JetStream does with a nak: the same park against a row no write
	// consumed. Production allows eight (queue.MaxDeliver).
	const redeliveries = 4
	for i := range redeliveries {
		if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), run.ID, spentNothing, quiet); got != usageRetryNotApplicable {
			t.Fatalf("delivery %d: outcome = %v, want usageRetryNotApplicable", i, got)
		}
	}
	if st.circuitFailures != 0 {
		t.Errorf("the shared retry circuit recorded %d failures for %d redeliveries of ONE park, want 0: the park persisted nothing, and the threshold (%d) is there for parks, not for deliveries",
			st.circuitFailures, redeliveries, retrycoord.DefaultThreshold)
	}
	if v := counterValue(t, reg.RunsUsageWindowBlocked); v != 0 {
		t.Errorf("iterion_runs_usage_window_blocked_total = %v after %d redeliveries, want 0: the run is counted when its park lands, not per delivery",
			v, redeliveries)
	}

	// The delivery that LANDS charges both, once.
	st.scheduleErr = nil
	if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), run.ID, spentNothing, quiet); got != usageRetryArmed {
		t.Fatalf("the delivery that lands: outcome = %v, want usageRetryArmed", got)
	}
	if st.circuitFailures != 1 {
		t.Errorf("the landed park fed the shared circuit %d times, want 1", st.circuitFailures)
	}
	if v := counterValue(t, reg.RunsUsageWindowBlocked); v != 1 {
		t.Errorf("iterion_runs_usage_window_blocked_total = %v once the park landed, want 1", v)
	}
}
