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
	// CleanConservative deletes only what cannot represent work: stale
	// directories that are no longer git worktrees, and worktrees whose
	// HEAD is already reachable from another ref with nothing uncommitted.
	CleanConservative CleanLevel = "conservative"
	// CleanModerate additionally deletes landed worktrees carrying
	// uncommitted files. The commits survive on the other ref; what goes
	// is whatever was never committed — typically build output
	// (node_modules, target/, .devbox), but the operator is asserting it.
	CleanModerate CleanLevel = "moderate"
	// CleanAggressive additionally deletes worktrees whose commits live
	// only on their own branch. Still no commit is lost: the branch is a
	// ref in the parent repository and outlives the worktree directory.
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

// Landing classes. These describe where a worktree's commits live, which
// is the only question that decides whether deleting the directory can
// destroy work.
const (
	// landingOrphan: the directory is not a git worktree at all — its
	// registration is gone, or it never had one. Nothing to lose.
	landingOrphan = "orphan"
	// landingElsewhere: HEAD is contained by a ref other than the
	// worktree's own branch. The work merged, or was promoted onto a
	// persistent branch and then merged.
	landingElsewhere = "landed-elsewhere"
	// landingOwnBranch: HEAD is contained only by the branch this
	// worktree has checked out. Deleting the directory keeps the commits
	// (the ref stays), but nothing else references them yet.
	landingOwnBranch = "own-branch"
	// landingNowhere: detached HEAD with no ref containing it. Deleting
	// the directory makes the commits reachable only via reflog and
	// eligible for GC. Never deleted, at any level.
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
	// ContainedBy names up to a few refs that hold this worktree's HEAD —
	// the evidence for a `landed-elsewhere` verdict, so the operator can
	// check the claim rather than trust it.
	ContainedBy []string `json:"contained_by,omitempty"`
	Deleted     bool     `json:"deleted"`
	// SkipReason is set on spared worktrees and empty on deleted ones.
	SkipReason string `json:"skip_reason,omitempty"`
	// RunDeleted reports whether the paired run record went too (--with-runs).
	RunDeleted bool `json:"run_deleted,omitempty"`
}

// CleanResult is the top-level payload for --json.
type CleanResult struct {
	Stores          []string          `json:"stores"`
	Level           string            `json:"level"`
	OlderThan       string            `json:"older_than"`
	KeepLast        int               `json:"keep_last"`
	DryRun          bool              `json:"dry_run"`
	WithRuns        bool              `json:"with_runs"`
	Scanned         int               `json:"scanned"`
	Deleted         []CleanedWorktree `json:"deleted"`
	Spared          []CleanedWorktree `json:"spared"`
	DeletedCount    int               `json:"deleted_count"`
	BytesReclaimed  int64             `json:"bytes_reclaimed"`
	ReposPruned     []string          `json:"repos_pruned,omitempty"`
	StaleTestStores int               `json:"stale_test_stores,omitempty"`
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

	var all []CleanedWorktree
	for _, storeDir := range stores {
		found, err := scanStoreWorktrees(storeDir, opts, now)
		if err != nil {
			return err
		}
		all = append(all, found...)
	}

	// Oldest first: the operator reads the list top-down and the head of
	// it is what they are least likely to want back.
	sort.Slice(all, func(i, j int) bool { return all[i].ModTime.Before(all[j].ModTime) })

	applyKeepLast(all, opts.KeepLast)

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

	repos := map[string]struct{}{}
	for i := range all {
		wt := &all[i]
		if wt.SkipReason != "" {
			result.Spared = append(result.Spared, *wt)
			continue
		}
		// Resolve the parent repo BEFORE deleting: the pointer that names
		// it lives inside the directory we are about to remove.
		if repo := worktreeRepoRoot(wt.Path); repo != "" {
			repos[repo] = struct{}{}
		}
		if opts.Apply {
			if err := os.RemoveAll(wt.Path); err != nil {
				return fmt.Errorf("delete worktree %s: %w", wt.Path, err)
			}
			wt.Deleted = true
			if opts.WithRuns && wt.RunID != "" {
				if err := deleteRunRecord(wt.StoreDir, wt.RunID); err != nil {
					return fmt.Errorf("delete run %s: %w", wt.RunID, err)
				}
				wt.RunDeleted = true
			}
		}
		result.BytesReclaimed += wt.Bytes
		result.Deleted = append(result.Deleted, *wt)
	}
	result.DeletedCount = len(result.Deleted)

	// Deleting the directory leaves the registration behind; without this
	// the parent repo accumulates administrative entries for worktrees
	// that no longer exist, and `git worktree list` becomes unusable.
	if opts.Apply && len(repos) > 0 {
		for repo := range repos {
			if err := gitWorktreePrune(repo); err != nil {
				fmt.Fprintf(os.Stderr, "warning: git worktree prune in %s: %v\n", repo, err)
				continue
			}
			result.ReposPruned = append(result.ReposPruned, repo)
		}
		sort.Strings(result.ReposPruned)
	}

	return renderCleanResult(p, result)
}

// resolveCleanStores lists the store directories to sweep. Without
// --all-projects that is the single store the working directory maps to;
// with it, every per-project store under the iterion data dir, plus the
// data dir itself when it holds worktrees of its own (the layout left by
// running iterion from $HOME, and by the e2e suite).
func resolveCleanStores(opts CleanOptions) ([]string, error) {
	if !opts.AllProjects {
		cwd, _ := os.Getwd()
		return []string{store.ResolveStoreDir(cwd, opts.StoreDir)}, nil
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
		wt := classifyWorktree(path, e.Name(), storeDir, statuses, opts, now)
		out = append(out, wt)
	}
	return out, nil
}

// classifyWorktree produces the full decision for one worktree
// directory. The guards are ordered by how expensive they are to check
// and how absolute they are: an active run is refused before git is ever
// consulted.
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
	wt.Bytes = dirSize(path)

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

	wt.Landing, wt.Dirty, wt.ContainedBy = inspectWorktreeGit(path)

	// Guard 2 — commits no ref can reach. Deleting the directory would
	// leave them reachable only through the reflog, which expires. No
	// level lifts this; recovering the work is the operator's call, made
	// with git, not a sweep's.
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
	return wt
}

// requiredLevel is the minimum level that admits a given landing class.
func requiredLevel(landing string, dirty bool) int {
	switch landing {
	case landingOrphan:
		return cleanLevelRank[CleanConservative]
	case landingElsewhere:
		if dirty {
			return cleanLevelRank[CleanModerate]
		}
		return cleanLevelRank[CleanConservative]
	case landingOwnBranch:
		return cleanLevelRank[CleanAggressive]
	}
	// landingNowhere is refused before this is reached; treat anything
	// unrecognised as needing more than the highest level rather than
	// defaulting it into a deletion.
	return cleanLevelRank[CleanAggressive] + 1
}

// inspectWorktreeGit reports where a worktree's commits live and whether
// its tree is clean. A directory git refuses to answer for is an orphan:
// its registration is gone, so there is no branch and no index behind it.
func inspectWorktreeGit(path string) (landing string, dirty bool, containedBy []string) {
	if _, err := gitOut(path, "rev-parse", "--git-dir"); err != nil {
		return landingOrphan, false, nil
	}
	head, err := gitOut(path, "rev-parse", "HEAD")
	if err != nil {
		// A worktree that never got a commit (empty repo) has no HEAD to
		// lose. Its only content is whatever is uncommitted.
		return landingOrphan, worktreeDirty(path), nil
	}
	dirty = worktreeDirty(path)

	own, _ := gitOut(path, "symbolic-ref", "--quiet", "--short", "HEAD")

	refs, err := gitOut(path, "for-each-ref", "--format=%(refname:short)",
		"--contains", head, "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		// Without the containment answer we cannot prove the commits are
		// safe, so we must not claim they are.
		return landingNowhere, dirty, nil
	}
	for _, ref := range strings.Split(refs, "\n") {
		ref = strings.TrimSpace(ref)
		if ref == "" || ref == own {
			continue
		}
		containedBy = append(containedBy, ref)
	}
	if len(containedBy) > 0 {
		if len(containedBy) > 3 {
			containedBy = containedBy[:3]
		}
		return landingElsewhere, dirty, containedBy
	}
	if own != "" {
		return landingOwnBranch, dirty, nil
	}
	return landingNowhere, dirty, nil
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

// worktreeRepoRoot resolves the repository a linked worktree belongs to,
// by reading the gitdir pointer its `.git` file carries. Returns "" when
// the pointer is missing or does not have the expected shape.
func worktreeRepoRoot(path string) string {
	out, err := gitOut(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || out == "" {
		return ""
	}
	// <repo>/.git -> <repo>; a bare repo has no worktrees to prune.
	if filepath.Base(out) != ".git" {
		return ""
	}
	return filepath.Dir(out)
}

func gitWorktreePrune(repo string) error {
	_, err := gitOut(repo, "worktree", "prune")
	return err
}

// gitOut runs git in dir and returns trimmed stdout.
func gitOut(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
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
// run id. A store whose runs cannot be listed yields an empty map: the
// git guards still apply, and every worktree with a live run is also a
// worktree git reports as dirty or unlanded.
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
	// `all` is oldest-first, so the tail holds the most recent.
	if len(eligible) <= keepLast {
		for _, i := range eligible {
			all[i].SkipReason = skipKeepLast
		}
		return
	}
	for _, i := range eligible[len(eligible)-keepLast:] {
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
		rows = append(rows, []string{
			shortRunID(wt.RunID),
			landing,
			formatAge(time.Duration(wt.AgeSeconds) * time.Second),
			humanBytes(wt.Bytes),
			wt.Path,
		})
	}
	p.Table([]string{"RUN", "LANDING", "AGE", "SIZE", "PATH"}, rows)
	p.Line("%s %d worktree(s), %s reclaimed (scanned %d in %d store(s))",
		verb, r.DeletedCount, humanBytes(r.BytesReclaimed), r.Scanned, len(r.Stores))
	if r.WithRuns {
		p.Line("run records deleted alongside their worktree (--with-runs)")
	}
	if len(r.ReposPruned) > 0 {
		p.Line("pruned stale worktree registrations in %d repo(s)", len(r.ReposPruned))
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
		parts = append(parts, fmt.Sprintf("%s: %d (%s)", reason, counts[reason], humanBytes(bytes[reason])))
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
