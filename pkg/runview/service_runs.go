package runview

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resolveBundleName returns the bot/bundle label for display in the
// run list. Prefers the persisted manifest name; falls back to
// basename(bundlePath) stripped of `.botz` so legacy runs (persisted
// before BundleName existed) still surface a readable label. Returns
// "" for plain .bot runs with no bundle at all.
func resolveBundleName(bundleName, bundlePath string) string {
	if bundleName != "" {
		return bundleName
	}
	if bundlePath == "" {
		return ""
	}
	base := filepath.Base(strings.TrimRight(bundlePath, "/"))
	return strings.TrimSuffix(base, ".botz")
}

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

// DeleteRunCtx permanently removes a run and all of its data. It LoadRuns
// first so a run outside the caller's tenant scope surfaces as not-found
// (a tenant can only delete its own runs); the actual delete is then
// tenant-scoped by the store as well. Idempotent at the store layer.
func (s *Service) DeleteRunCtx(ctx context.Context, runID string) error {
	if _, err := s.store.LoadRun(ctx, runID); err != nil {
		return err
	}
	return s.store.DeleteRun(ctx, runID)
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

// ListRunRecordsCtx returns the persisted runs matching f. It is the
// tenant-aware, record-level counterpart to ListCtx: the caller's context is
// propagated to every store operation, including the event scan used by the
// Node filter. Results are sorted by CreatedAt descending and truncated by
// Limit after sorting.
//
// A run that cannot be loaded is skipped so one corrupt run document does not
// break the whole listing. Returning the records directly lets server-side
// projections consume the same filtered snapshot without listing summaries
// and loading every run a second time.
func (s *Service) ListRunRecordsCtx(ctx context.Context, f ListFilter) ([]*store.Run, error) {
	ids, err := s.store.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Run, 0, len(ids))
	for _, id := range ids {
		r, err := s.store.LoadRun(ctx, id)
		if err != nil {
			// A single corrupt run.json shouldn't break the whole listing.
			s.logSkippedRun(id, err)
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
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// skipWarnRelogAfter bounds how often the same unreadable run is
// re-reported. A memoise-forever Warn would silence a genuinely corrupt
// document forever after ONE transient blip (mongo server-selection,
// EMFILE/EACCES) marked its id; re-logging on an interval keeps the
// diagnostic alive without letting a UI poll loop flood the log.
const skipWarnRelogAfter = 10 * time.Minute

// logSkippedRun reports a run id the listing had to skip, deduplicated
// per (id, category). ListRuns keeps returning ids whose document is
// gone (manual deletion, partial purge, migration), so without dedup
// every UI poll re-logs the same line: several WARN lines per second,
// indefinitely, drowning the instance log.
//
// A missing document (ErrRunNotFound) is a stale index entry, not a
// corrupt run: Debug, once — it cannot heal without the id leaving the
// listing anyway. Only an unreadable document rates a Warn, at most
// once per skipWarnRelogAfter.
func (s *Service) logSkippedRun(id string, err error) {
	if s.logger == nil {
		return
	}
	if errors.Is(err, store.ErrRunNotFound) {
		if _, dup := s.skipRunLogged.LoadOrStore("gone:"+id, struct{}{}); !dup {
			s.logger.Debug("runview: skip run %s (stale index entry, logged once): %v", id, err)
		}
		return
	}
	// A context error means the CALLER went away (or a deadline blew),
	// not that this document is unreadable — and the listing loop keeps
	// iterating, so ONE cancelled request would mark every remaining id
	// "corrupt" (the mongo store honours the caller's ctx; cloud mode is
	// exactly where the log flood was observed). Report it without
	// memoising.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		s.logger.Debug("runview: skip run %s (store call interrupted): %v", id, err)
		return
	}
	key := "corrupt:" + id
	if last, ok := s.skipRunLogged.Load(key); ok {
		if ts, ok := last.(time.Time); ok && time.Since(ts) < skipWarnRelogAfter {
			return
		}
	}
	s.skipRunLogged.Store(key, time.Now())
	s.logger.Warn("runview: skip run %s: %v", id, err)
}

// ListCtx is the tenant-aware variant of List: propagates the caller's
// ctx so mongo's tenant_id filter (stamped by requireAuth via
// store.WithIdentity) applies to both the ListRuns and per-id LoadRun
// calls. A cross-tenant caller sees an empty list instead of leaking
// other tenants' run summaries.
func (s *Service) ListCtx(ctx context.Context, f ListFilter) ([]RunSummary, error) {
	runs, err := s.ListRunRecordsCtx(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]RunSummary, 0, len(runs))
	for _, r := range runs {
		out = append(out, s.summarize(r))
	}
	return out, nil
}

// ListChildren returns the summaries of every run whose ParentRunID is
// parentRunID — a run's shard/child subtree (T4b, refs #125), ordered by
// created_at ascending (the store guarantees the ordering). Propagates
// the caller's ctx so the mongo tenant filter applies. A run that fails
// to load is skipped rather than failing the whole listing.
func (s *Service) ListChildren(ctx context.Context, parentRunID string) ([]RunSummary, error) {
	ids, err := s.store.ListChildRuns(ctx, parentRunID)
	if err != nil {
		return nil, err
	}
	out := make([]RunSummary, 0, len(ids))
	for _, id := range ids {
		r, err := s.store.LoadRun(ctx, id)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("runview: skip child run %s: %v", id, err)
			}
			continue
		}
		out = append(out, s.summarize(r))
	}
	return out, nil
}

// summarize projects a persisted Run into the lightweight RunSummary
// shape the run list + children endpoint return. Shared so every
// listing surface carries the same derived fields (source-kind
// classification, shard tuple, active flag).
func (s *Service) summarize(r *store.Run) RunSummary {
	return summarizeRun(r, s.manager.Active(r.ID))
}

// summarizeRun is the manager-free core of summarize: it projects a
// persisted Run into a RunSummary given a precomputed active flag. Shared
// by the Service (which knows liveness via its manager) and the cross-store
// read path (BuildChildrenFromStore, where runs are owned by another daemon
// and thus never active in this process).
func summarizeRun(r *store.Run, active bool) RunSummary {
	return RunSummary{
		ID:                r.ID,
		Name:              r.Name,
		WorkflowName:      r.WorkflowName,
		BundleName:        resolveBundleName(r.BundleName, r.BundlePath),
		BundleDisplayName: r.BundleDisplayName,
		SourceKind:        deriveSourceKind(r),
		Status:            r.Status,
		FilePath:          r.FilePath,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
		FinishedAt:        r.FinishedAt,
		Error:             r.Error,
		FailureCode:       r.FailureCode,
		Active:            active,
		FinalCommit:       r.FinalCommit,
		FinalBranch:       r.FinalBranch,
		FinalBranchError:  r.FinalBranchError,
		MergedInto:        r.MergedInto,
		MergedCommit:      r.MergedCommit,
		MergeStrategy:     r.MergeStrategy,
		MergeStatus:       r.MergeStatus,
		AutoMerge:         r.AutoMerge,
		WorkDir:           r.WorkDir,
		RepoRoot:          r.RepoRoot,
		ProjectPath:       r.ProjectPath,
		ParentRunID:       r.ParentRunID,
		ParentNodeID:      r.ParentNodeID,
		ShardIndex:        r.ShardIndex,
		ShardCount:        r.ShardCount,
		ShardLabel:        r.ShardLabel,
		RetryAfter:        retryAfterOf(r),
		RetryAttempts:     retryAttemptsOf(r),
	}
}

// retryAfterOf / retryAttemptsOf project the run's retry bookkeeping,
// tolerating every "not armed" shape (no state, no instant) so a caller
// never has to nil-walk two levels.
func retryAfterOf(r *store.Run) *time.Time {
	if r == nil || r.RetryState == nil {
		return nil
	}
	return r.RetryState.RetryAfter
}

func retryAttemptsOf(r *store.Run) int {
	if r == nil || r.RetryState == nil {
		return 0
	}
	return r.RetryState.Attempts
}

// BuildChildrenFromStore returns the shard/child subtree of a run read
// directly off an arbitrary store — the cross-store (?store=) counterpart
// to Service.ListChildren. Runs owned by another daemon are never active in
// this process, so their summaries carry Active=false. A child that fails to
// load is skipped rather than failing the whole listing.
func BuildChildrenFromStore(ctx context.Context, s store.RunStore, parentRunID string) ([]RunSummary, error) {
	ids, err := s.ListChildRuns(ctx, parentRunID)
	if err != nil {
		return nil, err
	}
	out := make([]RunSummary, 0, len(ids))
	for _, id := range ids {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, summarizeRun(r, false))
	}
	return out, nil
}

// deriveSourceKind classifies how a run was triggered, for list grouping /
// filtering. Derived from the run's source/owner; not persisted. Trigger
// sources (dispatcher, webhook) take precedence over the structural ones
// (fork, shard); a plain human launch (CLI / studio / cloud API) is "manual".
func deriveSourceKind(r *store.Run) string {
	switch {
	case r.Source != nil && r.Source.Kind != "":
		return r.Source.Kind // "dispatcher"
	case strings.HasPrefix(r.OwnerID, "webhook:"):
		return "webhook"
	case r.ForkedFrom != "":
		return "fork"
	case r.ParentRunID != "":
		return "shard"
	default:
		return "manual"
	}
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
	if f.Repo != "" && r.ProjectPath != f.Repo {
		return false
	}
	if f.Bundle != "" && !strings.EqualFold(resolveBundleName(r.BundleName, r.BundlePath), f.Bundle) {
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
	return s.SnapshotCtx(context.Background(), runID)
}

// SnapshotCtx is the tenant-aware variant of Snapshot.
func (s *Service) SnapshotCtx(ctx context.Context, runID string) (*RunSnapshot, error) {
	// Reconcile a still-"pending" run that was merged out-of-band (git CLI
	// / CI) so the run-view stops offering "Squash and merge" for a branch
	// already on the target. Best-effort, and only for "pending": "skipped"
	// is a deliberate branch-only outcome, and failed/conflicted have their
	// own UX.
	if r, err := s.store.LoadRun(ctx, runID); err == nil && r.MergeStatus == store.MergeStatusPending {
		_, _ = s.reconcileOutOfBandMerge(ctx, r, mergeRepoRoot(r), "")
	}
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
//
// The canonical value lives in runstream (the streaming seam batches
// replay pages at the same size); this alias keeps the historical
// runview.MaxEventsPerPage name working for the HTTP/WS layer.
func MaxEventsPerPage() int { return runstream.MaxEventsPerPage() }

// SetMaxEventsPerPageForTest lowers the page cap for the duration of a test
// and returns a restore func. One knob, read through one accessor, so a test
// cannot read one value while the streaming seam paginates on another.
func SetMaxEventsPerPageForTest(n int) func() { return runstream.SetMaxEventsPerPageForTest(n) }

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
	return s.store.LoadEventsRange(context.Background(), runID, from, to, MaxEventsPerPage())
}

// LoadEventsCtx is the tenant-aware variant of LoadEvents.
func (s *Service) LoadEventsCtx(ctx context.Context, runID string, from, to int64) ([]*store.Event, error) {
	return s.store.LoadEventsRange(ctx, runID, from, to, MaxEventsPerPage())
}
