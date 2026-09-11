package botdeps

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

type UpdateOptions struct {
	Workdir    string
	Name       string
	Ref        string
	AllowDirty bool
}

type UpdateResult struct {
	Name          string `json:"name"`
	Ref           string `json:"ref"`
	BundleSHA256  string `json:"bundle_sha256"`
	InstalledPath string `json:"installed_path"`
}

// Update resolves one existing dependency, rewrites its pin atomically, and
// materializes exactly that dependency. Local Git sources must be clean by
// default so the recorded ref can reproduce the recorded content hash.
func Update(ctx context.Context, opts UpdateOptions) (*UpdateResult, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, fmt.Errorf("bot dependencies: dependency name is required")
	}
	workdir := opts.Workdir
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		workdir = wd
	}
	absWorkdir, err := filepath.Abs(workdir)
	if err != nil {
		return nil, fmt.Errorf("bot dependencies: resolve workspace: %w", err)
	}
	lockPath := filepath.Join(absWorkdir, botlock.FileName)
	lockBefore, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("bot dependencies: read %s: %w", lockPath, err)
	}
	lockInfo, err := os.Stat(lockPath)
	if err != nil {
		return nil, fmt.Errorf("bot dependencies: stat %s: %w", lockPath, err)
	}
	lock, err := botlock.Load(absWorkdir)
	if err != nil {
		return nil, err
	}
	dep, ok := lock.Dependencies[opts.Name]
	if !ok {
		return nil, fmt.Errorf("bot dependencies: %q is not declared in %s", opts.Name, lockPath)
	}
	if opts.Ref != "" {
		dep.Ref = opts.Ref
	}
	source := resolveLocalSource(absWorkdir, dep.Source)
	fetched, cleanup, err := botinstall.Fetch(ctx, botinstall.Options{Source: source, Ref: dep.Ref, Path: dep.Path})
	if err != nil {
		return nil, fmt.Errorf("bot dependencies: update %q: %w", opts.Name, err)
	}
	defer cleanup()
	if local, statErr := os.Stat(source); statErr == nil && local.IsDir() {
		head, dirty, gitErr := localBundleGitState(ctx, fetched)
		if gitErr != nil {
			return nil, fmt.Errorf("bot dependencies: inspect local source for %q: %w", opts.Name, gitErr)
		}
		if dirty && !opts.AllowDirty {
			return nil, fmt.Errorf("bot dependencies: local source for %q has uncommitted bundle changes; commit them first or pass --allow-dirty", opts.Name)
		}
		if head != "" && !dirty {
			if opts.Ref != "" && opts.Ref != head {
				return nil, fmt.Errorf("bot dependencies: --ref %s does not match local source HEAD %s", opts.Ref, head)
			}
			dep.Ref = head
		}
	}
	b, err := bundle.OpenDir(fetched)
	if err != nil {
		return nil, fmt.Errorf("bot dependencies: open %q: %w", opts.Name, err)
	}
	if b.Manifest == nil || b.Manifest.Name != opts.Name {
		actual := "<missing>"
		if b.Manifest != nil {
			actual = b.Manifest.Name
		}
		return nil, fmt.Errorf("bot dependencies: lock name %q does not exactly match bundle manifest name %q", opts.Name, actual)
	}
	hash, err := bundle.ContentHashDir(fetched)
	if err != nil {
		return nil, fmt.Errorf("bot dependencies: hash %q: %w", opts.Name, err)
	}
	dep.BundleSHA256 = hash
	lock.Dependencies[opts.Name] = dep
	if err := botlock.Save(absWorkdir, lock); err != nil {
		return nil, err
	}
	synced, err := SyncOne(ctx, absWorkdir, opts.Name, dep)
	if err != nil {
		if restoreErr := restoreLockBytes(lockPath, lockBefore, lockInfo.Mode().Perm()); restoreErr != nil {
			return nil, fmt.Errorf("%v; additionally failed to restore bots.lock exactly: %w", err, restoreErr)
		}
		return nil, err
	}
	return &UpdateResult{Name: opts.Name, Ref: dep.Ref, BundleSHA256: hash, InstalledPath: synced.InstalledPath}, nil
}

func restoreLockBytes(path string, contents []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bots.lock-restore-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func localBundleGitState(ctx context.Context, bundleDir string) (head string, dirty bool, err error) {
	runAt := func(dir string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = gitlib.SanitizeEnv(os.Environ())
		return cmd.Output()
	}
	rootBody, rootErr := runAt(bundleDir, "rev-parse", "--show-toplevel")
	if rootErr != nil {
		return "", false, nil // A local non-Git source is valid, but has no reproducible Git ref to refresh.
	}
	root := strings.TrimSpace(string(rootBody))
	rel, err := filepath.Rel(root, bundleDir)
	if err != nil {
		return "", false, err
	}
	headBody, err := runAt(root, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	statusBody, err := runAt(root, "status", "--porcelain=v1", "--untracked-files=all", "--", rel)
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(headBody)), len(statusBody) > 0, nil
}
