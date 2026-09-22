package bundle

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// releaseNotesMarkers names, per syntax floor, a word the release notes of
// the pinned release carry — the changelog is generated from the merged
// commits' subjects — so a pin that names a release cut without the syntax
// (another feature took the number first) is loud. An empty marker is an
// exemption, with its reason beside it; a floor missing from this table
// fails the release test and refuses the cut: the next syntax declares its
// word.
var releaseNotesMarkers = map[string]string{
	// #1154 merged under the subject `dsl: the dsl: 2 syntax profile …`, not
	// a conventional type: the notes of 3.141.0 do not list it, and the tag
	// range (v3.140.3..v3.141.0 carries 3000e7279) is what holds this pin.
	"parser.ProfileSince[2]": "",
	"parser.ImportSince":     "import",
	"parser.ContractSince":   "contract",
	// The alias pin's release notes carry this PR's own merge subject
	// (feat(claw): … tool aliases …); joined to the release test so the
	// number cannot rot the way 3.144.0 and 3.146.0 did (#1155, Rda4616).
	"bundle.ToolAliasesSince": "alias",
	// #1350 lands as `feat(dsl): a var declares its own constraint …`, so
	// the notes of its release carry the word the constraint is spelled
	// with.
	"parser.VarMatchingSince": "matching",
}

// ReleaseNotesMarker is the word the release notes of a floor's pinned
// release must carry — "" for an exempted floor — and whether the floor
// has an entry in the table at all.
func ReleaseNotesMarker(name string) (marker string, known bool) {
	marker, known = releaseNotesMarkers[name]
	return marker, known
}

// HoldSyntaxFloor is the two-arm rule that holds a syntax floor against a
// checkout — version its package.json, changelog its CHANGELOG.md, marker
// the floor's ReleaseNotesMarker. TestSyntaxFloorsNameReleasesThatExist
// runs it on every tree; the release cut (internal/floorsalign) runs it on
// the tree it is about to commit, so the two never disagree.
//
// Ahead of the checkout — the pin above version — the pin must be exactly
// the next minor above the newest release the changelog carries: at or
// below that release, a release overtook the pin, which then names a build
// that does not read the syntax; above the next minor, the pin refuses
// every build in between that does. On a pull request's merge ref the
// changelog is main's, so both turn red before the merge lands. At or
// below the checkout, the pinned release must exist in the changelog and
// its notes must carry the marker, so a pin never realigned at the release,
// or a number another feature took, is loud instead of naming a build no
// one checked.
func HoldSyntaxFloor(name, floor, version, changelog, marker string) error {
	c, ok := CompareVersions(version, floor)
	if !ok {
		return fmt.Errorf("%s = %q or package.json = %q is not an orderable version", name, floor, version)
	}
	newest := NewestChangelogRelease(changelog)
	if newest == "" {
		return fmt.Errorf("CHANGELOG.md carries no `## [x.y.z]` release heading")
	}
	if c < 0 {
		want, ok := NextMinor(newest)
		if !ok {
			return fmt.Errorf("the newest release in the changelog, %q, is not a release number", newest)
		}
		if floor != want {
			return fmt.Errorf("%s = %s is ahead of this checkout (%s) but is not the next minor above the newest release in the changelog (%s → %s): below it a release overtook the pin and it names a build that does not read the syntax, above it the pin refuses every build that does — re-pin to %s (on a branch behind main, merge main first: its changelog decides)", name, floor, version, newest, want, want)
		}
		return nil
	}
	section, ok := ChangelogSection(changelog, floor)
	if !ok {
		return fmt.Errorf("%s names %s, but no release %s was ever cut (package.json is %s): the pin was not realigned at the release — move it to the release that first reads the syntax", name, floor, floor, version)
	}
	if marker != "" && !strings.Contains(section, marker) {
		return &SilentNotesError{Name: name, Floor: floor, Marker: marker}
	}
	return nil
}

// SilentNotesError is the released arm's verdict on a pin whose release was
// cut without the syntax: the section exists, the word does not. It is the
// one arm a release can still repair at the cut, so the aligner tells it
// apart (errors.As) and answers it with the remedy instead of re-stating
// the verdict.
type SilentNotesError struct {
	Name   string // the registry key of the floor
	Floor  string // the release the pin names
	Marker string // the word those notes had to carry
}

func (e *SilentNotesError) Error() string {
	return fmt.Sprintf("%s names %s, whose release notes do not carry %q: that release was cut without the syntax (another feature took the number) — move the pin to the release that first reads it", e.Name, e.Floor, e.Marker)
}

// NextMinor is major.(minor+1).0 of a release number — the pin a lot writes
// while its release is uncut, and the one shape the release cut realigns.
func NextMinor(v string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return "", false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%s.%d.0", parts[0], minor+1), true
}

// ChangelogSection is the text of a release's section: from its heading to
// the next release heading, or the end.
func ChangelogSection(changelog, release string) (string, bool) {
	heading := "## [" + release + "]"
	i := strings.Index(changelog, heading)
	if i < 0 {
		return "", false
	}
	rest := changelog[i+len(heading):]
	if j := strings.Index(rest, "\n## ["); j >= 0 {
		rest = rest[:j]
	}
	return rest, true
}

var changelogReleaseRe = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`)

// NewestChangelogRelease is the highest release heading of a changelog, or
// "" when it carries none.
func NewestChangelogRelease(changelog string) string {
	return newestChangelogRelease(changelog, func(string) bool { return true })
}

// NewestChangelogReleaseBelow is the highest release heading strictly below
// version — the release that preceded it — or "" when the changelog carries
// none.
func NewestChangelogReleaseBelow(changelog, version string) string {
	return newestChangelogRelease(changelog, func(release string) bool {
		c, ok := CompareVersions(release, version)
		return ok && c < 0
	})
}

// newestChangelogRelease orders the admitted headings, not the lines: the
// changelog is prepended at each release, but nothing forces the file to
// be sorted.
func newestChangelogRelease(changelog string, admit func(string) bool) string {
	var newest string
	for _, m := range changelogReleaseRe.FindAllStringSubmatch(changelog, -1) {
		if !admit(m[1]) {
			continue
		}
		if newest == "" {
			newest = m[1]
			continue
		}
		if c, ok := CompareVersions(m[1], newest); ok && c > 0 {
			newest = m[1]
		}
	}
	return newest
}
