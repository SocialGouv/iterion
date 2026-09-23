package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The attempted ledger select_candidate carries is an OBJECT keyed by package
// name: it spreads with `{ ...attempted, [p.name]: {...} }` and
// computeOnlyPatches reads `Object.values(...).every(v => v.risk === "patch")`.
//
// The `not has_patches` entry used to seed it with the `with:` literal "[]".
// A mapping that is not exactly one reference arrives as a STRING (C152), so
// the seed reached the script as the two-character string "[]" — and spreading
// a string spreads its character INDICES. Measured on the shipped script: the
// first pick produced {"0":"[", "1":"]", "lodash":{…}} instead of
// {"lodash":{…}}, which made computeOnlyPatches permanently false and left two
// junk names in the count that gates max_packages_per_run.
//
// Two consequences on a patch-only run entered through that edge: the Phase 2
// short-circuit (phase2_decider.go_done, which reads only_patches_attempted)
// could never fire, and the per-run package cap stopped two packages early.
// Binding the producer's typed attempted_after_batch — {} when there are no
// patches, the same field the has_families sibling edge already binds — is
// what fixes both.
//
// The test derives its input from the EDGE, so the mutation that reddens it is
// the production one: put the literal back on the mapping.
func TestRenovacyAttemptedLedgerSeedIsNotSpreadAsAString(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH")
	}
	const rel = "secured-renovacy/main.bot"
	wf := compilePlanPhaseBot(t, "secured-renovacy")

	var mapping *ir.DataMapping
	for _, e := range wf.Edges {
		if e.From == "bucket_patches" && e.To == "select_candidate" {
			if m := mappingOf(e, "attempted"); m != nil {
				mapping = m
			}
		}
	}
	if mapping == nil {
		t.Fatal("the bucket_patches -> select_candidate edge no longer maps `attempted`: the solo loop is seeded from somewhere else and this witness no longer watches it")
	}

	// What the runtime delivers to the script. A mapping that is exactly one
	// reference passes the producer's typed value through — bucket_patches
	// prints attempted_after_batch, and with no patches it is {}. Anything
	// else is text, and the script receives the text.
	seed := "{}"
	if !isSingleRef(mapping.Raw) {
		seed = string(mustJSON(t, mapping.Raw))
	}

	script := toolScript(t, rel, "select_candidate")
	for ref, val := range map[string]string{
		"{{input.packages}}":     `[{"name":"lodash","current":"4.17.20","target":"4.17.21","risk":"patch","ecosystem":"npm"}]`,
		"{{input.attempted}}":    seed,
		"{{input.scope}}":        `"all"`,
		"{{input.max_packages}}": "10",
		// A path that does not exist: isAtTarget must not find a lockfile and
		// filter the only candidate away.
		"{{input.workspace_dir}}":  `"` + filepath.Join(t.TempDir(), "absent") + `"`,
		"{{input.update_scope}}":   `""`,
		"{{vars.update_scope}}":    `""`,
		"{{input.records_dir}}":    `""`,
		"{{input.attempted_kind}}": `""`,
	} {
		script = strings.ReplaceAll(script, ref, val)
	}
	if i := strings.Index(script, "{{"); i >= 0 {
		t.Fatalf("unsubstituted ref left in the script near %q", script[i:min(i+60, len(script))])
	}

	path := filepath.Join(t.TempDir(), "select_candidate.js")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("node", path).Output()
	if err != nil {
		t.Fatalf("running the shipped select_candidate script: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var res struct {
		SelectedPackage      string                     `json:"selected_package"`
		CumulativeAttempted  map[string]json.RawMessage `json:"cumulative_attempted"`
		OnlyPatchesAttempted bool                       `json:"only_patches_attempted"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil {
		t.Fatalf("the script did not print the candidate object (%v): %s", err, lines[len(lines)-1])
	}
	if res.SelectedPackage != "lodash" {
		t.Fatalf("the fixture did not reach the pick path (selected %q): it proves nothing about the ledger", res.SelectedPackage)
	}
	if len(res.CumulativeAttempted) != 1 {
		names := make([]string, 0, len(res.CumulativeAttempted))
		for k := range res.CumulativeAttempted {
			names = append(names, k)
		}
		t.Errorf("cumulative_attempted carries %d entries %v after ONE pick, want 1 (`lodash`): the seed was spread as a string, so its characters became package names — they inflate attempted_count, which gates max_packages_per_run", len(res.CumulativeAttempted), names)
	}
	if !res.OnlyPatchesAttempted {
		t.Errorf("only_patches_attempted is false after one patch-tier pick: phase2_decider.go_done reads this field, so the Phase 2 short-circuit can never fire on a patch-only run entered through this edge")
	}
}

// isSingleRef reports whether a mapping value is exactly one reference — the
// only shape the runtime passes through with its type.
//
// It does NOT trim. pkg/runtime.resolveMapping requires the single reference
// span to cover the WHOLE raw value (span.start == 0 && span.end == len(Raw)),
// and the compiler's C152 mirror uses the identical untrimmed test: one
// leading space and the value is interpolated to text. A trimmed oracle here
// was more lenient than the producer, so a space-padded reference — which
// `iterion validate` refuses — read as a typed passthrough and this witness
// stayed green on a mapping the engine had already broken.
func isSingleRef(raw string) bool {
	return strings.HasPrefix(raw, "{{") && strings.HasSuffix(raw, "}}") &&
		strings.Count(raw, "{{") == 1 && strings.Count(raw, "}}") == 1
}

func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
