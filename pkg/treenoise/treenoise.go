// Package treenoise is the canonical list of "tree noise": paths the run's
// own setup and tooling write into the workspace, which never count as the
// pass's work — the `.claude/` mirror the engine lays at run start (#1364),
// and `devbox.lock`, rewritten by every devbox invocation the bot makes
// (#1459, #1464).
//
// ONE list, every shape derived. The gates that judge the tree read it as
// git pathspecs (`Pathspecs`), a bot's prompt carries it pre-rendered
// (`ShellPathspecs`, exposed as `{{run.tree_noise}}`), a tool script reads
// it from `ITERION_TREE_NOISE` (`EnvValue`), and the engine's own
// worktree finalization filters porcelain entries through `IsNoise` —
// the list `commit_uncommitted.go` used to carry as a single
// `scaffoldPrefix` constant.
//
// Adding a member is one line in Entries; every shape follows. What
// belongs here is narrow by design: paths the ENGINE or the run's declared
// tooling writes unconditionally, in every workspace. Build droppings a
// specific bot produces (`node_modules`, `go/`) are that bot's own
// exclusions; a file that IS a bot's deliverable (`devbox.json`,
// `devbox.lock` for the dependency bots) is staged by name, past the
// exclusion — the same doctrine `.claude/` has always had.
package treenoise

import "strings"

// TreeNoiseEnvVar is the environment variable tool scripts read the
// pathspecs from (space-separated — every entry is space-free by
// construction, asserted by the tests).
const TreeNoiseEnvVar = "ITERION_TREE_NOISE"

// MirrorPath is the engine's own mirror — the ONE noise path the
// operator-initiated commit-and-finalize keeps excluding. Named rather than
// positional: Entries is documented as growable, and a member prepended
// tomorrow must not silently redefine what a merge-destined commit excludes.
const MirrorPath = ".claude"

// Entry is one tree-noise path: what writes it, and whether it is a
// directory (a prefix match on porcelain paths) or a top-level file (an
// exact one).
type Entry struct {
	Path string
	Dir  bool
	Why  string
}

// Entries is the canonical list, in decision order. Every consumer derives
// its shape from this slice; nothing else in the repository may spell a
// noise path (the guards in bots/ and the tests here enforce that).
var Entries = []Entry{
	{MirrorPath, true, "iterion's skills/commands/agents/settings mirror, written at run start (#1364)"},
	{"devbox.lock", false, "rewritten by every devbox invocation — plugin_version drift (#1459, #1464)"},
}

// Pathspecs returns the list as git pathspec arguments: an exclusion
// anchored at the repository top, so the cwd of the git invocation cannot
// change what it covers. Use with an explicit tree spec (`':/'`) or after
// `--`, on `status`, `diff`, `add` and `stash push`.
func Pathspecs() []string {
	out := make([]string, 0, len(Entries))
	for _, e := range Entries {
		out = append(out, ":(exclude,top)"+e.Path)
	}
	return out
}

// ShellPathspecs renders Pathspecs the way a shell command line wants them:
// each entry single-quoted, space-separated — the form a bot's prompt
// carries ({{run.tree_noise}}) and an agent copies into its Bash call. The
// entries are shell-safe by construction (no quotes, no spaces, no
// metacharacters — asserted by the tests), so the single quotes are
// belt-and-braces, not load-bearing.
func ShellPathspecs() string {
	quoted := make([]string, 0, len(Entries))
	for _, e := range Entries {
		quoted = append(quoted, "':(exclude,top)"+e.Path+"'")
	}
	return strings.Join(quoted, " ")
}

// IsMirror reports whether a `git status --porcelain` path is the engine's
// own mirror — the ONE noise path the operator-initiated commit-and-finalize
// keeps excluding (its commit is merge-destined; a tracked-and-modified
// devbox.lock there is the dependency work itself, verdict 3 R5478b3).
func IsMirror(path string) bool {
	return path == MirrorPath || strings.HasPrefix(path, MirrorPath+"/")
}

// MirrorPathspec is the git pathspec excluding the engine's own mirror —
// the ONE noise path the operator-initiated commit-and-finalize keeps
// excluding when it stages merge-destined work (a tracked-and-modified
// devbox.lock there is the dependency work itself, not engine noise —
// verdict 3, R5478b3). Derived from MirrorPath, the name Entries carries.
func MirrorPathspec() string {
	return ":(exclude,top)" + MirrorPath
}

// EnvValue is the form tool scripts read from ITERION_TREE_NOISE: the
// pathspecs separated by single spaces, split with strings.Fields on the
// consumer side. Every entry is space-free by construction (asserted), so
// the split is lossless.
func EnvValue() string {
	return strings.Join(Pathspecs(), " ")
}

// IsNoise reports whether a `git status --porcelain` path is tree noise.
// The path arrives as runOutputPaths normalizes it: rename arrows cut to
// the destination, outer quotes stripped. A directory entry matches its
// prefix; a file entry matches exactly — git's own semantics for the same
// pathspec (`:(exclude,top).claude` hides a top-level FILE named .claude
// just as well as the directory), so the predicate and the pathspec can
// never disagree about one path.
func IsNoise(path string) bool {
	for _, e := range Entries {
		if e.Dir {
			if path == e.Path || strings.HasPrefix(path, e.Path+"/") {
				return true
			}
			continue
		}
		if path == e.Path {
			return true
		}
	}
	return false
}
