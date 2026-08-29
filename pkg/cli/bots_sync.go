package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// BotSyncResult describes one materialized lockfile dependency.
type BotSyncResult struct {
	Name          string `json:"name"`
	BundleSHA256  string `json:"bundle_sha256"`
	InstalledPath string `json:"installed_path"`
	Changed       bool   `json:"changed"`
}

// BotsSync materializes every project-root bots.lock dependency into .botz.
// Source content is validated and hashed before any existing install is
// replaced, so a bad ref or stale lock cannot destroy a known-good bundle.
func BotsSync(ctx context.Context, workdir string) ([]BotSyncResult, error) {
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
	lock, err := botlock.Load(absWorkdir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(lock.Dependencies))
	for name := range lock.Dependencies {
		names = append(names, name)
	}
	sort.Strings(names)

	results := make([]BotSyncResult, 0, len(names))
	for _, name := range names {
		dep := lock.Dependencies[name]
		result, err := syncBotDependency(ctx, absWorkdir, name, dep)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func syncBotDependency(ctx context.Context, workdir, name string, dep botlock.Dependency) (BotSyncResult, error) {
	target := filepath.Join(workdir, ".botz", name)
	if installedHash, hashErr := bundle.ContentHashDir(target); hashErr == nil && installedHash == dep.BundleSHA256 {
		return BotSyncResult{Name: name, BundleSHA256: installedHash, InstalledPath: target}, nil
	}

	source := resolveLocalLockSource(workdir, dep.Source)
	fetched, cleanup, fetchErr := botinstall.Fetch(ctx, botinstall.Options{Source: source, Ref: dep.Ref, Path: dep.Path})
	if fetchErr != nil {
		return BotSyncResult{}, fmt.Errorf("bot dependencies: sync %q: %w", name, fetchErr)
	}
	defer cleanup()
	fetchedHash, hashErr := bundle.ContentHashDir(fetched)
	if hashErr != nil {
		return BotSyncResult{}, fmt.Errorf("bot dependencies: hash %q: %w", name, hashErr)
	}
	if fetchedHash != dep.BundleSHA256 {
		return BotSyncResult{}, fmt.Errorf("bot dependencies: %q resolved to sha256 %s, lock requires %s", name, fetchedHash, dep.BundleSHA256)
	}
	fetchedBundle, openErr := bundle.OpenDir(fetched)
	if openErr != nil {
		return BotSyncResult{}, fmt.Errorf("bot dependencies: open %q: %w", name, openErr)
	}
	if fetchedBundle.Manifest == nil || fetchedBundle.Manifest.Name != name {
		actual := "<missing>"
		if fetchedBundle.Manifest != nil {
			actual = fetchedBundle.Manifest.Name
		}
		return BotSyncResult{}, fmt.Errorf("bot dependencies: lock name %q does not exactly match bundle manifest name %q", name, actual)
	}

	installed, installErr := botinstall.Install(ctx, botinstall.Options{
		Source: fetched, Name: name, Force: true, Workdir: workdir,
	})
	if installErr != nil {
		return BotSyncResult{}, fmt.Errorf("bot dependencies: install %q: %w", name, installErr)
	}
	installedHash, hashErr := bundle.ContentHashDir(installed.InstalledPath)
	if hashErr != nil || installedHash != dep.BundleSHA256 {
		return BotSyncResult{}, fmt.Errorf("bot dependencies: installed %q failed post-install hash verification", name)
	}
	return BotSyncResult{
		Name: name, BundleSHA256: installedHash, InstalledPath: installed.InstalledPath, Changed: true,
	}, nil
}

func resolveLocalLockSource(workdir, source string) string {
	if filepath.IsAbs(source) {
		return source
	}
	candidate := filepath.Join(workdir, source)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return source
}
