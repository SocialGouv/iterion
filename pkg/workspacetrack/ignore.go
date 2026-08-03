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
	for _, seg := range strings.Split(rel, "/") {
		if alwaysIgnored[seg] {
			return true
		}
	}
	excluded := false
	for _, r := range ig.rules {
		if r.matches(rel, isDir) {
			excluded = !r.negated
		}
	}
	return excluded
}

// CanPrune reports whether an excluded directory can be skipped whole.
//
// With no negations, an excluded directory holds nothing we want and the
// walk can stop there. As soon as one negation exists, something inside
// may be re-included, so the walk has to descend — the cost of making an
// allowlist expressible. Stat-only descent, no hashing, so it is cheap
// next to what it enables.
func (ig *Ignorer) CanPrune() bool { return !ig.hasNegate }

// matches reports whether the rule applies to a path, either directly or
// through one of its ancestor directories (a directory rule covers its
// whole subtree).
func (r rule) matches(rel string, isDir bool) bool {
	if r.matchPath(rel, isDir) {
		return true
	}
	// Ancestors: `state/` must exclude `state/db/x.json`.
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		if r.matchPath(strings.Join(parts[:i], "/"), true) {
			return true
		}
	}
	return false
}

func (r rule) matchPath(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	parts := strings.Split(rel, "/")
	if !r.anchored {
		// A bare name matches any single segment at any depth.
		if len(r.segments) == 1 {
			for _, p := range parts {
				if ok, _ := filepath.Match(r.segments[0], p); ok {
					return true
				}
			}
			return false
		}
		// Unanchored multi-segment: try every suffix start.
		for i := range parts {
			if matchSegments(r.segments, parts[i:]) {
				return true
			}
		}
		return false
	}
	return matchSegments(r.segments, parts)
}

// matchSegments matches a segment pattern against path segments, with
// "**" standing for zero or more segments.
func matchSegments(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		// Zero segments…
		if matchSegments(pattern[1:], path) {
			return true
		}
		// …or one or more.
		for i := 1; i <= len(path); i++ {
			if matchSegments(pattern[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	if ok, _ := filepath.Match(pattern[0], path[0]); !ok {
		return false
	}
	return matchSegments(pattern[1:], path[1:])
}
