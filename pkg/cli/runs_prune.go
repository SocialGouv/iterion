package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// PruneOptions holds the configuration for `iterion runs prune`.
type PruneOptions struct {
	StoreDir  string
	OlderThan time.Duration
	KeepLast  int
	Statuses  []string
	DryRun    bool
	Now       func() time.Time // test seam; nil = time.Now
}

// pruneAllowedStatuses is the closed set of statuses eligible for
// pruning. `failed_resumable` is included here (so the user can opt
// into it) but is NOT in the default `--status` list — a resumable
// failure is by definition still recoverable, so pruning it silently
// would drop rescuable state.
var pruneAllowedStatuses = map[string]store.RunStatus{
	"finished":         store.RunStatusFinished,
	"failed":           store.RunStatusFailed,
	"cancelled":        store.RunStatusCancelled,
	"failed_resumable": store.RunStatusFailedResumable,
}

// PrunedRun is a machine-readable record of one prune decision. Used
// as the row element of both the human table and the --json output.
type PrunedRun struct {
	ID           string    `json:"id"`
	Name         string    `json:"name,omitempty"`
	WorkflowName string    `json:"workflow_name,omitempty"`
	BundleName   string    `json:"bundle_name,omitempty"`
	Status       string    `json:"status"`
	AgeSeconds   int64     `json:"age_seconds"`
	Timestamp    time.Time `json:"timestamp"`
	Deleted      bool      `json:"deleted"`
}

// PruneResult is the top-level payload for --json.
type PruneResult struct {
	StoreDir     string      `json:"store_dir"`
	AgeField     string      `json:"age_field"`
	OlderThan    string      `json:"older_than"`
	KeepLast     int         `json:"keep_last"`
	Statuses     []string    `json:"statuses"`
	DryRun       bool        `json:"dry_run"`
	Scanned      int         `json:"scanned"`
	Pruned       []PrunedRun `json:"pruned"`
	PrunedCount  int         `json:"pruned_count"`
	SkippedCount int         `json:"skipped_count"`
	// Unreadable lists run dirs whose run.json could not be loaded
	// (partial delete, crash before the first write). They are never
	// deleted — the operator inspects or removes them by hand.
	Unreadable []string `json:"unreadable,omitempty"`
	// TombstonesReaped counts the deletion markers older than the
	// retention horizon that this sweep removed for good.
	TombstonesReaped int `json:"tombstones_reaped,omitempty"`
	// WorkspaceObjectsPruned / WorkspaceBytesReclaimed report the sweep of
	// the store-global workspace-versioning pool. Deleting a run's
	// directory removes its snapshot manifests but not the content they
	// referenced — the pool is shared across runs by design, so it can
	// only be swept against the manifests that REMAIN. Without this the
	// pool grew for the life of the store and pruning made it strictly
	// worse: the runs went, the bytes stayed, unreachable.
	WorkspaceObjectsPruned  int   `json:"workspace_objects_pruned,omitempty"`
	WorkspaceBytesReclaimed int64 `json:"workspace_bytes_reclaimed,omitempty"`
}

// pruneAgeField is the constant advertised on --json output and in the
// human summary. UpdatedAt is the "most recent activity" timestamp on
// FilesystemRunStore.Run: it is stamped on every write (create, status
// transition, event append that updates the doc), so for a terminal
// run it equals FinishedAt, and for a legacy record without FinishedAt
// it still tracks the last mutation. CreatedAt is the fallback when
// UpdatedAt is zero (very old legacy runs).
const pruneAgeField = "updated_at (falls back to created_at)"

// pruneObjectGrace is how recently a workspace object may have been
// written and still be spared by the pool sweep.
//
// docs/scheduling.md recommends `runs prune` from the host crontab, which
// is precisely when it overlaps a scheduled bot — so the sweep has to be
// safe against a capture in flight rather than assume an idle store. A
// capture writes its objects first and its manifest last, so anything
// written in the last hour may be alive with its manifest still pending.
// An hour is orders of magnitude above a real capture and costs only a
// deferral to the next sweep.
const pruneObjectGrace = time.Hour

// RunPrune is the entry point for `iterion runs prune`.
func RunPrune(opts PruneOptions, p *Printer) error {
	if opts.OlderThan < 0 {
		return UserInputError(fmt.Errorf("--older-than must be >= 0"))
	}
	if opts.KeepLast < 0 {
		return UserInputError(fmt.Errorf("--keep-last must be >= 0"))
	}
	statuses, err := validatePruneStatuses(opts.Statuses)
	if err != nil {
		return UserInputError(err)
	}

	cwd, _ := os.Getwd()
	storeDir := store.ResolveStoreDir(cwd, opts.StoreDir)

	// A missing store dir or a missing runs subdir is not an error:
	// there is simply nothing to prune. Bail early rather than let
	// store.New create an empty directory tree as a side effect.
	if !storeHasRuns(storeDir) {
		return emitEmpty(p, storeDir, opts, statuses)
	}

	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}

	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	ctx := context.Background()
	ids, err := s.ListRuns(ctx)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}

	// Load, filter, sort by age (newest first) so --keep-last preserves
	// the N most recent matching runs regardless of scan order.
	type candidate struct {
		run *store.Run
		ts  time.Time
	}
	scanned := 0
	var unreadable []string
	candidates := make([]candidate, 0, len(ids))
	for _, id := range ids {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			// A run dir without a loadable run.json (partial delete,
			// crash before the first write) must not sink the whole
			// retention sweep. Surface it loudly and move on — and
			// never delete what could not be read.
			unreadable = append(unreadable, id)
			continue
		}
		scanned++
		if _, ok := statuses[r.Status]; !ok {
			continue
		}
		ts := pruneTimestamp(r)
		if now().Sub(ts) < opts.OlderThan {
			continue
		}
		candidates = append(candidates, candidate{run: r, ts: ts})
	}

	// Newest first for keep-last, then reverse to oldest-first for output
	// (operators expect the oldest pruned runs to lead the list).
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ts.After(candidates[j].ts)
	})
	if opts.KeepLast > 0 && len(candidates) > opts.KeepLast {
		candidates = candidates[opts.KeepLast:]
	} else if opts.KeepLast >= len(candidates) {
		candidates = candidates[:0]
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ts.Before(candidates[j].ts)
	})

	pruned := make([]PrunedRun, 0, len(candidates))
	nowT := now()
	for _, c := range candidates {
		age := nowT.Sub(c.ts)
		if age < 0 {
			age = 0
		}
		row := PrunedRun{
			ID:           c.run.ID,
			Name:         c.run.Name,
			WorkflowName: c.run.WorkflowName,
			BundleName:   c.run.BundleName,
			Status:       string(c.run.Status),
			AgeSeconds:   int64(age.Seconds()),
			Timestamp:    c.ts,
		}
		if !opts.DryRun {
			if err := s.DeleteRun(ctx, c.run.ID); err != nil {
				return fmt.Errorf("delete run %s: %w", c.run.ID, err)
			}
			row.Deleted = true
		}
		pruned = append(pruned, row)
	}

	// Reap deletion tombstones past the same retention horizon: the
	// marker must outlive any plausible late writer (it IS the
	// resurrection guard), not live forever. Fresh markers — including
	// the ones this very sweep just wrote — stay.
	tombstonesReaped := 0
	if !opts.DryRun && opts.OlderThan > 0 {
		if n, err := s.PruneDeletionMarkers(ctx, now().Add(-opts.OlderThan)); err != nil {
			return fmt.Errorf("prune deletion markers: %w", err)
		} else {
			tombstonesReaped = n
		}
	}

	// Sweep the workspace-versioning pool AFTER the run directories are
	// gone, so the manifests of the runs just pruned no longer keep their
	// content alive. The pool is store-global (content is content, and a
	// per-run pool re-stored the whole workspace for every run), which is
	// exactly why pruning runs cannot blind-delete their objects — the
	// sweep has to run against the manifests that survive.
	wsObjects, wsBytes := 0, int64(0)
	if !opts.DryRun {
		if o, b, err := workspacetrack.NewNative(storeDir).PruneObjects(pruneObjectGrace); err != nil {
			// Never fail the prune over it: the runs are already gone and
			// the pool is reclaimable on the next sweep.
			fmt.Fprintf(os.Stderr, "warning: could not sweep the workspace object pool: %v\n", err)
		} else {
			wsObjects, wsBytes = o, b
		}
	}

	result := PruneResult{
		StoreDir:                storeDir,
		AgeField:                pruneAgeField,
		OlderThan:               opts.OlderThan.String(),
		KeepLast:                opts.KeepLast,
		Statuses:                sortedStatusNames(statuses),
		DryRun:                  opts.DryRun,
		Scanned:                 scanned,
		Pruned:                  pruned,
		PrunedCount:             len(pruned),
		SkippedCount:            scanned - len(pruned),
		Unreadable:              unreadable,
		TombstonesReaped:        tombstonesReaped,
		WorkspaceObjectsPruned:  wsObjects,
		WorkspaceBytesReclaimed: wsBytes,
	}
	return renderPruneResult(p, result)
}

// storeHasRuns reports whether the store directory and its runs
// subdirectory both exist. A missing store dir or a missing runs subdir
// is treated as "empty store, nothing to prune" — never as an error.
func storeHasRuns(storeDir string) bool {
	if _, err := os.Stat(storeDir); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(storeDir, "runs")); err != nil {
		return false
	}
	return true
}

// emitEmpty writes the "nothing to prune" result for a missing store
// or a store with no runs directory. Success, exit 0.
func emitEmpty(p *Printer, storeDir string, opts PruneOptions, statuses map[store.RunStatus]struct{}) error {
	result := PruneResult{
		StoreDir:     storeDir,
		AgeField:     pruneAgeField,
		OlderThan:    opts.OlderThan.String(),
		KeepLast:     opts.KeepLast,
		Statuses:     sortedStatusNames(statuses),
		DryRun:       opts.DryRun,
		Scanned:      0,
		Pruned:       []PrunedRun{},
		PrunedCount:  0,
		SkippedCount: 0,
	}
	return renderPruneResult(p, result)
}

// validatePruneStatuses parses the --status list into a set of
// RunStatus values. Empty input applies the default set (finished,
// failed, cancelled — never failed_resumable). Unknown or
// non-prunable values (running, queued, paused_waiting_human, …)
// produce an explicit error so the operator sees exactly what was
// rejected instead of silently pruning a subset.
func validatePruneStatuses(raw []string) (map[store.RunStatus]struct{}, error) {
	if len(raw) == 0 {
		return map[store.RunStatus]struct{}{
			store.RunStatusFinished:  {},
			store.RunStatusFailed:    {},
			store.RunStatusCancelled: {},
		}, nil
	}
	out := make(map[store.RunStatus]struct{}, len(raw))
	for _, name := range raw {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		s, ok := pruneAllowedStatuses[name]
		if !ok {
			return nil, fmt.Errorf(
				"--status %q is not prunable (allowed: %s)",
				name, allowedStatusList(),
			)
		}
		out[s] = struct{}{}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--status: no statuses supplied")
	}
	return out, nil
}

func allowedStatusList() string {
	names := make([]string, 0, len(pruneAllowedStatuses))
	for n := range pruneAllowedStatuses {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func sortedStatusNames(m map[store.RunStatus]struct{}) []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, string(s))
	}
	sort.Strings(out)
	return out
}

// pruneTimestamp picks the most recent activity timestamp for age
// filtering. Order: UpdatedAt → CreatedAt. FinishedAt is not consulted
// separately because applyStatusTransition stamps UpdatedAt to the
// same wall-clock instant when it sets FinishedAt.
func pruneTimestamp(r *store.Run) time.Time {
	if !r.UpdatedAt.IsZero() {
		return r.UpdatedAt
	}
	return r.CreatedAt
}

// formatAge renders a duration in a retention-friendly unit: days
// for anything >= 24h, hours for anything >= 1h, minutes for anything
// >= 1m, seconds otherwise. FormatDuration jumps straight to minutes
// past the 1-minute mark, which reads badly for weeks/months of age.
func formatAge(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%.1fd", d.Hours()/24)
}

func renderPruneResult(p *Printer, r PruneResult) error {
	if p.Format == OutputJSON {
		p.JSON(r)
		return nil
	}

	verb := "would prune"
	if !r.DryRun {
		verb = "pruned"
	}

	if len(r.Unreadable) > 0 {
		sample := r.Unreadable
		if len(sample) > 5 {
			sample = sample[:5]
		}
		p.Line("WARNING: %d unreadable run dir(s) (no loadable run.json) left untouched — inspect or remove by hand: %s%s",
			len(r.Unreadable), strings.Join(sample, ", "),
			map[bool]string{true: ", …", false: ""}[len(r.Unreadable) > 5])
	}

	if r.PrunedCount == 0 {
		p.Line("nothing to prune (scanned %d, store %s)", r.Scanned, r.StoreDir)
		renderWorkspaceReclaim(p, r)
		return nil
	}

	p.Header(fmt.Sprintf("runs prune (%s)", strings.Join(r.Statuses, ",")))
	rows := make([][]string, 0, len(r.Pruned))
	for _, row := range r.Pruned {
		label := row.Name
		if label == "" {
			label = row.BundleName
		}
		if label == "" {
			label = row.WorkflowName
		}
		if label == "" {
			label = "—"
		}
		rows = append(rows, []string{
			row.ID,
			row.Status,
			formatAge(time.Duration(row.AgeSeconds) * time.Second),
			label,
		})
	}
	p.Table([]string{"RUN", "STATUS", "AGE", "NAME"}, rows)
	p.Line("%s %d run(s); scanned %d; store %s (age field: %s)",
		verb, r.PrunedCount, r.Scanned, r.StoreDir, r.AgeField)
	renderWorkspaceReclaim(p, r)
	return nil
}

// renderWorkspaceReclaim reports the workspace-versioning pool sweep.
// Silent when it reclaimed nothing, so a store that never versioned a
// workspace shows no line at all.
func renderWorkspaceReclaim(p *Printer, r PruneResult) {
	if r.WorkspaceObjectsPruned == 0 {
		return
	}
	p.Line("reclaimed %d workspace object(s), %s", r.WorkspaceObjectsPruned, humanBytes(r.WorkspaceBytesReclaimed))
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}
