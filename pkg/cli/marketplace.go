package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// MarketplaceListOptions configures `iterion marketplace list`.
type MarketplaceListOptions struct {
	StoreDir string
	Text     string
	Tag      string
	// Kind filters by artifact kind: "" (any), "bot" or "plugin".
	Kind string
}

// MarketplaceSubmitOptions configures `iterion marketplace submit`.
type MarketplaceSubmitOptions struct {
	StoreDir string
	Source   string // git URL (optionally url#ref) or local path
	Ref      string
	Path     string
	Tags     []string
}

// MarketplaceInstallOptions configures `iterion marketplace install`.
type MarketplaceInstallOptions struct {
	StoreDir string
	Slug     string
	Workdir  string
	Force    bool
}

// MarketplaceInstallResult is the kind-aware outcome of
// MarketplaceInstall: Bot is set for bot entries (the .botz install
// result), Plugin for plugin entries (the installed plugin name).
type MarketplaceInstallResult struct {
	Kind   marketplace.Kind
	Bot    *botinstall.Result
	Plugin string
	Entry  *marketplace.Entry
}

// MarketplaceUninstallOptions configures `iterion marketplace uninstall`.
type MarketplaceUninstallOptions struct {
	StoreDir string
	Slug     string
	Workdir  string
}

// marketplaceStoreDir resolves the on-disk marketplace store directory
// from a --store-dir value (default ".iterion"), mirroring the studio's
// <store-dir>/marketplace layout so the CLI and the studio share one
// registry.
func marketplaceStoreDir(storeDir string) string {
	if storeDir == "" {
		storeDir = ".iterion"
	}
	return filepath.Join(storeDir, "marketplace")
}

func openMarketplaceStore(storeDir string) (*marketplace.JSONStore, error) {
	return marketplace.NewJSONStore(marketplaceStoreDir(storeDir))
}

// MarketplaceList returns the registry entries matching the filters.
func MarketplaceList(ctx context.Context, opts MarketplaceListOptions) ([]marketplace.Entry, error) {
	kind := marketplace.Kind(strings.TrimSpace(opts.Kind))
	switch kind {
	case "", marketplace.KindBot, marketplace.KindPlugin:
	default:
		return nil, fmt.Errorf("unknown kind %q (want %s|%s)", opts.Kind, marketplace.KindBot, marketplace.KindPlugin)
	}
	store, err := openMarketplaceStore(opts.StoreDir)
	if err != nil {
		return nil, err
	}
	return store.List(ctx, marketplace.Query{Text: opts.Text, Tag: opts.Tag, Kind: kind})
}

// MarketplaceSubmit validates the source (no install), detects its
// artifact kind (bot bundle or plugin) and indexes it in the local
// registry, returning the persisted entry. Mirrors the server's
// POST /api/v1/marketplace/submit business logic.
func MarketplaceSubmit(ctx context.Context, opts MarketplaceSubmitOptions) (*marketplace.Entry, error) {
	if strings.TrimSpace(opts.Source) == "" {
		return nil, fmt.Errorf("a git URL or local path is required")
	}
	store, err := openMarketplaceStore(opts.StoreDir)
	if err != nil {
		return nil, err
	}
	info, err := marketplace.InspectSource(ctx, opts.Source, opts.Ref, opts.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var entry marketplace.Entry
	switch info.Kind {
	case marketplace.KindPlugin:
		entry = marketplace.EntryFromPlugin(info.Plugin, opts.Source, opts.Ref, opts.Path)
		entry.Tags = normalizeMarketplaceTags(opts.Tags)
		entry.CreatedAt = now
		entry.UpdatedAt = now
	default: // bot
		md := info.Bot
		entry = marketplace.Entry{
			Slug:        botregistry.NormalizeName(md.Name),
			Name:        md.Name,
			DisplayName: md.DisplayName,
			Description: md.Description,
			Author:      md.Author,
			Tags:        normalizeMarketplaceTags(opts.Tags),
			RepoURL:     opts.Source,
			Ref:         opts.Ref,
			Subpath:     opts.Path,
			Version:     md.Version,
			README:      md.README,
			Presets:     toMarketplacePresets(md.Presets),
			CreatedAt:   now,
			UpdatedAt:   now,
		}
	}
	slug := entry.Slug
	if slug == "" {
		return nil, fmt.Errorf("%s has no name", info.Kind)
	}
	// A slug can't change kind through a re-submit (same rule as the
	// server): a bot slug stays a bot, a plugin slug stays a plugin.
	existing, exists, err := store.Get(ctx, slug)
	if err != nil {
		return nil, err
	}
	if exists && marketplace.EffectiveKind(*existing) != marketplace.EffectiveKind(entry) {
		return nil, fmt.Errorf("slug %q already exists as a %s entry", slug, marketplace.EffectiveKind(*existing))
	}
	if err := store.Upsert(ctx, entry); err != nil {
		return nil, fmt.Errorf("upsert: %w", err)
	}
	stored, ok, err := store.Get(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &entry, nil
	}
	return stored, nil
}

// MarketplaceInstall resolves the slug and installs the entry per its
// kind — a bot's bundle into the workspace's .botz/, a plugin into
// ~/.iterion/plugins/ — bumping the install counter. Returns the
// kind-aware result with the refreshed entry.
func MarketplaceInstall(ctx context.Context, opts MarketplaceInstallOptions) (*MarketplaceInstallResult, error) {
	store, err := openMarketplaceStore(opts.StoreDir)
	if err != nil {
		return nil, err
	}
	entry, ok, err := store.Get(ctx, opts.Slug)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("marketplace entry %q not found", opts.Slug)
	}
	out := &MarketplaceInstallResult{Kind: marketplace.EffectiveKind(*entry)}
	if out.Kind == marketplace.KindPlugin {
		name, err := plugin.InstallWith(ctx, plugin.InstallOptions{
			Source:  entry.RepoURL,
			Ref:     entry.Ref,
			Subpath: entry.Subpath,
		})
		if err != nil {
			return nil, err
		}
		out.Plugin = name
	} else {
		res, err := botinstall.Install(ctx, botinstall.Options{
			Source:  entry.RepoURL,
			Ref:     entry.Ref,
			Path:    entry.Subpath,
			Force:   opts.Force,
			Workdir: opts.Workdir,
		})
		if err != nil {
			return nil, err
		}
		out.Bot = res
	}
	// Best-effort counter bump — the install already succeeded.
	_ = store.IncrementInstalls(ctx, opts.Slug)
	refreshed, _, _ := store.Get(ctx, opts.Slug)
	if refreshed == nil {
		refreshed = entry
	}
	out.Entry = refreshed
	return out, nil
}

// MarketplaceUninstall resolves the slug and removes the installed
// artifact per its kind: a bot's workspace bundle (botinstall.Remove)
// or an installed plugin (plugin.Uninstall). The registry entry itself
// is untouched. Returns the entry so callers can echo what was removed.
func MarketplaceUninstall(ctx context.Context, opts MarketplaceUninstallOptions) (*marketplace.Entry, error) {
	store, err := openMarketplaceStore(opts.StoreDir)
	if err != nil {
		return nil, err
	}
	entry, ok, err := store.Get(ctx, opts.Slug)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("marketplace entry %q not found", opts.Slug)
	}
	// Both kinds install under the manifest name (entry.Name), not the
	// registry slug.
	if marketplace.EffectiveKind(*entry) == marketplace.KindPlugin {
		if err := plugin.Uninstall(entry.Name); err != nil {
			return nil, err
		}
		return entry, nil
	}
	if err := botinstall.Remove(ctx, botinstall.Options{Name: entry.Name, Workdir: opts.Workdir}); err != nil {
		return nil, err
	}
	return entry, nil
}

// marketplaceSeedPaths resolves the workspace-relative bundle roots to
// seed from. Overridable via ITERION_MARKETPLACE_SEED_PATHS (comma-
// separated); defaults to "bots". A value of "none" (or "-") disables
// seeding entirely.
func marketplaceSeedPaths() []string {
	raw := strings.TrimSpace(os.Getenv("ITERION_MARKETPLACE_SEED_PATHS"))
	if raw == "" {
		return []string{"bots"}
	}
	if raw == "none" || raw == "-" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SeedMarketplaceDefault seeds store from the default catalog roots
// (ITERION_MARKETPLACE_SEED_PATHS, comma-separated, workspace-relative;
// defaults to "bots") resolved under workdir. It is the cloud-server
// counterpart to the inline seeding the studio command does, exported so
// cmd/iterion can call it without duplicating the path resolution. A
// "none"/"-" override (or an empty result) skips seeding and returns
// (0, nil). Best-effort + idempotent — see SeedMarketplace.
func SeedMarketplaceDefault(ctx context.Context, store marketplace.Store, workdir string) (int, error) {
	paths := marketplaceSeedPaths()
	if len(paths) == 0 {
		return 0, nil
	}
	return SeedMarketplace(ctx, store, SeedOptions{Paths: paths, Workdir: workdir})
}

// SeedOptions configures SeedMarketplace.
type SeedOptions struct {
	// Paths are bundle source roots to index (e.g. ["bots"]). Resolved
	// relative to Workdir when not absolute. Whatever bots the target
	// repo ships under these paths become the seeded entries — the
	// mechanism is repo-agnostic; it never assumes a specific layout.
	Paths []string
	// Workdir is the base for relative Paths (default: current dir).
	Workdir string
}

// SeedMarketplace indexes the bundles discovered under opts.Paths as
// builtin, pre-approved, public registry entries so the marketplace
// isn't empty out of the box. It is idempotent and conservative:
//   - a slug already owned by a user (git/upload source) is never
//     clobbered;
//   - a builtin entry is rewritten only when absent or its version
//     drifted, so reseeding is a no-op and install counters survive
//     (Upsert preserves Installs when the new entry carries 0).
//
// Returns the number of entries written (created or updated).
func SeedMarketplace(ctx context.Context, store marketplace.Store, opts SeedOptions) (int, error) {
	workdir := opts.Workdir
	if workdir == "" {
		if wd, err := os.Getwd(); err == nil {
			workdir = wd
		}
	}
	written := 0
	for _, p := range opts.Paths {
		root := p
		if !filepath.IsAbs(root) {
			root = filepath.Join(workdir, p)
		}
		if _, err := os.Stat(root); err != nil {
			continue // a configured seed path that doesn't exist is fine
		}
		entries, err := botregistry.List(botregistry.ListOptions{Paths: []string{root}})
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsBundleDir {
				continue
			}
			md, err := botinstall.Inspect(ctx, botinstall.Options{Source: e.Path})
			if err != nil {
				continue
			}
			slug := botregistry.NormalizeName(md.Name)
			if slug == "" {
				continue
			}
			existing, ok, _ := store.Get(ctx, slug)
			if ok {
				// Never clobber a user-submitted (git/upload) entry.
				if marketplace.EffectiveSource(*existing) != marketplace.SourceBuiltin {
					continue
				}
				// Unchanged builtin → skip the write.
				if existing.Version == md.Version {
					continue
				}
			}
			now := time.Now().UTC().Format(time.RFC3339)
			created := now
			if ok && existing.CreatedAt != "" {
				created = existing.CreatedAt
			}
			entry := marketplace.Entry{
				Slug:        slug,
				Name:        md.Name,
				DisplayName: md.DisplayName,
				Description: md.Description,
				Author:      md.Author,
				RepoURL:     e.Path,
				Version:     md.Version,
				README:      md.README,
				Presets:     toMarketplacePresets(md.Presets),
				Source:      marketplace.SourceBuiltin,
				Status:      marketplace.StatusApproved,
				Scope:       marketplace.ScopePublic,
				CreatedAt:   created,
				UpdatedAt:   now,
			}
			if err := store.Upsert(ctx, entry); err == nil {
				written++
			}
		}
	}
	return written, nil
}

// normalizeMarketplaceTags strips blanks and de-dups (mirrors the
// server's normalizeTags).
func normalizeMarketplaceTags(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// toMarketplacePresets converts botinstall.PresetMeta into the registry
// EntryPreset shape (mirrors the server's toEntryPresets).
func toMarketplacePresets(in []botinstall.PresetMeta) []marketplace.EntryPreset {
	if len(in) == 0 {
		return nil
	}
	out := make([]marketplace.EntryPreset, len(in))
	for i, p := range in {
		out[i] = marketplace.EntryPreset{
			Name:        p.Name,
			DisplayName: p.DisplayName,
			Description: p.Description,
			Skills:      append([]string(nil), p.Skills...),
		}
	}
	return out
}
