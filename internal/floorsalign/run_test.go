package floorsalign

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// seededChangelog is the changelog of the fixture's committed release,
// 3.179.0: every released pin's section carries its word.
const seededChangelog = "# Changelog\n\n" +
	"## [3.179.0](y) (2026-09-21)\n\n### Features\n\n* **claw:** the tool alias floor\n* **dsl:** the contract syntax\n\n" +
	"## [3.178.0](z) (2026-09-20)\n\n### Features\n\n* **dsl:** import \"lib/x.bot\"\n\n" +
	"## [3.141.0](w) (2026-09-13)\n\n### Features\n\n* **bots:** something else entirely\n"

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixtureRepo is a repository the test owns, committed at release 3.179.0:
// package.json, the seeded changelog, and the floor files declaring the
// registry's own identifiers at the given pins.
func fixtureRepo(t *testing.T, contract, profile2, imp, alias string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "package.json", `{"name":"fixture","private":true,"version":"3.179.0"}`)
	writeFile(t, root, "CHANGELOG.md", seededChangelog)
	writeFile(t, root, "pkg/dsl/parser/floors.go", "package parser\n\n// ContractSince pins the contract syntax.\nconst ContractSince = \""+contract+"\"\n")
	writeFile(t, root, "pkg/dsl/parser/preamble.go", "package parser\n\nvar ProfileSince = map[int]string{2: \""+profile2+"\"}\n\nconst ImportSince = \""+imp+"\"\n")
	writeFile(t, root, "pkg/bundle/tool_aliases.go", "package bundle\n\nconst ToolAliasesSince = \""+alias+"\"\n")
	gittest.Run(t, root, "init", "-q")
	gittest.Run(t, root, "add", ".")
	gittest.Run(t, root, "commit", "-q", "-m", "chore: seed the fixture")
	return root
}

func fixtureFloors(contract, profile2, imp, alias string) func() map[string]string {
	return func() map[string]string {
		return map[string]string{
			"parser.ContractSince":    contract,
			"parser.ProfileSince[2]":  profile2,
			"parser.ImportSince":      imp,
			"bundle.ToolAliasesSince": alias,
		}
	}
}

// bump is the tree release-it hands the hook: package.json at the cut, the
// cut's section rendered on top of the changelog, nothing committed.
func bump(t *testing.T, root, cut, notes string) {
	t.Helper()
	writeFile(t, root, "package.json", `{"name":"fixture","private":true,"version":"`+cut+`"}`)
	writeFile(t, root, "CHANGELOG.md", "# Changelog\n\n## ["+cut+"](x) (2026-09-22)\n\n"+notes+"\n\n"+strings.TrimPrefix(seededChangelog, "# Changelog\n\n"))
}

func floorFiles(root string) []string {
	return []string{
		filepath.Join(root, "pkg", "dsl", "parser", "floors.go"),
		filepath.Join(root, "pkg", "dsl", "parser", "preamble.go"),
		filepath.Join(root, "pkg", "bundle", "tool_aliases.go"),
	}
}

func snapshot(t *testing.T, paths []string) [][]byte {
	t.Helper()
	out := make([][]byte, len(paths))
	for i, p := range paths {
		out[i] = readFile(t, p)
	}
	return out
}

func requireUntouched(t *testing.T, paths []string, before [][]byte) {
	t.Helper()
	for i, p := range paths {
		if after := readFile(t, p); !bytes.Equal(before[i], after) {
			t.Fatalf("%s was written:\n got %q\nwant %q", p, after, before[i])
		}
	}
}

// A patch cut that ships two floors pinned at the next minor: both pins
// are rewritten to the cut, byte for byte, and the two released ones stay.
func TestTheCutRealignsThePinsItClaims(t *testing.T) {
	root := fixtureRepo(t, "3.180.0", "3.180.0", "3.178.0", "3.179.0")
	aliasBefore := readFile(t, floorFiles(root)[2])
	bump(t, root, "3.179.1", "### Bug Fixes\n\n* **dsl:** the contract gallery template, read at last")
	var out bytes.Buffer
	if err := Run(Options{Root: root, Apply: true, Floors: fixtureFloors("3.180.0", "3.180.0", "3.178.0", "3.179.0"), Stdout: &out}); err != nil {
		t.Fatalf("the cut refused: %v", err)
	}
	if got, want := string(readFile(t, floorFiles(root)[0])), "package parser\n\n// ContractSince pins the contract syntax.\nconst ContractSince = \"3.179.1\"\n"; got != want {
		t.Fatalf("scalar pin not realigned byte-exact:\n got %q\nwant %q", got, want)
	}
	if got, want := string(readFile(t, floorFiles(root)[1])), "package parser\n\nvar ProfileSince = map[int]string{2: \"3.179.1\"}\n\nconst ImportSince = \"3.178.0\"\n"; got != want {
		t.Fatalf("map pin not realigned byte-exact, or the released pin beside it moved:\n got %q\nwant %q", got, want)
	}
	if after := readFile(t, floorFiles(root)[2]); !bytes.Equal(aliasBefore, after) {
		t.Fatalf("the released alias pin was rewritten: %q", after)
	}
	for _, line := range []string{"cutting 3.179.1", "parser.ContractSince = 3.180.0 → 3.179.1", "parser.ProfileSince[2] = 3.180.0 → 3.179.1", "parser.ImportSince = 3.178.0 is released", "bundle.ToolAliasesSince = 3.179.0 is released"} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("the report lacks %q:\n%s", line, out.String())
		}
	}
}

// Two pins in the SAME file both claimed by one cut: both move. The file is
// read once per pin, so a cache over the loop would write the first
// rewrite, drop the second, and leave a floor one minor above the build
// that first reads it — a shape the release test's ahead arm accepts, which
// is the whole defect #1287 removes.
func TestTheCutRealignsTwoPinsDeclaredInOneFile(t *testing.T) {
	root := fixtureRepo(t, "3.179.0", "3.180.0", "3.180.0", "3.179.0")
	floors := fixtureFloors("3.179.0", "3.180.0", "3.180.0", "3.179.0")
	bump(t, root, "3.179.1", "### Features\n\n* **dsl:** import \"lib/x.bot\", at last")
	if err := Run(Options{Root: root, Apply: true, Floors: floors}); err != nil {
		t.Fatalf("the cut refused: %v", err)
	}
	got, want := string(readFile(t, floorFiles(root)[1])), "package parser\n\nvar ProfileSince = map[int]string{2: \"3.179.1\"}\n\nconst ImportSince = \"3.179.1\"\n"
	if got != want {
		t.Fatalf("both pins of one file did not move:\n got %q\nwant %q", got, want)
	}
}

// A literal the cut cannot locate refuses the release with the tree
// UNTOUCHED. The pins are resolved in registry order, so bundle.* is
// rewritten before parser.*: a run that wrote as it went would leave the
// alias floor on the cut and the contract floor on its old number, a split
// no later cut can read — the alias one would then name a release nobody
// cut.
func TestALiteralTheCutCannotLocateLeavesEveryFileUntouched(t *testing.T) {
	root := fixtureRepo(t, "3.180.0", "3.141.0", "3.178.0", "3.180.0")
	writeFile(t, root, "pkg/dsl/parser/floors.go", "package parser\n\nconst prefix = \"3.180\"\nconst ContractSince = prefix + \".0\"\n")
	gittest.Run(t, root, "add", "pkg/dsl/parser/floors.go")
	gittest.Run(t, root, "commit", "-q", "-m", "chore: the contract floor is computed")
	before := snapshot(t, floorFiles(root))
	bump(t, root, "3.179.1", "### Features\n\n* **dsl:** the contract syntax and the claw tool alias resolver")
	err := Run(Options{Root: root, Apply: true, Floors: fixtureFloors("3.180.0", "3.141.0", "3.178.0", "3.180.0")})
	if err == nil {
		t.Fatal("a pin whose literal cannot be located did not refuse the release")
	}
	if !strings.Contains(err.Error(), "refusing the release") || !strings.Contains(err.Error(), "never an expression") {
		t.Fatalf("the refusal does not name the shape: %v", err)
	}
	requireUntouched(t, floorFiles(root), before)
}

// A write that fails leaves NOTHING moved. The pins are resolved in registry
// order, so pkg/bundle is staged before pkg/dsl/parser: a run that wrote as
// it went would leave the alias floor naming the release being cut and the
// parser floors naming the pending minor — a split no later cut can read,
// and one the documented recovery (restoring the bumped files) does not
// undo. The report must stay silent too: a line naming a realignment that
// did not happen is worse than no line.
func TestAWriteThatFailsMovesNoFileAndReportsNoRealignment(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test closes")
	}
	root := fixtureRepo(t, "3.180.0", "3.180.0", "3.180.0", "3.180.0")
	before := snapshot(t, floorFiles(root))
	bump(t, root, "3.179.1", "### Features\n\n* **dsl:** the contract syntax, the import unit and the claw tool alias resolver")
	sealed := filepath.Join(root, "pkg", "dsl", "parser")
	if err := os.Chmod(sealed, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })

	var out bytes.Buffer
	err := Run(Options{Root: root, Apply: true, Floors: fixtureFloors("3.180.0", "3.180.0", "3.180.0", "3.180.0"), Stdout: &out})
	if err == nil {
		t.Fatal("a cut that cannot write every floor reported success")
	}
	if !strings.Contains(err.Error(), "staging the realignment of") {
		t.Fatalf("the failure does not name the write it could not make: %v", err)
	}
	requireUntouched(t, floorFiles(root), before)
	if strings.Contains(out.String(), "→") {
		t.Fatalf("the report claims a realignment no file carries:\n%s", out.String())
	}
	for _, path := range floorFiles(root) {
		if _, err := os.Stat(path + ".floors-tmp"); !os.IsNotExist(err) {
			t.Fatalf("%s.floors-tmp survived the refusal (%v)", path, err)
		}
	}
}

// The cut witness reads HEAD through git, and every way that read can fail
// is a refusal naming it — never a default that would let --apply rewrite
// the pins on a tree where no release is being cut.
func TestTheCutWitnessRefusesWhatItCannotRead(t *testing.T) {
	floors := fixtureFloors("3.180.0", "3.141.0", "3.178.0", "3.179.0")
	for name, tc := range map[string]struct {
		break_ func(t *testing.T, root string)
		want   string
	}{
		"not a git repository": {
			func(t *testing.T, root string) {
				if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
					t.Fatal(err)
				}
			},
			"git show HEAD:package.json",
		},
		"HEAD is unborn": {
			func(t *testing.T, root string) {
				if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
					t.Fatal(err)
				}
				gittest.Run(t, root, "init", "-q")
			},
			"git show HEAD:package.json",
		},
		"the package.json HEAD carries has no version": {
			func(t *testing.T, root string) {
				writeFile(t, root, "package.json", `{"name":"fixture","private":true}`)
				gittest.Run(t, root, "add", "package.json")
				gittest.Run(t, root, "commit", "-q", "-m", "chore: drop the version")
			},
			"carries no version",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := fixtureRepo(t, "3.180.0", "3.141.0", "3.178.0", "3.179.0")
			before := snapshot(t, floorFiles(root))
			tc.break_(t, root)
			bump(t, root, "3.179.1", "### Features\n\n* **dsl:** the contract syntax")
			err := Run(Options{Root: root, Apply: true, Floors: floors})
			if err == nil {
				t.Fatalf("%s did not refuse --apply", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not name %q: %v", tc.want, err)
			}
			requireUntouched(t, floorFiles(root), before)
		})
	}
}

// The witness for "a release is being cut" is the VERSION, not the file:
// any other uncommitted edit to package.json would otherwise read as a cut
// and back-date a pending pin onto a release already cut without the
// syntax — and on an exempted floor nothing downstream would notice.
func TestApplyOnAnUnrelatedPackageJSONEditRefuses(t *testing.T) {
	root := fixtureRepo(t, "3.180.0", "3.180.0", "3.178.0", "3.179.0")
	before := snapshot(t, floorFiles(root))
	writeFile(t, root, "package.json", `{"name":"fixture","private":true,"version":"3.179.0","packageManager":"pnpm@10"}`)
	err := Run(Options{Root: root, Apply: true, Floors: fixtureFloors("3.180.0", "3.180.0", "3.178.0", "3.179.0")})
	if err == nil || !strings.Contains(err.Error(), "no release is being cut") {
		t.Fatalf("--apply on an edit that did not bump the version: %v", err)
	}
	requireUntouched(t, floorFiles(root), before)
}

// A cut that ships no floor: every pin is at or below the cut, the
// release test's released arm holds each one, and no file moves.
func TestAReleasedPinStaysUntouched(t *testing.T) {
	root := fixtureRepo(t, "3.179.0", "3.141.0", "3.178.0", "3.179.0")
	before := snapshot(t, floorFiles(root))
	bump(t, root, "3.179.1", "### Bug Fixes\n\n* **runner:** a leak")
	var out bytes.Buffer
	if err := Run(Options{Root: root, Apply: true, Floors: fixtureFloors("3.179.0", "3.141.0", "3.178.0", "3.179.0"), Stdout: &out}); err != nil {
		t.Fatalf("the cut refused: %v", err)
	}
	requireUntouched(t, floorFiles(root), before)
	if strings.Count(out.String(), "is released; the release test holds it") != 4 {
		t.Fatalf("the report does not say all four pins stay:\n%s", out.String())
	}
}

func TestAPinTwoMinorsAheadRefusesTheRelease(t *testing.T) {
	root := fixtureRepo(t, "3.181.0", "3.141.0", "3.178.0", "3.179.0")
	before := snapshot(t, floorFiles(root))
	bump(t, root, "3.179.1", "### Features\n\n* **dsl:** the contract syntax, again")
	err := Run(Options{Root: root, Apply: true, Floors: fixtureFloors("3.181.0", "3.141.0", "3.178.0", "3.179.0")})
	if err == nil {
		t.Fatal("a pin two minors ahead did not refuse the release")
	}
	if !strings.Contains(err.Error(), "refusing the release") || !strings.Contains(err.Error(), "parser.ContractSince = 3.181.0") || !strings.Contains(err.Error(), "next minor") {
		t.Fatalf("the refusal does not name the pin and the shape: %v", err)
	}
	requireUntouched(t, floorFiles(root), before)
}

// The notes the cut renders do not carry the floor's word — the shape a
// floor merged under a non-conventional subject leaves (#1154): the cut
// refuses, whether the pin is still at the next minor or already names the
// cut, and the refusal names the remedy — the marker table.
func TestSilentNotesRefuseTheCut(t *testing.T) {
	for name, contract := range map[string]string{
		"pending at the next minor": "3.180.0",
		"already naming the cut":    "3.179.1",
	} {
		t.Run(name, func(t *testing.T) {
			root := fixtureRepo(t, contract, "3.141.0", "3.178.0", "3.179.0")
			before := snapshot(t, floorFiles(root))
			bump(t, root, "3.179.1", "### Features\n\n* **dsl:** fragments declare their interface")
			err := Run(Options{Root: root, Apply: true, Floors: fixtureFloors(contract, "3.141.0", "3.178.0", "3.179.0")})
			if err == nil {
				t.Fatal("notes silent about the floor did not refuse the release")
			}
			for _, want := range []string{"parser.ContractSince would name 3.179.1", `do not carry "contract"`, "#1154", "releaseNotesMarkers"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal lacks %q: %v", want, err)
				}
			}
			requireUntouched(t, floorFiles(root), before)
		})
	}
}

// The marker table's exemption is the hatch: an exempted floor realigns
// through notes that say nothing about it.
func TestAnExemptedFloorRealignsThroughSilentNotes(t *testing.T) {
	root := fixtureRepo(t, "3.179.0", "3.180.0", "3.178.0", "3.179.0")
	bump(t, root, "3.179.1", "### Bug Fixes\n\n* **runner:** a leak")
	if err := Run(Options{Root: root, Apply: true, Floors: fixtureFloors("3.179.0", "3.180.0", "3.178.0", "3.179.0")}); err != nil {
		t.Fatalf("the exempted floor did not cut through: %v", err)
	}
	if got, want := string(readFile(t, floorFiles(root)[1])), "package parser\n\nvar ProfileSince = map[int]string{2: \"3.179.1\"}\n\nconst ImportSince = \"3.178.0\"\n"; got != want {
		t.Fatalf("the exempted pin was not realigned:\n got %q\nwant %q", got, want)
	}
}

// The hook wired one slot too early — package.json bumped, the changelog
// not rendered yet — refuses with the slot's name, and judges no pin.
func TestApplyBeforeTheChangelogIsRenderedRefuses(t *testing.T) {
	root := fixtureRepo(t, "3.180.0", "3.141.0", "3.178.0", "3.179.0")
	before := snapshot(t, floorFiles(root))
	writeFile(t, root, "package.json", `{"name":"fixture","private":true,"version":"3.179.1"}`)
	err := Run(Options{Root: root, Apply: true, Floors: fixtureFloors("3.180.0", "3.141.0", "3.178.0", "3.179.0")})
	if err == nil || !strings.Contains(err.Error(), "no section for 3.179.1") || !strings.Contains(err.Error(), "before:git:beforeRelease") {
		t.Fatalf("a cut whose section is not rendered did not refuse with the slot's name: %v", err)
	}
	requireUntouched(t, floorFiles(root), before)
}

// --apply on a committed tree is not a cut: nothing is being released, and
// a "realignment" would move a pending pin onto a release already cut
// without the syntax.
func TestApplyOnACommittedTreeRefuses(t *testing.T) {
	root := fixtureRepo(t, "3.180.0", "3.141.0", "3.178.0", "3.179.0")
	before := snapshot(t, floorFiles(root))
	floors := fixtureFloors("3.180.0", "3.141.0", "3.178.0", "3.179.0")
	err := Run(Options{Root: root, Apply: true, Floors: floors})
	if err == nil || !strings.Contains(err.Error(), "no release is being cut") {
		t.Fatalf("--apply on the committed release did not refuse: %v", err)
	}
	// The same after a cut has been committed: its notes carry the word,
	// so only the witness stands between the pending pin and a rewrite.
	bump(t, root, "3.179.1", "### Bug Fixes\n\n* **dsl:** the contract gallery template, read at last")
	gittest.Run(t, root, "add", "package.json", "CHANGELOG.md")
	gittest.Run(t, root, "commit", "-q", "-m", "chore: release v3.179.1")
	err = Run(Options{Root: root, Apply: true, Floors: floors})
	if err == nil || !strings.Contains(err.Error(), "no release is being cut") {
		t.Fatalf("--apply on a committed cut did not refuse: %v", err)
	}
	requireUntouched(t, floorFiles(root), before)
}

// A preview writes nothing and says what the release test says: a pending
// pin is reported as such, a released one holds, a shape the test refuses
// is refused with the test's own words.
func TestPreviewWritesNothingAndHoldsTheReleaseTestsRule(t *testing.T) {
	root := fixtureRepo(t, "3.180.0", "3.141.0", "3.178.0", "3.179.0")
	before := snapshot(t, floorFiles(root))
	var out bytes.Buffer
	if err := Run(Options{Root: root, Floors: fixtureFloors("3.180.0", "3.141.0", "3.178.0", "3.179.0"), Stdout: &out}); err != nil {
		t.Fatalf("the preview refused: %v", err)
	}
	requireUntouched(t, floorFiles(root), before)
	for _, line := range []string{"preview 3.179.0", "parser.ContractSince = 3.180.0 is ahead at the next minor: the next cut realigns it", "bundle.ToolAliasesSince = 3.179.0 is released"} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("the report lacks %q:\n%s", line, out.String())
		}
	}

	over := fixtureRepo(t, "3.181.0", "3.141.0", "3.178.0", "3.179.0")
	overBefore := snapshot(t, floorFiles(over))
	err := Run(Options{Root: over, Floors: fixtureFloors("3.181.0", "3.141.0", "3.178.0", "3.179.0")})
	if err == nil || !strings.Contains(err.Error(), "the syntax floors do not hold") || !strings.Contains(err.Error(), "not the next minor") {
		t.Fatalf("a preview of an over-pin did not say what the release test says: %v", err)
	}
	requireUntouched(t, floorFiles(over), overBefore)
}

// Right after a minor cut the release before the checkout is one minor
// down; the pending pin main holds is the next minor above the NEWEST
// release, and the preview must measure from the same place as the test.
func TestPreviewAfterAMinorCutHoldsWhatTheReleaseTestHolds(t *testing.T) {
	root := fixtureRepo(t, "3.181.0", "3.141.0", "3.178.0", "3.179.0")
	bump(t, root, "3.180.0", "### Features\n\n* **dsl:** the contract syntax, again")
	gittest.Run(t, root, "add", "package.json", "CHANGELOG.md")
	gittest.Run(t, root, "commit", "-q", "-m", "chore: release v3.180.0")
	var out bytes.Buffer
	if err := Run(Options{Root: root, Floors: fixtureFloors("3.181.0", "3.141.0", "3.178.0", "3.179.0"), Stdout: &out}); err != nil {
		t.Fatalf("the preview refused a pin the release test holds: %v", err)
	}
	if !strings.Contains(out.String(), "parser.ContractSince = 3.181.0 is ahead at the next minor") {
		t.Fatalf("the pending pin is not reported as such:\n%s", out.String())
	}
}
