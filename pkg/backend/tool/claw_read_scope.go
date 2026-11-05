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
	mu          sync.RWMutex
	workspace   string
	workspaceFn func() string
	extra       []extraRoot
}

// extraRoot is one read-only root past the workspace.
//
// A plain root (AddExtraRoot) is the directory itself: its own path
// belongs to whoever attached it, so it is canonicalized through symlinks
// — at attachment and again on every resolve.
//
// A nested root (AddExtraRootBeneath) is a canonical ANCHOR plus a lexical
// TAIL. Only the anchor is canonicalized; the tail's components are never
// followed. It is the shape of a root that lives INSIDE a directory other
// runs write — the run's scratch subtree under the shared project scratch
// (#2869), which sandboxed runs write through their bind — where a plain
// root would let a link planted at runs/<id> become the root itself and
// widen the read scope to wherever it points. A target is contained only
// when its fully-resolved path lies lexically under anchor+tail, and it is
// opened by a no-follow walk from the anchor: a link anywhere in the tail
// contains nothing.
type extraRoot struct {
	anchor string
	tail   string // "" for a plain root
}

// path is the root as the containment check and the result spelling see
// it: the anchor joined with the lexical tail.
func (r extraRoot) path() string {
	if r.tail == "" {
		return r.anchor
	}
	return filepath.Join(r.anchor, r.tail)
}

// NewReadScope anchors the scope at the run workspace. Empty workspace
// keeps the process-cwd-less refusal behaviour of the pre-scope registry.
func NewReadScope(workspace string) *ReadScope {
	return &ReadScope{workspace: workspace}
}

// NewReadScopeWithProvider anchors the scope at a workspace PROVIDER,
// consulted on every resolve, rather than a string captured at
// construction. It is the scope twin of the command/skill provider seam
// (#2870): a run whose workspace moves after the registry exists — the
// engine replaces the executor's workDir when `worktree: auto` produces a
// per-run worktree (ClawExecutor.SetWorkDir) — keeps its reads, AND the
// bash/write_file/file_edit resolution RegisterClawBuiltinsWithScope
// derives from the scope, rooted in the run's checkout (#2889). A nil
// provider behaves as the empty workspace: every read refuses.
//
// Contract: the provider runs while the scope's lock is HELD — every
// resolve answers workspace and extra roots under one lock read. A
// provider that re-enters the scope (AddExtraRoot, ExtraRoots) deadlocks
// on that lock; it must answer from state it owns. The host closure
// runview wires reads the executor's workDir (an atomic field, #2897) and
// nothing else.
func NewReadScopeWithProvider(workspace func() string) *ReadScope {
	if workspace == nil {
		workspace = func() string { return "" }
	}
	return &ReadScope{workspaceFn: workspace}
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
	s.addExtraRoot(extraRoot{anchor: canon})
}

// AddExtraRootBeneath widens the scope by one read-only root that lies
// beneath anchor, attached as a NESTED root (see extraRoot): the anchor is
// canonicalized, the path from it to root is kept lexical and never
// followed. Use it for a root inside a directory other runs can write —
// a symlink planted anywhere between anchor and root then contains
// nothing instead of becoming the root. root equal to anchor attaches the
// anchor as a plain root; empty root is a no-op. A root that is not
// beneath anchor is refused with an error, unattached.
func (s *ReadScope) AddExtraRootBeneath(anchor, root string) error {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	if strings.TrimSpace(anchor) == "" {
		return fmt.Errorf("read root %q: no anchor to attach it beneath", root)
	}
	absAnchor, err := filepath.Abs(anchor)
	if err != nil {
		return fmt.Errorf("read root anchor %q: %w", anchor, err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("read root %q: %w", root, err)
	}
	tail, err := filepath.Rel(absAnchor, absRoot)
	if err != nil || tail == ".." || strings.HasPrefix(tail, ".."+string(filepath.Separator)) || filepath.IsAbs(tail) {
		return fmt.Errorf("read root %s is not beneath its anchor %s", absRoot, absAnchor)
	}
	if tail == "." {
		s.AddExtraRoot(absAnchor)
		return nil
	}
	canon, err := canonicalReadRoot(absAnchor)
	if err != nil {
		canon = absAnchor
	}
	s.addExtraRoot(extraRoot{anchor: canon, tail: tail})
	return nil
}

// addExtraRoot appends r unless it duplicates an attached root or names
// the workspace itself.
func (s *ReadScope) addExtraRoot(r extraRoot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.anchor == "" || (r.tail == "" && r.anchor == s.canonicalWorkspaceLocked()) {
		return
	}
	for _, e := range s.extra {
		if e == r {
			return
		}
	}
	s.extra = append(s.extra, r)
}

// ExtraRoots reports the read-only roots attached past the workspace, a
// nested root spelled as its anchor joined with its lexical tail.
func (s *ReadScope) ExtraRoots() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return extraRootPaths(s.extra)
}

func extraRootPaths(extra []extraRoot) []string {
	out := make([]string, 0, len(extra))
	for _, e := range extra {
		out = append(out, e.path())
	}
	return out
}

// snapshot returns the workspace and extra roots under one lock read, so a
// resolve never judges a path against half an update. A provider-backed
// scope answers with the provider's CURRENT workspace — consulted on every
// call, so the read tools follow a workspace the host moves after
// registration (#2889) exactly as its extra roots follow a root pushed
// after registration (#2650).
func (s *ReadScope) snapshot() (string, []extraRoot) {
	if s == nil {
		return "", nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workspaceLocked(), append([]extraRoot(nil), s.extra...)
}

// workspaceLocked answers the scope's workspace: the provider's live answer
// when one is wired, else the construction-time string.
func (s *ReadScope) workspaceLocked() string {
	if s.workspaceFn != nil {
		return s.workspaceFn()
	}
	return s.workspace
}

func (s *ReadScope) canonicalWorkspaceLocked() string {
	canon, err := canonicalReadRoot(s.workspaceLocked())
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

// matchedRoot is the scope root containingRoot found for a resolved path.
// path is the root the containment held against (and the spelling result
// paths are reported relative to); base is where the no-follow open walk
// starts — path itself for the workspace and a plain root, the canonical
// anchor for a nested one, so no tail component is ever followed.
type matchedRoot struct {
	path string
	base string
}

func (m matchedRoot) found() bool { return m.path != "" }

// containingRoot returns the root — workspace first, then the extra roots
// in attachment order — that contains the RESOLVED path, or the zero
// matchedRoot when none does. A plain root is re-canonicalized here; a
// nested root re-canonicalizes its anchor only and joins its tail
// lexically, so a link swapped into the tail after attachment moves
// nothing: the resolved target of anything read through it lies outside.
// An EvalSymlinks failure on a candidate skips it: a root that does not
// exist contains nothing.
func containingRoot(resolved, workspace string, extra []extraRoot) matchedRoot {
	candidates := make([]extraRoot, 0, len(extra)+1)
	if workspace != "" {
		candidates = append(candidates, extraRoot{anchor: workspace})
	}
	candidates = append(candidates, extra...)
	for _, candidate := range candidates {
		base, err := canonicalReadRoot(candidate.anchor)
		if err != nil {
			continue
		}
		root := base
		if candidate.tail != "" {
			root = filepath.Join(base, candidate.tail)
		}
		rel, err := filepath.Rel(root, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return matchedRoot{path: root, base: base}
		}
	}
	return matchedRoot{}
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
	if !containingRoot(resolved, workspace, extra).found() {
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
	return openReadFileGated(scope, raw, newCredentialReadGate(scope))
}

// openReadFileGated is openReadFile over a credential gate the caller
// already built — a grep/glob walk shares one derivation across every
// file it visits instead of paying it per open.
func openReadFileGated(scope *ReadScope, raw string, gate credentialReadGate) (*os.File, error) {
	workspace, extra := scope.snapshot()
	path, err := resolveReadPath(scope, raw)
	if err != nil {
		return nil, err
	}
	if gate.refused(raw) || gate.refused(path) {
		return nil, fmt.Errorf("path is a protected credential or secret file")
	}
	root := containingRoot(path, workspace, extra)
	if !root.found() {
		return nil, fmt.Errorf("path is outside workspace")
	}
	// The walk starts at the root's base: for a nested root that is its
	// anchor, so every tail component is opened no-follow too.
	rel, err := filepath.Rel(root.base, path)
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
	f, err := openWorkspaceFileAt(root.base, strings.Split(rel, string(filepath.Separator)))
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
