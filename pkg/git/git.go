// Package git is a minimal wrapper around the `git` CLI for the studio's
// modified-files panel. It exposes Status (porcelain → typed entries),
// Diff (HEAD ↔ working-tree contents for one path), and a path validator.
//
// All operations shell out to `git`; `dir` must be an absolute path inside
// a git repository (or worktree). Errors that mean "this directory is not
// a git repository" are flattened to ErrNotGitRepo so callers can render
// a friendly message.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// gitCommandTimeout bounds every `git` subprocess spawned by this
// package. Without it, a wedged git process (stale index.lock, a hung
// smudge/clean filter, an interactive credential prompt on a
// misconfigured remote) blocks the calling goroutine forever — these
// helpers are called directly from studio HTTP handlers, so a hang here
// pins a request goroutine indefinitely instead of surfacing an error.
const gitCommandTimeout = 30 * time.Second

// ErrNotGitRepo is returned by Status/Diff when the target directory is
// not inside a git working tree (no .git, or `git` reports "not a git
// repository"). Callers in the HTTP layer translate it to a 200 with
// `available: false, reason: "not_git_repo"` so the studio can render a
// neutral empty-state instead of a red error.
var ErrNotGitRepo = errors.New("git: not a git repository")

// ErrUnknownRevision is returned when git rejects a revision/range that
// no longer resolves in the repository (branch pruned, base commit
// gc'd, checkout living elsewhere). Callers in the HTTP layer translate
// it — like ErrNotGitRepo — to a 200 with `available: false` so a
// missing history renders as an empty-state, not a 500 carrying raw
// git stderr. Classified inside run(), next to the LC_ALL=C pinning
// that makes the stderr substrings stable.
var ErrUnknownRevision = errors.New("git: unknown revision")

// FileStatus is a single entry in the porcelain output, distilled to one
// effective change per path. The on-disk reality (worktree) wins over the
// index when both columns disagree — the studio cares about "what would I
// see if I opened the file right now" more than the staging state.
//
// Added/Deleted carry the line counts from `git diff --numstat`, merged
// in by Status/StatusBetween so the studio can render Git-Graph-style
// "+N | -N" badges without a second round trip. Binary files set
// Binary=true and Added=Deleted=-1 (sentinel) so the UI shows
// "(binary)" instead of misleading zeros. Both numeric fields are
// always serialized — `(+0 | -0)` is meaningful for pure renames or
// whitespace-only diffs and we don't want JSON omission to hide it.
type FileStatus struct {
	Path    string `json:"path"`
	Status  string `json:"status"`             // "M" | "A" | "D" | "R" | "??"
	OldPath string `json:"old_path,omitempty"` // populated only when Status == "R"
	Added   int    `json:"added"`              // -1 when Binary
	Deleted int    `json:"deleted"`            // -1 when Binary
	Binary  bool   `json:"binary,omitempty"`
	// Lifecycle is a server-side annotation for the studio's "combined"
	// files view: "committed" (the change landed on the run's branch,
	// BaseCommit..HEAD) or "uncommitted" (still pending in the working
	// tree). It is populated ONLY by the server's combined-mode merge
	// (pkg/server/runs_files.go); the git primitives here never set it,
	// and `omitempty` keeps every other mode's wire shape unchanged.
	Lifecycle string `json:"lifecycle,omitempty"`
	// CountsUnknown marks Added/Deleted as UNPOPULATED rather than zero.
	//
	// Needed because the two fields are always serialized, so a producer
	// that cannot compute line counts is indistinguishable from one
	// reporting a genuine +0/-0. iterion's own workspace versioning is
	// such a producer: it stores content, not diffs, and shells out to no
	// git — so every modified text file on an IN-PLACE run (the default
	// run shape) rendered as "modified … +0 −0", i.e. as if nothing in it
	// had changed, on a surface that invites the reviewer to skip files.
	// Set it and the UI omits the counts instead of asserting zeros.
	CountsUnknown bool `json:"counts_unknown,omitempty"`
	// Uncaptured marks a path present in the range whose CONTENT was never
	// stored (over the workspace-versioning size cap), so no diff can be
	// rendered for it. Distinct from Binary, where content exists but is
	// not text. Set only by the workspace backend.
	Uncaptured bool `json:"uncaptured,omitempty"`
}

// DiffPayload carries the two sides of a file diff for the Monaco
// DiffEditor. Both fields are nil (omitted in JSON) when the file does
// not exist on that side: `Before == nil` for untracked/added files,
// `After == nil` for deleted files. Binary files set Binary = true and
// leave Before/After nil — the studio swaps in a "binary file not shown"
// message instead of feeding non-text into Monaco.
//
// Oversized mirrors Binary for files that exceed the diff payload cap on
// either side: Before/After are left nil and Oversized = true, so the
// studio can surface a "file too large to diff" placeholder rather than
// the server reading a multi-GB tracked file entirely into memory on a
// diff click. The size check runs independently for each side before any
// content is read, and takes precedence over binary detection (an
// oversized side is never loaded, so it cannot be NUL-scanned).
// Status is intentionally absent: the caller already has it from the
// prior /files listing and feeds it back as UI metadata. Recomputing it
// here would force a second `git status` scan on every diff click.
type DiffPayload struct {
	Path      string  `json:"path"`
	Before    *string `json:"before"`
	After     *string `json:"after"`
	Binary    bool    `json:"binary"`
	Oversized bool    `json:"oversized,omitempty"`
}

// gitEnv returns an environment that pins git's user-facing messages to
// English (LC_ALL=C / LANG=C). Required because callers branch on
// stderr substrings like "not a git repository" or "exists on disk,
// but not in" — those strings are localized when the user's locale is
// non-English (e.g. fr_FR), and the substring match silently fails.
// gitRedirectionEnv lists the variables that override WHICH repository, index
// or object store a git command acts on. Every exported function in this
// package names its repository through an explicit `dir` argument, so honouring
// these would silently contradict the caller: a `Status(dir)` running under an
// inherited GIT_INDEX_FILE reports dir's whole tree as deleted-and-untracked,
// and under GIT_DIR it answers about another repository entirely.
//
// Git sets them in every hook environment and under `git rebase --exec` /
// `git bisect run`, so any process iterion spawns from one inherits them.
// Identity (GIT_AUTHOR_* / GIT_COMMITTER_*), transport (GIT_SSH_COMMAND,
// GIT_TERMINAL_PROMPT) and GIT_CONFIG_* are deliberately NOT here: the sandbox
// path sets those on purpose so an in-container commit has an identity and
// skips signing.
var gitRedirectionEnv = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
}

// SanitizeEnv returns env without the redirection variables (see
// gitRedirectionEnv). Exported because this package is not the only place that
// shells out to git: anything running a git command against a directory it
// names itself wants the same guarantee, and a caller that assembles its own
// environment cannot get it from run() below.
func SanitizeEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if slices.Contains(gitRedirectionEnv, key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// autoMaintenanceOff is the config that keeps a git command from spawning
// automatic maintenance.
//
// Since git 2.48 a command that WRITES to a repository — fetch, commit, merge,
// rebase, am — ends by running `git maintenance run --auto --detach`, and that
// process DAEMONIZES: it forks, the parent exits, the child calls setsid() and
// closes stdin/stdout/stderr (git's setup.c daemonize()). The parent's exit and
// the closed pipes are exactly what os/exec waits for, so CombinedOutput
// returns while the maintenance child is still alive, still holding
// `<repo>/.git/objects/maintenance.lock` and still writing under
// `.git/objects`. The caller has no handle on it: it cannot be waited for, and
// removing the checkout races it.
//
// `maintenance.auto=false` refuses the spawn outright (git ≥ 2.48).
// `gc.auto=0` is the knob older git reads — there the detaching process is
// `git gc --auto` — and is also the fallback git ≥ 2.48 consults when
// maintenance.auto is unset.
var autoMaintenanceOff = []string{"-c", "maintenance.auto=false", "-c", "gc.auto=0"}

// NoAutoMaintenance returns args prefixed with the config above, for a git
// command run against a directory iterion owns and disposes of: a cache
// checkout, a disposable server-side clone, a scratch repository a test
// builds under t.TempDir(). Such a directory has nothing to maintain, and a
// maintenance process outliving the command that started it is a process
// nothing can wait for.
//
// Exported for the same reason SanitizeEnv is: this package is not the only
// place that shells out to git, and a caller assembling its own argv cannot
// get it from run() below.
func NoAutoMaintenance(args ...string) []string {
	return append(slices.Clone(autoMaintenanceOff), args...)
}

func gitEnv() []string {
	return append(SanitizeEnv(os.Environ()), "LC_ALL=C", "LANG=C")
}

// run executes `git args...` with cwd=dir and returns combined stdout.
// Stderr is captured separately to surface git's own diagnostics in
// wrapped errors. Any error mentioning "not a git repository" is mapped
// to ErrNotGitRepo so the caller can branch on it cleanly.
func run(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("git %s: timed out after %s", strings.Join(args, " "), gitCommandTimeout)
		}
		msg := stderr.String()
		if strings.Contains(msg, "not a git repository") {
			return nil, ErrNotGitRepo
		}
		if strings.Contains(msg, "Invalid revision range") ||
			strings.Contains(msg, "unknown revision") ||
			strings.Contains(msg, "bad revision") {
			return nil, fmt.Errorf("%w: git %s (stderr: %s)", ErrUnknownRevision, strings.Join(args, " "), strings.TrimSpace(msg))
		}
		return nil, fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(msg))
	}
	return out, nil
}

// isGitDir is a fast pre-check so callers can return ErrNotGitRepo
// without spawning a process. It accepts both regular checkouts (.git
// is a directory) and linked worktrees (.git is a file pointing to the
// real gitdir under the parent repo's worktrees/).
func isGitDir(dir string) bool {
	// #nosec G304 G703 — dir is an internally-derived working/repo root (run-state,
	// resolved checkout paths), never raw external request input.
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}
