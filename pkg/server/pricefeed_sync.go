package server

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// The price feed's elected reconciler (ADR-163 §2/§7): one replica
// refreshes each enabled feed on its own period, under lease; restart is
// normal, the reconciliation is idempotent (each pass re-reads before it
// writes, and Refresh is the one body the manual trigger runs too). A
// feed that has not refreshed successfully inside its freshness window
// is STALE — a first-class state the pass derives, warns about on the
// transition, and never escalates into a refusal.

// priceFeedSyncInterval is the reconciler's pass tick. Feeds carry their
// own periods (1 minute to 30 days); the pass walks every scope's feeds
// and refreshes the DUE ones only, so a slow feed costs a pass one point
// read and a comparison.
const priceFeedSyncInterval = time.Minute

// runElectedPriceFeedSync is the lease campaign around the price-feed
// sweep. The election is inside, not around, so there is no unelected
// entry point to start by mistake (the board-sync precedent).
func (s *Server) runElectedPriceFeedSync(ctx context.Context) {
	health := s.newLeaseHealth()
	err := lease.Run(ctx, s.leases, lease.Spec{
		Name:   leasePriceFeedSync,
		Owner:  s.replicaID,
		TTL:    3 * priceFeedSyncInterval,
		Retry:  priceFeedSyncInterval,
		Info:   s.infof,
		Warn:   s.warnf,
		Health: health,
	}, func(ctx context.Context) { s.priceFeedSyncPasses(ctx, health) })
	if err != nil && ctx.Err() == nil {
		s.warnf("price feed sync: the lease campaign ended: %v", err)
	}
}

// priceFeedSyncPasses runs one pass at term start, then on every tick.
// A pass that finds nothing due is a healthy pass — most minutes, for
// most deployments, exactly that.
func (s *Server) priceFeedSyncPasses(ctx context.Context, health *lease.Health) {
	s.priceFeedSyncPass(ctx, health)
	ticker := time.NewTicker(priceFeedSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.priceFeedSyncPass(ctx, health)
		}
	}
}

// priceFeedSyncPass walks every scope's feeds — the platform document's
// with the tenants' — and refreshes the due ones, then derives every
// fetched feed's live status and says the staleness TRANSITION out loud
// — once per feed per transition, not per pass (the ADR's
// loud-but-not-spammy bound). A failing fetch is the feed's own error to
// carry in its state; it never fails the pass, because a dead source
// must not end the elected term for every other feed.
func (s *Server) priceFeedSyncPass(ctx context.Context, health *lease.Health) {
	health.Observe(s.priceFeedSyncSweep(ctx))
}

func (s *Server) priceFeedSyncSweep(ctx context.Context) lease.PassVerdict {
	var pass lease.Pass
	configs, err := s.priceFeedConfigs.List(ctx)
	if err != nil {
		s.warnf("price feed sync: list configs: %v", err)
		pass.FailedUnlessDone(ctx, err)
		return pass.Verdict()
	}
	now := time.Now()
	for docID, rec := range configs {
		for _, feed := range rec.Feeds {
			if ctx.Err() != nil {
				return pass.Verdict()
			}
			// A declaration answers from its config at every fold: there
			// is nothing to refresh and no window to fall out of, so the
			// pass neither reads a state under its name nor derives a
			// staleness from one.
			if !feed.Enabled || pricefeed.IsStatic(feed.Type) {
				continue
			}
			stateID := pricefeed.StateDocID(docID, feed.Name)
			state, err := s.priceFeedStates.Get(ctx, stateID)
			if err != nil {
				s.warnf("price feed sync: read %s: %v", stateID, err)
				pass.FailedUnlessDone(ctx, err)
				continue
			}
			s.warnPriceFeedTransition(docID, feed, state, now)
			if !pricefeed.Due(feed, state, now) {
				continue
			}
			if _, err := s.priceFeedRefresh(ctx, docID, feed); err != nil {
				// The refresh's own outcome carries the fetch error in
				// the durable state; this is only the write failing.
				s.warnf("price feed sync: refresh %s: %v", feed.Name, err)
				pass.FailedUnlessDone(ctx, err)
				continue
			}
			pass.SucceededUnlessDone(ctx)
		}
	}
	return pass.Verdict()
}

// warnPriceFeedTransition says the first-class staleness transitions —
// ok → stale and ok → failing — once each per process per feed: the
// in-memory memo is what bounds the repetition, because the derived
// status re-derives identically on every pass until a refresh lands.
func (s *Server) warnPriceFeedTransition(docID string, feed pricefeed.FeedConfig, state *pricefeed.StateRecord, now time.Time) {
	derived := priceFeedDerivedStatus(state, now)
	if derived == pricefeed.StateOK {
		// Recovered: the next fall warns again — a memo that never clears
		// would silence every staleness after the first for the process's
		// whole life (round 1: ok→stale→ok→stale warned once, not per
		// transition).
		s.priceFeedTransitionsMu.Lock()
		delete(s.priceFeedTransitions, docID+"/"+feed.Name)
		s.priceFeedTransitionsMu.Unlock()
		return
	}
	if derived == pricefeed.StateNever {
		return
	}
	key := docID + "/" + feed.Name
	s.priceFeedTransitionsMu.Lock()
	if s.priceFeedTransitions[key] == derived {
		s.priceFeedTransitionsMu.Unlock()
		return
	}
	s.priceFeedTransitions[key] = derived
	s.priceFeedTransitionsMu.Unlock()
	switch derived {
	case pricefeed.StateStale:
		s.warnf("price feed %s: no successful refresh inside its freshness window — it has stepped down and the operator table answers (ADR-163: staleness, not an error)", feed.Name)
	case pricefeed.StateFailing:
		if state != nil && state.LastError != "" {
			s.warnf("price feed %s: failing — %s; the operator table answers", feed.Name, state.LastError)
		} else {
			s.warnf("price feed %s: failing; the operator table answers", feed.Name)
		}
	}
}
