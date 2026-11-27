package bots

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The criteria no agent can decide are never bought: a criterion the engine
// says only a rendered page settles, on an audit that read no page, with no
// evidence harvested from the source, is triaged out of the worklist at
// zero tokens; an asked criterion with no evidence to cite is not re-asked;
// both stay « à évaluer » in the audit (no verdict the bot would have to
// justify) and the coverage annex lists them as points to re-check, with why.
func TestRgaaAuditTriagesWhatNoAgentCanDecide(t *testing.T) {
	bin, pin := rgaaResolveEngine(t)
	scripts := rgaaBotScripts(t)
	// A component and nothing else: no HTML document, no image, no page.
	site := filepath.Join(t.TempDir(), "site")
	if err := os.MkdirAll(filepath.Join(site, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "src", "Notice.tsx"), []byte("export function Notice() {\n  return <p className=\"notice\">Bienvenue</p>;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "scratch")
	const runID = "triage-run"
	run := filepath.Join(base, runID)
	env := []string{"ULTRA11Y_BIN=" + bin}
	in := func(extra map[string]any) map[string]any {
		v := map[string]any{"input.run_dir": run, "input.workspace_dir": site, "input.standard": "rgaa", "run.id": runID, "input.wave_batches": 0, "input.max_reask": 1, "input.batch_size": 8}
		maps.Copy(v, extra)
		return v
	}
	prep := rgaaOK(t, "prepare", rgaaRunNode(t, scripts, "prepare", site, env, map[string]any{
		"input.run_dir": base, "input.workspace_dir": site, "input.standard": "rgaa", "run.id": runID,
		"input.engine_version": pin.Version, "input.engine_sha256": pin.SHA256, "input.engine_size": fmt.Sprint(pin.Size),
		"input.scope_globs": "", "input.exclude_globs": "", "input.max_files": "4000",
		"input.force_jsx": "true", "input.rendered": "off", "input.browser_path": "", "input.ledger_path": "", "input.undecidable_path": "", "input.report_dir": "",
	}))
	rgaaOK(t, "static_audit", rgaaRunNode(t, scripts, "static_audit", site, env, in(map[string]any{"input.max_files": 4000, "input.audit_args": prep["audit_args"]})))
	wl := rgaaOK(t, "worklist", rgaaRunNode(t, scripts, "worklist", site, env, in(nil)))

	recheck := rgaaStrings(wl["recheck_ids"])
	pending := rgaaStrings(wl["pending_ids"])
	if len(recheck) == 0 {
		t.Fatalf("no criterion triaged out on a site with no page and no image: %v", wl)
	}
	if rgaaInt(wl["asked"]) != len(pending) || len(pending)+len(recheck) != rgaaInt(wl["count"]) {
		t.Fatalf("asked %v, pending %d, triaged %d, count %v: the worklist must split exactly into the two", wl["asked"], len(pending), len(recheck), wl["count"])
	}
	triaged := map[string]bool{}
	for _, id := range recheck {
		triaged[id] = true
		var item struct {
			Automatability string           `json:"automatability"`
			Rendered       bool             `json:"needsRenderedEvidence"`
			Evidence       []map[string]any `json:"evidence"`
		}
		rgaaReadJSON(t, filepath.Join(fmt.Sprint(wl["items_dir"]), id+".json"), &item)
		if (item.Automatability != "needs-rendering" && !item.Rendered) || len(item.Evidence) != 0 {
			t.Fatalf("criterion %s triaged out but it is not render-only with an empty harvest: %+v", id, item)
		}
	}
	for _, id := range pending {
		if triaged[id] {
			t.Fatalf("criterion %s is both triaged and pending", id)
		}
	}
	var adj struct {
		Items []any `json:"items"`
	}
	rgaaReadJSON(t, filepath.Join(run, "adjudication.json"), &adj)
	if adj.Items == nil || len(adj.Items) != 0 {
		t.Fatalf("the worklist must leave an empty adjudication for a fold with nothing dispatched: %+v", adj)
	}

	// Round 0 answered nothing: everything pending is asked once already,
	// so the probe re-asks only what has evidence to cite — never a triaged
	// criterion, never an evidence-less one.
	asked := map[string]int{}
	for _, id := range pending {
		asked[id] = 1
	}
	rgaaWriteJSON(t, filepath.Join(run, "asked.json"), asked)
	probe := rgaaOK(t, "fold_probe", rgaaRunNode(t, scripts, "fold_probe", site, env, in(map[string]any{"input.round": 0})))
	withEvidence := 0
	for _, id := range pending {
		var item struct {
			Evidence []map[string]any `json:"evidence"`
		}
		rgaaReadJSON(t, filepath.Join(fmt.Sprint(wl["items_dir"]), id+".json"), &item)
		if len(item.Evidence) > 0 {
			withEvidence++
		}
	}
	reasked := rgaaStrings(probe["pending_ids"])
	for _, id := range reasked {
		if triaged[id] {
			t.Fatalf("the probe re-asks triaged criterion %s", id)
		}
	}
	if len(reasked) < withEvidence || rgaaInt(probe["no_evidence"])+len(reasked) != len(pending) || rgaaInt(probe["no_evidence"]) == 0 {
		t.Fatalf("probe re-asks %d of %d asked (%d with evidence), no_evidence %v: only the criteria with evidence (or flagged without subject) earn a re-ask", len(reasked), len(pending), withEvidence, probe["no_evidence"])
	}

	// The fold names them open; the annex lists every open criterion as a
	// point to re-check, the triaged ones with their reason.
	fold := rgaaOK(t, "fold_final", rgaaRunNode(t, scripts, "fold_final", site, env, in(map[string]any{"input.require_complete": false})))
	open := map[string]bool{}
	for _, id := range rgaaStrings(fold["still_open_ids"]) {
		open[id] = true
	}
	for _, id := range recheck {
		if !open[id] {
			t.Fatalf("triaged criterion %s is not open after the fold: %v", id, fold["still_open_ids"])
		}
	}
	rgaaOK(t, "draft_report", rgaaRunNode(t, scripts, "draft_report", site, env, in(nil)))
	final := rgaaOK(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, in(map[string]any{"input.html_report": false, "input.report_dir": "", "input.require_complete": false, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": ""})))
	annex, err := os.ReadFile(fmt.Sprint(final["annex_path"]))
	if err != nil {
		t.Fatal(err)
	}
	text := string(annex)
	if !strings.Contains(text, "## Points à revérifier") || !strings.Contains(text, "| Tri avant agents | ok | "+fmt.Sprint(len(recheck))+" critère(s) non soumis aux agents") {
		t.Fatalf("the annex does not list the points to re-check and the triage:\n%s", text)
	}
	for _, id := range rgaaStrings(final["undecided_ids"]) {
		line := ""
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, "- **"+id+"**") {
				line = l
			}
		}
		if line == "" {
			t.Fatalf("open criterion %s is not listed as a point to re-check:\n%s", id, text)
		}
		if triaged[id] && !strings.Contains(line, "non soumis aux agents") {
			t.Fatalf("triaged criterion %s is listed without its reason: %s", id, line)
		}
	}
}
