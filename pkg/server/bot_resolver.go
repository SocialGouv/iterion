package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// This file is the server's ONE bot-resolution authority. Every site that
// turns a bot id into launchable source, a manifest, or catalog metadata goes
// through it, so the resolution precedence — team botsource → platform
// botsource → baked catalog FS — holds at every surface at once (launch,
// resume, webhooks, schedules, board dispatch, triggers, command discovery,
// hand-offs, listing). A site that read the baked registry directly would
// silently ignore a platform override; the static sweep test
// (bot_resolver_sweep_test.go) forbids new direct botregistry reads in this
// package.
//
// The team tier is deliberately consulted ONLY where an active-team context
// exists (the studio launch surface): a team's experimental fork must not
// silently hijack that team's schedules/webhooks. The platform tier applies
// everywhere — overriding the deployment's catalog is its purpose.

// launchBot is a resolved, launch-ready bot: its source, provenance, and —
// for a STORED bot (team or platform botsource row) — the materialized
// bundle dir the compile path merges prompts from, plus the ref the cloud
// publisher stamps on the queue message.
type launchBot struct {
	BotID  string
	Origin string // "team" | "platform" | "catalog"
	Path   string // logical label for stored bots, FS path for catalog
	Source string
	// BundleDir is the server-side temp materialization of a stored bundle
	// ("" for catalog bots — their bundle lives on the pod FS at Path's
	// dir). The resolver owns it; call Cleanup once the launch returned.
	BundleDir string
	// Ref identifies the stored row (nil for catalog bots).
	Ref *runview.BotBundleRef
}

// Stamp applies the resolution onto a LaunchSpec.
func (lb *launchBot) Stamp(spec *runview.LaunchSpec) {
	spec.FilePath = lb.Path
	spec.Source = lb.Source
	spec.BotID = lb.BotID
	lb.StampBundle(spec)
}

// StampBundle stamps only the stored-bundle fields (compile dir + runner
// ref) — for callers that resolved path/source separately (the studio
// launch derives an absolute path first). Nil-safe: a catalog/loose bot
// stamps nothing.
func (lb *launchBot) StampBundle(spec *runview.LaunchSpec) {
	if lb == nil {
		return
	}
	spec.BundleDir = lb.BundleDir
	spec.BotBundle = lb.Ref
}

// Cleanup removes the materialized bundle dir, if any. Safe on nil.
func (lb *launchBot) Cleanup() {
	if lb != nil && lb.BundleDir != "" {
		_ = os.RemoveAll(lb.BundleDir)
	}
}

// resolveBotTiered resolves a bot id (or a catalog-shaped file path) through
// the tiers: the caller's team store (when teamID is non-empty), the platform
// store, then the baked catalog FS. Returns (nil, nil) when nothing matches —
// the caller keeps its own "not found" semantics. A stored row that fails to
// materialize is an explicit error, never a silent fall-through to the baked
// tier (that would pair this launch with resources the operator replaced).
func (s *Server) resolveBotTiered(ctx context.Context, teamID, botID, filePath string) (*launchBot, error) {
	slug := strings.TrimSpace(botID)
	if slug == "" {
		slug = inferCatalogBotID(filePath)
	}
	if slug == "" {
		return nil, nil
	}
	// The catalog tier tolerates spelling variants (feature_dev /
	// Feature-Dev / "feature dev" → feature-dev; ResolveBotPath normalizes
	// internally), and board cards / trigger subscriptions rely on it. The
	// PLATFORM tier must tolerate the same set, or an override is silently
	// bypassed for every non-canonical spelling. Resolved into its OWN
	// variable: rewriting `slug` itself let a platform entry's canonical
	// name miss the team row and hijack the team tier — team > platform
	// must hold even across spellings.
	platformSlug := slug
	if s.botSources != nil {
		if norm := botregistry.NormalizeName(slug); norm != slug {
			for _, e := range s.platformBotEntries() {
				if botregistry.NormalizeName(e.Name) == norm {
					platformSlug = e.Name
					break
				}
			}
		}
	}
	if s.botSources != nil {
		// Only ErrNotFound falls through to the next tier: any other store
		// error must surface, or a Mongo blip would silently launch the
		// STALE BAKED bot — the exact façade the runner-side version check
		// refuses (erreurs-explicites, both halves of the same contract).
		if teamID != "" {
			bs, err := s.botSources.GetBySlug(store.WithTenant(ctx, teamID), teamID, slug)
			switch {
			case err == nil:
				return s.storedLaunchBot(bs, "team")
			case !errors.Is(err, botsource.ErrNotFound):
				return nil, fmt.Errorf("resolve bot %q (team tier): %w", slug, err)
			}
		}
		pctx := store.WithTenant(ctx, botsource.PlatformTenantID)
		bs, err := s.botSources.GetBySlug(pctx, botsource.PlatformTenantID, platformSlug)
		switch {
		case err == nil:
			return s.storedLaunchBot(bs, "platform")
		case !errors.Is(err, botsource.ErrNotFound):
			return nil, fmt.Errorf("resolve bot %q (platform tier): %w", platformSlug, err)
		}
	}
	path, err := botregistry.ResolveBotPath(slug, s.effectivePaths())
	if err != nil {
		return nil, nil //nolint:nilerr // unknown id = not found here, the caller decides
	}
	b, err := os.ReadFile(path)
	if err != nil {
		// The id RESOLVED but its source cannot be read — a real error, not
		// an absence.
		return nil, fmt.Errorf("read bot %q: %w", slug, err)
	}
	return &launchBot{BotID: slug, Origin: "catalog", Path: path, Source: string(b)}, nil
}

// storedLaunchBot materializes one stored row into a launch-ready bot.
func (s *Server) storedLaunchBot(bs botsource.BotSource, origin string) (*launchBot, error) {
	main := bs.Files[botsource.MainBotFile]
	if strings.TrimSpace(main) == "" {
		return nil, fmt.Errorf("bot source %s/%s: %s is empty", bs.TenantID, bs.Slug, botsource.MainBotFile)
	}
	dir, err := os.MkdirTemp("", "iterion-launch-bot-*")
	if err != nil {
		return nil, fmt.Errorf("bot source %s: %w", bs.Slug, err)
	}
	if err := botsource.Materialize(dir, bs.Files); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &launchBot{
		BotID:     bs.Slug,
		Origin:    origin,
		Path:      "bots/" + bs.Slug + "/" + botsource.MainBotFile,
		Source:    main,
		BundleDir: dir,
		Ref:       &runview.BotBundleRef{TenantID: bs.TenantID, Slug: bs.Slug, Version: bs.Version},
	}, nil
}

// resolveBotSource resolves a bot id for the tenant-context-free launch
// surfaces (webhooks, schedules, board dispatch, triggers): platform store
// first, then the baked catalog. Errors when the id resolves nowhere.
func (s *Server) resolveBotSource(ctx context.Context, botID string) (*launchBot, error) {
	lb, err := s.resolveBotTiered(ctx, "", botID, "")
	if err != nil {
		return nil, err
	}
	if lb == nil {
		return nil, fmt.Errorf("bot %q not found", botID)
	}
	return lb, nil
}

// ---- catalog metadata overlay (entries + manifests) ----

// platformBotSet is the cached projection of the platform tenant's rows:
// the schema-augmented entry set AND the decoded manifests, built from the
// same list read so the two can never disagree within a TTL window.
type platformBotSet struct {
	entries   []botregistry.EntryWithSchema
	manifests map[string]*bundle.Manifest
}

// platformBotSetCached returns the resolver-cached set (30s TTL,
// invalidate-on-own-write, serve-last-known on a store outage — one cache
// mechanism, not a sibling with divergent semantics). Nil without a
// bot-source store (local mode).
func (s *Server) platformBotSetCached() *platformBotSet {
	if s.botSources == nil || s.platformBots == nil {
		return nil
	}
	return s.platformBots.Get(context.Background())
}

// platformBotEntries returns the platform overrides as schema-augmented
// entries; nil when the platform tenant holds no rows.
func (s *Server) platformBotEntries() []botregistry.EntryWithSchema {
	set := s.platformBotSetCached()
	if set == nil {
		return nil
	}
	return set.entries
}

// newPlatformBotsResolver builds the entry-set cache. The fetch propagates
// a ListByTenant failure so an outage serves the LAST-KNOWN entry set
// instead of caching an empty one (platform metadata silently vanishing
// from command discovery / hand-offs for a TTL window).
func (s *Server) newPlatformBotsResolver() *platformcfg.Resolver[platformBotSet] {
	return platformcfg.NewResolverFunc(func(ctx context.Context) (*platformBotSet, error) {
		if s.botSources == nil {
			return nil, nil
		}
		list, err := s.botSources.ListByTenant(store.WithTenant(ctx, botsource.PlatformTenantID), botsource.PlatformTenantID)
		if err != nil {
			return nil, err
		}
		set := platformBotSet{
			entries:   s.materializeBotEntries(list),
			manifests: make(map[string]*bundle.Manifest, len(list)),
		}
		for i := range list {
			if m := list[i].Manifest(); m != nil {
				set.manifests[list[i].Slug] = m
			}
		}
		return &set, nil
	}, s.logger.Warn)
}

// invalidatePlatformBots forces the next read to re-list, so the replica
// that served a platform-bot mutation reads its own write immediately.
func (s *Server) invalidatePlatformBots() {
	s.platformBots.Invalidate()
}

// effectiveEntriesWithSchema returns the baked catalog overlaid with the
// platform overrides: a platform entry REPLACES the same-slug baked entry,
// a new-slug platform bot is appended. This is the metadata set every
// tenant-context-free consumer (command discovery, hand-offs, gate-var
// defaults) reads.
func (s *Server) effectiveEntriesWithSchema() ([]botregistry.EntryWithSchema, error) {
	catalog, err := botregistry.ListWithSchema(s.botListOptions())
	if err != nil {
		return nil, err
	}
	overrides := s.platformBotEntries()
	if len(overrides) == 0 {
		return catalog, nil
	}
	byName := make(map[string]int, len(catalog))
	for i, e := range catalog {
		byName[e.Name] = i
	}
	out := catalog
	for _, e := range overrides {
		if i, ok := byName[e.Name]; ok {
			out[i] = e
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// effectiveEntries is effectiveEntriesWithSchema flattened to plain entries.
func (s *Server) effectiveEntries() ([]botregistry.Entry, error) {
	withSchema, err := s.effectiveEntriesWithSchema()
	if err != nil {
		return nil, err
	}
	out := make([]botregistry.Entry, 0, len(withSchema))
	for _, e := range withSchema {
		out = append(out, e.Entry)
	}
	return out, nil
}

// platformBotManifest returns the manifest of a platform override, or nil
// when there is none (or it carries no manifest). The manifest tier behind
// botManifest, so retry-policy/config-share reads honor an override. Served
// from the resolver cache — botManifest sits on every launch's retry-policy
// resolution, which must not pay an unbounded per-call store read.
func (s *Server) platformBotManifest(slug string) *bundle.Manifest {
	if slug == "" {
		return nil
	}
	set := s.platformBotSetCached()
	if set == nil {
		return nil
	}
	return set.manifests[slug]
}

// entryOrigin reports how a bot name currently resolves for display:
// "platform" when a deployment override shadows it, else "catalog". Keeps
// the detail/PUT responses consistent with the list's origin so the studio
// badge doesn't flicker between surfaces.
func (s *Server) entryOrigin(name string) string {
	for _, e := range s.platformBotEntries() {
		if e.Name == name {
			return "platform"
		}
	}
	return "catalog"
}

// botExists reports whether a bot id resolves on this deployment (platform
// override or baked catalog) WITHOUT materializing anything — the cheap
// probe for callers that only route (e.g. the /revi converse gate). Team
// bots are deliberately out of scope, matching launch resolution on the
// tenant-context-free surfaces.
func (s *Server) botExists(botID string) bool {
	for _, e := range s.platformBotEntries() {
		if e.Name == botID {
			return true
		}
	}
	_, err := botregistry.ResolveBotPath(botID, s.effectivePaths())
	return err == nil
}

// effectiveFindByName returns the effective (platform-overlaid) entry for a
// bot name: an exact match first, then the NormalizeName-folded spelling
// (review_pr → review-pr), because the launcher accepts both and a run's
// persisted BotID may carry either. The hand-off lookup
// (handoffConsumersFor), the pause-notice role and the bots route all read
// through here.
func (s *Server) effectiveFindByName(name string) (botregistry.EntryWithSchema, bool, error) {
	entries, err := s.effectiveEntriesWithSchema()
	if err != nil {
		return botregistry.EntryWithSchema{}, false, err
	}
	for _, e := range entries {
		if e.Name == name {
			return e, true, nil
		}
	}
	// Tolerant match: normalised comparison after the exact pass.
	nn := botregistry.NormalizeName(name)
	if nn == "" {
		return botregistry.EntryWithSchema{}, false, nil
	}
	for _, e := range entries {
		if botregistry.NormalizeName(e.Name) == nn {
			return e, true, nil
		}
	}
	return botregistry.EntryWithSchema{}, false, nil
}
