package dispatcher

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Workspaces manages per-issue workspace directories under a single
// root. It enforces filename sanitization and refuses to traverse
// outside the root via symlinks or pathological IDs.
type Workspaces struct {
	root string
	mu   sync.Mutex
}

// workspaceKeyNamespace deliberately starts with a dot. sanitizeKey never
// returns a leading-dot name, so this namespace cannot alias a workspace
// created by versions that used filepath.Join(root, sanitizeKey(issueID)).
//
// We do not automatically adopt or rename those legacy directories: the old
// sanitization was many-to-one, so their owning issue cannot be proven from
// the directory name alone. Leaving them untouched is the only fail-closed
// migration behaviour; new workspaces are allocated in this disjoint
// namespace.
const workspaceKeyNamespace = ".issue-workspaces-v2"

const workspaceKeySlugMax = 80

const workspaceOwnersDir = ".owners"

type workspaceOwnerState string

const (
	workspaceOwnerActive  workspaceOwnerState = "active"
	workspaceOwnerRetired workspaceOwnerState = "retired"
)

type workspaceOwnerRecord struct {
	FormatVersion int                 `json:"format_version"`
	IssueID       string              `json:"issue_id"`
	RunID         string              `json:"run_id,omitempty"`
	State         workspaceOwnerState `json:"state"`
	CreatedAt     time.Time           `json:"created_at"`
}

// NewWorkspaces returns a manager rooted at the given path. The root
// itself is created on first Create.
func NewWorkspaces(root string) (*Workspaces, error) {
	if root == "" {
		return nil, errors.New("workspace: root path required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root: %w", err)
	}
	return &Workspaces{root: abs}, nil
}

// Root returns the absolute root path.
func (w *Workspaces) Root() string { return w.root }

// Path returns the absolute, versioned workspace path for the given issue ID,
// without creating anything on disk. The readable slug is not an identity
// boundary: a full SHA-256 digest of the original (unsanitized) ID makes IDs
// which sanitize to the same text resolve to different paths.
func (w *Workspaces) Path(issueID string) string {
	return filepath.Join(w.root, workspaceKeyNamespace, workspaceKey(issueID))
}

// PathForRun returns a generation-specific path for one dispatch run. A
// completed run's absolute path is never reused by a later run of the same
// issue, so a stale background writer cannot contaminate the next dispatch
// after the old checkout has been atomically quarantined.
func (w *Workspaces) PathForRun(issueID, runID string) string {
	if runID == "" {
		return w.Path(issueID)
	}
	return filepath.Join(w.root, workspaceKeyNamespace, workspaceKeyForRun(issueID, runID))
}

// Create ensures a workspace directory exists for issueID. The boolean
// return reports whether the directory was created by this call (so a
// caller can run after_create hooks only on first creation). The
// returned path is guaranteed to live under the configured root.
func (w *Workspaces) Create(issueID string) (path string, created bool, err error) {
	return w.CreateForRun(issueID, "")
}

// CreateForRun is Create with a run generation bound into both the path and
// its ownership marker. Dispatcher production paths use this method; Create
// remains available for callers that intentionally manage a single
// non-generational workspace.
func (w *Workspaces) CreateForRun(issueID, runID string) (path string, created bool, err error) {
	if issueID == "" {
		return "", false, errors.New("workspace: issue id required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := os.MkdirAll(w.root, 0o755); err != nil {
		return "", false, fmt.Errorf("workspace: mkdir root: %w", err)
	}
	rootCanon, err := filepath.EvalSymlinks(w.root)
	if err != nil {
		return "", false, fmt.Errorf("workspace: canonicalize root: %w", err)
	}

	namespace := filepath.Join(w.root, workspaceKeyNamespace)
	if err := os.Mkdir(namespace, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return "", false, fmt.Errorf("workspace: mkdir key namespace: %w", err)
	}
	namespaceInfo, err := os.Lstat(namespace)
	if err != nil {
		return "", false, fmt.Errorf("workspace: stat key namespace: %w", err)
	}
	if !namespaceInfo.IsDir() || namespaceInfo.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("workspace: key namespace %s is not a real directory", namespace)
	}
	namespaceCanon, err := filepath.EvalSymlinks(namespace)
	if err != nil {
		return "", false, fmt.Errorf("workspace: canonicalize key namespace: %w", err)
	}
	if !isWithin(namespaceCanon, rootCanon) {
		return "", false, fmt.Errorf("workspace: key namespace %q escapes root %q", namespaceCanon, rootCanon)
	}

	ownersDir := filepath.Join(namespace, workspaceOwnersDir)
	if err := os.Mkdir(ownersDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", false, fmt.Errorf("workspace: mkdir ownership namespace: %w", err)
	}
	ownersInfo, err := os.Lstat(ownersDir)
	if err != nil {
		return "", false, fmt.Errorf("workspace: stat ownership namespace: %w", err)
	}
	if !ownersInfo.IsDir() || ownersInfo.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("workspace: ownership namespace %s is not a real directory", ownersDir)
	}

	target := w.PathForRun(issueID, runID)
	ownerPath := w.ownerPathForRun(issueID, runID)
	targetInfo, targetErr := os.Lstat(target)
	owner, ownerErr := readWorkspaceOwner(ownerPath)

	switch {
	case targetErr == nil:
		if ownerErr != nil {
			return "", false, fmt.Errorf(
				"workspace: refusing to adopt existing target %s without valid ownership: %w",
				target, ownerErr,
			)
		}
		if err := verifyWorkspaceOwner(owner, issueID, runID, workspaceOwnerActive); err != nil {
			return "", false, err
		}
	case !errors.Is(targetErr, os.ErrNotExist):
		return "", false, fmt.Errorf("workspace: stat target: %w", targetErr)
	case ownerErr == nil:
		return "", false, fmt.Errorf(
			"workspace: owned target %s is missing while marker is %q; manual recovery required",
			target, owner.State,
		)
	case !errors.Is(ownerErr, os.ErrNotExist):
		return "", false, fmt.Errorf("workspace: read ownership marker: %w", ownerErr)
	default:
		if err := os.Mkdir(target, 0o755); err != nil {
			return "", false, fmt.Errorf("workspace: mkdir target: %w", err)
		}
		// os.Mkdir is the creation authority. Unlike a Stat+MkdirAll
		// sequence, exactly one concurrent caller can observe success and
		// therefore report created=true/run the after_create hook.
		created = true
		record := workspaceOwnerRecord{
			FormatVersion: 1,
			IssueID:       issueID,
			RunID:         runID,
			State:         workspaceOwnerActive,
			CreatedAt:     time.Now().UTC(),
		}
		if err := createWorkspaceOwner(ownerPath, record); err != nil {
			// Never remove the target here: even in this narrow failure
			// window another process may already have written into it.
			return "", false, fmt.Errorf(
				"workspace: publish ownership marker (target preserved unowned at %s): %w",
				target, err,
			)
		}
		targetInfo, targetErr = os.Lstat(target)
		if targetErr != nil {
			return "", false, fmt.Errorf("workspace: stat freshly-created target: %w", targetErr)
		}
	}

	if !targetInfo.IsDir() || targetInfo.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("workspace: %s exists and is not a real directory", target)
	}
	canon, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", false, fmt.Errorf("workspace: canonicalize target: %w", err)
	}
	if !isWithin(canon, rootCanon) {
		return "", false, fmt.Errorf("workspace: target %q escapes root %q (symlink or traversal)", canon, rootCanon)
	}
	return canon, created, nil
}

// Retire revokes an active workspace before any teardown hook or filesystem
// mutation. Create refuses retired markers, so a late writer that recreates the
// canonical path after quarantine can never make that directory authoritative
// for a future dispatch.
func (w *Workspaces) Retire(issueID string) error {
	return w.RetireForRun(issueID, "")
}

// RetireForRun revokes the exact issue/run generation before teardown.
func (w *Workspaces) RetireForRun(issueID, runID string) error {
	if issueID == "" {
		return errors.New("workspace: issue id required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	owner, err := readWorkspaceOwner(w.ownerPathForRun(issueID, runID))
	if err != nil {
		return fmt.Errorf("workspace: read ownership marker for retirement: %w", err)
	}
	if err := verifyWorkspaceOwnerID(owner, issueID, runID); err != nil {
		return err
	}
	if owner.State == workspaceOwnerRetired {
		return nil
	}
	if owner.State != workspaceOwnerActive {
		return fmt.Errorf("workspace: unsupported ownership state %q", owner.State)
	}
	if _, err := w.verifyOwnedTarget(issueID, runID); err != nil {
		return err
	}
	owner.State = workspaceOwnerRetired
	if err := replaceWorkspaceOwner(w.ownerPathForRun(issueID, runID), owner); err != nil {
		return fmt.Errorf("workspace: retire ownership marker: %w", err)
	}
	return nil
}

// AuthoritySinceForRun returns the trusted creation boundary stored outside
// the checkout. Unlike a .git mtime, a process running inside the worktree
// cannot advance this value to evade the cleanup process census.
func (w *Workspaces) AuthoritySinceForRun(issueID, runID string) (time.Time, error) {
	if issueID == "" {
		return time.Time{}, errors.New("workspace: issue id required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	owner, err := readWorkspaceOwner(w.ownerPathForRun(issueID, runID))
	if err != nil {
		return time.Time{}, fmt.Errorf("workspace: read ownership marker authority: %w", err)
	}
	if err := verifyWorkspaceOwnerID(owner, issueID, runID); err != nil {
		return time.Time{}, err
	}
	if owner.CreatedAt.IsZero() {
		return time.Time{}, fmt.Errorf("workspace: ownership marker has no trusted creation time")
	}
	return owner.CreatedAt, nil
}

// Release removes a retired ownership marker only after the canonical path is
// provably absent. It is called after an atomic quarantine (or an explicit
// operator hook that removed the path). If a late writer recreated the path,
// the retired marker remains as a fail-closed tombstone.
func (w *Workspaces) Release(issueID string) error {
	return w.ReleaseForRun(issueID, "")
}

// ReleaseForRun removes the retired marker for one generation after its exact
// path is absent. A later dispatch uses a different run ID and therefore a
// different path even if an old absolute-path writer wakes after release.
func (w *Workspaces) ReleaseForRun(issueID, runID string) error {
	if issueID == "" {
		return errors.New("workspace: issue id required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	target := w.PathForRun(issueID, runID)
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("workspace: refusing to release ownership while target exists: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace: inspect target before ownership release: %w", err)
	}
	ownerPath := w.ownerPathForRun(issueID, runID)
	owner, err := readWorkspaceOwner(ownerPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("workspace: read ownership marker for release: %w", err)
	}
	if err := verifyWorkspaceOwner(owner, issueID, runID, workspaceOwnerRetired); err != nil {
		return err
	}
	if err := os.Remove(ownerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace: remove retired ownership marker: %w", err)
	}
	return nil
}

// Remove deletes an owned workspace directory tree if present. Returns nil
// when both the directory and its ownership marker are already absent.
func (w *Workspaces) Remove(issueID string) error {
	return w.RemoveForRun(issueID, "")
}

// RemoveForRun removes an explicitly-owned generation. Runtime/dispatcher
// success cleanup uses atomic quarantine instead; this method is retained for
// setup rollback and operator-authorized lifecycle code.
func (w *Workspaces) RemoveForRun(issueID, runID string) error {
	if issueID == "" {
		return errors.New("workspace: issue id required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	target := w.PathForRun(issueID, runID)
	ownerPath := w.ownerPathForRun(issueID, runID)
	rootCanon, err := filepath.EvalSymlinks(w.root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace: canonicalize root: %w", err)
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		if _, ownerErr := os.Lstat(ownerPath); errors.Is(ownerErr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("workspace: target %s is absent but its ownership marker remains", target)
	}
	if err != nil {
		return fmt.Errorf("workspace: stat target: %w", err)
	}
	owner, err := readWorkspaceOwner(ownerPath)
	if err != nil {
		return fmt.Errorf("workspace: refusing removal without valid ownership: %w", err)
	}
	if err := verifyWorkspaceOwnerID(owner, issueID, runID); err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("workspace: refusing to remove non-directory target %q", target)
	}
	canon, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("workspace: canonicalize target: %w", err)
	}
	if rootCanon != "" && !isWithin(canon, rootCanon) {
		return fmt.Errorf("workspace: refusing to remove %q outside root", canon)
	}
	if err := os.RemoveAll(canon); err != nil {
		return err
	}
	if err := os.Remove(ownerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace: remove ownership marker after directory removal: %w", err)
	}
	return nil
}

func (w *Workspaces) ownerPath(issueID string) string {
	return filepath.Join(w.root, workspaceKeyNamespace, workspaceOwnersDir, workspaceKey(issueID)+".json")
}

func (w *Workspaces) ownerPathForRun(issueID, runID string) string {
	if runID == "" {
		return w.ownerPath(issueID)
	}
	return filepath.Join(w.root, workspaceKeyNamespace, workspaceOwnersDir, workspaceKeyForRun(issueID, runID)+".json")
}

func (w *Workspaces) verifyOwnedTarget(issueID, runID string) (string, error) {
	target := w.PathForRun(issueID, runID)
	info, err := os.Lstat(target)
	if err != nil {
		return "", fmt.Errorf("workspace: stat owned target: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("workspace: owned target is not a real directory: %s", target)
	}
	canon, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("workspace: canonicalize owned target: %w", err)
	}
	rootCanon, err := filepath.EvalSymlinks(w.root)
	if err != nil {
		return "", fmt.Errorf("workspace: canonicalize root: %w", err)
	}
	if !isWithin(canon, rootCanon) {
		return "", fmt.Errorf("workspace: owned target %q escapes root %q", canon, rootCanon)
	}
	return canon, nil
}

func readWorkspaceOwner(path string) (workspaceOwnerRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return workspaceOwnerRecord{}, err
	}
	var owner workspaceOwnerRecord
	if err := json.Unmarshal(data, &owner); err != nil {
		return workspaceOwnerRecord{}, err
	}
	return owner, nil
}

func verifyWorkspaceOwnerID(owner workspaceOwnerRecord, issueID, runID string) error {
	if owner.FormatVersion != 1 {
		return fmt.Errorf("workspace: unsupported ownership marker version %d", owner.FormatVersion)
	}
	if owner.IssueID != issueID {
		return fmt.Errorf("workspace: ownership marker belongs to %q, not %q", owner.IssueID, issueID)
	}
	if owner.RunID != runID {
		return fmt.Errorf("workspace: ownership marker run is %q, not %q", owner.RunID, runID)
	}
	return nil
}

func verifyWorkspaceOwner(owner workspaceOwnerRecord, issueID, runID string, state workspaceOwnerState) error {
	if err := verifyWorkspaceOwnerID(owner, issueID, runID); err != nil {
		return err
	}
	if owner.State != state {
		return fmt.Errorf(
			"workspace: ownership marker for %q is %q, expected %q",
			issueID, owner.State, state,
		)
	}
	return nil
}

func createWorkspaceOwner(path string, owner workspaceOwnerRecord) error {
	return createWorkspaceOwnerWithOpener(path, owner, func(name string, flag int, perm os.FileMode) (workspaceOwnerFile, error) {
		return os.OpenFile(name, flag, perm)
	})
}

type workspaceOwnerFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type workspaceOwnerFileOpener func(name string, flag int, perm os.FileMode) (workspaceOwnerFile, error)

func createWorkspaceOwnerWithOpener(path string, owner workspaceOwnerRecord, open workspaceOwnerFileOpener) error {
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	f, err := open(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	publishErr := error(nil)
	if _, err := f.Write(append(data, '\n')); err != nil {
		publishErr = err
	} else if err := f.Sync(); err != nil {
		publishErr = err
	}
	if err := f.Close(); publishErr == nil {
		publishErr = err
	}
	if publishErr == nil {
		return nil
	}

	// OpenFile(O_EXCL) proves this invocation created the marker. If
	// publication fails, remove that marker so callers never mistake
	// truncated JSON (or a close whose durability is unknown) for a valid
	// ownership record. The workspace directory is deliberately untouched:
	// another process may already have written recoverable output into it.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(publishErr, fmt.Errorf("workspace: remove failed ownership marker: %w", err))
	}
	return publishErr
}

func replaceWorkspaceOwner(path string, owner workspaceOwnerRecord) error {
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".owner-atomic-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

var sanitizeKeyRe = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// workspaceKey combines a bounded human-readable prefix with the complete
// digest of the original ID. The digest, rather than the sanitized prefix, is
// the identity-bearing part of the key.
func workspaceKey(issueID string) string {
	return workspaceKeyWithIdentity(issueID, issueID)
}

// workspaceKeyForRun keeps the issue slug readable while deriving identity
// from both the original issue ID and the unique run generation.
func workspaceKeyForRun(issueID, runID string) string {
	identity := "iterion-dispatch-workspace-run-v1\x00" + issueID + "\x00" + runID
	return workspaceKeyWithIdentity(issueID, identity)
}

func workspaceKeyWithIdentity(issueID, identity string) string {
	slug := sanitizeKey(issueID)
	if len(slug) > workspaceKeySlugMax {
		// sanitizeKey emits ASCII only, so a byte slice cannot split UTF-8.
		slug = slug[:workspaceKeySlugMax]
	}
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s--%x", slug, sum)
}

// sanitizeKey replaces filesystem-hostile characters with underscore.
// A leading dot is also escaped so the directory is not hidden.
func sanitizeKey(s string) string {
	out := sanitizeKeyRe.ReplaceAllString(s, "_")
	out = strings.TrimSpace(out)
	if out == "" {
		out = "_"
	}
	if strings.HasPrefix(out, ".") {
		out = "_" + out
	}
	return out
}

// isWithin reports whether child sits at or below parent. Both must be
// absolute, canonical paths.
func isWithin(child, parent string) bool {
	if child == parent {
		return true
	}
	prefix := parent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(child, prefix)
}
