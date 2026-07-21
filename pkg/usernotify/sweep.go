package usernotify

import (
	"context"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The eventbus is deliberately lossy (at-most-once on core NATS): if no
// server replica is subscribed at publish time, the run-outcome event — and
// with it the "a human is blocking this run" push — is silently gone. The
// Sweeper is the reconciliation net: it periodically re-derives the outcome
// event for every notifiable run and replays it through the Dispatcher,
// whose SentStore claim makes the replay idempotent (already-notified
// episodes are skipped in one indexed read).

// RunRef identifies one run the sweep should re-examine, carrying just
// enough (status + pending interaction + updated_at) to derive the episode
// key without loading the run, and to keyset-paginate.
type RunRef struct {
	ID            string
	Status        string
	InteractionID string
	UpdatedAt     time.Time
}

// ListNotifiableRuns returns one page of runs to (re-)examine: every run
// still paused on a human interaction, plus terminal runs updated since
// `since` — restricted to updated_at < `before` when non-zero (the keyset
// cursor), newest first. Wired to the Mongo store's ListNotifiableRuns in
// cloud mode.
type ListNotifiableRuns func(ctx context.Context, since, before time.Time, limit int) ([]RunRef, error)

const (
	// sweepInterval paces the reconciliation scan. Push loss is the rare
	// case (a replica gap at publish instant), so a couple of minutes of
	// worst-case notification lag is acceptable.
	sweepInterval = 2 * time.Minute
	// sweepTerminalLookback bounds how far back terminal outcomes are
	// reconciled. Paused runs are NOT time-bounded (they are still
	// waiting); a finished/failed run older than this simply misses its
	// (by then stale) notification.
	sweepTerminalLookback = 24 * time.Hour
	// sweepLimit caps one scan page; sweepMaxPages bounds a pass. The
	// keyset cursor (updated_at) guarantees old-but-unclaimed episodes are
	// reached even when newer rows outnumber a page — without it, a flat
	// newest-first LIMIT would starve exactly the runs waiting longest.
	sweepLimit    = 500
	sweepMaxPages = 20
)

// Sweeper replays missed notification episodes through the Dispatcher.
type Sweeper struct {
	dispatcher *Dispatcher
	list       ListNotifiableRuns
	logger     *iterlog.Logger
	// interval overrides sweepInterval in tests.
	interval time.Duration
}

func NewSweeper(d *Dispatcher, list ListNotifiableRuns, logger *iterlog.Logger) *Sweeper {
	if logger == nil {
		logger = iterlog.Nop()
	}
	return &Sweeper{dispatcher: d, list: list, logger: logger, interval: sweepInterval}
}

// Start runs the sweep loop until ctx is cancelled.
func (sw *Sweeper) Start(ctx context.Context) {
	t := time.NewTicker(sw.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sw.SweepOnce(ctx)
		}
	}
}

// SweepOnce performs one reconciliation pass, paginating by updated_at so
// a backlog larger than one page cannot starve the oldest episodes.
func (sw *Sweeper) SweepOnce(ctx context.Context) {
	fctx := store.WithoutTenantFilter(ctx)
	since := time.Now().Add(-sweepTerminalLookback)
	var before time.Time
	for page := 0; page < sweepMaxPages; page++ {
		refs, err := sw.list(fctx, since, before, sweepLimit)
		if err != nil {
			sw.logger.Warn("usernotify: sweep list runs: %v", err)
			return
		}
		sw.sweepRefs(fctx, refs)
		if len(refs) < sweepLimit {
			return
		}
		last := refs[len(refs)-1].UpdatedAt
		if last.IsZero() || (!before.IsZero() && !last.Before(before)) {
			// A lister without updated_at cannot cursor — stop rather
			// than loop on the same page.
			return
		}
		before = last
	}
}

func (sw *Sweeper) sweepRefs(fctx context.Context, refs []RunRef) {
	for _, ref := range refs {
		// Cheap pre-check: derive the episode key from the listing alone
		// and skip already-claimed episodes WITHOUT loading the run. In
		// steady state nearly every listed run (notably long-lived pauses,
		// which are re-listed forever) is already claimed, so this turns
		// up-to-500 run loads per pass into a handful.
		if sw.dispatcher.sent != nil {
			key := trigger.RunOutcomeEventID(ref.ID, ref.Status, ref.InteractionID, ref.UpdatedAt)
			claimed, cErr := sw.dispatcher.sent.IsMarked(fctx, key)
			if cErr != nil {
				sw.logger.Warn("usernotify: sweep claim pre-check for run %s: %v", ref.ID, cErr)
			} else if claimed {
				continue
			}
		}
		// Re-derive the outcome event from the persisted run — the same
		// authority the live emitters use, so the episode key matches and
		// the SentStore dedups against the bus path.
		ev := trigger.BuildRunOutcome(fctx, sw.dispatcher.runs, ref.ID, nil)
		if err := sw.dispatcher.Handle(fctx, ev); err != nil {
			sw.logger.Warn("usernotify: sweep replay for run %s: %v", ref.ID, err)
		}
	}
}
