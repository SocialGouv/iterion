package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ReadScope is the set of directories claw's READ tools (read_file, glob,
// grep, read_image — and the workspace_grep diagnostic alias) accept: the
// run workspace plus zero or more EXTRA read-only roots a host attaches
// after the registry is built.
//
// The canonical extra root is the run's ${PROJECT_SCRATCH_DIR} (#2650):
// bots keep their run state out of the audited tree there BY DESIGN, and
// the claude_code backend's unbounded Read reaches it, so a bot whose
// agents read their own briefs lost every one of them on claw — read_file
// and glob refused any path outside the workspace. The engine pushes the
// resolved scratch dir onto the executor once the run (and its sandbox
// decision) pins it down, which is why the scope is mutable: the registry
// is built before the engine knows the value.
//
// Only READS consult the extra roots — write_file and file_edit are not
// scope-aware at all (relative paths rebase onto the workspace, absolute
// paths pass through, as they always have), and the credential/secret
// exclusions apply under every root alike.
//
// The zero value is valid: no workspace, no extra roots — every read
// refuses, as an empty workspace string always did.
type ReadScope struct {
	mu        sync.RWMutex
	workspace string
	extra     []string
}

// NewReadScope anchors the scope at the run workspace. Empty workspace
// keeps the process-cwd-less refusal behaviour of the pre-scope registry.
func NewReadScope(workspace string) *ReadScope {
	return &ReadScope{workspace: workspace}
}

// AddExtraRoot widens the scope by one read-only root. Idempotent and
// canonicalized (absolute, symlinks evaluated when the directory exists):
// the engine's push and a host that wired it at registration must not
// stack duplicates, and a root spelled through a symlink must contain the
// same resolved paths as its target. Empty is a no-op.
func (s *ReadScope) AddExtraRoot(root string) {
	if strings.TrimSpace(root) == "" {
		return
	}
	canon, err := canonicalReadRoot(root)
	if err != nil {
		canon, _ = filepath.Abs(root)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if canon == "" || canon == s.canonicalWorkspaceLocked() {
		return
	}
	for _, e := range s.extra {
		if e == canon {
			return
		}
	}
	s.extra = append(s.extra, canon)
}

// ExtraRoots reports the read-only roots attached past the workspace.
func (s *ReadScope) ExtraRoots() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.extra...)
}

// snapshot returns the workspace and extra roots under one lock read, so a
// resolve never judges a path against half an update.
func (s *ReadScope) snapshot() (string, []string) {
	if s == nil {
		return "", nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workspace, append([]string(nil), s.extra...)
}

func (s *ReadScope) canonicalWorkspaceLocked() string {
	canon, err := canonicalReadRoot(s.workspace)
	if err != nil {
		return ""
	}
	return canon
}

// canonicalReadRoot resolves root to the absolute, symlink-evaluated path
// the containment check compares against — the same canonicalization
// resolveWorkspacePath has always applied to the workspace, so an extra
// root and a read target meet on the same side of any link.
func canonicalReadRoot(root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

// containingRoot returns the canonical root — workspace first, then the
// extra roots in attachment order — that contains the RESOLVED path, or ""
// when none does. An EvalSymlinks failure on a candidate root skips it: a
// root that does not exist contains nothing.
func containingRoot(resolved, workspace string, extra []string) string {
	candidates := make([]string, 0, len(extra)+1)
	if workspace != "" {
		candidates = append(candidates, workspace)
	}
	candidates = append(candidates, extra...)
	for _, candidate := range candidates {
		root, err := canonicalReadRoot(candidate)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return root
		}
	}
	return ""
}

// readResultPath spells a found path the way the OTHER read tools take it
// back. read_file JOINS a relative path onto the workspace (resolveReadPath),
// so a path spelled relative to an extra read root would reopen the
// workspace's homonym — the wrong file, silently, with no refusal to
// notice. Inside the workspace the relative spelling is kept — the one
// the path contract pins (claw_workspace_path_contract_test.go) and
// agents hand back; under any other root the path is absolute, which is
// what grep already labels its matches with. One helper, so a third
// path-emitting read tool cannot invent a fourth spelling.
func readResultPath(scope *ReadScope, root, path string) string {
	workspace, _ := scope.snapshot()
	if canon, err := canonicalReadRoot(workspace); err == nil && canon != "" && canon == root {
		if rel, relErr := filepath.Rel(root, path); relErr == nil {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}

// resolveReadPath is the scope-wide path gate, shared by read_file, glob
// and grep: a relative path joins the workspace (and so can never leave
// it); an absolute path must land under the workspace or one of the extra
// read roots. Symlinks are evaluated before the comparison, so a link
// pointing out of every root refuses rather than smuggles.
func resolveReadPath(scope *ReadScope, raw string) (string, error) {
	workspace, extra := scope.snapshot()
	if strings.TrimSpace(workspace) == "" {
		return "", fmt.Errorf("active workspace is required")
	}
	path := raw
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if containingRoot(resolved, workspace, extra) == "" {
		if len(extra) == 0 {
			return "", fmt.Errorf("path %q is outside workspace", raw)
		}
		return "", fmt.Errorf("path %q is outside workspace and its read roots", raw)
	}
	return resolved, nil
}

// openReadFile is openWorkspaceFile over a scope: same alias checks, same
// opened-identity verification, but the descriptor walk roots at whichever
// scope root contains the path, so an extra root's file is opened with the
// same no-follow discipline as a workspace one.
func openReadFile(scope *ReadScope, raw string) (*os.File, error) {
	workspace, extra := scope.snapshot()
	path, err := resolveReadPath(scope, raw)
	if err != nil {
		return nil, err
	}
	if sensitiveWorkspacePath(raw) || sensitiveWorkspacePath(path) {
		return nil, fmt.Errorf("path is a protected credential or secret file")
	}
	root := containingRoot(path, workspace, extra)
	if root == "" {
		return nil, fmt.Errorf("path is outside workspace")
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", filepath.Base(path))
	}
	f, err := openWorkspaceFileAt(root, strings.Split(rel, string(filepath.Separator)))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("workspace file changed or is not a regular file")
	}
	return f, nil
}
