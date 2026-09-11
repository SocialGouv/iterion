package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/assistantmission"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

const (
	assistantMissionSweepInterval = 10 * time.Second
	assistantMissionLease         = 30 * time.Second
	assistantMissionReceiptGrace  = 2 * time.Minute
	assistantMissionPageSize      = 250
)

type assistantMissionCoordinator struct {
	server   *Server
	runs     *runview.Service
	watches  runwatch.Store
	missions assistantmission.Store
	worker   string
	// wake carries at most one pending sweep request from the event bus to
	// sweepLoop. Buffered size 1 so a burst of run events coalesces into one
	// extra pass instead of one goroutine each.
	wake chan struct{}
}

func (s *Server) startAssistantMissions() {
	if s.runs == nil || s.assistantWatches == nil || s.assistantMissions == nil || s.cfg.RecoveryPassive {
		return
	}
	if err := s.assistantMissions.EnsureSchema(context.Background()); err != nil {
		s.logWarn("assistant missions disabled: ensure schema: %v", err)
		return
	}
	s.restartAssistantMissions(s.runs, s.assistantWatches, s.assistantMissions)
}

// restartAssistantMissions is called at boot AND on a project switch, from
// projects.go AFTER s.stateMu is released — while a request goroutine reads
// s.assistantMission and Shutdown reads s.assistantMissionCancel. Both
// fields are therefore shared state and take s.stateMu: `race` is a
// required check here, and the interleaving that made it one (a switch
// racing a shutdown) also drops a cancel and leaks a coordinator.
//
// The lock is never held across the bus subscription or the previous
// coordinator's cancel — eventsBus and the cancel closure reach back into
// the server.
func (s *Server) restartAssistantMissions(runs *runview.Service, watches runwatch.Store, missions assistantmission.Store) {
	s.stateMu.Lock()
	prevCancel := s.assistantMissionCancel
	s.assistantMission, s.assistantMissionCancel = nil, nil
	s.stateMu.Unlock()
	if prevCancel != nil {
		prevCancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &assistantMissionCoordinator{server: s, runs: runs, watches: watches, missions: missions, worker: "assistant-mission:" + uuid.NewString()}
	c.wake = make(chan struct{}, 1)
	cancelBus := func() {}
	if bus := s.eventsBus(); bus != nil {
		if stop, err := bus.Subscribe("assistant-mission-"+uuid.NewString(), trigger.Matcher{Sources: []trigger.Source{trigger.SourceRun}}, c.handleEvent); err != nil {
			s.logWarn("assistant mission: subscribe: %v", err)
		} else {
			cancelBus = stop
		}
	}

	s.stateMu.Lock()
	s.assistantMission = c
	s.assistantMissionCancel = func() { cancelBus(); cancel() }
	s.stateMu.Unlock()
	go c.sweepLoop(ctx)
}

// handleEvent coalesces instead of spawning. The matcher filters only by
// source, so every started/finished/failed/cancelled/paused of every run
// arrives here; `go c.sweep(context.Background())` per event was unbounded
// in count AND detached from the ctx restartAssistantMissions cancels, so a
// burst produced a burst of concurrent ListReconcileCandidates(250) passes
// and a project switch left them running against the old stores.
//
// A size-1 buffered channel is the whole mechanism: one pending wake-up is
// all a sweep that re-reads the store needs, and the drain happens on the
// coordinator's own goroutine under its own ctx.
func (c *assistantMissionCoordinator) handleEvent(context.Context, trigger.Event) error {
	c.nudge()
	return nil
}

// nudge asks for one extra sweep without blocking the caller. Dropping the
// request when one is already queued is the point: a sweep re-reads the
// whole candidate page, so a second pending wake-up would find nothing the
// first will not.
func (c *assistantMissionCoordinator) nudge() {
	if c == nil || c.wake == nil {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *assistantMissionCoordinator) sweepLoop(ctx context.Context) {
	c.sweep(ctx)
	ticker := time.NewTicker(assistantMissionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			c.sweep(ctx)
		case <-ticker.C:
			c.sweep(ctx)
		}
	}
}

func (c *assistantMissionCoordinator) sweep(ctx context.Context) {
	now := time.Now().UTC()
	missions, err := c.missions.ListReconcileCandidates(ctx, now, assistantMissionPageSize)
	if err != nil {
		c.server.logWarn("assistant mission: list reconcile candidates: %v", err)
		return
	}
	for _, mission := range missions {
		c.attempt(ctx, mission.ID)
	}
}

func (c *assistantMissionCoordinator) attempt(ctx context.Context, id string) {
	now := time.Now().UTC()
	m, won, err := c.missions.Claim(ctx, id, c.worker, now, assistantMissionLease)
	if err != nil || !won {
		return
	}
	mctx := store.WithIdentity(context.WithoutCancel(ctx), m.TenantID, m.OperatorID)
	update := func(next assistantmission.Mission) bool {
		next.UpdatedAt = time.Now().UTC()
		persisted, updateErr := c.missions.UpdateClaimed(mctx, next, c.worker)
		if updateErr != nil {
			c.server.logWarn("assistant mission %s: persist: %v", m.ID, updateErr)
			return false
		}
		m = persisted
		return true
	}
	terminal := func(state assistantmission.State, reason string) {
		at := time.Now().UTC()
		m.State, m.Reason, m.TerminalAt = state, reason, &at
		update(m)
	}
	if !m.Policy.ExpiresAt.IsZero() && !now.Before(m.Policy.ExpiresAt) {
		terminal(assistantmission.StateExpired, "mission TTL expired")
		return
	}
	target, err := c.runs.LoadRunCtx(mctx, m.TargetRunID)
	if err != nil || target == nil || target.TenantID != m.TenantID || target.ProjectPath != m.ProjectID && target.ProjectPath != "" {
		m.State, m.Reason = assistantmission.StateCapabilityBlocked, "target binding is no longer available"
		update(m)
		return
	}
	if target.Status == store.RunStatusFinished {
		terminal(assistantmission.StateCompleted, "target run finished")
		return
	}
	if target.Status == store.RunStatusFailed {
		terminal(assistantmission.StateCompleted, "target run failed without a resumable checkpoint")
		return
	}
	if target.Status == store.RunStatusCancelled && !hasIssuedAction(m) {
		terminal(assistantmission.StateStopped, "target run was cancelled")
		return
	}
	watch, err := c.watches.GetWatch(mctx, m.WatchID)
	if err != nil || watch.State != runwatch.WatchActive || watch.TenantID != m.TenantID || watch.TargetRunID != m.TargetRunID || watch.AssistantRunID != m.AssistantRunID || watch.OwnerID != m.OperatorID {
		m.State, m.Reason = assistantmission.StateCapabilityBlocked, "bound assistant watch is no longer active"
		update(m)
		return
	}
	assistant, err := c.runs.LoadRunCtx(mctx, m.AssistantRunID)
	if err != nil || assistant == nil || assistant.TenantID != m.TenantID || assistantWatchStopsForStatus(assistant.Status) {
		m.State, m.Reason = assistantmission.StateCapabilityBlocked, "bound assistant run is unavailable"
		update(m)
		return
	}
	if m.Activation == nil {
		m.Activation = &assistantmission.DeliveryReceipt{ID: deterministicMissionID(m.ID, "activation"), Kind: "assistant-mission-started", State: assistantmission.ReceiptPrepared}
		m.State, m.Reason = assistantmission.StateActive, "waiting to activate assistant mission"
		update(m)
		return
	}
	if !c.reconcileDelivery(mctx, &m, m.Activation, assistant.ID, update) {
		return
	}
	if m.Activation.State == assistantmission.ReceiptPrepared {
		if _, _, boundaryErr := c.server.safeAssistantWatchPause(mctx, assistant); boundaryErr != nil {
			m.State, m.Reason = assistantmission.StateActive, "waiting for assistant chat boundary"
			update(m)
			return
		}
		attempted := time.Now().UTC()
		m.Activation.State, m.Activation.AttemptedAt = assistantmission.ReceiptIssued, &attempted
		if !update(m) {
			return
		}
		event := map[string]any{"mission_id": m.ID, "target_run_id": m.TargetRunID, "watch_id": m.WatchID, "actions": m.Policy.Actions, "expires_at": m.Policy.ExpiresAt, "max_actions": m.Policy.MaxActions}
		if err := c.server.deliverHostEventWithService(mctx, c.runs, assistant.ID, m.Activation.Kind, event, false, m.Activation.ID); err != nil {
			m.Activation.State, m.Activation.Error = assistantmission.ReceiptRejected, err.Error()
			m.State, m.Reason = assistantmission.StateCapabilityBlocked, "mission activation was rejected"
			update(m)
		}
		return
	}
	if m.Activation.State != assistantmission.ReceiptSucceeded {
		return
	}

	if idx := unresolvedActionIndex(m); idx >= 0 {
		c.advanceAction(mctx, &m, idx, target, assistant, update, terminal)
		return
	}
	if m.DispatchedActions >= m.Policy.MaxActions {
		terminal(assistantmission.StateExhausted, "mission action budget exhausted")
		return
	}
	if target.Status == store.RunStatusPausedWaitingHuman {
		m.State, m.Reason = assistantmission.StateWaitingHuman, "target is waiting for operator input"
		update(m)
		return
	}
	proposal, nodeID, version, found, scanErr := c.nextProposal(mctx, &m)
	if scanErr != nil {
		m.State, m.Reason = assistantmission.StateCapabilityBlocked, scanErr.Error()
		update(m)
		return
	}
	if !found {
		if m.State == assistantmission.StateCapabilityBlocked {
			// Keep the actionable rejection visible until the assistant emits a
			// newer artifact or the bound capability changes.
			return
		}
		m.State, m.Reason = assistantmission.StateActive, "waiting for assistant action proposal"
		update(m)
		return
	}
	receipt := c.prepareActionReceipt(&m, proposal, nodeID, version, target)
	if receipt.State == assistantmission.ReceiptPrepared && receipt.Action == assistantmission.ActionRewind {
		auto, _ := proposal.Args["auto"].(bool)
		node, _ := proposal.Args["node_id"].(string)
		pivot, pivotErr := c.runs.ResolveRewindPivot(mctx, runview.RewindSpec{RunID: m.TargetRunID, Auto: auto, NodeID: node, RestoreScope: runview.RestoreScopeNone})
		if pivotErr != nil || pivot.NodeID == pivot.EntryNodeID {
			receipt.State = assistantmission.ReceiptRejected
			if pivotErr != nil {
				receipt.Error = pivotErr.Error()
			} else {
				receipt.Error = "rewinding the workflow entry is outside mission authority"
			}
		} else {
			receipt.ExpectedPivot = pivot.NodeID
		}
	}
	m.Receipts = append(m.Receipts, receipt)
	m.State, m.Reason = assistantmission.StateActive, "assistant proposal recorded"
	update(m)
}

func (c *assistantMissionCoordinator) reconcileDelivery(ctx context.Context, m *assistantmission.Mission, receipt *assistantmission.DeliveryReceipt, assistantID string, update func(assistantmission.Mission) bool) bool {
	if receipt == nil || receipt.State != assistantmission.ReceiptIssued {
		return true
	}
	recorded, err := c.server.hostEventReceiptRecorded(ctx, c.runs, assistantID, receipt.Kind, receipt.ID)
	if err != nil {
		return false
	}
	if recorded {
		now := time.Now().UTC()
		receipt.State, receipt.CompletedAt, receipt.Error = assistantmission.ReceiptSucceeded, &now, ""
		return update(*m)
	}
	if receipt.AttemptedAt != nil && time.Since(*receipt.AttemptedAt) >= assistantMissionReceiptGrace {
		receipt.State, receipt.Error = assistantmission.ReceiptUncertain, "receipt outcome is unknown; action will not be retried"
		m.State, m.Reason = assistantmission.StateCapabilityBlocked, receipt.Error
		update(*m)
	}
	return false
}

func (c *assistantMissionCoordinator) advanceAction(ctx context.Context, m *assistantmission.Mission, idx int, target, assistant *store.Run, update func(assistantmission.Mission) bool, terminal func(assistantmission.State, string)) {
	r := &m.Receipts[idx]
	if r.State == assistantmission.ReceiptPrepared {
		if r.Action == assistantmission.ActionResume && target.Status != r.ExpectedStatus || r.Action == assistantmission.ActionRewind && target.Status == store.RunStatusRunning {
			r.State, r.Error, r.UpdatedAt = assistantmission.ReceiptRejected, "target precondition changed before dispatch", time.Now().UTC()
			update(*m)
			return
		}
		r.State, r.UpdatedAt = assistantmission.ReceiptIssued, time.Now().UTC()
		m.DispatchedActions++
		if !update(*m) {
			return
		}
		r = &m.Receipts[idx]
		var err error
		switch r.Action {
		case assistantmission.ActionResume:
			_, err = c.runs.Resume(ctx, runview.ResumeSpec{RunID: m.TargetRunID, FilePath: target.FilePath, ExpectedStatus: r.ExpectedStatus, ReceiptID: r.ID})
		case assistantmission.ActionRewind:
			_, err = c.runs.Rewind(ctx, runview.RewindSpec{RunID: m.TargetRunID, NodeID: r.ExpectedPivot, ExpectedPivot: r.ExpectedPivot, RestoreScope: runview.RestoreScopeNone, ReceiptID: r.ID})
		}
		if err != nil {
			r.State, r.Error, r.UpdatedAt = assistantmission.ReceiptRejected, err.Error(), time.Now().UTC()
			update(*m)
		}
		return
	}
	if r.State == assistantmission.ReceiptIssued {
		typ := store.EventRunResumed
		if r.Action == assistantmission.ActionRewind {
			typ = store.EventRunRewound
		}
		seq, recorded, err := actionReceiptRecorded(ctx, c.runs, m.TargetRunID, typ, r.ID)
		if err != nil {
			return
		}
		if !recorded {
			if time.Since(r.UpdatedAt) >= assistantMissionReceiptGrace {
				r.State, r.Error = assistantmission.ReceiptUncertain, "target mutation outcome is unknown; action will not be retried"
				m.State, m.Reason = assistantmission.StateCapabilityBlocked, r.Error
				update(*m)
			}
			return
		}
		r.State, r.EventSeq, r.UpdatedAt = assistantmission.ReceiptSucceeded, seq, time.Now().UTC()
		update(*m)
		return
	}
	if r.State == assistantmission.ReceiptUncertain {
		m.State, m.Reason = assistantmission.StateCapabilityBlocked, r.Error
		update(*m)
		return
	}
	if r.State == assistantmission.ReceiptSucceeded || r.State == assistantmission.ReceiptRejected {
		if r.ResultDelivery == nil {
			r.ResultDelivery = &assistantmission.DeliveryReceipt{ID: deterministicMissionID(r.ID, "result"), Kind: "assistant-mission-action-result", State: assistantmission.ReceiptPrepared}
			update(*m)
			return
		}
		if !c.reconcileDelivery(ctx, m, r.ResultDelivery, assistant.ID, update) {
			return
		}
		r = &m.Receipts[idx]
		if r.ResultDelivery.State == assistantmission.ReceiptPrepared {
			if _, _, err := c.server.safeAssistantWatchPause(ctx, assistant); err != nil {
				m.State, m.Reason = assistantmission.StateActive, "waiting to deliver action result at assistant chat boundary"
				update(*m)
				return
			}
			now := time.Now().UTC()
			r.ResultDelivery.State, r.ResultDelivery.AttemptedAt = assistantmission.ReceiptIssued, &now
			if !update(*m) {
				return
			}
			r = &m.Receipts[idx]
			event := map[string]any{"mission_id": m.ID, "target_run_id": m.TargetRunID, "action": r.Action, "action_receipt_id": r.ID, "state": r.State, "error": r.Error, "event_seq": r.EventSeq}
			if err := c.server.deliverHostEventWithService(ctx, c.runs, assistant.ID, r.ResultDelivery.Kind, event, false, r.ResultDelivery.ID); err != nil {
				r.ResultDelivery.State, r.ResultDelivery.Error = assistantmission.ReceiptRejected, err.Error()
				m.State, m.Reason = assistantmission.StateCapabilityBlocked, "action result delivery was rejected"
				update(*m)
			}
			return
		}
		if r.ResultDelivery.State == assistantmission.ReceiptSucceeded {
			if m.DispatchedActions >= m.Policy.MaxActions {
				terminal(assistantmission.StateExhausted, "mission action budget exhausted")
				return
			}
			m.State, m.Reason = assistantmission.StateActive, "waiting for assistant action proposal"
			update(*m)
		}
	}
}

func unresolvedActionIndex(m assistantmission.Mission) int {
	for i := range m.Receipts {
		r := m.Receipts[i]
		if r.State == assistantmission.ReceiptPrepared || r.State == assistantmission.ReceiptIssued || r.State == assistantmission.ReceiptUncertain ||
			((r.State == assistantmission.ReceiptSucceeded || r.State == assistantmission.ReceiptRejected) && (r.ResultDelivery == nil || r.ResultDelivery.State != assistantmission.ReceiptSucceeded)) {
			return i
		}
	}
	return -1
}

func hasIssuedAction(m assistantmission.Mission) bool {
	for _, r := range m.Receipts {
		if r.State == assistantmission.ReceiptIssued {
			return true
		}
	}
	return false
}

func (c *assistantMissionCoordinator) nextProposal(ctx context.Context, m *assistantmission.Mission) (assistantmission.Proposal, string, int, bool, error) {
	if m.ProposalFrontier == nil {
		m.ProposalFrontier = map[string]int{}
	}
	summaries, err := c.runs.ListAllArtifactsCtx(ctx, m.AssistantRunID)
	if err != nil {
		return assistantmission.Proposal{}, "", 0, false, err
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].WrittenAt.After(summaries[j].WrittenAt) })
	for _, summary := range summaries {
		if frontier, seen := m.ProposalFrontier[summary.NodeID]; seen && summary.Version <= frontier {
			continue
		}
		m.ProposalFrontier[summary.NodeID] = summary.Version
		artifact, loadErr := c.runs.LoadArtifactCtx(ctx, m.AssistantRunID, summary.NodeID, summary.Version)
		if loadErr != nil {
			return assistantmission.Proposal{}, "", 0, false, loadErr
		}
		raw, exists := artifact.Data["assistant_actions"]
		if !exists {
			continue
		}
		proposals, parseErr := assistantmission.ParseProposals(raw)
		if parseErr != nil {
			return assistantmission.Proposal{}, summary.NodeID, summary.Version, false, parseErr
		}
		allowed := make(map[string]bool, len(m.Policy.Actions))
		for _, action := range m.Policy.Actions {
			allowed[action] = true
		}
		proposal, validateErr := assistantmission.ValidateSingleSupported(proposals, allowed)
		if validateErr != nil {
			return assistantmission.Proposal{}, summary.NodeID, summary.Version, false, validateErr
		}
		if validateErr = validateMissionProposal(proposal, m.TargetRunID); validateErr != nil {
			return assistantmission.Proposal{}, summary.NodeID, summary.Version, false, validateErr
		}
		return proposal, summary.NodeID, summary.Version, true, nil
	}
	return assistantmission.Proposal{}, "", 0, false, nil
}

func validateMissionProposal(p assistantmission.Proposal, targetID string) error {
	runID, _ := p.Args["run_id"].(string)
	if runID != targetID {
		return fmt.Errorf("%s must target the mission run exactly", p.ID)
	}
	switch p.ID {
	case assistantmission.ActionResume:
		if len(p.Args) != 1 {
			return errors.New("run.resume mission args accept run_id only; file_path and force are forbidden")
		}
	case assistantmission.ActionRewind:
		if len(p.Args) != 2 {
			return errors.New("run.rewind mission args require run_id and exactly one of auto or node_id")
		}
		auto, hasAuto := p.Args["auto"].(bool)
		node, hasNode := p.Args["node_id"].(string)
		if hasAuto == hasNode || hasAuto && !auto || hasNode && strings.TrimSpace(node) == "" {
			return errors.New("run.rewind requires exactly auto=true or a non-empty node_id")
		}
	default:
		return fmt.Errorf("unsupported action %q", p.ID)
	}
	return nil
}

func (c *assistantMissionCoordinator) prepareActionReceipt(m *assistantmission.Mission, p assistantmission.Proposal, node string, version int, target *store.Run) assistantmission.ActionReceipt {
	body, _ := json.Marshal(struct {
		Mission string                    `json:"mission"`
		Node    string                    `json:"node"`
		Version int                       `json:"version"`
		Action  assistantmission.Proposal `json:"action"`
	}{m.ID, node, version, p})
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	now := time.Now().UTC()
	receipt := assistantmission.ActionReceipt{ID: deterministicMissionID(m.ID, digest), Action: p.ID, Digest: digest, AssistantRunID: m.AssistantRunID, ArtifactNodeID: node, ArtifactVersion: version, Args: p.Args, State: assistantmission.ReceiptPrepared, CreatedAt: now, UpdatedAt: now}
	if p.ID == assistantmission.ActionResume {
		receipt.ExpectedStatus = target.Status
		if target.Status != store.RunStatusFailedResumable && (target.Status != store.RunStatusPausedOperator || !hasSucceededRewind(*m)) {
			receipt.State, receipt.Error = assistantmission.ReceiptRejected, "mission resume requires failed_resumable or a paused_operator state produced by this mission"
		}
	}
	return receipt
}

func hasSucceededRewind(m assistantmission.Mission) bool {
	for _, r := range m.Receipts {
		if r.Action == assistantmission.ActionRewind && r.State == assistantmission.ReceiptSucceeded {
			return true
		}
	}
	return false
}

func deterministicMissionID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func actionReceiptRecorded(ctx context.Context, runs *runview.Service, runID string, typ store.EventType, receiptID string) (int64, bool, error) {
	var seq int64
	recorded := false
	err := runs.ScanEventsCtx(ctx, runID, func(event *store.Event) bool {
		if event != nil && event.Type == typ {
			if candidate, _ := event.Data["receipt_id"].(string); candidate == receiptID {
				seq = event.Seq
				recorded = true
				return false
			}
		}
		return true
	})
	return seq, recorded, err
}

type createAssistantMissionRequest struct {
	InvocationKey  string   `json:"invocation_key"`
	WatchID        string   `json:"watch_id,omitempty"`
	AssistantRunID string   `json:"assistant_run_id,omitempty"`
	Actions        []string `json:"actions,omitempty"`
	TTLSeconds     int64    `json:"ttl_seconds,omitempty"`
	MaxActions     int      `json:"max_actions,omitempty"`
}

func (s *Server) handleCreateAssistantMission(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) || s.rejectCrossStoreWrite(w, r) {
		return
	}
	if s.assistantMissions == nil || s.assistantWatches == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "assistant mission persistence is unavailable")
		return
	}
	if runview.DetachedEnabled() && s.cfg.Mode != "cloud" {
		s.httpErrorFor(w, r, http.StatusConflict, "assistant missions require in-process local runs; detached mode cannot preserve receipts")
		return
	}
	var req createAssistantMissionRequest
	if !decodeJSONCapped(s, w, r, &req, 32<<10) {
		return
	}
	target, err := s.runs.LoadRunCtx(r.Context(), r.PathValue("id"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "target run not found")
		return
	}
	ident, _ := auth.FromContext(r.Context())
	if ident.IsSynthetic() {
		s.httpErrorFor(w, r, http.StatusForbidden, "assistant missions require an authenticated operator")
		return
	}
	operator := ident.UserID
	if operator == "" {
		operator = target.OwnerID
	}
	if operator == "" {
		operator = "local"
	}
	actions, err := canonicalMissionActions(req.Actions)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	if req.TTLSeconds == 0 {
		req.TTLSeconds = 7200
	}
	if req.TTLSeconds < 60 || req.TTLSeconds > 86400 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "ttl_seconds must be between 60 and 86400")
		return
	}
	if req.MaxActions == 0 {
		req.MaxActions = 6
	}
	if req.MaxActions < 1 || req.MaxActions > 20 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "max_actions must be between 1 and 20")
		return
	}
	if strings.TrimSpace(req.InvocationKey) == "" {
		req.InvocationKey = "goal:" + target.ID
	}
	requestedPolicy := assistantmission.Policy{Actions: actions, TTLSeconds: req.TTLSeconds, MaxActions: req.MaxActions, ContractVersion: assistantmission.ContractVersion}
	scope := assistantmission.Scope{TenantID: target.TenantID, OperatorID: operator, TargetRunID: target.ID}
	if existing, getErr := s.assistantMissions.GetByInvocation(r.Context(), scope, req.InvocationKey); getErr == nil {
		if !existing.Policy.Equal(requestedPolicy) || req.WatchID != "" && req.WatchID != existing.WatchID || req.AssistantRunID != "" && req.AssistantRunID != existing.AssistantRunID {
			s.httpErrorFor(w, r, http.StatusConflict, "invocation key is already bound to a different mission policy")
			return
		}
		// A permanent invocation binding reattaches even after its watch or
		// assistant becomes terminal. It never renews TTL or action budget.
		s.writeJSONFor(w, r, existing)
		return
	} else if !errors.Is(getErr, assistantmission.ErrNotFound) {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "lookup assistant mission: %v", getErr)
		return
	}
	watch, err := s.resolveMissionWatch(r.Context(), target, operator, req.WatchID, req.AssistantRunID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusConflict, "%v", err)
		return
	}
	assistant, err := s.runs.LoadRunCtx(r.Context(), watch.AssistantRunID)
	if err != nil || assistantWatchStopsForStatus(assistant.Status) {
		s.httpErrorFor(w, r, http.StatusConflict, "assistant run is unavailable")
		return
	}
	if _, _, err := s.resolveAssistantChatCapability(r.Context(), assistant); err != nil {
		s.httpErrorFor(w, r, http.StatusConflict, "assistant is not host-event capable: %v", err)
		return
	}
	now := time.Now().UTC()
	projectID := target.ProjectPath
	if projectID == "" {
		projectID = s.CurrentProjectID()
	}
	frontier, err := s.assistantArtifactFrontier(r.Context(), assistant.ID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "capture assistant proposal frontier: %v", err)
		return
	}
	requestedPolicy.ExpiresAt = now.Add(time.Duration(req.TTLSeconds) * time.Second)
	mission := assistantmission.Mission{Version: assistantmission.SchemaVersion, ID: uuid.NewString(), InvocationKey: req.InvocationKey, TenantID: target.TenantID, ProjectID: projectID, OperatorID: operator, TargetRunID: target.ID, WatchID: watch.ID, AssistantRunID: assistant.ID, Policy: requestedPolicy, State: assistantmission.StateActive, Reason: "mission accepted", ProposalFrontier: frontier, CreatedAt: now, UpdatedAt: now}
	persisted, _, err := s.assistantMissions.CreateOrGet(r.Context(), mission)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, assistantmission.ErrConflict) {
			status = http.StatusConflict
		}
		s.httpErrorFor(w, r, status, "create assistant mission: %v", err)
		return
	}
	// The coordinator pointer is swapped on a project switch, so read it
	// under the lock and nudge it through its own wake channel — a `go
	// sweep` here would run detached from the ctx that switch cancels.
	s.stateMu.RLock()
	coordinator := s.assistantMission
	s.stateMu.RUnlock()
	if coordinator != nil {
		coordinator.nudge()
	}
	s.writeJSONFor(w, r, persisted)
}

func (s *Server) resolveMissionWatch(ctx context.Context, target *store.Run, owner, watchID, assistantID string) (runwatch.Watch, error) {
	if s.assistantWatches == nil || s.assistantMissions == nil {
		return runwatch.Watch{}, errors.New("assistant mission persistence is unavailable")
	}
	if watchID != "" {
		watch, err := s.assistantWatches.GetWatch(ctx, watchID)
		if err != nil || watch.State != runwatch.WatchActive || watch.TenantID != target.TenantID || watch.TargetRunID != target.ID || watch.OwnerID != owner || assistantID != "" && watch.AssistantRunID != assistantID {
			return runwatch.Watch{}, errors.New("watch_id is not an active exact watch owned by this operator")
		}
		return watch, nil
	}
	watches, err := s.assistantWatches.ListActiveByTarget(ctx, target.TenantID, target.ID)
	if err != nil {
		return runwatch.Watch{}, err
	}
	var matches []runwatch.Watch
	for _, watch := range watches {
		if watch.OwnerID == owner && (assistantID == "" || watch.AssistantRunID == assistantID) {
			matches = append(matches, watch)
		}
	}
	if len(matches) != 1 {
		return runwatch.Watch{}, fmt.Errorf("mission requires exactly one active exact watch; found %d", len(matches))
	}
	return matches[0], nil
}

func canonicalMissionActions(actions []string) ([]string, error) {
	if len(actions) == 0 {
		actions = []string{assistantmission.ActionResume, assistantmission.ActionRewind}
	}
	seen := map[string]bool{}
	for _, action := range actions {
		if action != assistantmission.ActionResume && action != assistantmission.ActionRewind || seen[action] {
			return nil, fmt.Errorf("actions may contain run.resume and run.rewind once each")
		}
		seen[action] = true
	}
	sort.Strings(actions)
	return actions, nil
}

func (s *Server) assistantArtifactFrontier(ctx context.Context, runID string) (map[string]int, error) {
	summaries, err := s.runs.ListAllArtifactsCtx(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, summary := range summaries {
		out[summary.NodeID] = summary.Version
	}
	return out, nil
}

func missionScope(r *http.Request, target *store.Run) assistantmission.Scope {
	ident, _ := auth.FromContext(r.Context())
	operator := ident.UserID
	if operator == "" {
		operator = target.OwnerID
	}
	if operator == "" {
		operator = "local"
	}
	return assistantmission.Scope{TenantID: target.TenantID, OperatorID: operator, TargetRunID: target.ID}
}

func (s *Server) handleListAssistantMissions(w http.ResponseWriter, r *http.Request) {
	if s.assistantMissions == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "assistant mission persistence is unavailable")
		return
	}
	target, err := s.runs.LoadRunCtx(r.Context(), r.PathValue("id"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "target run not found")
		return
	}
	missions, err := s.assistantMissions.List(r.Context(), missionScope(r, target), 100)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list assistant missions: %v", err)
		return
	}
	if missions == nil {
		missions = []assistantmission.Mission{}
	}
	s.writeJSONFor(w, r, missions)
}

func (s *Server) handleGetAssistantMission(w http.ResponseWriter, r *http.Request) {
	if s.assistantMissions == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "assistant mission persistence is unavailable")
		return
	}
	target, err := s.runs.LoadRunCtx(r.Context(), r.PathValue("id"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "target run not found")
		return
	}
	mission, err := s.assistantMissions.Get(r.Context(), missionScope(r, target), r.PathValue("missionID"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "assistant mission not found")
		return
	}
	s.writeJSONFor(w, r, mission)
}

func (s *Server) handleStopAssistantMission(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) || s.rejectCrossStoreWrite(w, r) {
		return
	}
	if s.assistantMissions == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "assistant mission persistence is unavailable")
		return
	}
	target, err := s.runs.LoadRunCtx(r.Context(), r.PathValue("id"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "target run not found")
		return
	}
	mission, err := s.assistantMissions.RequestStop(r.Context(), missionScope(r, target), r.PathValue("missionID"), "stopped by operator", time.Now().UTC())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "assistant mission not found")
		return
	}
	s.writeJSONFor(w, r, mission)
}
