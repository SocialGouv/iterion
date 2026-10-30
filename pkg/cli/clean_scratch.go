package cli

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/memory"
	iterruntime "github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// CleanedScratch is one out-of-tree working directory the sweep reclaimed
// (or would reclaim) from a workspace's ${PROJECT_SCRATCH_DIR}.
type CleanedScratch struct {
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	AgeSeconds int64  `json:"age_seconds"`
}

// scratchRootForStore maps a store directory to the workspace scratch root
// whose entries belong to it.
//
// The two layouts differ, and assuming one is how a sweep silently covers
// half the machine: a per-project store under the iterion data dir keeps
// scratch/ as a sibling of runs/, but a store INSIDE a workspace
// (<repo>/.iterion) does not — ${PROJECT_SCRATCH_DIR} always resolves into
// the global data dir, keyed by the repo root, which is this store's
// parent.
func scratchRootForStore(storeDir string) string {
	projects := filepath.Join(absStore(store.GlobalIterionDataDir()), "projects")
	if strings.HasPrefix(storeDir, projects+string(os.PathSeparator)) {
		return filepath.Join(storeDir, "scratch")
	}
	return memory.WorkspaceScratchDir(filepath.Dir(storeDir))
}

// sweepStoreScratch reclaims the stale entries of every store's scratch
// root, appending what it took (or would take) to the result.
//
// Age is the whole test, and it has to be: scratch is deliberately SHARED
// between runs — a subbot child writes into its parent's scratch, which is
// how fan-in works — so no entry can be attributed to one run and deleted
// on its behalf. An entry nothing has written to in days is one no live
// run is using. `iterion runs prune` never touched this directory and
// neither did the worktree sweep, which is how a single project reached
// 54 GB of it.
func sweepStoreScratch(stores []string, opts CleanOptions, now func() time.Time, r *CleanResult) {
	seen := make(map[string]bool, len(stores))
	for _, storeDir := range stores {
		root := scratchRootForStore(storeDir)
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true

		entries, err := memory.ScanScratch(root, opts.OlderThan, now())
		if err != nil {
			r.ScratchErrors = append(r.ScratchErrors, err.Error())
			continue
		}
		r.ScratchScanned += len(entries)
		for _, e := range entries {
			if !e.Stale {
				continue
			}
			if !opts.Apply {
				r.Scratch = append(r.Scratch, toCleanedScratch(e, now()))
				r.ScratchBytes += e.Size
				continue
			}
			if err := os.RemoveAll(e.Path); err != nil {
				r.ScratchErrors = append(r.ScratchErrors, err.Error())
				continue
			}
			r.Scratch = append(r.Scratch, toCleanedScratch(e, now()))
			r.ScratchBytes += e.Size
		}
	}
}

// sweepSandboxCaches reclaims the sandbox's own warm caches — the store
// host_state gives a sandboxed run instead of the host's own (#2069) — by the
// same age rule as scratch, and reports them in the same list.
//
// Same rule because it is the same question: a cache nothing has touched
// within --older-than is one no live run is warming, and age is the only thing
// that can be said about a directory deliberately SHARED between runs. Same
// list because the two read alike for the operator: an out-of-tree directory
// the sweep took, and the bytes it returned.
//
// It is swept at all because the store is the one piece of durable state the
// guard introduced with no reclaim: measured on this repository, a shared Go
// build cache grows ~20 GB/h under ~100 clones, and `iterion runs prune`, the
// worktree sweep and the scratch sweep all walked past it. The store belongs
// to the GLOBAL iterion home, so it is swept once, not per store.
func sweepSandboxCaches(opts CleanOptions, now func() time.Time, r *CleanResult) {
	storeDir := iterruntime.SandboxCacheStoreDir(absStore(store.GlobalIterionDataDir()))
	if storeDir == "" {
		return
	}
	paths := make([]string, 0, len(iterruntime.SandboxCacheRelPaths()))
	for _, rel := range iterruntime.SandboxCacheRelPaths() {
		paths = append(paths, filepath.Join(storeDir, filepath.FromSlash(rel)))
	}
	entries, err := memory.ScanNamedPaths(paths, opts.OlderThan, now())
	if err != nil {
		r.ScratchErrors = append(r.ScratchErrors, err.Error())
	}
	r.ScratchScanned += len(entries)
	for _, e := range entries {
		if !e.Stale {
			continue
		}
		if opts.Apply {
			if rmErr := os.RemoveAll(e.Path); rmErr != nil {
				r.ScratchErrors = append(r.ScratchErrors, rmErr.Error())
				continue
			}
		}
		r.Scratch = append(r.Scratch, toCleanedScratch(e, now()))
		r.ScratchBytes += e.Size
	}
}

func toCleanedScratch(e memory.ScratchEntry, now time.Time) CleanedScratch {
	return CleanedScratch{
		Path:       e.Path,
		Bytes:      e.Size,
		AgeSeconds: int64(now.Sub(e.Newest).Seconds()),
	}
}

// renderCleanScratch reports the scratch half of the sweep. It prints
// nothing when there was nothing to take, so the common case stays as
// quiet as it was.
func renderCleanScratch(p *Printer, r CleanResult) {
	for _, e := range r.ScratchErrors {
		p.Line("scratch: %s", e)
	}
	if len(r.Scratch) == 0 {
		return
	}
	verb := "would delete"
	if !r.DryRun {
		verb = "deleted"
	}
	p.Line("%s %d out-of-tree scratch dir(s), %s reclaimed (scanned %d)",
		verb, len(r.Scratch), humanBytes(r.ScratchBytes), r.ScratchScanned)
	for _, e := range r.Scratch {
		p.Line("  %-9s %s  %s", formatAge(time.Duration(e.AgeSeconds)*time.Second), humanBytes(e.Bytes), e.Path)
	}
}
