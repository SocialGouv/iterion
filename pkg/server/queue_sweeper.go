package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"

	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Orphan sweeper: a run is QUEUED in Mongo until a runner's first
// status write, and RUNNING until its last. A runner pod that dies at
// the wrong instant (crash mid-decode, eviction before the terminal
// write, message purged after MaxDeliver) strands the row in a state
// no operator action can reach — the studio shows an eternal spinner
// and `iterion resume` refuses (status isn't resumable).
//
// The sweeper closes that gap: periodically scan for queued/running
// rows whose updated_at is stale AND whose NATS KV lease is absent
// (the lease TTL is ~60s with 20s refreshes, so "no lease" is a
// strong crashed-or-never-claimed signal — a healthy long LLM step
// keeps the lease alive even when the run doc goes quiet), then CAS
// them to failed_resumable so resume/replay paths light up. A false
// positive self-heals: the runner's redelivery reconciliation
// auto-converts failed_resumable back into a resume.

// staleRunLister is the store capability the sweeper scans with
// (implemented by the Mongo store; local mode has no queue and no
// sweeper).
type staleRunLister interface {
	ListStaleActiveRuns(ctx context.Context, statuses []store.RunStatus, before time.Time, limit int) ([]mongostore.StaleRunRef, error)
}

// runLeaseChecker reports whether a runner currently holds the run's
// KV lease (implemented by natsq.Conn).
type runLeaseChecker interface {
	IsRunLocked(ctx context.Context, runID string) (bool, error)
}

const (
	// sweepInterval is how often the sweeper scans.
	sweepInterval = 60 * time.Second
	// sweepQueuedFallback is the queued-staleness floor when the lease
	// checker can't report the queue's actual redelivery window (tests,
	// exotic wiring). Must exceed the shipped defaults' MaxDeliver ×
	// AckWait (8 × 10m) plus margin — a message still legitimately
	// bouncing through redeliveries (a deep backlog with every runner
	// busy) never holds a lease, so a too-short cutoff flips it to
	// failed_resumable mid-flight.
	sweepQueuedFallback = 90 * time.Minute
	// sweepQueuedMargin pads the reported redelivery window so the
	// sweeper never races the final delivery attempt.
	sweepQueuedMargin = 10 * time.Minute
	// sweepRunningAfter bounds how long a quiet `running` row may go
	// without a lease before being flipped. The lease check is the
	// real signal; the time floor just avoids racing a run between
	// claim and first heartbeat.
	sweepRunningAfter = 10 * time.Minute
)

// redeliveryWindower is the optional lease-checker capability the
// sweeper derives its queued-staleness cutoff from (implemented by
// natsq.Conn), so the cutoff tracks operator overrides of
// MaxDeliver/AckWait instead of drifting from a hardcoded constant.
type redeliveryWindower interface {
	RedeliveryWindow() time.Duration
}

// queuedSweepCutoff returns how long a queued row must have been stale
// before the sweeper may flip it: the queue's worst-case redelivery
// window plus margin, or the conservative fallback when unknown.
func queuedSweepCutoff(leases runLeaseChecker) time.Duration {
	if rw, ok := leases.(redeliveryWindower); ok {
		if w := rw.RedeliveryWindow(); w > 0 {
			return w + sweepQueuedMargin
		}
	}
	return sweepQueuedFallback
}

// runQueueSweeper loops until ctx is cancelled. Started by
// ListenAndServe in cloud mode when both the Mongo store and the
// queue connection are wired.
func (s *Server) runQueueSweeper(ctx context.Context, lister staleRunLister, leases runLeaseChecker) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOrphanRuns(ctx, lister, leases, time.Now().UTC())
			// Piggy-back the DLQ depth gauge on the same cadence — the
			// sweeper only runs in cloud mode where the queue is wired.
			if s.queue != nil && s.cfg.Metrics != nil {
				if depth, err := s.queue.DLQDepth(ctx); err == nil {
					s.cfg.Metrics.DLQDepth.Set(float64(depth))
				}
			}
		}
	}
}

// sweepOrphanRuns performs one scan pass. Extracted (with an
// injectable clock) for tests.
func (s *Server) sweepOrphanRuns(ctx context.Context, lister staleRunLister, leases runLeaseChecker, now time.Time) {
	type pass struct {
		statuses []store.RunStatus
		before   time.Time
	}
	passes := []pass{
		{[]store.RunStatus{store.RunStatusQueued}, now.Add(-queuedSweepCutoff(leases))},
		{[]store.RunStatus{store.RunStatusRunning}, now.Add(-sweepRunningAfter)},
	}
	// Per-pass error accounting: any failed step means orphan recovery is
	// DEGRADED for the runs it skipped — a state a success-only counter
	// cannot distinguish from "nothing to do". Each failure increments the
	// stage counter; the pass summary below is edge-triggered so the log
	// carries one Warn per episode, not one per tick.
	var scanErrs, leaseErrs, flipErrs, probed, scanned int
	var lastErr error
	countErr := func(stage string, err error) {
		lastErr = err
		if s.cfg.Metrics != nil {
			s.cfg.Metrics.OrphanSweepErrors.WithLabelValues(stage).Inc()
		}
	}

	// Platform-level scan — the per-run tenant comes back on each ref
	// and is re-stamped for the CAS below.
	scanCtx := store.WithoutTenantFilter(ctx)
	for _, p := range passes {
		refs, err := lister.ListStaleActiveRuns(scanCtx, p.statuses, p.before, 100)
		if err != nil {
			// A dead scan is orphan recovery 100% disabled — strictly worse
			// than a partial lease failure, so it opens the episode too.
			// The per-tick line stays Debug: the episode summary is the
			// Warn, once, with the error.
			countErr("scan", err)
			scanErrs++
			if s.logger != nil {
				s.logger.Debug("sweeper: scan %v: %v", p.statuses, err)
			}
			continue
		}
		scanned++
		for _, ref := range refs {
			probed++
			locked, err := leases.IsRunLocked(ctx, ref.ID)
			if err != nil {
				// Lease state unknown — fail safe (skip), but count it: a
				// persistent NATS-KV fault would otherwise disable orphan
				// recovery with no signal at all.
				countErr("lease", err)
				leaseErrs++
				continue
			}
			if locked {
				continue // in flight
			}
			runCtx := store.WithIdentity(ctx, ref.TenantID, "sweeper")
			changed, err := s.cfg.Store.UpdateRunOutcome(runCtx, ref.ID, store.RunStatusFailedResumable,
				"orphaned by a runner crash or exhausted redelivery — resume to retry",
				store.RunOutcomeMeta{Code: store.FailureProcessOrphaned, Continuation: store.ContinuationFinal},
				p.statuses)
			if err != nil {
				countErr("flip", err)
				flipErrs++
				if s.logger != nil {
					s.logger.Warn("sweeper: flip %s: %v", ref.ID, err)
				}
				continue
			}
			if changed {
				if s.cfg.Metrics != nil {
					s.cfg.Metrics.RunsOrphanRecovered.Inc()
				}
				if s.logger != nil {
					s.logger.Info("sweeper: orphan run %s (%s, tenant %s) → failed_resumable", ref.ID, ref.Status, ref.TenantID)
				}
			}
		}
	}

	// Edge-triggered episode summary. Per replica by design: there is no
	// shared "definitive" instant on a lock-less sweeper, so each replica
	// brackets its own episode and the aggregate lives in the counter
	// (OrphanSweepErrors, incremented every tick — the log line is the
	// comfort channel, the metric is the truth).
	//
	// The CLOSE needs positive evidence PER FAILING STAGE, or the flag
	// either flaps or latches (both halves were paid for separately): a
	// SCAN episode closes on any pass whose scans returned — an empty
	// healthy minute IS scan evidence — while a LEASE/FLIP episode wants a
	// cleanly probed candidate. But a healthy fleet may never re-produce a
	// stale candidate (a peer replica flips it during the blindness), so
	// the probe half ALSO closes after a bounded run of clean passes: a
	// latched flag makes every later outage's edge-Warn silent, an
	// optimistic close merely re-warns on the next failure.
	degraded := scanErrs+leaseErrs+flipErrs > 0
	if degraded {
		if !s.sweepDegraded {
			if s.logger != nil {
				s.logger.Warn("sweeper: orphan recovery degraded — %d scan failure(s), %d candidate(s) skipped on lease probes, %d flips failed this pass (last: %v); repeats stay quiet until it recovers", scanErrs, leaseErrs, flipErrs, lastErr)
			}
			s.sweepDegraded = true
		}
		if scanErrs > 0 {
			s.sweepDegradedByScan = true
		}
		if leaseErrs+flipErrs > 0 {
			s.sweepDegradedByProbe = true
		}
		s.sweepCleanPasses = 0
		return
	}
	if !s.sweepDegraded {
		return
	}
	s.sweepCleanPasses++
	if s.sweepDegradedByScan && scanned > 0 {
		s.sweepDegradedByScan = false
	}
	if s.sweepDegradedByProbe && (probed > 0 || s.sweepCleanPasses >= sweepProbeCloseAfter) {
		s.sweepDegradedByProbe = false
	}
	if !s.sweepDegradedByScan && !s.sweepDegradedByProbe {
		if s.logger != nil {
			s.logger.Info("sweeper: orphan recovery back to healthy")
		}
		s.sweepDegraded = false
	}
}

// sweepProbeCloseAfter bounds how many clean-but-unprobing passes close a
// lease/flip episode (~30 minutes at the sweep interval).
const sweepProbeCloseAfter = 30

// ---- DLQ admin REST ----

// QueueBackend is the slice of the cloud queue connection the server
// itself consumes: the DLQ admin endpoints and the sweeper's lease
// probe. Implemented by *natsq.Conn; the DLQ payload types stay
// natsq's, since the admin views are exactly what the queue reports.
// The endpoints are only registered when cloud mode wired a queue.
type QueueBackend interface {
	ListDLQ(ctx context.Context, cursorSeq uint64, limit int) ([]natsq.DLQMessage, uint64, error)
	PeekDLQ(ctx context.Context, seq uint64) (natsq.DLQMessage, json.RawMessage, error)
	RepublishDLQ(ctx context.Context, seq uint64) (string, error)
	DiscardDLQ(ctx context.Context, seq uint64) error
	DLQDepth(ctx context.Context) (uint64, error)
	IsRunLocked(ctx context.Context, runID string) (bool, error)
}

func (s *Server) registerQueueAdminRoutes() {
	if s.queue == nil {
		return
	}
	s.mux.Handle("GET /api/admin/dlq", s.requireSuperAdmin(http.HandlerFunc(s.handleDLQList)))
	s.mux.Handle("GET /api/admin/dlq/{seq}", s.requireSuperAdmin(http.HandlerFunc(s.handleDLQPeek)))
	s.mux.Handle("POST /api/admin/dlq/{seq}/replay", s.requireSuperAdmin(http.HandlerFunc(s.handleDLQReplay)))
	s.mux.Handle("DELETE /api/admin/dlq/{seq}", s.requireSuperAdmin(http.HandlerFunc(s.handleDLQDiscard)))
}

func dlqSeq(r *http.Request) (uint64, bool) {
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	return seq, err == nil && seq > 0
}

func (s *Server) handleDLQList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cursor, _ := strconv.ParseUint(q.Get("cursor"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	msgs, next, err := s.queue.ListDLQ(r.Context(), cursor, limit)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "dlq list: %v", err)
		return
	}
	writeJSON(w, map[string]any{"messages": msgs, "next_cursor": next})
}

func (s *Server) handleDLQPeek(w http.ResponseWriter, r *http.Request) {
	seq, ok := dlqSeq(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid seq")
		return
	}
	view, payload, err := s.queue.PeekDLQ(r.Context(), seq)
	if err != nil {
		httpError(w, http.StatusNotFound, "dlq peek: %v", err)
		return
	}
	writeJSON(w, map[string]any{"message": view, "payload": payload})
}

func (s *Server) handleDLQReplay(w http.ResponseWriter, r *http.Request) {
	seq, ok := dlqSeq(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid seq")
		return
	}
	runID, err := s.queue.RepublishDLQ(r.Context(), seq)
	if err != nil && runID == "" {
		httpError(w, http.StatusBadGateway, "dlq replay: %v", err)
		return
	}
	s.auditPlatform(r, "", "dlq.replayed", "run", runID, map[string]any{"seq": seq})
	writeJSON(w, map[string]any{"status": "replayed", "run_id": runID})
}

func (s *Server) handleDLQDiscard(w http.ResponseWriter, r *http.Request) {
	seq, ok := dlqSeq(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid seq")
		return
	}
	if err := s.queue.DiscardDLQ(r.Context(), seq); err != nil {
		httpError(w, http.StatusNotFound, "dlq discard: %v", err)
		return
	}
	s.auditPlatform(r, "", "dlq.discarded", "run", "", map[string]any{"seq": seq})
	w.WriteHeader(http.StatusNoContent)
}
