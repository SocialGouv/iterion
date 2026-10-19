package bots

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #2449: the engine's completeness check holds per-page cells open — a
// criterion the trial pruned can leave a page cell « à évaluer » while the
// DOCUMENT decides the criterion — and final_report used to read only the
// global undecided list: complete=false, undecided=0, nothing named. The
// annex now carries a « Grille par page incomplète » section and the
// GRID_INCOMPLETE refusal names the open cells. Driven against the real
// engine on the fixture site with the committed captures
// (testdata/rgaa-audit-site-captures) copied in at audit time — the engine
// registers the sample pages from their presence, no browser needed.
func TestRgaaAuditFinalReportNamesOpenPageCells(t *testing.T) {
	bin, _ := rgaaResolveEngine(t)
	scripts := rgaaBotScripts(t)
	site := rgaaCopyFixtureSite(t)
	pages, err := filepath.Abs(filepath.Join("testdata", "rgaa-audit-site-captures", "pages"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(site, ".ultra11y", "pages"), os.DirFS(pages)); err != nil {
		t.Fatal(err)
	}
	std := []string{"--standard", "rgaa", "--lang", "fr"}
	run := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(filepath.Join(run, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(run, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	// prepare's two artifacts the final_report node reads through lib.js.
	lib := rgaaRenderScript(t, "prepare", extractHelpers(t, scripts["prepare"]), map[string]any{
		"input.run_dir": run, "input.workspace_dir": site, "input.standard": "rgaa", "run.id": "page-grid-test",
	})
	if !strings.HasPrefix(lib, "function helpers() {") || !strings.HasSuffix(strings.TrimSpace(lib), "}") {
		t.Fatalf("the extracted helpers are not one closed function:\n%s", lib[:min(len(lib), 200)])
	}
	if err := os.WriteFile(filepath.Join(run, "bin", "lib.js"), []byte("module.exports = ("+lib+")();\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pin := rgaaReadPin(t)
	rgaaWriteJSON(t, filepath.Join(run, "bin", "engine.json"), map[string]any{
		"entry": bin, "size": pin.Size, "sha256": pin.SHA256,
	})
	if code, _, stderr := rgaaEngineRun(t, bin, site, nil, append([]string{"audit", ".", "--graph", "--jsx", "--out", run, "--json"}, std...)...); code != 0 {
		t.Fatalf("audit exit %d: %s", code, stderr)
	}
	// checkAudit: the audit records its own hash at RUN/state/audit.json.
	auditRaw := []byte(mustRead(t, filepath.Join(run, "audit-latest.json")))
	sum := fmt.Sprintf("%x", sha256.Sum256(auditRaw))
	rgaaWriteJSON(t, filepath.Join(run, "state", "audit.json"), map[string]any{"sha256": sum, "at": "test"})
	var worklist worklistOut
	rgaaEngineJSON(t, bin, site, nil, &worklist, append([]string{"verify", "--manual", "--in", filepath.Join(run, "audit-latest.json"), "--out", run, "--no-web", "--json"}, std...)...)
	var todo struct {
		Items []struct {
			CriteriaID string           `json:"criteriaId"`
			Evidence   []map[string]any `json:"evidence"`
		} `json:"items"`
	}
	rgaaReadJSON(t, worklist.TodoPath, &todo)
	var items []any
	for _, it := range todo.Items {
		if it.CriteriaID == "1.1" {
			// Left unanswered: the criterion stays « à évaluer », and the
			// engine holds its page cells open (RGAA 1.1 is audited on both
			// fixture pages).
			items = append(items, map[string]any{"criteriaId": it.CriteriaID, "verdict": nil})
			continue
		}
		if len(it.Evidence) == 0 {
			items = append(items, map[string]any{"criteriaId": it.CriteriaID, "verdict": "NA", "justification": "Aucun sujet de ce type dans le périmètre audité (fixture)."})
			continue
		}
		e := it.Evidence[0]
		items = append(items, map[string]any{"criteriaId": it.CriteriaID, "verdict": "C", "justification": "Conforme sur l'évidence citée (fixture).", "citations": []any{map[string]any{"file": e["file"], "line": e["line"], "snippet": e["snippet"]}}})
	}
	var skeleton map[string]any
	rgaaReadJSON(t, worklist.VerdictsPath, &skeleton)
	skeleton["items"] = items
	adj := filepath.Join(run, "adj.json")
	rgaaWriteJSON(t, adj, skeleton)
	var f foldOut
	rgaaEngineJSON(t, bin, site, nil, &f, append([]string{"verify", "--apply", adj, "--in", filepath.Join(run, "audit-latest.json"), "--out", run, "--json"}, std...)...)
	if len(f.RejectedCriteria) != 1 || !rgaaContains(f.RejectedCriteria, "1.1") {
		t.Fatalf("the fold refused %v, want exactly 1.1 (left unanswered on purpose)", f.RejectedCriteria)
	}
	// The fold rewrote the audit: re-record its hash, as the bot's
	// commitAudit would (checkAudit compares against this record).
	foldedRaw := []byte(mustRead(t, filepath.Join(run, "audit-latest.json")))
	rgaaWriteJSON(t, filepath.Join(run, "state", "audit.json"), map[string]any{"sha256": fmt.Sprintf("%x", sha256.Sum256(foldedRaw)), "at": "test"})
	env := []string{"ULTRA11Y_BIN=" + bin}
	values := map[string]any{
		"input.run_dir": run, "input.workspace_dir": site, "input.standard": "rgaa", "input.html_report": false,
		"input.report_dir": "", "input.require_complete": false, "input.undecidable_present": false,
		"input.undecidable_path": "", "input.scope_notes": "", "run.id": "page-grid-test",
	}
	out := rgaaOK(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, values))
	annex, err := os.ReadFile(fmt.Sprint(out["annex_path"]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(annex), "## Grille par page incomplète") ||
		!strings.Contains(string(annex), "- Accueil : 1.1") || !strings.Contains(string(annex), "- Contact : 1.1") {
		t.Fatalf("the annex does not name the open page cells:\n%s", annex)
	}
	// require_complete: the refusal names the cells, not just « 0 criteria ».
	values["input.require_complete"] = true
	c := rgaaRunCopy(t, run)
	values["input.run_dir"] = c
	res := rgaaRunNode(t, scripts, "final_report", site, env, values)
	if res.Code != 0 {
		t.Fatalf("final_report died under require_complete:\n%s", res.Stderr)
	}
	if ok, _ := res.Out["ok"].(bool); ok || res.Out["fail_code"] != "GRID_INCOMPLETE" {
		t.Fatalf("final_report answered %v / %v, want the GRID_INCOMPLETE refusal", res.Out["ok"], res.Out["fail_code"])
	}
	if reason := fmt.Sprint(res.Out["reason"]); !strings.Contains(reason, "Accueil (1.1)") || !strings.Contains(reason, "Contact (1.1)") {
		t.Fatalf("the GRID_INCOMPLETE reason does not name the open page cells:\n%s", reason)
	}
}

// extractHelpers cuts the prepare script at its helpers() function: the
// runtime renders every template in a script body, and the test only needs
// the helper closure — rendering the whole body would demand launch values
// no node-level test carries.
func extractHelpers(t *testing.T, prepareScript string) string {
	t.Helper()
	const fn = "function helpers() {"
	at := strings.Index(prepareScript, fn)
	if at < 0 {
		t.Fatal("prepare carries no helpers function")
	}
	lines := strings.Split(prepareScript[at:], "\n")
	for end, line := range lines {
		if line == "}" {
			return strings.Join(lines[:end+1], "\n")
		}
	}
	t.Fatal("the helpers function never closes")
	return ""
}

var _ = exec.Command // keep os/exec imported for callers that shell out
