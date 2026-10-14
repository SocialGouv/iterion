package floortest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pinNames are the syntax floors, by the name of the constant that pins each
// — bundle.SyntaxFloors spells them the same way.
var pinNames = []string{"ProfileSince", "ImportSince", "ContractSince", "VarMatchingSince", "ToolAliasesSince", "DeclaredEmptyToolsSince"}

// exemptMarker is the greppable escape hatch: a test that rests a floor case
// on a pin on purpose carries it on one line, NAMING the pin it covers and
// the reason. An exemption reaches the pins its own line names and no other,
// so a file that spells the floor a case is AT as that floor's own pin does
// not thereby vouch for a sibling pin spliced in beside it.
const exemptMarker = "floortest:exempt"

var (
	pinAlternatives = strings.Join(pinNames, "|")
	quotedString    = "(?:\"[^\"\n]*\"|`[^`\n]*`)"
	pinReference    = regexp.MustCompile(`\b(?:parser\.|bundle\.)?(` + pinAlternatives + `)\b`)
	pinSplicedIn    = regexp.MustCompile(
		quotedString + `\s*\+\s*(?:parser\.|bundle\.)?(` + pinAlternatives + `)\b` +
			`|\b(?:parser\.|bundle\.)?(` + pinAlternatives + `)(?:\[[^\]\n]*\])?\s*\+\s*` + quotedString)
	// profilePinByIndex is a profile pin read by a literal index. The
	// collapse a case runs under repoints the pin of the NEWEST profile,
	// parser.ProfileSince[parser.MaxProfile]; an index spelled out names a
	// profile this build has already shipped, whose release no cut moves
	// again.
	profilePinByIndex = regexp.MustCompile(`\bProfileSince\[\s*\d+\s*\]`)
	eachPinOrderCall  = regexp.MustCompile(`\bEachPinOrder\(`)
)

// exemptedPins are the pins the exemption marker lines of a file name.
func exemptedPins(body string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, exemptMarker) {
			continue
		}
		for _, pin := range matched(pinReference, line) {
			out[pin] = true
		}
	}
	return out
}

// floorPinComplaints reads one test file for the two shapes that rest a floor
// case on a pin the release cut moves out from under it — the mechanical half
// of this package, since reviewing the class by hand does not work:
//
//   - a pin spliced into a string (a floor, a constraint, an engine build)
//     while a second pin is named. The cut writes the version being released
//     into every pin it realigns (internal/floorsalign), so two floors cut in
//     one release carry the same number: that day the lower pin IS the floor,
//     and a case meaning "a release below the floor" states the floor itself
//     — five sites at once (#1566). Below derives the release from the floor
//     under test instead, and reads no other pin.
//   - a case run under EachPinOrder that reads a profile pin by a literal
//     index. The collapse repoints the newest profile's pin, so a case keyed
//     on another index runs the live world twice: the collapsed order is then
//     a copy of its twin, green and asserting nothing. parser.MaxProfile keys
//     it to the pin that moves.
//
// A file that does either on purpose carries the exemption marker naming the
// pin it rests on, with its reason.
func floorPinComplaints(body string) []string {
	exempt := exemptedPins(body)
	var out []string
	if named := matched(pinReference, body); len(named) > 1 {
		var uncovered []string
		for _, pin := range matched(pinSplicedIn, body) {
			if !exempt[pin] {
				uncovered = append(uncovered, pin)
			}
		}
		if len(uncovered) > 0 {
			out = append(out, "builds a string from "+strings.Join(uncovered, ", ")+", and names "+strings.Join(named, ", "))
		}
	}
	if eachPinOrderCall.MatchString(body) && profilePinByIndex.MatchString(body) && !exempt["ProfileSince"] {
		out = append(out, "runs a case under EachPinOrder while reading "+profilePinByIndex.FindString(body)+", not the pin the collapse moves (parser.ProfileSince[parser.MaxProfile])")
	}
	return out
}

// TestNoTestSpellsABelowFloorAsAnotherPin holds the whole tree to what
// floorPinComplaints reads, this package's own files included: the chokepoint
// is Below, and a file routed through here is swept like any other, so one
// case coming through the helper cannot vouch for the pin its neighbour
// splices in.
func TestNoTestSpellsABelowFloorAsAnotherPin(t *testing.T) {
	root := filepath.Join("..", "..")
	skipNames := map[string]bool{"vendor": true, "studio": true, "node_modules": true, "testdata": true}

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Hidden directories hold scratch clones of other people's code
			// (.local, .works, .repos), as in the production sweeps.
			name := info.Name()
			if skipNames[name] || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path) // #nosec G304 -- walking this repository's own tree
		if rerr != nil {
			return rerr
		}
		for _, complaint := range floorPinComplaints(string(src)) {
			offenders = append(offenders, filepath.ToSlash(path)+" "+complaint)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("test file(s) resting a floor case on a pin the release cut moves out from under it: two floors cut in one release carry the same number, and a case keyed on their order asserts something else than it says.\nDerive a release below a floor from that floor (floortest.Below), key a collapse case on parser.MaxProfile, or keep the pin and add a %q comment NAMING it and the reason:\n  %s",
			exemptMarker, strings.Join(offenders, "\n  "))
	}
}

// What the sweep reads in one file, shape by shape. The bodies are built by
// concatenation rather than spelled out: the patterns read the SOURCE of a
// test file, and a literal of the condemned shape would make this file an
// offender of its own sweep.
func TestTheSweepReadsEachShapeThatRestsAFloorOnAPin(t *testing.T) {
	pro, imp, con := "ProfileSince", "ImportSince", "ContractSince"
	lines := func(l ...string) string { return strings.Join(l, "\n") + "\n" }
	floorFrom := func(pin string) string { return "\t\tIterion: \">= \" + parser." + pin + "}}, one)" }
	reads := func(pin string) string { return "\t\twant := parser." + pin }

	for _, tc := range []struct {
		name  string
		body  string
		wants []string
	}{
		{
			// The class, in a file routed through this package: the import
			// is no cover for the splice beside it.
			name:  "a floor spliced from a sibling pin, in a file that imports this package",
			body:  lines(`import "github.com/SocialGouv/iterion/internal/floortest"`, floorFrom(imp), reads(con)),
			wants: []string{"builds a string from ImportSince, and names ImportSince, ContractSince"},
		},
		{
			name: "a floor spliced from the only pin the file names",
			body: lines(floorFrom(imp)),
		},
		{
			name: "an exemption naming the pin spliced",
			body: lines("\t// "+exemptMarker+" "+imp+" — the floor this case is AT", floorFrom(imp), reads(con)),
		},
		{
			// An exemption is not a blanket: the pin it does not name stays
			// swept, which is the shape the five sites of #1566 had.
			name:  "an exemption naming another pin than the one spliced",
			body:  lines("\t// "+exemptMarker+" "+imp+" — the floor this case is AT", floorFrom(con), reads(imp)),
			wants: []string{"builds a string from ContractSince, and names ImportSince, ContractSince"},
		},
		{
			name:  "a collapse case keyed on a profile pin by index",
			body:  lines("\tfloortest.EachPinOrder(t, parser."+imp+", func(t *testing.T) {", reads(pro+"[2]")),
			wants: []string{"runs a case under EachPinOrder while reading " + pro + "[2], not the pin the collapse moves (parser.ProfileSince[parser.MaxProfile])"},
		},
		{
			name: "a collapse case keyed on the newest profile",
			body: lines("\tfloortest.EachPinOrder(t, parser."+imp+", func(t *testing.T) {", reads(pro+"[parser.MaxProfile]")),
		},
		{
			name: "a profile pin by index outside any collapse case",
			body: lines(reads(pro + "[2]")),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := floorPinComplaints(tc.body)
			if len(got) != len(tc.wants) {
				t.Fatalf("%d complaint(s) for %s, want %d: %v", len(got), tc.name, len(tc.wants), got)
			}
			for i, want := range tc.wants {
				if got[i] != want {
					t.Errorf("complaint %d = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

// matched is the distinct pin names a pattern finds, in the order pinNames
// lists them, so a failure reads the same on every run.
func matched(re *regexp.Regexp, body string) []string {
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		for _, group := range m[1:] {
			if group != "" {
				seen[group] = true
			}
		}
	}
	var out []string
	for _, name := range pinNames {
		if seen[name] {
			out = append(out, name)
		}
	}
	return out
}
