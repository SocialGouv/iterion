package workspacetrack

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// alwaysIgnored are directories never worth versioning, whatever the
// project declares. `.git` is the repository's own database (capturing it
// would be both enormous and meaningless), `.iterion` is the run store
// itself — capturing it from inside a run would have the tracker record
// its own objects, growing without bound.
//
// Matching the store BY NAME is not sufficient on its own: store.
// ResolveStoreDir returns an explicit --store-dir verbatim, so a store
// living in the workspace under any other name is invisible here. That is
// what ExcludeRoot covers — see its doc.
var alwaysIgnored = map[string]bool{
	".git":     true,
	".iterion": true,
}

// defaultIgnored are heavy, regenerable directories skipped unless the
// project says otherwise. They are dependency and build output: hashing
// them on every node boundary would dominate the cost of a run.
//
// Deliberately NOT including `vendor/`: in Go projects it is committed
// source that a bot may legitimately edit.
var defaultIgnored = []string{
	"node_modules",
	".venv",
	"__pycache__",
	".mypy_cache",
	".pytest_cache",
	".next",
	".nuxt",
	".turbo",
	".gradle",
	".terraform",
}

// rule is one line of an ignore file.
type rule struct {
	// segments is the pattern split on "/", with "**" kept as a segment.
	segments []string
	negated  bool
	// anchored patterns ("/build", "docs/build") match from the workspace
	// root; a bare name ("node_modules") matches at any depth.
	anchored bool
	dirOnly  bool
	// segLiteral[i] is true when segments[i] carries no glob
	// metacharacters. Computed ONCE at parse: deriving it per call meant a
	// strings.ContainsAny on every segment of every rule for every path,
	// which the CPU profile showed as 25% of the matcher (IndexAny +
	// makeASCIISet), with filepath.Match another 60% on segments that are
	// plain strings.
	segLiteral []bool
	// rootLiteral is segments[0] when the rule is ANCHORED and that
	// segment is a plain literal. Every candidate — the path and each of
	// its ancestor prefixes — shares parts[0], so one string compare then
	// rejects the rule for all of them at once. This is what keeps the
	// per-path cost flat as the rule set grows: a repo's ignore file is
	// mostly anchored rules naming different top-level directories.
	rootLiteral string
}

// Ignorer decides which workspace paths are versioned.
//
// The rule set is iterion's own — a project states it in `.iterionignore`,
// falling back to `.gitignore` when that file is absent. The fallback is
// pragmatic (in a repository, what git is told to skip is usually what we
// should skip too) but the precedence is the point: `.gitignore` states a
// PACKAGING policy, and it should not by itself decide what a rewind can
// undo. A project that writes `.iterionignore` takes full control — the
// git file is then not read at all.
//
// # Divergence from gitignore, on purpose
//
// git cannot re-include a file whose parent directory is excluded. That
// makes an allowlist inexpressible, which is exactly the shape a media
// pipeline needs: "none of runs/, except the delivered mp4s". Here a
// negation wins regardless of any parent exclusion, so
//
//	runs/**
//	!runs/**/export/*.mp4
//
// means what it looks like it means. The cost is that an excluded
// directory can no longer be pruned from the walk when negations exist —
// see Ignorer.CanPrune.
type Ignorer struct {
	rules     []rule
	protected map[string]bool // workspace-relative, exact match
	hasNegate bool
	// alwaysPrefixes are workspace-relative roots excluded STRUCTURALLY —
	// the same class as alwaysIgnored, but discovered at runtime rather
	// than by name (the store dir, wherever the operator put it). No rule
	// re-includes anything beneath them and they stay prunable.
	alwaysPrefixes []string
}

// Protect marks absolute paths as untouchable for this Ignorer. Used by a
// restore for files that live in the workspace but must survive it — the
// workflow source above all: a rewind exists to test an edit to that
// file, so reverting it would undo the very change under test and the
// following resume would recompile the old workflow.
//
// Paths outside the workspace are ignored (they cannot be matched).
func (ig *Ignorer) Protect(workspaceDir string, paths ...string) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(workspaceDir, abs)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		if ig.protected == nil {
			ig.protected = map[string]bool{}
		}
		ig.protected[filepath.ToSlash(rel)] = true
	}
}

// NewIgnorer builds the rule set for a workspace.
func NewIgnorer(workspaceDir string) *Ignorer {
	ig := &Ignorer{}
	for _, name := range defaultIgnored {
		ig.rules = append(ig.rules, parseRule(name))
	}
	for _, name := range []string{".iterionignore", ".gitignore"} {
		lines, ok := readIgnoreFile(filepath.Join(workspaceDir, name))
		if ok {
			for _, l := range lines {
				r := parseRule(l)
				ig.rules = append(ig.rules, r)
				if r.negated {
					ig.hasNegate = true
				}
			}
			// `.iterionignore` wins outright: a project that states
			// iterion's rules is not also asking for git's.
			break
		}
	}
	return ig
}

// ExcludeRoot marks an absolute path as always-ignored when it resolves
// INSIDE the workspace, in the same class as `.git`/`.iterion`: no rule,
// negated or not, can re-include anything beneath it, and it stays
// prunable.
//
// Excluding the store by NAME alone was a hole with teeth. `--store-dir
// "$PWD/mystore"` (or any studio/dogfood invocation not using the exact
// name `.iterion`) puts the object pool inside the workspace, and then
// two things go wrong: every boundary captures the PREVIOUS boundary's
// objects and manifests as new content, so the pool compounds per node
// instead of holding flat; and Restore, which deletes whatever the target
// snapshot lacks, deletes pool objects and manifests written later —
// content that other snapshots, and other runs (the pool is store-global)
// still reference. The damage surfaces much later, as an unrelated
// restore failing with "object … is unavailable".
//
// A no-op when the path is outside the workspace, which is the normal
// arrangement.
func (ig *Ignorer) ExcludeRoot(workspaceDir, root string) {
	rel, ok := relativeInside(workspaceDir, root)
	if !ok {
		return
	}
	ig.alwaysPrefixes = append(ig.alwaysPrefixes, rel)
}

// relativeInside returns the slash-separated path of `target` relative to
// `base` when it sits inside it. Symlinks are resolved on both sides so a
// store reached through a link is still recognised.
func relativeInside(base, target string) (string, bool) {
	if base == "" || target == "" {
		return "", false
	}
	resolve := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			return ""
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			return real
		}
		return abs // not created yet — the lexical form is the best we have
	}
	b, t := resolve(base), resolve(target)
	if b == "" || t == "" || b == t {
		return "", false
	}
	rel, err := filepath.Rel(b, t)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// underAlwaysPrefix reports whether rel is one of the structurally
// excluded roots, or lives beneath one.
func (ig *Ignorer) underAlwaysPrefix(rel string) bool {
	for _, p := range ig.alwaysPrefixes {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

func parseRule(line string) rule {
	r := rule{}
	if strings.HasPrefix(line, "!") {
		r.negated = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if strings.HasPrefix(line, "/") {
		r.anchored = true
		line = strings.TrimPrefix(line, "/")
	} else if strings.Contains(line, "/") {
		// A pattern with an interior separator is anchored, as in git.
		r.anchored = true
	}
	r.segments = strings.Split(line, "/")
	r.segLiteral = make([]bool, len(r.segments))
	for i, seg := range r.segments {
		r.segLiteral[i] = seg != "**" && !strings.ContainsAny(seg, "*?[\\")
	}
	if r.anchored && len(r.segments) > 0 && r.segments[0] != "**" &&
		!strings.ContainsAny(r.segments[0], "*?[\\") {
		r.rootLiteral = r.segments[0]
	}
	return r
}

func readIgnoreFile(path string) ([]string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, true
}

// Match reports whether a workspace-relative, slash-separated path is
// excluded. Rules apply in file order and the LAST one to match wins, so
// a negation placed after a broad exclusion re-includes.
func (ig *Ignorer) Match(rel string, isDir bool) bool {
	if rel == "" || rel == "." {
		return false
	}
	if ig.protected[rel] {
		return true
	}
	// Split ONCE and pass the segments down. The matcher used to re-derive
	// the path for every rule and every ancestor prefix (Split, then Join,
	// then Split again inside matchPath), which is O(depth²) string work
	// per rule: measured at 667 µs and 1129 allocs for one depth-7 path
	// against this repo's own rule set, turning a 130 ms walk into 7 s —
	// 50x the per-boundary budget, on a path that is ON by default.
	parts := strings.Split(rel, "/")
	for _, seg := range parts {
		if alwaysIgnored[seg] {
			return true
		}
	}
	if ig.underAlwaysPrefix(rel) {
		return true
	}
	// LAST match wins, so scanning in reverse and stopping at the first
	// hit picks the same rule — and lets the common case exit early
	// instead of evaluating every rule for every path.
	for i := len(ig.rules) - 1; i >= 0; i-- {
		if ig.rules[i].matches(parts, isDir) {
			return !ig.rules[i].negated
		}
	}
	return false
}

// CanPrune reports whether ANY excluded directory can be skipped whole.
//
// With no negations, an excluded directory holds nothing we want and the
// walk can stop there. As soon as one negation exists, something inside
// may be re-included, so the walk has to descend — the cost of making an
// allowlist expressible. Stat-only descent, no hashing, so it is cheap
// next to what it enables.
//
// Prefer CanPruneDir, which answers the same question for one directory
// and can say yes where this has to say no.
func (ig *Ignorer) CanPrune() bool { return !ig.hasNegate }

// CanPruneDir reports whether THIS excluded directory can be skipped
// whole — the per-directory refinement of CanPrune.
//
// alwaysIgnored directories are the case CanPrune gets wrong: Match
// short-circuits on those segments BEFORE the rule loop, so no negation
// can ever re-include anything beneath them, and descending is pure
// waste. Waste that grows, at that: in a managed store the object pool
// lives at <workspace>/.iterion/workspace-objects/, so every boundary
// walk would stat everything the run has ever captured, and the
// per-boundary cost climbs with each capture instead of holding flat.
//
// This is not a corner case — one negation anywhere is enough to trip it,
// and this repo's own .gitignore carries five, so iterion self-hosting
// was squarely in it.
func (ig *Ignorer) CanPruneDir(rel string) bool {
	if !ig.hasNegate {
		return true
	}
	for _, seg := range strings.Split(rel, "/") {
		if alwaysIgnored[seg] {
			return true
		}
	}
	// Structurally excluded roots (the store dir) are as safe to prune as
	// .git: Match short-circuits on them before the rule loop, so nothing
	// under them can be re-included.
	if ig.underAlwaysPrefix(rel) {
		return true
	}
	// Otherwise: prunable when no negated rule COULD re-include anything
	// beneath this directory.
	//
	// The blanket "any negation anywhere ⇒ descend everywhere" rule made
	// the trip condition the norm rather than the corner — this repo's own
	// .gitignore carries five negations — and the cost lands on exactly
	// the directories the default ignore list exists to skip. On a JS
	// target repo node_modules is routinely 100k-300k entries, each one
	// running Match over the whole rule set, on every capture AND on the
	// restore's deletion walk. The published 105 ms/boundary was measured
	// on this Go repo, which has no node_modules.
	for _, r := range ig.rules {
		if r.negated && r.couldMatchUnder(rel) {
			return false
		}
	}
	return true
}

// couldMatchUnder reports whether this rule might match some path beneath
// dir. Conservative: it answers true whenever it cannot prove otherwise,
// so a wrong answer costs a walk, never a dropped file.
func (r rule) couldMatchUnder(dir string) bool {
	if !r.anchored {
		// A bare name matches at any depth, so it can always re-include
		// something deeper.
		return true
	}
	dirSegs := strings.Split(dir, "/")
	for i, seg := range dirSegs {
		if i >= len(r.segments) {
			// The rule is fully consumed and matched every segment so far,
			// so dir is inside the rule's own subtree.
			return true
		}
		rs := r.segments[i]
		if rs == "**" || strings.ContainsAny(rs, "*?[") {
			return true // wildcard: cannot rule it out
		}
		if rs != seg {
			return false // literal divergence: this rule lives elsewhere
		}
	}
	return true
}

// matches reports whether the rule applies to a path, either directly or
// through one of its ancestor directories (a directory rule covers its
// whole subtree).
func (r rule) matches(parts []string, isDir bool) bool {
	// Cheap rejection before any segment walk: an anchored rule rooted at
	// a literal cannot match a path — nor any ancestor of it — whose first
	// segment differs.
	if r.rootLiteral != "" && (len(parts) == 0 || parts[0] != r.rootLiteral) {
		return false
	}
	// A bare single-segment rule (`node_modules`, `*.log`) matches at any
	// depth, so "the path OR any ancestor" reduces to "any segment" —
	// ancestors are prefixes and contribute no segment the path lacks.
	// One scan instead of one matchPath per ancestor, which for this class
	// is where the remaining per-path cost sat.
	if !r.anchored && len(r.segments) == 1 {
		last := len(parts) - 1
		for i, p := range parts {
			// dirOnly: every ancestor segment IS a directory; only the
			// final segment depends on isDir.
			if r.dirOnly && i == last && !isDir {
				continue
			}
			if segMatch(r.segments[0], p, r.segLiteral[0]) {
				return true
			}
		}
		return false
	}
	if r.matchPath(parts, isDir) {
		return true
	}
	// Ancestors: `state/` must exclude `state/db/x.json`. Sub-slices, not
	// Join+Split — an ancestor prefix is already the head of `parts`.
	for i := 1; i < len(parts); i++ {
		if r.matchPath(parts[:i], true) {
			return true
		}
	}
	return false
}

func (r rule) matchPath(parts []string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if !r.anchored {
		// A bare name matches any single segment at any depth.
		if len(r.segments) == 1 {
			for _, p := range parts {
				if segMatch(r.segments[0], p, r.segLiteral[0]) {
					return true
				}
			}
			return false
		}
		// Unanchored multi-segment: try every suffix start.
		for i := range parts {
			if matchSegments(r.segments, r.segLiteral, parts[i:]) {
				return true
			}
		}
		return false
	}
	return matchSegments(r.segments, r.segLiteral, parts)
}

// matchSegments matches a segment pattern against path segments, with
// "**" standing for zero or more segments.
// segMatch compares one pattern segment to one path segment. `literal`
// is the precomputed verdict from parseRule — a plain compare for the
// overwhelming majority of real ignore rules, filepath.Match only for
// the ones that actually glob.
func segMatch(pattern, seg string, literal bool) bool {
	if literal {
		return pattern == seg
	}
	ok, _ := filepath.Match(pattern, seg)
	return ok
}

func matchSegments(pattern []string, lit []bool, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		// Zero segments…
		if matchSegments(pattern[1:], lit[1:], path) {
			return true
		}
		// …or one or more.
		for i := 1; i <= len(path); i++ {
			if matchSegments(pattern[1:], lit[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	if !segMatch(pattern[0], path[0], lit[0]) {
		return false
	}
	return matchSegments(pattern[1:], lit[1:], path[1:])
}
