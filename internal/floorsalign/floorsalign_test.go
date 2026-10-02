package floorsalign

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// cutChangelog is what release-it has rendered when the hook runs a patch
// cut of 3.179.1 after 3.179.0: the cut's own section on top. The notes
// name "contract" (the cut ships it), the previous release names "alias",
// 3.178.0 names "import", and 3.141.0 — the profile-2 exemption's release
// — lists something else entirely.
const cutChangelog = "# Changelog\n\n" +
	"## [3.179.1](x) (2026-09-22)\n\n### Bug Fixes\n\n* **dsl:** the contract gallery template, read at last\n\n" +
	"## [3.179.0](y) (2026-09-21)\n\n### Features\n\n* **claw:** the tool alias floor\n\n" +
	"## [3.178.0](z) (2026-09-20)\n\n### Features\n\n* **dsl:** import \"lib/x.bot\"\n\n" +
	"## [3.141.0](w) (2026-09-13)\n\n### Features\n\n* **bots:** something else entirely\n"

// The registry's own names go through Plan, so the real marker table is
// what decides — a fixture with invented names would exercise a stub of it.
func TestPlanDecidesPerShape(t *testing.T) {
	majorCut := "# Changelog\n\n## [4.0.0](m) (2026-09-22)\n\n### Features\n\n* **dsl:** the contract, rebuilt\n\n" + strings.TrimPrefix(cutChangelog, "# Changelog\n\n")
	afterMinor := "# Changelog\n\n## [3.180.0](n) (2026-09-22)\n\n### Features\n\n* **dsl:** the contract syntax\n\n" + strings.TrimPrefix(cutChangelog, "# Changelog\n\n")
	for name, tc := range map[string]struct {
		pin, value, version, changelog string
		cutting                        bool
		wantKind                       Kind
		wantTo, wantDetail             string
	}{
		"at the previous release holds":                             {"bundle.ToolAliasesSince", "3.179.0", "3.179.1", cutChangelog, true, Holds, "", "is released"},
		"below the previous release holds":                          {"parser.ImportSince", "3.178.0", "3.179.1", cutChangelog, true, Holds, "", "is released"},
		"exempted and released holds":                               {"parser.ProfileSince[2]", "3.141.0", "3.179.1", cutChangelog, true, Holds, "", "is released"},
		"names the cut, whose notes carry the word":                 {"parser.ContractSince", "3.179.1", "3.179.1", cutChangelog, true, Holds, "", "names the release being cut"},
		"names the cut, whose notes are silent":                     {"parser.ImportSince", "3.179.1", "3.179.1", cutChangelog, true, Refuse, "", "do not carry"},
		"the cut claims the next minor":                             {"parser.ContractSince", "3.180.0", "3.179.1", cutChangelog, true, Realign, "3.179.1", "3.180.0 → 3.179.1"},
		"the cut claims the next minor, but the notes are silent":   {"parser.ImportSince", "3.180.0", "3.179.1", cutChangelog, true, Refuse, "", "releaseNotesMarkers"},
		"the cut claims the next minor of an exempted floor":        {"parser.ProfileSince[2]", "3.180.0", "3.179.1", cutChangelog, true, Realign, "3.179.1", "3.180.0 → 3.179.1"},
		"a major cut claims the pin":                                {"parser.ContractSince", "3.180.0", "4.0.0", majorCut, true, Realign, "4.0.0", "3.180.0 → 4.0.0"},
		"two minors ahead refuses":                                  {"parser.ContractSince", "3.181.0", "3.179.1", cutChangelog, true, Refuse, "", "not the next minor"},
		"between the previous release and the cut refuses":          {"parser.ContractSince", "3.179.5", "3.179.1", cutChangelog, true, Refuse, "", "not the next minor"},
		"a number never cut refuses":                                {"parser.ContractSince", "3.178.5", "3.179.1", cutChangelog, true, Refuse, "", "was ever cut"},
		"an unorderable pin refuses":                                {"parser.ContractSince", "3.179.0-rc1", "3.179.1", cutChangelog, true, Refuse, "", "not an orderable version"},
		"a floor without a marker entry refuses":                    {"parser.NobodySince", "3.180.0", "3.179.1", cutChangelog, true, Refuse, "", "releaseNotesMarkers"},
		"preview: ahead at the next minor is pending":               {"parser.ContractSince", "3.180.0", "3.179.1", cutChangelog, false, Pending, "", "the next cut realigns it"},
		"preview: two minors ahead is what the release test says":   {"parser.ContractSince", "3.181.0", "3.179.1", cutChangelog, false, Refuse, "", "not the next minor"},
		"preview: released holds":                                   {"bundle.ToolAliasesSince", "3.179.0", "3.179.1", cutChangelog, false, Holds, "", "is released"},
		"preview: released with silent notes is what the test says": {"parser.ImportSince", "3.179.1", "3.179.1", cutChangelog, false, Refuse, "", "do not carry"},
		// After a minor cut the previous release is one minor down; the
		// release test measures the next minor from the NEWEST release, and
		// so must the preview, or a pending pin main holds would be refused.
		"preview after a minor cut holds what the release test holds": {"parser.ContractSince", "3.181.0", "3.180.0", afterMinor, false, Pending, "", "the next cut realigns it"},
	} {
		t.Run(name, func(t *testing.T) {
			_, ident, index, err := ParsePinName(tc.pin)
			if err != nil {
				t.Fatal(err)
			}
			pins := []Pin{{Name: tc.pin, Ident: ident, Index: index, Value: tc.value, File: "x.go"}}
			actions := Plan(pins, tc.version, tc.changelog, tc.cutting)
			if len(actions) != 1 {
				t.Fatalf("Plan returned %d actions, want 1: %+v", len(actions), actions)
			}
			a := actions[0]
			if a.Kind != tc.wantKind {
				t.Fatalf("kind = %v, want %v (%s)", a.Kind, tc.wantKind, a.Detail)
			}
			if a.To != tc.wantTo {
				t.Fatalf("To = %q, want %q", a.To, tc.wantTo)
			}
			if !strings.Contains(a.Detail, tc.wantDetail) {
				t.Fatalf("detail %q does not carry %q", a.Detail, tc.wantDetail)
			}
		})
	}
}

// A cut whose section is not in the changelog yet is a hook wired in the
// wrong slot: one refusal naming the slot, and no pin is judged on notes
// that do not exist.
func TestPlanRefusesACutWhoseSectionIsNotRendered(t *testing.T) {
	notYet := strings.Replace(cutChangelog, "## [3.179.1](x) (2026-09-22)\n\n### Bug Fixes\n\n* **dsl:** the contract gallery template, read at last\n\n", "", 1)
	pins := []Pin{{Name: "parser.ContractSince", Ident: "ContractSince", Index: -1, Value: "3.180.0", File: "x.go"}}
	actions := Plan(pins, "3.179.1", notYet, true)
	if len(actions) != 1 || actions[0].Kind != Refuse {
		t.Fatalf("Plan = %+v, want one refusal", actions)
	}
	if !strings.Contains(actions[0].Detail, "before:git:beforeRelease") {
		t.Fatalf("the refusal does not name the hook slot: %s", actions[0].Detail)
	}
	if actions := Plan(pins, "3.179.1", notYet, false); actions[0].Kind != Pending {
		t.Fatalf("a preview on a tree whose newest release is the checkout = %v, want pending: %s", actions[0].Kind, actions[0].Detail)
	}
}

// A refusal the release cannot repair by re-wording a subject keeps the
// release test's own verdict: only notes silent about the floor (#1154) get
// the marker-table remedy. Reached here by a changelog whose heading is not
// line-anchored — the cut's section is found as a substring, but the rule
// that orders releases sees no heading at all.
func TestACutRefusalThatIsNotSilentNotesKeepsTheReleaseTestsVerdict(t *testing.T) {
	mangled := "a line mentioning ## [3.179.1] without starting one\n"
	pins := []Pin{{Name: "parser.ContractSince", Ident: "ContractSince", Index: -1, Value: "3.179.1", File: "x.go"}}
	actions := Plan(pins, "3.179.1", mangled, true)
	if len(actions) != 1 || actions[0].Kind != Refuse {
		t.Fatalf("Plan = %+v, want one refusal", actions)
	}
	if !strings.Contains(actions[0].Detail, "carries no `## [x.y.z]` release heading") {
		t.Fatalf("the refusal lost the release test's verdict: %s", actions[0].Detail)
	}
	if strings.Contains(actions[0].Detail, "#1154") || strings.Contains(actions[0].Detail, "releaseNotesMarkers") {
		t.Fatalf("a refusal that is not silent notes was dressed as one: %s", actions[0].Detail)
	}
}

func TestApplyPinMovesOnlyTheLiteral(t *testing.T) {
	src := []byte("package parser\n\nconst FixtureSince = \"3.180.0\"\n\n// after\n")
	out, err := ApplyPin(src, Pin{Name: "parser.FixtureSince", Ident: "FixtureSince", Index: -1, File: "x.go"}, "3.179.1")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "package parser\n\nconst FixtureSince = \"3.179.1\"\n\n// after\n" {
		t.Fatalf("spliced %q", out)
	}
}

// The decoy key sits BEFORE the target and contains it as a prefix: a
// rewrite that reads the map as text lands on 12 and leaves the pin.
func TestApplyPinRewritesOnlyTheIndexedMapEntry(t *testing.T) {
	src := []byte("package parser\n\nvar FixtureSince = map[int]string{\n\t12: \"3.190.0\",\n\t2: \"3.141.0\",\n\t1: \"3.100.0\",\n}\n")
	out, err := ApplyPin(src, Pin{Name: "parser.FixtureSince[2]", Ident: "FixtureSince", Index: 2, File: "x.go"}, "3.179.1")
	if err != nil {
		t.Fatal(err)
	}
	want := "package parser\n\nvar FixtureSince = map[int]string{\n\t12: \"3.190.0\",\n\t2: \"3.179.1\",\n\t1: \"3.100.0\",\n}\n"
	if string(out) != want {
		t.Fatalf("spliced %q, want %q", out, want)
	}
}

// A spec that binds several names binds several values: the pin's own
// position decides, or the rewrite lands on its neighbour's literal.
func TestApplyPinRewritesTheNameItWasAskedFor(t *testing.T) {
	src := []byte("package parser\n\nconst Other, FixtureSince = \"1.2.3\", \"3.180.0\"\n")
	out, err := ApplyPin(src, Pin{Name: "parser.FixtureSince", Ident: "FixtureSince", Index: -1, File: "x.go"}, "3.179.1")
	if err != nil {
		t.Fatal(err)
	}
	want := "package parser\n\nconst Other, FixtureSince = \"1.2.3\", \"3.179.1\"\n"
	if string(out) != want {
		t.Fatalf("spliced %q, want %q", out, want)
	}
}

// A comment inside the literal spelling an entry is a comment: the pin is
// what the declaration binds, not what the file says.
func TestApplyPinDoesNotWriteIntoACommentOrAString(t *testing.T) {
	src := []byte("package parser\n\n// FixtureSince = \"3.150.0\" was the floor before the reader moved.\nvar FixtureSince = map[int]string{\n\t// 2: \"3.141.0\" shipped in the profile lot.\n\t2: \"3.180.0\",\n}\n\nconst Note = \"FixtureSince = \\\"3.111.0\\\"\"\n")
	out, err := ApplyPin(src, Pin{Name: "parser.FixtureSince[2]", Ident: "FixtureSince", Index: 2, File: "x.go"}, "3.179.1")
	if err != nil {
		t.Fatal(err)
	}
	want := "package parser\n\n// FixtureSince = \"3.150.0\" was the floor before the reader moved.\nvar FixtureSince = map[int]string{\n\t// 2: \"3.141.0\" shipped in the profile lot.\n\t2: \"3.179.1\",\n}\n\nconst Note = \"FixtureSince = \\\"3.111.0\\\"\"\n"
	if string(out) != want {
		t.Fatalf("spliced %q,\nwant %q", out, want)
	}
}

// A pin the registry names but the file does not bind exactly once, or
// binds to something other than a release literal, is an error: writing it
// would land in a declaration nobody checked.
func TestApplyPinRefusesWhatItCannotLocate(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"declared twice": {
			"package x\n\nconst FixtureSince = \"3.180.0\"\nconst Other = \"3.180.0\"\nvar FixtureSince = \"3.180.0\"\n",
			"declared 2 times",
		},
		"declared nowhere": {"package x\n", "declared 0 times"},
		"not a literal": {
			"package x\n\nconst prefix = \"3.180\"\nconst FixtureSince = prefix + \".0\"\n",
			"never an expression",
		},
		"not a release": {
			"package x\n\nconst FixtureSince = \"edge\"\n",
			"is not a release number",
		},
		"no value": {
			"package x\n\nvar FixtureSince string\n",
			"has no value to rewrite",
		},
		"unparseable": {"package x\n\nconst FixtureSince = \n", "parsing"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ApplyPin([]byte(tc.src), Pin{Name: "x.FixtureSince", Ident: "FixtureSince", Index: -1, File: "x.go"}, "3.179.1")
			if err == nil {
				t.Fatalf("%s applied anyway", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not say %q: %v", tc.want, err)
			}
		})
	}
	dup := []byte("package x\n\nvar FixtureSince = map[int]string{\n\t2: \"3.180.0\",\n\t2: \"3.181.0\",\n}\n")
	_, err := ApplyPin(dup, Pin{Name: "x.FixtureSince[2]", Ident: "FixtureSince", Index: 2, File: "x.go"}, "3.179.1")
	if err == nil || !strings.Contains(err.Error(), "appears 2 times") {
		t.Fatalf("a map key bound twice applied anyway: %v", err)
	}
}

func TestParsePinName(t *testing.T) {
	if _, ident, index, err := ParsePinName("parser.ContractSince"); err != nil || ident != "ContractSince" || index != -1 {
		t.Fatalf("scalar: %q %d %v", ident, index, err)
	}
	if _, ident, index, err := ParsePinName("parser.ProfileSince[2]"); err != nil || ident != "ProfileSince" || index != 2 {
		t.Fatalf("map: %q %d %v", ident, index, err)
	}
	if _, _, _, err := ParsePinName("ContractSince"); err == nil {
		t.Fatal("a name without a package prefix parsed")
	}
}

// ResolvePins follows the declarations, so the aligner carries no file list:
// a floor that moves files is followed, a registry entry that declares
// nowhere is loud.
func TestResolvePinsFollowsTheDeclarations(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/bundle/floors.go", "package bundle\n\nconst FixtureSince = \"3.180.0\"\n")
	write("pkg/other/unrelated.go", "package other\n\nconst FixtureSinceOther = \"3.0.0\"\n")

	pins, err := ResolvePins(root, map[string]string{"bundle.FixtureSince": "3.180.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].File != filepath.Join("pkg", "bundle", "floors.go") {
		t.Fatalf("resolved %+v", pins)
	}

	if _, err := ResolvePins(root, map[string]string{"bundle.MissingSince": "3.180.0"}); err == nil {
		t.Fatal("a pin that declares nowhere resolved silently")
	}

	write("pkg/bundle/twin.go", "package bundle\n\nconst FixtureSince2 = \"3.0.0\"\nconst FixtureSince = \"3.181.0\"\n")
	if _, err := ResolvePins(root, map[string]string{"bundle.FixtureSince": "3.180.0"}); err == nil {
		t.Fatal("a pin declared twice resolved to one file")
	}
}

// A declaration is what the file BINDS: a comment or a string quoting the
// pin declares nothing, and a `const (…)` block declares as surely as a
// one-line const. Each of these reads the same to a source-text match and
// differently to the parser, and each would send the rewrite at the wrong
// file — or at none.
func TestResolvePinsReadsDeclarationsNotText(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/bundle/floors.go", "package bundle\n\nconst (\n\t// FixtureSince pins the fixture syntax.\n\tFixtureSince = \"3.180.0\"\n)\n")
	write("pkg/decoy/comment.go", "package decoy\n\n// const FixtureSince = \"3.180.0\" moved to pkg/bundle.\nconst Unrelated = \"3.0.0\"\n")
	write("pkg/decoy/text.go", "package decoy\n\nvar Hint = \"var FixtureSince = the floor\"\n")
	write("pkg/decoy/prefix.go", "package decoy\n\nconst FixtureSinceOther = \"3.0.0\"\n")
	write("pkg/decoy/local.go", "package decoy\n\nfunc f() string {\n\tconst FixtureSince = \"3.0.0\"\n\treturn FixtureSince\n}\n")

	pins, err := ResolvePins(root, map[string]string{"bundle.FixtureSince": "3.180.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].File != filepath.Join("pkg", "bundle", "floors.go") {
		t.Fatalf("resolved %+v, want the const block in pkg/bundle/floors.go alone", pins)
	}
}

// The real tree keeps the resolver honest: every registry pin resolves to
// the one file that declares it, its value is a release number, and the
// marker table knows it.
func TestTheRealRegistryResolvesAgainstThisRepo(t *testing.T) {
	floors := bundle.SyntaxFloors()
	if len(floors) < 4 {
		t.Fatalf("the registry carries %d pins — the four syntax floors at least", len(floors))
	}
	pins, err := ResolvePins(filepath.Join("..", ".."), floors)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != len(floors) {
		t.Fatalf("resolved %d pins of %d registry entries", len(pins), len(floors))
	}
	for _, p := range pins {
		if _, ok := bundle.CompareVersions(p.Value, p.Value); !ok {
			t.Errorf("%s = %q is not an orderable version", p.Name, p.Value)
		}
		if _, err := os.Stat(filepath.Join("..", "..", p.File)); err != nil {
			t.Errorf("%s: %v", p.Name, err)
		}
		if _, known := bundle.ReleaseNotesMarker(p.Name); !known {
			t.Errorf("%s has no releaseNotesMarkers entry: the cut would refuse every release", p.Name)
		}
	}
}

// The wiring a release runs is itself under test: the hook line dropping
// out of .release-it.mjs — or moving to a slot where the changelog is not
// rendered yet — would leave the aligner green and never run, or refusing
// every cut.
func TestTheReleaseHookWiresTheAligner(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", ".release-it.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "'before:git:beforeRelease': ['go run ./cmd/release-floors --apply']") {
		t.Fatal(".release-it.mjs no longer runs `go run ./cmd/release-floors --apply` from before:git:beforeRelease — the pins realign by hand again, or the aligner judges notes that are not rendered yet")
	}
}
