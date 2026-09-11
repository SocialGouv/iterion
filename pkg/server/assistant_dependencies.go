package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/botdeps"
	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// assistantDependencyBotsUpdateRequest is purposefully not a general command
// surface. The assistant can name one existing dependency, give its exact
// immutable Git object and a bounded commit message; the host owns every
// filesystem path, source URL, materialization destination and Git argv.
type assistantDependencyBotsUpdateRequest struct {
	Name    string `json:"name"`
	Ref     string `json:"ref"`
	Message string `json:"message"`
}

type assistantDependencyBotsUpdateResponse struct {
	Name          string `json:"name"`
	PreviousRef   string `json:"previous_ref"`
	Ref           string `json:"ref"`
	BundleSHA256  string `json:"bundle_sha256"`
	Commit        string `json:"commit"`
	InstalledPath string `json:"installed_path"`
}

// assistantDependencyBotsLocalizeRequest deliberately carries no destination,
// source URL, ref, or Git options. The browser binds it to the open consumer
// bundle and the host derives the single safe local ownership path.
type assistantDependencyBotsLocalizeRequest struct {
	EditorPath string `json:"editor_path"`
	Name       string `json:"name"`
	Message    string `json:"message"`
}

type assistantDependencyBotsLocalizeResponse struct {
	Name               string `json:"name"`
	LocalSource        string `json:"local_source"`
	SourceCommit       string `json:"source_commit"`
	LockCommit         string `json:"lock_commit"`
	BundleSHA256       string `json:"bundle_sha256"`
	InstalledPath      string `json:"installed_path"`
	ResumedAfterImport bool   `json:"resumed_after_import"`
}

var (
	assistantDependencyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	assistantDependencySHA  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func (s *Server) handleAssistantDependencyBotsUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	// Snapshot-then-release, the pattern resolveAuthoringTarget already
	// uses. The work below reaches the network — botdeps.Update →
	// botinstall.Fetch clones a remote repository — and sync.RWMutex blocks
	// NEW readers once a writer is queued, so holding this RLock for the
	// clone lets one slow fetch plus one project switch stall every
	// stateMu reader in the process (server_info, /api/bots, the pipeline
	// board) for the duration of the network call.
	s.stateMu.RLock()
	mode, workDir := s.cfg.Mode, s.cfg.WorkDir
	s.stateMu.RUnlock()
	if mode == "cloud" {
		s.httpErrorFor(w, r, http.StatusForbidden, "dependency update is unavailable in cloud mode")
		return
	}
	if workDir == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "dependency update requires a project workspace")
		return
	}
	var req assistantDependencyBotsUpdateRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid dependency update request: %v", err)
		return
	}
	if err := validateAssistantDependencyBotsUpdate(req); err != nil {
		s.authoringError(w, r, err)
		return
	}

	s.assistantDependencyMu.Lock()
	defer s.assistantDependencyMu.Unlock()
	result, err := assistantDependencyBotsUpdate(r.Context(), workDir, req)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	s.writeJSONFor(w, r, result)
}

// handleAssistantDependencyBotsLocalize imports one verified, already locked
// materialization into the open consumer bundle. It is intentionally not a
// generic copy or Git endpoint: the dependency name, active bundle, lockfile,
// cache directory, destination and both commit scopes are all host-derived.
func (s *Server) handleAssistantDependencyBotsLocalize(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req assistantDependencyBotsLocalizeRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid dependency localization request: %v", err)
		return
	}
	if err := validateAssistantDependencyBotsLocalize(req); err != nil {
		s.authoringError(w, r, err)
		return
	}
	target, err := s.resolveAuthoringTarget(r, req.EditorPath)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	s.assistantDependencyMu.Lock()
	defer s.assistantDependencyMu.Unlock()
	result, err := assistantDependencyBotsLocalize(r.Context(), target, req)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	s.writeJSONFor(w, r, result)
}

func validateAssistantDependencyBotsUpdate(req assistantDependencyBotsUpdateRequest) error {
	if req.Name != strings.TrimSpace(req.Name) || !assistantDependencyName.MatchString(req.Name) {
		return fmt.Errorf("dependency name is invalid")
	}
	// Do not trim here: a model-supplied uppercase, abbreviated or padded SHA
	// must be rejected, never normalized into a different immutable object.
	if !assistantDependencySHA.MatchString(req.Ref) {
		return fmt.Errorf("ref must be a lowercase 40-character Git commit SHA")
	}
	if req.Message != strings.TrimSpace(req.Message) || !utf8.ValidString(req.Message) || len(req.Message) == 0 || len(req.Message) > 240 || strings.ContainsAny(req.Message, "\r\n\x00") {
		return fmt.Errorf("message must be one non-empty line of at most 240 bytes")
	}
	return nil
}

func validateAssistantDependencyBotsLocalize(req assistantDependencyBotsLocalizeRequest) error {
	if strings.TrimSpace(req.EditorPath) == "" {
		return errors.New("editor_path is required")
	}
	if req.Name != strings.TrimSpace(req.Name) || !assistantDependencyName.MatchString(req.Name) {
		return fmt.Errorf("dependency name is invalid")
	}
	if req.Message != strings.TrimSpace(req.Message) || !utf8.ValidString(req.Message) || len(req.Message) == 0 || len(req.Message) > 240 || strings.ContainsAny(req.Message, "\r\n\x00") {
		return fmt.Errorf("message must be one non-empty line of at most 240 bytes")
	}
	return nil
}

func assistantDependencyBotsUpdate(ctx context.Context, workdir string, req assistantDependencyBotsUpdateRequest) (assistantDependencyBotsUpdateResponse, error) {
	rootOut, err := assistantDependencyGit(ctx, workdir, "rev-parse", "--show-toplevel")
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(rootOut))
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("resolve project Git root: %w", err)
	}
	lockPath := filepath.Join(workdir, botlock.FileName)
	lockRel, err := filepath.Rel(root, lockPath)
	if err != nil || lockRel == ".." || strings.HasPrefix(filepath.ToSlash(lockRel), "../") {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("bots.lock is outside the project Git root")
	}
	lockRel = filepath.ToSlash(lockRel)
	if clean, err := assistantDependencyGitQuiet(ctx, root, "diff", "--quiet", "--", lockRel); err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	} else if !clean {
		return assistantDependencyBotsUpdateResponse{}, authoringConflictError{"bots.lock has uncommitted changes"}
	}
	if clean, err := assistantDependencyGitQuiet(ctx, root, "diff", "--cached", "--quiet"); err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	} else if !clean {
		return assistantDependencyBotsUpdateResponse{}, authoringConflictError{"staged changes block the dependency update"}
	}
	beforeLock, err := os.ReadFile(lockPath)
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	beforeHash := contentSHA256(string(beforeLock))
	beforeLockModel, err := botlock.Load(workdir)
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	previous, exists := beforeLockModel.Dependencies[req.Name]
	if !exists {
		return assistantDependencyBotsUpdateResponse{}, authoringConflictError{fmt.Sprintf("dependency %q is not declared in bots.lock", req.Name)}
	}
	if previous.Ref == req.Ref {
		return assistantDependencyBotsUpdateResponse{}, authoringConflictError{"bots.lock already pins this exact commit"}
	}
	otherPaths := []string{".", ":(exclude)" + lockRel, ":(exclude).botz"}
	beforeOtherDiff, err := assistantDependencyGit(ctx, root, append([]string{"diff", "--no-ext-diff", "--binary", "--"}, otherPaths...)...)
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	// .botz is the intentionally materialized, non-source cache for the one
	// locked bundle. It may be untracked in a project that has not ignored it;
	// no other path is exempt from this before/after equality check.
	beforeOtherStatus, err := assistantDependencyGit(ctx, root, append([]string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--"}, otherPaths...)...)
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}

	updated, err := botdeps.Update(ctx, botdeps.UpdateOptions{Workdir: workdir, Name: req.Name, Ref: req.Ref})
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	if updated.Name != req.Name || updated.Ref != req.Ref {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("dependency updater returned an unexpected pin")
	}
	afterLock, err := os.ReadFile(lockPath)
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("updated bots.lock could not be read: %w", err)
	}
	afterHash := contentSHA256(string(afterLock))
	if afterHash == beforeHash {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("dependency update left bots.lock unchanged")
	}
	afterLockModel, err := botlock.Load(workdir)
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("updated bots.lock is invalid: %w", err)
	}
	after, exists := afterLockModel.Dependencies[req.Name]
	if !exists || after.Ref != req.Ref || after.BundleSHA256 != updated.BundleSHA256 {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("updated bots.lock does not contain the verified dependency pin")
	}
	afterOtherDiff, err := assistantDependencyGit(ctx, root, append([]string{"diff", "--no-ext-diff", "--binary", "--"}, otherPaths...)...)
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	afterOtherStatus, err := assistantDependencyGit(ctx, root, append([]string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--"}, otherPaths...)...)
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	if afterOtherDiff != beforeOtherDiff || afterOtherStatus != beforeOtherStatus {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("dependency update changed files outside bots.lock (diff_changed=%t status_changed=%t); no rollback was attempted", afterOtherDiff != beforeOtherDiff, afterOtherStatus != beforeOtherStatus)
	}
	// Re-read immediately before staging so a concurrent edit cannot be
	// silently committed under the assistant's receipt.
	lockBeforeStage, err := os.ReadFile(lockPath)
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	if contentSHA256(string(lockBeforeStage)) != afterHash {
		return assistantDependencyBotsUpdateResponse{}, authoringConflictError{"bots.lock changed concurrently before staging"}
	}
	if _, err := assistantDependencyGit(ctx, root, "add", "--", lockRel); err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	stagedNames, err := assistantDependencyGit(ctx, root, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	if !sameGitPathSet(splitGitPathList(stagedNames), []string{lockRel}) {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("staged paths diverged from bots.lock; no rollback was attempted")
	}
	stagedLock, err := assistantDependencyGit(ctx, root, "show", ":"+lockRel)
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	if contentSHA256(stagedLock) != afterHash {
		return assistantDependencyBotsUpdateResponse{}, authoringConflictError{"staged bots.lock differs from the verified update"}
	}
	if _, err := assistantDependencyGit(ctx, root, "commit", "--only", "-m", req.Message, "--", lockRel); err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	commit, err := assistantDependencyGit(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, err
	}
	commit = strings.TrimSpace(commit)
	committedNames, err := assistantDependencyGit(ctx, root, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "HEAD")
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("commit %s exists but changed paths could not be verified: %w", commit, err)
	}
	if !sameGitPathSet(splitGitPathList(committedNames), []string{lockRel}) {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("commit %s exists but changed paths diverged from bots.lock; no rollback was attempted", commit)
	}
	committedLock, err := assistantDependencyGit(ctx, root, "show", "HEAD:"+lockRel)
	if err != nil {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("commit %s exists but bots.lock could not be verified: %w", commit, err)
	}
	if contentSHA256(committedLock) != afterHash {
		return assistantDependencyBotsUpdateResponse{}, fmt.Errorf("commit %s exists but bots.lock differs from the verified update; no rollback was attempted", commit)
	}
	return assistantDependencyBotsUpdateResponse{
		Name: req.Name, PreviousRef: previous.Ref, Ref: updated.Ref,
		BundleSHA256: updated.BundleSHA256, Commit: commit, InstalledPath: updated.InstalledPath,
	}, nil
}

// assistantDependencyBotsLocalize imports the already verified materialized
// copy of one dependency into a fixed plugins/<name> directory beneath the
// open consumer bundle. Keeping the lock name and bot:// URI intact means an
// in-flight run can still resolve its child through the normal lock/cache
// contract, while future Copi sessions edit a first-class local source.
func assistantDependencyBotsLocalize(ctx context.Context, target *authoringTarget, req assistantDependencyBotsLocalizeRequest) (assistantDependencyBotsLocalizeResponse, error) {
	if target == nil || target.files != nil || target.manifest == nil || target.bundleDir == "" || target.workDir == "" {
		return assistantDependencyBotsLocalizeResponse{}, errors.New("dependency localization requires an open local consumer bundle")
	}
	if !manifestDeclaresWorkflowDependency(target.manifest, req.Name) {
		return assistantDependencyBotsLocalizeResponse{}, authoringConflictError{fmt.Sprintf("open bundle does not declare workflow dependency %q", req.Name)}
	}
	gitRootOut, err := assistantDependencyGit(ctx, target.workDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	gitRoot, err := filepath.EvalSymlinks(strings.TrimSpace(gitRootOut))
	if err != nil {
		return assistantDependencyBotsLocalizeResponse{}, fmt.Errorf("resolve project Git root: %w", err)
	}
	if !pathContains(target.workDir, target.bundleDir) {
		return assistantDependencyBotsLocalizeResponse{}, errors.New("open consumer bundle is outside the active project")
	}
	lockPath := filepath.Join(target.workDir, botlock.FileName)
	lockRel, err := filepath.Rel(gitRoot, lockPath)
	if err != nil || lockRel == ".." || strings.HasPrefix(filepath.ToSlash(lockRel), "../") {
		return assistantDependencyBotsLocalizeResponse{}, errors.New("bots.lock is outside the active Git repository")
	}
	lockRel = filepath.ToSlash(lockRel)
	if clean, err := assistantDependencyGitQuiet(ctx, gitRoot, "diff", "--quiet", "--", lockRel); err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	} else if !clean {
		return assistantDependencyBotsLocalizeResponse{}, authoringConflictError{"bots.lock has uncommitted changes"}
	}
	lockBefore, err := os.ReadFile(lockPath)
	if err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	lock, err := botlock.Load(target.workDir)
	if err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	dep, ok := lock.Dependencies[req.Name]
	if !ok {
		return assistantDependencyBotsLocalizeResponse{}, authoringConflictError{fmt.Sprintf("dependency %q is not declared in bots.lock", req.Name)}
	}
	installedPath := filepath.Join(target.workDir, ".botz", req.Name)
	installedBundle, err := bundle.OpenDir(installedPath)
	if err != nil || installedBundle.Manifest == nil || installedBundle.Manifest.Name != req.Name {
		return assistantDependencyBotsLocalizeResponse{}, authoringConflictError{fmt.Sprintf("installed dependency %q is unavailable or invalid", req.Name)}
	}
	installedHash, err := bundle.ContentHashDir(installedPath)
	if err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	if installedHash != dep.BundleSHA256 {
		return assistantDependencyBotsLocalizeResponse{}, authoringConflictError{fmt.Sprintf("installed dependency %q does not match bots.lock", req.Name)}
	}

	pluginsDir := filepath.Join(target.bundleDir, "plugins")
	if info, statErr := os.Lstat(pluginsDir); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return assistantDependencyBotsLocalizeResponse{}, authoringConflictError{"derived plugins directory must not be a symlink"}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return assistantDependencyBotsLocalizeResponse{}, statErr
	}
	localDir := filepath.Join(pluginsDir, req.Name)
	localRel, err := filepath.Rel(gitRoot, localDir)
	if err != nil || localRel == "." || localRel == ".." || strings.HasPrefix(filepath.ToSlash(localRel), "../") {
		return assistantDependencyBotsLocalizeResponse{}, errors.New("derived local dependency path is outside the active Git repository")
	}
	localRel = filepath.ToSlash(localRel)
	pluginsRel, err := filepath.Rel(target.workDir, pluginsDir)
	if err != nil || pluginsRel == "." || pluginsRel == ".." || strings.HasPrefix(filepath.ToSlash(pluginsRel), "../") {
		return assistantDependencyBotsLocalizeResponse{}, errors.New("derived local dependency source is outside the active project")
	}
	pluginsRel = filepath.ToSlash(pluginsRel)
	localFiles, err := dependencyLocalizationFiles(installedPath)
	if err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	expectedGitFiles := dependencyLocalizationGitFiles(localRel, localFiles)
	stagedNames, err := assistantDependencyGit(ctx, gitRoot, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	stagedPaths := splitGitPathList(stagedNames)

	resumedAfterImport := false
	sourceCommit := ""
	if info, err := os.Lstat(localDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return assistantDependencyBotsLocalizeResponse{}, authoringConflictError{fmt.Sprintf("localized source path %s must not be a symlink", localRel)}
		}
		if dep.Source == pluginsRel && dep.Path == req.Name {
			return assistantDependencyBotsLocalizeResponse{}, authoringConflictError{fmt.Sprintf("dependency %q is already localized at %s", req.Name, filepath.ToSlash(filepath.Join(pluginsRel, req.Name)))}
		}
		if len(stagedPaths) > 0 {
			sourceCommit, err = resumableStagedLocalizationImport(ctx, gitRoot, localDir, localRel, expectedGitFiles, installedHash, req.Message)
		} else {
			sourceCommit, err = resumableLocalizationImport(ctx, gitRoot, localDir, localRel, expectedGitFiles, installedHash)
		}
		if err != nil {
			return assistantDependencyBotsLocalizeResponse{}, err
		}
		resumedAfterImport = true
	} else if !os.IsNotExist(err) {
		return assistantDependencyBotsLocalizeResponse{}, err
	} else {
		if len(stagedPaths) > 0 {
			return assistantDependencyBotsLocalizeResponse{}, authoringConflictError{"staged changes block dependency localization"}
		}
		if err := copyDependencyLocalizationTree(installedPath, localDir); err != nil {
			return assistantDependencyBotsLocalizeResponse{}, err
		}
		localHash, err := bundle.ContentHashDir(localDir)
		if err != nil {
			return assistantDependencyBotsLocalizeResponse{}, err
		}
		if localHash != installedHash {
			return assistantDependencyBotsLocalizeResponse{}, fmt.Errorf("localized dependency hash differs from the verified materialization")
		}
		if _, err := assistantDependencyGit(ctx, gitRoot, "add", "--", localRel); err != nil {
			return assistantDependencyBotsLocalizeResponse{}, err
		}
		if err := requireStagedDependencyPaths(ctx, gitRoot, expectedGitFiles); err != nil {
			return assistantDependencyBotsLocalizeResponse{}, err
		}
		if _, err := assistantDependencyGit(ctx, gitRoot, "commit", "--only", "-m", req.Message, "--", localRel); err != nil {
			return assistantDependencyBotsLocalizeResponse{}, err
		}
		sourceCommit, err = assistantDependencyGit(ctx, gitRoot, "rev-parse", "HEAD")
		if err != nil {
			return assistantDependencyBotsLocalizeResponse{}, err
		}
		sourceCommit = strings.TrimSpace(sourceCommit)
		if err := requireCommitDependencyPaths(ctx, gitRoot, sourceCommit, expectedGitFiles); err != nil {
			return assistantDependencyBotsLocalizeResponse{}, err
		}
	}

	// A retry after the first, source-only commit is deliberately resumable.
	// From this point, a failure restores the old lock bytes but never removes
	// that confirmed source commit or touches unrelated user work.
	dep.Source = pluginsRel
	dep.Path = req.Name
	dep.Ref = sourceCommit
	dep.BundleSHA256 = installedHash
	lock.Dependencies[req.Name] = dep
	if err := botlock.Save(target.workDir, lock); err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	synced, err := botdeps.SyncOne(ctx, target.workDir, req.Name, dep)
	if err != nil {
		if restoreErr := restoreAssistantDependencyLock(lockPath, lockBefore, 0o644); restoreErr != nil {
			return assistantDependencyBotsLocalizeResponse{}, fmt.Errorf("%v; additionally failed to restore bots.lock exactly: %w", err, restoreErr)
		}
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	verifiedLock, err := botlock.Load(target.workDir)
	if err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	verified, ok := verifiedLock.Dependencies[req.Name]
	if !ok || verified.Source != pluginsRel || verified.Path != req.Name || verified.Ref != sourceCommit || verified.BundleSHA256 != installedHash {
		return assistantDependencyBotsLocalizeResponse{}, errors.New("localized bots.lock does not contain the verified local dependency")
	}
	materializedHash, err := bundle.ContentHashDir(synced.InstalledPath)
	if err != nil || materializedHash != installedHash {
		return assistantDependencyBotsLocalizeResponse{}, errors.New("localized dependency failed materialization hash verification")
	}
	if _, err := assistantDependencyGit(ctx, gitRoot, "add", "--", lockRel); err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	if err := requireStagedDependencyPaths(ctx, gitRoot, []string{lockRel}); err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	lockMessage := fmt.Sprintf("chore(bots): localize %s source", req.Name)
	if _, err := assistantDependencyGit(ctx, gitRoot, "commit", "--only", "-m", lockMessage, "--", lockRel); err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	lockCommit, err := assistantDependencyGit(ctx, gitRoot, "rev-parse", "HEAD")
	if err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	lockCommit = strings.TrimSpace(lockCommit)
	if err := requireCommitDependencyPaths(ctx, gitRoot, lockCommit, []string{lockRel}); err != nil {
		return assistantDependencyBotsLocalizeResponse{}, err
	}
	return assistantDependencyBotsLocalizeResponse{
		Name: req.Name, LocalSource: filepath.ToSlash(filepath.Join(pluginsRel, req.Name)), SourceCommit: sourceCommit,
		LockCommit: lockCommit, BundleSHA256: installedHash, InstalledPath: synced.InstalledPath, ResumedAfterImport: resumedAfterImport,
	}, nil
}

func manifestDeclaresWorkflowDependency(m *bundle.Manifest, name string) bool {
	for _, dep := range m.Dependencies.Workflows {
		if dep.Name == name {
			return true
		}
	}
	return false
}

func dependencyLocalizationFiles(root string) ([]string, error) {
	files := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("materialized dependency contains unsupported symlink %q", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if bundle.IsPackSkipped(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("materialized dependency contains unsupported non-regular file %q", path)
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, errors.New("materialized dependency has no files")
	}
	return files, nil
}

func dependencyLocalizationGitFiles(root string, files []string) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, filepath.ToSlash(filepath.Join(root, file)))
	}
	sort.Strings(out)
	return out
}

func copyDependencyLocalizationTree(source, destination string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("materialized dependency contains unsupported symlink %q", path)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if bundle.IsPackSkipped(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, filepath.FromSlash(rel))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("materialized dependency contains unsupported non-regular file %q", path)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func requireStagedDependencyPaths(ctx context.Context, gitRoot string, expected []string) error {
	staged, err := assistantDependencyGit(ctx, gitRoot, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return err
	}
	if !sameGitPathSet(splitGitPathList(staged), expected) {
		return errors.New("staged paths diverged from the bounded dependency localization scope")
	}
	return nil
}

func requireCommitDependencyPaths(ctx context.Context, gitRoot, commit string, expected []string) error {
	changed, err := assistantDependencyGit(ctx, gitRoot, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", commit)
	if err != nil {
		return err
	}
	if !sameGitPathSet(splitGitPathList(changed), expected) {
		return fmt.Errorf("commit %s changed paths outside the bounded dependency localization scope", commit)
	}
	return nil
}

// resumableLocalizationImport accepts only the exact partial state produced
// after the source-import commit and before the lock commit. This makes a
// retry useful without treating an arbitrary existing plugins/<name> tree as
// assistant-owned.
func resumableLocalizationImport(ctx context.Context, gitRoot, localDir, localRel string, expected []string, expectedHash string) (string, error) {
	localHash, err := bundle.ContentHashDir(localDir)
	if err != nil || localHash != expectedHash {
		return "", authoringConflictError{fmt.Sprintf("localized source already exists at %s but does not match the verified dependency", localRel)}
	}
	if clean, err := assistantDependencyGitQuiet(ctx, gitRoot, "diff", "--quiet", "--", localRel); err != nil {
		return "", err
	} else if !clean {
		return "", authoringConflictError{fmt.Sprintf("localized source already exists at %s with uncommitted changes", localRel)}
	}
	head, err := assistantDependencyGit(ctx, gitRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	head = strings.TrimSpace(head)
	if err := requireCommitDependencyPaths(ctx, gitRoot, head, expected); err != nil {
		return "", authoringConflictError{fmt.Sprintf("localized source already exists at %s; its import commit cannot be resumed safely: %v", localRel, err)}
	}
	return head, nil
}

// resumableStagedLocalizationImport is the one recoverable state before the
// first source commit: every staged path must be an exact logical bundle file
// beneath the fixed local source, the worktree must still match that index,
// and the source must hash to the materialization already verified from the
// lock. This permits retrying an interrupted host import without accepting or
// clearing arbitrary operator staging.
func resumableStagedLocalizationImport(ctx context.Context, gitRoot, localDir, localRel string, expected []string, expectedHash, message string) (string, error) {
	if err := requireStagedDependencyPaths(ctx, gitRoot, expected); err != nil {
		return "", authoringConflictError{fmt.Sprintf("localized source already exists at %s with staged changes outside the verified import", localRel)}
	}
	localHash, err := bundle.ContentHashDir(localDir)
	if err != nil || localHash != expectedHash {
		return "", authoringConflictError{fmt.Sprintf("localized source already exists at %s but does not match the verified dependency", localRel)}
	}
	if clean, err := assistantDependencyGitQuiet(ctx, gitRoot, "diff", "--quiet", "--", localRel); err != nil {
		return "", err
	} else if !clean {
		return "", authoringConflictError{fmt.Sprintf("localized source already exists at %s with unstaged changes", localRel)}
	}
	if _, err := assistantDependencyGit(ctx, gitRoot, "commit", "--only", "-m", message, "--", localRel); err != nil {
		return "", err
	}
	commit, err := assistantDependencyGit(ctx, gitRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	commit = strings.TrimSpace(commit)
	if err := requireCommitDependencyPaths(ctx, gitRoot, commit, expected); err != nil {
		return "", fmt.Errorf("recovered source commit %s changed paths outside the bounded dependency localization scope: %w", commit, err)
	}
	return commit, nil
}

func restoreAssistantDependencyLock(path string, contents []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bots.lock-restore-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func assistantDependencyGit(ctx context.Context, workdir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workdir}, args...)...)
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func assistantDependencyGitQuiet(ctx context.Context, workdir string, args ...string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workdir}, args...)...)
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
}
