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
	// the usernotify sweep's constants and sharing its bounded-window
	// terminal-run query (one index serves every net).
	opsSweepInterval = 2 * time.Minute
	opsSweepGrace    = time.Minute
	opsSweepLookback = 30 * time.Minute
	opsSweepBatch    = 200
)

// Handle processes one run-outcome event — the eventbus.Handler and the
// sweep's replay entry point. Non-failure outcomes exit on a kind check.
func (d *OpsDispatcher) Handle(ctx context.Context, ev trigger.Event) error {
	if ev.Kind != trigger.KindRunFailed || ev.Subject.ID == "" || d.Runs == nil {
		return nil
	}
	a, ok := d.classify(ctx, ev)
	if !ok {
		return nil
	}
	key := opsEpisodePrefix + ev.ID
	if d.Claims != nil {
		won, err := d.Claims.TryMark(ctx, key)
		if err != nil {
			return fmt.Errorf("alert: claim ops episode %s: %w", key, err)
		}
		if !won {
			return nil
		}
	}
	var wg sync.WaitGroup
	for _, sink := range d.Sinks {
		wg.Add(1)
		go func(sink Sink) {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultNotifyTimeout)
			defer cancel()
			sink.Notify(sctx, a)
		}(sink)
	}
	wg.Wait()
	// Sink.Notify reports failures by logging, not by error (the Manager's
	// contract) — so a delivered episode is confirmed unconditionally. The
	// webhook's own Warn is the operator's signal that the channel itself
	// is broken; retrying an alert whose channel is down would spam the
	// moment it recovers.
	d.markDelivered(ctx, key)
	if d.Logger != nil {
		d.Logger.Info("alert: operator alert %s for run %s delivered (%s)", a.Kind, a.RunID, ev.ID)
	}
	return nil
}

// classify maps the persisted run onto an operator alert. failed_resumable
// is the interesting one — the status the in-process Manager never alerted
// on and the one that goes quiet for DAYS when nobody is told.
func (d *OpsDispatcher) classify(ctx context.Context, ev trigger.Event) (Alert, bool) {
	run, err := d.Runs.LoadRun(store.WithoutTenantFilter(ctx), ev.Subject.ID)
	if err != nil || run == nil {
		return Alert{}, false
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
		return a, true
	case store.RunStatusFailed:
		a.Kind = KindRunFailed
		a.Reason = truncate(run.Error, 200)
		return a, true
	default:
		// Already resumed/finished by the time we looked — nothing owed.
		return Alert{}, false
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
		d.Logger.Info("alert: operator-alert sweep active (every %s, %s grace, %s lookback) — the net under the lossy outcome event", opsSweepInterval, opsSweepGrace, opsSweepLookback)
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
// of work, and the tests' entry point).
func (d *OpsDispatcher) SweepOnce(ctx context.Context, list usernotify.ListNotifiableRuns) {
	now := d.now()
	refs, err := list(store.WithoutTenantFilter(ctx), now.Add(-opsSweepLookback), now.Add(-opsSweepGrace), opsSweepBatch)
	if err != nil {
		if d.Logger != nil {
			d.Logger.Warn("alert: operator-alert sweep scan: %v", err)
		}
		return
	}
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
