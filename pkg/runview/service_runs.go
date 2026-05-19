package runview

import (
	"context"
	"sort"

	"github.com/SocialGouv/iterion/pkg/store"
)

// LoadRun returns the persisted Run metadata for runID.
//
// Uses context.Background — does NOT carry caller identity. Cloud
// callers that need tenant-scoped lookup (e.g. authorize a WS
// subscription before upgrading) must use LoadRunCtx.
func (s *Service) LoadRun(runID string) (*store.Run, error) {
	return s.store.LoadRun(context.Background(), runID)
}

// LoadRunCtx is the tenant-aware variant of LoadRun: it propagates the
// caller's ctx so the mongo store applies the tenant_id filter
// stamped by requireAuth (store.WithIdentity). A cross-tenant ID
// resolves to not-found instead of leaking the run document.
func (s *Service) LoadRunCtx(ctx context.Context, runID string) (*store.Run, error) {
	return s.store.LoadRun(ctx, runID)
}

// RenameRunCtx replaces a run's friendly Name. The run id stays
// stable; only the human-readable label changes. The store is the
// source of truth — clients keep their per-runId state and the next
// snapshot push surfaces the new name.
func (s *Service) RenameRunCtx(ctx context.Context, runID, name string) (*store.Run, error) {
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if r.Name == name {
		return r, nil
	}
	r.Name = name
	if err := s.store.SaveRun(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// List returns every run in the store filtered by f. The result is
// sorted by CreatedAt descending (newest first); Limit truncates after
// sort.
//
// Uses context.Background — does NOT carry caller identity. Cloud
// HTTP handlers must call ListCtx so the mongo tenant_id filter
// applies; CLI / system paths (single-tenant) can keep using this.
func (s *Service) List(f ListFilter) ([]RunSummary, error) {
	return s.ListCtx(context.Background(), f)
}

// ListCtx is the tenant-aware variant of List: propagates the caller's
// ctx so mongo's tenant_id filter (stamped by requireAuth via
// store.WithIdentity) applies to both the ListRuns and per-id LoadRun
// calls. A cross-tenant caller sees an empty list instead of leaking
// other tenants' run summaries.
func (s *Service) ListCtx(ctx context.Context, f ListFilter) ([]RunSummary, error) {
	ids, err := s.store.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RunSummary, 0, len(ids))
	for _, id := range ids {
		r, err := s.store.LoadRun(ctx, id)
		if err != nil {
			// A single corrupt run.json shouldn't break the whole listing.
			if s.logger != nil {
				s.logger.Warn("runview: skip run %s: %v", id, err)
			}
			continue
		}
		if !matchesFilter(r, f) {
			continue
		}
		// Node filter is more expensive (loads events.jsonl for each
		// candidate). Run it last so cheaper rejection criteria above
		// short-circuit first.
		if f.Node != "" && !runTouchedNode(ctx, s.store, r.ID, f.Node) {
			continue
		}
		out = append(out, RunSummary{
			ID:               r.ID,
			Name:             r.Name,
			WorkflowName:     r.WorkflowName,
			Status:           r.Status,
			FilePath:         r.FilePath,
			CreatedAt:        r.CreatedAt,
			UpdatedAt:        r.UpdatedAt,
			FinishedAt:       r.FinishedAt,
			Error:            r.Error,
			Active:           s.manager.Active(r.ID),
			FinalCommit:      r.FinalCommit,
			FinalBranch:      r.FinalBranch,
			FinalBranchError: r.FinalBranchError,
			MergedInto:       r.MergedInto,
			MergedCommit:     r.MergedCommit,
			MergeStrategy:    r.MergeStrategy,
			MergeStatus:      r.MergeStatus,
			AutoMerge:        r.AutoMerge,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// runTouchedNode returns true if the run's events.jsonl contains at
// least one node_started event for nodeID. Short-circuits on first
// match. Errors loading events are treated as "didn't touch" — a
// run we can't read shouldn't surface as a hit.
//
// Streams events through ScanEvents instead of materialising the full
// slice via LoadEvents — long-running runs can have hundreds of MB of
// events.jsonl, and a list filter pass that calls this for every
// candidate run would otherwise be O(N*size) memory.
func runTouchedNode(ctx context.Context, s store.RunStore, runID, nodeID string) bool {
	hit := false
	_ = s.ScanEvents(ctx, runID, func(e *store.Event) bool {
		if e.Type == store.EventNodeStarted && e.NodeID == nodeID {
			hit = true
			return false
		}
		return true
	})
	return hit
}

func matchesFilter(r *store.Run, f ListFilter) bool {
	if f.Status != "" && r.Status != f.Status {
		return false
	}
	if f.Workflow != "" && r.WorkflowName != f.Workflow {
		return false
	}
	if !f.Since.IsZero() && r.UpdatedAt.Before(f.Since) {
		return false
	}
	return true
}

// Snapshot returns the structured RunSnapshot for runID by folding the
// persisted events through the canonical reducer.
//
// Uses context.Background — does NOT carry caller identity. Use
// SnapshotCtx from cloud HTTP/WS handlers so the mongo tenant filter
// applies.
func (s *Service) Snapshot(runID string) (*RunSnapshot, error) {
	return BuildSnapshot(context.Background(), s.store, runID)
}

// SnapshotCtx is the tenant-aware variant of Snapshot.
func (s *Service) SnapshotCtx(ctx context.Context, runID string) (*RunSnapshot, error) {
	return BuildSnapshot(ctx, s.store, runID)
}

// MaxEventsPerPage caps the number of events any single LoadEvents
// response materialises. The original 5000 was tuned for a world where
// tool I/O bodies (multi-MB Bash stdout, LLM thinking blocks) were
// inlined into events.jsonl, so a single page could easily exceed
// 100MB of allocation. The sidecar-blob migration moved those bodies
// out (preview ≤4KB stays inline; the rest lives in
// runs/<id>/tools/<tool_use_id>/{input,output}), bounding per-event
// size to a few KB regardless of payload size.
//
// 25000 keeps the worst-case per-page allocation in the low tens of
// MB on typical events while letting most full runs replay in a
// single round-trip (the WS subscriber + the /events HTTP endpoint
// both paginate, so this is a per-page knob, not a hard ceiling).
// Callers paginate by passing the next page's `from` as
// previous_last.Seq+1; len(out) == cap means "more available".
const MaxEventsPerPage = 25000

// LoadEvents returns events in [from, to] (inclusive on from, exclusive
// on to), capped at MaxEventsPerPage. Pass to=0 for "no upper bound".
// Used by the scrubber to lazy-load segments of a long run.
//
// Streams via store.LoadEventsRange so we never materialise more than
// the page-cap worth of events at once; callers paginate.
//
// Uses context.Background — does NOT carry caller identity. Use
// LoadEventsCtx from cloud HTTP/WS handlers.
func (s *Service) LoadEvents(runID string, from, to int64) ([]*store.Event, error) {
	return s.store.LoadEventsRange(context.Background(), runID, from, to, MaxEventsPerPage)
}

// LoadEventsCtx is the tenant-aware variant of LoadEvents.
func (s *Service) LoadEventsCtx(ctx context.Context, runID string, from, to int64) ([]*store.Event, error) {
	return s.store.LoadEventsRange(ctx, runID, from, to, MaxEventsPerPage)
}
