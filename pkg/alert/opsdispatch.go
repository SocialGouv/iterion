package alert

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/usernotify"
)

// OpsDispatcher is the CLOUD twin of the in-process Manager for run-outcome
// alerts. The Manager only observes file-tailed events and in-process runs,
// so on a cloud server it is blind to everything the runner pods execute —
// which is exactly where the 2026-08-31 incident lived: five digests parked
// `failed_resumable` on a usage cap over a whole morning, with a configured
// webhook that never fired because nothing feeding it ever saw the runs.
//
// SCOPE: this is the PLATFORM operator's channel — deliberately
// cross-tenant (the webhook URL is deployment config, the listing and run
// loads run WithoutTenantFilter), so run names and error excerpts from
// every org reach it. Point it at a channel with platform-operator
// visibility only.
//
// It consumes run.failed outcome events from the shared event spine (queue
// group ⇒ one replica per event), classifies the persisted run
// (parked-with-retry / parked-needs-operator / hard-failed), dedups episodes
// through a first-writer-wins claim shared with the reconciliation sweep,
// and fans out to the ordinary alert Sinks (webhook, errtrack). The lossy
// bus gets the usernotify treatment: RunOpsSweep replays the window the bus
// dropped.
type OpsDispatcher struct {
	Runs store.RunStore
	// Claims is the first-writer-wins episode claim — the SAME
	// usernotify.SentStore contract (and, in cloud, the same Mongo
	// collection + TTL) the user-notification family already runs; ops
	// keys are namespaced by opsEpisodePrefix so the two families share
	// one collection without colliding. nil ⇒ no dedup (tests only).
	Claims usernotify.SentStore
	Sinks  []Sink
	// BaseURL builds the /runs/<id> deep link (the deployment PublicURL).
	BaseURL string
	Logger  *iterlog.Logger
	// Now is the clock seam; nil → time.Now().UTC.
	Now func() time.Time
}

const (
	// OpsSubscriberName is the eventbus subscriber (NATS queue group).
	OpsSubscriberName = "operator-alerts"
	// opsEpisodePrefix namespaces this dispatcher's claims inside the
	// shared sent-notifications collection.
	opsEpisodePrefix = "ops|"
	// Sweep pacing — the reconciliation net under the lossy bus, mirroring
	// the usernotify sweep (same bounded-window terminal-run query, same
	// keyset pagination so a backlog cannot starve the oldest episodes,
	// same 24h lookback so a control-plane outage of a morning — the KEDA
	// freeze, the incident this component closes — does not fall off the
	// window; the IsMarked pre-check keeps the long window cheap).
	opsSweepInterval = 2 * time.Minute
	opsSweepLookback = 24 * time.Hour
	opsSweepBatch    = 500
	opsSweepMaxPages = 20
)

// Handle processes one run-outcome event — the eventbus.Handler and the
// sweep's replay entry point. Non-failure outcomes exit on a kind check.
//
// Two claim keys per episode, for two different jobs:
//   - the EPISODE key (opsEpisodeKey) is the delivery dedup. For a parked
//     run it is STABLE across the retry cycle's updated_at bumps
//     (ScheduleRunRetry / ClaimRunRetry / AbandonRunRetry all rewrite the
//     row every sweep-minute while the status never leaves
//     failed_resumable) — keyed on the monotonic RetryState.Attempts, so
//     the operator gets one message per actual retry cycle, never one per
//     bookkeeping write.
//   - the TRANSITION marker (the event id) only feeds the sweep's cheap
//     pre-check: it is stamped once the episode is settled, so a bumped
//     row stops costing a run load on every sweep pass.
func (d *OpsDispatcher) Handle(ctx context.Context, ev trigger.Event) error {
	if ev.Kind != trigger.KindRunFailed || ev.Subject.ID == "" || d.Runs == nil {
		return nil
	}
	a, run, ok := d.classify(ctx, ev)
	if !ok {
		return nil
	}
	evKey := opsEpisodePrefix + ev.ID
	epKey := opsEpisodeKey(run)
	if d.Claims != nil {
		won, err := d.Claims.TryMark(ctx, epKey)
		if err != nil {
			return fmt.Errorf("alert: claim ops episode %s: %w", epKey, err)
		}
		if !won {
			// Stamp this transition's marker ONLY when the incident is
			// CONFIRMED delivered. A pending claim is a live fan-out that
			// may still fail and Unmark — a loser certifying its evKey then
			// would make every later sweep skip the released episode before
			// Handle runs, silencing a parked-no-retry alert forever (the
			// exact run this component exists to surface).
			if delivered, derr := d.Claims.WasDelivered(ctx, epKey); derr == nil && delivered {
				d.stampSeen(ctx, evKey)
			}
			return nil
		}
	}

	// Fan out. Sinks that can report failure (ErrorReportingSink) decide
	// the episode's fate: if EVERY one of them fails, the claim is
	// RELEASED so the 2-minute sweep retries — a Mattermost rolling
	// restart or an ingress 502 must not consume the one alert this
	// component exists to deliver (usernotify.SentStore.Unmark documents
	// exactly this contract). Fire-and-forget sinks (tracker breadcrumbs)
	// never count as delivery.
	var wg sync.WaitGroup
	var attempted, failed int
	errs := make([]error, len(d.Sinks))
	for i, sink := range d.Sinks {
		wg.Add(1)
		go func(i int, sink Sink) {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultNotifyTimeout)
			defer cancel()
			if es, ok := sink.(ErrorReportingSink); ok {
				errs[i] = es.NotifyErr(sctx, a)
				return
			}
			sink.Notify(sctx, a)
		}(i, sink)
	}
	wg.Wait()
	for i, sink := range d.Sinks {
		if _, ok := sink.(ErrorReportingSink); ok {
			attempted++
			if errs[i] != nil {
				failed++
			}
		}
	}
	if attempted > 0 && failed == attempted {
		if d.Claims != nil {
			if err := d.Claims.Unmark(ctx, epKey); err != nil && d.Logger != nil {
				d.Logger.Warn("alert: release ops episode %s after failed delivery: %v", epKey, err)
			}
		}
		if d.Logger != nil {
			d.Logger.Warn("alert: operator alert %s for run %s failed on every channel — released for the sweep to retry", a.Kind, a.RunID)
		}
		return nil
	}
	d.markDelivered(ctx, epKey)
	d.stampSeen(ctx, evKey)
	if d.Logger != nil {
		d.Logger.Info("alert: operator alert %s for run %s delivered (%s)", a.Kind, a.RunID, epKey)
	}
	return nil
}

// opsEpisodeKey derives the delivery-dedup key from the persisted run. A
// parked run's key rides (status, attempts) — stable across the retry
// bookkeeping writes, advancing once per real retry cycle. A hard failure
// keeps the per-transition derivation (nothing rewrites a failed run, and a
// resumed-then-refailed run is a genuinely new episode).
func opsEpisodeKey(run *store.Run) string {
	if run.Status == store.RunStatusFailedResumable {
		attempts := 0
		if run.RetryState != nil {
			attempts = run.RetryState.Attempts
		}
		// The failure code joins the key because attempts alone can
		// stand still across two DIFFERENT parks: ScheduleRunRetry is
		// the only writer that advances it, so a resumed run re-parking
		// on an unretryable cause (no retry armed) reuses the old
		// count — and the operator would never hear about the new,
		// manual-intervention failure. Same code repeated = still one
		// episode, so the retry-cycle anti-spam is intact.
		return fmt.Sprintf("%srun:%s:parked:%d:%s", opsEpisodePrefix, run.ID, attempts, run.FailureCode)
	}
	return opsEpisodePrefix + trigger.RunOutcomeEventID(run.ID, string(run.Status), "", run.UpdatedAt)
}

// ErrorReportingSink is the optional Sink capability the ops dispatcher
// uses to decide an episode's fate: a sink that can say "this delivery
// FAILED" participates in the release-and-retry contract; fire-and-forget
// sinks (tracker breadcrumbs) never count as delivery.
type ErrorReportingSink interface {
	NotifyErr(ctx context.Context, a Alert) error
}

// stampSeen marks a transition id as settled for the sweep pre-check.
// Best-effort — a lost stamp costs one redundant run load next pass.
func (d *OpsDispatcher) stampSeen(ctx context.Context, evKey string) {
	if d.Claims == nil {
		return
	}
	if _, err := d.Claims.TryMark(ctx, evKey); err != nil {
		return
	}
	_ = d.Claims.MarkDelivered(ctx, evKey)
}

// classify maps the persisted run onto an operator alert. failed_resumable
// is the interesting one — the status the in-process Manager never alerted
// on and the one that goes quiet for DAYS when nobody is told.
func (d *OpsDispatcher) classify(ctx context.Context, ev trigger.Event) (Alert, *store.Run, bool) {
	run, err := d.Runs.LoadRun(store.WithoutTenantFilter(ctx), ev.Subject.ID)
	if err != nil || run == nil {
		return Alert{}, nil, false
	}
	a := Alert{
		RunID:     run.ID,
		RunName:   firstNonEmpty(run.Name, run.WorkflowName),
		Timestamp: d.now(),
	}
	if d.BaseURL != "" {
		a.Link = strings.TrimRight(d.BaseURL, "/") + "/runs/" + run.ID
	}
	switch run.Status {
	case store.RunStatusFailedResumable:
		a.Kind = KindRunParked
		rs := run.RetryState
		switch {
		case rs != nil && rs.RetryAfter != nil:
			a.Reason = fmt.Sprintf("waiting out %s — automatic retry armed for %s (attempt %d)",
				firstNonEmpty(rs.Reason, rs.Code, "a provider window"),
				rs.RetryAfter.UTC().Format(time.RFC3339), rs.Attempts)
		case rs != nil && rs.LastError != "":
			a.Reason = "automatic retry stopped: " + rs.LastError
		default:
			a.Reason = "no automatic retry armed — needs an operator resume"
		}
		if run.Error != "" {
			a.Reason += " · " + truncate(run.Error, 200)
		}
		a.FailureCode = string(run.FailureCode)
		return a, run, true
	case store.RunStatusFailed:
		a.Kind = KindRunFailed
		a.Reason = truncate(run.Error, 200)
		a.FailureCode = string(run.FailureCode)
		return a, run, true
	default:
		// Already resumed/finished by the time we looked — nothing owed.
		return Alert{}, nil, false
	}
}

// RunOpsSweep ticks the reconciliation net until ctx is cancelled: re-offer
// every recently-terminal run whose episode is unclaimed to Handle. The
// grace keeps it off runs the live bus path is still delivering; the
// lookback bounds how far a replica restart can reach back.
func (d *OpsDispatcher) RunOpsSweep(ctx context.Context, list usernotify.ListNotifiableRuns) {
	if list == nil {
		return
	}
	if d.Logger != nil {
		d.Logger.Info("alert: operator-alert sweep active (every %s, %s lookback, keyset-paginated) — the net under the lossy outcome event", opsSweepInterval, opsSweepLookback)
	}
	t := time.NewTicker(opsSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.SweepOnce(ctx, list)
		}
	}
}

// SweepOnce performs one reconciliation pass (exported: the ticker's unit
// of work, and the tests' entry point), keyset-paginated by updated_at so a
// backlog larger than one page cannot starve the oldest unclaimed episodes —
// a burst of newer parked runs (which the retry cycle keeps re-floating to
// the top of the newest-first sort) must not push the run waiting longest
// off the page forever (usernotify's sweep documents the same trap).
func (d *OpsDispatcher) SweepOnce(ctx context.Context, list usernotify.ListNotifiableRuns) {
	fctx := store.WithoutTenantFilter(ctx)
	since := d.now().Add(-opsSweepLookback)
	var before time.Time
	for page := 0; page < opsSweepMaxPages; page++ {
		refs, err := list(fctx, since, before, opsSweepBatch)
		if err != nil {
			if d.Logger != nil {
				d.Logger.Warn("alert: operator-alert sweep scan: %v", err)
			}
			return
		}
		d.sweepRefs(ctx, refs)
		if len(refs) < opsSweepBatch {
			return
		}
		last := refs[len(refs)-1].UpdatedAt
		if last.IsZero() || (!before.IsZero() && !last.Before(before)) {
			return // a lister without updated_at cannot cursor — stop, don't loop
		}
		before = last
	}
}

func (d *OpsDispatcher) sweepRefs(ctx context.Context, refs []usernotify.RunRef) {
	for _, ref := range refs {
		if store.RunStatus(ref.Status) != store.RunStatusFailed && store.RunStatus(ref.Status) != store.RunStatusFailedResumable {
			continue
		}
		// Cheap pre-check: derive the episode key from the listing alone
		// (the same derivation the live event carries) and skip claimed
		// episodes WITHOUT loading the run — in steady state nearly every
		// listed run is already claimed, and a parked run stays in the
		// window for the whole lookback (usernotify's sweep sets the
		// pattern at pkg/usernotify/sweep.go).
		if d.Claims != nil {
			key := opsEpisodePrefix + trigger.RunOutcomeEventID(ref.ID, ref.Status, ref.InteractionID, ref.UpdatedAt)
			if marked, err := d.Claims.IsMarked(ctx, key); err == nil && marked {
				continue
			}
		}
		// Rebuild the SAME canonical event the live path consumed, so the
		// claim inside Handle dedups against the bus delivery.
		ev := trigger.BuildRunOutcome(ctx, d.Runs, ref.ID, nil)
		if err := d.Handle(ctx, ev); err != nil && d.Logger != nil {
			d.Logger.Warn("alert: operator-alert sweep replay run %s: %v", ref.ID, err)
		}
	}
}

func (d *OpsDispatcher) markDelivered(ctx context.Context, key string) {
	if d.Claims == nil {
		return
	}
	if err := d.Claims.MarkDelivered(ctx, key); err != nil && d.Logger != nil {
		d.Logger.Warn("alert: confirm ops episode %s: %v", key, err)
	}
}

func (d *OpsDispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
