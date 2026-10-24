// The shipped-manifest sweep of the cut's report.
//
// A downward realignment is the one move that can strand a shipped bundle:
// the pin drops from the pending next minor to the patch being cut
// (`3.180.0 → 3.179.1`), and a manifest whose `requires.iterion` copied the
// pending number now refuses runners that can read its sources. The cut is
// the one place that holds both numbers, so after the realignment it names
// every shipped `bots/*/` and `examples/*/` manifest still carrying the old
// value — as a warning beside the realignment line, never a refusal: a
// floor above the minimum may be deliberate (a backend capability, a bug
// fix), and no arithmetic can tell an intended floor from a stale one.
package floorsalign

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// realignment is one downward move the cut made: the pin's value before it
// and the release it now names.
type realignment struct {
	from string
	to   string
}

// shipped is one manifest the sweep looked at. err carries why it could not
// be read — reported in the warning list, never skipped: a stale floor in a
// manifest the sweep cannot certify would otherwise pass unnamed.
type shipped struct {
	path     string // relative to the repo root
	manifest *bundle.Manifest
	err      error
}

// symbolicManifestActions joins uncut manifest floors to the same staged
// rewrite as Go pins. Numeric manifest floors retain their own contract.
func symbolicManifestActions(root, version string) []Action {
	var actions []Action
	for _, s := range shippedManifests(root) {
		if s.err != nil {
			continue // The existing stale-manifest report names unreadable bundles.
		}
		c, err := bundle.ParseEngineConstraint(s.manifest.Requires.Iterion)
		if err != nil || !c.Next {
			continue
		}
		p := Pin{Name: s.path + ":requires.iterion", File: s.path, Value: "next", Index: -1}
		actions = append(actions, Action{Pin: p, Kind: Realign, To: version,
			Detail: fmt.Sprintf("%s = next → %s (realigned to the release being cut)", p.Name, version)})
	}
	return actions
}

// staleManifestWarnings is the warning lines the cut's report carries after
// a downward realignment: one per shipped manifest whose `requires.iterion`
// still names the realigned pin's old value, and one per manifest the sweep
// could not read. Empty without a downward realignment, and when nothing
// shipped still names the old value.
func staleManifestWarnings(root string, actions []Action) []string {
	var pairs []realignment
	seen := map[string]bool{}
	for _, a := range actions {
		if a.Kind != Realign {
			continue
		}
		down, ok := bundle.CompareVersions(a.To, a.Pin.Value)
		if !ok || down >= 0 {
			continue // only a move DOWN can strand a manifest
		}
		if !seen[a.Pin.Value] {
			seen[a.Pin.Value] = true
			pairs = append(pairs, realignment{from: a.Pin.Value, to: a.To})
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	var lines []string
	for _, s := range shippedManifests(root) {
		if s.err != nil {
			lines = append(lines, fmt.Sprintf("warning: %s could not be read for the stale-floor check: %v", s.path, s.err))
			continue
		}
		for _, p := range pairs {
			if !namesTheRelease(s.manifest, p.from) {
				continue
			}
			lines = append(lines, fmt.Sprintf(
				"warning: %s requires.iterion %q still names %s, which the cut just realigned to %s: the bundle refuses runners that can read its sources — if the floor copied the pending pin rather than a real need, lower it to %q; a floor above what its sources need may be deliberate, and this is a warning, never a refusal",
				s.path, s.manifest.Requires.Iterion, p.from, p.to, ">= "+p.to))
		}
	}
	return lines
}

// shippedManifests reads every shipped bundle manifest under
// `<root>/bots/*/manifest.yaml` and `<root>/examples/*/manifest.yaml` — the
// two collections the release test walks. ReadDir returns its entries
// sorted, so the sweep answers in a deterministic order, bots before
// examples. A collection that does not exist ships nothing; a directory
// without a manifest ships nothing; a manifest that cannot be read or
// decoded is carried with its error.
func shippedManifests(root string) []shipped {
	var out []shipped
	for _, collection := range []string{"bots", "examples"} {
		entries, err := os.ReadDir(filepath.Join(root, collection))
		if err != nil {
			if !os.IsNotExist(err) {
				out = append(out, shipped{path: collection + "/", err: err})
			}
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			rel := filepath.Join(collection, entry.Name(), "manifest.yaml")
			m, err := bundle.LoadManifest(filepath.Join(root, rel))
			if err != nil {
				out = append(out, shipped{path: rel, err: err})
				continue
			}
			if m == nil || m.Requires == nil || strings.TrimSpace(m.Requires.Iterion) == "" {
				continue // no floor declared: nothing can be stranded
			}
			out = append(out, shipped{path: rel, manifest: m})
		}
	}
	return out
}

// namesTheRelease reports whether the manifest's floor names release: its
// parsed minimum equals release's, zero-padded — the same reading
// CompareVersions gives two orderable versions. ParseEngineConstraint
// cannot fail here for a decoded manifest (the decoder validates the
// requirement), and a false for any other input says "not the realigned
// value" rather than guessing a match.
func namesTheRelease(m *bundle.Manifest, release string) bool {
	c, err := bundle.ParseEngineConstraint(m.Requires.Iterion)
	if err != nil {
		return false
	}
	cmp, ok := c.CompareFloor(release)
	return ok && cmp == 0
}
