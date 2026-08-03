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
// them on every node boundary would dominate the cost of a run, and
// restoring them is never what an operator means by "put the docs back".
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

// Ignorer decides which workspace paths are versioned.
//
// The rule set is iterion's own — a project states it in `.iterionignore`,
// falling back to `.gitignore` when that file is absent. The fallback is
// pragmatic (in a repository, what git is told to skip is nearly always
// what we should skip too) but the precedence matters: a project can take
// control of what iterion versions without touching how it packages
// itself for git, which is the coupling to avoid. `.gitignore` decides
// what ships in a commit; it should not, by itself, decide what a rewind
// can undo.
type Ignorer struct {
	patterns  []string
	protected map[string]bool // workspace-relative, exact match
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
	ig := &Ignorer{patterns: append([]string(nil), defaultIgnored...)}
	for _, name := range []string{".iterionignore", ".gitignore"} {
		lines, ok := readIgnoreFile(filepath.Join(workspaceDir, name))
		if ok {
			ig.patterns = append(ig.patterns, lines...)
			// `.iterionignore` wins outright when present: a project that
			// states iterion's rules is not also asking for git's.
			break
		}
	}
	return ig
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
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			// Negation patterns are not supported; skipping them is the
			// conservative choice (the path stays versioned).
			continue
		}
		out = append(out, strings.TrimSuffix(line, "/"))
	}
	return out, true
}

// Match reports whether a workspace-relative, slash-separated path is
// excluded. Directories are matched by name at any depth, which is how
// the overwhelming majority of real ignore entries behave.
func (ig *Ignorer) Match(rel string, isDir bool) bool {
	if rel == "" || rel == "." {
		return false
	}
	if ig.protected[rel] {
		return true
	}
	segments := strings.Split(rel, "/")
	for _, seg := range segments {
		if alwaysIgnored[seg] {
			return true
		}
	}
	base := segments[len(segments)-1]
	for _, pat := range ig.patterns {
		if pat == "" {
			continue
		}
		// Anchored pattern ("/build" or "docs/build"): compare against the
		// full relative path.
		if strings.Contains(pat, "/") {
			if rel == strings.TrimPrefix(pat, "/") || strings.HasPrefix(rel, strings.TrimPrefix(pat, "/")+"/") {
				return true
			}
			continue
		}
		if ok, _ := filepath.Match(pat, base); ok {
			return true
		}
		// A bare directory name excludes the whole subtree.
		if !isDir {
			for _, seg := range segments[:len(segments)-1] {
				if seg == pat {
					return true
				}
			}
		}
	}
	return false
}
