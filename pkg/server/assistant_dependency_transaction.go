package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/botdeps"
	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// This is a local-workspace transaction, not atomic reader visibility or crash
// recovery. Readers can briefly observe a missing cache or lock/hash mismatch.
// Git's index lock serializes cooperating writers; observable independent edits
// (including ignored runtime caches) withhold joint restoration. Retained private
// material and record.json explain recovery after interference or process death.
type assistantDependencyTransaction struct {
	originParent, originPath, stagedOrigin, backupOrigin                string
	oldOriginLocation, newOriginLocation                                string
	originalOrigin, candidateOrigin                                     dependencyObject
	originParentInfo, stageOriginInfo, workdirInfo                      os.FileInfo
	originParentCreated                                                 bool
	root, workdir, lockPath, lockRel, cachePath, cacheParent, indexPath string
	head, binding, otherDiff, otherStatus                               string
	discovery                                                           []string
	originalLock, candidateLock                                         []byte
	lockState, indexState, originalCache, candidateCache                dependencyObject
	parentInfo                                                          os.FileInfo
	parentCreated                                                       bool
	dir, stageParent, stagePath, backupPath                             string
	dirInfo, stageInfo                                                  os.FileInfo
	recordState, originalBytesState, candidateBytesState                dependencyObject
	oldLocation, newLocation                                            string
	phase                                                               string
	mutated, retain                                                     bool
	// Per-invocation filesystem seam also allows deterministic rename failures.
	rename func(string, string) error
}

type dependencyObject struct {
	info os.FileInfo
	hash string
}

func dependencySnapshot(ctx context.Context, path string) (dependencyObject, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return dependencyObject{}, nil
	}
	if err != nil {
		return dependencyObject{}, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return dependencyObject{}, fmt.Errorf("unsupported dependency object %s", path)
	}
	h := sha256.New()
	err = filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		meta, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, current)
		if err != nil {
			return err
		}
		size := meta.Size()
		if meta.IsDir() {
			size = 0
		}
		fmt.Fprintf(h, "%q:%d:%d:", rel, meta.Mode(), size)
		switch {
		case meta.IsDir(): // Directory size is not stable across rename/child removal.
			// Directory entries are represented by their children below.
		case meta.Mode().IsRegular():
			file, err := os.Open(current)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(h, file)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
		case meta.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(current)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%q", target)
		default:
			return fmt.Errorf("unsupported dependency object %s", current)
		}
		return nil
	})
	if err != nil {
		return dependencyObject{}, err
	}
	return dependencyObject{info: info, hash: hex.EncodeToString(h.Sum(nil))}, nil
}

func (o dependencyObject) matches(ctx context.Context, path string) error {
	current, err := dependencySnapshot(ctx, path)
	if err != nil {
		return err
	}
	if o.info == nil && current.info == nil {
		return nil
	}
	if o.info == nil || current.info == nil || !os.SameFile(o.info, current.info) || o.hash != current.hash {
		return authoringConflictError{fmt.Sprintf("dependency transaction lost ownership of %s", path)}
	}
	return nil
}

func dependencyDirectory(path string, expected os.FileInfo) error {
	info, err := os.Lstat(path)
	if expected == nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if expected == nil || !info.IsDir() || !os.SameFile(expected, info) || info.Mode() != expected.Mode() {
		return authoringConflictError{fmt.Sprintf("dependency directory changed: %s", path)}
	}
	return nil
}

func prepareAssistantDependencyTransaction(ctx context.Context, root, workdir, lockRel string, discovery []string) (_ *assistantDependencyTransaction, err error) {
	t := &assistantDependencyTransaction{root: root, workdir: workdir, lockRel: lockRel, lockPath: filepath.Join(root, filepath.FromSlash(lockRel)), cacheParent: filepath.Join(workdir, ".botz"), discovery: append([]string(nil), discovery...), phase: "preparation", rename: nil}
	t.rename = func(from, to string) error {
		return renameDependencyNoReplace(from, to, t.directoryInfo(filepath.Dir(from)), t.directoryInfo(filepath.Dir(to)))
	}
	t.workdirInfo, err = os.Lstat(workdir)
	if err != nil {
		return nil, err
	}

	t.lockState, err = dependencySnapshot(ctx, t.lockPath)
	if err != nil {
		return nil, err
	}
	if t.lockState.info == nil || !t.lockState.info.Mode().IsRegular() {
		return nil, errors.New("bots.lock must be a regular tracked file")
	}
	t.originalLock, err = os.ReadFile(t.lockPath)
	if err != nil {
		return nil, err
	}
	if err = t.lockState.matches(ctx, t.lockPath); err != nil {
		return nil, err
	}
	t.parentInfo, err = os.Lstat(t.cacheParent)
	if errors.Is(err, os.ErrNotExist) {
		t.parentInfo = nil
	} else if err != nil {
		return nil, err
	} else if !t.parentInfo.IsDir() {
		return nil, errors.New(".botz must be a real directory, not a symlink")
	}
	head, err := assistantDependencyGit(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	t.head = strings.TrimSpace(head)
	t.binding, err = authoringHeadBinding(ctx, authoringGitIndex{root: root})
	if err != nil {
		return nil, err
	}
	index, err := assistantDependencyGit(ctx, root, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return nil, err
	}
	t.indexPath = strings.TrimSpace(index)
	t.otherDiff, t.otherStatus, err = t.otherState(ctx)
	if err != nil {
		return nil, err
	}
	t.indexState, err = dependencySnapshot(ctx, t.indexPath)
	if err != nil {
		return nil, err
	}
	gitDir, err := assistantDependencyGit(ctx, root, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return nil, err
	}
	administrative, err := filepath.EvalSymlinks(strings.TrimSpace(gitDir))
	if err != nil {
		return nil, err
	}
	if err = t.checkStorage(administrative); err != nil {
		return nil, err
	}
	t.dir, err = os.MkdirTemp(administrative, "iterion-dependency-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(t.dir)
		}
	}()
	t.dirInfo, err = os.Lstat(t.dir)
	if err != nil {
		return nil, err
	}
	t.stageParent = filepath.Join(t.dir, "stage")
	if err = os.Mkdir(t.stageParent, 0o700); err != nil {
		return nil, err
	}
	t.stageInfo, err = os.Lstat(t.stageParent)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(t.dir, "original.lock"), t.originalLock, 0o600); err != nil {
		return nil, err
	}
	t.originalBytesState, err = dependencySnapshot(ctx, filepath.Join(t.dir, "original.lock"))
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (t *assistantDependencyTransaction) prepare(ctx context.Context, req assistantDependencyBotsUpdateRequest) (*botdeps.PreparedUpdate, error) {
	t.cachePath = filepath.Join(t.cacheParent, req.Name)
	t.stagePath = filepath.Join(t.stageParent, req.Name)
	t.backupPath = filepath.Join(t.dir, "original-cache")
	t.originParent = filepath.Join(t.cacheParent, ".origins")
	t.originPath = filepath.Join(t.originParent, req.Name+".json")
	t.stagedOrigin = filepath.Join(t.stageParent, ".origins", req.Name+".json")
	t.backupOrigin = filepath.Join(t.dir, "original-origin.json")
	var err error
	t.originParentInfo, err = os.Lstat(t.originParent)
	if errors.Is(err, os.ErrNotExist) {
		t.originParentInfo = nil
	} else if err != nil {
		return nil, err
	} else if !t.originParentInfo.IsDir() {
		return nil, errors.New(".origins must be a real directory, not a symlink")
	}
	t.originalOrigin, err = dependencySnapshot(ctx, t.originPath)
	if err != nil {
		return nil, err
	}
	if t.originalOrigin.info != nil {
		if !t.originalOrigin.info.Mode().IsRegular() {
			return nil, errors.New("dependency origin must be a regular file")
		}
		t.oldOriginLocation = t.originPath
	}
	t.originalCache, err = dependencySnapshot(ctx, t.cachePath)
	if err != nil {
		return nil, err
	}
	if t.originalCache.info != nil {
		if !t.originalCache.info.IsDir() {
			return nil, errors.New("installed dependency must be a real directory")
		}
		if _, err = bundle.ContentHashDir(t.cachePath); err != nil {
			return nil, err
		}
		t.oldLocation = t.cachePath
	}
	prepared, err := botdeps.PrepareUpdate(ctx, botdeps.UpdateOptions{Workdir: t.workdir, Name: req.Name, Ref: req.Ref})
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(prepared.OriginalLock, t.originalLock) || prepared.OriginalMode != t.lockState.info.Mode().Perm() || prepared.Dependency.Ref != req.Ref || bytes.Equal(prepared.CandidateLock, t.originalLock) {
		return nil, authoringConflictError{"dependency preparation does not match the original lock and requested pin"}
	}
	t.candidateLock = prepared.CandidateLock
	if err = os.WriteFile(filepath.Join(t.dir, "candidate.lock"), t.candidateLock, 0o600); err != nil {
		return nil, err
	}
	t.candidateBytesState, err = dependencySnapshot(ctx, filepath.Join(t.dir, "candidate.lock"))
	if err != nil {
		return nil, err
	}
	if _, err = botdeps.StageOne(ctx, t.workdir, t.stageParent, req.Name, prepared.Dependency); err != nil {
		return nil, err
	}
	t.candidateCache, err = dependencySnapshot(ctx, t.stagePath)
	if err != nil {
		return nil, err
	}
	t.newLocation = t.stagePath

	t.stageOriginInfo, err = os.Lstat(filepath.Dir(t.stagedOrigin))
	if err != nil {
		return nil, err
	}
	t.candidateOrigin, err = dependencySnapshot(ctx, t.stagedOrigin)
	if err != nil {
		return nil, err
	}
	provenance, err := botinstall.ReadOrigin(t.stagePath)
	if err != nil {
		return nil, err
	}
	if provenance.Ref != prepared.Dependency.Ref || provenance.SourcePath != prepared.Dependency.Path || provenance.Source == "" {
		return nil, errors.New("candidate origin differs from verified dependency")
	}
	t.newOriginLocation = t.stagedOrigin

	if err = t.record(); err != nil {
		return nil, err
	}
	return prepared, nil
}

func (t *assistantDependencyTransaction) otherState(ctx context.Context) (string, string, error) {
	cacheRel, err := filepath.Rel(t.root, t.cacheParent)
	if err != nil {
		return "", "", err
	}
	paths := []string{".", ":(exclude)" + t.lockRel, ":(exclude)" + filepath.ToSlash(cacheRel)}
	diff, err := assistantDependencyGit(ctx, t.root, append([]string{"--no-optional-locks", "diff", "--no-ext-diff", "--binary", "--"}, paths...)...)
	if err != nil {
		return "", "", err
	}
	status, err := assistantDependencyGit(ctx, t.root, append([]string{"--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--"}, paths...)...)
	return diff, status, err
}

func (t *assistantDependencyTransaction) checkStorage(private string) error {
	roots := append(botregistry.DefaultPaths(t.workdir), t.discovery...)
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		real, err := evalSymlinksLongestPrefix(absolute)
		if err != nil {
			return err
		}
		if pathContains(real, private) {
			return authoringConflictError{fmt.Sprintf("private dependency recovery storage is inside discovery root %s", root)}
		}
	}
	parent := t.cacheParent
	if _, err := os.Lstat(parent); errors.Is(err, os.ErrNotExist) {
		parent = t.workdir
	} else if err != nil {
		return err
	}
	return requireDependencySameFilesystem(private, parent)
}

func (t *assistantDependencyTransaction) record() error {
	record := map[string]any{
		"phase": t.phase, "parent": t.head, "binding": t.binding, "lock_path": t.lockPath, "lock_mode": t.lockState.info.Mode().Perm(),
		"original_lock_sha256": contentSHA256(string(t.originalLock)), "candidate_lock_sha256": contentSHA256(string(t.candidateLock)),
		"cache_path": t.cachePath, "original_cache": t.oldLocation, "candidate_cache": t.newLocation,
		"original_cache_fingerprint": t.originalCache.hash, "candidate_cache_fingerprint": t.candidateCache.hash,
		"origin_path": t.originPath, "original_origin": t.oldOriginLocation, "candidate_origin": t.newOriginLocation,
		"original_origin_fingerprint": t.originalOrigin.hash, "candidate_origin_fingerprint": t.candidateOrigin.hash,
		"created_cache_parent": t.parentCreated, "created_origin_parent": t.originParentCreated,
	}

	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err = restoreAssistantDependencyLock(filepath.Join(t.dir, "record.json"), body, 0o600); err != nil {
		return err
	}
	t.recordState, err = dependencySnapshot(context.Background(), filepath.Join(t.dir, "record.json"))
	return err
}

func (t *assistantDependencyTransaction) verifyObjects(ctx context.Context, live bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if live {
		if err := dependencyDirectory(t.cacheParent, t.parentInfo); err != nil {
			return err
		}
		if err := dependencyDirectory(t.originParent, t.originParentInfo); err != nil {
			return err
		}
		if t.originParentInfo != nil {
			if err := requireDependencySameFilesystem(t.dir, t.originParent); err != nil {
				return err
			}
		}

	}
	if err := dependencyDirectory(filepath.Dir(t.stagedOrigin), t.stageOriginInfo); err != nil {
		return err
	}

	if err := dependencyDirectory(t.dir, t.dirInfo); err != nil {
		return err
	}
	if err := dependencyDirectory(t.stageParent, t.stageInfo); err != nil {
		return err
	}
	if live {
		if err := t.checkStorage(t.dir); err != nil {
			return err
		}
	}
	for path, object := range map[string]dependencyObject{t.lockPath: t.lockState, filepath.Join(t.dir, "record.json"): t.recordState, filepath.Join(t.dir, "original.lock"): t.originalBytesState, filepath.Join(t.dir, "candidate.lock"): t.candidateBytesState} {
		if !live && !pathContains(t.dir, path) {
			continue
		}
		if err := object.matches(ctx, path); err != nil {
			return err
		}
	}
	if t.oldLocation != "" && (live || pathContains(t.dir, t.oldLocation)) {
		if err := t.originalCache.matches(ctx, t.oldLocation); err != nil {
			return err
		}
	}
	if t.newLocation != "" && (live || pathContains(t.dir, t.newLocation)) {
		if err := t.candidateCache.matches(ctx, t.newLocation); err != nil {
			return err
		}
	}
	if t.oldOriginLocation != "" && (live || pathContains(t.dir, t.oldOriginLocation)) {
		if err := t.originalOrigin.matches(ctx, t.oldOriginLocation); err != nil {
			return err
		}
	}
	if t.newOriginLocation != "" && (live || pathContains(t.dir, t.newOriginLocation)) {
		if err := t.candidateOrigin.matches(ctx, t.newOriginLocation); err != nil {
			return err
		}
	}
	for _, path := range []string{t.originPath, t.backupOrigin, t.stagedOrigin} {
		if !live && !pathContains(t.dir, path) {
			continue
		}
		if path != t.oldOriginLocation && path != t.newOriginLocation {
			if err := (dependencyObject{}).matches(ctx, path); err != nil {
				return err
			}
		}
	}

	for _, path := range []string{t.cachePath, t.backupPath, t.stagePath} {
		if !live && !pathContains(t.dir, path) {
			continue
		}
		if path != t.oldLocation && path != t.newLocation {
			if err := (dependencyObject{}).matches(ctx, path); err != nil {
				return err
			}
		}
	}
	// Unexpected private files are independent work too; never delete them.
	for dir, allowed := range map[string]map[string]bool{t.dir: {"record.json": true, "original.lock": true, "candidate.lock": true, "stage": true, "original-cache": t.oldLocation == t.backupPath, "original-origin.json": t.oldOriginLocation == t.backupOrigin}, t.stageParent: {filepath.Base(t.stagePath): t.newLocation == t.stagePath, ".origins": true}, filepath.Dir(t.stagedOrigin): {filepath.Base(t.stagedOrigin): t.newOriginLocation == t.stagedOrigin}} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !allowed[entry.Name()] {
				return fmt.Errorf("unexpected recovery artifact %s", filepath.Join(dir, entry.Name()))
			}
		}
	}
	return nil
}

func (t *assistantDependencyTransaction) verify(ctx context.Context, originalGit bool) error {
	if err := t.verifyObjects(ctx, true); err != nil {
		return err
	}
	if originalGit {
		head, err := assistantDependencyGit(ctx, t.root, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return err
		}
		binding, err := authoringHeadBinding(ctx, authoringGitIndex{root: t.root})
		if err != nil {
			return err
		}
		if strings.TrimSpace(head) != t.head || binding != t.binding {
			return authoringConflictError{"HEAD changed during dependency update"}
		}
		if err = t.indexState.matches(ctx, t.indexPath); err != nil {
			return err
		}
	}
	return nil
}

func (t *assistantDependencyTransaction) install(ctx context.Context, parent, binding string) error {
	if parent != t.head || binding != t.binding {
		return authoringConflictError{"HEAD changed during dependency preparation"}
	}
	if err := t.verify(ctx, true); err != nil {
		return err
	}
	staged, err := assistantDependencyGit(ctx, t.root, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return err
	}
	if staged != "" {
		return authoringConflictError{"staged changes block the dependency update"}
	}
	diff, status, err := t.otherState(ctx)
	if err != nil {
		return err
	}
	if diff != t.otherDiff || status != t.otherStatus {
		return authoringConflictError{"unrelated files changed during dependency preparation"}
	}
	if t.parentInfo == nil {
		info, createErr := createDependencyDirectory(t.workdir, t.workdirInfo, ".botz", 0o755)
		if createErr != nil {
			return createErr
		}
		t.parentCreated = true
		t.mutated = true
		t.parentInfo = info
	}
	if t.oldLocation != "" {
		if err = t.rename(t.cachePath, t.backupPath); err != nil {
			return err
		}
		t.oldLocation = t.backupPath
		t.mutated = true
	}
	t.phase = "original-cache-backed-up"
	if err = t.record(); err != nil {
		return err
	}
	if err = t.verify(ctx, true); err != nil {
		return err
	}
	if err = t.rename(t.stagePath, t.cachePath); err != nil {
		return err
	}
	t.newLocation = t.cachePath
	t.mutated = true
	t.phase = "candidate-cache-installed"
	if err = t.record(); err != nil {
		return err
	}
	if err = t.verify(ctx, true); err != nil {
		return err
	}
	if t.originParentInfo == nil {
		info, createErr := createDependencyDirectory(t.cacheParent, t.parentInfo, ".origins", 0o700)
		if createErr != nil {
			return createErr
		}
		t.originParentCreated = true
		t.originParentInfo = info
		t.phase = "origin-parent-created"
		if err = t.record(); err != nil {
			return err
		}
	}
	if err = t.verify(ctx, true); err != nil {
		return err
	}
	if t.oldOriginLocation != "" {
		if err = t.rename(t.originPath, t.backupOrigin); err != nil {
			return err
		}
		t.oldOriginLocation = t.backupOrigin
		t.phase = "original-origin-backed-up"
		if err = t.record(); err != nil {
			return err
		}
	}
	if err = t.verify(ctx, true); err != nil {
		return err
	}
	if err = t.rename(t.stagedOrigin, t.originPath); err != nil {
		return err
	}
	t.newOriginLocation = t.originPath
	t.phase = "candidate-origin-installed"
	if err = t.record(); err != nil {
		return err
	}
	if err = t.verify(ctx, true); err != nil {
		return err
	}

	if err = t.replaceLock(ctx, t.candidateLock); err != nil {
		return err
	}
	t.phase = "candidate-lock-installed"
	return t.record()
}

func (t *assistantDependencyTransaction) attest(ctx context.Context) error {
	if t.phase != "candidate-lock-installed" && t.phase != "publication-attempt" {
		return errors.New("dependency candidate is not installed")
	}
	if err := t.verify(ctx, true); err != nil {
		return err
	}
	body, err := os.ReadFile(t.lockPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, t.candidateLock) {
		return authoringConflictError{"installed lock differs from the candidate"}
	}
	diff, status, err := t.otherState(ctx)
	if err != nil {
		return err
	}
	if diff != t.otherDiff || status != t.otherStatus {
		return authoringConflictError{"unrelated files changed during dependency commit"}
	}
	return nil
}

func (t *assistantDependencyTransaction) finish(ctx context.Context, published string, cause error) error {
	if !t.mutated {
		return cause
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	var uncertain *authoringPublicationUncertainError
	if published != "" || errors.As(cause, &uncertain) {
		t.phase = "publication-attempt"
		if published != "" {
			t.phase = "published"
		}
		recordErr := t.record()
		if cause != nil || recordErr != nil {
			return t.recoveryError(errors.Join(cause, recordErr), "publication was not rolled back")
		}
		if err := t.verify(finishCtx, false); err != nil {
			return t.recoveryError(err, "commit published; cleanup withheld")
		}
		t.mutated = false
		return nil
	}
	if err := t.verify(finishCtx, true); err != nil {
		return t.recoveryError(errors.Join(cause, err), "joint restoration withheld")
	}
	// Every step updates the owned locations before another fallible operation.
	// Neither original nor candidate is deleted until the pair is restored.
	if t.newOriginLocation == t.originPath {
		if err := t.rename(t.originPath, t.stagedOrigin); err != nil {
			return t.recoveryError(errors.Join(cause, err), "origin recovery failed")
		}
		t.newOriginLocation = t.stagedOrigin
		t.phase = "restoring-origin"
		if err := t.record(); err != nil {
			return t.recoveryError(errors.Join(cause, err), "recovery record failed")
		}
	}
	if err := t.verify(finishCtx, true); err != nil {
		return t.recoveryError(errors.Join(cause, err), "joint restoration interrupted")
	}
	if t.oldOriginLocation == t.backupOrigin {
		if err := t.rename(t.backupOrigin, t.originPath); err != nil {
			return t.recoveryError(errors.Join(cause, err), "original origin recovery failed")
		}
		t.oldOriginLocation = t.originPath
		t.phase = "origin-restored"
		if err := t.record(); err != nil {
			return t.recoveryError(errors.Join(cause, err), "recovery record failed")
		}
	}
	if err := t.verify(finishCtx, true); err != nil {
		return t.recoveryError(errors.Join(cause, err), "joint restoration interrupted")
	}

	if t.newLocation == t.cachePath {
		if err := t.rename(t.cachePath, t.stagePath); err != nil {
			return t.recoveryError(errors.Join(cause, err), "cache recovery failed")
		}
		t.newLocation = t.stagePath
		t.phase = "restoring-cache"
		if err := t.record(); err != nil {
			return t.recoveryError(errors.Join(cause, err), "recovery record failed")
		}
	}
	if err := t.verify(finishCtx, true); err != nil {
		return t.recoveryError(errors.Join(cause, err), "joint restoration interrupted")
	}
	if t.oldLocation == t.backupPath {
		if err := t.rename(t.backupPath, t.cachePath); err != nil {
			return t.recoveryError(errors.Join(cause, err), "original cache recovery failed")
		}
		t.oldLocation = t.cachePath
	}
	t.phase = "cache-restored"
	if err := t.record(); err != nil {
		return t.recoveryError(errors.Join(cause, err), "recovery record failed")
	}
	if err := t.verify(finishCtx, true); err != nil {
		return t.recoveryError(errors.Join(cause, err), "joint restoration interrupted")
	}
	body, err := os.ReadFile(t.lockPath)
	if err != nil {
		return t.recoveryError(errors.Join(cause, err), "lock recovery failed")
	}
	if !bytes.Equal(body, t.originalLock) {
		if err = t.replaceLock(finishCtx, t.originalLock); err != nil {
			return t.recoveryError(errors.Join(cause, err), "lock recovery failed")
		}
	}
	t.phase = "restored"
	if err = t.record(); err != nil {
		return t.recoveryError(errors.Join(cause, err), "recovery record failed")
	}
	if err = t.verify(finishCtx, true); err != nil {
		return t.recoveryError(errors.Join(cause, err), "restoration verification failed")
	}
	if t.originParentCreated {
		if err = dependencyDirectory(t.originParent, t.originParentInfo); err == nil {
			err = os.Remove(t.originParent)
		}
		if err != nil {
			return t.recoveryError(errors.Join(cause, err), "owned origin parent cleanup failed")
		}
		t.originParentInfo = nil
		t.originParentCreated = false
	}

	if t.parentCreated {
		if err = dependencyDirectory(t.cacheParent, t.parentInfo); err == nil {
			err = os.Remove(t.cacheParent)
		}
		if err != nil {
			return t.recoveryError(errors.Join(cause, err), "owned cache parent cleanup failed")
		}
		t.parentInfo = nil
		t.parentCreated = false
	}
	t.mutated = false
	return cause
}

func (t *assistantDependencyTransaction) recoveryError(cause error, detail string) error {
	t.retain = true
	return fmt.Errorf("%w; %s; recovery material retained at %s", cause, detail, t.dir)
}

// Runs after the helper has finished recovery, including errors before it took
// the index lock. Never let that helper's unconditional temp cleanup own backups.
func (t *assistantDependencyTransaction) discard(cause error) error {
	if t.retain || t.mutated {
		return cause
	}
	if err := dependencyDirectory(t.dir, t.dirInfo); err != nil {
		return t.recoveryError(errors.Join(cause, err), "private cleanup withheld")
	}
	// Once a complete candidate exists, attest all owned private artifacts before
	// removing them. Partial preparation owns only its newly created private tree.
	if t.recordState.info != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := t.verifyObjects(ctx, false); err != nil {
			return t.recoveryError(errors.Join(cause, err), "private cleanup withheld")
		}
	}
	if err := os.RemoveAll(t.dir); err != nil {
		return t.recoveryError(errors.Join(cause, err), "private cleanup failed")
	}
	return cause
}

// Directory descriptors bind moves to the captured parents. A symlink or
// replacement between inspection and open cannot redirect a move elsewhere.
func (t *assistantDependencyTransaction) directoryInfo(path string) os.FileInfo {
	switch path {
	case t.cacheParent:
		return t.parentInfo
	case t.originParent:
		return t.originParentInfo
	case t.dir:
		return t.dirInfo
	case t.stageParent:
		return t.stageInfo
	case filepath.Dir(t.stagedOrigin):
		return t.stageOriginInfo
	default:
		return nil
	}
}

func createDependencyDirectory(parent string, expected os.FileInfo, name string, mode os.FileMode) (os.FileInfo, error) {
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	if expected == nil || !os.SameFile(expected, info) || info.Mode() != expected.Mode() {
		return nil, errors.New("dependency parent changed before directory creation")
	}
	if err = root.Mkdir(name, mode); err != nil {
		return nil, err
	}
	return root.Lstat(name)
}

// Capture the replacement's identity BEFORE its rename. Reading the pathname
// afterward could adopt an independent replacement as our own rollback token.
func (t *assistantDependencyTransaction) replaceLock(ctx context.Context, body []byte) error {
	file, err := os.CreateTemp(filepath.Dir(t.lockPath), ".bots.lock-transaction-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = file.Close(); _ = os.Remove(name) }()
	if err = file.Chmod(t.lockState.info.Mode().Perm()); err != nil {
		return err
	}
	if _, err = file.Write(body); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	replacement, err := dependencySnapshot(ctx, name)
	if err != nil {
		return err
	}
	if err = t.lockState.matches(ctx, t.lockPath); err != nil {
		return err
	}
	if err = os.Rename(name, t.lockPath); err != nil {
		return err
	}
	t.lockState = replacement
	return nil
}
