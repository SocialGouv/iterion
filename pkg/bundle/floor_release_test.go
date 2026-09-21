package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// releaseNotesMarkers names, per syntax floor, a word the release notes of
// the pinned release carry — the changelog is generated from the merged
// commits' subjects — so a pin that names a release cut without the syntax
// (another feature took the number first) is loud. An empty marker is an
// exemption, with its reason beside it; a floor missing from this table
// fails the test: the next syntax declares its word.
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
}

// A syntax floor is pinned to the release that will carry the syntax and
// held there by two exact arms. Ahead of this checkout — the pin above
// package.json — it must be exactly the next minor above the newest release
// the changelog carries: at or below that release, a release overtook the
// pin, which then names a build that does not read the syntax; above the
// next minor, the pin refuses every build in between that does. On a pull
// request's merge ref the changelog is main's, so both turn red before the
// merge lands. At or below the checkout, the pinned release must exist in
// the changelog and its notes must carry the syntax's word
// (releaseNotesMarkers), so a pin never realigned at the release, or a
// number another feature took, is loud instead of naming a build no one
// checked.
func TestSyntaxFloorsNameReleasesThatExist(t *testing.T) {
	root := filepath.Join("..", "..")
	pkg, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`"version":\s*"([^"]+)"`).FindSubmatch(pkg)
	if m == nil {
		t.Fatal("package.json carries no version")
	}
	version := string(m[1])
	changelog, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	for name, floor := range SyntaxFloors() {
		marker, known := releaseNotesMarkers[name]
		if !known {
			t.Fatalf("%s has no entry in releaseNotesMarkers: name the word its release notes carry, or exempt it with its reason", name)
		}
		if err := holdSyntaxFloor(name, floor, version, string(changelog), marker); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s = %s holds against package.json %s", name, floor, version)
	}
}

// holdSyntaxFloor is the two-arm rule of TestSyntaxFloorsNameReleasesThatExist.
func holdSyntaxFloor(name, floor, version, changelog, marker string) error {
	c, ok := CompareVersions(version, floor)
	if !ok {
		return fmt.Errorf("%s = %q or package.json = %q is not an orderable version", name, floor, version)
	}
	newest := newestChangelogRelease(changelog)
	if newest == "" {
		return fmt.Errorf("CHANGELOG.md carries no `## [x.y.z]` release heading")
	}
	if c < 0 {
		want, ok := nextMinor(newest)
		if !ok {
			return fmt.Errorf("the newest release in the changelog, %q, is not a release number", newest)
		}
		if floor != want {
			return fmt.Errorf("%s = %s is ahead of this checkout (%s) but is not the next minor above the newest release in the changelog (%s → %s): below it a release overtook the pin and it names a build that does not read the syntax, above it the pin refuses every build that does — re-pin to %s (on a branch behind main, merge main first: its changelog decides)", name, floor, version, newest, want, want)
		}
		return nil
	}
	section, ok := changelogSection(changelog, floor)
	if !ok {
		return fmt.Errorf("%s names %s, but no release %s was ever cut (package.json is %s): the pin was not realigned at the release — move it to the release that first reads the syntax", name, floor, floor, version)
	}
	if marker != "" && !strings.Contains(section, marker) {
		return fmt.Errorf("%s names %s, whose release notes do not carry %q: that release was cut without the syntax (another feature took the number) — move the pin to the release that first reads it", name, floor, marker)
	}
	return nil
}

// nextMinor is major.(minor+1).0 of a release number.
func nextMinor(v string) (string, bool) {
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

// changelogSection is the text of a release's section: from its heading to
// the next release heading, or the end.
func changelogSection(changelog, release string) (string, bool) {
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

// newestChangelogRelease is the highest release heading of a changelog, or
// "" when it carries none.
func newestChangelogRelease(changelog string) string {
	var newest string
	for _, m := range changelogReleaseRe.FindAllStringSubmatch(changelog, -1) {
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

// The two arms bite both ways: ahead, only the next minor passes — an
// over-pin and an overtaken pin are refused; released, the section must
// exist and carry the syntax's word, unless the floor is exempted.
func TestHoldSyntaxFloorBitesBothWays(t *testing.T) {
	log := "## [3.149.4](a) (2026-09-15)\n### Bug Fixes\n* **runner:** a fix\n## [3.149.0](b) (2026-09-15)\n### Features\n* **dsl:** a bot in several files — import \"lib/x.bot\" read as one unit\n## [3.148.0](c) (2026-09-15)\n### Features\n* **bots:** something else entirely\n"
	for name, tc := range map[string]struct {
		floor, version, marker, wantErr string
	}{
		"ahead, exactly the next minor":    {"3.150.0", "3.149.4", "contract", ""},
		"ahead, over-pinned by one":        {"3.151.0", "3.149.4", "contract", "not the next minor"},
		"ahead, over-pinned by many":       {"4.0.0", "3.149.4", "contract", "not the next minor"},
		"ahead of package.json, overtaken": {"3.149.4", "3.149.3", "contract", "not the next minor"},
		"released, notes carry the word":   {"3.149.0", "3.149.4", "import", ""},
		"released, notes silent":           {"3.148.0", "3.149.4", "import", "do not carry"},
		"released, exempted":               {"3.148.0", "3.149.4", "", ""},
		"never cut":                        {"3.147.0", "3.149.4", "x", "was ever cut"},
	} {
		t.Run(name, func(t *testing.T) {
			err := holdSyntaxFloor("pin", tc.floor, tc.version, log, tc.marker)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want an error mentioning %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// The "ahead" arm orders headings, not lines.
func TestNewestChangelogReleaseOrdersHeadings(t *testing.T) {
	log := "## [3.147.0](x)\n### Bug Fixes\n## [3.149.0](y)\n## [3.148.2](z)\n"
	if got := newestChangelogRelease(log); got != "3.149.0" {
		t.Fatalf("newest = %q", got)
	}
	if newestChangelogRelease("no headings") != "" {
		t.Fatal("a changelog without headings has a newest release")
	}
	if got, ok := nextMinor("3.149.4"); !ok || got != "3.150.0" {
		t.Fatalf("next minor of 3.149.4 = %q, %v", got, ok)
	}
}
