package docsguard

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// An ADR number is an identifier: it is cited from code comments, commit
// messages, pull requests and other ADRs, and from places this repository
// cannot reach. Two records sharing one make every citation of it ambiguous,
// and the ambiguity is SILENT — a reader follows the wrong document and finds
// coherent prose about the wrong subject.
//
// Eight numbers were taken twice before anything watched. Renumbering them
// would repair the links inside this tree by breaking every citation outside
// it, so they stay, declared in docs/adr/README.md where a reader who follows
// one can resolve it.
//
// This guard is the chokepoint that keeps the list from growing. It compares
// the collisions ON DISK with the ones the README declares, in BOTH
// directions: a new collision nobody declared fails, and a declared number
// that no longer collides fails too, so the table cannot rot into fiction.
func TestADRNumberCollisionsAreDeclared(t *testing.T) {
	for _, problem := range checkADRNumbers(t, filepath.Join("..", "..", "docs", "adr")) {
		t.Error(problem)
	}
}

// TestTheGuardAcceptsTheEndStateItDrivesToward.
//
// Renumbering the last colliding record and deleting its rows is the
// remediation this guard exists to make easy. A guard that then refused the
// clean repository would punish the fix — and, reading an empty table as a
// broken matcher, would blame the wrong cause while pointing at a tree that
// has nothing left to declare.
func TestTheGuardAcceptsTheEndStateItDrivesToward(t *testing.T) {
	dir := writeADRFixture(t, "# Architecture decision records\n\nNo number is used twice.\n",
		"001-first.md", "002-second.md", "003-third.md")
	if problems := checkADRNumbers(t, dir); len(problems) != 0 {
		t.Fatalf("a tree with no collision left was refused:\n  %s", strings.Join(problems, "\n  "))
	}
}

// And the inertness check still bites where it is meant to: collisions on
// disk and an index that declares none is a guard no longer reading the
// table, not a clean tree.
func TestAnIndexDeclaringNothingIsRefusedWhileCollisionsRemain(t *testing.T) {
	dir := writeADRFixture(t, "# Architecture decision records\n\nNothing declared here.\n",
		"001-first.md", "002-second.md", "002-second-take.md")
	problems := checkADRNumbers(t, dir)
	if len(problems) == 0 {
		t.Fatal("a collision with an empty index passed: the guard reads a table it no longer matches")
	}
	if !strings.Contains(strings.Join(problems, "\n"), "declares no collision row") {
		t.Fatalf("the refusal does not name the broken matcher: %v", problems)
	}
}

// The guard's two working directions, and the row that lies about where it
// points. None of the three is reachable from the tests above — the real tree
// has nothing to report and the inertness fixture returns early — so without
// these, inverting a comparison or dropping an append would ship green and
// the guard would pass on a tree full of duplicates.
func TestAnUndeclaredCollisionIsRefused(t *testing.T) {
	dir := writeADRFixture(t, declaredPairRows,
		"001-first.md", "001-first-take.md", "002-second.md", "002-second-take.md")
	problems := strings.Join(checkADRNumbers(t, dir), "\n")
	if !strings.Contains(problems, "002 002-second-take.md") {
		t.Fatalf("a collision the index does not declare was not reported: %q", problems)
	}
}

func TestADeclaredNumberThatNoLongerCollidesIsRefused(t *testing.T) {
	dir := writeADRFixture(t, declaredPairRows+"| `003` | [003-gone.md](003-gone.md) | Stale |\n",
		"001-first.md", "001-first-take.md")
	problems := strings.Join(checkADRNumbers(t, dir), "\n")
	if !strings.Contains(problems, "003 003-gone.md") {
		t.Fatalf("a row declaring a number that no longer collides was not reported: %q", problems)
	}
}

// A half-applied rename updates the label and leaves the target: the row
// still satisfies both directions while sending every reader who follows it
// to the other record of the pair.
func TestARowWhoseLabelAndLinkDisagreeIsRefused(t *testing.T) {
	dir := writeADRFixture(t,
		"| `001` | [001-first.md](001-first.md) | Declared |\n"+
			"| `001` | [001-renamed.md](001-first-take.md) | Label renamed, link not |\n",
		"001-first.md", "001-first-take.md")
	problems := strings.Join(checkADRNumbers(t, dir), "\n")
	if !strings.Contains(problems, "reads [001-renamed.md] but links to 001-first-take.md") {
		t.Fatalf("a row pointing somewhere other than it reads was accepted: %q", problems)
	}
}

const declaredPairRows = "| `001` | [001-first.md](001-first.md) | Declared |\n" +
	"| `001` | [001-first-take.md](001-first-take.md) | Declared |\n"

func writeADRFixture(t *testing.T, readme string, records ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range records {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("# "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// | `098` | [098-connector-catalog.md](098-connector-catalog.md) | … |
//
// Both the label and the TARGET are captured. The target is what a reader
// follows, so it is what the guard compares; the label is checked against it
// because a half-applied rename that updates one and not the other sends
// every reader of that row to the wrong document — the silent
// mis-resolution the table exists to prevent.
var adrCollisionRow = regexp.MustCompile("\\|\\s*`(\\d{3})`\\s*\\|\\s*\\[([^\\]]+\\.md)\\]\\(([^)]+\\.md)\\)")

var adrRecord = regexp.MustCompile(`^(\d{3})-.+\.md$`)

// checkADRNumbers reports one message per inconsistency between the records
// in dir and the collisions its README declares, and nothing when the two
// agree — including when neither has any, which is the end state.
func checkADRNumbers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	byNumber := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if m := adrRecord.FindStringSubmatch(e.Name()); m != nil {
			byNumber[m[1]] = append(byNumber[m[1]], e.Name())
		}
	}
	if len(byNumber) == 0 {
		t.Fatalf("no file in %s matched %s — the guard read nothing and would pass on anything", dir, adrRecord)
	}

	onDisk := map[string]bool{}
	for number, files := range byNumber {
		if len(files) > 1 {
			for _, f := range files {
				onDisk[number+" "+f] = true
			}
		}
	}

	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("read the ADR index: %v", err)
	}
	declared := map[string]bool{}
	var problems []string
	for _, m := range adrCollisionRow.FindAllStringSubmatch(string(readme), -1) {
		number, label, target := m[1], m[2], m[3]
		declared[number+" "+target] = true
		if label != target {
			problems = append(problems, "the index row for "+number+" reads ["+label+"] but links to "+target+
				"\nA reader follows the link: make the two name the same record.")
		}
	}

	if len(onDisk) > 0 && len(declared) == 0 {
		return append(problems, "ADRs on disk share a number, yet the index declares no collision row — either the table moved or this guard stopped matching it, and it would then pass on a tree full of duplicates")
	}

	var undeclared, stale []string
	for k := range onDisk {
		if !declared[k] {
			undeclared = append(undeclared, k)
		}
	}
	for k := range declared {
		if !onDisk[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(stale)

	if len(undeclared) > 0 {
		problems = append(problems, "these ADRs share a number with another and are not in docs/adr/README.md:\n  "+
			strings.Join(undeclared, "\n  ")+
			"\nAn ADR number is never reassigned: give the new record the next FREE number rather than declaring the clash.")
	}
	if len(stale) > 0 {
		problems = append(problems, "docs/adr/README.md declares these as sharing a number, and they no longer do:\n  "+
			strings.Join(stale, "\n  ")+
			"\nRemove the rows: a table naming collisions that do not exist sends a reader looking for a document that is no longer ambiguous.")
	}
	return problems
}
