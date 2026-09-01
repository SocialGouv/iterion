package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	// Atomic like every other run-scoped JSON persister in the package
	// (store_atomic.go): a SIGKILL between truncate and write left a
	// partial file that loadRouteDecisions could never decode again, so
	// ClaimRouteDecision would error on every later offer and the run
	// could not be routed at all — with its whole decision audit gone.
	// Being fsync+rename is also what lets ListRoutableRuns read these
	// files without the store mutex: a reader sees old-or-new, never torn.
	if err := writeFileAtomic(s.routeDecisionsPath(runID), data, filePerm); err != nil {
		return fmt.Errorf("store: write route decisions %s: %w", runID, err)
	}
	return nil
}

// ClaimRouteDecision — see RouteDecisionStore.
func (s *FilesystemRunStore) ClaimRouteDecision(_ context.Context, d RouteDecision, staleBefore time.Time) (bool, *RouteDecision, error) {
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
		reclaimable := (cur.State == RouteDecisionClaimed && cur.ClaimedAt.Before(staleBefore) && cur.Attempts < MaxRouteDecisionAttempts) ||
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
	if state != RouteDecisionSucceeded && state != RouteDecisionFailed && state != RouteDecisionRequiresAction {
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
	type cand struct {
		id string
		at time.Time
	}
	var cands []cand
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, err := s.loadRunRaw(e.Name())
		if err != nil || r.RoutingPolicy == nil || r.UpdatedAt.Before(since) {
			continue
		}
		switch r.Status {
		case RunStatusFinished, RunStatusFailed, RunStatusFailedResumable:
		default:
			continue
		}
		if settled, err := s.episodeSettled(r.ID, r.OutcomeSeq); err != nil || settled {
			continue
		}
		cands = append(cands, cand{id: r.ID, at: r.UpdatedAt})
	}
	// Oldest first, and only THEN the limit — truncating in directory
	// (lexical) order would make the oldest sleeping terminal, the very
	// run this net exists for, structurally unreachable behind a page of
	// newer ones (the mongo twin documents the same trap).
	sort.Slice(cands, func(i, j int) bool { return cands[i].at.Before(cands[j].at) })
	if len(cands) > limit {
		cands = cands[:limit]
	}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.id)
	}
	return out, nil
}

// episodeSettled is the FS half of the sweep anti-join: the run's
// CURRENT episode already has a decision no offer can act on again —
// succeeded, requires_action, or failed at the attempt cap. Must mirror
// the ClaimRouteDecision reclaim predicate exactly: a state that is not
// re-claimable but reads unsettled clogs the sweep batch forever.
func (s *FilesystemRunStore) episodeSettled(runID string, outcomeSeq int64) (bool, error) {
	ds, err := s.loadRouteDecisions(runID)
	if err != nil {
		return false, err
	}
	for i := range ds {
		if ds[i].OutcomeSeq != outcomeSeq {
			continue
		}
		if ds[i].State == RouteDecisionSucceeded || ds[i].State == RouteDecisionRequiresAction ||
			(ds[i].State == RouteDecisionFailed && ds[i].Attempts >= MaxRouteDecisionAttempts) {
			return true, nil
		}
	}
	return false, nil
}

// EnsureRouterWatermark — see RouteDecisionStore. First-writer-wins on
// a store-root file, so a restart (or a second replica sharing the
// store) reads the instant the switch FIRST went live, not its own
// boot time.
func (s *FilesystemRunStore) EnsureRouterWatermark(_ context.Context) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "router_watermark.json")
	if data, err := os.ReadFile(path); err == nil {
		var wm struct {
			ActivatedAt time.Time `json:"activated_at"`
		}
		if err := json.Unmarshal(data, &wm); err != nil {
			return time.Time{}, fmt.Errorf("store: parse router watermark: %w", err)
		}
		return wm.ActivatedAt, nil
	} else if !os.IsNotExist(err) {
		return time.Time{}, fmt.Errorf("store: read router watermark: %w", err)
	}
	now := time.Now().UTC()
	data, err := json.MarshalIndent(struct {
		ActivatedAt time.Time `json:"activated_at"`
	}{now}, "", "  ")
	if err != nil {
		return time.Time{}, fmt.Errorf("store: marshal router watermark: %w", err)
	}
	// Exclusive-create, not a plain write: the store mutex only
	// serialises THIS process, and the doc's own claim ("a second
	// replica sharing the store reads the instant the switch FIRST went
	// live") is exactly what a read-ENOENT-then-write pair cannot keep —
	// both replicas would write and the later instant would win, moving
	// the watermark forward and skipping the terminals in between. On a
	// lost race the winner's file is read back below.
	if err := WriteFileAtomicNew(path, data, filePerm); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return time.Time{}, fmt.Errorf("store: write router watermark: %w", err)
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return time.Time{}, fmt.Errorf("store: read router watermark after a lost create race: %w", rerr)
		}
		var wm struct {
			ActivatedAt time.Time `json:"activated_at"`
		}
		if uerr := json.Unmarshal(raw, &wm); uerr != nil {
			return time.Time{}, fmt.Errorf("store: parse router watermark: %w", uerr)
		}
		return wm.ActivatedAt, nil
	}
	return now, nil
}
