// Package worktreepool is the safety classifier for the per-run git
// worktrees iterion parks under `<store>/worktrees/`.
//
// It answers one question — what can git PROVE about this directory —
// and it exists as a package because two callers must answer it the
// same way: `iterion clean`, where an operator picks how much they are
// willing to lose, and the runtime pool bound, which reclaims what can
// be taken without losing anything at all. A second, weaker copy of
// these rules is how a sweep eventually deletes someone's work.
package worktreepool

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

type Level string

const (
	// LevelConservative deletes only worktrees whose commits another ref
	// has already built upon, with nothing uncommitted in the tree. Git
	// proves the content is recoverable; nothing else qualifies.
	LevelConservative Level = "conservative"
	// LevelModerate additionally deletes merged worktrees carrying
	// uncommitted files. The commits survive on the other ref; what goes
	// is whatever was never committed.
	LevelModerate Level = "moderate"
	// LevelAggressive additionally deletes worktrees whose commits are
	// held only by a ref pointing at them (no commit is lost — the ref is
	// in the parent repository and outlives the directory) and orphan
	// directories, whose contents git cannot account for at all.
	LevelAggressive Level = "aggressive"
)

// levelRank orders the levels so eligibility is a single comparison.
var levelRank = map[Level]int{
	LevelConservative: 0,
	LevelModerate:     1,
	LevelAggressive:   2,
}

const (
	// LandingOrphan: git cannot account for this directory — it is not a
	// worktree, or the answers git gives belong to some enclosing
	// repository rather than to it. Its contents are unknowable, so it is
	// not conservative-deletable even though it is often pure leftover.
	LandingOrphan = "orphan"
	// LandingMerged: a ref whose tip is NOT this HEAD contains this HEAD.
	// Another line of work was built on top of these commits, so they are
	// reachable independently of anything this worktree holds.
	LandingMerged = "merged"
	// LandingOwnBranch: refs contain HEAD, but every one of them points
	// exactly AT it — they are labels on this commit, not work built upon
	// it. Deleting the directory keeps the commits (the ref stays), but
	// nothing has adopted them yet.
	LandingOwnBranch = "own-branch"
	// LandingNowhere: no ref contains HEAD, or git could not be made to
	// answer. Deleting the directory would leave the commits reachable
	// only via reflog and eligible for GC. Never deleted, at any level.
	LandingNowhere = "unlanded"
	// LandingNested: the tree carries a repository of its own — an
	// initialised submodule, or a plain clone someone dropped inside it.
	// Its objects live under the directory (or under the administrative
	// directory that goes with it) and containment in the outer
	// repository says nothing about them. Never deleted, at any level.
	LandingNested = "nested-repo"
)

// Skip reasons — why an otherwise-eligible worktree was spared.
const (
	SkipRunActive = "run-active"
	SkipUnlanded  = "unlanded"
	SkipNested    = "nested-repo"
	SkipTooRecent = "too-recent"
	SkipKeepLast  = "keep-last"
	SkipLevel     = "needs-higher-level"
	// SkipResumable: the run is terminal to a poller but `iterion resume`
	// restarts it in this very worktree. Sparing it is the default and
	// --include-resumable is the way to say the resume is not wanted —
	// the same opt-in `runs prune` requires for the same statuses.
	SkipResumable = "resumable"
	// SkipVanished: the directory was gone before we reached it, so this
	// sweep neither deleted it nor decided anything about it.
	SkipVanished = "already-gone"
)

// iterionRefPrefix is where iterion persists its own per-run checkpoints
// (pkg/store/turn.go). Those refs hold a run's commits alive, which is
// why they must be consulted — a worktree whose only refs live there was
// being reported as `unlanded`, i.e. unrecoverable, when it was not. But
// they are this run's own bookkeeping, not another line of work adopting
// it, and they are reaped with the run: containment by one of them can
// never mean `merged`.
const iterionRefPrefix = "refs/iterion/"

// Entry is one decision record, shared by the table and --json.
type Entry struct {
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
	// DurablyHeld reports that some ref in the PARENT repository holds
	// this HEAD, so the commits survive the directory. It is not a
	// verdict and it does not move an entry between levels; it is the one
	// extra fact the pool bound needs, and it is reported so an operator
	// reading --json can see why the bound took something conservative
	// spares.
	DurablyHeld bool `json:"durably_held,omitempty"`
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

// gitTimeout bounds every git invocation the sweep makes. A wedged
// git (stale index.lock, hung filter) must not hang a maintenance
// command that operators wire into cron.
const gitTimeout = 30 * time.Second

func scanStore(storeDir string, opts ScanOptions, now func() time.Time) ([]Entry, error) {
	wtRoot := filepath.Join(storeDir, "worktrees")
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read worktrees dir %s: %w", wtRoot, err)
	}

	statuses := loadRunStatuses(storeDir)

	out := make([]Entry, 0, len(entries))
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
		out = append(out, classify(path, e.Name(), storeDir, statuses, opts, now))
	}
	return out, nil
}

// classify produces the full decision for one worktree
// directory. The guards are ordered by how absolute they are and how
// expensive they are to check: an active run is refused before git is
// ever consulted, and the directory is only measured once it is a real
// candidate.
func classify(
	path, runID, storeDir string,
	statuses map[string]store.RunStatus,
	opts ScanOptions,
	now func() time.Time,
) Entry {
	wt := Entry{RunID: runID, Path: path, StoreDir: storeDir, resolvedPath: resolvePath(path)}
	resumable := false

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
		if ownsWorktree(st) {
			wt.SkipReason = SkipRunActive
			return wt
		}
		resumable = isResumable(st) && !opts.IncludeResumable
	}

	insp := inspectGit(path)
	wt.Landing, wt.Dirty, wt.ContainedBy = insp.landing, insp.dirty, insp.containedBy
	wt.DurablyHeld = insp.durablyHeld
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
	if wt.Landing == LandingNowhere {
		wt.SkipReason = SkipUnlanded
	} else if wt.Landing == LandingNested {
		// Guard 3 — a repository inside the tree, whose objects the outer
		// repository cannot vouch for.
		wt.SkipReason = SkipNested
	} else if opts.OlderThan > 0 && time.Duration(wt.AgeSeconds)*time.Second < opts.OlderThan {
		wt.SkipReason = SkipTooRecent
	} else if reason, ok := opts.admission()(wt.Landing, wt.Dirty, wt.DurablyHeld); !ok {
		wt.SkipReason = reason
	} else if resumable {
		// Only where the entry would otherwise be taken at THIS level:
		// reporting `resumable` for something the level ladder also holds
		// back would promise a gain --include-resumable cannot deliver
		// on its own.
		wt.SkipReason = SkipResumable
	}

	// Measuring costs a walk of every file, so it is spent only where the
	// number changes a decision: on candidates, and on what only the
	// level ladder is holding back — an operator told what this level
	// yields cannot otherwise see that the next frees ten times more.
	// Nothing else is walked. In particular never the live checkout of a
	// running campaign, whose size would be an incoherent snapshot taken
	// in competition with the run's own I/O.
	if wt.SkipReason != "" && (!opts.MeasureSpared ||
		(wt.SkipReason != SkipLevel && wt.SkipReason != SkipResumable)) {
		return wt
	}
	bytes, nested, complete := scanTree(path)
	wt.Bytes = bytes
	if len(nested) > 0 {
		wt.Landing, wt.SkipReason, wt.Bytes = LandingNested, SkipNested, 0
		wt.NestedRepos = nested
		if len(wt.NestedRepos) > 3 {
			wt.NestedRepos = wt.NestedRepos[:3]
		}
	} else if !complete && wt.SkipReason == "" {
		// The walk is what proves there is no repository hiding in
		// there. A walk that could not finish has not proved it.
		wt.Landing, wt.SkipReason = LandingNested, SkipNested
		wt.Error = "tree could not be fully read; refusing to claim it holds no nested repository"
	}
	return wt
}

// requiredLevel is the minimum level that admits a given landing class.
func requiredLevel(landing string, dirty bool) int {
	switch landing {
	case LandingMerged:
		if dirty {
			return levelRank[LevelModerate]
		}
		return levelRank[LevelConservative]
	case LandingOwnBranch, LandingOrphan:
		// An orphan is usually pure leftover, but git cannot say what is
		// in it — a checkout whose parent repository moved looks exactly
		// like a stale directory, and it may hold a day of uncommitted
		// work. "We could not tell" does not belong in the level whose
		// contract is that nothing which could be work is touched.
		return levelRank[LevelAggressive]
	}
	// LandingNowhere and LandingNested are refused before this is
	// reached; treat anything unrecognised as needing more than the
	// highest level rather than defaulting it into a deletion.
	return levelRank[LevelAggressive] + 1
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
// The re-derivation and the removal are separated by several git calls
// and a full tree walk — ample for a concurrent sweep to take the
// directory. Two different checks cover the two halves of that gap, and
// each needs a place a test can stand to prove it is load-bearing. Both

func stillEligible(wt *Entry, admit admission, during func(string)) (string, bool) {
	during(wt.Path)
	insp := inspectGit(wt.Path)
	if insp.head != wt.head {
		// Something committed, reset, or checked out under us. Whatever
		// the new HEAD is, it is not what was judged.
		return SkipUnlanded, false
	}
	switch insp.landing {
	case LandingNowhere:
		return SkipUnlanded, false
	case LandingNested:
		return SkipNested, false
	}
	if reason, ok := admit(insp.landing, insp.dirty, insp.durablyHeld); !ok {
		wt.Dirty = insp.dirty
		return reason, false
	}
	if _, nested, complete := scanTree(wt.Path); len(nested) > 0 || !complete {
		wt.NestedRepos = nested
		return SkipNested, false
	}
	return "", true
}

// inspection is what git could be made to say about a directory.

type inspection struct {
	landing        string
	head           string
	dirty          bool
	containedBy    []string
	commonDir      string
	ignoredEntries int
	// durablyHeld records that a ref OUTSIDE iterion's own per-run
	// namespace contains this HEAD. Those refs are reaped with the run,
	// so a worktree they alone hold loses its commits when the run goes;
	// any other ref lives in the parent repository and outlives the
	// directory entirely.
	durablyHeld bool
	// err records why git could not answer, so a refusal caused by a
	// broken environment is visible instead of reading as a clean verdict.
	err error
}

// inspectGit reports what git can prove about a worktree.
//
// Every answer is refused unless git is talking about THIS directory.
// Run under a directory merely nested inside some repository — and the
// project-local `<repo>/.iterion/` store layout puts the whole worktree
// pool inside the operator's checkout — git walks up and answers for the
// enclosing repository instead. Its HEAD, its clean status and its refs
// then describe a tree this directory has nothing to do with, which reads
// as a landed, clean worktree and deletes whatever the directory held.
func inspectGit(path string) inspection {
	// "git could not answer" and "git says this is not a worktree" are
	// different facts and must not collapse into the same verdict.
	// `orphan` is deletable at aggressive, so inferring it from a broken
	// environment — git missing from a cron PATH, an unreadable
	// ~/.gitconfig, a git too old for a flag — would turn every worktree
	// in the store into a deletion candidate at once. And "everything is
	// suddenly orphan" is precisely the symptom that pushes an operator
	// to reach for a higher level.
	notARepo := inspection{landing: LandingOrphan, dirty: true}
	cannotTell := inspection{landing: LandingNowhere, dirty: true}

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

	insp := inspection{head: head, dirty: worktreeDirty(path)}

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
		insp.landing = LandingNowhere
		insp.err = err
		return insp
	}
	insp.commonDir = common
	if isInside(resolvePath(path), resolvePath(common)) {
		insp.landing = LandingNested
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
		insp.landing = LandingNowhere
		insp.err = err
		return insp
	}
	for _, line := range strings.Split(subs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		insp.landing = LandingNested
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
		insp.landing = LandingNowhere
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
		if !strings.HasPrefix(full, iterionRefPrefix) {
			// A ref that outlives the run holds these commits whether it
			// was built on top of them or merely labels them. That is a
			// weaker claim than `merged` and a different one: it says
			// nothing was ADOPTED, only that nothing is LOST. The pool
			// bound is the caller that needs exactly that distinction.
			insp.durablyHeld = true
			if tip != head {
				insp.containedBy = append(insp.containedBy, strings.TrimPrefix(full, "refs/heads/"))
			}
		}
	}
	switch {
	case len(insp.containedBy) > 0:
		if len(insp.containedBy) > 3 {
			insp.containedBy = insp.containedBy[:3]
		}
		insp.landing = LandingMerged
	case labelled:
		insp.landing = LandingOwnBranch
	default:
		insp.landing = LandingNowhere
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

var scaffoldDirs = []string{
	".claude/skills/",
	".claude/commands/",
	".claude/agents/",
}

// managedDirs are the mirror's own bookkeeping — sha256 markers
// beside each mirrored kind, and the hooks sidecar next to
// settings.json. Nobody's work at any status, and rewritten on every run.
//
// They are matched at their exact depth rather than as a segment: a run
// that names a skill `.iterion-managed` would otherwise disappear from
// the tree's dirtiness entirely.
var managedDirs = []string{
	".claude/.iterion-managed/",
	".claude/skills/.iterion-managed/",
	".claude/commands/.iterion-managed/",
	".claude/agents/.iterion-managed/",
}

// scaffoldFiles are exact paths, never prefixes: iterion rewrites
// settings.json in place, but `.claude/settings.json.orig`, `.bak`, `.rej`
// are a failed merge or an editor's backup — someone's work, and a
// prefix test would have swallowed them.
var scaffoldFiles = []string{".claude/settings.json"}

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
	for _, dir := range managedDirs {
		if strings.HasPrefix(p, dir) {
			return true
		}
	}
	if strings.TrimSpace(status) != "??" {
		return false
	}
	for _, exact := range scaffoldFiles {
		if p == exact {
			return true
		}
	}
	for _, dir := range scaffoldDirs {
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

// ownsWorktree reports whether anything still lays claim to a run's
// checkout.
//
// This is deliberately NOT `!IsTerminal()`. That predicate answers a
// different question — whether a poller should stop refreshing — and its
// own documentation says `failed_resumable` is terminal "even though the
// run can be resumed". `iterion resume` accepts failed_resumable,
// cancelled and paused_operator; a run it will restart still owns its
// worktree, and sweeping it destroys the resume while every other guard
// nods along, because the commits are merged and nothing is lost except
// the ability to continue.

func ownsWorktree(st store.RunStatus) bool {
	return !st.IsTerminal()
}

// isResumable reports whether `iterion resume` would restart this run in
// its existing worktree. These statuses are terminal to a poller, so the
// ordinary guard lets them through — and on a real store they are the
// bulk of what accumulates, so refusing them outright leaves nothing to
// reclaim. Spared by default, released by --include-resumable, which is
// the same opt-in `runs prune` requires before it touches them.
func isResumable(st store.RunStatus) bool {
	switch st {
	case store.RunStatusFailedResumable, store.RunStatusCancelled, store.RunStatusPausedOperator:
		return true
	}
	return false
}

// lockRun takes the same per-run advisory lock `iterion run` and
// `iterion resume` hold for a run's lifetime. It is non-blocking: a held
// lock means someone else owns the run right now, which is exactly the
// case where this sweep must keep its hands off.
//
// A worktree with no run id, or a store that cannot be opened, yields no
// lock and no error — the status guards still apply, and refusing to
// sweep a store whose runs directory is merely absent would make the
// command useless on stores pruned by `runs prune`.
func lockRun(storeDir, runID string) (store.RunLock, error) {
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

// PreflightGit proves git is usable before a single verdict is formed,
// with the exact prefix the sweep uses. Without it a git missing from a
// cron PATH, an unreadable ~/.gitconfig, or a git too old for a flag
// makes every directory in the store unclassifiable at once — and the
// sweep would report that as a store full of disposable leftovers.
func PreflightGit() error {
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
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
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
				strings.Join(args, " "), dir, gitTimeout)
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
	// store.New provisions the store — it creates runs/ and drops a
	// .gitignore. Inspecting must not do that: a dry run would mutate its
	// target, and runs/ is one of the markers that promote a stray
	// directory to a managed store for every later command.
	if _, err := os.Stat(filepath.Join(storeDir, "runs")); err != nil {
		return statuses
	}
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

// deleteRunRecord removes a run's record, and reports whether there was
// one. DeleteRun creates the run directory before writing its deletion
// marker, so calling it for a worktree that never had a run — an orphan
// directory, a run already pruned — leaves a durable tombstone behind
// that makes every later write to that id fail, and reports a record as
// deleted that never existed.
func deleteRunRecord(storeDir, runID string) (bool, error) {
	s, err := store.New(storeDir)
	if err != nil {
		return false, fmt.Errorf("open store %s: %w", storeDir, err)
	}
	ctx := context.Background()
	if _, err := s.LoadRun(ctx, runID); err != nil {
		return false, nil
	}
	return true, s.DeleteRun(ctx, runID)
}

// ApplyKeepLast spares the N most recent worktrees that would otherwise
// be deleted. Entries already spared for another reason do not consume a
// slot — the flag is a floor on what survives, not a quota on the scan.
// Callers pass ONE store's entries, oldest first.
func ApplyKeepLast(all []Entry, keepLast int) {
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
		all[i].SkipReason = SkipKeepLast
	}
}

// RemoveAllForce removes a tree, restoring write permission on the
// directories that refuse it.
//
// A plain os.RemoveAll walks into the tree and only fails when it meets
// the read-only directory — by which point everything before it is
// already gone. The Go module cache is laid down at 0555 by the go tool
// itself, so a run worktree that ever fetched a module contains hundreds
// of them: the sweep would half-destroy a multi-gigabyte tree, report a
// partial deletion, and retry the same wreck on every subsequent run.
func RemoveAllForce(root string) error {
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

// HasWorktreeDir reports whether a store directory holds a worktree pool
// at all. Callers use it to skip stores that can yield nothing.
func HasWorktreeDir(storeDir string) bool {
	info, err := os.Stat(filepath.Join(storeDir, "worktrees"))
	return err == nil && info.IsDir()
}

// PoolDir is where a store parks its per-run worktrees.
func PoolDir(storeDir string) string {
	return filepath.Join(storeDir, "worktrees")
}

// KnownLevel reports whether a level is one the ladder defines. Callers
// validate operator input with it rather than reaching into the rank map.
func KnownLevel(l Level) bool {
	_, ok := levelRank[l]
	return ok
}

// AbsPath makes a store path absolute.
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
func AbsPath(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}
