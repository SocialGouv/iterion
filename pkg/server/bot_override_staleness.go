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

// bakedVersions walks the catalog ONCE and returns slug → manifest version.
//
// Callers comparing several rows build it once and reuse it: resolving a
// version per row would re-walk every configured bot root and re-parse every
// manifest for each one, turning a listing into O(rows × catalog) filesystem
// work. The version comes straight off botregistry.Entry, which already
// mirrors the manifest field — loading the manifest again would re-read what
// the walk just produced.
//
// "Baked" means whatever THIS server discovers on disk, deliberately the same
// source every other bot lookup uses: the field answers "what would serve if
// this override were removed", not "what some image ships". With no
// --bots-path pinned that follows the live WorkDir, so on a local studio the
// answer legitimately changes with the open project — which is the correct
// answer to the question the field asks.
// bakedCatalog is the cached slug → version projection of the on-disk catalog.
// A struct rather than a bare map so "never read successfully" (nil) stays
// distinguishable from "read, and it holds nothing" (empty map): reporting a
// broken catalog as a clean inventory would be a lie told by the very endpoint
// the runbook calls the check to run after a release.
type bakedCatalog struct {
	versions map[string]string
}

// newBakedCatalogResolver builds the TTL cache. Walking the catalog is the
// expensive half of the comparison, so it is the DATA that is cached, never
// the verdict: a cached verdict would outlive the platform-override overlay it
// was computed against. A walk failure propagates, so the resolver logs it and
// serves the LAST-KNOWN map rather than an empty one.
func (s *Server) newBakedCatalogResolver() *platformcfg.Resolver[bakedCatalog] {
	return platformcfg.NewResolverFunc(func(context.Context) (*bakedCatalog, error) {
		entries, err := botregistry.List(s.botListOptions())
		if err != nil {
			return nil, fmt.Errorf("bot catalog walk for the override-staleness check: %w", err)
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
	set := s.platformBotSetCached()
	if set == nil {
		return baked, true
	}
	// Copy: the cached catalog map is shared, and the platform overlay is
	// per-tenant-tier.
	out := make(map[string]string, len(baked)+len(set.manifests))
	for k, v := range baked {
		out[k] = v
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
// The dedup check runs BEFORE the catalog walk, so a repeat launch of the same
// row costs a map lookup rather than a full discovery pass.
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
	s.logger.Warn("bot %q serves the %s override of tenant %s at version %s while this deployment would otherwise serve %s — the override wins by design, so the newer bundle will not serve until it is re-pushed or removed (iterion remote admin bots push bots/%s, or DELETE /api/admin/bots/%s)",
		slug, origin, tenantID, storedVersion, below, slug, slug)
}
