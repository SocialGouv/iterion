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

// The deep scanner writes its findings to a file inside the sandbox, and the
// pod is destroyed with the run. A pass that dies AFTER it — at triage, at the
// jury, on a provider usage cap — takes the whole contribution with it, and
// the published envelope carries only the PATH.
//
// Measured 2026-09-10: 88 minutes of investigation lost that way, twice in one
// day, the second time on a session limit hit three batches from the end.
//
// bank_deepsec_findings carries the findings themselves out of the pod. These
// tests run the REAL command body against fixture exports, because the
// property is what the node produces on each shape of input.
func bankScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, blk := range commandBackticks(string(raw)) {
		if !strings.Contains(blk, "no deepsec export at ") {
			continue
		}
		const marker = "python3 -c '"
		i := strings.Index(blk, marker)
		if i < 0 {
			t.Fatal("the banking command embeds no python3 -c body")
		}
		start := i + len(marker)
		end := strings.Index(blk[start:], "'")
		if end < 0 {
			t.Fatal("unterminated python3 -c body in the banking command")
		}
		return blk[start : start+end]
	}
	t.Fatal("no bank_deepsec_findings command found — the findings no longer leave the pod")
	return ""
}

type bankOut struct {
	Findings  []map[string]any `json:"findings"`
	Embedded  int              `json:"embedded"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated"`
	Note      string           `json:"note"`
}

func TestDeepsecFindingsLeaveThePod(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	script := bankScript(t)
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "bank.py")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	// Forty findings, each large enough that a small budget must cut.
	var items []map[string]any
	for i := 0; i < 40; i++ {
		items = append(items, map[string]any{"id": i, "body": strings.Repeat("x", 200)})
	}
	full := filepath.Join(dir, "full.json")
	writeJSON(t, full, items)
	empty := filepath.Join(dir, "empty.json")
	writeJSON(t, empty, []map[string]any{})
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, out, budget string) bankOut {
		t.Helper()
		cmd := exec.Command("python3", scriptPath)
		cmd.Env = append(os.Environ(), "DEEPSEC_OUT="+out, "MAX_BYTES="+budget)
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("the banking node exited non-zero (%v) — it must always emit its envelope: %q", err, raw)
		}
		var got bankOut
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("output is not the declared envelope: %v (%q)", err, raw)
		}
		return got
	}

	t.Run("a whole export is banked", func(t *testing.T) {
		got := run(t, full, "524288")
		if got.Embedded != 40 || got.Total != 40 || got.Truncated {
			t.Errorf("embedded=%d total=%d truncated=%v, want 40/40 and no truncation", got.Embedded, got.Total, got.Truncated)
		}
		if len(got.Findings) != 40 {
			t.Errorf("the findings themselves did not travel: %d carried", len(got.Findings))
		}
	})

	t.Run("truncation drops WHOLE findings and says so", func(t *testing.T) {
		// A partial bank still has to parse, and the gap has to be a
		// subtraction rather than a guess — silence here would be the same
		// class of loss the node exists to end.
		got := run(t, full, "2000")
		if !got.Truncated {
			t.Error("a bank cut by the budget did not report truncated")
		}
		if got.Total != 40 {
			t.Errorf("total=%d, want the FULL count so the gap is computable", got.Total)
		}
		if got.Embedded == 0 || got.Embedded >= got.Total {
			t.Errorf("embedded=%d is not a partial bank of %d", got.Embedded, got.Total)
		}
		if len(got.Findings) != got.Embedded {
			t.Errorf("embedded=%d but %d findings carried — the count must describe the payload", got.Embedded, len(got.Findings))
		}
	})

	t.Run("an absent export is reported, not failed", func(t *testing.T) {
		got := run(t, filepath.Join(dir, "nope.json"), "524288")
		if got.Total != 0 || got.Truncated {
			t.Errorf("got %+v, want an empty bank", got)
		}
		if !strings.Contains(got.Note, "no deepsec export") {
			t.Errorf("the note does not say the export was absent: %q", got.Note)
		}
	})

	t.Run("an unreadable export names the failure", func(t *testing.T) {
		got := run(t, broken, "524288")
		if !strings.Contains(got.Note, "could not be read") {
			t.Errorf("the note does not name the read failure: %q", got.Note)
		}
	})

	// A valid JSON export need not be a CONTAINER. null, a bare number, a bare
	// string and a bool all parse, and calling .get on one raises AttributeError
	// OUTSIDE the read guard — the node then exited non-zero with empty stdout,
	// and because the bank sits on the control path into scan_join it took the
	// generic/lang/custom scanner results down with it. The node promises the
	// opposite two lines above its body: always exit 0, always emit the envelope.
	t.Run("a JSON export that is not a findings container is banked empty, not crashed", func(t *testing.T) {
		for _, shape := range []string{"null", "5", `"oops"`, "true", `{"findings": "nope"}`, `{"findings": {"a": 1}}`} {
			t.Run(shape, func(t *testing.T) {
				path := filepath.Join(dir, "shape.json")
				if err := os.WriteFile(path, []byte(shape), 0o644); err != nil {
					t.Fatal(err)
				}
				got := run(t, path, "524288")
				if got.Embedded != 0 || got.Total != 0 || got.Truncated || len(got.Findings) != 0 {
					t.Errorf("got %+v, want an empty bank", got)
				}
				// "banked 0 of 0" would read as a clean scan. An export that is
				// not a scan result has to say so, or the bank reproduces the
				// facade the export_unusable guard exists to refuse.
				if !strings.Contains(got.Note, "nothing to bank") {
					t.Errorf("the note does not name the unusable shape: %q", got.Note)
				}
			})
		}
	})

	t.Run("an empty export is not an error", func(t *testing.T) {
		got := run(t, empty, "524288")
		if got.Total != 0 || got.Truncated || strings.Contains(got.Note, "could not") {
			t.Errorf("a clean scan with no findings must bank quietly, got %+v", got)
		}
	})
}

// TestBankSitsBesideTheTriagePath pins the wiring. The findings must NOT be
// copied into the triage prompt: json_paths exists precisely so triage reads
// the file itself, and an embedded copy would balloon its context. So the bank
// sits between the scanner and scan_join, while scan_join keeps referencing
// the SCANNER — not the bank.
func TestBankSitsBesideTheTriagePath(t *testing.T) {
	wf := compilePlanPhaseBot(t, "sec-audit-source")
	if _, ok := wf.Nodes["bank_deepsec_findings"]; !ok {
		t.Fatal("no bank_deepsec_findings node — a pass that dies after the scanner loses its whole contribution")
	}
	join, ok := wf.Nodes["scan_join"].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("scan_join is %T, want *ir.ComputeNode", wf.Nodes["scan_join"])
	}
	sawDeepsecScan := false
	for _, ex := range join.Exprs {
		if ex.Key != "deepsec_scan" {
			continue
		}
		sawDeepsecScan = true
		if strings.Contains(ex.Raw, "bank_deepsec_findings") {
			t.Errorf("scan_join reads the BANK (%s) — the banked findings would ride into the triage prompt, "+
				"which is what json_paths exists to avoid", ex.Raw)
		}
		if !strings.Contains(ex.Raw, "run_deepsec_scanner") {
			t.Errorf("scan_join no longer reads the scanner envelope: %s", ex.Raw)
		}
	}
	// Both assertions above live inside the key filter. Rename or drop
	// deepsec_scan and the loop body never runs, the test goes green, and it
	// pins nothing — the same silent failure it exists to prevent.
	if !sawDeepsecScan {
		t.Fatal("scan_join has no deepsec_scan expr — this test now asserts nothing; re-anchor it on the current key")
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
