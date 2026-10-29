package bundle

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/floortest"
)

// A syntax floor is pinned to the release that will carry the syntax and
// held there by HoldSyntaxFloor's two exact arms, against this checkout's
// package.json and CHANGELOG.md — the same rule the release cut runs on
// the tree it is about to commit (internal/floorsalign), so a pin the cut
// let through is a pin this test holds.
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
		marker, known := ReleaseNotesMarker(name)
		if !known {
			t.Fatalf("%s has no entry in releaseNotesMarkers: name the word its release notes carry, or exempt it with its reason", name)
		}
		if err := HoldSyntaxFloor(name, floor, version, string(changelog), marker); err != nil {
			t.Fatal(err)
		}
		if warning := NumericSyntaxFloorWarning(name, floor, NewestChangelogRelease(string(changelog))); warning != "" {
			t.Log(warning)
		}
		t.Logf("%s = %s holds against package.json %s", name, floor, version)
	}
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
		"unorderable pin":                  {"3.149.0-rc1", "3.149.4", "import", "not an orderable version"},
	} {
		t.Run(name, func(t *testing.T) {
			err := HoldSyntaxFloor("pin", tc.floor, tc.version, log, tc.marker)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want an error mentioning %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// The "ahead" arm orders headings, not lines; the cut reads the release
// that preceded the one it makes from the same headings.
func TestNewestChangelogReleaseOrdersHeadings(t *testing.T) {
	log := "## [3.147.0](x)\n### Bug Fixes\n## [3.149.0](y)\n## [3.148.2](z)\n"
	if got := NewestChangelogRelease(log); got != "3.149.0" {
		t.Fatalf("newest = %q", got)
	}
	if NewestChangelogRelease("no headings") != "" {
		t.Fatal("a changelog without headings has a newest release")
	}
	if got := NewestChangelogReleaseBelow(log, "3.149.0"); got != "3.148.2" {
		t.Fatalf("newest below 3.149.0 = %q, want 3.148.2 (the highest heading under it, not the next line)", got)
	}
	if got := NewestChangelogReleaseBelow(log, "3.147.0"); got != "" {
		t.Fatalf("newest below the oldest heading = %q, want none", got)
	}
	if got, ok := NextMinor("3.149.4"); !ok || got != "3.150.0" {
		t.Fatalf("next minor of 3.149.4 = %q, %v", got, ok)
	}
	if got, ok := NextMinor("v3.179.0"); !ok || got != "3.180.0" {
		t.Fatalf("next minor of v3.179.0 = %q, %v", got, ok)
	}
	if _, ok := NextMinor("not-a-release"); ok {
		t.Fatal("NextMinor accepted a non-release")
	}
}

// Notes that do not name the syntax are the one verdict the release cut can
// still repair, and it answers them with a remedy of its own
// (internal/floorsalign), so the arm travels as a type and not as a
// sentence — and no other arm may be mistaken for it.
func TestSilentNotesTravelAsATypedVerdict(t *testing.T) {
	log := "## [3.149.0](b) (2026-09-15)\n### Features\n* **dsl:** import \"lib/x.bot\"\n"
	var silent *SilentNotesError
	err := HoldSyntaxFloor("parser.ContractSince", "3.149.0", "3.149.4", log, "contract")
	if !errors.As(err, &silent) {
		t.Fatalf("notes silent about the floor did not travel as *SilentNotesError: %v", err)
	}
	if silent.Name != "parser.ContractSince" || silent.Floor != "3.149.0" || silent.Marker != "contract" {
		t.Fatalf("the verdict lost its subject: %+v", silent)
	}
	for name, tc := range map[string]struct{ floor, version, marker string }{
		"never cut":       {"3.147.0", "3.149.4", "contract"},
		"ahead, over-pin": {"3.151.0", "3.149.4", "contract"},
		"unorderable":     {"3.149.0-rc1", "3.149.4", "contract"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := HoldSyntaxFloor("parser.ContractSince", tc.floor, tc.version, log, tc.marker); errors.As(err, &silent) {
				t.Fatalf("%s travelled as silent notes — the cut would answer it with the #1154 remedy: %v", name, err)
			}
		})
	}
}

// The release cut moves a floor CONSTANT (internal/floorsalign); a manifest
// that spells the release out does not follow it. Every shipped bundle's
// declared floor must therefore reach what its own sources need, so a cut
// that realigns a floor and leaves a manifest behind reddens here — where
// `iterion validate` only warns (C252 is a warning, exit 0) while the push
// admission refuses the same bundle with a 409.
func TestEveryShippedBundleDeclaresAFloorThatReachesWhatItNeeds(t *testing.T) {
	root := filepath.Join("..", "..")
	checked, keyFloors := 0, 0
	for _, collection := range []string{"bots", "examples"} {
		entries, err := os.ReadDir(filepath.Join(root, collection))
		if err != nil {
			t.Fatalf("%s: %v", collection, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(root, collection, entry.Name())
			path := filepath.Join(dir, "manifest.yaml")
			if _, err := os.Stat(path); err != nil {
				continue
			}
			checked++
			m, err := LoadManifest(path)
			if err != nil {
				t.Errorf("%s: %v", path, err)
				continue
			}
			// One predicate for both halves of what the bundle asks: the
			// sources it is written in AND the keys its own manifest
			// declares (the taxonomy floor), so the catalogue is held to
			// what `validate` C252 and the push admission read.
			if pf := CheckSyntaxFloor(m, MaxSyntaxRequirementsDir(dir)); !pf.OK {
				t.Errorf("%s declares %q, which does not reach %s, the release that reads %s: a floor constant moved and this manifest did not follow — raise it", path, pf.Declared, pf.Need, pf.Reason)
			}
			if SubjectOf(m, SyntaxRequirements{}).Asks() {
				keyFloors++
			}
		}
	}
	if checked < 30 {
		t.Fatalf("only %d shipped manifests were read — the walk lost a collection, and a green verdict would mean nothing", checked)
	}
	if keyFloors == 0 {
		t.Fatalf("not one of the %d shipped manifests asks for a floor through a MANIFEST KEY: the keys stopped being read (Manifest.DeclaredKeys), so the taxonomy floor above passed on nothing", checked)
	}
	t.Logf("%d shipped manifests declare a floor that reaches what they need, %d of them through a manifest key", checked, keyFloors)
}

func TestCatalogTaxonomyFloorFollowsTopLevelKeyPresence(t *testing.T) {
	check := func(t *testing.T, taxonomy, requirement string, wantErr bool) {
		t.Helper()
		body := "name: x\n" + taxonomy + requirement
		m, err := DecodeManifest([]byte(body), "test")
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		// The PRODUCT predicate, never a floor the test holds alone: what
		// the catalogue owes is what `validate` (C252), the push admission
		// and the scaffold read off the one floors table.
		pf := CheckSyntaxFloor(m, SyntaxRequirements{})
		if pf.OK == wantErr {
			t.Fatalf("the taxonomy floor passes = %v, want a refusal = %v (need %q, declared %q, uses %q)\nmanifest:\n%s", pf.OK, wantErr, pf.Need, pf.Declared, pf.Uses, body)
		}
		if !wantErr {
			return
		}
		if pf.Need != CatalogTaxonomySince || pf.Reason != "top-level `category`/`tags`" || pf.Uses != pf.Reason {
			t.Fatalf("the refusal does not name the keys and the release that reads them: %+v", pf)
		}
		if !strings.Contains(pf.Consequence, "refuses the whole document") {
			t.Fatalf("the refusal does not say what a build below the floor does with the manifest: %q", pf.Consequence)
		}
	}

	taxonomies := map[string]string{
		"category only": "category: verify\n",
		"tags only":     "tags: [read-only]\n",
		"both":          "category: verify\ntags: [read-only]\n",
		"root merge":    "compat: &taxonomy\n  category: verify\n  tags: [read-only]\n<<: *taxonomy\n",
		"neither":       "",
	}
	floors := map[string]struct {
		yaml                string
		wantErrWithTaxonomy bool
	}{
		"absent": {"", true},
		"lower":  {"requires:\n  iterion: \">= " + floortest.Below(t, CatalogTaxonomySince) + "\"\n", true},
		"equal":  {"requires:\n  iterion: \">= " + CatalogTaxonomySince + "\"\n", false},
		"higher": {"requires:\n  iterion: \">= 9.999.0\"\n", false},
	}
	for taxonomyName, taxonomy := range taxonomies {
		for floorName, floor := range floors {
			t.Run(taxonomyName+"/"+floorName, func(t *testing.T) {
				check(t, taxonomy, floor.yaml, taxonomy != "" && floor.wantErrWithTaxonomy)
			})
		}
	}

	for name, taxonomy := range map[string]string{ // a key declared with no value is still declared
		"empty category":                            "category:\n",
		"empty tags":                                "tags:\n",
		"empty category and tags":                   "category:\ntags:\n",
		"null category and tags root merge":         "compat: &taxonomy\n  category:\n  tags:\n<<: *taxonomy\n",
		"empty-valued category and tags root merge": "compat: &taxonomy\n  category: \"\"\n  tags: []\n<<: *taxonomy\n",
	} {
		t.Run(name, func(t *testing.T) {
			check(t, taxonomy, "requires:\n  iterion: \">= "+floortest.Below(t, CatalogTaxonomySince)+"\"\n", true)
		})
	}
}
