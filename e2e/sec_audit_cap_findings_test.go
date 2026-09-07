package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestSecAuditSource_CapFindings_BoundsScannerOutput is the regression test
// for the large-repo context-overflow fix. gosec/semgrep can emit thousands
// of findings on an iterion-sized repo; triage reads the scanner JSON via its
// tools and overflows the model context. The deterministic cap_findings tool
// must rewrite each scanner JSON in place to a bounded, severity-sorted,
// bulk-trimmed set — keeping the scanner's native shape so triage still parses
// it — and emit an explicit truncation budget so report_card can surface it.
//
// This runs the ACTUAL command embedded in the bot's cap_findings node, so a
// regression in the cap logic (or its removal) fails here.
func TestSecAuditSource_CapFindings_BoundsScannerOutput(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not in PATH — skipping cap_findings regression test")
	}

	wf := compileFixture(t, "sec-audit-source/main.bot")
	node, ok := wf.Nodes["cap_findings"]
	if !ok {
		t.Fatal("workflow missing cap_findings node (overflow guard removed?)")
	}
	tool, ok := node.(*ir.ToolNode)
	if !ok {
		t.Fatalf("cap_findings is not a ToolNode (got %T)", node)
	}

	scanDir := t.TempDir()
	// gosec shape: 200 Issues, each carrying a bulky `code` field.
	capWriteJSON(t, filepath.Join(scanDir, "gosec.json"), map[string]any{"Issues": capMakeFindings(200, "Issues")})
	// semgrep shape: 300 results, each with bulky extra.lines / extra.metadata.
	capWriteJSON(t, filepath.Join(scanDir, "go-semgrep.json"), map[string]any{"results": capMakeFindings(300, "results")})
	// a non-findings scanner artifact: must be left untouched.
	capWriteJSON(t, filepath.Join(scanDir, "shards.json"), map[string]any{"shard_count": 0, "shards": []any{}})

	before := capDirSize(t, scanDir)

	cmd := tool.Command
	cmd = strings.ReplaceAll(cmd, "{{vars.scan_dir}}", scanDir)
	cmd = strings.ReplaceAll(cmd, "{{vars.findings_cap_per_file}}", "50")
	out, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		t.Fatalf("cap_findings command failed: %v", err)
	}

	var budget struct {
		TotalRaw    int  `json:"total_raw"`
		TotalKept   int  `json:"total_kept"`
		TotalCapped int  `json:"total_capped"`
		PerFileCap  int  `json:"per_file_cap"`
		Truncated   bool `json:"truncated"`
	}
	if err := json.Unmarshal(out, &budget); err != nil {
		t.Fatalf("budget JSON: %v\nout: %s", err, out)
	}
	if budget.TotalRaw != 500 || budget.TotalKept != 100 || budget.TotalCapped != 400 || !budget.Truncated {
		t.Errorf("budget = %+v, want raw=500 kept=100 capped=400 truncated=true", budget)
	}

	gosec := capReadJSON(t, filepath.Join(scanDir, "gosec.json"))
	issues, _ := gosec["Issues"].([]any)
	if len(issues) != 50 {
		t.Errorf("gosec Issues capped to %d, want 50", len(issues))
	}
	if _, marked := gosec["_iterion_truncated"]; !marked {
		t.Error("gosec.json missing _iterion_truncated marker after capping")
	}
	if len(issues) > 0 {
		if _, hasCode := issues[0].(map[string]any)["code"]; hasCode {
			t.Error("bulky `code` field not trimmed from kept gosec finding")
		}
		if sev, _ := issues[0].(map[string]any)["severity"].(string); sev != "HIGH" {
			t.Errorf("first kept gosec finding severity = %q, want HIGH (severity-sorted)", sev)
		}
	}

	shards := capReadJSON(t, filepath.Join(scanDir, "shards.json"))
	if _, marked := shards["_iterion_truncated"]; marked {
		t.Error("shards.json (a non-findings artifact) was wrongly capped")
	}

	after := capDirSize(t, scanDir)
	if after >= before/10 {
		t.Errorf("scan dir only shrank %d->%d bytes; expected >=10x reduction (this IS the overflow fix)", before, after)
	}
}

func capMakeFindings(n int, key string) []any {
	sevs := []string{"LOW", "MEDIUM", "HIGH"}
	out := make([]any, n)
	for i := 0; i < n; i++ {
		if key == "Issues" { // gosec
			out[i] = map[string]any{
				"rule_id": fmt.Sprintf("G%03d", i), "severity": sevs[i%3],
				"file": fmt.Sprintf("pkg/x%d.go", i), "line": fmt.Sprintf("%d", i),
				"details": fmt.Sprintf("issue %d", i), "code": strings.Repeat("X", 4000),
			}
		} else { // semgrep results
			out[i] = map[string]any{
				"check_id": fmt.Sprintf("r%d", i), "path": fmt.Sprintf("pkg/y%d.go", i),
				"start": map[string]any{"line": i},
				"extra": map[string]any{
					"severity": sevs[i%3], "message": "m",
					"lines": strings.Repeat("CODE", 2000), "metadata": map[string]any{"big": strings.Repeat("Z", 5000)},
				},
			}
		}
	}
	return out
}

func capWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func capReadJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return m
}

func capDirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil {
			total += info.Size()
		}
	}
	return total
}
