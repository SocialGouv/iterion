package reliability

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The divergence baseline published in docs/workflow-reliability-1006.md is
// the artifact #1006's close-out stands on: eleven acceptance criteria, each
// pinned to the witness that refuses its violation. A criterion is only as
// true as that witness, so the ledger has to be checked against the tree
// rather than believed — a renamed or deleted test would otherwise leave a
// published "met" verdict standing over nothing, which is the false green
// the rollout doc's own compatibility rule refuses.

var (
	ledgerRow     = regexp.MustCompile(`(?m)^\| \d+ \| `)
	ledgerWitness = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")
	ledgerLink    = regexp.MustCompile(`\]\(\.\./([^)]+)\)`)
	testDecl      = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
)

const (
	rolloutDoc           = "../../docs/workflow-reliability-1006.md"
	ledgerHeading        = "## The divergence baseline"
	ledgerAcceptanceRows = 11
)

func readDivergenceLedger(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(rolloutDoc)
	if err != nil {
		t.Fatalf("read %s: %v", rolloutDoc, err)
	}
	doc := string(data)
	i := strings.Index(doc, ledgerHeading)
	if i < 0 {
		t.Fatalf("%s no longer publishes %q — the close-out's verdicts are the deliverable, not a draft", rolloutDoc, ledgerHeading)
	}
	return doc[i:]
}

// TestDivergenceLedgerCoversEveryAcceptanceCriterion refuses a published
// ledger that has quietly stopped covering the ticket's eleven criteria.
// Dropping a row is how an open criterion disappears without being decided:
// a criterion is either met with a witness, named as open, or retired on the
// ticket — never absent from the table that answers for it.
func TestDivergenceLedgerCoversEveryAcceptanceCriterion(t *testing.T) {
	ledger := readDivergenceLedger(t)
	if rows := len(ledgerRow.FindAllString(ledger, -1)); rows != ledgerAcceptanceRows {
		t.Fatalf("the divergence ledger has %d numbered rows, want %d — #1006 states eleven acceptance criteria and the table answers for each", rows, ledgerAcceptanceRows)
	}
}

// TestDivergenceLedgerWitnessesExist is the ratchet: every test the ledger
// names as a witness must be a test that exists. Judged on the declaration,
// so a mention in prose cannot stand in for a suite.
func TestDivergenceLedgerWitnessesExist(t *testing.T) {
	ledger := readDivergenceLedger(t)
	var named []string
	for _, m := range ledgerWitness.FindAllStringSubmatch(ledger, -1) {
		named = append(named, m[1])
	}
	sort.Strings(named)
	if len(named) == 0 {
		t.Fatal("the ledger names no witness at all — a verdict with no witness is an intention")
	}

	declared := map[string]bool{}
	repoRoot := filepath.Join("..", "..")
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", ".iterion", "testdata", "graphify-out":
				return filepath.SkipDir
			case "studio":
				if filepath.Dir(path) == repoRoot {
					return filepath.SkipDir
				}
			}
			// A directory carrying its own .git is a nested checkout (a
			// worktree, a sibling clone): its files belong to another tree.
			if path != repoRoot {
				if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, m := range testDecl.FindAllSubmatch(data, -1) {
			declared[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	if len(declared) == 0 {
		t.Fatal("the sweep found no test declaration at all — a sweep that sees nothing passes over anything")
	}
	for _, name := range named {
		if !declared[name] {
			t.Errorf("the divergence ledger names %s as a witness and no test declares it — a published verdict standing on a test that does not exist is worse than an open one", name)
		}
	}
}

// TestDivergenceLedgerLinksResolve keeps the owner column pointing at code
// that is still there: an owner link that 404s makes a verdict
// unverifiable by the next reader, which is the only reader that matters.
func TestDivergenceLedgerLinksResolve(t *testing.T) {
	ledger := readDivergenceLedger(t)
	links := map[string]bool{}
	for _, m := range ledgerLink.FindAllStringSubmatch(ledger, -1) {
		links[m[1]] = true
	}
	if len(links) == 0 {
		t.Fatal("the ledger links no owner — the verdicts name nothing to read")
	}
	for link := range links {
		if _, err := os.Stat(filepath.Join("..", "..", link)); err != nil {
			t.Errorf("the divergence ledger links %s, which does not resolve: %v", link, err)
		}
	}
}
