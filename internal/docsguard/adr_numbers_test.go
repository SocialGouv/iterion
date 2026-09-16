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
var adrCollisionRow = regexp.MustCompile("\\|\\s*`(\\d{3})`\\s*\\|\\s*\\[([^\\]]+\\.md)\\]")

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
	for _, m := range adrCollisionRow.FindAllStringSubmatch(string(readme), -1) {
		declared[m[1]+" "+m[2]] = true
	}

	var problems []string
	if len(onDisk) > 0 && len(declared) == 0 {
		return []string{"ADRs on disk share a number, yet the index declares no collision row — either the table moved or this guard stopped matching it, and it would then pass on a tree full of duplicates"}
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
