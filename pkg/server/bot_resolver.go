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
// The team tier applies on EVERY launch surface, because every launcher
// knows the team it launches for: the card's, the subscription's, the
// schedule's, the webhook token's. A team that forks a bot gets its fork on
// its board cards and its webhook reviews as much as on the studio button —
// and each launch RECORDS the tier that served it (launchBot.Tier →
// LaunchSpec.BotSourceTier → Run.BotSourceTier), so a fork is never a silent
// substitution. A surface with no team id has none to invent: it resolves
// platform-over-baked and says so by passing an empty team.
//
// The METADATA reads below are a separate, narrower view:
// effectiveEntries* / effectiveFindByName / botExists / platformBotManifest
// are tenant-context-FREE (platform over baked). A metadata read must match
// the tier the LAUNCH it describes will resolve, or the two disagree in
// silence — so a lane whose launch is tenant-aware reads through
// effectiveFindByNameForTeam, and a lane whose launch is tenant-free keeps
// the tenant-free view. Wired so far: the webhook hand-off `consumes:` seeds
// and the gate-var defaults, the two that describe a delivery this file's
// launch resolution now serves from the team tier. Still tenant-free, and
// therefore still able to disagree with a team fork: the retry-policy
// manifest read (botManifest), the command discovery, the /bots listing, and
// the hand-off PRODUCER set — pre-existing on the manual surface, tracked as
// #946. A caller that holds the tier a bot ACTUALLY resolved through — a
// run's BotSourceTenant, stamped at launch — asks teamBotManifest instead.

// launchBot is a resolved, launch-ready bot. Cloud resolution freezes its
// collection before compile and carries the same snapshot to the runner.
type launchBot struct {
	BotID  string
	Origin string // "team" | "platform" | "catalog"
	Path   string // logical label for stored bots, FS path for catalog
	Source string
	// BundleDir is the server-side materialization used for compilation.
	// Cloud catalog and stored bots both use it; Cleanup owns the collection.
	BundleDir string
	// Ref carries the snapshot and, for stored origins, the row provenance.
	Ref             *runview.BotBundleRef
	cleanupSnapshot func()
}

// Tier maps the resolution's origin onto the persisted tier vocabulary
// (store.BotSourceTier*). One conversion site: Origin is the resolver's
// internal word, BotSourceTier the operator-facing one.
func (lb *launchBot) Tier() string {
	switch lb.Origin {
	case "team":
		return store.BotSourceTierTeam
	case "platform":
		return store.BotSourceTierPlatform
	default:
		return store.BotSourceTierBaked
	}
}

// Stamp applies the resolution onto a LaunchSpec.
func (lb *launchBot) Stamp(spec *runview.LaunchSpec) {
	spec.FilePath = lb.Path
	spec.Source = lb.Source
	spec.BotID = lb.BotID
	lb.StampBundle(spec)
}

// StampBundle stamps the compile dir, the runner ref and the tier that served
// the launch, for callers that resolved path/source separately (the studio
// derives an absolute path first). Nil-safe.
func (lb *launchBot) StampBundle(spec *runview.LaunchSpec) {
	if lb == nil {
		return
	}
	spec.BundleDir = lb.BundleDir
	spec.BotBundle = lb.Ref
	spec.BotSourceTier = lb.Tier()
}

// Cleanup removes the materialized bundle dir, if any. Safe on nil.
func (lb *launchBot) Cleanup() {
	if lb != nil && lb.cleanupSnapshot != nil {
		lb.cleanupSnapshot()
		return
	}
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
	lb, err := s.resolveBotTieredRaw(ctx, teamID, botID, filePath)
	if err != nil || lb == nil || s.cfg.Mode != "cloud" {
		return lb, err
	}
	return s.snapshotLaunchBot(ctx, teamID, lb)
}

// Raw resolution is also used while collecting a snapshot's child bundles;
// recursively snapshotting each child would lose the shared collection root.
func (s *Server) resolveBotTieredRaw(ctx context.Context, teamID, botID, filePath string) (*launchBot, error) {
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
	// PLATFORM tier (here) and the TEAM tier (teamBotRow) must tolerate the
	// same set, or a stored bot is silently bypassed for every non-canonical
	// spelling. Each resolves into its OWN variable and never rewrites
	// `slug`: rewriting it let a platform entry's canonical name miss the
	// team row and hijack the team tier — team > platform must hold even
	// across spellings.
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
			bs, found, err := s.teamBotRow(ctx, teamID, slug)
			switch {
			case err != nil:
				return nil, fmt.Errorf("resolve bot %q (team tier): %w", slug, err)
			case found:
				return s.storedLaunchBot(bs, "team")
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

// teamBotRow finds teamID's OWN botsource row for a bot id, tolerating the
// spelling variants the catalog and platform tiers tolerate. It reports
// (row, found, err); only a genuine absence is (_, false, nil), so a store
// failure can never be read as "this team authored no such bot" and fall
// through to a bundle nobody chose.
//
// Two passes. The indexed read answers the exact spelling — the common
// case, and the only one a `(tenant, slug)` index can serve. The scan is
// what makes the tolerance SYMMETRIC: `botsource.ValidSlug` admits `_`, so
// BOTH the query and the stored slug may be non-canonical, and matching
// those needs both sides normalized, which no index can answer. It runs
// only after the exact read missed, and reads one tenant's own rows.
func (s *Server) teamBotRow(ctx context.Context, teamID, slug string) (botsource.BotSource, bool, error) {
	tctx := store.WithTenant(ctx, teamID)
	bs, err := s.botSources.GetBySlug(tctx, teamID, slug)
	switch {
	case err == nil:
		return bs, true, nil
	case !errors.Is(err, botsource.ErrNotFound):
		return botsource.BotSource{}, false, err
	}
	norm := botregistry.NormalizeName(slug)
	list, err := s.botSources.ListByTenant(tctx, teamID)
	if err != nil {
		return botsource.BotSource{}, false, err
	}
	for i := range list {
		if botregistry.NormalizeName(list[i].Slug) == norm {
			return list[i], true, nil
		}
	}
	return botsource.BotSource{}, false, nil
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
	// The override wins — that is the tier's purpose — but say so when this
	// image bakes a NEWER bundle for the same slug, or a bot pinned by one
	// push keeps serving across releases with nothing naming the shadow.
	if m := bs.Manifest(); m != nil {
		s.warnIfOverrideShadowsNewerBake(bs.TenantID, bs.Slug, origin, m.Version)
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

// resolveBotSource resolves a bot id for the AUTOMATED launch surfaces
// (webhooks, schedules, board dispatch, triggers) through the full tier
// order: teamID's own botsource row, then the platform store, then the baked
// catalog. Errors when the id resolves nowhere.
//
// teamID is the tenant the launch is FOR — the card's, the subscription's,
// the schedule's, the webhook token's. It is the one thing this chokepoint
// cannot derive for itself, so it is a parameter and not a ctx read: nearby
// code re-scopes the ctx for forge and secret lookups, and a tier resolved
// off whichever tenant happened to be on the ctx would be a different
// question from "who is this launch for". Empty is legitimate only for a
// surface that genuinely has no tenant; the resolver sweep test keeps that
// set closed.
func (s *Server) resolveBotSource(ctx context.Context, teamID, botID string) (*launchBot, error) {
	lb, err := s.resolveBotTiered(ctx, teamID, botID, "")
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
	// slugs is EVERY platform row, versioned or not — a row still WINS
	// resolution when it carries no manifest (storedLaunchBot asks only for a
	// non-empty main.bot), so "does a platform row serve this slug" cannot be
	// answered from manifests. Nor from entries: materializeBotEntries drops a
	// row whose Materialize failed and returns nil on a ListWithSchema error,
	// so absence there is not absence in the store. The list read is the
	// authority, and this is its projection.
	slugs map[string]struct{}
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
			slugs:     make(map[string]struct{}, len(list)),
		}
		for i := range list {
			set.slugs[list[i].Slug] = struct{}{}
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

// teamBotManifest reads the manifest of a TEAM-authored bot row. It is for
// callers holding the tier a bot actually resolved through — a run's
// BotSourceTenant — so that a run's own manifest is read where the run came
// from. Nil when the tenant names no team row (empty, the platform sentinel,
// or no such slug), leaving the caller on the platform + baked tiers.
//
// Deliberately NOT folded into effectiveFindByName: see the contract at the
// top of this file — a lane whose LAUNCH is tenant-free must stay blind to a
// team fork, or it would describe a bundle it will not run.
//
// It resolves the row through teamBotRow, so one row resolution serves the
// launch, the entry metadata and the manifest: a card's spelling cannot
// reach one and miss another.
func (s *Server) teamBotManifest(ctx context.Context, tenantID, slug string) *bundle.Manifest {
	slug = strings.TrimSpace(slug)
	tenantID = strings.TrimSpace(tenantID)
	if s.botSources == nil || slug == "" || tenantID == "" || tenantID == botsource.PlatformTenantID {
		return nil
	}
	bs, found, err := s.teamBotRow(ctx, tenantID, slug)
	if err != nil {
		// A store blip is not "this team authored no such bot": say so,
		// then fall through to the tiers that can still answer.
		s.logger.Warn("bot source %s/%s: %v — reading the manifest fell through to the platform tier", tenantID, slug, err)
		return nil
	}
	if !found {
		return nil
	}
	return bs.Manifest()
}

// effectiveFindByNameForTeam is effectiveFindByName with the launching
// team's own row consulted first — the metadata counterpart of the launch
// resolution, for the lanes where the two describe the SAME delivery.
//
// It exists because a lane that launches a team's fork and then reads the
// origin's manifest disagrees with itself in silence: a fork that renames a
// `consumes:` var gets an empty seed, not an error. It resolves the row
// through teamBotRow, so the spelling a card carries reaches the same row on
// both reads.
//
// Deliberately NOT the default: effectiveFindByName stays tenant-free for
// the lanes whose launch is tenant-free too (see the contract at the top of
// this file). Widening it is #946, not this seam.
func (s *Server) effectiveFindByNameForTeam(ctx context.Context, teamID, name string) (botregistry.EntryWithSchema, bool, error) {
	if teamID != "" && s.botSources != nil && strings.TrimSpace(name) != "" {
		bs, found, err := s.teamBotRow(ctx, teamID, name)
		switch {
		case err != nil:
			// A store blip is not "this team authored no such bot": say so,
			// then fall through to the tiers that can still answer.
			s.logger.Warn("bot source %s/%s: %v — reading the metadata fell through to the platform tier", teamID, name, err)
		case found:
			if entries := s.materializeBotEntries([]botsource.BotSource{bs}); len(entries) == 1 {
				return entries[0], true, nil
			}
			s.logger.Warn("bot source %s/%s: metadata could not be materialized — falling through to the platform tier", teamID, bs.Slug)
		}
	}
	return s.effectiveFindByName(name)
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
