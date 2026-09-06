package server

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// A stored bundle (team or platform botsource row) OUTRANKS the baked catalog
// at every launch surface — that is the tier's whole purpose. The cost is that
// a bundle pushed once keeps serving after a later release bakes a NEWER one
// into the image, and nothing said so.
//
// The override is never REFUSED: pinning an older bundle is a legitimate
// operator choice, and this package warns rather than rejects wherever an
// operator could want the thing. What changes is that a shadowed newer bake is
// no longer silent — it is reported once per drift state in the log, and on
// every row the operator's own inventory returns.

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

// bakedBundleVersion returns the version this image bakes for slug, or "" when
// the slug has no baked bundle (a stored-only bot shadows nothing) or its
// manifest carries no version.
func (s *Server) bakedBundleVersion(slug string) string {
	path, err := botregistry.ResolveBotPath(slug, s.effectivePaths())
	if err != nil {
		return ""
	}
	m, err := bundle.LoadManifest(filepath.Join(filepath.Dir(path), bundle.ManifestFile))
	if err != nil || m == nil {
		return ""
	}
	return strings.TrimSpace(m.Version)
}

// overrideShadowsNewerBake reports the baked version for slug and whether the
// stored bundle at storedVersion is strictly OLDER than it — i.e. whether this
// override is holding back what the deployment already ships.
func (s *Server) overrideShadowsNewerBake(slug, storedVersion string) (baked string, shadowed bool) {
	baked = s.bakedBundleVersion(slug)
	if baked == "" || strings.TrimSpace(storedVersion) == "" {
		return baked, false
	}
	cmp, ok := bundleVersionOrder(storedVersion, baked)
	return baked, ok && cmp < 0
}

// staleOverrideWarned dedups the resolve-time warning per drift state, so a
// shadowed override costs one line per (slug, stored, baked) triple instead of
// one per launch — a bot serving every webhook would otherwise drown its own
// signal.
var staleOverrideWarned sync.Map

// warnIfOverrideShadowsNewerBake logs, once per drift state, that a stored
// bundle is serving while this image bakes a newer one for the same slug.
// Deliberately observational: the launch proceeds on the override.
func (s *Server) warnIfOverrideShadowsNewerBake(slug, origin, storedVersion string) {
	if s.logger == nil {
		return
	}
	baked, shadowed := s.overrideShadowsNewerBake(slug, storedVersion)
	if !shadowed {
		return
	}
	key := slug + "\x00" + storedVersion + "\x00" + baked
	if _, seen := staleOverrideWarned.LoadOrStore(key, struct{}{}); seen {
		return
	}
	s.logger.Warn("bot %q serves the %s override at version %s while this image bakes %s — the override wins by design, so the newer bundle will not serve until it is re-pushed or removed (iterion remote admin bots push bots/%s, or DELETE /api/admin/bots/%s)",
		slug, origin, storedVersion, baked, slug, slug)
}
