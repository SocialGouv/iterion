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
)

// Skip reasons — why an otherwise-eligible worktree was spared.
const (
	skipRunActive = "run-active"
	skipUnlanded  = "unlanded"
	skipTooRecent = "too-recent"
	skipKeepLast  = "keep-last"
	skipLevel     = "needs-higher-level"
)

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
	// SkipReason is set on spared worktrees and empty on deleted ones.
	SkipReason string `json:"skip_reason,omitempty"`
	// RunDeleted reports whether the paired run record went too (--with-runs).
	RunDeleted bool `json:"run_deleted,omitempty"`
	// Error records a deletion that was attempted and failed. The sweep
	// continues past it; the directory may be partially removed.
	Error string `json:"error,omitempty"`
	// gitCommonDir is captured before deletion so the registration can be
	// dropped afterwards. Not serialised.
	gitCommonDir string
}

// CleanResult is the top-level payload for --json.
type CleanResult struct {
	Stores         []string          `json:"stores"`
	Level          string            `json:"level"`
	OlderThan      string            `json:"older_than"`
	KeepLast       int               `json:"keep_last"`
	DryRun         bool              `json:"dry_run"`
	WithRuns       bool              `json:"with_runs"`
	Scanned        int               `json:"scanned"`
	Deleted        []CleanedWorktree `json:"deleted"`
	Spared         []CleanedWorktree `json:"spared"`
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

		// Re-read the run's status immediately before deleting. The scan
		// is a snapshot, and `iterion resume` reuses a run's existing
		// worktree — a resumed run turns a terminal status back into
		// `running` while the sweep is still walking other directories.
		if st, ok := reloadRunStatus(wt.StoreDir, wt.RunID); ok && !st.IsTerminal() {
			wt.RunStatus = string(st)
			wt.SkipReason = skipRunActive
			result.Spared = append(result.Spared, *wt)
			continue
		}

		if err := os.RemoveAll(wt.Path); err != nil {
			wt.Error = err.Error()
			failures = append(failures, fmt.Errorf("delete worktree %s: %w", wt.Path, err))
			result.Deleted = append(result.Deleted, *wt)
			continue
		}
		wt.Deleted = true
		result.BytesReclaimed += wt.Bytes

		if wt.gitCommonDir != "" {
			if pruned, err := pruneWorktreeRegistration(wt.gitCommonDir, wt.Path); err != nil {
				failures = append(failures, fmt.Errorf("prune registration for %s: %w", wt.Path, err))
			} else if pruned {
				result.RegistrationsPruned++
			}
		}

		if opts.WithRuns && wt.RunID != "" {
			if err := deleteRunRecord(wt.StoreDir, wt.RunID); err != nil {
				wt.Error = err.Error()
				failures = append(failures, fmt.Errorf("delete run %s: %w", wt.RunID, err))
			} else {
				wt.RunDeleted = true
			}
		}
		result.Deleted = append(result.Deleted, *wt)
	}
	result.DeletedCount = len(result.Deleted)
	for _, wt := range result.Deleted {
		if wt.Error != "" {
			result.FailedCount++
		}
	}

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
		storeDir := store.ResolveStoreDir(cwd, opts.StoreDir)
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

	root := store.GlobalIterionDataDir()
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
	wt := CleanedWorktree{RunID: runID, Path: path, StoreDir: storeDir}

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

	// Guard 2 — commits nothing was built upon and no ref even holds.
	// Deleting the directory would leave them reachable only through the
	// reflog, which expires. No level lifts this; recovering the work is
	// the operator's call, made with git, not a sweep's.
	if wt.Landing == landingNowhere {
		wt.SkipReason = skipUnlanded
		return wt
	}

	if opts.OlderThan > 0 && time.Duration(wt.AgeSeconds)*time.Second < opts.OlderThan {
		wt.SkipReason = skipTooRecent
		return wt
	}

	if cleanLevelRank[opts.Level] < requiredLevel(wt.Landing, wt.Dirty) {
		wt.SkipReason = skipLevel
		return wt
	}

	// Only now is the directory a candidate, so only now is it worth
	// walking. Sizing every worktree up front meant stat-ing millions of
	// files — including the live checkout of a running campaign — to
	// produce numbers that fed no decision.
	wt.Bytes = dirSize(path)
	wt.IgnoredEntries = insp.ignoredEntries
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
	// landingNowhere is refused before this is reached; treat anything
	// unrecognised as needing more than the highest level rather than
	// defaulting it into a deletion.
	return cleanLevelRank[CleanAggressive] + 1
}

// worktreeInspection is what git could be made to say about a directory.
type worktreeInspection struct {
	landing        string
	dirty          bool
	containedBy    []string
	commonDir      string
	ignoredEntries int
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
	unknown := worktreeInspection{landing: landingOrphan, dirty: true}

	top, err := gitOut(path, "rev-parse", "--show-toplevel")
	if err != nil || !samePath(top, path) {
		return unknown
	}
	head, err := gitOut(path, "rev-parse", "HEAD")
	if err != nil {
		// A worktree with no commit yet has no HEAD to lose, but it may
		// hold a tree full of uncommitted work; `orphan` already requires
		// the highest level, and dirty stays true.
		return unknown
	}

	insp := worktreeInspection{dirty: worktreeDirty(path)}
	if common, err := gitOut(path, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		insp.commonDir = common
	}
	insp.ignoredEntries = countIgnoredEntries(path)

	// A submodule's commits live in the worktree's own administrative
	// directory, which goes when the registration does. Containment in
	// the superproject proves nothing about them, and iterion has no
	// submodule handling to compensate — so the proof simply does not
	// reach that far, and we must not claim it does.
	if out, err := gitOut(path, "submodule", "status"); err != nil || out != "" {
		insp.landing = landingNowhere
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
	refs, err := gitOut(path, "for-each-ref", "--format=%(objectname) %(refname:short)",
		"--contains", head, "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		// Without the containment answer we cannot prove the commits are
		// safe, so we must not claim they are.
		insp.landing = landingNowhere
		return insp
	}
	labelled := false
	for _, line := range strings.Split(refs, "\n") {
		tip, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || name == "" {
			continue
		}
		labelled = true
		if tip != head {
			insp.containedBy = append(insp.containedBy, name)
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

// samePath compares two paths through symlinks, so a store under a
// symlinked temp dir does not read as a foreign repository.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = filepath.Clean(a)
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = filepath.Clean(b)
	}
	return ra == rb
}

func worktreeDirty(path string) bool {
	out, err := gitOut(path, "status", "--porcelain")
	if err != nil {
		// Unreadable status is treated as dirty: the conservative reading
		// of "we could not tell" is "there may be something here".
		return true
	}
	return out != ""
}

// countIgnoredEntries counts gitignored paths present in the tree. They
// are reported, not gated on: in a run worktree they are the build output
// the command exists to reclaim, and gating on them would spare every
// worktree that ever compiled anything.
func countIgnoredEntries(path string) int {
	out, err := gitOut(path, "status", "--porcelain", "--ignored=matching")
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "!!") {
			n++
		}
	}
	return n
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
func pruneWorktreeRegistration(commonDir, path string) (bool, error) {
	admin := filepath.Join(commonDir, "worktrees", filepath.Base(path))
	raw, err := os.ReadFile(filepath.Join(admin, "gitdir")) // #nosec G304 -- derived from git's own answer
	if err != nil {
		return false, nil // no registration to drop
	}
	if !samePath(filepath.Dir(strings.TrimSpace(string(raw))), path) {
		return false, nil // registration belongs to a different worktree
	}
	if err := os.RemoveAll(admin); err != nil {
		return false, fmt.Errorf("remove %s: %w", admin, err)
	}
	return true, nil
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
		return "", fmt.Errorf("git %s in %s: %w (stderr: %s)",
			strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
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

// dirSize sums the apparent size of every regular file under root.
// Unreadable subtrees contribute what was walked before the error rather
// than sinking the whole sweep — the number is a report, not a decision
// input.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // partial size beats no sweep
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // same
		}
		total += info.Size()
		return nil
	})
	return total
}

func renderCleanResult(p *Printer, r CleanResult) error {
	if p.Format == OutputJSON {
		p.JSON(r)
		return nil
	}

	if r.DeletedCount == 0 {
		p.Line("nothing to clean at level %s (scanned %d worktree(s) in %d store(s))",
			r.Level, r.Scanned, len(r.Stores))
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
		switch {
		case wt.Error != "":
			note = "FAILED (may be partially deleted): " + wt.Path
		case wt.IgnoredEntries > 0:
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
		verb, r.DeletedCount-r.FailedCount, humanBytes(r.BytesReclaimed), r.Scanned, len(r.Stores))
	if r.FailedCount > 0 {
		p.Line("WARNING: %d deletion(s) failed and may have left a partially removed directory", r.FailedCount)
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

// renderCleanSpared summarises what was protected and why. A sweep that
// silently reports only its deletions leaves the operator unable to tell
// "nothing was eligible" from "everything was guarded".
func renderCleanSpared(p *Printer, r CleanResult) {
	if len(r.Spared) == 0 {
		return
	}
	counts := map[string]int{}
	for _, wt := range r.Spared {
		counts[wt.SkipReason]++
	}
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s: %d", reason, counts[reason]))
	}
	p.Line("spared — %s", strings.Join(parts, "; "))
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
