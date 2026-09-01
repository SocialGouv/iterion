package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Filesystem implementation of RouteDecisionStore: one JSON file per
// run under the run's directory, guarded by the store mutex (same
// serialisation the run.json writers rely on). Kept deliberately
// simple — the router runs on the cloud store in production; this
// implementation exists so the decision path is testable without a
// mongod and so a local operator can audit decisions on disk.

func (s *FilesystemRunStore) routeDecisionsPath(runID string) string {
	return filepath.Join(filepath.Dir(s.runJSONPath(runID)), "route_decisions.json")
}

func (s *FilesystemRunStore) loadRouteDecisions(runID string) ([]RouteDecision, error) {
	data, err := os.ReadFile(s.routeDecisionsPath(runID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read route decisions %s: %w", runID, err)
	}
	var out []RouteDecision
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("store: parse route decisions %s: %w", runID, err)
	}
	return out, nil
}

func (s *FilesystemRunStore) writeRouteDecisions(runID string, ds []RouteDecision) error {
	data, err := json.MarshalIndent(ds, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal route decisions %s: %w", runID, err)
	}
	if err := os.WriteFile(s.routeDecisionsPath(runID), data, 0o644); err != nil {
		return fmt.Errorf("store: write route decisions %s: %w", runID, err)
	}
	return nil
}

// ClaimRouteDecision — see RouteDecisionStore.
func (s *FilesystemRunStore) ClaimRouteDecision(_ context.Context, d RouteDecision) (bool, *RouteDecision, error) {
	if d.RunID == "" {
		return false, nil, fmt.Errorf("store: claim route decision without run_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadRunRaw(d.RunID); err != nil {
		return false, nil, err
	}
	ds, err := s.loadRouteDecisions(d.RunID)
	if err != nil {
		return false, nil, err
	}
	now := time.Now().UTC()
	for i := range ds {
		if ds[i].OutcomeSeq != d.OutcomeSeq {
			continue
		}
		cur := ds[i]
		reclaimable := (cur.State == RouteDecisionClaimed && now.Sub(cur.ClaimedAt) > RouteClaimLease) ||
			(cur.State == RouteDecisionFailed && cur.Attempts < MaxRouteDecisionAttempts)
		if !reclaimable {
			existing := cur
			return false, &existing, nil
		}
		ds[i].State = RouteDecisionClaimed
		ds[i].Decision = d.Decision
		ds[i].Reason = d.Reason
		ds[i].PolicyHash = d.PolicyHash
		ds[i].ClaimedAt = now
		ds[i].Attempts = cur.Attempts + 1
		if err := s.writeRouteDecisions(d.RunID, ds); err != nil {
			return false, nil, err
		}
		return true, nil, nil
	}
	d.ID = fmt.Sprintf("%s:%d", d.RunID, d.OutcomeSeq)
	d.State = RouteDecisionClaimed
	d.ClaimedAt = now
	d.Attempts = 1
	ds = append(ds, d)
	if err := s.writeRouteDecisions(d.RunID, ds); err != nil {
		return false, nil, err
	}
	return true, nil, nil
}

// FinishRouteDecision — see RouteDecisionStore.
func (s *FilesystemRunStore) FinishRouteDecision(_ context.Context, runID string, outcomeSeq int64, state, actionErr string) error {
	if state != RouteDecisionSucceeded && state != RouteDecisionFailed {
		return fmt.Errorf("store: finish route decision: invalid state %q", state)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ds, err := s.loadRouteDecisions(runID)
	if err != nil {
		return err
	}
	for i := range ds {
		if ds[i].OutcomeSeq == outcomeSeq && ds[i].State == RouteDecisionClaimed {
			now := time.Now().UTC()
			ds[i].State = state
			ds[i].Error = actionErr
			ds[i].FinishedAt = &now
			return s.writeRouteDecisions(runID, ds)
		}
	}
	return fmt.Errorf("store: finish route decision %s:%d: no claimed row", runID, outcomeSeq)
}

// ListRouteDecisions — see RouteDecisionStore.
func (s *FilesystemRunStore) ListRouteDecisions(_ context.Context, runID string) ([]RouteDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ds, err := s.loadRouteDecisions(runID)
	if err != nil {
		return nil, err
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].OutcomeSeq > ds[j].OutcomeSeq })
	return ds, nil
}

// ListRoutableRuns — filesystem twin of the sweep query. Scans the run
// directory (bounded by limit); acceptable for the local store's scale
// and for tests.
func (s *FilesystemRunStore) ListRoutableRuns(_ context.Context, since time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 200
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(s.root, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list routable runs: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || len(out) >= limit {
			continue
		}
		r, err := s.loadRunRaw(e.Name())
		if err != nil || r.RoutingPolicy == nil || r.UpdatedAt.Before(since) {
			continue
		}
		switch r.Status {
		case RunStatusFinished, RunStatusFailed, RunStatusFailedResumable:
			out = append(out, r.ID)
		}
	}
	return out, nil
}
