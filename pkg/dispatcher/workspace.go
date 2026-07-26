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

// Path returns the stable, versioned workspace path for an issue whose persist
// policy intentionally reuses one workspace across dispatches.
func (w *Workspaces) Path(issueID string) string {
	return filepath.Join(w.root, workspaceKeyNamespace, workspaceKey(issueID))
}

// PathForRun returns a generation-specific path for one logical dispatch.
// The readable slug is not an identity boundary: a full SHA-256 digest of the
// original issue ID and generation separates sanitization collisions. A
// completed dispatch's absolute path is never reused, so stale background
// writers remain isolated from later dispatches.
func (w *Workspaces) PathForRun(issueID, runID string) string {
	if runID == "" {
		return w.Path(issueID)
	}
	return filepath.Join(w.root, workspaceKeyNamespace, workspaceKeyForRun(issueID, runID))
}

// Create ensures the stable workspace for issueID exists. It is retained for
// persist=keep and for API compatibility; cleanup policies use CreateForRun.
func (w *Workspaces) Create(issueID string) (path string, created bool, err error) {
	return w.CreateForRun(issueID, "")
}

// CreateForRun ensures a generation-owned workspace exists. The boolean
// reports whether this call created it, so callers run after_create exactly
// once. The returned path is guaranteed to remain under the configured root.
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
	if errors.Is(targetErr, os.ErrNotExist) && ownerErr == nil && runID == "" {
		// The stable API can be retried after a crash between deleting a
		// retired target and deleting its marker. Active orphan markers remain
		// fail-closed: only an explicit retirement proves deletion was allowed.
		if verifyErr := verifyWorkspaceOwner(owner, issueID, runID, workspaceOwnerRetired); verifyErr == nil {
			if err := os.Remove(ownerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", false, fmt.Errorf("workspace: clear retired ownership marker %s: %w", ownerPath, err)
			}
			ownerErr = os.ErrNotExist
		}
	}

	switch {
	case targetErr == nil:
		if ownerErr != nil {
			return "", false, fmt.Errorf(
				"workspace: refusing to adopt existing target %s without valid ownership marker %s: %w",
				target, ownerPath, ownerErr,
			)
		}
		if err := verifyWorkspaceOwner(owner, issueID, runID, workspaceOwnerActive); err != nil {
			return "", false, fmt.Errorf("workspace: verify ownership marker %s: %w", ownerPath, err)
		}
	case !errors.Is(targetErr, os.ErrNotExist):
		return "", false, fmt.Errorf("workspace: stat target: %w", targetErr)
	case ownerErr == nil:
		return "", false, fmt.Errorf(
			"workspace: owned target %s is missing while marker %s is %q; inspect or remove that marker for manual recovery",
			target, ownerPath, owner.State,
		)
	case !errors.Is(ownerErr, os.ErrNotExist):
		return "", false, fmt.Errorf("workspace: read ownership marker %s: %w", ownerPath, ownerErr)
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

// RetireForRun revokes the exact issue/run generation before teardown. Once
// retired, CreateForRun refuses the generation even if a late writer recreates
// its old absolute path.
func (w *Workspaces) RetireForRun(issueID, runID string) error {
	if issueID == "" {
		return errors.New("workspace: issue id required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	ownerPath := w.ownerPathForRun(issueID, runID)
	owner, err := readWorkspaceOwner(ownerPath)
	if err != nil {
		return fmt.Errorf("workspace: read ownership marker %s for retirement: %w", ownerPath, err)
	}
	if err := verifyWorkspaceOwnerID(owner, issueID, runID); err != nil {
		return fmt.Errorf("workspace: verify ownership marker %s for retirement: %w", ownerPath, err)
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
	if err := replaceWorkspaceOwner(ownerPath, owner); err != nil {
		return fmt.Errorf("workspace: retire ownership marker %s: %w", ownerPath, err)
	}
	return nil
}

// Remove preserves the pre-generational API while applying the same safe
// active→retired→deleted lifecycle to the stable workspace.
func (w *Workspaces) Remove(issueID string) error {
	retireErr := w.RetireForRun(issueID, "")
	if retireErr == nil {
		return w.RemoveForRun(issueID, "")
	}
	removeErr := w.RemoveForRun(issueID, "")
	if removeErr == nil {
		return nil
	}
	return errors.Join(retireErr, removeErr)
}

// RemoveForRun deletes an explicitly retired generation. If teardown was
// interrupted after directory removal, a later call safely clears the retired
// marker. An active marker with a missing target remains fail-closed because
// its disappearance was not authorized by this lifecycle.
func (w *Workspaces) RemoveForRun(issueID, runID string) error {
	if issueID == "" {
		return errors.New("workspace: issue id required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	target := w.PathForRun(issueID, runID)
	ownerPath := w.ownerPathForRun(issueID, runID)
	owner, ownerErr := readWorkspaceOwner(ownerPath)
	if errors.Is(ownerErr, os.ErrNotExist) {
		if _, targetErr := os.Lstat(target); errors.Is(targetErr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("workspace: refusing removal without ownership marker %s", ownerPath)
	}
	if ownerErr != nil {
		return fmt.Errorf("workspace: refusing removal with invalid ownership marker %s: %w", ownerPath, ownerErr)
	}
	if err := verifyWorkspaceOwnerID(owner, issueID, runID); err != nil {
		return fmt.Errorf("workspace: verify ownership marker %s: %w", ownerPath, err)
	}

	rootCanon, err := filepath.EvalSymlinks(w.root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace: canonicalize root: %w", err)
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		if owner.State != workspaceOwnerRetired {
			return fmt.Errorf(
				"workspace: target %s is absent but ownership marker %s is %q; inspect or remove that marker for manual recovery",
				target, ownerPath, owner.State,
			)
		}
		if err := os.Remove(ownerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("workspace: remove retired ownership marker %s: %w", ownerPath, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("workspace: stat target: %w", err)
	}
	if err := verifyWorkspaceOwner(owner, issueID, runID, workspaceOwnerRetired); err != nil {
		return fmt.Errorf("workspace: verify retired ownership marker %s: %w", ownerPath, err)
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
		return fmt.Errorf("workspace: remove retired target %s: %w", canon, err)
	}
	if err := os.Remove(ownerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"workspace: target removed but retired ownership marker %s remains; remove it manually or retry cleanup: %w",
			ownerPath, err,
		)
	}
	return nil
}

func (w *Workspaces) ownerPathForRun(issueID, runID string) string {
	if runID == "" {
		return filepath.Join(w.root, workspaceKeyNamespace, workspaceOwnersDir, workspaceKey(issueID)+".json")
	}
	return filepath.Join(w.root, workspaceKeyNamespace, workspaceOwnersDir, workspaceKeyForRun(issueID, runID)+".json")
}

// resumeGeneration returns the active, verified workspace shape that owns a
// resumable run. A unique run shape wins over a stable workspace retained from
// an older keep-mode dispatch; if that unique shape exists but is invalid, we
// fail closed instead of falling through to an unrelated stable workspace.
// The probes are deliberately lock-free: they run on the dispatcher actor and
// must never wait behind a worker's recursive delete.
func (w *Workspaces) resumeGeneration(issueID, runID string) (string, bool) {
	if w.generationShapeExists(issueID, runID) {
		return runID, w.generationIsManaged(issueID, runID)
	}
	if w.generationShapeExists(issueID, "") {
		return "", w.generationIsManaged(issueID, "")
	}
	return "", false
}

func (w *Workspaces) generationShapeExists(issueID, runID string) bool {
	for _, path := range []string{
		w.PathForRun(issueID, runID),
		w.ownerPathForRun(issueID, runID),
	} {
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func (w *Workspaces) generationIsManaged(issueID, runID string) bool {
	owner, err := readWorkspaceOwner(w.ownerPathForRun(issueID, runID))
	if err != nil {
		return false
	}
	if err := verifyWorkspaceOwner(owner, issueID, runID, workspaceOwnerActive); err != nil {
		return false
	}
	_, err = w.verifyOwnedTarget(issueID, runID)
	return err == nil
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
// digest of the original ID. The digest is the identity-bearing part.
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
