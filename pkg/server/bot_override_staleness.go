package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
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
		entries, diagnostics, err := botregistry.ListWithDiagnostics(s.botListOptions())
		if err != nil {
			return nil, fmt.Errorf("bot catalog walk for the override-staleness check: %w", err)
		}
		if len(diagnostics) > 0 {
			return nil, fmt.Errorf("bot catalog walk for the override-staleness check: %s", diagnostics[0].Error)
		}
		out := bakedCatalog{versions: make(map[string]string, len(entries))}
		for _, e := range entries {
			if v := strings.TrimSpace(e.Version); v != "" {
				out.versions[e.Name] = v
			}
		}
		return &out, nil
	}, s.logger.Warn)
}

// bakedVersions serves the cached slug → version projection, with false when
// the catalog has never been read successfully — the caller must then report
// "unknown", never "nothing is shadowed".
//
// A caller comparing several rows takes the map ONCE and reuses it: resolving
// a version per row would re-walk every configured bot root and re-parse every
// manifest for each one, turning a listing into O(rows × catalog) filesystem
// work. The returned map is the SHARED cached one — read-only for callers;
// versionsBelow copies before overlaying the platform tier onto it.
func (s *Server) bakedVersions() (map[string]string, bool) {
	c := s.bakedCatalog.Get(context.Background())
	if c == nil {
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
	// baked version is never what a team row holds back once one exists. Drop
	// it first, then put back only the versions actually known: a platform row
	// with no manifest (a fork of a loose <name>.bot copies none) or an empty
	// version leaves the slug UNORDERED, which reports nothing — rather than
	// naming the bake, a bundle removing this team row would not serve.
	for slug := range set.slugs {
		delete(out, slug)
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

// shadowsNewerVersion reports what would serve for slug without this row, and
// whether the stored bundle at storedVersion is strictly OLDER than it — i.e.
// whether this override is holding back what this deployment would otherwise
// serve. Takes the map so a caller comparing N rows pays for one walk.
func shadowsNewerVersion(below map[string]string, slug, storedVersion string) (string, bool) {
	b := below[slug]
	if b == "" || strings.TrimSpace(storedVersion) == "" {
		return b, false
	}
	cmp, ok := bundleVersionOrder(storedVersion, b)
	return b, ok && cmp < 0
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
