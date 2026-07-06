// Package memory provides the per-workspace iterion memory tree at
// ~/.iterion/projects/<encoded-workdir>/memory/<scope>/.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// WorkspaceMemoryDir returns the per-workspace memory root for the
// given working directory. Reuses the same encoder + home resolver
// as the run store so the memory tree sits next to the run state.
// Returns "" when workDir is empty.
func WorkspaceMemoryDir(workDir string) string {
	return workspaceSubdir(workDir, "memory")
}

// WorkspaceScratchDir returns the per-workspace scratch root, a sibling
// of WorkspaceMemoryDir at ~/.iterion/projects/<encoded-workdir>/scratch/.
// It is the home for OUT-OF-TREE working files a bot must not leave in
// the target repo (e.g. a chunked-review's per-chunk diffs) — keyed off
// the repo root like the memory tree, and the SAME host path is
// bind-mounted inside the sandbox (~/.iterion auto-mount), so it resolves
// in both modes without remapping. Returns "" when workDir is empty.
func WorkspaceScratchDir(workDir string) string {
	return workspaceSubdir(workDir, "scratch")
}

// workspaceSubdir resolves the per-workspace root for the given
// subdirectory name (e.g. "memory" or "scratch"), following the same
// abs-path + encode-key convention as the run store. Returns "" when
// workDir is empty or can't be made absolute.
func workspaceSubdir(workDir, subdir string) string {
	if workDir == "" {
		return ""
	}
	abs := workDir
	if !filepath.IsAbs(abs) {
		resolved, err := filepath.Abs(workDir)
		if err != nil {
			return ""
		}
		abs = resolved
	}
	return filepath.Join(store.GlobalIterionDataDir(), "projects", store.EncodeWorkDirKey(abs), subdir)
}

// Scope is a sandboxed view of a single feature subfolder. All
// read/write/list ops are path-clamped to the scope's root —
// callers cannot escape via "../" or absolute paths.
type Scope struct {
	root string // absolute path; never escapes
}

// OpenScope returns the scope subfolder under the workspace memory
// root, validating the scope name first.
func OpenScope(workDir, scope string) (*Scope, error) {
	if err := ValidateScopeName(scope); err != nil {
		return nil, err
	}
	base := WorkspaceMemoryDir(workDir)
	if base == "" {
		return nil, fmt.Errorf("memory: empty workDir")
	}
	return &Scope{root: filepath.Join(base, scope)}, nil
}

// Root returns the absolute path of this scope's folder.
func (s *Scope) Root() string { return s.root }

// Resolve translates a scope-relative path to an absolute one,
// rejecting any path that escapes the scope root. Empty `rel`
// returns the scope root itself.
func (s *Scope) Resolve(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("memory: path must be scope-relative, got absolute %q", rel)
	}
	abs := filepath.Clean(filepath.Join(s.root, rel))
	if abs != s.root && !strings.HasPrefix(abs, s.root+string(filepath.Separator)) {
		return "", fmt.Errorf("memory: path %q escapes scope root", rel)
	}
	return abs, nil
}

// Read returns the contents of the file at the scope-relative
// path. Returns os.ErrNotExist when the file is absent.
func (s *Scope) Read(rel string) ([]byte, error) {
	abs, err := s.Resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// Write replaces the file at the scope-relative path with the
// given content, creating parent directories as needed.
func (s *Scope) Write(rel string, content []byte) error {
	abs, err := s.Resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, content, 0o644)
}

// List enumerates files (not directories) under the scope-relative
// directory, returning paths relative to the scope root. Order is
// filesystem-defined. Missing directories return an empty slice
// without error so callers can use it as a probe.
func (s *Scope) List(relDir string) ([]string, error) {
	abs, err := s.Resolve(relDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, filepath.Join(relDir, e.Name()))
	}
	return out, nil
}

// Autoload reads every file matching one of the given relative
// glob patterns under the scope. Returns a slice of (path,
// content) pairs in deterministic (lexicographic) order, suitable
// for prepending to a system prompt or compaction injection.
// Missing files are silently skipped. Errors only surface for
// path-escape attempts or unreadable files that exist.
//
// Returns an empty slice when patterns is empty — the auto-index
// (BuildIndex) covers the "what exists in the scope" question, so
// Autoload is reserved for files whose FULL content must always
// be in the system prompt (e.g. CONTEXT_BRIEF.md).
func (s *Scope) Autoload(patterns []string) ([]AutoloadEntry, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(patterns))
	var matches []string
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		abs, err := s.Resolve(pat)
		if err != nil {
			return nil, err
		}
		hits, _ := filepath.Glob(abs)
		for _, h := range hits {
			if seen[h] {
				continue
			}
			seen[h] = true
			matches = append(matches, h)
		}
	}
	out := make([]AutoloadEntry, 0, len(matches))
	for _, abs := range matches {
		data, err := os.ReadFile(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		rel, _ := filepath.Rel(s.root, abs)
		out = append(out, AutoloadEntry{Path: rel, Content: data})
	}
	return out, nil
}

// AutoloadEntry is one file's worth of autoloaded memory content.
type AutoloadEntry struct {
	Path    string // relative to scope root
	Content []byte
}

// ValidateScopeName rejects scope names that contain path
// separators, are empty, or attempt traversal. Names must be a
// single folder segment.
func ValidateScopeName(scope string) error {
	if scope == "" {
		return fmt.Errorf("memory: scope name is required")
	}
	if strings.ContainsAny(scope, `/\`) || scope == "." || scope == ".." {
		return fmt.Errorf("memory: scope name %q must be a single folder segment", scope)
	}
	return nil
}
