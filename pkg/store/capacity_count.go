package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// AliveCapacityLister lists every run the run-admission ledger counts as a
// holder of some credential or capacity subject at the listing's instant:
// alive or in its redelivery transit (Run.HoldsCapacity) and not deleted,
// model-idle and admission-pending runs included — exactly the set
// Run.SpendsCredential reads. The listing decides the holders; the
// per-subject readings (CountCapacityHolders, the gateway gauge, the gateway
// status route) group it in Go by stamp alone (Run.StampsCapacity), so none
// reads a clock of its own and none can answer differently from the ledger's
// count.
//
// The listing is tenant-blind by contract, whatever ctx carries: a shared
// gateway and a platform key serve every tenant on one count, as
// CountAliveRunsWithCredFingerprint does. An unreadable record is an error,
// never a run left out: a reading that silently drops a holder under-counts
// the subject it holds.
//
// Each run is projected to the fields the readings need: ID, TenantID,
// BotID, Status, ContinuationState, ContinuationUntil, ParentRunID,
// ParentNodeID, CredFingerprints, CapacitySubjects, CapacityStampedFor and
// ContentBoundary.KeyID (ProjectForCapacity).
type AliveCapacityLister interface {
	ListAliveRunsForCapacity(ctx context.Context) ([]Run, error)
}

// AsAliveCapacityLister returns s as AliveCapacityLister when the backend
// hosts the listing, or nil otherwise. Callers MUST nil-check.
func AsAliveCapacityLister(s RunStore) AliveCapacityLister {
	return capability[AliveCapacityLister](s)
}

// ProjectForCapacity keeps the fields of r that AliveCapacityLister
// promises, and nothing else; both twins answer through it, so a field the
// readings start to need is added here once. ContentBoundary is kept only
// when it names a key id: the readings use it as the hint that a launch
// sealed a gateway key, never as the host the run dials.
func ProjectForCapacity(r *Run) Run {
	out := Run{
		ID:                 r.ID,
		TenantID:           r.TenantID,
		BotID:              r.BotID,
		Status:             r.Status,
		ContinuationState:  r.ContinuationState,
		ContinuationUntil:  cloneTimePtr(r.ContinuationUntil),
		ParentRunID:        r.ParentRunID,
		ParentNodeID:       r.ParentNodeID,
		CredFingerprints:   slices.Clone(r.CredFingerprints),
		CapacitySubjects:   slices.Clone(r.CapacitySubjects),
		CapacityStampedFor: r.CapacityStampedFor,
	}
	if r.ContentBoundary != nil && r.ContentBoundary.KeyID != "" {
		out.ContentBoundary = &RunContentBoundary{KeyID: r.ContentBoundary.KeyID}
	}
	return out
}

// ListAliveRunsForCapacity is the filesystem twin (see AliveCapacityLister):
// the strict scan, so a run directory that exists and cannot be read is an
// error rather than a holder skipped. Each run is projected from the one read
// the scan made of it. The filesystem store keeps no tenant filter, so the
// listing spans every tenant whatever ctx carries.
func (s *FilesystemRunStore) ListAliveRunsForCapacity(ctx context.Context) ([]Run, error) {
	read := map[string]Run{}
	now := time.Now()
	ids, err := s.filterRunsSortedChecked(ctx, func(r *Run) bool {
		if !r.HoldsCapacity(now) {
			return false
		}
		read[r.ID] = ProjectForCapacity(r)
		return true
	}, true)
	if err != nil {
		return nil, fmt.Errorf("store: alive runs for capacity: %w", err)
	}
	out := make([]Run, 0, len(ids))
	for _, id := range ids {
		out = append(out, read[id])
	}
	return out, nil
}

// AliveSubbotTreeRoots maps the id of every run in runs to the root of its
// alive subbot tree: its topmost ancestor reached through subbot links alone,
// or itself. A subbot child runs in its parent's runner, under its parent's
// message (ADR-167 §5: a tree is one place), so the readings count a tree,
// not each of its runs.
//
// The climb follows ParentRunID only while the current run is a subbot child
// (ParentNodeID set) and its parent is in runs — alive. A fork, a shard or an
// API launch carries a ParentRunID without a ParentNodeID: it runs on its own
// message and is a tree of its own. A child whose parent has ended is its own
// root. A cycle, which no writer produces, folds into one tree rooted at its
// smallest id, so the answer never depends on the order of runs.
func AliveSubbotTreeRoots(runs []Run) map[string]string {
	byID := make(map[string]*Run, len(runs))
	for i := range runs {
		byID[runs[i].ID] = &runs[i]
	}
	roots := make(map[string]string, len(runs))
	for i := range runs {
		cur := &runs[i]
		path := []string{cur.ID}
		root := ""
		for root == "" {
			parent, alive := byID[cur.ParentRunID]
			if cur.ParentNodeID == "" || cur.ParentRunID == "" || !alive {
				root = cur.ID
				break
			}
			if at := slices.Index(path, parent.ID); at >= 0 {
				root = slices.Min(path[at:])
				break
			}
			path = append(path, parent.ID)
			cur = parent
		}
		roots[runs[i].ID] = root
	}
	return roots
}

// CapacityCount is one subject's holders: Runs counts the alive runs that
// spend it — the ledger's count, CountAliveRunsWithCredFingerprint's answer —
// and Trees the alive subbot trees those runs belong to (AliveSubbotTreeRoots).
type CapacityCount struct {
	Runs  int
	Trees int
}

// CountCapacityHolders is the per-subject count over one alive listing
// (AliveCapacityLister, whose runs are the holders at its instant): for every
// value with the prefix that some run's stamp names (CapacitySubjects or
// CredFingerprints), the runs stamped with it (Run.StampsCapacity, each run
// once) and the distinct subbot trees among them. The answer does not depend
// on the order of runs. Prefix GatewayCapacitySubjectPrefix reads the gateway
// subjects; an empty prefix reads every stamped value.
func CountCapacityHolders(runs []Run, prefix string) map[string]CapacityCount {
	roots := AliveSubbotTreeRoots(runs)
	holders := map[string]map[string]bool{}
	trees := map[string]map[string]bool{}
	for i := range runs {
		r := &runs[i]
		for _, subject := range slices.Concat(r.CapacitySubjects, r.CredFingerprints) {
			if !strings.HasPrefix(subject, prefix) || !r.StampsCapacity(subject) {
				continue
			}
			if holders[subject] == nil {
				holders[subject], trees[subject] = map[string]bool{}, map[string]bool{}
			}
			holders[subject][r.ID] = true
			trees[subject][roots[r.ID]] = true
		}
	}
	out := make(map[string]CapacityCount, len(holders))
	for subject, ids := range holders {
		out[subject] = CapacityCount{Runs: len(ids), Trees: len(trees[subject])}
	}
	return out
}
