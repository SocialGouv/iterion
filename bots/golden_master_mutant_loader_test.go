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
//
// The body embeds the directory name so every mutant hashes DIFFERENTLY.
// Sharing one body makes the held-out set collide by fingerprint with anything
// written into mutants/audit/, and the run then stops on `holdout_reused`
// before reaching what these benches are about — which is how the audit-pile
// guard went vacuously green until a positive assertion caught it.
func writeMutant(t *testing.T, dir, archetype string, withMeta bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\n# " + filepath.Base(dir) + "\ntrue\n"
	if err := os.WriteFile(filepath.Join(dir, "apply.sh"), []byte(body), 0o755); err != nil {
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
	return runHarnessEnv(t, ws)
}

func runHarnessEnv(t *testing.T, ws string, extra ...string) string {
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
	// GM_SEALED_DIR, or `sealed_dir_for` falls back to the system temp dir and
	// `seal_holdout` MOVES mutants/holdout/* out of t.TempDir() into
	// /tmp/gm-holdout-<basename>-<sha10>/ — outside anything Go cleans up, and
	// named after the absolute workspace path, so every run leaves a new pile.
	cmd.Env = append(append(os.Environ(),
		"GM_WORKSPACE="+ws,
		"GM_SEALED_DIR="+filepath.Join(ws, "sealed")), extra...)
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

	for _, forbidden := range []string{"has no meta.json", "unreadable meta.json"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("the gate refused over a structural directory or a loose note — "+
				"mutants/audit/ and *.md files are not mutants, and excluding them must "+
				"be deliberate rather than a side effect of the silent skip (%q):\n%s",
				forbidden, line)
		}
	}
	// Absence alone would go VACUOUSLY green: any bail BEFORE the mutant loader
	// also produces a report without those strings, and the guard would pass
	// while testing nothing — the same both-sides-shrink vacuity this file
	// exists to close. So pin that the run actually got PAST the loader.
	//
	// This fixture has no application to boot, so it stops at the first refusal
	// that follows the loader: the missing `routes_probe`. Pinning it couples
	// this test to that ORDER on purpose — if a future refusal lands earlier,
	// this must go red and be re-pointed at whatever now follows the loader,
	// rather than quietly stop proving anything.
	var report struct {
		LogTail string `json:"log_tail"`
	}
	if err := json.Unmarshal([]byte(line), &report); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, line)
	}
	if !strings.Contains(report.LogTail, "routes_probe") {
		t.Fatalf("the run did not reach the refusal that FOLLOWS the mutant loader, so this "+
			"guard proves nothing about mutants/audit/: it stopped on %q instead. Re-point "+
			"this assertion at whatever now sits just after the loader:\n%s",
			report.LogTail, line)
	}
}

// THE regression the first round of this PR introduced, and the reason a
// refusal has to fail EVERY term a consumer converges on — not the one that was
// on the author's mind.
//
// `bail()` prints the DEFAULT report, in which `invalid` and `missing` do not
// exist: both are only set inside the `MODE=validate` arm, which this path never
// reaches. `reanchor.bot` computes
// `all_valid = not verdict.get("invalid") and not verdict.get("missing")` and
// converges its gate on it — so a LOADER refusal read as "mechanically valid",
// declaring sound a set of mutants the harness never read. Before the refusal
// existed, that case surfaced as `missing`.
//
// Same shape as `holdout_detected == holdout_total`, one gate further out.
func TestAMalformedMutantDoesNotReadValidInValidateMode(t *testing.T) {
	ws := mutantLoaderWorkspace(t)
	gm := filepath.Join(ws, ".golden-master")
	writeMutant(t, filepath.Join(gm, "mutants", "v3-lost-its-meta"), "create_lost", false)

	line := runHarnessEnv(t, ws, "GM_MODE=validate")

	var report struct {
		Invalid []struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		} `json:"invalid"`
		Missing []string `json:"missing"`
	}
	if err := json.Unmarshal([]byte(line), &report); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, line)
	}
	// reanchor.bot's exact predicate.
	allValid := len(report.Invalid) == 0 && len(report.Missing) == 0
	if allValid {
		t.Fatalf("a loader refusal reads all_valid — reanchor.bot converges on "+
			"`not invalid and not missing`, so this declares mechanically sound a set "+
			"the harness never loaded:\n%s", line)
	}
	if len(report.Invalid) == 0 || report.Invalid[0].ID != "v3-lost-its-meta" {
		t.Errorf("the refused report must name the offending mutant in `invalid`, "+
			"otherwise the consumer has a false term and no diagnosis:\n%s", line)
	}
}

// The third arm of the same event: a meta.json that EXISTS but will not parse.
//
// Left to propagate it is a json.JSONDecodeError no caller catches, and main()
// has no wrapper — the harness dies on exit 1 having printed NO report at all,
// which leaves the gate's consumer the bare exit code the named refusal exists
// to replace. Truncated writes and half-materialised fetches produce exactly
// this, and they are the failure mode the successor-on-a-ref design makes
// reachable.
func TestCorruptHeldOutMetaRefusesByNameInsteadOfKillingTheHarness(t *testing.T) {
	ws := mutantLoaderWorkspace(t)
	gm := filepath.Join(ws, ".golden-master")
	writeMutant(t, filepath.Join(gm, "mutants", "holdout", "h1-intact"), "create_lost", true)
	writeMutant(t, filepath.Join(gm, "mutants", "holdout", "h2-truncated"), "create_lost", true)
	// A meta cut mid-write: valid prefix, no closing brace.
	if err := os.WriteFile(
		filepath.Join(gm, "mutants", "holdout", "h2-truncated", "meta.json"),
		[]byte(`{"surface": "write", "archetype": "create_`), 0o644); err != nil {
		t.Fatal(err)
	}

	line := runHarness(t, ws)

	for _, want := range []string{"h2-truncated", "unreadable meta.json"} {
		if !strings.Contains(line, want) {
			t.Errorf("the report does not carry %q — a truncated meta.json must refuse BY NAME, "+
				"not kill the harness with a traceback and no report:\n%s", want, line)
		}
	}
}

// The spellings the first fix missed, and each one restores the exact outcome
// the refusal exists to remove: a traceback and NO report.
//
//   - valid JSON that is not an OBJECT dies one line later on
//     `meta["id"] = …` with a TypeError;
//   - a meta that cannot be OPENED raises OSError between the isfile check and
//     the read — the "half-materialised by a partial fetch" case the docstring
//     itself names.
//
// Catching two decode spellings and calling it done is the enumeration trap:
// the fix is `(ValueError, OSError)` plus a type check, not a longer list.
func TestEveryUnreadableMetaSpellingRefusesByName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, dir string)
	}{
		{"json-non-objet", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`["not", "an", "object"]`), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"illisible", func(t *testing.T, dir string) {
			if os.Geteuid() == 0 {
				t.Skip("root ignore les permissions de fichier")
			}
			if err := os.Chmod(filepath.Join(dir, "meta.json"), 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "meta.json"), 0o644) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := mutantLoaderWorkspace(t)
			gm := filepath.Join(ws, ".golden-master")
			dir := filepath.Join(gm, "mutants", "holdout", "h9-"+tc.name)
			writeMutant(t, dir, "create_lost", true)
			tc.write(t, dir)

			line := runHarness(t, ws)

			if !strings.Contains(line, "h9-"+tc.name) {
				t.Errorf("the report does not name the offending mutant — this spelling still "+
					"kills the harness with a traceback and no report:\n%s", line)
			}
		})
	}
}
