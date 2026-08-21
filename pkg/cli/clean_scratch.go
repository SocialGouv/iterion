package cli

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/memory"
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
