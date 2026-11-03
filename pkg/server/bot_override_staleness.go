package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A stored bundle (team or platform botsource row) OUTRANKS the baked catalog
// at every launch surface — that is the tier's whole purpose. The cost is that
// a bundle pushed once keeps serving after a later release bakes a NEWER one
// into the image, and nothing said so.
//
// The override is never REFUSED: pinning an older bundle is a legitimate
// operator choice, and this package warns rather than rejects wherever an
// operator could want the thing. What changes is that a shadowed newer bake is
// no longer silent — it is reported once per distinct shadow in the log, and
// on every row the operator's own inventory returns.

// bundleVersionOrder compares two free-form bundle version strings by their
// dotted numeric components: -1, 0 or +1, with ok=false when either side
// cannot be ordered.
//
// Manifest.Version is documented free-form ("semver or any"), so an
// unparsable pair is UNORDERED rather than guessed — claiming staleness
// against an operator's own naming scheme would be a false alarm, and a false
// alarm on a warning nobody can silence is worse than the silence it replaces.
// Components are compared numerically (so 0.10.0 > 0.9.0, which a string
// compare gets backwards), and a shorter version is padded with zeros
// (1.2 == 1.2.0).
//
// KNOWN BLIND SPOT: a suffixed version (0.8.0-rc1) is unorderable, so an
// override pinned at one is never flagged against a plain 0.8.0 bake. Widening
// this to semver precedence would mean deciding that -rc1 sorts BEFORE 0.8.0
// for every operator, which the free-form contract does not license.
func bundleVersionOrder(a, b string) (int, bool) {
	pa, okA := numericVersionParts(a)
	pb, okB := numericVersionParts(b)
	if !okA || !okB {
		return 0, false
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var ca, cb int
		if i < len(pa) {
			ca = pa[i]
		}
		if i < len(pb) {
			cb = pb[i]
		}
		if ca != cb {
			if ca < cb {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// numericVersionParts splits a version into its dotted numeric components. A
// leading "v" is tolerated; anything else non-numeric makes the whole version
// unorderable.
func numericVersionParts(v string) ([]int, bool) {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return nil, false
	}
	fields := strings.Split(s, ".")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// bakedCatalog is the cached slug → version projection of the on-disk catalog.
// A struct rather than a bare map so "never read successfully" (nil) stays
// distinguishable from "read, and it holds nothing" (empty map): reporting a
// broken catalog as a clean inventory would be a lie told by the very endpoint
// the runbook calls the check to run after a release.
//
// Every discovered slug is a KEY, carrying an empty version when the bundle
// declares none (bots/smoke bakes no manifest at all): "a bundle serves this
// slug and what it serves cannot be ordered" is a different fact from "no
// other tier offers this slug", and a projection that drops the unversioned
// entries folds the two — reporting a comparison nobody could make as the
// clean reading of a deployment with nothing to report.
//
// "Baked" means whatever THIS server discovers on disk, deliberately the same
// source every other bot lookup uses: it answers "what would serve if this
// override were removed", not "what some image ships". With no --bots-path
// pinned that follows the live WorkDir, so on a local studio the answer
// legitimately changes with the open project — which is the correct answer to
// the question the field asks.
type bakedCatalog struct {
	versions map[string]string
}

// newBakedCatalogResolver builds the TTL cache. Walking the catalog is the
// expensive half of the comparison, so it is the DATA that is cached, never
// the verdict: a cached verdict would outlive the platform-override overlay it
// was computed against. A walk failure propagates, so the resolver logs it and
// serves the LAST-KNOWN map rather than an empty one.
//
// THE CONTEXT IS DELIBERATELY DISCARDED, and that is a documented deviation,
// not an oversight: botregistry.List takes no ctx, so platformcfg's 3s
// fetchTimeout — which bounds every OTHER resolver's fetch — does not bound
// this one. A cold-start caller therefore blocks for however long the bot-root
// walk takes (the walk is a LOCAL filesystem read; on a network mount that is
// not a bound anyone chose). Wrapping the walk in a goroutine and selecting on
// ctx would NOT fix it — it cannot cancel a blocked walk, only abandon it, and
// since Resolver.Get re-arms the TTL after a failure a wedged mount would
// strand a fresh goroutine every refresh interval: bounded blocking traded for
// an unbounded leak. The real remedy is cooperative cancellation inside
// botregistry (or one long-lived single-flight walker), both out of scope here.
func (s *Server) newBakedCatalogResolver() *platformcfg.Resolver[bakedCatalog] {
	return platformcfg.NewResolverFunc(func(context.Context) (*bakedCatalog, error) {
		c, err := s.walkBakedCatalog()
		// The outcome travels WITH the map, in the one place that knows it.
		// The resolver serves last-known values through a failure, so from
		// the map alone a reader cannot tell a fresh reading from a blind
		// one — and this check's whole contract is that it never publishes
		// the clean zero of a pass it could not run.
		s.bakedCatalogWalkFailed.Store(err != nil)
		return c, err
	}, s.logger.Warn)
}

// walkBakedCatalog is one discovery pass over the configured bot roots,
// projected to slug → version.
//
// A catalog nobody could look at is UNKNOWN, never empty. botregistry skips a
// MISSING root silently so callers can pass optimistic default paths, so a
// dropped ITERION_BOTS_PATH — a custom image, an overridden command — hands
// this projection an empty map, which classifyShadow reads for every slug as
// "no other tier offers it": the byte-identical exposition of a clean
// deployment, published by a check that opened nothing. An EXISTING root
// holding no bundle is the opposite fact and stays available, which is the
// escape hatch for a deployment that deliberately serves stored bundles only:
// point the bots path at a directory that exists.
func (s *Server) walkBakedCatalog() (*bakedCatalog, error) {
	// Versions only: an overlay its reader refused decides no version, so
	// this projection reads past it (every entry listed disabled, which it
	// never reads).
	opts := s.botListOptions()
	opts.ListPastAnUnreadOverlay = true
	if !botregistry.AnyRootPresent(opts.Paths) {
		where := "no bot root is configured"
		if len(opts.Paths) > 0 {
			where = "none of " + strings.Join(opts.Paths, ", ") + " is on disk"
		}
		return nil, fmt.Errorf("bot catalog walk for the override-staleness check: %s, so what serves below an override is unknown — point --bots-path (ITERION_BOTS_PATH) at the baked catalog, or at a directory that exists to declare this deployment bakes none", where)
	}
	entries, diagnostics, err := botregistry.ListWithDiagnostics(opts)
	if err != nil {
		return nil, fmt.Errorf("bot catalog walk for the override-staleness check: %w", err)
	}
	for _, d := range diagnostics {
		// A SHADOWED source leaves no gap in this projection: the name it
		// lost routes to the claimer, whose version is the one collected
		// below — including when the claimer is a loose .bot carrying no
		// version, in which case the slug is present with an EMPTY version
		// (nothing orderable serves it). Any other diagnostic is a source
		// whose version could not be read, and answering "nothing is
		// shadowed" out of a partial catalog would clear an override that
		// really is stale.
		if d.Kind == botregistry.DiscoveryErrorShadowed {
			continue
		}
		// A source listed without its frontmatter identity still carries
		// its manifest's version, and an overlay that is not read hides
		// nothing from this projection: neither leaves a version unread.
		if d.Kind == botregistry.DiscoveryErrorIdentity || d.Kind == botregistry.DiscoveryErrorOverlay {
			continue
		}
		return nil, fmt.Errorf("bot catalog walk for the override-staleness check: %s", d.Error)
	}
	out := bakedCatalog{versions: make(map[string]string, len(entries))}
	for _, e := range entries {
		out.versions[e.Name] = strings.TrimSpace(e.Version)
	}
	return &out, nil
}

// bakedVersions serves the cached slug → version projection, with false when
// the LAST walk did not succeed — the caller must then report "unknown", never
// "nothing is shadowed".
//
// Two failures, one answer. Never read at all is the nil record; read once and
// failing NOW is the resolver serving its last-known map, which is the shape a
// live pod takes when a bot root wedges after boot (the cold shape only exists
// for the first 30s of a process). Both are a comparison this replica cannot
// make, and the difference between them is not a reason to publish one of them
// as current.
//
// A caller comparing several rows takes the map ONCE and reuses it: resolving
// a version per row would re-walk every configured bot root and re-parse every
// manifest for each one, turning a listing into O(rows × catalog) filesystem
// work. The returned map is the SHARED cached one — read-only for callers;
// versionsBelow copies before overlaying the platform tier onto it.
func (s *Server) bakedVersions() (map[string]string, bool) {
	c := s.bakedCatalog.Get(context.Background())
	if c == nil || s.bakedCatalogWalkFailed.Load() {
		return nil, false
	}
	return c.versions, true
}

// versionsBelow returns what would serve for each slug if tenantID's own row
// were removed — the whole point of the comparison, and not the same map for
// both tiers. Resolution is team → platform → baked, so a TEAM row is shadowed
// by the platform override when one exists, not by the baked catalog behind
// it: comparing a team row against the bake alone would call a 0.9.0 team
// override "current" while it holds back a 1.0.0 platform one.
// The bool is false when the catalog could not be read at all: the caller must
// then report "unknown", never "nothing is shadowed".
func (s *Server) versionsBelow(tenantID string) (map[string]string, bool) {
	baked, ok := s.bakedVersions()
	if !ok {
		return nil, false
	}
	if tenantID == botsource.PlatformTenantID {
		return baked, true
	}
	if s.botSources == nil {
		// No stored tier at all on this deployment: the bake IS what serves
		// below a team row, and there is nothing unknown about that.
		return baked, true
	}
	set := s.platformBotSetCached()
	if set == nil {
		// A SUCCESSFUL read that found nothing returns a non-nil set with an
		// empty map, so nil here is never "no platform rows" — it is a
		// cold-start failure of the platform read. Answering "the bake" would
		// measure a team row deliberately pinned to match an older platform
		// override against the bake instead, and the warn path caches only
		// POSITIVE verdicts, so that false line could never be superseded once
		// the overlay recovered. Unknown, exactly as an unreadable catalog is.
		return nil, false
	}
	// Copy: the cached catalog map is shared, and the platform overlay is
	// per-tenant-tier.
	out := make(map[string]string, len(baked)+len(set.slugs))
	for k, v := range baked {
		out[k] = v
	}
	// A platform row SERVES for its slug whatever version it carries — so the
	// baked version is never what a team row holds back once one exists.
	// Blank it first, then put back only the versions actually known: a
	// platform row with no manifest (a fork of a loose <name>.bot copies
	// none) or an empty version leaves the slug PRESENT with no version,
	// which reports no verdict — neither naming the bake, which a bundle
	// removing this team row would not serve, nor dropping the slug, which
	// would claim nothing serves below this row at all.
	for slug := range set.slugs {
		out[slug] = ""
	}
	for slug, m := range set.manifests {
		if m == nil {
			continue
		}
		if v := strings.TrimSpace(m.Version); v != "" {
			out[slug] = v
		}
	}
	return out, true
}

// shadowClass is what one stored bundle IS, measured against what would serve
// without it. Three outcomes, never two: "not shadowing" and "cannot be
// ordered" are different facts, and a reader that folds them together reports
// a free-form pin — or a bundle that declares no version at all — as healthy.
type shadowClass int

const (
	// shadowClassNone — nothing is held back: no other tier offers the slug
	// at all, or the pair orders and the stored bundle is at or above what
	// sits below it. The only class that may read like a clean deployment,
	// because it IS one.
	shadowClassNone shadowClass = iota
	// shadowClassNewer — the pair orders and the stored bundle is strictly
	// older, so the override is holding back what this deployment would
	// otherwise serve. The reportable shadow.
	shadowClassNewer
	// shadowClassUnorderable — a bundle DOES serve below this row and the
	// two cannot be ordered, so no staleness verdict can be made either way:
	// a suffixed or free-form pin on either side, or a side that declares no
	// version at all (manifest.yaml is optional for a stored bundle, and
	// bots/smoke bakes none). Its own class, never folded into "none": the
	// shadow this ticket exists to surface can hide in here, and publishing
	// it as the reading of a healthy row is the silence the gauge replaces.
	// Never a shadow either — a staleness alarm on an operator's own naming
	// scheme, or on a bundle that never declared a version, would be a false
	// one nobody can clear.
	shadowClassUnorderable
)

// classifyShadow reports what would serve for slug without this row, and which
// class the pair falls in. The single comparison every reader of this fact goes
// through — the inventory field, the resolve-time warning and the gauge pass —
// so the alert and the row an operator reads after it can never disagree.
// Takes the map so a caller comparing N rows pays for one walk.
func classifyShadow(below map[string]string, slug, storedVersion string) (string, shadowClass) {
	b, serves := below[slug]
	if !serves {
		// No other tier offers the slug: this row holds nothing back, and
		// saying so is the one honest "none".
		return "", shadowClassNone
	}
	stored := strings.TrimSpace(storedVersion)
	if b == "" || stored == "" {
		// Something DOES serve below and one of the two declares no version
		// — a stored bundle pushed without a manifest, or a baked bundle
		// that carries none. The comparison cannot be made, which is the
		// unorderable class and NOT the clean one: the override still
		// outranks everything below it forever, which is the very fact this
		// check reports.
		return b, shadowClassUnorderable
	}
	cmp, ok := bundleVersionOrder(stored, b)
	switch {
	case !ok:
		return b, shadowClassUnorderable
	case cmp < 0:
		return b, shadowClassNewer
	default:
		return b, shadowClassNone
	}
}

// shadowsNewerVersion reports what would serve for slug without this row, and
// whether the stored bundle at storedVersion is strictly OLDER than it — i.e.
// whether this override is holding back what this deployment would otherwise
// serve.
func shadowsNewerVersion(below map[string]string, slug, storedVersion string) (string, bool) {
	b, class := classifyShadow(below, slug, storedVersion)
	return b, class == shadowClassNewer
}

// staleOverrideWarned dedups the resolve-time warning, so a shadowed override
// costs one line per distinct shadow instead of one per launch — a bot serving
// every webhook would otherwise drown its own signal.
//
// The key carries the TENANT and the origin, not just the slug: on the team
// tier many tenants hold a row for the same slug, and a slug-only key would
// let the first team to launch consume it and silence every other team —
// exactly the silence this file exists to end.
//
// Only a POSITIVE verdict is ever stored. Caching "nothing to report" would
// silence a shadow that appears later in the same process, and both inputs do
// change at runtime: a team row is measured against the platform overlay,
// which is a TTL cache that every `admin bots push` refills. Recomputing is
// cheap because what is cached is the catalog walk (bakedCatalog), not the
// verdict.
var staleOverrideWarned sync.Map

// warnIfOverrideShadowsNewerBake logs, once per (tenant, origin, slug, stored
// version), that a stored bundle is serving while this deployment would
// otherwise serve a newer one. Deliberately observational: the launch proceeds
// on the override.
//
// The dedup check runs LAST, after the comparison — deliberately, and not the
// cheaper order. Consuming the key first would make the dedup a cache of the
// VERDICT, and a negative verdict must stay recomputable: the platform overlay
// a team row is measured against is a TTL cache that every `admin bots push`
// refills, so a shadow can appear mid-process on inputs that changed under a
// key already burned. What keeps the repeat launch cheap is that the expensive
// half — the catalog walk — is itself TTL-cached (bakedCatalog), leaving a map
// copy and an overlay merge per call rather than a discovery pass.
func (s *Server) warnIfOverrideShadowsNewerBake(tenantID, slug, origin, storedVersion string) {
	if s.logger == nil || strings.TrimSpace(storedVersion) == "" {
		return
	}
	versions, ok := s.versionsBelow(tenantID)
	if !ok {
		// The catalog could not be read; the resolver already warned about
		// that. Staying silent here is honest — claiming "nothing shadowed"
		// would not be.
		return
	}
	below, shadowed := shadowsNewerVersion(versions, slug, storedVersion)
	if !shadowed {
		return
	}
	key := strings.Join([]string{tenantID, origin, slug, storedVersion}, "\x00")
	if _, seen := staleOverrideWarned.LoadOrStore(key, struct{}{}); seen {
		return
	}
	s.logger.Warn("bot %q serves the %s override of tenant %s at version %s while this deployment would otherwise serve %s — the override wins by design, so the newer bundle will not serve until it is re-pushed or removed (%s)",
		slug, origin, tenantID, storedVersion, below, shadowRemedy(tenantID, slug))
}

// shadowRemedy names the endpoints that clear the shadow FOR THIS ROW's tier.
// The team and platform tiers are both first-class here (storedLaunchBot warns
// for either origin), and they live at different endpoints: handing a team row
// the platform remedy sends the operator to a 404 — or, for a super-admin, to
// deleting the PLATFORM override of that slug, a different row whose removal
// changes what every tenant is served.
func shadowRemedy(tenantID, slug string) string {
	if tenantID != botsource.PlatformTenantID {
		return fmt.Sprintf("re-push it, or DELETE /api/teams/%s/bot-sources/%s", tenantID, slug)
	}
	return fmt.Sprintf("iterion remote admin bots push bots/%s, or DELETE /api/admin/bots/%s", slug, slug)
}

// ---- the poll that looks on its own -------------------------------------

// overrideShadowObserveInterval is the observer's cadence, read against what
// consumes it: IterionBotOverrideShadowsNewer waits `for: 1h`, so twelve
// readings land inside the window the alert needs before it fires at all, and
// a shadow is a condition that lasts for releases.
//
// A pass is NOT free, which is why it is not the sweeper's minute: it lists
// every platform row with its full bundle content (botsource.Store offers no
// metadata-only read, and MaxBundleBytes is 6 MiB per row), and the catalog
// walk it compares them against is TTL-cached for platformcfg.DefaultTTL —
// SHORTER than this cadence, so every single pass re-walks every configured
// bot root. Sub-minute freshness would be worth a metadata-only projection on
// the store seam, never a tighter spin on this one.
const overrideShadowObserveInterval = 5 * time.Minute

// startOverrideShadowObserver arms the poll that turns the two PASSIVE shadow
// reporters into a signal. Both of them need someone to show up: the
// inventory fields wait for an operator to call GET /api/admin/bots, and
// warnIfOverrideShadowsNewerBake fires at resolve time, so a bot nobody
// launches warns never — which is exactly the bot whose override rots.
//
// No election and no durable state: the gauges are a reading of THIS
// replica's own filesystem catalog against the shared platform rows, so every
// replica is entitled to its own answer (a rolling release serves two image
// generations at once, and a shadow true on the new pods is the one an
// operator must see). The alert rules aggregate with max accordingly.
func (s *Server) startOverrideShadowObserver() {
	if s.cfg.Metrics == nil || s.botSources == nil {
		// No /metrics endpoint, or no stored tier at all (local mode): no
		// override exists to shadow anything, so the registered zeros are
		// the honest reading and there is nothing to poll.
		return
	}
	// Unavailable until a pass says otherwise. Every one of these gauges
	// reads 0 on a healthy deployment AND on one where the loop never ran,
	// and "the check could not run" must never be published as a clean
	// inventory — the same reason the listing carries
	// shadow_check_unavailable rather than an empty shadow field.
	s.cfg.Metrics.BotShadowCheckUnavailable.Set(1)
	s.goUntilShutdown("server.botOverrideShadowObserver", func(ctx context.Context) {
		s.runOverrideShadowObserver(ctx, overrideShadowObserveInterval)
	})
}

// runOverrideShadowObserver polls until ctx is cancelled.
func (s *Server) runOverrideShadowObserver(ctx context.Context, interval time.Duration) {
	// Boot pass before the ticker: a restart re-phases the ticker onto this
	// pod's start time, so a whole interval would otherwise pass with the
	// per-slug gauges carrying no series at all — which an alert on `> 0`
	// reads as "no shadow", the one answer this loop exists to stop guessing.
	s.observePlatformOverrideShadows(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.observePlatformOverrideShadows(ctx)
		}
	}
}

// observePlatformOverrideShadows publishes one whole pass over the PLATFORM
// tier: per slug, whether its override holds back a newer baked bundle.
//
// Platform only, deliberately. botsource.Store offers no cross-tenant listing
// (ListByTenant is the widest read), so covering the team tier would cost one
// Mongo list per team per pass — and a platform override is the row that runs
// across every tenant, which is where a shadow costs the most. The team tier
// keeps the inventory field and the resolve-time warning.
//
// The rows are read with the SAME call the admin inventory uses rather than
// from the 30s platformBotSetCached projection: the alert tells an operator to
// run `iterion remote admin bots`, and a gauge computed off a cache that
// serves last-known through an outage would disagree with the row they then
// read — and would hide the outage this signal has to surface.
func (s *Server) observePlatformOverrideShadows(ctx context.Context) {
	m := s.cfg.Metrics
	if m == nil || s.botSources == nil {
		return
	}
	below, ok := s.versionsBelow(botsource.PlatformTenantID)
	if !ok {
		// The catalog could not be read; the resolver already logged why.
		// HOLD the per-slug gauges — zeroing them here would paint a clean
		// inventory at exactly the moment the check went blind, and an
		// override that WAS shadowing still is.
		m.BotShadowCheckUnavailable.Set(1)
		return
	}
	rows, err := s.botSources.ListByTenant(store.WithTenant(ctx, botsource.PlatformTenantID), botsource.PlatformTenantID)
	if err != nil {
		// The EDGE only, like the resolve-time warning sixty lines up: the
		// branch runs on every pass of every replica, so a store outage
		// would otherwise write one identical line per pod per pass and
		// drown the signal it carries. The standing condition is the gauge's
		// job.
		if s.overrideShadowRowsWentBlind(true) {
			s.logWarn("bot override-shadow observer: the platform rows could not be listed (%v) — iterion_bot_shadow_check_unavailable goes to 1 and the per-slug gauges hold their last values until the store answers again", err)
		}
		m.BotShadowCheckUnavailable.Set(1)
		return
	}
	s.overrideShadowRowsWentBlind(false)
	// Both legs read, so a WHOLE pass is publishable. Write the new values
	// FIRST and retire the vanished slugs after — never Reset-then-write: a
	// scrape landing inside that window reads an EMPTY vector for a slug that
	// is shadowing, and `max by (slug) (...) > 0` over an empty vector is
	// "no shadow", which resets the alert's `for` clock on a condition that
	// never cleared.
	published := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		version := ""
		if mf := row.Manifest(); mf != nil {
			version = strings.TrimSpace(mf.Version)
		}
		_, class := classifyShadow(below, row.Slug, version)
		// Every row seen is written on BOTH vectors, zeros included: an
		// absent series is what an unscraped one looks like, so a row that
		// stopped shadowing must say 0 rather than go quiet.
		shadowsNewer, unorderable := 0.0, 0.0
		switch class {
		case shadowClassNewer:
			shadowsNewer = 1
		case shadowClassUnorderable:
			unorderable = 1
		case shadowClassNone:
		}
		m.BotOverrideShadowsNewer.WithLabelValues(row.Slug).Set(shadowsNewer)
		m.BotOverrideUnorderableVersion.WithLabelValues(row.Slug).Set(unorderable)
		published[row.Slug] = struct{}{}
	}
	s.retireOverrideShadowSeries(published)
	// Last, and only now: the pass read both legs and published every row.
	m.BotShadowCheckUnavailable.Set(0)
}

// retireOverrideShadowSeries drops the series of the slugs the last pass
// published and this one did not — an override that has been removed, which is
// the very remedy the alert asks for and must not keep paging. Retiring AFTER
// the writes, per slug, is what keeps the pass free of a window in which a
// shadowing slug carries no series at all.
func (s *Server) retireOverrideShadowSeries(published map[string]struct{}) {
	m := s.cfg.Metrics
	s.overrideShadowMu.Lock()
	defer s.overrideShadowMu.Unlock()
	for slug := range s.overrideShadowSlugs {
		if _, still := published[slug]; still {
			continue
		}
		m.BotOverrideShadowsNewer.DeleteLabelValues(slug)
		m.BotOverrideUnorderableVersion.DeleteLabelValues(slug)
	}
	s.overrideShadowSlugs = published
}

// overrideShadowRowsWentBlind records whether the platform rows could be
// listed and reports the RISING edge — true only on the pass that discovers
// the outage, so the warning costs one line per outage instead of one per
// pass. A pass that recovers clears it, so the next outage speaks again.
func (s *Server) overrideShadowRowsWentBlind(blind bool) bool {
	s.overrideShadowMu.Lock()
	defer s.overrideShadowMu.Unlock()
	rising := blind && !s.overrideShadowRowsBlind
	s.overrideShadowRowsBlind = blind
	return rising
}
