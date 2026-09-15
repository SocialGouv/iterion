package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A syntax floor is pinned to the next release when the syntax ships, and
// realigned at the release if the number differs. This is what makes a
// missed realignment loud: once the repository's version has reached the
// pinned release, that release must exist in the changelog — otherwise the
// floor names a version no build ever carried, and every bundle held to it
// asks for an engine that cannot exist.
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
	floors := map[string]string{"parser.ImportSince": parser.ImportSince}
	for profile, since := range parser.ProfileSince {
		floors[fmt.Sprintf("parser.ProfileSince[%d]", profile)] = since
	}
	for name, floor := range floors {
		c, ok := CompareVersions(version, floor)
		if !ok {
			t.Fatalf("%s = %q or package.json = %q is not an orderable version", name, floor, version)
		}
		if c < 0 {
			t.Logf("%s = %s is ahead of this checkout (%s): the release is still to come", name, floor, version)
			continue
		}
		if !strings.Contains(string(changelog), "## ["+floor+"]") {
			t.Fatalf("%s names %s, but no release %s was ever cut (package.json is %s): the pin was not realigned at the release — move it to the release that first reads the syntax", name, floor, floor, version)
		}
	}
}
