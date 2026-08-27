package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/worktreepool"
)

// The classifier this command drives lives in pkg/worktreepool, because
// the runtime pool bound has to answer the same questions the same way.
// What stays here is what belongs to a command: flag validation, which
// stores to sweep, and how the verdicts read.
//
// The aliases keep `iterion clean`'s Go and JSON surfaces unchanged
// across that move — cmd/iterion builds a cli.CleanOptions, and --json
// consumers key on field names these types own.
type (
	// CleanLevel selects how much of the worktree pool `iterion clean` is
	// willing to reclaim. The levels are cumulative: moderate includes
	// everything conservative takes, aggressive everything moderate takes.
	CleanLevel = worktreepool.Level
	// CleanedWorktree is one decision record, shared by the table and --json.
	CleanedWorktree = worktreepool.Entry
)

const (
	CleanConservative = worktreepool.LevelConservative
	CleanModerate     = worktreepool.LevelModerate
	CleanAggressive   = worktreepool.LevelAggressive
)

// Landing classes and skip reasons, re-exported under the names this
// package's tests and rendering already use.
const (
	landingOrphan    = worktreepool.LandingOrphan
	landingMerged    = worktreepool.LandingMerged
	landingOwnBranch = worktreepool.LandingOwnBranch
	landingNowhere   = worktreepool.LandingNowhere

	skipRunActive = worktreepool.SkipRunActive
	skipUnlanded  = worktreepool.SkipUnlanded
	skipNested    = worktreepool.SkipNested
	skipTooRecent = worktreepool.SkipTooRecent
	skipKeepLast  = worktreepool.SkipKeepLast
	skipLevel     = worktreepool.SkipLevel
	skipResumable = worktreepool.SkipResumable
	skipVanished  = worktreepool.SkipVanished
)

// CleanOptions holds the configuration for `iterion clean`.
type CleanOptions struct {
	StoreDir    string
	Level       CleanLevel
	OlderThan   time.Duration
	Apply       bool
	AllProjects bool
	KeepLast    int
	WithRuns    bool
	// IncludeResumable gives up the ability to resume the runs whose
	// worktrees are swept. Off by default.
	IncludeResumable bool
	Now              func() time.Time // test seam; nil = time.Now

	// Remove, DuringEligibility and AfterEligibility are test seams, nil
	// in production. They stand in the window between the pre-deletion
	// re-derivation and the removal itself, which is the only place a
	// test can prove the guards covering that window are load-bearing.
	Remove            func(string) error
	DuringEligibility func(string)
	AfterEligibility  func(string)
}

// CleanResult is the top-level payload for --json.
type CleanResult struct {
	Stores    []string          `json:"stores"`
	Level     string            `json:"level"`
	OlderThan string            `json:"older_than"`
	KeepLast  int               `json:"keep_last"`
	DryRun    bool              `json:"dry_run"`
	WithRuns  bool              `json:"with_runs"`
	Scanned   int               `json:"scanned"`
	Deleted   []CleanedWorktree `json:"deleted"`
	Spared    []CleanedWorktree `json:"spared"`
	// Failed is kept apart from Deleted so `deleted_count` counts
	// deletions rather than attempts: a machine consumer reading
	// `deleted_count: 2` on a sweep that removed nothing is worse than no
	// report at all.
	Failed         []CleanedWorktree `json:"failed,omitempty"`
	DeletedCount   int               `json:"deleted_count"`
	FailedCount    int               `json:"failed_count,omitempty"`
	BytesReclaimed int64             `json:"bytes_reclaimed"`
	// RegistrationsPruned counts the per-worktree administrative entries
	// dropped from parent repositories after a successful deletion.
	RegistrationsPruned int `json:"registrations_pruned,omitempty"`

	// Scratch is the out-of-tree half of the sweep: ${PROJECT_SCRATCH_DIR}
	// entries nothing has written to within --older-than. Kept apart from
	// Deleted because the two answer different questions — a worktree's
	// fate is decided by what git can prove, a scratch entry's by whether
	// anything still touches it.
	Scratch        []CleanedScratch `json:"scratch,omitempty"`
	ScratchScanned int              `json:"scratch_scanned,omitempty"`
	ScratchBytes   int64            `json:"scratch_bytes_reclaimed,omitempty"`
	ScratchErrors  []string         `json:"scratch_errors,omitempty"`
}

// RunClean is the entry point for `iterion clean`.
func RunClean(opts CleanOptions, p *Printer) error {
	if opts.OlderThan < 0 {
		return UserInputError(errors.New("--older-than must be >= 0"))
	}
	if opts.KeepLast < 0 {
		return UserInputError(errors.New("--keep-last must be >= 0"))
	}
	if !worktreepool.KnownLevel(opts.Level) {
		return UserInputError(fmt.Errorf(
			"--level %q is not a level (allowed: conservative, moderate, aggressive)", opts.Level))
	}

	if err := worktreepool.PreflightGit(); err != nil {
		return err
	}

	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	stores, err := resolveCleanStores(opts)
	if err != nil {
		return err
	}

	sweepOpts := opts.sweepOptions()

	// keep-last is applied per store: an operator who asks to keep the
	// last 10 means 10 of this project's, not 10 across every project on
	// the machine — under --all-projects a global floor would empty whole
	// stores to make room for a busier one's recent runs.
	var all []CleanedWorktree
	for _, storeDir := range stores {
		found, err := worktreepool.Scan(storeDir, sweepOpts.ScanOptions)
		if err != nil {
			return err
		}
		all = append(all, found...)
	}

	// Oldest first: the operator reads the list top-down and the head of
	// it is what they are least likely to want back.
	worktreepool.SortOldestFirst(all)

	result := CleanResult{
		Stores:    stores,
		Level:     string(opts.Level),
		OlderThan: opts.OlderThan.String(),
		KeepLast:  opts.KeepLast,
		DryRun:    !opts.Apply,
		WithRuns:  opts.WithRuns,
		Scanned:   len(all),
		Deleted:   []CleanedWorktree{},
		Spared:    []CleanedWorktree{},
	}

	swept := worktreepool.Sweep(all, sweepOpts)
	result.Deleted = swept.Deleted
	result.Spared = swept.Spared
	result.Failed = swept.Failed
	result.BytesReclaimed = swept.BytesReclaimed
	result.RegistrationsPruned = swept.RegistrationsPruned
	result.DeletedCount = len(result.Deleted)
	result.FailedCount = len(result.Failed)

	// The out-of-tree half. Run last so a scratch error can never cost the
	// operator the worktree report they came for.
	sweepStoreScratch(stores, opts, now, &result)

	if err := renderCleanResult(p, result); err != nil {
		return err
	}
	// Failures never abort the sweep; they are reported alongside what it
	// did take, and only then returned.
	return errors.Join(swept.Errors...)
}

// sweepOptions maps the command's flags onto the classifier's.
func (o CleanOptions) sweepOptions() worktreepool.SweepOptions {
	return worktreepool.SweepOptions{
		ScanOptions: worktreepool.ScanOptions{
			Level:            o.Level,
			OlderThan:        o.OlderThan,
			KeepLast:         o.KeepLast,
			IncludeResumable: o.IncludeResumable,
			Now:              o.Now,
			// The report exists to tell an operator what the NEXT
			// level would free, so a spared entry's size is the point.
			MeasureSpared: true,
		},
		Apply:             o.Apply,
		WithRuns:          o.WithRuns,
		Remove:            o.Remove,
		DuringEligibility: o.DuringEligibility,
		AfterEligibility:  o.AfterEligibility,
	}
}

// absStore keeps the legacy CLI helper while the shared implementation
// lives beside the classifier that requires absolute paths.
func absStore(dir string) string { return worktreepool.AbsPath(dir) }

// resolveCleanStores lists the store directories to sweep. Without
// --all-projects that is the single store the working directory maps to;
// with it, every per-project store under the iterion data dir, plus the
// data dir itself when it holds worktrees of its own (the layout left by
// running iterion from $HOME, and by the e2e suite).
func resolveCleanStores(opts CleanOptions) ([]string, error) {
	if !opts.AllProjects {
		cwd, _ := os.Getwd()
		storeDir := absStore(store.ResolveStoreDir(cwd, opts.StoreDir))
		// A --store-dir the operator named explicitly must exist.
		// Reporting "nothing to clean" for a typo'd path is a silent
		// success a cron would repeat forever while the real store fills.
		if opts.StoreDir != "" {
			if _, err := os.Stat(storeDir); err != nil {
				return nil, UserInputError(fmt.Errorf("--store-dir %s: %w", storeDir, err))
			}
		}
		return []string{storeDir}, nil
	}
	if opts.StoreDir != "" {
		return nil, UserInputError(errors.New("--all-projects and --store-dir are mutually exclusive"))
	}

	root := absStore(store.GlobalIterionDataDir())
	var stores []string
	if isSweepableStore(root) {
		stores = append(stores, root)
	}
	entries, err := os.ReadDir(filepath.Join(root, "projects"))
	if err != nil {
		if os.IsNotExist(err) {
			return stores, nil
		}
		return nil, fmt.Errorf("list project stores under %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, "projects", e.Name())
		if isSweepableStore(dir) {
			stores = append(stores, dir)
		}
	}
	sort.Strings(stores)
	return stores, nil
}

// isSweepableStore reports whether a directory holds anything this
// command reclaims. Worktrees are not the only answer: a workspace can
// accumulate gigabytes of ${PROJECT_SCRATCH_DIR} while never running a
// `worktree: auto` bot, and keying discovery on worktrees alone made
// those stores invisible to --all-projects — a sweep that silently covers
// part of the machine is worse than one that admits it found nothing.
func isSweepableStore(storeDir string) bool {
	return worktreepool.HasWorktreeDir(storeDir) || isDir(filepath.Join(storeDir, "scratch"))
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// renderCleanResult renders one sweep's verdicts for the operator, as a
// table or as --json.
func renderCleanResult(p *Printer, r CleanResult) error {
	if p.Format == OutputJSON {
		p.JSON(r)
		return nil
	}

	if r.DeletedCount == 0 {
		p.Line("nothing to clean at level %s (scanned %d worktree(s) in %d store(s))",
			r.Level, r.Scanned, len(r.Stores))
		// A sweep whose every deletion failed still deleted nothing — but
		// saying only that would send the operator away reassured while a
		// tree may be half removed.
		renderCleanFailures(p, r)
		renderCleanSpared(p, r)
		renderCleanScratch(p, r)
		if r.DryRun && len(r.Scratch) > 0 {
			p.Line("dry run — nothing was deleted; re-run with --apply")
		}
		return nil
	}

	verb := "would delete"
	if !r.DryRun {
		verb = "deleted"
	}

	p.Header(fmt.Sprintf("clean — level %s", r.Level))
	rows := make([][]string, 0, len(r.Deleted))
	for _, wt := range r.Deleted {
		landing := wt.Landing
		if wt.Dirty {
			landing += " +dirty"
		}
		note := wt.Path
		if wt.IgnoredEntries > 0 {
			note = fmt.Sprintf("%s (%d gitignored path(s))", wt.Path, wt.IgnoredEntries)
		}
		rows = append(rows, []string{
			shortRunID(wt.RunID),
			landing,
			formatAge(time.Duration(wt.AgeSeconds) * time.Second),
			humanBytes(wt.Bytes),
			note,
		})
	}
	p.Table([]string{"RUN", "LANDING", "AGE", "SIZE", "PATH"}, rows)
	p.Line("%s %d worktree(s), %s reclaimed (scanned %d in %d store(s))",
		verb, r.DeletedCount, humanBytes(r.BytesReclaimed), r.Scanned, len(r.Stores))
	renderCleanFailures(p, r)
	for _, wt := range r.Deleted {
		if wt.RunError != "" {
			p.Line("deleted, but its run record could not be removed: %s (%s)", wt.RunID, wt.RunError)
		}
		if wt.RegistrationError != "" {
			p.Line("deleted, but its worktree registration could not be dropped: %s (%s)", wt.Path, wt.RegistrationError)
		}
	}
	if r.WithRuns {
		if r.DryRun {
			p.Line("run records would be deleted alongside their worktree (--with-runs)")
		} else {
			p.Line("run records deleted alongside their worktree (--with-runs)")
		}
	}
	if r.RegistrationsPruned > 0 {
		p.Line("dropped %d stale worktree registration(s)", r.RegistrationsPruned)
	}
	renderCleanSpared(p, r)
	renderCleanScratch(p, r)
	if r.DryRun {
		p.Line("dry run — nothing was deleted; re-run with --apply")
	}
	return nil
}

// renderCleanFailures names every deletion that was attempted and did
// not complete. os.RemoveAll works its way into a tree before it fails,
// so "failed" and "untouched" are not the same thing.
func renderCleanFailures(p *Printer, r CleanResult) {
	for _, wt := range r.Failed {
		p.Line("FAILED, may be partially deleted: %s (%s)", wt.Path, wt.Error)
	}
}

// renderCleanSpared summarises what was protected and why. A sweep that
// silently reports only its deletions leaves the operator unable to tell
// "nothing was eligible" from "everything was guarded".
func renderCleanSpared(p *Printer, r CleanResult) {
	if len(r.Spared) == 0 {
		return
	}
	counts := map[string]int{}
	bytes := map[string]int64{}
	for _, wt := range r.Spared {
		counts[wt.SkipReason]++
		bytes[wt.SkipReason] += wt.Bytes
	}
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		// What a higher level would yield is the operator's next decision,
		// so the size withheld by the level ladder is worth naming.
		part := fmt.Sprintf("%s: %d", reason, counts[reason])
		if bytes[reason] > 0 {
			part += " (" + humanBytes(bytes[reason]) + ")"
		}
		if reason == skipResumable {
			part += ", released by --include-resumable"
		}
		parts = append(parts, part)
	}
	p.Line("spared — %s", strings.Join(parts, "; "))
	// A spared entry that carries a reason of its own — a held run lock,
	// a tree that could not be read — is not the same as the plain
	// category it was filed under, and the operator cannot act on a
	// count alone.
	for _, wt := range r.Spared {
		if wt.Error != "" {
			p.Line("  %s: %s", wt.Path, wt.Error)
		}
	}
}

// shortRunID trims a UUIDv7 run id to its leading segment for the table.
// Run ids are unique in their first segment within any one store.
func shortRunID(id string) string {
	if i := strings.Index(id, "-"); i > 0 {
		return id[:i]
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
