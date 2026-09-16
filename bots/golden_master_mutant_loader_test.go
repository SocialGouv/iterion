package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeMutant creates one mutant directory, with or without its meta.json.
func writeMutant(t *testing.T, dir, archetype string, withMeta bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apply.sh"), []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !withMeta {
		return
	}
	meta := `{"surface": "write", "archetype": "` + archetype + `", "targets": ["001"]}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mutantLoaderWorkspace builds the smallest tree the gate will read far enough
// to reach the mutant loader, and returns the workspace root.
func mutantLoaderWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	gm := filepath.Join(ws, ".golden-master")
	if err := os.MkdirAll(filepath.Join(gm, "canon"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"config.json":    `{"up": "true", "base_url": "http://127.0.0.1:1", "standard": 3, "personas": [{"name": "alice", "login": {"fields": {"user": "alice", "pass": "x"}}}, {"name": "alice-upper", "case_variant_of": "alice", "login": {"fields": {"user": "ALICE", "pass": "x"}}}]}`,
		"corpus.json":    `{"entries": [{"id": "001", "surface": "write", "method": "POST", "path": "/items", "probes": ["write_create"]}, {"id": "002", "surface": "write", "method": "POST", "path": "/items/edit", "steps": [{"path": "/items/edit", "fields": {"name": ""}}, {"path": "/items/edit", "fields": {"name": "ok"}}], "probes": ["error_then_corrected"]}, {"id": "003", "surface": "read", "path": "/Items", "probes": ["case_pair"]}, {"id": "004", "surface": "read", "path": "/items", "probes": ["case_pair"]}, {"id": "005", "surface": "read", "path": "/items?sort=name", "probes": ["text_sort"]}, {"id": "006", "surface": "read", "path": "/profile", "persona": "alice-upper"}]}`,
		"standard-mark":  "3\n",
		"canon/rules.py": "def canonicalize(entry, status, headers, body):\n    return status, headers, body\n",
	} {
		if err := os.WriteFile(filepath.Join(gm, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeMutant(t, filepath.Join(gm, "mutants", "v1-roundtrip"), "roundtrip_corruption", true)
	writeMutant(t, filepath.Join(gm, "mutants", "v2-create"), "create_lost", true)
	return ws
}

func runHarness(t *testing.T, ws string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	harness, err := filepath.Abs("golden-master/oracle-harness.py")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", harness)
	cmd.Dir = ws
	cmd.Env = append(os.Environ(), "GM_WORKSPACE="+ws)
	out, runErr := cmd.CombinedOutput()
	line := lastHarnessReport(string(out))
	if line == "" {
		t.Fatalf("the harness printed no JSON report (err=%v):\n%s", runErr, out)
	}
	return line
}

// THE bench, and the reason this file exists.
//
// A held-out directory that has lost its meta.json used to be dropped in
// SILENCE: `load_mutants` skipped it and the set simply got smaller. The gate
// then compared `detected == total` over what the loader had been willing to
// return, so a set of 7 amputated of 3 reports a green 4/4 — the vacuity trap
// of `holdout 0/0` with a different number, and the one the conjunction cannot
// see because both sides shrink together.
//
// It stayed invisible only because git does not lose a tracked file. Carrying
// the set anywhere less durable — a sealed directory, a fetched ref — makes it
// reachable, which is why this is fixed BEFORE the set moves.
func TestMalformedHeldOutMutantRefusesInsteadOfShrinkingTheSet(t *testing.T) {
	ws := mutantLoaderWorkspace(t)
	gm := filepath.Join(ws, ".golden-master")
	writeMutant(t, filepath.Join(gm, "mutants", "holdout", "h1-intact"), "create_lost", true)
	writeMutant(t, filepath.Join(gm, "mutants", "holdout", "h2-lost-its-meta"), "create_lost", false)

	line := runHarness(t, ws)

	// Both terms, so a coincidental mention of the name elsewhere in the report
	// cannot satisfy this: the refusal has to be ABOUT the missing meta.json.
	for _, want := range []string{"h2-lost-its-meta", "has no meta.json"} {
		if !strings.Contains(line, want) {
			t.Errorf("the report does not carry %q — a held-out directory without "+
				"meta.json was dropped in silence, and the figures below describe a "+
				"set of 1 where 2 were laid down:\n%s", want, line)
		}
	}
	var report struct {
		HoldoutDetected int `json:"holdout_detected"`
		HoldoutTotal    int `json:"holdout_total"`
	}
	if err := json.Unmarshal([]byte(line), &report); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, line)
	}
	if report.HoldoutDetected == report.HoldoutTotal {
		t.Errorf("the refused report carries holdout_detected == holdout_total (%d == %d), "+
			"a term the gate converges on: a refusal that reads green on one of the "+
			"gate's own terms is how it sails through", report.HoldoutDetected, report.HoldoutTotal)
	}
}

// The other half, and the one that would have broken production.
//
// `mutants/audit/` is a DIRECTORY under the mutant root with no meta.json —
// it is the pile the net publishes its spent sets into. Before this change it
// was excluded by ACCIDENT, riding the very silence being removed. Turning that
// silence into a refusal without naming the exclusion would have made every
// gate refuse on the net's own evidence.
//
// Measured on a live campaign at the time of the fix: 159 entries under
// mutants/, of which four *.md files and `audit` carry no meta.json.
func TestTheEvidencePileAndLooseFilesAreNotReadAsMalformedMutants(t *testing.T) {
	ws := mutantLoaderWorkspace(t)
	gm := filepath.Join(ws, ".golden-master")
	// The published pile: a directory, no meta.json, and a mutant INSIDE it.
	writeMutant(t, filepath.Join(gm, "mutants", "audit", "01a0-previous", "z1-spent"), "create_lost", true)
	// Loose notes at the mutant root — files, not directories.
	for _, name := range []string{"X-LENS.md", "ADVERSARIAL-NOTES.md"} {
		if err := os.WriteFile(filepath.Join(gm, "mutants", name), []byte("# notes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeMutant(t, filepath.Join(gm, "mutants", "holdout", "h1-intact"), "create_lost", true)

	line := runHarness(t, ws)

	for _, forbidden := range []string{"has no meta.json", "MALFORMED mutant"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("the gate refused over a structural directory or a loose note — "+
				"mutants/audit/ and *.md files are not mutants, and excluding them must "+
				"be deliberate rather than a side effect of the silent skip (%q):\n%s",
				forbidden, line)
		}
	}
}
