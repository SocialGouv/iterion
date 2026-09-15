package bundle

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A syntax floor is pinned to the next release when the syntax ships, and
// realigned at the release if the number differs. Two arms hold it. Ahead
// of this checkout, the pin must be strictly above the newest release the
// changelog carries — on a pull request's merge ref that is main's
// changelog, so a release cut before the merge, which the pin would then
// name without reading the syntax, turns the check red before the merge
// lands. At or below the checkout, the pinned release must exist in the
// changelog, so a pin that was never realigned to the release that carried
// it is loud instead of naming a version no build ever had.
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
	newest := newestChangelogRelease(string(changelog))
	if newest == "" {
		t.Fatal("CHANGELOG.md carries no `## [x.y.z]` release heading")
	}
	for name, floor := range SyntaxFloors() {
		c, ok := CompareVersions(version, floor)
		if !ok {
			t.Fatalf("%s = %q or package.json = %q is not an orderable version", name, floor, version)
		}
		if c < 0 {
			if above, ok := CompareVersions(floor, newest); !ok || above <= 0 {
				t.Fatalf("%s = %s is ahead of this checkout (%s) but not above the newest release in the changelog (%s): a release overtook the pin, and the pin names a build that does not read the syntax — re-pin to the next minor above %s", name, floor, version, newest, newest)
			}
			t.Logf("%s = %s is ahead of this checkout (%s) and above the newest release (%s): the release is still to come", name, floor, version, newest)
			continue
		}
		if !strings.Contains(string(changelog), "## ["+floor+"]") {
			t.Fatalf("%s names %s, but no release %s was ever cut (package.json is %s): the pin was not realigned at the release — move it to the release that first reads the syntax", name, floor, floor, version)
		}
	}
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

// The "ahead" arm bites: a pin at the newest release of the changelog, or
// below it, is refused; one above passes.
func TestNewestChangelogReleaseOrdersHeadings(t *testing.T) {
	log := "## [3.147.0](x)\n### Bug Fixes\n## [3.149.0](y)\n## [3.148.2](z)\n"
	if got := newestChangelogRelease(log); got != "3.149.0" {
		t.Fatalf("newest = %q", got)
	}
	if newestChangelogRelease("no headings") != "" {
		t.Fatal("a changelog without headings has a newest release")
	}
}
