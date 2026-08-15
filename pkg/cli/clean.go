package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// CleanLevel selects how much of the worktree pool `iterion clean` is
// willing to reclaim. The levels are cumulative: moderate includes
// everything conservative takes, aggressive everything moderate takes.
type CleanLevel string

const (
	// CleanConservative deletes only worktrees whose commits another ref
	// has already built upon, with nothing uncommitted in the tree. Git
	// proves the content is recoverable; nothing else qualifies.
	CleanConservative CleanLevel = "conservative"
	// CleanModerate additionally deletes merged worktrees carrying
	// uncommitted files. The commits survive on the other ref; what goes
	// is whatever was never committed.
	CleanModerate CleanLevel = "moderate"
	// CleanAggressive additionally deletes worktrees whose commits are
	// held only by a ref pointing at them (no commit is lost — the ref is
	// in the parent repository and outlives the directory) and orphan
	// directories, whose contents git cannot account for at all.
	CleanAggressive CleanLevel = "aggressive"
)

// cleanLevelRank orders the levels so eligibility is a single comparison.
var cleanLevelRank = map[CleanLevel]int{
	CleanConservative: 0,
	CleanModerate:     1,
	CleanAggressive:   2,
}

// CleanOptions holds the configuration for `iterion clean`.
type CleanOptions struct {
	StoreDir    string
	Level       CleanLevel
	OlderThan   time.Duration
	Apply       bool
	AllProjects bool
	KeepLast    int
	WithRuns    bool
	Now         func() time.Time // test seam; nil = time.Now
}

// Landing classes. These describe what git can prove about a worktree's
// commits, which is the only question that decides whether deleting the
// directory can destroy work.
const (
	// landingOrphan: git cannot account for this directory — it is not a
	// worktree, or the answers git gives belong to some enclosing
	// repository rather than to it. Its contents are unknowable, so it is
	// not conservative-deletable even though it is often pure leftover.
	landingOrphan = "orphan"
	// landingMerged: a ref whose tip is NOT this HEAD contains this HEAD.
	// Another line of work was built on top of these commits, so they are
	// reachable independently of anything this worktree holds.
	landingMerged = "merged"
	// landingOwnBranch: refs contain HEAD, but every one of them points
	// exactly AT it — they are labels on this commit, not work built upon
	// it. Deleting the directory keeps the commits (the ref stays), but
	// nothing has adopted them yet.
	landingOwnBranch = "own-branch"
	// landingNowhere: no ref contains HEAD, or git could not be made to
	// answer. Deleting the directory would leave the commits reachable
	// only via reflog and eligible for GC. Never deleted, at any level.
	landingNowhere = "unlanded"
	// landingNested: the tree carries a repository of its own — an
	// initialised submodule, or a plain clone someone dropped inside it.
	// Its objects live under the directory (or under the administrative
	// directory that goes with it) and containment in the outer
	// repository says nothing about them. Never deleted, at any level.
	landingNested = "nested-repo"
)

// Skip reasons — why an otherwise-eligible worktree was spared.
const (
	skipRunActive = "run-active"
	skipUnlanded  = "unlanded"
	skipNested    = "nested-repo"
	skipTooRecent = "too-recent"
	skipKeepLast  = "keep-last"
	skipLevel     = "needs-higher-level"
	// skipVanished: the directory was gone before we reached it, so this
	// sweep neither deleted it nor decided anything about it.
	skipVanished = "already-gone"
)

// iterionRefPrefix is where iterion persists its own per-run checkpoints
// (pkg/store/turn.go). Those refs hold a run's commits alive, which is
// why they must be consulted — a worktree whose only refs live there was
// being reported as `unlanded`, i.e. unrecoverable, when it was not. But
// they are this run's own bookkeeping, not another line of work adopting
// it, and they are reaped with the run: containment by one of them can
// never mean `merged`.
const iterionRefPrefix = "refs/iterion/"

// CleanedWorktree is one decision record, shared by the table and --json.
type CleanedWorktree struct {
	RunID      string    `json:"run_id"`
	Path       string    `json:"path"`
	StoreDir   string    `json:"store_dir"`
	Landing    string    `json:"landing"`
	Dirty      bool      `json:"dirty"`
	RunStatus  string    `json:"run_status,omitempty"`
	Bytes      int64     `json:"bytes"`
	AgeSeconds int64     `json:"age_seconds"`
	ModTime    time.Time `json:"mod_time"`
	// ContainedBy names up to a few refs that were built on top of this
	// HEAD — the evidence for a `merged` verdict, so the operator can
	// check the claim rather than trust it.
	ContainedBy []string `json:"contained_by,omitempty"`
	// IgnoredEntries counts top-level gitignored paths present in the
	// tree. Ignored content is deleted at every level (in a run worktree
	// it is the build output the command exists to reclaim), so the count
	// is surfaced rather than gated on.
	IgnoredEntries int  `json:"ignored_entries,omitempty"`
	Deleted        bool `json:"deleted"`
	// NestedRepos names repositories found inside the tree, which is why
	// the outer repository's verdict cannot speak for it.
	NestedRepos []string `json:"nested_repos,omitempty"`
	// SkipReason is set on spared worktrees and empty on deleted ones.
	SkipReason string `json:"skip_reason,omitempty"`
	// RunDeleted reports whether the paired run record went too (--with-runs).
	RunDeleted bool `json:"run_deleted,omitempty"`
	// Error records why the directory could not be classified or could not
	// be removed. A removal that failed may have left a partial tree.
	Error string `json:"error,omitempty"`
	// RunError and RegistrationError are kept apart from Error so a
	// worktree that WAS removed and only failed its bookkeeping is not
	// reported as a failed, possibly-partial deletion.
	RunError          string `json:"run_error,omitempty"`
	RegistrationError string `json:"registration_error,omitempty"`
	// gitCommonDir and resolvedPath are captured before deletion so the
	// registration can be matched afterwards, when the path is gone and
	// symlinks can no longer be resolved. Not serialised.
	gitCommonDir string
	resolvedPath string
	head         string
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
}

// cleanGitTimeout bounds every git invocation the sweep makes. A wedged
// git (stale index.lock, hung filter) must not hang a maintenance
// command that operators wire into cron.
const cleanGitTimeout = 30 * time.Second

// RunClean is the entry point for `iterion clean`.
func RunClean(opts CleanOptions, p *Printer) error {
	if opts.OlderThan < 0 {
		return UserInputError(errors.New("--older-than must be >= 0"))
	}
	if opts.KeepLast < 0 {
		return UserInputError(errors.New("--keep-last must be >= 0"))
	}
	if _, ok := cleanLevelRank[opts.Level]; !ok {
		return UserInputError(fmt.Errorf(
			"--level %q is not a level (allowed: conservative, moderate, aggressive)", opts.Level))
	}

	if err := preflightGit(); err != nil {
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

	// keep-last is applied per store: an operator who asks to keep the
	// last 10 means 10 of this project's, not 10 across every project on
	// the machine — under --all-projects a global floor would empty whole
	// stores to make room for a busier one's recent runs.
	var all []CleanedWorktree
	for _, storeDir := range stores {
		found, err := scanStoreWorktrees(storeDir, opts, now)
		if err != nil {
			return err
		}
		sort.Slice(found, func(i, j int) bool { return found[i].ModTime.Before(found[j].ModTime) })
		applyKeepLast(found, opts.KeepLast)
		all = append(all, found...)
	}

	// Oldest first: the operator reads the list top-down and the head of
	// it is what they are least likely to want back.
	sort.SliceStable(all, func(i, j int) bool { return all[i].ModTime.Before(all[j].ModTime) })

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

	// Failures never abort the sweep. Returning early would strand the
	// deletions already made with no report at all — the operator would
	// read an error and have no way to learn what is already gone.
	var failures []error

	for i := range all {
		wt := &all[i]
		if wt.SkipReason != "" {
			result.Spared = append(result.Spared, *wt)
			continue
		}
		if !opts.Apply {
			result.BytesReclaimed += wt.Bytes
			result.Deleted = append(result.Deleted, *wt)
			continue
		}

		// Hold the run's lock across the whole deletion. Re-reading the
		// status is not enough on its own: the window it closes is not an
		// instant but the entire os.RemoveAll, which on a real worktree
		// (node_modules, target, .venv) runs for seconds. `iterion run`
		// and `iterion resume` hold this same lock for a run's lifetime,
		// so taking it is what actually makes "a live run keeps its
		// worktree" true rather than likely.
		lock, err := lockRunForClean(wt.StoreDir, wt.RunID)
		if err != nil {
			wt.SkipReason = skipRunActive
			wt.Error = err.Error()
			result.Spared = append(result.Spared, *wt)
			continue
		}

		if st, ok := reloadRunStatus(wt.StoreDir, wt.RunID); ok && !st.IsTerminal() {
			wt.RunStatus = string(st)
			wt.SkipReason = skipRunActive
			result.Spared = append(result.Spared, *wt)
			releaseLock(lock)
			continue
		}

		// The classification is a photograph, and the sweep can take tens
		// of seconds to reach this entry. The run lock says nothing about
		// the writer that matters here — an operator, an editor, an agent
		// working outside iterion. Re-ask the whole question, not just
		// "is it dirty": a COMMIT leaves a clean tree, so asking only
		// about the working tree waves through the one change that
		// creates something to lose. Deleting then takes the worktree's
		// administrative directory with it, and the new commit — held by
		// nothing else — is dangling until the next gc.
		// A concurrent sweep may already have taken it. Asked before the
		// re-derivation, because inspecting a path that is gone yields
		// "git could not tell" and would file it under `unlanded` — a
		// verdict about work that is not what happened.
		if _, err := os.Lstat(wt.Path); os.IsNotExist(err) {
			// Its bytes were reclaimed by whoever removed it, not held
			// back by us: reporting them would read as "still to gain".
			wt.SkipReason, wt.Bytes = skipVanished, 0
			result.Spared = append(result.Spared, *wt)
			releaseLock(lock)
			continue
		}

		if reason, ok := stillEligible(wt, opts.Level); !ok {
			wt.SkipReason = reason
			result.Spared = append(result.Spared, *wt)
			releaseLock(lock)
			continue
		}

		afterEligibility(wt.Path)

		// stillEligible spends several git calls and a full walk, so ask
		// once more: os.RemoveAll succeeds on a path that is already gone
		// and both sweeps would claim the deletion and its bytes.
		if _, err := os.Lstat(wt.Path); os.IsNotExist(err) {
			wt.SkipReason, wt.Bytes = skipVanished, 0
			result.Spared = append(result.Spared, *wt)
			releaseLock(lock)
			continue
		}

		if err := removeTree(wt.Path); err != nil {
			wt.Error = err.Error()
			failures = append(failures, fmt.Errorf("delete worktree %s: %w", wt.Path, err))
			result.Failed = append(result.Failed, *wt)
			releaseLock(lock)
			continue
		}
		wt.Deleted = true
		result.BytesReclaimed += wt.Bytes

		if wt.gitCommonDir != "" {
			if pruned, err := pruneWorktreeRegistration(wt.gitCommonDir, wt.resolvedPath); err != nil {
				wt.RegistrationError = err.Error()
				failures = append(failures, fmt.Errorf("prune registration for %s: %w", wt.Path, err))
			} else if pruned {
				result.RegistrationsPruned++
			}
		}
		releaseLock(lock)

		if opts.WithRuns && wt.RunID != "" {
			if err := deleteRunRecord(wt.StoreDir, wt.RunID); err != nil {
				wt.RunError = err.Error()
				failures = append(failures, fmt.Errorf("delete run %s: %w", wt.RunID, err))
			} else {
				wt.RunDeleted = true
			}
		}
		result.Deleted = append(result.Deleted, *wt)
	}
	result.DeletedCount = len(result.Deleted)
	result.FailedCount = len(result.Failed)

	if err := renderCleanResult(p, result); err != nil {
		return err
	}
	return errors.Join(failures...)
}

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
	if hasWorktreeDir(root) {
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
		if hasWorktreeDir(dir) {
			stores = append(stores, dir)
		}
	}
	sort.Strings(stores)
	return stores, nil
}

// absStore makes a store path absolute.
//
// Every answer git gives comes back absolute, and a store dir does not
// have to: `ResolveStoreDir` returns an explicit --store-dir verbatim, and
// `--store-dir .iterion` is the documented incantation for the
// project-local layout. Comparing git's absolute answer against a relative
// path then fails for every worktree at once — `samePath` never matches,
// so every directory reads `orphan`, which aggressive deletes; `isInside`
// cannot even form a relative path, so the nested-repo guard never fires;
// and the registration lookup never matches its recorded gitdir. One
// normalisation at the boundary is what keeps all three honest.
func absStore(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func hasWorktreeDir(storeDir string) bool {
	info, err := os.Stat(filepath.Join(storeDir, "worktrees"))
	return err == nil && info.IsDir()
}

// scanStoreWorktrees classifies every run worktree in one store.
func scanStoreWorktrees(storeDir string, opts CleanOptions, now func() time.Time) ([]CleanedWorktree, error) {
	wtRoot := filepath.Join(storeDir, "worktrees")
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read worktrees dir %s: %w", wtRoot, err)
	}

	statuses := loadRunStatuses(storeDir)

	out := make([]CleanedWorktree, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Dot-prefixed entries are not per-run worktrees: `.state` holds
		// gate state (built jars, database volumes, browser diagnostics)
		// that belongs to the store, not to any one run. Reclaiming it is
		// a different decision with a different cost, so clean never
		// touches it.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(wtRoot, e.Name())
		out = append(out, classifyWorktree(path, e.Name(), storeDir, statuses, opts, now))
	}
	return out, nil
}

// classifyWorktree produces the full decision for one worktree
// directory. The guards are ordered by how absolute they are and how
// expensive they are to check: an active run is refused before git is
// ever consulted, and the directory is only measured once it is a real
// candidate.
func classifyWorktree(
	path, runID, storeDir string,
	statuses map[string]store.RunStatus,
	opts CleanOptions,
	now func() time.Time,
) CleanedWorktree {
	wt := CleanedWorktree{RunID: runID, Path: path, StoreDir: storeDir, resolvedPath: resolvePath(path)}

	if info, err := os.Stat(path); err == nil {
		wt.ModTime = info.ModTime()
		age := now().Sub(wt.ModTime)
		if age < 0 {
			age = 0
		}
		wt.AgeSeconds = int64(age.Seconds())
	}

	// Guard 1 — an unfinished run owns its worktree. This is checked
	// against run status, not against the directory's mtime: a run that
	// spends hours inside a single agent turn stops touching its
	// worktree, so age alone would declare a live run abandoned.
	if st, ok := statuses[runID]; ok {
		wt.RunStatus = string(st)
		if !st.IsTerminal() {
			wt.SkipReason = skipRunActive
			return wt
		}
	}

	insp := inspectWorktreeGit(path)
	wt.Landing, wt.Dirty, wt.ContainedBy = insp.landing, insp.dirty, insp.containedBy
	wt.gitCommonDir = insp.commonDir
	wt.head = insp.head
	wt.IgnoredEntries = insp.ignoredEntries
	if insp.err != nil {
		wt.Error = insp.err.Error()
	}

	// Guard 2 — commits nothing was built upon and no ref even holds.
	// Deleting the directory would leave them reachable only through the
	// reflog, which expires. No level lifts this; recovering the work is
	// the operator's call, made with git, not a sweep's.
	if wt.Landing == landingNowhere {
		wt.SkipReason = skipUnlanded
	} else if wt.Landing == landingNested {
		// Guard 3 — a repository inside the tree, whose objects the outer
		// repository cannot vouch for.
		wt.SkipReason = skipNested
	} else if opts.OlderThan > 0 && time.Duration(wt.AgeSeconds)*time.Second < opts.OlderThan {
		wt.SkipReason = skipTooRecent
	} else if cleanLevelRank[opts.Level] < requiredLevel(wt.Landing, wt.Dirty) {
		wt.SkipReason = skipLevel
	}

	// Measuring costs a walk of every file, so it is spent only where the
	// number changes a decision: on candidates, and on what only the
	// level ladder is holding back — an operator told what this level
	// yields cannot otherwise see that the next frees ten times more.
	// Nothing else is walked. In particular never the live checkout of a
	// running campaign, whose size would be an incoherent snapshot taken
	// in competition with the run's own I/O.
	if wt.SkipReason != "" && wt.SkipReason != skipLevel {
		return wt
	}
	bytes, nested, complete := scanTree(path)
	wt.Bytes = bytes
	if len(nested) > 0 {
		wt.Landing, wt.SkipReason = landingNested, skipNested
		wt.NestedRepos = nested
		if len(wt.NestedRepos) > 3 {
			wt.NestedRepos = wt.NestedRepos[:3]
		}
	} else if !complete && wt.SkipReason == "" {
		// The walk is what proves there is no repository hiding in
		// there. A walk that could not finish has not proved it.
		wt.Landing, wt.SkipReason = landingNested, skipNested
		wt.Error = "tree could not be fully read; refusing to claim it holds no nested repository"
	}
	return wt
}

// requiredLevel is the minimum level that admits a given landing class.
func requiredLevel(landing string, dirty bool) int {
	switch landing {
	case landingMerged:
		if dirty {
			return cleanLevelRank[CleanModerate]
		}
		return cleanLevelRank[CleanConservative]
	case landingOwnBranch, landingOrphan:
		// An orphan is usually pure leftover, but git cannot say what is
		// in it — a checkout whose parent repository moved looks exactly
		// like a stale directory, and it may hold a day of uncommitted
		// work. "We could not tell" does not belong in the level whose
		// contract is that nothing which could be work is touched.
		return cleanLevelRank[CleanAggressive]
	}
	// landingNowhere and landingNested are refused before this is
	// reached; treat anything unrecognised as needing more than the
	// highest level rather than defaulting it into a deletion.
	return cleanLevelRank[CleanAggressive] + 1
}

// scanTree walks a candidate once, returning its apparent size and the
// repositories nested inside it.
//
// A plain clone dropped into a worktree — the `.repos/<tool>` convention
// of keeping a dependency's source next to the code that uses it, a
// vendored checkout, a stray `git clone` — is invisible to every question
// asked of the OUTER repository. `git submodule status` says nothing
// about it, and it is normally gitignored, so the tree reads clean. Its
// commits live only in its own object store, under this directory: the
// outer HEAD being merged proves nothing about them, and deleting the
// directory destroys them for good.
//
// Unreadable subtrees contribute what was walked before the error rather
// than sinking the sweep — the size is a report. The nested-repo answer
// is different: it gates a deletion, so a walk that could not complete
// must not read as "none found".
func scanTree(root string) (bytes int64, nested []string, complete bool) {
	complete = true
	ownGit := filepath.Join(root, ".git")
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			complete = false
			return nil //nolint:nilerr // partial walk beats no sweep; `complete` carries the caveat
		}
		if d.Name() == ".git" && p != ownGit {
			nested = append(nested, filepath.Dir(p))
			if d.IsDir() {
				return filepath.SkipDir
			}
			// SkipDir on a non-directory skips the REST of the parent
			// directory, which would silently drop its siblings from the
			// size. The entry itself is a file; there is nothing to skip.
			return nil
		}
		if d.IsDir() {
			// A bare repository has no `.git` at all — it IS the git
			// directory. `git clone --bare`, a mirror, a vendored cache:
			// invisible to a `.git` search, normally gitignored, and its
			// objects exist nowhere else.
			if p != root && p != ownGit && looksLikeGitDir(p) {
				nested = append(nested, p)
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			complete = false
			return nil //nolint:nilerr // same
		}
		bytes += info.Size()
		return nil
	})
	return bytes, nested, complete
}

// looksLikeGitDir reports the on-disk signature of a git directory:
// HEAD alongside objects/ and refs/. That is what `git rev-parse
// --resolve-git-dir` checks, without a process per directory.
func looksLikeGitDir(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return false
	}
	for _, sub := range []string{"objects", "refs"} {
		info, err := os.Stat(filepath.Join(dir, sub))
		if err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

// removeAllForce removes a tree, restoring write permission on the
// directories that refuse it.
//
// A plain os.RemoveAll walks into the tree and only fails when it meets
// the read-only directory — by which point everything before it is
// already gone. The Go module cache is laid down at 0555 by the go tool
// itself, so a run worktree that ever fetched a module contains hundreds
// of them: the sweep would half-destroy a multi-gigabyte tree, report a
// partial deletion, and retry the same wreck on every subsequent run.
// afterEligibility marks the gap between the re-derivation and the
// removal — several git calls and a full tree walk wide, which is ample
// for a concurrent sweep to take the directory. Nothing in production,
// the only place a test can stand to prove the check that follows it is
// load-bearing.
var afterEligibility = func(string) {}

// removeTree is the seam the sweep deletes through. A removal that fails
// for a reason a single uid cannot arrange — another owner's files, a
// busy mount — is exactly the case the continuation contract exists for,
// so the tests substitute it rather than approximate it.
var removeTree = removeAllForce

func removeAllForce(root string) error {
	err := os.RemoveAll(root)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || !d.IsDir() {
			return nil //nolint:nilerr // best effort; the retry below reports what remains
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil //nolint:nilerr // same
		}
		if info.Mode().Perm()&0o200 == 0 {
			_ = os.Chmod(p, info.Mode().Perm()|0o700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// stillEligible re-derives the verdict immediately before a deletion and
// reports whether it still admits one. It is the whole classification
// again, not a subset: any question left unasked is a change the sweep
// waves through.
func stillEligible(wt *CleanedWorktree, level CleanLevel) (string, bool) {
	insp := inspectWorktreeGit(wt.Path)
	if insp.head != wt.head {
		// Something committed, reset, or checked out under us. Whatever
		// the new HEAD is, it is not what was judged.
		return skipUnlanded, false
	}
	switch insp.landing {
	case landingNowhere:
		return skipUnlanded, false
	case landingNested:
		return skipNested, false
	}
	if cleanLevelRank[level] < requiredLevel(insp.landing, insp.dirty) {
		wt.Dirty = insp.dirty
		return skipLevel, false
	}
	if _, nested, complete := scanTree(wt.Path); len(nested) > 0 || !complete {
		wt.NestedRepos = nested
		return skipNested, false
	}
	return "", true
}

// worktreeInspection is what git could be made to say about a directory.
type worktreeInspection struct {
	landing        string
	head           string
	dirty          bool
	containedBy    []string
	commonDir      string
	ignoredEntries int
	// err records why git could not answer, so a refusal caused by a
	// broken environment is visible instead of reading as a clean verdict.
	err error
}

// inspectWorktreeGit reports what git can prove about a worktree.
//
// Every answer is refused unless git is talking about THIS directory.
// Run under a directory merely nested inside some repository — and the
// project-local `<repo>/.iterion/` store layout puts the whole worktree
// pool inside the operator's checkout — git walks up and answers for the
// enclosing repository instead. Its HEAD, its clean status and its refs
// then describe a tree this directory has nothing to do with, which reads
// as a landed, clean worktree and deletes whatever the directory held.
func inspectWorktreeGit(path string) worktreeInspection {
	// "git could not answer" and "git says this is not a worktree" are
	// different facts and must not collapse into the same verdict.
	// `orphan` is deletable at aggressive, so inferring it from a broken
	// environment — git missing from a cron PATH, an unreadable
	// ~/.gitconfig, a git too old for a flag — would turn every worktree
	// in the store into a deletion candidate at once. And "everything is
	// suddenly orphan" is precisely the symptom that pushes an operator
	// to reach for a higher level.
	notARepo := worktreeInspection{landing: landingOrphan, dirty: true}
	cannotTell := worktreeInspection{landing: landingNowhere, dirty: true}

	top, err := gitOut(path, "rev-parse", "--show-toplevel")
	switch {
	case errors.Is(err, errNotARepo):
		return notARepo
	case err != nil:
		cannotTell.err = err
		return cannotTell
	case !samePath(top, path):
		// Git answered for an enclosing repository, not for this
		// directory: it is unknown content, whatever the answer said.
		return notARepo
	}

	head, err := gitOut(path, "rev-parse", "HEAD")
	if err != nil {
		if errors.Is(err, errNotARepo) {
			return notARepo
		}
		// --show-toplevel already answered for THIS directory, so git does
		// know the worktree: an unresolvable HEAD — an unborn branch, a
		// `checkout --orphan` an agent left behind, a dangling symbolic
		// ref — is "we could not tell", not "this is not a worktree". The
		// difference decides whether a tree full of uncommitted work is
		// deletable at aggressive.
		cannotTell.err = err
		return cannotTell
	}

	insp := worktreeInspection{head: head, dirty: worktreeDirty(path)}

	// A repository that lives INSIDE the directory answers containment with
	// refs and objects that are destroyed along with it. `merged` means
	// "reachable independently of what this worktree holds", and a
	// self-contained clone dropped into the pool can never satisfy that
	// however confidently git answers.
	//
	// Not knowing where the common dir is must therefore refuse, not wave
	// through: this is the only thing standing between such a clone and
	// its own destruction, and burying it in an `err == nil` made the
	// guard vanish silently on any git that could not answer.
	common, err := gitCommonDir(path)
	if err != nil {
		insp.landing = landingNowhere
		insp.err = err
		return insp
	}
	insp.commonDir = common
	if isInside(resolvePath(path), resolvePath(common)) {
		insp.landing = landingNested
		return insp
	}
	if n, err := countIgnoredEntries(path); err != nil {
		insp.err = err
	} else {
		insp.ignoredEntries = n
	}

	// An INITIALISED submodule's commits live in the worktree's own
	// administrative directory, which goes when the registration does,
	// and containment in the superproject proves nothing about them.
	//
	// A submodule that was never initialised is a different matter: git
	// prints it with a leading '-', there is no working tree and no
	// object of its own under this directory, so there is nothing to
	// lose. Refusing on those made every worktree of every repository
	// that merely DECLARES a submodule permanently unreclaimable —
	// `git worktree add` never populates submodules, so that is their
	// normal state.
	subs, err := gitOut(path, "submodule", "status")
	if err != nil {
		insp.landing = landingNowhere
		insp.err = err
		return insp
	}
	for _, line := range strings.Split(subs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		insp.landing = landingNested
		return insp
	}

	// Refs are classified by whether they were BUILT UPON this HEAD or
	// merely point at it. A ref whose tip is some other commit and which
	// contains HEAD means another line of work adopted these commits. A
	// ref whose tip IS HEAD is a label — it keeps the commits alive, but
	// nothing has taken them up.
	//
	// Comparing object ids rather than ref names also removes a whole
	// family of misreadings: `symbolic-ref --short` and `for-each-ref
	// %(refname:short)` do not shorten identically, so a name-based "is
	// this my own branch" test misfires on ambiguous names — and in
	// production it never fired at all, because iterion creates its
	// worktrees detached (`git worktree add <path> <sha>`), leaving no
	// symbolic ref to compare against.
	// %(*objectname) is the PEELED id, non-empty only for annotated tags.
	// Without it an annotated tag sitting exactly on this HEAD compares
	// unequal — the tag OBJECT's id is never the commit's — and reads as
	// work built on top, which silently drops an aggressive-only worktree
	// to conservative-deletable.
	refs, err := gitOut(path, "for-each-ref", "--format=%(objectname) %(*objectname) %(refname)",
		"--contains", head, "refs/heads", "refs/remotes", "refs/tags", iterionRefPrefix)
	if err != nil {
		// Without the containment answer we cannot prove the commits are
		// safe, so we must not claim they are.
		insp.landing = landingNowhere
		insp.err = err
		return insp
	}
	labelled := false
	for _, line := range strings.Split(refs, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tip, full := fields[0], fields[len(fields)-1]
		if len(fields) == 3 {
			tip = fields[1] // annotated tag: compare the commit it peels to
			// %(*objectname) dereferences ONE level, so a tag pointing at
			// a tag peels to another tag object and compares unequal
			// again. Resolve it properly; the case is rare enough to
			// afford a process.
			if commit, err := gitOut(path, "rev-parse", full+"^{commit}"); err == nil {
				tip = commit
			}
		}
		labelled = true
		// iterion's own per-run checkpoints keep the commits alive, so
		// they lift the worktree out of `unlanded` — but they are this
		// run's bookkeeping, reaped with the run, never an adoption.
		if tip != head && !strings.HasPrefix(full, iterionRefPrefix) {
			insp.containedBy = append(insp.containedBy, strings.TrimPrefix(full, "refs/heads/"))
		}
	}
	switch {
	case len(insp.containedBy) > 0:
		if len(insp.containedBy) > 3 {
			insp.containedBy = insp.containedBy[:3]
		}
		insp.landing = landingMerged
	case labelled:
		insp.landing = landingOwnBranch
	default:
		insp.landing = landingNowhere
	}
	return insp
}

// resolvePath resolves symlinks, falling back to a lexical clean when the
// path cannot be resolved.
func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// samePath compares two paths through symlinks, so a store reached via a
// symlink does not read as a foreign repository.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return resolvePath(a) == resolvePath(b)
}

// cleanScaffoldPrefix is where iterion mirrors a bundle's skills inside
// the run worktree at run start (pkg/runtime, mirrorBundleSkills). What
// lives there is written BY iterion, not produced by the run, so counting
// it as uncommitted work is counting our own litter — pkg/runtime already
// excludes it for the same reason when deciding whether a run left work
// behind. Measured here: 10 of 25 otherwise-clean merged worktrees were
// held back a whole level by nothing but this directory.
// cleanScaffoldPaths are the destinations iterion writes into a run
// worktree at run start, and only those. `.claude/` wholesale would hide
// anything a run chose to put beside them; a single `.claude/skills/`
// misses the rest of what the mirror lays down — plugin.MirrorKinds
// (skills, commands, agents), the hooks settings file and its sidecar.
//
// pkg/runtime answers the same question for a different purpose
// (deciding whether a run left work behind) and is the reason this list
// has to stay in step with what the mirror actually writes.
var cleanScaffoldDirs = []string{
	".claude/skills/",
	".claude/commands/",
	".claude/agents/",
}

// cleanManagedDirs are the mirror's own bookkeeping — sha256 markers
// beside each mirrored kind, and the hooks sidecar next to
// settings.json. Nobody's work at any status, and rewritten on every run.
//
// They are matched at their exact depth rather than as a segment: a run
// that names a skill `.iterion-managed` would otherwise disappear from
// the tree's dirtiness entirely.
var cleanManagedDirs = []string{
	".claude/.iterion-managed/",
	".claude/skills/.iterion-managed/",
	".claude/commands/.iterion-managed/",
	".claude/agents/.iterion-managed/",
}

// cleanScaffoldFiles are exact paths, never prefixes: iterion rewrites
// settings.json in place, but `.claude/settings.json.orig`, `.bak`, `.rej`
// are a failed merge or an editor's backup — someone's work, and a
// prefix test would have swallowed them.
var cleanScaffoldFiles = []string{".claude/settings.json"}

// renameDestination returns the DEST half of a porcelain rename entry,
// stepping over a quoted source rather than cutting at the first " -> ".
func renameDestination(rest string) string {
	if strings.HasPrefix(rest, `"`) {
		if end := strings.Index(rest[1:], `"`); end >= 0 {
			after := rest[1+end+1:]
			if _, dst, ok := strings.Cut(after, " -> "); ok {
				return dst
			}
			return rest
		}
	}
	if _, dst, ok := strings.Cut(rest, " -> "); ok {
		return dst
	}
	return rest
}

func dequotePath(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 && strings.HasPrefix(p, `"`) && strings.HasSuffix(p, `"`) {
		p = p[1 : len(p)-1]
	}
	return p
}

// isScaffold reports whether a porcelain entry is iterion's own mirror
// rather than the run's work.
//
// The status code is part of the question. A TRACKED file under one of
// these directories came from the repository, and a repository that
// versions its `.claude/` owns what is in it — `.claude/settings.json`
// and `.claude/agents/` are checked-in project configuration in plenty of
// them, and an agent editing one is delivering work.
//
// The trade is deliberate and it costs yield, not safety: the mirror CAN
// rewrite a tracked file it previously wrote (reconcileSkillFile refreshes
// a destination whose marker still matches), so a repository that commits
// the mirror's own output reads dirty after a bundle changes and needs
// `--level moderate`. Reading it the other way costs work instead.
func isScaffold(status, p string) bool {
	for _, dir := range cleanManagedDirs {
		if strings.HasPrefix(p, dir) {
			return true
		}
	}
	if strings.TrimSpace(status) != "??" {
		return false
	}
	for _, exact := range cleanScaffoldFiles {
		if p == exact {
			return true
		}
	}
	for _, dir := range cleanScaffoldDirs {
		if p == strings.TrimSuffix(dir, "/") || strings.HasPrefix(p, dir) {
			return true
		}
	}
	return false
}

// gitCommonDir locates the repository a worktree belongs to, as an
// absolute path.
//
// --path-format=absolute needs git 2.31; on anything older the option is
// rejected, and the bare form answers RELATIVELY (plain `.git` from a
// checkout's root). Falling back and absolutising keeps the guard that
// depends on this answer working there instead of quietly evaporating.
func gitCommonDir(path string) (string, error) {
	if out, err := gitOut(path, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		return out, nil
	}
	out, err := gitOut(path, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(path, out)
	}
	return filepath.Clean(out), nil
}

// isInside reports whether child sits within parent.
func isInside(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func worktreeDirty(path string) bool {
	// --untracked-files=all, because git otherwise folds an untracked
	// directory into a single `.claude/` line and the exclusion could not
	// tell the mirror from anything else the run put beside it.
	out, err := gitOut(path, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		// Unreadable status is treated as dirty: the conservative reading
		// of "we could not tell" is "there may be something here".
		return true
	}
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		status, rest := line[:2], strings.TrimSpace(line[2:])
		// A rename is one line, `XY ORIG -> DEST`. Judging it on ORIG
		// means a file moved OUT of the scaffold takes the whole entry
		// with it, and the destination — real, uncommitted work — is
		// never seen. The source is skipped past first: git quotes a path
		// containing " -> ", and cutting the raw line would land inside
		// the quotes and hand back a fragment of the source as the
		// destination.
		if strings.ContainsAny(status, "RC") {
			rest = renameDestination(rest)
		}
		if p := dequotePath(rest); p != "" && !isScaffold(status, p) {
			return true
		}
	}
	return false
}

// countIgnoredEntries counts gitignored paths present in the tree. They
// are reported, not gated on: in a run worktree they are the build output
// the command exists to reclaim, and gating on them would spare every
// worktree that ever compiled anything.
func countIgnoredEntries(path string) (int, error) {
	out, err := gitOut(path, "status", "--porcelain", "--ignored=matching")
	if err != nil {
		// A count of zero would read as "nothing ignored here", which is a
		// verdict we did not reach.
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "!!") {
			n++
		}
	}
	return n, nil
}

// pruneWorktreeRegistration drops the administrative entry of the one
// worktree just deleted, and only if that entry still names it.
//
// `git worktree prune` would be the obvious call and is the wrong one: it
// sweeps the whole repository and removes the registration of ANY
// worktree whose path is missing at that instant — an operator's checkout
// on an unmounted volume, a stopped container's bind mount. That entry
// holds the worktree's index, so pruning it discards their staged work
// and leaves the checkout unusable when the volume comes back.
// The entry is found by reading each `gitdir` pointer rather than by
// guessing the directory name: git disambiguates colliding basenames with
// a numeric suffix, so two worktrees named after the same run id in two
// stores land in `<id>` and `<id>1` — and the one named `<id>` may well be
// the operator's. Matching on the recorded path is the only way to be
// sure which entry belongs to what was just deleted.
func pruneWorktreeRegistration(commonDir, resolvedPath string) (bool, error) {
	root := filepath.Join(commonDir, "worktrees")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", root, err)
	}
	for _, e := range entries {
		admin := filepath.Join(root, e.Name())
		raw, err := os.ReadFile(filepath.Join(admin, "gitdir")) // #nosec G304 -- under git's own admin dir
		if err != nil {
			continue
		}
		// The pointer names <worktree>/.git; compare the worktree itself.
		// resolvedPath was captured before the deletion, because
		// EvalSymlinks cannot resolve a path that no longer exists and
		// would otherwise never match a store reached through a symlink.
		if filepath.Clean(filepath.Dir(strings.TrimSpace(string(raw)))) != resolvedPath {
			continue
		}
		if err := os.RemoveAll(admin); err != nil {
			return false, fmt.Errorf("remove %s: %w", admin, err)
		}
		return true, nil
	}
	return false, nil
}

// lockRunForClean takes the same per-run advisory lock `iterion run` and
// `iterion resume` hold for a run's lifetime. It is non-blocking: a held
// lock means someone else owns the run right now, which is exactly the
// case where this sweep must keep its hands off.
//
// A worktree with no run id, or a store that cannot be opened, yields no
// lock and no error — the status guards still apply, and refusing to
// sweep a store whose runs directory is merely absent would make the
// command useless on stores pruned by `runs prune`.
func lockRunForClean(storeDir, runID string) (store.RunLock, error) {
	if runID == "" {
		return nil, nil
	}
	s, err := store.New(storeDir)
	if err != nil {
		return nil, nil //nolint:nilerr // no store, no lock to take; the git guards still stand
	}
	if _, err := s.LoadRun(context.Background(), runID); err != nil {
		return nil, nil //nolint:nilerr // no run record: nothing owns this worktree
	}
	lock, err := s.LockRun(context.Background(), runID)
	if err != nil {
		return nil, fmt.Errorf("run %s is locked by another iterion process: %w", runID, err)
	}
	return lock, nil
}

func releaseLock(lock store.RunLock) {
	if lock == nil {
		return
	}
	if err := lock.Unlock(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: releasing run lock: %v\n", err)
	}
}

// errNotARepo is git's own verdict that a directory is not a working
// tree — the only thing that may be read as `orphan`. Every other
// failure means the environment is broken, not that the directory is
// disposable.
var errNotARepo = errors.New("not a git repository")

// preflightGit proves git is usable before a single verdict is formed,
// with the exact prefix the sweep uses. Without it a git missing from a
// cron PATH, an unreadable ~/.gitconfig, or a git too old for a flag
// makes every directory in the store unclassifiable at once — and the
// sweep would report that as a store full of disposable leftovers.
func preflightGit() error {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = os.TempDir()
	}
	if _, err := gitOut(cwd, "version"); err != nil {
		return fmt.Errorf("git is not usable, so no worktree can be classified: %w", err)
	}
	return nil
}

// gitOut runs git in dir and returns trimmed stdout.
func gitOut(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanGitTimeout)
	defer cancel()
	// --no-optional-locks keeps read-only inspection read-only: `git
	// status` otherwise refreshes and rewrites the worktree's index and
	// takes index.lock, so even a dry run would collide with the git of
	// an agent working in that same checkout.
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = dir
	// LC_ALL/LANG pinned so callers may branch on git's own wording, and
	// the environment sanitised so an inherited GIT_DIR cannot redirect
	// the command away from the directory it names.
	cmd.Env = append(gitlib.SanitizeEnv(os.Environ()), "LC_ALL=C", "LANG=C")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("git %s in %s: timed out after %s",
				strings.Join(args, " "), dir, cleanGitTimeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "not a git repository") || strings.Contains(msg, "not a working tree") {
			return "", fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), dir, errNotARepo)
		}
		return "", fmt.Errorf("git %s in %s: %w (stderr: %s)",
			strings.Join(args, " "), dir, err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// loadRunStatuses reads the status of every run in the store, keyed by
// run id.
func loadRunStatuses(storeDir string) map[string]store.RunStatus {
	statuses := map[string]store.RunStatus{}
	s, err := store.New(storeDir)
	if err != nil {
		return statuses
	}
	ctx := context.Background()
	ids, err := s.ListRuns(ctx)
	if err != nil {
		return statuses
	}
	for _, id := range ids {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			// An unreadable run.json must not be read as "no run here":
			// treat it as running so its worktree is spared.
			statuses[id] = store.RunStatusRunning
			continue
		}
		statuses[id] = r.Status
	}
	return statuses
}

// reloadRunStatus re-reads one run's status at deletion time. Reports
// ok=false when the store or the run cannot be opened at all — the scan's
// snapshot then stands, having already refused unreadable records.
func reloadRunStatus(storeDir, runID string) (store.RunStatus, bool) {
	if runID == "" {
		return "", false
	}
	s, err := store.New(storeDir)
	if err != nil {
		return "", false
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		return "", false
	}
	return r.Status, true
}

func deleteRunRecord(storeDir, runID string) error {
	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("open store %s: %w", storeDir, err)
	}
	return s.DeleteRun(context.Background(), runID)
}

// applyKeepLast spares the N most recent worktrees that would otherwise
// be deleted. Entries already spared for another reason do not consume a
// slot — the flag is a floor on what survives, not a quota on the scan.
// Callers pass ONE store's entries, oldest first.
func applyKeepLast(all []CleanedWorktree, keepLast int) {
	if keepLast <= 0 {
		return
	}
	eligible := make([]int, 0, len(all))
	for i := range all {
		if all[i].SkipReason == "" {
			eligible = append(eligible, i)
		}
	}
	if len(eligible) > keepLast {
		eligible = eligible[len(eligible)-keepLast:]
	}
	for _, i := range eligible {
		all[i].SkipReason = skipKeepLast
	}
}

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
