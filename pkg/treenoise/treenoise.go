// Package treenoise is the canonical list of "tree noise": paths the run's
// own setup and tooling write into the workspace, which never count as the
// pass's work — the `.claude/` mirror the engine lays at run start (#1364),
// `devbox.lock`, rewritten by every devbox invocation the bot makes
// (#1459, #1464), and the tool-node script scratch `.iterion-script-*` a
// hard kill can leave behind (#1464).
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

// Entry is one tree-noise path: what writes it, and how it matches. A
// plain entry covers the exact path and everything under it — the same
// leading-path semantics its git pathspec carries. A Prefix entry ends in
// a literal `*`, kept as-is by the pathspec shapes and matched as a raw
// prefix by IsNoise — how the list swallows a generated-by-pattern class
// without pretending it is one directory.
type Entry struct {
	Path   string
	Prefix bool
	Why    string
}

// Pathspec is the entry as ONE git pathspec argument: an exclusion anchored
// at the repository top, the wildcard appended for a Prefix entry. It is
// the unit Pathspecs and ShellPathspecs are built from, and the shape a
// consumer filtering the list entry by entry (the engine's staging
// gestures) spells.
func (e Entry) Pathspec() string {
	p := e.Path
	if e.Prefix {
		p += "*"
	}
	return ":(exclude,top)" + p
}

// mirrorEntry is the list member for MirrorPath — MirrorEntry returns it,
// Entries carries it first.
var mirrorEntry = Entry{MirrorPath, false, "iterion's skills/commands/agents/settings mirror, written at run start (#1364)"}

// Entries is the canonical list, in decision order. Every consumer derives
// its shape from this slice; nothing else in the repository may spell a
// noise path (the guards in bots/ and the tests here enforce that).
var Entries = []Entry{
	mirrorEntry,
	{"devbox.lock", false, "rewritten by every devbox invocation — plugin_version drift (#1459, #1464)"},
	{".iterion-script-", true, "tool-node script scratch (.iterion-script-<random>.<ext>, executor_tool.go): the cleanup loses the race against a hard kill and the file then reads as uncommitted work"},
}

// Pathspecs returns the list as git pathspec arguments: an exclusion
// anchored at the repository top, so the cwd of the git invocation cannot
// change what it covers. Use with an explicit tree spec (`':/'`) or after
// `--`, on `status`, `diff`, `add` and `stash push`.
func Pathspecs() []string {
	out := make([]string, 0, len(Entries))
	for _, e := range Entries {
		out = append(out, e.Pathspec())
	}
	return out
}

// ShellPathspecs renders Pathspecs the way a shell command line wants them:
// each entry single-quoted, space-separated — the form a bot's prompt
// carries ({{run.tree_noise}}) and an agent copies into its Bash call. The
// entries carry no quotes, spaces, or shell-significant characters beyond
// the wildcard a Prefix entry ends in — asserted by the tests — and the
// single quotes keep that wildcard out of the caller's shell: for a Prefix
// entry they are load-bearing, not decoration.
func ShellPathspecs() string {
	quoted := make([]string, 0, len(Entries))
	for _, e := range Entries {
		quoted = append(quoted, "'"+e.Pathspec()+"'")
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

// MirrorEntry is the engine's own mirror as a list member — the ONE entry
// the operator-initiated commit-and-finalize keeps excluding when it stages
// merge-destined work (a tracked-and-modified devbox.lock there is the
// dependency work itself, not engine noise — verdict 3, R5478b3; a leftover
// `.iterion-script-*` scratch file rides that commit too, the same
// deliberate disagreement, by mirror-only design). Named rather than read
// out of Entries by position: a member prepended tomorrow must not
// silently redefine what a merge-destined commit excludes.
func MirrorEntry() Entry {
	return mirrorEntry
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
// the destination, outer quotes stripped. Every plain entry agrees with
// its pathspec on every path — git's own semantics hide a top-level FILE
// named `.claude` just as well as the directory, AND a directory named
// `devbox.lock` just as well as the file — so the predicate matches the
// exact path and anything under it either way. A Prefix entry matches a
// raw prefix.
func IsNoise(path string) bool {
	for _, e := range Entries {
		if e.Prefix {
			if strings.HasPrefix(path, e.Path) {
				return true
			}
			continue
		}
		if path == e.Path || strings.HasPrefix(path, e.Path+"/") {
			return true
		}
	}
	return false
}
