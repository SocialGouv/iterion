package runtime

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The engine carries no knowledge of a SPECIFIC run or a SPECIFIC bot
// (CLAUDE.md, "The ENGINE stays bot-agnostic"). The shape that broke that
// rule was live debugging left behind: a branch gated on a literal run id
// so a single production run would log extra detail. It compiles, it is
// unreachable for every other deployment, and nothing failed when it
// shipped — so this sweep is the deterministic check that would have.
//
// Scoped to non-test files of this package: a TEST may of course write a
// run id, and the sweep is not a repo-wide gate.
func TestEngineSweep_NoHardcodedRunID(t *testing.T) {
	// A canonical UUID as a Go string literal. Run ids are UUIDv7 here, but
	// any literal of that shape inside the engine is the same defect.
	runID := regexp.MustCompile(`"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if m := runID.FindString(string(body)); m != "" {
			t.Errorf("%s hardcodes the run id %s — the engine must not branch on a specific run (or a specific bot's node/field names); drive the behaviour from the IR or an option instead", name, m)
		}
	}
}
