package server

import (
	"context"
	"sync"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// LOC-changed stat for the run header: the three-dot numstat of the
// run's final commits against the fork point. Computed lazily on the
// run-detail read and memoized — the pair (FinalCommit, MergedCommit)
// is the version key, so a re-merge recomputes and everything else is
// a map hit (the git subprocess never runs on WS-driven re-reads).
//
// nil fields = "—" (refs unresolvable: branch deleted, commit GC'd,
// repo gone); zero values = a genuinely empty diff. No pathspec
// exclusions: the store dir is gitignored and worktree runs never
// commit it, so the diff is already code-only.

type locEntry struct {
	version    string
	added, del int
	ok         bool
}

type runLOCCache struct {
	mu      sync.Mutex
	entries map[string]locEntry
}

func newRunLOCCache() *runLOCCache {
	return &runLOCCache{entries: make(map[string]locEntry)}
}

// enrichRunLOC fills header.LocAdded/LocDeleted for a finalized
// worktree run. Best-effort by design: a run without a FinalCommit, or
// with unresolvable refs, just leaves the fields nil.
func (s *Server) enrichRunLOC(ctx context.Context, header *runview.RunHeader) {
	if header == nil || header.FinalCommit == "" {
		return
	}
	r, err := s.runs.LoadRunCtx(ctx, header.ID)
	if err != nil || r.RepoRoot == "" {
		return
	}
	version := r.FinalCommit + "|" + r.MergedCommit
	s.locCache.mu.Lock()
	if e, hit := s.locCache.entries[header.ID]; hit && e.version == version {
		s.locCache.mu.Unlock()
		applyLOC(header, e)
		return
	}
	s.locCache.mu.Unlock()

	e := locEntry{version: version}
	e.added, e.del, e.ok = gitlib.DiffLOC(r.RepoRoot, locDiffTarget(r), r.FinalCommit)

	s.locCache.mu.Lock()
	s.locCache.entries[header.ID] = e
	s.locCache.mu.Unlock()
	applyLOC(header, e)
}

// locDiffTarget picks the ref the run's commits are measured against:
// the merge target when one was recorded, else the fork-point commit
// itself (merge-base(base, final) == base, so the three-dot collapses
// to "what the run added").
func locDiffTarget(r *store.Run) string {
	if r.MergedInto != "" && r.MergedInto != "none" {
		return r.MergedInto
	}
	return r.BaseCommit
}

func applyLOC(header *runview.RunHeader, e locEntry) {
	if !e.ok {
		return
	}
	added, del := e.added, e.del
	header.LocAdded = &added
	header.LocDeleted = &del
}
