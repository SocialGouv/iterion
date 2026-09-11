package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/alert"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/clock"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

const (
	assistantWatchSweepInterval = 20 * time.Second
	assistantWatchLease         = 45 * time.Second
	assistantWatchRetryBusy     = 20 * time.Second
	assistantAutoWatchCooldown  = 300
	// assistantWatchHealthPageSize bounds one durable reconciliation pass. A
	// pending health episode holds the cursor before itself, so later pages
	// are never skipped when a child was still progressing.
	assistantWatchHealthPageSize = 250
	// A malformed ParentRunID cycle must not turn one watch into an unbounded
	// control-plane walk. Real trees are far smaller; truncation is logged.
	assistantWatchTreeNodeCap = 5000
	// Wait one normal sweep before waking a supervisor for a newly persisted
	// stall. If progress resumed immediately, alert.Manager has time to append
	// the matching stall_recovered event and the sweep remains quiet.
	assistantWatchStallSettleDelay = assistantWatchSweepInterval
)

type assistantWatchCoordinator struct {
	server               *Server
	mu                   sync.RWMutex
	store                runwatch.Store
	worker               string
	clock                clock.Clock
	healthMu             sync.RWMutex
	lastSweepStartedAt   time.Time
	lastSweepCompletedAt time.Time
	lastSweepDuration    time.Duration
	resumeRun            func(context.Context, runview.ResumeSpec) (*runview.LaunchResult, error) // test seam; nil delegates to server.runs
}

func (c *assistantWatchCoordinator) currentStore() runwatch.Store {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.store
}

func (c *assistantWatchCoordinator) setStore(next runwatch.Store) {
	c.mu.Lock()
	c.store = next
	c.mu.Unlock()
}

func (c *assistantWatchCoordinator) now() time.Time {
	c.healthMu.RLock()
	clk := c.clock
	c.healthMu.RUnlock()
	if clk == nil {
		clk = clock.Default
	}
	return clk.Now().UTC()
}

func (c *assistantWatchCoordinator) markSweepStarted(at time.Time) {
	c.healthMu.Lock()
	c.lastSweepStartedAt = at
	c.healthMu.Unlock()
}

func (c *assistantWatchCoordinator) markSweepCompleted(at time.Time, duration time.Duration) {
	c.healthMu.Lock()
	c.lastSweepCompletedAt = at
	c.lastSweepDuration = duration
	c.healthMu.Unlock()
}

type assistantWatchHeartbeat struct {
	Present     bool
	StartedAt   time.Time
	CompletedAt time.Time
	Duration    time.Duration
	Stale       bool
}

func (c *assistantWatchCoordinator) heartbeat() assistantWatchHeartbeat {
	c.healthMu.RLock()
	started, completed, duration := c.lastSweepStartedAt, c.lastSweepCompletedAt, c.lastSweepDuration
	c.healthMu.RUnlock()
	if completed.IsZero() {
		return assistantWatchHeartbeat{Present: true, StartedAt: started, CompletedAt: completed, Duration: duration, Stale: true}
	}
	return assistantWatchHeartbeat{
		Present: true, StartedAt: started, CompletedAt: completed, Duration: duration,
		Stale: c.now().Sub(completed) > 3*assistantWatchSweepInterval,
	}
}

func (c *assistantWatchCoordinator) resume(ctx context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
	if c.resumeRun != nil {
		return c.resumeRun(ctx, spec)
	}
	return c.server.runs.Resume(ctx, spec)
}

func (s *Server) startAssistantRunWatches() {
	if s.runs == nil || s.assistantWatches == nil {
		return
	}
	ctx := context.Background()
	if err := s.assistantWatches.EnsureSchema(ctx); err != nil {
		s.logWarn("assistant run watch disabled: ensure schema: %v", err)
		return
	}
	c := &assistantWatchCoordinator{server: s, store: s.assistantWatches, worker: "assistant-watch:" + uuid.NewString(), clock: clock.Default}
	s.assistantWatch = c
	if bus := s.eventsBus(); bus != nil {
		cancel, err := bus.Subscribe("assistant-run-watch", trigger.Matcher{Sources: []trigger.Source{trigger.SourceRun}}, c.handleEvent)
		if err != nil {
			s.logWarn("assistant run watch: subscribe: %v", err)
		} else {
			s.assistantWatchCancel = cancel
		}
	}
	go c.sweepLoop(s.shutdown)
}

func (c *assistantWatchCoordinator) sweepLoop(shutdown <-chan struct{}) {
	// Sweep immediately: watches created while the process was stopped must
	// not wait a full interval, and the event bus is only a fast path.
	c.sweep(context.Background())
	ticker := time.NewTicker(assistantWatchSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-shutdown:
			return
		case <-ticker.C:
			c.sweep(context.Background())
		}
	}
}

func (c *assistantWatchCoordinator) handleEvent(ctx context.Context, ev trigger.Event) error {
	if ev.Source != trigger.SourceRun || ev.Subject.ID == "" {
		return nil
	}
	// Arm BEFORE observing: a run spawned by a watched card has no link to
	// the assistant until this point, so observing first would find no watch
	// and drop the outcome on the floor.
	switch ev.Kind {
	case trigger.KindRunFailed, trigger.KindRunFinished, trigger.KindRunCancelled:
		rctx := store.WithIdentity(ctx, ev.TenantID, "")
		if target, err := c.server.runs.LoadRunCtx(rctx, ev.Subject.ID); err == nil {
			c.armWatchesForTarget(ctx, target)
		}
	}
	switch ev.Kind {
	case trigger.KindRunFailed:
		if err := c.observeFailure(ctx, ev.TenantID, ev.Subject.ID, ev.ID); err != nil {
			return err
		}
	case trigger.KindRunFinished, trigger.KindRunCancelled:
		if err := c.observeTerminal(ctx, ev.TenantID, ev.Subject.ID, ev.ID, ev.Kind); err != nil {
			return err
		}
	}
	// A non-recoverable assistant outcome owns watch cleanup. A graceful
	// server drain writes failed_resumable, which must retain its watches for
	// the next boot and resume. A pause may have made a previously-busy chat
	// safe, so opportunistically drain due episodes.
	if ev.Kind == trigger.KindRunFinished || ev.Kind == trigger.KindRunFailed || ev.Kind == trigger.KindRunCancelled {
		c.stopForTerminalAssistant(ctx, ev.TenantID, ev.Subject.ID, "assistant_"+ev.Kind)
	}
	if ev.Kind == trigger.KindRunPaused {
		c.deliverDue(ctx, 50)
	}
	return nil
}

func (c *assistantWatchCoordinator) sweep(ctx context.Context) {
	started := c.now()
	c.markSweepStarted(started)
	ws := c.currentStore()
	watches, err := ws.ListActive(ctx, 500)
	if err != nil {
		c.server.logWarn("assistant run watch: list active: %v", err)
		return
	}
	for _, w := range watches {
		rctx := store.WithIdentity(ctx, w.TenantID, w.OwnerID)
		target, err := c.server.runs.LoadRunCtx(rctx, w.TargetRunID)
		if errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrRunDeleted) {
			_ = c.stopWatch(ctx, w, runwatch.WatchStopped, "target_missing", time.Now().UTC())
			continue
		}
		if err != nil {
			continue
		}
		now := time.Now().UTC()
		started := w.CreatedAt
		if w.TreeTrackingStartedAt == nil {
			// Legacy watches baseline their already-existing descendants exactly
			// once at upgrade. New watches already carry CreatedAt here.
			started = now
		}
		w, err = ws.InitializeTreeTracking(ctx, w.ID, w.TenantID, started, now)
		if err != nil {
			c.server.logWarn("assistant run watch: initialize tree tracking watch=%s: %v", w.ID, err)
			continue
		}
		tree := c.loadWatchTree(rctx, target)
		observations := c.ensureTreeObservations(ctx, w, tree, now)
		c.observePausedHumanGatesInTree(ctx, w, tree)
		for _, observed := range tree.runs {
			if cursor, ok := observations[observed.ID]; ok {
				c.observeRunHealthInTree(ctx, w, observed, cursor, tree)
			}
			c.observeTerminalStateForWatch(ctx, w, observed, false)
		}
		assistant, aerr := c.server.runs.LoadRunCtx(rctx, w.AssistantRunID)
		if aerr == nil && assistantWatchStopsForStatus(assistant.Status) {
			_ = c.stopWatch(ctx, w, runwatch.WatchStopped, "assistant_"+string(assistant.Status), time.Now().UTC())
		}
	}
	c.reconcileArmedWatches(ctx)
	c.deliverDue(ctx, 100)
	c.markSweepCompleted(c.now(), c.now().Sub(started))
}

type assistantWatchTree struct {
	runs     []*store.Run
	children map[string][]*store.Run
}

func (c *assistantWatchCoordinator) loadWatchTree(ctx context.Context, root *store.Run) assistantWatchTree {
	tree := assistantWatchTree{children: map[string][]*store.Run{}}
	if root == nil {
		return tree
	}
	queue := []*store.Run{root}
	seen := map[string]bool{root.ID: true}
	for len(queue) > 0 && len(tree.runs) < assistantWatchTreeNodeCap {
		current := queue[0]
		queue = queue[1:]
		tree.runs = append(tree.runs, current)
		ids, err := c.server.runs.RunStore().ListChildRuns(ctx, current.ID)
		if err != nil {
			c.server.logWarn("assistant run watch: list children for tree target %s: %v", current.ID, err)
			continue
		}
		sort.Strings(ids)
		for _, id := range ids {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			child, err := c.server.runs.LoadRunCtx(ctx, id)
			if err != nil {
				c.server.logWarn("assistant run watch: load tree child %s: %v", id, err)
				continue
			}
			tree.children[current.ID] = append(tree.children[current.ID], child)
			queue = append(queue, child)
		}
	}
	if len(queue) > 0 {
		c.server.logWarn("assistant run watch: tree target %s exceeded %d nodes; sweep truncated", root.ID, assistantWatchTreeNodeCap)
	}
	return tree
}

func (c *assistantWatchCoordinator) ensureTreeObservations(ctx context.Context, w runwatch.Watch, tree assistantWatchTree, now time.Time) map[string]runwatch.RunObservation {
	out := make(map[string]runwatch.RunObservation, len(tree.runs))
	trackingStarted := w.CreatedAt
	if w.TreeTrackingStartedAt != nil {
		trackingStarted = *w.TreeTrackingStartedAt
	}
	for _, observed := range tree.runs {
		seed := runview.NoEventsSeq
		if observed.ID == w.TargetRunID {
			seed = w.LastObservedEventSeq
		} else if observed.CreatedAt.Before(trackingStarted) {
			if snap, err := c.server.runs.SnapshotCtx(store.WithIdentity(ctx, w.TenantID, w.OwnerID), observed.ID); err == nil && snap != nil {
				seed = snap.LastSeq
			}
		}
		entry := runwatch.RunObservation{RunID: observed.ID, EventSeq: seed, FirstObservedAt: now}
		persisted, _, err := c.currentStore().EnsureRunObservation(ctx, w.ID, w.TenantID, entry, now)
		if err != nil {
			c.server.logWarn("assistant run watch: initialize observation watch=%s run=%s: %v", w.ID, observed.ID, err)
			continue
		}
		out[observed.ID] = persisted
	}
	return out
}

// observePausedHumanGates finds real human gates anywhere below a watched
// root. A subbot pause deliberately leaves its parent running, so observing
// only the root outcome event would never wake the supervising assistant.
// Episodes are keyed to the paused child's stable outcome id, making this
// sweep idempotent across restarts and repeated reconciliation passes.
func (c *assistantWatchCoordinator) observePausedHumanGates(ctx context.Context, w runwatch.Watch, target *store.Run) {
	if !watchIncludes(w, trigger.KindRunPaused) || target == nil {
		return
	}
	rctx := store.WithIdentity(ctx, w.TenantID, w.OwnerID)
	c.observePausedHumanGatesInTree(ctx, w, c.loadWatchTree(rctx, target))
}

func (c *assistantWatchCoordinator) observePausedHumanGatesInTree(ctx context.Context, w runwatch.Watch, tree assistantWatchTree) {
	if !watchIncludes(w, trigger.KindRunPaused) {
		return
	}
	for _, current := range tree.runs {
		if current.Status == store.RunStatusPausedWaitingHuman && current.Checkpoint != nil {
			c.observePausedHumanGate(ctx, w, current)
		}
	}
}

func (c *assistantWatchCoordinator) observePausedHumanGate(ctx context.Context, w runwatch.Watch, gate *store.Run) {
	if gate == nil || gate.Checkpoint == nil || gate.Status != store.RunStatusPausedWaitingHuman {
		return
	}
	now := time.Now().UTC()
	eventID := trigger.RunOutcomeEventID(gate.ID, string(gate.Status), gate.Checkpoint.InteractionID, gate.UpdatedAt)
	ep := runwatch.Episode{
		ID:                  episodeID(w.ID, eventID),
		WatchID:             w.ID,
		TenantID:            w.TenantID,
		TargetRunID:         w.TargetRunID,
		ObservedRunID:       gate.ID,
		AssistantRunID:      w.AssistantRunID,
		OutcomeEventID:      eventID,
		Kind:                trigger.KindRunPaused,
		PausedRunID:         gate.ID,
		PausedNodeID:        gate.Checkpoint.NodeID,
		PausedInteractionID: gate.Checkpoint.InteractionID,
		FailureFingerprint:  "paused:" + gate.ID + ":" + gate.Checkpoint.InteractionID,
		State:               runwatch.EpisodePending,
		NextAttemptAt:       now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	created, err := c.currentStore().CreateEpisode(ctx, ep)
	if err != nil {
		c.server.logWarn("assistant run watch: create paused-gate episode target=%s gate=%s: %v", w.TargetRunID, gate.ID, err)
		return
	}
	if created {
		c.attempt(ctx, ep.ID)
	}
}

// observeRunHealth reconciles persisted alert episodes. The trigger bus is a
// fast path elsewhere in the product, but it is intentionally lossy, so a
// supervisor wake-up must be derived from this durable event stream.
func (c *assistantWatchCoordinator) observeRunHealth(ctx context.Context, w runwatch.Watch, target *store.Run) {
	if target == nil {
		return
	}
	rctx := store.WithIdentity(ctx, w.TenantID, w.OwnerID)
	tree := c.loadWatchTree(rctx, target)
	observations := c.ensureTreeObservations(ctx, w, tree, time.Now().UTC())
	if cursor, ok := observations[target.ID]; ok {
		c.observeRunHealthInTree(ctx, w, target, cursor, tree)
	}
}

func (c *assistantWatchCoordinator) observeRunHealthInTree(ctx context.Context, w runwatch.Watch, target *store.Run, cursor runwatch.RunObservation, tree assistantWatchTree) {
	if !watchIncludes(w, trigger.KindRunStalled) || target == nil || target.Status != store.RunStatusRunning {
		return
	}
	from := cursor.EventSeq + 1
	if from < 0 {
		from = 0
	}
	rctx := store.WithIdentity(ctx, w.TenantID, w.OwnerID)
	events, err := c.server.runs.RunStore().LoadEventsRange(rctx, target.ID, from, 0, assistantWatchHealthPageSize)
	if err != nil || len(events) == 0 {
		if err != nil {
			c.server.logWarn("assistant run watch: reconcile health target %s: %v", target.ID, err)
		}
		return
	}

	now := time.Now().UTC()
	advanceTo := events[len(events)-1].Seq
	for i, evt := range events {
		if evt.Type != store.EventRunHealth || healthKind(evt) != "stall" {
			continue
		}
		if recovered, ok := matchingHealthRecovery(events[i+1:], evt); ok {
			c.logHealthInfo("suppress recovered stall target=%s stall_seq=%d recovery_seq=%d", target.ID, evt.Seq, recovered.Seq)
			continue
		}
		// A transient alert that has just been persisted gets one full sweep
		// interval for its recovery twin to arrive. Keep the cursor before the
		// stall so the next pass observes that twin if it appears.
		if !evt.Timestamp.IsZero() && now.Sub(evt.Timestamp) < assistantWatchStallSettleDelay {
			advanceTo = evt.Seq - 1
			c.logHealthInfo("pending young stall target=%s seq=%d", target.ID, evt.Seq)
			break
		}
		progressing, progressErr := c.hasProgressingDescendantInTree(rctx, tree, target.ID, now)
		if progressErr != nil || progressing {
			advanceTo = evt.Seq - 1
			if progressErr != nil {
				c.server.logWarn("assistant run watch: retain stall target=%s seq=%d: descendant check: %v", target.ID, evt.Seq, progressErr)
			} else {
				c.logHealthInfo("suppress active-descendant stall target=%s seq=%d", target.ID, evt.Seq)
			}
			break
		}
		ep := runwatch.Episode{
			ID: episodeID(w.ID, trigger.RunHealthEventID(target.ID, evt.Seq)), WatchID: w.ID, TenantID: w.TenantID,
			TargetRunID: w.TargetRunID, ObservedRunID: target.ID, AssistantRunID: w.AssistantRunID,
			OutcomeEventID: trigger.RunHealthEventID(target.ID, evt.Seq), Kind: trigger.KindRunStalled,
			HealthEventSeq: evt.Seq, HealthNodeID: evt.NodeID, HealthReason: healthReason(evt),
			FailureFingerprint: "health:stall:" + evt.NodeID,
			State:              runwatch.EpisodePending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		}
		created, createErr := c.currentStore().CreateEpisode(ctx, ep)
		if createErr != nil {
			c.server.logWarn("assistant run watch: create stall episode target=%s seq=%d: %v", target.ID, evt.Seq, createErr)
			advanceTo = evt.Seq - 1
			break
		}
		if created {
			c.attempt(ctx, ep.ID)
		}
	}
	if advanceTo > cursor.EventSeq {
		if err := c.currentStore().AdvanceObservedRunEventSeq(ctx, w.ID, w.TenantID, target.ID, advanceTo, now); err != nil {
			c.server.logWarn("assistant run watch: advance health cursor watch=%s target=%s: %v", w.ID, target.ID, err)
		}
	}
}

func (c *assistantWatchCoordinator) logHealthInfo(format string, args ...any) {
	if c.server.logger != nil {
		c.server.logger.Info("assistant run watch: "+format, args...)
	}
}

func healthKind(evt *store.Event) string {
	if evt == nil || evt.Data == nil {
		return ""
	}
	kind, _ := evt.Data["kind"].(string)
	return kind
}

func healthReason(evt *store.Event) string {
	if evt == nil || evt.Data == nil {
		return ""
	}
	reason, _ := evt.Data["reason"].(string)
	return reason
}

func matchingHealthRecovery(events []*store.Event, stall *store.Event) (*store.Event, bool) {
	if stall == nil {
		return nil, false
	}
	for _, evt := range events {
		if evt.Type == store.EventRunHealth && evt.NodeID == stall.NodeID && healthKind(evt) == "stall_recovered" {
			return evt, true
		}
	}
	return nil, false
}

func (c *assistantWatchCoordinator) hasProgressingDescendantInTree(ctx context.Context, tree assistantWatchTree, targetID string, now time.Time) (bool, error) {
	window := alert.DefaultStallTimeout
	if m := c.server.runs.AlertManager(); m != nil && m.StallTimeout() > 0 {
		window = m.StallTimeout()
	}
	deadline := now.Add(-window)
	rs := c.server.runs.RunStore()
	queue := []string{targetID}
	seen := map[string]bool{targetID: true}
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]
		for _, child := range tree.children[parentID] {
			if child == nil || child.ID == "" || seen[child.ID] {
				continue
			}
			seen[child.ID] = true
			queue = append(queue, child.ID)
			if child.Status == store.RunStatusQueued {
				return true, nil
			}
			if child.Status != store.RunStatusRunning {
				continue
			}
			// A just-created running descendant can have no persisted event
			// yet. Its creation timestamp is enough to cover startup.
			if child.CreatedAt.After(deadline) {
				return true, nil
			}
			progressing := false
			err := rs.ScanEvents(ctx, child.ID, func(evt *store.Event) bool {
				if evt.Type != store.EventRunHealth && evt.Timestamp.After(deadline) {
					progressing = true
					return false
				}
				return true
			})
			if err != nil {
				return false, err
			}
			if progressing {
				return true, nil
			}
		}
	}
	return false, nil
}

func (c *assistantWatchCoordinator) observeFailure(ctx context.Context, tenant, targetID, eventID string) error {
	rctx := store.WithIdentity(ctx, tenant, "")
	observed, err := c.server.runs.LoadRunCtx(rctx, targetID)
	if err != nil {
		return err
	}
	watches, err := c.coveringWatches(ctx, tenant, observed)
	if err != nil {
		return err
	}
	for _, w := range watches {
		if err := c.observeFailureForWatch(ctx, w, observed, eventID); err != nil {
			return err
		}
	}
	return nil
}

func (c *assistantWatchCoordinator) observeFailureForWatch(ctx context.Context, w runwatch.Watch, observed *store.Run, eventID string) error {
	if observed == nil || !watchIncludes(w, trigger.KindRunFailed) {
		return nil
	}
	// The native retry sweeper already owns this episode. Waking a model now
	// would spend a turn diagnosing a failure that is scheduled to disappear.
	if observed.Status == store.RunStatusFailedResumable && observed.RetryState != nil && observed.RetryState.RetryAfter != nil {
		return nil
	}
	rctx := store.WithIdentity(ctx, w.TenantID, w.OwnerID)
	resolved, err := loadAssistantRun(rctx, observed.ID, c.server.runs.RunStore())
	if err != nil {
		return nil
	}
	now := time.Now().UTC()
	ep := runwatch.Episode{
		ID: episodeID(w.ID, eventID), WatchID: w.ID, TenantID: w.TenantID,
		TargetRunID: w.TargetRunID, ObservedRunID: observed.ID, AssistantRunID: w.AssistantRunID,
		OutcomeEventID: eventID, Kind: trigger.KindRunFailed, FailureFingerprint: assistantFailureFingerprint(resolved),
		State: runwatch.EpisodePending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	created, err := c.currentStore().CreateEpisode(ctx, ep)
	if err != nil {
		return err
	}
	if created {
		c.attempt(ctx, ep.ID)
	}
	return nil
}

func watchIncludes(w runwatch.Watch, kind string) bool {
	for _, candidate := range w.Kinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

func (c *assistantWatchCoordinator) observeTerminal(ctx context.Context, tenant, targetID, eventID, kind string) error {
	rctx := store.WithIdentity(ctx, tenant, "")
	observed, err := c.server.runs.LoadRunCtx(rctx, targetID)
	if err != nil {
		return err
	}
	watches, err := c.coveringWatches(ctx, tenant, observed)
	if err != nil {
		return err
	}
	for _, w := range watches {
		if err := c.observeTerminalForWatch(ctx, w, observed, eventID, kind); err != nil {
			return err
		}
	}
	return nil
}

func (c *assistantWatchCoordinator) coveringWatches(ctx context.Context, tenant string, observed *store.Run) ([]runwatch.Watch, error) {
	if observed == nil {
		return nil, nil
	}
	ws := c.currentStore()
	rctx := store.WithIdentity(ctx, tenant, "")
	seenRuns := map[string]bool{}
	seenWatches := map[string]bool{}
	var out []runwatch.Watch
	for current := observed; current != nil && current.ID != "" && !seenRuns[current.ID]; {
		seenRuns[current.ID] = true
		watches, err := ws.ListActiveByTarget(ctx, tenant, current.ID)
		if err != nil {
			return nil, err
		}
		for _, watch := range watches {
			if !seenWatches[watch.ID] {
				seenWatches[watch.ID] = true
				out = append(out, watch)
			}
		}
		if current.ParentRunID == "" {
			break
		}
		parent, err := c.server.runs.LoadRunCtx(rctx, current.ParentRunID)
		if err != nil {
			break
		}
		current = parent
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (c *assistantWatchCoordinator) observeTerminalForWatch(ctx context.Context, w runwatch.Watch, observed *store.Run, eventID, kind string) error {
	if observed == nil {
		return nil
	}
	// A successful child is progress inside the rooted tree. It must neither
	// wake Copi nor pass through the legacy branch that resolves a watch when
	// run.finished is not selected.
	if kind == trigger.KindRunFinished && observed.ID != w.TargetRunID {
		return nil
	}
	// Done is the sole root-owned automatic end of a watch. Cancellation is
	// resumable and transient during rewind, so it never resolves the link.
	if kind == trigger.KindRunFinished && !watchIncludes(w, kind) {
		return c.stopWatch(ctx, w, runwatch.WatchResolved, "target_"+kind, time.Now().UTC())
	}
	if !watchIncludes(w, kind) {
		return nil
	}
	now := time.Now().UTC()
	ep := runwatch.Episode{
		ID: episodeID(w.ID, eventID), WatchID: w.ID, TenantID: w.TenantID,
		TargetRunID: w.TargetRunID, ObservedRunID: observed.ID, AssistantRunID: w.AssistantRunID,
		OutcomeEventID: eventID, Kind: kind, FailureFingerprint: "outcome:" + kind,
		State: runwatch.EpisodePending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	created, err := c.currentStore().CreateEpisode(ctx, ep)
	if err != nil {
		return err
	}
	if created {
		c.attempt(ctx, ep.ID)
	}
	return nil
}

func assistantFailureFingerprint(r *assistantResolvedRun) string {
	if r == nil {
		return "unknown"
	}
	errorExcerpt := stableFailureText(r.Error)
	sum := sha256.Sum256([]byte(r.FailingNode + "\x00" + r.ErrorCode + "\x00" + errorExcerpt))
	return r.FailingNode + ":" + r.ErrorCode + ":" + hex.EncodeToString(sum[:8])
}

var volatileFailureText = regexp.MustCompile(`(?i)(?:\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b|\b\d{4}-\d{2}-\d{2}T[^ ]+|\b0x[0-9a-f]+\b)`)

func stableFailureText(s string) string {
	s = strings.Join(strings.Fields(volatileFailureText.ReplaceAllString(s, "<volatile>")), " ")
	return truncateRunes(s, assistantContextMaxError)
}

func episodeID(watchID, outcomeID string) string {
	sum := sha256.Sum256([]byte(watchID + "\x00" + outcomeID))
	return "watch-episode-" + hex.EncodeToString(sum[:16])
}

func (c *assistantWatchCoordinator) deliverDue(ctx context.Context, limit int) {
	ws := c.currentStore()
	episodes, err := ws.ListDueEpisodes(ctx, time.Now().UTC(), limit)
	if err != nil {
		c.server.logWarn("assistant run watch: list episodes: %v", err)
		return
	}
	for _, ep := range episodes {
		c.attempt(ctx, ep.ID)
	}
}

func (c *assistantWatchCoordinator) attempt(ctx context.Context, episodeID string) {
	ws := c.currentStore()
	now := time.Now().UTC()
	ep, won, err := ws.ClaimEpisode(ctx, episodeID, c.worker, now, assistantWatchLease)
	if err != nil || !won {
		return
	}
	release := func(base, max time.Duration, reason string) {
		delay := assistantWatchRetryDelay(ep.Attempts, base, max)
		if ep.Attempts >= 8 && ep.Attempts&(ep.Attempts-1) == 0 {
			c.server.logWarn(
				"assistant run watch: episode %s (watch %s, target %s) remains pending after %d attempts — %s; retrying in %s",
				ep.ID, ep.WatchID, ep.TargetRunID, ep.Attempts, reason, delay,
			)
		}
		_ = ws.ReleaseEpisode(ctx, ep.ID, c.worker, now.Add(delay), reason)
	}
	w, err := ws.GetWatch(ctx, ep.WatchID)
	if err != nil || w.State != runwatch.WatchActive {
		_ = ws.BlockEpisode(ctx, ep.ID, c.worker, now, "watch_not_active")
		return
	}
	if w.LastDeliveredAt != nil && w.CooldownSeconds > 0 {
		next := w.LastDeliveredAt.Add(time.Duration(w.CooldownSeconds) * time.Second)
		if next.After(now) {
			_ = ws.ReleaseEpisode(ctx, ep.ID, c.worker, next, "cooldown")
			return
		}
	}
	rctx := store.WithIdentity(ctx, w.TenantID, w.OwnerID)
	assistant, err := c.server.runs.LoadRunCtx(rctx, w.AssistantRunID)
	if err != nil {
		release(time.Minute, 15*time.Minute, "assistant_unavailable")
		return
	}
	switch assistant.Status {
	case store.RunStatusRunning, store.RunStatusQueued:
		release(assistantWatchRetryBusy, 5*time.Minute, "assistant_busy")
		return
	case store.RunStatusPausedOperator:
		release(time.Minute, 15*time.Minute, "assistant_paused_by_operator")
		return
	case store.RunStatusFailedResumable:
		release(time.Minute, 15*time.Minute, "assistant_failed_resumable")
		return
	case store.RunStatusPausedWaitingHuman:
		// handled below
	default:
		_ = ws.BlockEpisode(ctx, ep.ID, c.worker, now, "assistant_"+string(assistant.Status))
		_ = c.stopWatch(ctx, w, runwatch.WatchStopped, "assistant_"+string(assistant.Status), now)
		return
	}
	nodeID, hostField, err := c.server.safeAssistantWatchPause(rctx, assistant)
	if err != nil || assistant.Checkpoint == nil || assistant.Checkpoint.NodeID != nodeID {
		// Most importantly this covers ask_user mid-turn: its checkpoint is the
		// agent node, not the manifest chat node, so the pending question is never stolen.
		release(assistantWatchRetryBusy, 5*time.Minute, "assistant_not_at_chat_boundary")
		return
	}
	if assistantBudgetNearCap(assistant) {
		release(5*time.Minute, 30*time.Minute, "assistant_budget_near_cap")
		return
	}
	target, err := loadAssistantRun(rctx, w.TargetRunID, c.server.runs.RunStore())
	if err != nil {
		release(time.Minute, 15*time.Minute, "target_unavailable")
		return
	}
	observedID := ep.ObservedRunID
	if observedID == "" && ep.PausedRunID != "" {
		observedID = ep.PausedRunID
	}
	if observedID == "" {
		observedID = ep.TargetRunID
	}
	observed := target
	if observedID != w.TargetRunID {
		observed, err = loadAssistantRun(rctx, observedID, c.server.runs.RunStore())
		if err != nil {
			release(time.Minute, 15*time.Minute, "outcome_run_unavailable")
			return
		}
	}
	eventKind := ep.Kind
	if eventKind == "" {
		eventKind = trigger.KindRunFailed
	}
	if eventKind == trigger.KindRunCancelled && observed.Status != store.RunStatusCancelled {
		// Rewind claims cancelled only briefly before parking paused_operator.
		// A delayed episode about that transient state is stale, not a reason to
		// wake the assistant; completing it keeps the watch itself active.
		_ = ws.CompleteEpisode(ctx, ep.ID, c.worker, time.Now().UTC())
		return
	}
	hostEvent := map[string]any{
		"kind": "assistant-watch-event", "event": eventKind, "watch_id": w.ID,
		"episode_id": ep.ID, "outcome_event_id": ep.OutcomeEventID,
		"mode": string(w.Mode), "target_run": target,
		"outcome_run": observed,
		"authority":   "iterion-host", "operator_authorized": false,
	}
	if eventKind == trigger.KindRunStalled {
		hostEvent["health"] = map[string]any{
			"event_seq": ep.HealthEventSeq,
			"node_id":   ep.HealthNodeID,
			"reason":    ep.HealthReason,
		}
	}
	if eventKind == trigger.KindRunPaused {
		if !c.pausedGateStillActive(rctx, ep) {
			_ = ws.CompleteEpisode(ctx, ep.ID, c.worker, time.Now().UTC())
			return
		}
		hostEvent["human_gate"] = map[string]any{
			"run_id":         ep.PausedRunID,
			"node_id":        ep.PausedNodeID,
			"interaction_id": ep.PausedInteractionID,
			"status":         string(store.RunStatusPausedWaitingHuman),
		}
	}
	hostInputs, err := c.server.assistantChatHostInputs(rctx, assistant)
	if err != nil {
		_ = ws.ReleaseEpisode(ctx, ep.ID, c.worker, now.Add(assistantWatchRetryBusy), "chat_history_projection_failed")
		return
	}
	if _, err := c.resume(rctx, runview.ResumeSpec{
		RunID: assistant.ID, FilePath: assistant.FilePath,
		Answers: map[string]any{hostField: hostEvent}, HostInputs: hostInputs,
		// This is a host-attested delivery at a verified chat boundary, not
		// an operator's resume. Copi's own configuration may legitimately
		// have changed in a product deploy since it armed the veille; without
		// Force the durable episode would remain pending until it expires and
		// the assistant would silently stop watching the target.
		Force: true,
	}); err != nil {
		_ = ws.ReleaseEpisode(ctx, ep.ID, c.worker, now.Add(assistantWatchRetryBusy), truncateRunes(err.Error(), 300))
		return
	}
	if err := ws.CompleteEpisode(ctx, ep.ID, c.worker, time.Now().UTC()); err != nil {
		c.server.logWarn("assistant run watch: mark episode %s delivered: %v", ep.ID, err)
	}
	if eventKind == trigger.KindRunFinished && observedID == w.TargetRunID {
		_ = c.stopWatch(ctx, w, runwatch.WatchResolved, "target_"+eventKind, time.Now().UTC())
	}
}

func assistantWatchRetryDelay(attempts int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = assistantWatchRetryBusy
	}
	if max < base {
		max = base
	}
	delay := base
	for i := 1; i < attempts && delay < max; i++ {
		if delay > max/2 {
			return max
		}
		delay *= 2
	}
	if delay > max {
		return max
	}
	return delay
}

// pausedGateStillActive prevents a delayed supervisor wake-up from reporting
// a gate that an operator has already answered. A later gate has its own
// interaction-backed episode and will be delivered independently.
func (c *assistantWatchCoordinator) pausedGateStillActive(ctx context.Context, ep runwatch.Episode) bool {
	if ep.PausedRunID == "" {
		return false
	}
	gate, err := c.server.runs.LoadRunCtx(ctx, ep.PausedRunID)
	if err != nil || gate == nil || gate.Status != store.RunStatusPausedWaitingHuman || gate.Checkpoint == nil {
		return false
	}
	return (ep.PausedNodeID == "" || gate.Checkpoint.NodeID == ep.PausedNodeID) &&
		(ep.PausedInteractionID == "" || gate.Checkpoint.InteractionID == ep.PausedInteractionID)
}

func assistantBudgetNearCap(r *store.Run) bool {
	if r == nil || r.Budget == nil || r.Checkpoint == nil {
		return false
	}
	cp, b := r.Checkpoint, r.Budget
	if b.MaxTokens > 0 && float64(cp.BudgetTokensUsed)/float64(b.MaxTokens) >= .9 {
		return true
	}
	if b.MaxCostUSD > 0 && cp.BudgetCostUSD/b.MaxCostUSD >= .9 {
		return true
	}
	if b.MaxIterations > 0 && float64(cp.BudgetIterationsUsed)/float64(b.MaxIterations) >= .9 {
		return true
	}
	return false
}

func (s *Server) safeAssistantWatchPause(ctx context.Context, run *store.Run) (string, string, error) {
	if run == nil || run.Checkpoint == nil || run.Checkpoint.BackendPendingToolUseID != "" {
		return "", "", errors.New("not at chat boundary")
	}
	chat, err := s.loadAssistantChatSurface(ctx, run)
	if err != nil {
		return "", "", fmt.Errorf("resolve assistant manifest: %w", err)
	}
	if chat == nil {
		return "", "", errors.New("assistant chat manifest is missing")
	}
	n, ok := chat.Nodes[run.Checkpoint.NodeID]
	if !ok || n.Kind != bundle.ChatNodeHuman || n.HostEventField == "" {
		return "", "", errors.New("pause is not host-event-capable chat node")
	}
	if !isSafeAssistantChatPause(run, run.Checkpoint.NodeID) {
		return "", "", errors.New("not at safe chat boundary")
	}
	return run.Checkpoint.NodeID, n.HostEventField, nil
}

func isSafeAssistantChatPause(run *store.Run, chatNodeID string) bool {
	return run != nil && run.Status == store.RunStatusPausedWaitingHuman &&
		run.Checkpoint != nil && run.Checkpoint.NodeID == chatNodeID &&
		run.Checkpoint.BackendPendingToolUseID == ""
}

func (c *assistantWatchCoordinator) stopForAssistant(ctx context.Context, tenant, assistantID, reason string) {
	ws := c.currentStore()
	watches, _ := ws.ListActiveByAssistant(ctx, tenant, assistantID)
	for _, w := range watches {
		_ = c.stopWatch(ctx, w, runwatch.WatchStopped, reason, time.Now().UTC())
	}
}

// stopForTerminalAssistant re-reads the exact persisted status because the
// shared run.failed outcome kind represents both failed and
// failed_resumable. A graceful server drain writes failed_resumable, and that
// operational interruption must retain the assistant's watches for the next
// boot. A load error is likewise not evidence that the watch should end.
func (c *assistantWatchCoordinator) stopForTerminalAssistant(ctx context.Context, tenant, assistantID, reason string) {
	rctx := store.WithIdentity(ctx, tenant, "")
	assistant, err := c.server.runs.LoadRunCtx(rctx, assistantID)
	if err != nil || assistant == nil || !assistantWatchStopsForStatus(assistant.Status) {
		return
	}
	c.stopForAssistant(ctx, tenant, assistantID, reason)
}

func assistantWatchStopsForStatus(status store.RunStatus) bool {
	switch status {
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusCancelled:
		return true
	default:
		return false
	}
}

type createAssistantWatchRequest struct {
	AssistantRunID string        `json:"assistant_run_id"`
	Mode           runwatch.Mode `json:"mode"`
	MaxEpisodes    int           `json:"max_episodes,omitempty"`
	// Pointer distinguishes an omitted pacing preference from an explicit 0.
	// That matters when a child request widens an existing ancestor watch.
	CooldownSeconds *int     `json:"cooldown_seconds,omitempty"`
	Kinds           []string `json:"kinds,omitempty"`
}

type assistantWatchResponse struct {
	runwatch.Watch
	CoveredRunID string `json:"covered_run_id,omitempty"`
}

func (s *Server) findCoveringAncestorWatch(ctx context.Context, target *store.Run, owner, assistantID string) (runwatch.Watch, bool) {
	if target == nil || s.assistantWatches == nil {
		return runwatch.Watch{}, false
	}
	seen := map[string]bool{target.ID: true}
	parentID := target.ParentRunID
	for parentID != "" && !seen[parentID] {
		seen[parentID] = true
		parent, err := s.runs.LoadRunCtx(ctx, parentID)
		if err != nil {
			return runwatch.Watch{}, false
		}
		watches, err := s.assistantWatches.ListActiveByTarget(ctx, target.TenantID, parent.ID)
		if err != nil {
			return runwatch.Watch{}, false
		}
		for _, candidate := range watches {
			if candidate.OwnerID == owner && candidate.AssistantRunID == assistantID {
				return candidate, true
			}
		}
		parentID = parent.ParentRunID
	}
	return runwatch.Watch{}, false
}

func mergeAssistantWatchPolicy(existing runwatch.Watch, requested createAssistantWatchRequest, requestedCooldown int) runwatch.Watch {
	merged := existing
	wanted := make(map[string]bool, len(existing.Kinds)+len(requested.Kinds))
	for _, kind := range existing.Kinds {
		wanted[kind] = true
	}
	for _, kind := range requested.Kinds {
		wanted[kind] = true
	}
	// The request cap remains four, but a root can legitimately carry all five
	// after absorbing a broader child policy.
	canonical := []string{
		trigger.KindRunPaused,
		trigger.KindRunFailed,
		trigger.KindRunStalled,
		trigger.KindRunFinished,
		trigger.KindRunCancelled,
	}
	merged.Kinds = make([]string, 0, len(canonical))
	for _, kind := range canonical {
		if wanted[kind] {
			merged.Kinds = append(merged.Kinds, kind)
		}
	}
	if requested.Mode == runwatch.ModePropose {
		merged.Mode = runwatch.ModePropose
	}
	if requested.CooldownSeconds != nil && requestedCooldown < merged.CooldownSeconds {
		merged.CooldownSeconds = requestedCooldown
	}
	merged.MaxEpisodes = 0
	merged.UpdatedAt = time.Now().UTC()
	return merged
}

func (s *Server) handleCreateAssistantWatch(w http.ResponseWriter, r *http.Request) {
	// A watch arms a durable link that later force-resumes the assistant run
	// and spends LLM budget, or transfers an existing watch away from
	// another assistant. That is a state change, so it takes the same gate
	// as its sibling handleCreateAssistantMission — decodeJSONCapped does
	// not check Content-Type, so without it a page on any origin could
	// arm one with a preflight-free POST at the loopback studio.
	if !s.requireSafeOrigin(w, r) || s.rejectCrossStoreWrite(w, r) {
		return
	}
	if s.assistantWatches == nil {
		s.httpErrorFor(w, r, http.StatusNotImplemented, "assistant run watch is unavailable")
		return
	}
	var req createAssistantWatchRequest
	if !decodeJSONCapped(s, w, r, &req, 8<<10) {
		return
	}
	targetID := strings.TrimSpace(r.PathValue("id"))
	req.AssistantRunID = strings.TrimSpace(req.AssistantRunID)
	if targetID == "" || req.AssistantRunID == "" || targetID == req.AssistantRunID {
		s.httpErrorFor(w, r, http.StatusBadRequest, "target and assistant run ids are required and must differ")
		return
	}
	if req.Mode == "" {
		req.Mode = runwatch.ModeDiagnose
	}
	if req.Mode != runwatch.ModeDiagnose && req.Mode != runwatch.ModePropose {
		s.httpErrorFor(w, r, http.StatusBadRequest, "mode must be diagnose or propose; auto_safe is not available until server-side action authority is configured")
		return
	}
	if len(req.Kinds) == 0 {
		req.Kinds = []string{trigger.KindRunFailed}
	}
	if len(req.Kinds) > 4 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid watch event kinds")
		return
	}
	seenKinds := map[string]bool{}
	for _, kind := range req.Kinds {
		if kind != trigger.KindRunFailed && kind != trigger.KindRunFinished && kind != trigger.KindRunCancelled && kind != trigger.KindRunStalled && kind != trigger.KindRunPaused || seenKinds[kind] {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid watch event kinds")
			return
		}
		seenKinds[kind] = true
	}
	// max_episodes is accepted only for compatibility with older Studio and
	// Copi payloads. The lifetime is now server-owned: 0 means unlimited and
	// every legacy positive value is normalized to it.
	if req.MaxEpisodes < 0 || req.MaxEpisodes > 20 || req.CooldownSeconds != nil && (*req.CooldownSeconds < 0 || *req.CooldownSeconds > 86400) {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid watch limits")
		return
	}
	req.MaxEpisodes = 0
	requestedCooldown := 0
	if req.CooldownSeconds != nil {
		requestedCooldown = *req.CooldownSeconds
	}
	target, err := s.runs.LoadRunCtx(r.Context(), targetID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "target run not found")
		return
	}
	assistant, err := s.runs.LoadRunCtx(r.Context(), req.AssistantRunID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "assistant run not found")
		return
	}
	if assistantWatchStopsForStatus(assistant.Status) {
		s.httpErrorFor(w, r, http.StatusConflict, "assistant run is %s", assistant.Status)
		return
	}
	ident, _ := auth.FromContext(r.Context())
	owner := ident.UserID
	if owner == "" {
		owner = assistant.OwnerID
	}
	if covering, ok := s.findCoveringAncestorWatch(r.Context(), target, owner, assistant.ID); ok {
		merged := mergeAssistantWatchPolicy(covering, req, requestedCooldown)
		updated, found, updateErr := s.assistantWatches.ReconfigureActiveWatch(r.Context(), merged)
		if updateErr != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "merge covering watch: %v", updateErr)
			return
		}
		if !found {
			// The ancestor may have ended between lookup and CAS. Re-read once;
			// never manufacture a nested watch from a stale coverage decision.
			if retry, retryOK := s.findCoveringAncestorWatch(r.Context(), target, owner, assistant.ID); retryOK {
				merged = mergeAssistantWatchPolicy(retry, req, requestedCooldown)
				updated, found, updateErr = s.assistantWatches.ReconfigureActiveWatch(r.Context(), merged)
			}
			if updateErr != nil || !found {
				s.httpErrorFor(w, r, http.StatusConflict, "covering ancestor watch changed; retry the request")
				return
			}
		}
		if s.assistantWatch != nil {
			go s.assistantWatch.sweep(context.Background())
		}
		s.writeJSONFor(w, r, assistantWatchResponse{Watch: updated, CoveredRunID: target.ID})
		return
	}
	now := time.Now().UTC()
	lastObservedSeq := runview.NoEventsSeq
	if snap, snapErr := s.runs.SnapshotCtx(r.Context(), target.ID); snapErr != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "read target event cursor: %v", snapErr)
		return
	} else if snap != nil {
		lastObservedSeq = snap.LastSeq
	}
	watch := runwatch.Watch{
		ID: uuid.NewString(), TenantID: target.TenantID, OwnerID: owner, TargetRunID: target.ID, AssistantRunID: assistant.ID,
		Mode: req.Mode, Kinds: req.Kinds, State: runwatch.WatchActive,
		MaxEpisodes: req.MaxEpisodes, CooldownSeconds: requestedCooldown, LastObservedEventSeq: lastObservedSeq,
		TreeTrackingStartedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.writeWatchCreate(r.Context(), watch); err != nil {
		if errors.Is(err, runwatch.ErrAlreadyExists) {
			// POST is retried by the Studio and may also be proposed again after
			// the target resumes. A failure episode does not consume the watch:
			// the same link deliberately remains active until the target finishes
			// or the operator stops it. Returning 409 for the exact same intent turns that
			// healthy state into a false action failure for the operator.
			existing, listErr := s.assistantWatches.ListActiveByTarget(
				r.Context(), watch.TenantID, watch.TargetRunID,
			)
			if listErr != nil {
				s.httpErrorFor(w, r, http.StatusInternalServerError, "lookup existing watch: %v", listErr)
				return
			}
			for handoffAttempt := 0; handoffAttempt < 2; handoffAttempt++ {
				var incumbent *runwatch.Watch
				for i := range existing {
					candidate := existing[i]
					if sameAssistantWatchIntent(candidate, watch) {
						s.writeJSONFor(w, r, assistantWatchResponse{Watch: candidate})
						return
					}
					// A same-assistant reconfiguration preserves the existing row
					// without changing who is supervising it.
					if candidate.TenantID == watch.TenantID &&
						candidate.OwnerID == watch.OwnerID &&
						candidate.TargetRunID == watch.TargetRunID &&
						candidate.AssistantRunID == watch.AssistantRunID {
						updated, found, updateErr := s.assistantWatches.ReconfigureActiveWatch(r.Context(), watch)
						if updateErr != nil {
							s.httpErrorFor(w, r, http.StatusInternalServerError, "reconfigure watch: %v", updateErr)
							return
						}
						if found {
							s.writeJSONFor(w, r, assistantWatchResponse{Watch: updated})
							if s.assistantWatch != nil {
								go s.assistantWatch.sweep(context.Background())
							}
							return
						}
						continue
					}
					if candidate.TenantID == watch.TenantID &&
						candidate.OwnerID == watch.OwnerID &&
						candidate.TargetRunID == watch.TargetRunID {
						incumbent = &candidate
					}
				}
				if incumbent == nil {
					break
				}
				incumbentRun, loadErr := s.runs.LoadRunCtx(r.Context(), incumbent.AssistantRunID)
				if loadErr != nil {
					// Never take over a watch when the incumbent cannot be
					// established; the operator sees an honest conflict instead.
					break
				}
				if incumbentRun.Status == store.RunStatusQueued || incumbentRun.Status == store.RunStatusRunning {
					// A queued/running assistant may be in the middle of a real
					// recovery. Its watch cannot be displaced by another chat.
					break
				}
				updated, found, transferErr := s.writeWatchTransfer(r.Context(), *incumbent, watch)
				if transferErr != nil {
					s.httpErrorFor(w, r, http.StatusInternalServerError, "transfer watch: %v", transferErr)
					return
				}
				if found {
					s.writeJSONFor(w, r, assistantWatchResponse{Watch: updated})
					if s.assistantWatch != nil {
						go s.assistantWatch.sweep(context.Background())
					}
					return
				}
				// The outgoing assistant changed after inspection. Re-read once
				// rather than overwriting a new owner with stale evidence.
				existing, listErr = s.assistantWatches.ListActiveByTarget(r.Context(), watch.TenantID, watch.TargetRunID)
				if listErr != nil {
					s.httpErrorFor(w, r, http.StatusInternalServerError, "lookup changed watch: %v", listErr)
					return
				}
			}
			s.httpErrorFor(w, r, http.StatusConflict, "this run already has an active assistant watch")
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "create watch: %v", err)
		return
	}
	s.writeJSONFor(w, r, assistantWatchResponse{Watch: watch})
	if s.assistantWatch != nil && (target.Status == store.RunStatusFailed || target.Status == store.RunStatusFailedResumable) {
		eventID := trigger.RunOutcomeEventID(target.ID, string(target.Status), "", target.UpdatedAt)
		go func() { _ = s.assistantWatch.observeFailure(context.Background(), target.TenantID, target.ID, eventID) }()
	} else if s.assistantWatch != nil && (target.Status == store.RunStatusFinished || target.Status == store.RunStatusCancelled) {
		kind := trigger.KindRunFinished
		if target.Status == store.RunStatusCancelled {
			kind = trigger.KindRunCancelled
		}
		eventID := trigger.RunOutcomeEventID(target.ID, string(target.Status), "", target.UpdatedAt)
		go func() {
			_ = s.assistantWatch.observeTerminal(context.Background(), target.TenantID, target.ID, eventID, kind)
		}()
	}
}

// sameAssistantWatchIntent identifies a safe idempotent create retry. Runtime
// delivery fields (episode count and timestamps) and the deprecated episode
// cap are deliberately ignored; changing the assistant or policy remains a
// real ownership conflict.
func sameAssistantWatchIntent(existing, requested runwatch.Watch) bool {
	if existing.TenantID != requested.TenantID ||
		existing.OwnerID != requested.OwnerID ||
		existing.TargetRunID != requested.TargetRunID ||
		existing.AssistantRunID != requested.AssistantRunID ||
		existing.Mode != requested.Mode ||
		existing.CooldownSeconds != requested.CooldownSeconds ||
		len(existing.Kinds) != len(requested.Kinds) {
		return false
	}
	wantedKinds := make(map[string]struct{}, len(requested.Kinds))
	for _, kind := range requested.Kinds {
		wantedKinds[kind] = struct{}{}
	}
	for _, kind := range existing.Kinds {
		if _, ok := wantedKinds[kind]; !ok {
			return false
		}
	}
	return true
}

func (s *Server) handleListAssistantWatches(w http.ResponseWriter, r *http.Request) {
	if s.assistantWatches == nil {
		s.writeJSONFor(w, r, []runwatch.Watch{})
		return
	}
	run, err := s.runs.LoadRunCtx(r.Context(), r.PathValue("id"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found")
		return
	}
	watches, err := s.listCoveringAssistantWatches(r.Context(), run)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list watches: %v", err)
		return
	}
	s.writeJSONFor(w, r, watches)
}

func (s *Server) listCoveringAssistantWatches(ctx context.Context, run *store.Run) ([]assistantWatchResponse, error) {
	if run == nil {
		return []assistantWatchResponse{}, nil
	}
	seen := map[string]bool{}
	seenRuns := map[string]bool{}
	current := run
	out := make([]assistantWatchResponse, 0)
	for current != nil && current.ID != "" && !seenRuns[current.ID] {
		seenRuns[current.ID] = true
		watches, err := s.assistantWatches.ListActiveByTarget(ctx, run.TenantID, current.ID)
		if err != nil {
			return nil, err
		}
		for _, watch := range watches {
			if seen[watch.ID] {
				continue
			}
			seen[watch.ID] = true
			response := assistantWatchResponse{Watch: watch}
			if watch.TargetRunID != run.ID {
				response.CoveredRunID = run.ID
			}
			out = append(out, response)
		}
		if current.ParentRunID == "" {
			break
		}
		parent, err := s.runs.LoadRunCtx(ctx, current.ParentRunID)
		if err != nil {
			break
		}
		current = parent
	}
	return out, nil
}

func (s *Server) handleStopAssistantWatch(w http.ResponseWriter, r *http.Request) {
	// Disarming one is a state change too — the mirror of the create gate
	// above, and of handleStopAssistantMission.
	if !s.requireSafeOrigin(w, r) || s.rejectCrossStoreWrite(w, r) {
		return
	}
	if s.assistantWatches == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "watch not found")
		return
	}
	watch, err := s.assistantWatches.GetWatch(r.Context(), r.PathValue("watchID"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "watch not found")
		return
	}
	if _, err := s.runs.LoadRunCtx(r.Context(), watch.TargetRunID); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "watch not found")
		return
	}
	if err := s.writeWatchStop(r.Context(), watch, runwatch.WatchStopped, "operator", time.Now().UTC()); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "stop watch: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"id": watch.ID, "state": runwatch.WatchStopped})
}

// resolveAssistantChatCapability answers "could this run EVER be woken by a
// host event?" — a property of the bot, not of where the run happens to be
// standing. It deliberately does NOT go through the checkpoint the way
// safeAssistantWatchPause does: arming happens when a watched target ends,
// which is very often while the assistant is mid-turn. Reading the checkpoint
// there would resolve the agent node, fail the lookup, and silently skip
// arming a perfectly capable assistant — a race whose only symptom is Copi
// never speaking.
//
// safeAssistantWatchPause keeps its own, stricter check: capability is not
// permission to deliver.
func (s *Server) resolveAssistantChatCapability(ctx context.Context, run *store.Run) (string, string, error) {
	chat, err := s.loadAssistantChatSurface(ctx, run)
	if err != nil {
		return "", "", fmt.Errorf("resolve assistant manifest: %w", err)
	}
	if chat == nil {
		return "", "", errors.New("assistant chat manifest is missing")
	}
	// Sorted so a bot declaring two host-event gates arms deterministically
	// instead of picking whatever the map iteration handed us this time.
	ids := make([]string, 0, len(chat.Nodes))
	for id := range chat.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := chat.Nodes[id]
		if n.Kind == bundle.ChatNodeHuman && n.HostEventField != "" {
			return id, n.HostEventField, nil
		}
	}
	return "", "", errors.New("bot declares no host-event-capable chat node")
}

// armWatchesForTarget links every assistant run watching a card to the run
// that card just produced, at the moment that run reaches a terminal state.
//
// The link is written on ARRIVAL rather than at dispatch on purpose: the
// dispatcher stamps the card's last_run before the run record exists
// (pkg/dispatcher/loop.go — the id is minted, the store write happens
// off-actor afterwards), so arming there would race the sweep's
// target_missing cleanup and need a grace period whose mis-tuning is silent.
// Here the target has just emitted its own outcome, so it certainly exists.
//
// Idempotent: CreateWatch rejects a duplicate active link, and the episode id
// is derived from (watch, outcome event), so the fast path and the
// reconciliation sweep cannot double-deliver.
func (c *assistantWatchCoordinator) armWatchesForTarget(ctx context.Context, target *store.Run) {
	if target == nil || target.Source == nil || target.Source.IssueID == "" {
		return
	}
	watchers, err := c.listIssueWatchers(ctx)
	if err != nil {
		c.server.logWarn("assistant run watch: arm for issue %s: list runs: %v", target.Source.IssueID, err)
		return
	}
	c.armWatchesForTargetFrom(ctx, target, watchers)
}

// listIssueWatchers loads the runs that carry at least one watched card and
// can still be delivered to. It is the ONLY full-store pass either arming
// path needs: a sweep builds it once and hands it to every target, instead
// of re-listing and re-loading every run in the store per target.
func (c *assistantWatchCoordinator) listIssueWatchers(ctx context.Context) ([]*store.Run, error) {
	rs := c.server.runs.RunStore()
	ids, err := rs.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	watchers := make([]*store.Run, 0, 8)
	for _, runID := range ids {
		watcher, err := rs.LoadRun(ctx, runID)
		if err != nil || watcher == nil || len(watcher.WatchedIssueIDs) == 0 {
			continue
		}
		if !watchDeliverable(watcher.Status) {
			continue
		}
		watchers = append(watchers, watcher)
	}
	return watchers, nil
}

// armWatchesForTargetFrom is armWatchesForTarget over an already-loaded
// candidate set. Splitting it is what takes the reconciliation sweep from
// quadratic back to the single store pass its own comment claims.
func (c *assistantWatchCoordinator) armWatchesForTargetFrom(ctx context.Context, target *store.Run, watchers []*store.Run) {
	if target == nil || target.Source == nil || target.Source.IssueID == "" {
		return
	}
	issueID := target.Source.IssueID
	now := time.Now().UTC()
	for _, watcher := range watchers {
		if watcher == nil || watcher.ID == target.ID {
			continue
		}
		if !slices.Contains(watcher.WatchedIssueIDs, issueID) {
			continue
		}
		// A card watched by a NON-conversational run must not open an
		// episode: nothing would ever be able to deliver it, and the
		// coordinator would re-attempt it every 20s for the rest of that
		// run's life. This guard is load-bearing, not cosmetic.
		if _, _, err := c.server.resolveAssistantChatCapability(ctx, watcher); err != nil {
			continue
		}
		// Replaying a card's history would ambush an operator who armed a
		// watch this morning with every failure since last week. Without an
		// arming timestamp on WatchedIssueIDs, the assistant run's own start
		// is the honest lower bound available today.
		if target.CreatedAt.Before(watcher.CreatedAt) {
			continue
		}
		if covering, ok := c.server.findCoveringAncestorWatch(ctx, target, watcher.OwnerID, watcher.ID); ok {
			merged := mergeAssistantWatchPolicy(covering, createAssistantWatchRequest{
				AssistantRunID: watcher.ID,
				Mode:           runwatch.ModeDiagnose,
				Kinds:          autoWatchKinds(),
			}, 0)
			if _, found, mergeErr := c.currentStore().ReconfigureActiveWatch(ctx, merged); mergeErr != nil {
				c.server.logWarn("assistant run watch: merge auto coverage %s -> %s: %v", watcher.ID, covering.TargetRunID, mergeErr)
			} else if found {
				continue
			}
		}
		lastObservedSeq := runview.NoEventsSeq
		if snap, snapErr := c.server.runs.SnapshotCtx(store.WithIdentity(ctx, target.TenantID, watcher.OwnerID), target.ID); snapErr == nil && snap != nil {
			lastObservedSeq = snap.LastSeq
		}
		watch := runwatch.Watch{
			ID: uuid.NewString(), TenantID: target.TenantID, OwnerID: watcher.OwnerID,
			TargetRunID: target.ID, AssistantRunID: watcher.ID,
			Mode: runwatch.ModeDiagnose, Kinds: autoWatchKinds(), State: runwatch.WatchActive,
			CooldownSeconds: assistantAutoWatchCooldown, LastObservedEventSeq: lastObservedSeq,
			TreeTrackingStartedAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if err := c.createWatch(ctx, watch); err != nil {
			if !errors.Is(err, runwatch.ErrAlreadyExists) {
				c.server.logWarn("assistant run watch: arm %s -> %s: %v", watcher.ID, target.ID, err)
			}
			continue
		}
	}
}

// autoWatchKinds is deliberately narrower than what an operator may request
// by hand. run.finished ("the delegated task reports back") is the loop we
// eventually want, but it makes the assistant speak on every successful
// delegation — a change in character to open after a measured dogfood, not
// on the first pass.
func autoWatchKinds() []string {
	return []string{trigger.KindRunFailed, trigger.KindRunCancelled}
}

// reconcileArmedWatches is the net under the event bus, which is lossy. The
// fast path arms from a run-outcome event; if that event is dropped, nothing
// else would ever notice, because the sweep can only re-observe watches that
// already exist. So the sweep also walks the other way round: from each live
// assistant veille to the terminal runs its watched cards produced.
//
// Finding the veilles costs ONE store pass (listIssueWatchers); from there
// ListRunsBySourceIssue is indexed, so the rest is bounded by the number of
// active veilles rather than by the size of the store. That is what the
// comment used to claim while the code did the opposite: the outer loop
// listed and loaded every run, and each terminal target it found triggered
// ANOTHER full list-and-load inside armWatchesForTarget — quadratic in the
// store, every 20s, plus once per run-outcome event. A long-lived store
// degraded the server continuously.
func (c *assistantWatchCoordinator) reconcileArmedWatches(ctx context.Context) {
	rs := c.server.runs.RunStore()
	watchers, err := c.listIssueWatchers(ctx)
	if err != nil {
		return
	}
	// A target reached through two watchers, or through two cards of the
	// same watcher, is the common shape — arming and observing it twice in
	// one sweep is pure repeat work (both are idempotent, so it was only
	// ever cost).
	seen := make(map[string]struct{}, len(watchers))
	for _, watcher := range watchers {
		for _, issueID := range watcher.WatchedIssueIDs {
			targets, err := rs.ListRunsBySourceIssue(ctx, issueID)
			if err != nil {
				continue
			}
			for _, targetID := range targets {
				if targetID == watcher.ID {
					continue
				}
				if _, done := seen[targetID]; done {
					continue
				}
				seen[targetID] = struct{}{}
				target, err := rs.LoadRun(ctx, targetID)
				if err != nil || target == nil || !target.Status.IsTerminal() {
					continue
				}
				c.armWatchesForTargetFrom(ctx, target, watchers)
				c.observeTerminalState(ctx, target)
			}
		}
	}
}

// observeTerminalState re-derives the outcome event a terminal run WOULD have
// published and feeds it to the ordinary observe path. Shared by the sweep's
// two entry points so a re-derived event id is computed in exactly one place
// (episode dedup keys on it).
func (c *assistantWatchCoordinator) observeTerminalState(ctx context.Context, target *store.Run) {
	switch target.Status {
	case store.RunStatusFailed, store.RunStatusFailedResumable:
		eventID := trigger.RunOutcomeEventID(target.ID, string(target.Status), "", target.UpdatedAt)
		_ = c.observeFailure(ctx, target.TenantID, target.ID, eventID)
	case store.RunStatusFinished, store.RunStatusCancelled:
		kind := trigger.KindRunFinished
		if target.Status == store.RunStatusCancelled {
			kind = trigger.KindRunCancelled
		}
		eventID := trigger.RunOutcomeEventID(target.ID, string(target.Status), "", target.UpdatedAt)
		_ = c.observeTerminal(ctx, target.TenantID, target.ID, eventID, kind)
	}
}

// observeTerminalStateForWatch is the durable tree reconciliation path. The
// per-watch tracking stamp baselines descendants that existed before a legacy
// watch learned tree semantics, while children created afterwards remain
// observable even when they start and finish between two sweeps.
func (c *assistantWatchCoordinator) observeTerminalStateForWatch(ctx context.Context, w runwatch.Watch, observed *store.Run, direct bool) {
	if observed == nil || !observed.Status.IsTerminal() {
		return
	}
	if observed.ID != w.TargetRunID && !direct {
		floor := w.CreatedAt
		if w.TreeTrackingStartedAt != nil && w.TreeTrackingStartedAt.After(floor) {
			floor = *w.TreeTrackingStartedAt
		}
		if observed.UpdatedAt.Before(floor) {
			return
		}
	}
	switch observed.Status {
	case store.RunStatusFailed, store.RunStatusFailedResumable:
		eventID := trigger.RunOutcomeEventID(observed.ID, string(observed.Status), "", observed.UpdatedAt)
		_ = c.observeFailureForWatch(ctx, w, observed, eventID)
	case store.RunStatusFinished, store.RunStatusCancelled:
		kind := trigger.KindRunFinished
		if observed.Status == store.RunStatusCancelled {
			kind = trigger.KindRunCancelled
		}
		eventID := trigger.RunOutcomeEventID(observed.ID, string(observed.Status), "", observed.UpdatedAt)
		_ = c.observeTerminalForWatch(ctx, w, observed, eventID, kind)
	}
}

// createWatch, transferWatch and stopWatch are the ONLY writers of a
// runwatch link, so the observational event the dock renders cannot drift
// from the state it describes. The target disappearing, the assistant ending,
// target Done, an explicit standby handoff and the operator's button all pass
// through these functions.
func (c *assistantWatchCoordinator) createWatch(ctx context.Context, w runwatch.Watch) error {
	if err := c.currentStore().CreateWatch(ctx, w); err != nil {
		return err
	}
	c.publishVeille(ctx, store.EventAssistantVeilleArmed, w, "")
	return nil
}

func (c *assistantWatchCoordinator) stopWatch(ctx context.Context, w runwatch.Watch, state runwatch.WatchState, reason string, now time.Time) error {
	if err := c.currentStore().StopWatch(ctx, w.ID, w.TenantID, state, reason, now); err != nil {
		return err
	}
	c.publishVeille(ctx, store.EventAssistantVeilleStopped, w, reason)
	return nil
}

// transferWatch preserves the durable delivery ledger while moving future
// supervision to an explicitly requested standby assistant. A claimed episode
// may still deliver once to the outgoing assistant; future delivery resolves
// Watch.AssistantRunID after this compare-and-swap.
func (c *assistantWatchCoordinator) transferWatch(ctx context.Context, existing, requested runwatch.Watch) (runwatch.Watch, bool, error) {
	updated, found, err := c.currentStore().TransferActiveWatch(ctx, existing.AssistantRunID, requested)
	if err != nil || !found {
		return updated, found, err
	}
	c.publishVeille(ctx, store.EventAssistantVeilleStopped, existing, "watch_transferred")
	c.publishVeille(ctx, store.EventAssistantVeilleArmed, updated, "")
	return updated, true, nil
}

// publishVeille writes onto the ASSISTANT's log: the operator reads that
// transcript to understand why the assistant went quiet, and later why it
// spoke without being asked. The target run's own log is not the place.
func (c *assistantWatchCoordinator) publishVeille(ctx context.Context, typ store.EventType, w runwatch.Watch, reason string) {
	rs := c.server.runs.RunStore()
	publish := c.server.runs.BrokerPublish()
	if typ == store.EventAssistantVeilleArmed {
		store.PublishVeilleArmed(ctx, rs, publish, w.AssistantRunID, store.VeilleChannelRun, w.TargetRunID)
		return
	}
	store.PublishVeilleStopped(ctx, rs, publish, w.AssistantRunID, store.VeilleChannelRun, w.TargetRunID, reason)
}

// writeWatchCreate / writeWatchTransfer / writeWatchStop are the HTTP
// handlers' door into the same choke point. The coordinator owns the
// emission, but it may be nil (the watch store is wired before
// startAssistantRunWatches, and stays wired in tests that never start a
// coordinator) — so the write must not depend on it.
func (s *Server) writeWatchCreate(ctx context.Context, w runwatch.Watch) error {
	if s.assistantWatch != nil {
		return s.assistantWatch.createWatch(ctx, w)
	}
	return s.assistantWatches.CreateWatch(ctx, w)
}

func (s *Server) writeWatchStop(ctx context.Context, w runwatch.Watch, state runwatch.WatchState, reason string, now time.Time) error {
	if s.assistantWatch != nil {
		return s.assistantWatch.stopWatch(ctx, w, state, reason, now)
	}
	return s.assistantWatches.StopWatch(ctx, w.ID, w.TenantID, state, reason, now)
}

func (s *Server) writeWatchTransfer(ctx context.Context, existing, requested runwatch.Watch) (runwatch.Watch, bool, error) {
	if s.assistantWatch != nil {
		return s.assistantWatch.transferWatch(ctx, existing, requested)
	}
	return s.assistantWatches.TransferActiveWatch(ctx, existing.AssistantRunID, requested)
}

// runVeilleResponse is what the dock needs to render "standing by" and to
// offer ONE button that actually stops it. Both channels travel together
// because stopping only the run watches is not idempotent: the card veille
// would re-arm a fresh watch at that card's next dispatch, and the operator
// who pressed "stop" would be spoken to again anyway.
type runVeilleResponse struct {
	RunWatches      []runwatch.Watch `json:"run_watches"`
	WatchedIssueIDs []string         `json:"watched_issue_ids"`
}

// handleListRunVeille is the reconciliation read behind the dock's live
// events: the banner is pushed by assistant_veille_* on the WS, and this is
// what a freshly mounted dock (or one that missed an event) calls to catch up.
// Same fast-path + poll discipline the board already uses.
func (s *Server) handleListRunVeille(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.LoadRunCtx(r.Context(), r.PathValue("id"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found")
		return
	}
	out := runVeilleResponse{RunWatches: []runwatch.Watch{}, WatchedIssueIDs: run.WatchedIssueIDs}
	if out.WatchedIssueIDs == nil {
		out.WatchedIssueIDs = []string{}
	}
	if s.assistantWatches != nil {
		watches, err := s.assistantWatches.ListActiveByAssistant(r.Context(), run.TenantID, run.ID)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "list watches: %v", err)
			return
		}
		if watches != nil {
			out.RunWatches = watches
		}
	}
	s.writeJSONFor(w, r, out)
}

// hasActiveVeille reports whether a run is parked BECAUSE it is standing by,
// rather than because it is waiting on the operator. usernotify reads it to
// stay truthful: "your run is waiting on you" is false for an assistant armed
// on a card or a run outcome, and a notification that cries wolf is worse
// than none.
func (s *Server) hasActiveVeille(ctx context.Context, runID string) bool {
	run, err := s.runs.LoadRunCtx(ctx, runID)
	if err != nil || run == nil {
		return false
	}
	if len(run.WatchedIssueIDs) > 0 {
		return true
	}
	if s.assistantWatches == nil {
		return false
	}
	watches, err := s.assistantWatches.ListActiveByAssistant(ctx, run.TenantID, run.ID)
	return err == nil && len(watches) > 0
}
