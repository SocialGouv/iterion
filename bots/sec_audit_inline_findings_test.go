package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// triage reads the scanner findings through json_paths — it opens the files
// the tool nodes wrote. That works only where triage shares a filesystem with
// those nodes, which is a sandbox-routed backend: `claw` is in-process and
// never enters the sandbox, and `codex` refuses to run inside one at all. On
// either, every path is unopenable and the node renders an EMPTY triage, which
// downstream reads as a clean repository.
//
// vars.triage_inline_max_bytes carries the capped findings in the envelope
// instead, under a byte budget. These tests run the REAL cap_findings body
// against fixture scan dirs: the property is what the node emits per budget.

func capFindingsScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, blk := range commandBackticks(string(raw)) {
		if !strings.Contains(blk, "INLINE_MAX=") {
			continue
		}
		const marker = "python3 -c \""
		i := strings.Index(blk, marker)
		if i < 0 {
			t.Fatal("the capping command embeds no python3 -c body")
		}
		start := i + len(marker)
		end := strings.LastIndex(blk, "\"")
		if end <= start {
			t.Fatal("unterminated python3 -c body in the capping command")
		}
		return blk[start:end]
	}
	t.Fatal("cap_findings no longer takes INLINE_MAX: the findings cannot travel in the envelope, so triage is pinned to a sandbox-routed backend")
	return ""
}

type capOut struct {
	TotalRaw  int `json:"total_raw"`
	TotalKept int `json:"total_kept"`
	Inline    []struct {
		File     string           `json:"file"`
		Array    string           `json:"array"`
		Findings []map[string]any `json:"findings"`
	} `json:"inline"`
	InlineEmbedded  int  `json:"inline_embedded"`
	InlineTotal     int  `json:"inline_total"`
	InlineTruncated bool `json:"inline_truncated"`
}

// capFixture builds a scan dir holding two scanner files, and returns a runner
// that executes the shipped body against it at a given inline budget.
func capFixture(t *testing.T) func(budget string) capOut {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "cap.py")
	if err := os.WriteFile(scriptPath, []byte(capFindingsScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	scanDir := filepath.Join(dir, "scan")
	if err := os.MkdirAll(scanDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mk := func(n int, sev string) []map[string]any {
		var out []map[string]any
		for i := 0; i < n; i++ {
			out = append(out, map[string]any{
				"check_id": "rule-" + sev,
				"severity": sev,
				"path":     "src/file.go",
				"message":  strings.Repeat("y", 200),
			})
		}
		return out
	}
	writeJSON(t, filepath.Join(scanDir, "semgrep.json"), map[string]any{"results": mk(12, "high")})
	writeJSON(t, filepath.Join(scanDir, "trivy.json"), map[string]any{"Issues": mk(8, "medium")})

	return func(budget string) capOut {
		t.Helper()
		// The body rewrites the scanner files in place, so each run starts
		// from a fresh copy — otherwise the second run caps already-capped
		// input and the counts drift for a reason the test does not control.
		fresh := t.TempDir()
		for _, name := range []string{"semgrep.json", "trivy.json"} {
			b, err := os.ReadFile(filepath.Join(scanDir, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fresh, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("python3", scriptPath)
		cmd.Env = append(os.Environ(), "SCAN_DIR="+fresh, "CAP=50", "INLINE_MAX="+budget)
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("cap_findings exited non-zero (%v): %q", err, raw)
		}
		var got capOut
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("output is not the declared envelope: %v (%q)", err, raw)
		}
		return got
	}
}

// The default must not change what any existing run sends to triage: a budget
// of 0 means "read the files", and the envelope carries no payload.
func TestInlineFindings_OffByDefault(t *testing.T) {
	got := capFixture(t)("0")
	if len(got.Inline) != 0 || got.InlineEmbedded != 0 {
		t.Fatalf("with no budget the findings must NOT ride the envelope (it would land whole in every triage prompt), got %d group(s)/%d finding(s)",
			len(got.Inline), got.InlineEmbedded)
	}
	if got.InlineTruncated {
		t.Fatal("nothing was carried, so nothing was truncated")
	}
	if got.TotalKept != 20 {
		t.Fatalf("the capping itself must be unchanged: want 20 kept, got %d", got.TotalKept)
	}
}

// With room, every capped finding travels — that is what makes triage
// independent of the backend it runs on.
func TestInlineFindings_CarryEverythingWhenTheBudgetAllows(t *testing.T) {
	got := capFixture(t)("524288")
	if got.InlineEmbedded != got.TotalKept {
		t.Fatalf("a generous budget must carry every kept finding, got %d of %d", got.InlineEmbedded, got.TotalKept)
	}
	if got.InlineTruncated {
		t.Fatal("nothing was dropped, so truncated must be false")
	}
	if len(got.Inline) != 2 {
		t.Fatalf("both scanner files must be represented, got %d", len(got.Inline))
	}
	for _, grp := range got.Inline {
		for _, f := range grp.Findings {
			if _, ok := f["severity"]; !ok {
				t.Fatalf("a carried finding lost its fields: %v", f)
			}
		}
	}
}

// A cut drops WHOLE findings and reports the gap as a subtraction, so a
// partial payload still parses and the loss is never silent.
func TestInlineFindings_ACutDropsWholeFindingsAndSaysSo(t *testing.T) {
	got := capFixture(t)("900")
	if got.InlineEmbedded == 0 {
		t.Fatal("a small budget still carries what fits — an empty payload would read as a clean repository")
	}
	if got.InlineEmbedded >= got.TotalKept {
		t.Fatalf("a 900-byte budget cannot hold %d findings of ~200 bytes each; got %d carried", got.TotalKept, got.InlineEmbedded)
	}
	if !got.InlineTruncated {
		t.Fatal("findings were dropped and the envelope did not say so — the signal loss is exactly what must stay visible")
	}
	if got.InlineTotal != got.TotalKept {
		t.Fatalf("the gap must be a subtraction: inline_total %d should equal total_kept %d", got.InlineTotal, got.TotalKept)
	}
	// Whole objects only: every carried finding still has all its fields.
	for _, grp := range got.Inline {
		for _, f := range grp.Findings {
			if _, ok := f["message"]; !ok {
				t.Fatalf("a finding was cut mid-object, so the payload no longer parses as findings: %v", f)
			}
		}
	}
}

// The triage prompt has to KNOW about the transport, or the envelope travels
// and nobody reads it.
func TestInlineFindings_TriagePromptNamesTheTransport(t *testing.T) {
	raw, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "findings_budget.inline") {
		t.Fatal("no prompt mentions findings_budget.inline: cap_findings would carry the findings and triage would still open json_paths it cannot read")
	}
}
