package server

import (
	"strconv"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/botregistry"
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
func (s *Server) bakedVersions() map[string]string {
	entries, err := botregistry.List(s.botListOptions())
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if v := strings.TrimSpace(e.Version); v != "" {
			out[e.Name] = v
		}
	}
	return out
}

// shadowsNewerBake reports the baked version for slug and whether the stored
// bundle at storedVersion is strictly OLDER than it — i.e. whether this
// override is holding back what this deployment would otherwise serve. Takes
// the catalog map so a caller comparing N rows pays for one walk.
func shadowsNewerBake(baked map[string]string, slug, storedVersion string) (string, bool) {
	b := baked[slug]
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
// exactly the silence this file exists to end. The baked version is
// deliberately NOT in the key: it cannot change without a new image, and a new
// image is a new process with a fresh map.
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
	key := strings.Join([]string{tenantID, origin, slug, storedVersion}, "\x00")
	if _, seen := staleOverrideWarned.LoadOrStore(key, struct{}{}); seen {
		return
	}
	// Not a shadow: the key stays stored. The answer cannot change within a
	// process, so re-walking the catalog on every later launch of the same row
	// would buy nothing.
	baked, shadowed := shadowsNewerBake(s.bakedVersions(), slug, storedVersion)
	if !shadowed {
		return
	}
	s.logger.Warn("bot %q serves the %s override of tenant %s at version %s while this deployment would otherwise serve %s — the override wins by design, so the newer bundle will not serve until it is re-pushed or removed (iterion remote admin bots push bots/%s, or DELETE /api/admin/bots/%s)",
		slug, origin, tenantID, storedVersion, baked, slug, slug)
}
