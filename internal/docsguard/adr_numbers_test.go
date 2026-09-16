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
	dir := filepath.Join("..", "..", "docs", "adr")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	byNumber := map[string][]string{}
	numbered := regexp.MustCompile(`^(\d{3})-.+\.md$`)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if m := numbered.FindStringSubmatch(e.Name()); m != nil {
			byNumber[m[1]] = append(byNumber[m[1]], e.Name())
		}
	}
	if len(byNumber) == 0 {
		t.Fatalf("no ADR matched %s in %s — the guard reads nothing and would pass on anything", numbered, dir)
	}

	onDisk := map[string]bool{}
	for number, files := range byNumber {
		if len(files) > 1 {
			sort.Strings(files)
			for _, f := range files {
				onDisk[number+" "+f] = true
			}
		}
	}

	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("read the ADR index: %v", err)
	}
	// | `098` | [098-connector-catalog.md](098-connector-catalog.md) | … |
	row := regexp.MustCompile("\\|\\s*`(\\d{3})`\\s*\\|\\s*\\[([^\\]]+\\.md)\\]")
	declared := map[string]bool{}
	for _, m := range row.FindAllStringSubmatch(string(readme), -1) {
		declared[m[1]+" "+m[2]] = true
	}
	if len(declared) == 0 {
		t.Fatal("the ADR index declares no collision row — either the table moved or this guard stopped matching it, and it would then pass on a repository full of duplicates")
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
		t.Errorf("these ADRs share a number with another and are not in docs/adr/README.md:\n  %s\n"+
			"An ADR number is never reassigned: give the new record the next FREE number rather than declaring the clash.",
			strings.Join(undeclared, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("docs/adr/README.md declares these as sharing a number, and they no longer do:\n  %s\n"+
			"Remove the rows: a table that names collisions which do not exist sends a reader looking for a document that is no longer ambiguous.",
			strings.Join(stale, "\n  "))
	}
}
