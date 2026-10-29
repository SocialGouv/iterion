// Package botdeps manages project-pinned shared bot dependencies.
//
// It deliberately sits below both the CLI and Studio server packages so
// host-owned assistant actions and command-line operations share the same
// lock/update/materialization semantics.
package botdeps

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

// SyncResult describes one materialized lockfile dependency.
type SyncResult struct {
	Name          string `json:"name"`
	BundleSHA256  string `json:"bundle_sha256"`
	InstalledPath string `json:"installed_path"`
	Changed       bool   `json:"changed"`
	// CatalogError is the rendered cause of the catalog refresh that failed
	// after the dependency materialized: the bundle is in place and its
	// hash verified, but the orchestrator-facing catalog is stale until a
	// regeneration succeeds. Empty for a dependency already at its pin (no
	// install, no refresh) and for private staging, which preserves the
	// pinned catalog bytes by construction.
	CatalogError string `json:"catalog_error,omitempty"`
	// Fragments counts the shared `.bot` fragments the second pass
	// materialized into this dependency's own lib/ from its
	// `dependencies.fragments` (ADR-145). Zero for the bundles that declare
	// none, which is every bundle that carries its own fragments.
	Fragments int `json:"fragments,omitempty"`
	// Skills counts the shared skill documents the same pass materialized
	// into this dependency's own skills/ from its `dependencies.skills`
	// (ADR-144). Zero for the bundles that declare none, which is every
	// bundle that carries a copy of every skill it references.
	Skills int `json:"skills,omitempty"`
}

// Sync materializes every project-root bots.lock dependency into .botz.
// Source content is validated and hashed before any existing install is
// replaced, so a bad ref or stale lock cannot destroy a known-good bundle.
//
// A non-nil error comes back with the results collected before it: those
// dependencies ARE materialized, so their CatalogError is the caller's only
// word on a workspace catalog the aborted sync left stale.
func Sync(ctx context.Context, workdir string) ([]SyncResult, error) {
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

	results := make([]SyncResult, 0, len(names))
	for _, name := range names {
		result, err := syncOne(ctx, absWorkdir, name, lock.Dependencies[name])
		if err != nil {
			// The dependencies synced before this one are materialized, and
			// one of them may carry a stale catalog: they travel WITH the
			// error so the caller can report both.
			return results, err
		}
		results = append(results, result)
	}
	// Shared fragments and skills resolve in a SECOND pass, once every
	// bundle of the lockfile is materialized. The first pass walks names
	// alphabetically,
	// so a consumer `a-bot` is installed before `.botz/z-lib` exists: a
	// resolution inside it would refuse a correct configuration whenever the
	// consumer sorts before its producer, and succeed on the other order.
	// The order a verdict depends on is a fact the machine owes, not one the
	// operator should have to supply.
	for i := range results {
		resolved, err := resolveSharedDepsInto(absWorkdir, results[i])
		if err != nil {
			return results, err
		}
		results[i] = resolved
	}
	return results, nil
}

// SyncOne materializes one already-validated lock dependency, and resolves
// its shared dependencies. A single-bundle request is EXPLICIT, so a producer
// the project has not materialized yet is a true miss, named with the
// command that fixes it; Sync defers the resolution to its second pass
// instead, where no producer can still be missing by an accident of order.
func SyncOne(ctx context.Context, workdir, name string, dep botlock.Dependency) (SyncResult, error) {
	result, err := syncOne(ctx, workdir, name, dep)
	if err != nil {
		return result, err
	}
	return resolveSharedDepsInto(workdir, result)
}

// resolveSharedDepsInto is the ONE place a materialized bundle's shared
// dependencies are resolved, so every entry point that materializes one gets
// the same reconciliation and the same refusals — for every artefact kind of
// the sharing key, in one place, rather than one pass per kind that a new
// entry point can forget half of.
func resolveSharedDepsInto(workdir string, result SyncResult) (SyncResult, error) {
	fragments, err := botinstall.ResolveFragments(workdir, result.InstalledPath)
	if err != nil {
		return result, fmt.Errorf("bot dependencies: %q: %w", result.Name, err)
	}
	result.Fragments = len(fragments)
	skills, err := botinstall.ResolveSkills(workdir, result.InstalledPath)
	if err != nil {
		return result, fmt.Errorf("bot dependencies: %q: %w", result.Name, err)
	}
	result.Skills = len(skills)
	return result, nil
}

func syncOne(ctx context.Context, workdir, name string, dep botlock.Dependency) (SyncResult, error) {
	target := filepath.Join(workdir, ".botz", name)
	if installedHash, hashErr := bundle.ContentHashDir(target); hashErr == nil && installedHash == dep.BundleSHA256 {
		return SyncResult{Name: name, BundleSHA256: installedHash, InstalledPath: target}, nil
	}
	return materializeOne(ctx, workdir, "", name, dep, false)
}

// StageOne installs into a caller-owned private destination, without replacing
// an existing target or regenerating any live catalog. Sources are still
// resolved against the real workspace, independently of the destination.
func StageOne(ctx context.Context, workdir, dest, name string, dep botlock.Dependency) (SyncResult, error) {
	if !filepath.IsAbs(workdir) || !filepath.IsAbs(dest) {
		return SyncResult{}, fmt.Errorf("bot dependencies: private staging requires absolute workspace and destination paths")
	}
	result, err := materializeOne(ctx, workdir, dest, name, dep, true)
	if err != nil {
		return result, err
	}
	return resolveSharedDepsInto(workdir, result)
}

func materializeOne(ctx context.Context, workdir, dest, name string, dep botlock.Dependency, private bool) (SyncResult, error) {
	if err := ctx.Err(); err != nil {
		return SyncResult{}, err
	}
	source := resolveLocalSource(workdir, dep.Source)
	fetched, cleanup, fetchErr := botinstall.Fetch(ctx, botinstall.Options{Source: source, Ref: dep.Ref, Path: dep.Path})
	if fetchErr != nil {
		return SyncResult{}, fmt.Errorf("bot dependencies: sync %q: %w", name, fetchErr)
	}
	defer cleanup()
	fetchedHash, hashErr := bundle.ContentHashDir(fetched)
	if hashErr != nil {
		return SyncResult{}, fmt.Errorf("bot dependencies: hash %q: %w", name, hashErr)
	}
	if fetchedHash != dep.BundleSHA256 {
		return SyncResult{}, fmt.Errorf("bot dependencies: %q resolved to sha256 %s, lock requires %s", name, fetchedHash, dep.BundleSHA256)
	}
	fetchedBundle, openErr := bundle.OpenDir(fetched)
	if openErr != nil {
		return SyncResult{}, fmt.Errorf("bot dependencies: open %q: %w", name, openErr)
	}
	if fetchedBundle.Manifest == nil || fetchedBundle.Manifest.Name != name {
		actual := "<missing>"
		if fetchedBundle.Manifest != nil {
			actual = fetchedBundle.Manifest.Name
		}
		return SyncResult{}, fmt.Errorf("bot dependencies: lock name %q does not exactly match bundle manifest name %q", name, actual)
	}

	var origin *botinstall.Origin
	if private {
		origin = &botinstall.Origin{Source: source, Ref: dep.Ref, SourcePath: dep.Path}
	}
	installed, installErr := botinstall.Install(ctx, botinstall.Options{
		Source: fetched, Name: name, Force: !private, Workdir: workdir, Dest: dest, SkipCatalog: private,
		Origin: origin,
		// Shared dependencies are resolved by the caller, after the whole
		// lockfile is materialized: Sync's second pass, or StageOne for the
		// single bundle it stages. Install refusing here would make every
		// sync of a consumer fail on the order its producer sorts in.
		DeferSharedDeps: true,
		// bots.lock IS the record of management for .botz/<name>: the pin
		// names the dependency in a committed file, and the hash check above
		// has already proven the installed copy is not what that pin says.
		// Sync's whole contract is to make .botz match the lock, so it owns
		// this destination even where the installer's own origin record is
		// missing (a workspace materialized before origins existed). The
		// refusal this skips guards a DIFFERENT destination: an operator
		// pointing --dest at authored bundles.
		ReplaceUnrecorded: !private,
	})
	if installErr != nil {
		return SyncResult{}, fmt.Errorf("bot dependencies: install %q: %w", name, installErr)
	}
	installedHash, hashErr := bundle.ContentHashDir(installed.InstalledPath)
	if hashErr != nil || installedHash != dep.BundleSHA256 {
		return SyncResult{}, fmt.Errorf("bot dependencies: installed %q failed post-install hash verification", name)
	}
	return SyncResult{
		Name: name, BundleSHA256: installedHash, InstalledPath: installed.InstalledPath, Changed: true,
		CatalogError: installed.CatalogError,
	}, nil
}

func resolveLocalSource(workdir, source string) string {
	if filepath.IsAbs(source) {
		return source
	}
	candidate := filepath.Join(workdir, source)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return source
}
