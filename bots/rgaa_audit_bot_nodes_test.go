package bots

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dryrun"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The rgaa-audit bot's tool nodes are JavaScript scripts that drive the
// pinned ultra11y engine. These tests run the REAL script bodies — taken
// from the compiled IR, rendered the way the runtime renders a `script:`
// (every {{ref}} a JSON literal) — on the real engine against the planted
// fixture site, in the order the workflow chains them, with fixture verdicts
// standing in for the agents. A node body asserted on its source text would
// pin a spelling; a stub engine would be kinder than the real one. Each
// mutation subtest removes one guarantee and expects the typed refusal.
//
// Two shapes of the runtime the chain respects, because an earlier draft
// of this test did not and let two run-killing defects through: the ENTRY
// node's input is the launch payload (plain `vars`, strings when they come
// from `--var`) and the run directory is what `prepare` itself joins; and
// every node reads its predecessors through the files and hashes the run
// directory holds, never through a value the test chose by hand.
//
// The subtests are sequential and share one run directory (the workflow's
// own shape); the refusal probes run on COPIES of it. Run the whole test,
// not one subtest.

var rgaaScriptRef = regexp.MustCompile(`\{\{\s*(input|vars|run)\.([A-Za-z0-9_.]+)\s*\}\}`)

// rgaaBotScripts returns the script body of every tool node of the bot.
func rgaaBotScripts(t *testing.T) map[string]string {
	t.Helper()
	wf := compilePlanPhaseBot(t, "rgaa-audit")
	out := map[string]string{}
	for id, n := range wf.Nodes {
		tn, ok := n.(*ir.ToolNode)
		if !ok {
			continue
		}
		if strings.TrimSpace(tn.Script) == "" || tn.Language != "js" {
			t.Fatalf("tool node %s is not a js script (language %q): the bot's glue is JavaScript on purpose — the slim sandbox image has no python", id, tn.Language)
		}
		out[id] = tn.Script
	}
	return out
}

// rgaaRenderScript substitutes every {{input.x}} / {{vars.x}} / {{run.id}}
// reference with the JSON literal of the value under that key, as
// renderScript does; a reference with no value is a test bug, not a null.
func rgaaRenderScript(t *testing.T, node, script string, values map[string]any) string {
	t.Helper()
	rendered := rgaaScriptRef.ReplaceAllStringFunc(script, func(ref string) string {
		m := rgaaScriptRef.FindStringSubmatch(ref)
		key := m[1] + "." + m[2]
		v, ok := values[key]
		if !ok {
			t.Fatalf("node %s reads %s and the test supplied no value for it", node, ref)
		}
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	})
	if strings.Contains(rendered, "{{") {
		t.Fatalf("node %s still carries a template reference after rendering: %s", node, rendered[strings.Index(rendered, "{{"):min(len(rendered), strings.Index(rendered, "{{")+60)])
	}
	return rendered
}

type rgaaNodeResult struct {
	Code   int
	Out    map[string]any
	Stdout string
	Stderr string
}

// rgaaRunNode runs one rendered script with node, in cwd, and parses its
// stdout as the node output. A non-zero exit is returned, not fatal: some
// subtests expect the untyped death.
func rgaaRunNode(t *testing.T, scripts map[string]string, node, cwd string, env []string, values map[string]any) rgaaNodeResult {
	t.Helper()
	script, ok := scripts[node]
	if !ok {
		t.Fatalf("the bot has no tool node %q", node)
	}
	file := filepath.Join(t.TempDir(), node+".js")
	if err := os.WriteFile(file, []byte(rgaaRenderScript(t, node, script, values)), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", file)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "npm_config_update_notifier=false")
	cmd.Env = append(cmd.Env, env...)
	stdout, err := cmd.Output()
	res := rgaaNodeResult{Stdout: string(stdout)}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		res.Code = exitErr.ExitCode()
		res.Stderr = string(exitErr.Stderr)
	} else if err != nil {
		t.Fatalf("node %s: %v", node, err)
	}
	if ctx.Err() != nil {
		t.Fatalf("node %s timed out", node)
	}
	if res.Code == 0 {
		if err := json.Unmarshal(stdout, &res.Out); err != nil {
			t.Fatalf("node %s printed no JSON object on exit 0: %v\n%s", node, err, stdout)
		}
	}
	return res
}

func rgaaOK(t *testing.T, node string, r rgaaNodeResult) map[string]any {
	t.Helper()
	if r.Code != 0 {
		t.Fatalf("node %s died (exit %d):\n%s", node, r.Code, r.Stderr)
	}
	if ok, _ := r.Out["ok"].(bool); !ok {
		t.Fatalf("node %s refused: %v %v", node, r.Out["fail_code"], r.Out["reason"])
	}
	return r.Out
}

func rgaaRefused(t *testing.T, node string, r rgaaNodeResult, code string) map[string]any {
	t.Helper()
	if r.Code != 0 {
		t.Fatalf("node %s died (exit %d) instead of refusing with %s:\n%s", node, r.Code, code, r.Stderr)
	}
	if ok, _ := r.Out["ok"].(bool); ok || r.Out["fail_code"] != code {
		t.Fatalf("node %s answered ok=%v fail_code=%v reason=%v, expected the typed refusal %s", node, r.Out["ok"], r.Out["fail_code"], r.Out["reason"], code)
	}
	return r.Out
}

func rgaaInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return -1
}

func rgaaStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func rgaaFileSHA(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func rgaaCopyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
}

// rgaaRunCopy copies the shared run directory so a refusal probe leaves no
// state (layers, probes, markers) behind in the real chain.
func rgaaRunCopy(t *testing.T, run string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "run")
	rgaaCopyDir(t, run, dst)
	return dst
}

// rgaaTamper appends one byte to the audit document: the kind of
// out-of-band edit the hash record exists to catch.
func rgaaTamper(t *testing.T, run string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(run, "audit-latest.json"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// rgaaEvidenceOf reads the anchors the engine harvested for one criterion.
func rgaaEvidenceOf(t *testing.T, itemsDir, id string) []map[string]any {
	t.Helper()
	var it struct {
		Evidence []map[string]any `json:"evidence"`
	}
	rgaaReadJSON(t, filepath.Join(itemsDir, id+".json"), &it)
	return it.Evidence
}

// rgaaFixtureVerdict is the stand-in agent: C citing the first evidence
// anchor when the engine harvested some, NA otherwise — the adjudication
// the engine folds in full on the fixture (TestRgaaAuditEngineContract).
func rgaaFixtureVerdict(t *testing.T, itemsDir, id string) map[string]any {
	t.Helper()
	ev := rgaaEvidenceOf(t, itemsDir, id)
	if len(ev) > 0 {
		e := ev[0]
		return map[string]any{"criteriaId": id, "verdict": "C", "justification": "Conforme sur l'évidence citée (fixture).", "citations": []any{map[string]any{"file": e["file"], "line": e["line"], "snippet": e["snippet"]}}}
	}
	return map[string]any{"criteriaId": id, "verdict": "NA", "justification": "Aucun sujet de ce type dans le périmètre audité (fixture)."}
}

func TestRgaaAuditEngineDrivenNodes(t *testing.T) {
	bin, pin := rgaaResolveEngine(t)
	scripts := rgaaBotScripts(t)
	site := rgaaCopyFixtureSite(t)
	base := filepath.Join(t.TempDir(), "scratch")
	const runID = "test-run"
	run := filepath.Join(base, runID)
	audit := filepath.Join(run, "audit-latest.json")
	env := []string{"ULTRA11Y_BIN=" + bin}
	const std = "rgaa"
	// Every node after prepare reads run_dir the way the edges hand it over:
	// prepare's own output. The values map carries that under `input.run_dir`.
	in := func(extra map[string]any) map[string]any {
		v := map[string]any{"input.run_dir": run, "input.workspace_dir": site, "input.standard": std, "run.id": runID}
		maps.Copy(v, extra)
		return v
	}
	// The entry node's input is the launch payload — strings, as `--var`
	// hands them over — and the BASE run_dir var, not the joined one.
	prepareIn := func(extra map[string]any) map[string]any {
		v := map[string]any{
			"input.run_dir": base, "input.workspace_dir": site, "input.standard": std, "run.id": runID,
			"input.engine_version": pin.Version, "input.engine_sha256": pin.SHA256, "input.engine_size": fmt.Sprint(pin.Size),
			"input.scope_globs": "", "input.exclude_globs": "**/node_modules/**,**/*.test.*", "input.max_files": "4000",
			"input.force_jsx": "true", "input.rendered": "off", "input.ledger_path": "", "input.undecidable_path": "", "input.report_dir": "",
		}
		maps.Copy(v, extra)
		return v
	}
	sha := func() string { return rgaaFileSHA(t, audit) }
	recorded := func(dir string) string {
		var rec struct {
			SHA string `json:"sha256"`
		}
		rgaaReadJSON(t, filepath.Join(dir, "state", "audit.json"), &rec)
		return rec.SHA
	}

	var prep map[string]any
	t.Run("prepare_joins_the_run_id_coerces_launch_strings_and_resolves_the_explicit_engine_by_fingerprint", func(t *testing.T) {
		prep = rgaaOK(t, "prepare", rgaaRunNode(t, scripts, "prepare", site, env, prepareIn(nil)))
		if prep["run_dir"] != run {
			t.Fatalf("prepare's run_dir is %v, want the base joined with the run id (%s)", prep["run_dir"], run)
		}
		want, _ := filepath.EvalSymlinks(bin)
		if prep["engine_path"] != want || prep["engine_source"] != "ULTRA11Y_BIN" || prep["engine_version"] != pin.Version {
			t.Fatalf("prepare resolved %v (%v) %v, want %s from ULTRA11Y_BIN", prep["engine_path"], prep["engine_source"], prep["engine_version"], want)
		}
		args := rgaaStrings(prep["audit_args"])
		if len(args) < 3 || args[0] != "." || args[1] != "--graph" || args[2] != "--jsx" || !rgaaContains(args, "--exclude") || !rgaaContains(args, "--max-files") {
			t.Fatalf("audit_args %v: want the scope, --graph, --jsx (from the STRING \"true\"), --exclude and --max-files", args)
		}
		if prep["rc_present"] != true || prep["ledger_present"] != false || prep["browser_ok"] != false {
			t.Fatalf("prepare facts: rc_present=%v ledger_present=%v browser_ok=%v", prep["rc_present"], prep["ledger_present"], prep["browser_ok"])
		}
		for _, f := range []string{filepath.Join(run, "bin", "lib.js"), filepath.Join(run, "bin", "engine.json"), filepath.Join(run, "layers", "engine.json")} {
			if _, err := os.Stat(f); err != nil {
				t.Fatalf("prepare left no %s", f)
			}
		}
		// The boolean string "false" must not read as true.
		dir := filepath.Join(t.TempDir(), "scratch")
		off := rgaaOK(t, "prepare", rgaaRunNode(t, scripts, "prepare", site, env, prepareIn(map[string]any{"input.run_dir": dir, "input.force_jsx": "false"})))
		if rgaaContains(rgaaStrings(off["audit_args"]), "--jsx") {
			t.Fatalf("force_jsx=\"false\" (the string a --var carries) still added --jsx: %v", off["audit_args"])
		}
	})
	if t.Failed() {
		return
	}

	t.Run("prepare_refuses_an_explicit_engine_whose_fingerprint_is_not_the_pin", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "scratch")
		out := rgaaRefused(t, "prepare", rgaaRunNode(t, scripts, "prepare", site, env, prepareIn(map[string]any{"input.run_dir": dir, "input.engine_sha256": strings.Repeat("0", 64)})), "ENGINE_HASH_MISMATCH")
		if !strings.Contains(fmt.Sprint(out["reason"]), "refusing to substitute") {
			t.Fatalf("the refusal does not say the explicit choice was refused: %v", out["reason"])
		}
		rgaaRefused(t, "prepare", rgaaRunNode(t, scripts, "prepare", site, env, prepareIn(map[string]any{"input.run_dir": dir, "input.engine_version": "9.9.9"})), "ENGINE_HASH_MISMATCH")
	})

	t.Run("prepare_reports_an_unresolvable_engine_instead_of_auditing_with_none", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "scratch")
		hermetic := []string{"ULTRA11Y_BIN=", "ULTRA11Y_HOME=" + t.TempDir(), "PATH=" + t.TempDir()}
		out := rgaaRefused(t, "prepare", rgaaRunNode(t, scripts, "prepare", site, hermetic, prepareIn(map[string]any{"input.run_dir": dir})), "ENGINE_UNRESOLVED")
		if !strings.Contains(fmt.Sprint(out["reason"]), "no engine produced") {
			t.Fatalf("reason: %v", out["reason"])
		}
	})

	t.Run("a_node_output_larger_than_the_pipe_buffer_reaches_the_runtime_whole", func(t *testing.T) {
		// stdout to a pipe is asynchronous in node: a `process.stdout.write`
		// followed by `process.exit` drops everything past 64 KiB.
		big := map[string]string{"_big": "const RUN = {{input.run_dir}};\nconst L = require(require(\"path\").join(RUN, \"bin\", \"lib.js\")).init(RUN);\nL.out({ ok: true, pad: \"x\".repeat(300000) });\n"}
		r := rgaaRunNode(t, big, "_big", site, env, in(nil))
		if r.Code != 0 || len(fmt.Sprint(r.Out["pad"])) != 300000 {
			t.Fatalf("a 300 KB output arrived as %d bytes (exit %d)", len(r.Stdout), r.Code)
		}
	})

	t.Run("the_rate_helper_rounds_like_the_engine", func(t *testing.T) {
		// (v / a) * 100 then round: 23/40 is 57 for the engine and would be
		// 58 under 100 * v / a. The report line is cross-checked against this
		// helper, so the two must agree on every split, not only the one the
		// fixture happens to produce.
		probe := map[string]string{"_rate": "const RUN = {{input.run_dir}};\nconst L = require(require(\"path\").join(RUN, \"bin\", \"lib.js\")).init(RUN);\nL.out({ ok: true, split: L.rate({ conformes: 23, nc: 17, aEvaluer: 0 }), empty: L.rate({ conformes: 0, nc: 0, aEvaluer: 0 }), full: L.rate({ conformes: 40, nc: 0, aEvaluer: 0 }) });\n"}
		r := rgaaRunNode(t, probe, "_rate", site, env, in(nil))
		if r.Code != 0 || rgaaInt(r.Out["split"]) != 57 || rgaaInt(r.Out["empty"]) != -1 || rgaaInt(r.Out["full"]) != 100 {
			t.Fatalf("rate helper: %v (exit %d) — want 23/40 → 57, 0/0 → -1, 40/40 → 100", r.Out, r.Code)
		}
		if rgaaRate(rgaaDocCounts{Conformes: 23, NC: 17}) != 57 {
			t.Fatal("the Go twin of the rate helper rounds 23/40 to 58")
		}
	})

	var auditOut map[string]any
	t.Run("static_audit_runs_the_engine_and_records_the_document_hash", func(t *testing.T) {
		auditOut = rgaaOK(t, "static_audit", rgaaRunNode(t, scripts, "static_audit", site, env, in(map[string]any{"input.max_files": 4000, "input.audit_args": prep["audit_args"]})))
		if rgaaInt(auditOut["files"]) != 3 || rgaaInt(auditOut["findings"]) < 4 || auditOut["audit_sha256"] != sha() || recorded(run) != sha() || auditOut["truncated"] != false {
			t.Fatalf("static_audit: files=%v findings=%v sha=%v recorded=%s file=%s truncated=%v", auditOut["files"], auditOut["findings"], auditOut["audit_sha256"], recorded(run), sha(), auditOut["truncated"])
		}
	})
	if t.Failed() {
		return
	}

	t.Run("static_audit_refuses_an_empty_scope_as_EMPTY_SCOPE_not_as_a_clean_audit", func(t *testing.T) {
		copyRun := rgaaRunCopy(t, run)
		out := rgaaRefused(t, "static_audit", rgaaRunNode(t, scripts, "static_audit", site, env, in(map[string]any{"input.run_dir": copyRun, "input.max_files": 4000, "input.audit_args": []string{"nothing-here/**", "--graph"}})), "EMPTY_SCOPE")
		if rgaaInt(out["files"]) != 0 {
			t.Fatalf("files=%v", out["files"])
		}
	})

	t.Run("bank_static_publishes_the_findings", func(t *testing.T) {
		out := rgaaOK(t, "bank_static", rgaaRunNode(t, scripts, "bank_static", site, env, in(nil)))
		if out["banked"] != true || len(rgaaStrings(out["findings"])) != rgaaInt(auditOut["findings"]) {
			t.Fatalf("bank: %v", out)
		}
		if _, err := os.Stat(filepath.Join(run, "bank", "audit-static.json")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ledger_replay_without_a_ledger_is_a_first_run_and_an_unreadable_one_is_LEDGER_INVALID", func(t *testing.T) {
		out := rgaaOK(t, "ledger_replay", rgaaRunNode(t, scripts, "ledger_replay", site, env, in(map[string]any{"input.ledger_present": false, "input.ledger_path": "/nowhere/rgaa.json"})))
		if out["present"] != false || out["audit_sha256"] != sha() {
			t.Fatalf("first run: %v", out)
		}
		probe := rgaaRunCopy(t, run)
		bad := filepath.Join(t.TempDir(), "rgaa.json")
		for _, body := range []string{"not json", `{"kind":"verdict-ledger"}`} {
			if err := os.WriteFile(bad, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			rgaaRefused(t, "ledger_replay", rgaaRunNode(t, scripts, "ledger_replay", site, env, in(map[string]any{"input.run_dir": probe, "input.ledger_present": true, "input.ledger_path": bad})), "LEDGER_INVALID")
		}
		if err := os.WriteFile(bad, []byte(`{"kind":"verdict-ledger","entries":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		out = rgaaOK(t, "ledger_replay", rgaaRunNode(t, scripts, "ledger_replay", site, env, in(map[string]any{"input.run_dir": probe, "input.ledger_present": true, "input.ledger_path": bad})))
		if out["present"] != false || !strings.Contains(fmt.Sprint(out["reason"]), "empty") {
			t.Fatalf("empty ledger: %v", out)
		}
	})

	var wl map[string]any
	var ids []string
	t.Run("worklist_writes_one_item_file_per_open_criterion_and_the_round_zero_pending_set", func(t *testing.T) {
		wl = rgaaOK(t, "worklist", rgaaRunNode(t, scripts, "worklist", site, env, in(nil)))
		ids = rgaaStrings(wl["pending_ids"])
		if rgaaInt(wl["count"]) < 90 || len(ids) != rgaaInt(wl["count"]) {
			t.Fatalf("worklist count=%v ids=%d", wl["count"], len(ids))
		}
		for _, id := range ids {
			if _, err := os.Stat(filepath.Join(fmt.Sprint(wl["items_dir"]), id+".json")); err != nil {
				t.Fatalf("no item file for %s: %v", id, err)
			}
			if _, err := os.Stat(filepath.Join(fmt.Sprint(wl["briefs_dir"]), id+".md")); err != nil {
				t.Fatalf("no brief for %s: %v", id, err)
			}
		}
		var pending struct {
			Round int      `json:"round"`
			IDs   []string `json:"ids"`
		}
		rgaaReadJSON(t, filepath.Join(run, "pending.json"), &pending)
		if pending.Round != 0 || len(pending.IDs) != len(ids) {
			t.Fatalf("pending.json: %+v", pending)
		}
		if c, _ := wl["contract"].(map[string]any); len(c) == 0 {
			t.Fatal("worklist carries no engine contract")
		}
	})
	if t.Failed() {
		return
	}
	itemsDir := fmt.Sprint(wl["items_dir"])

	// The engine's own "no subject" set, measured by folding a C-only
	// adjudication on a copy (the shape TestRgaaAuditEngineContract pins).
	var flagged []string
	t.Run("the_engine_flags_subject_less_criteria_only_after_a_fold", func(t *testing.T) {
		dir := t.TempDir()
		copyPath := filepath.Join(dir, "audit-latest.json")
		rgaaCopyFile(t, audit, copyPath)
		var skeleton map[string]any
		rgaaReadJSON(t, fmt.Sprint(wl["verdicts_path"]), &skeleton)
		var withEv string
		for _, id := range ids {
			if len(rgaaEvidenceOf(t, itemsDir, id)) > 0 {
				withEv = id
				break
			}
		}
		skeleton["items"] = []any{rgaaFixtureVerdict(t, itemsDir, withEv)}
		rgaaWriteJSON(t, filepath.Join(dir, "adj.json"), skeleton)
		var f foldOut
		rgaaEngineJSON(t, bin, site, nil, &f, "verify", "--apply", filepath.Join(dir, "adj.json"), "--in", copyPath, "--out", dir, "--json", "--standard", std, "--lang", "fr")
		var doc packAuditDoc
		rgaaReadJSON(t, copyPath, &doc)
		flagged = rgaaFlagged(doc)
		if len(flagged) == 0 {
			t.Fatal("no flagged criterion on the fixture")
		}
	})
	if t.Failed() {
		return
	}

	type batch struct {
		ID          string            `json:"id"`
		Round       int               `json:"round"`
		Theme       string            `json:"theme"`
		IDs         []string          `json:"ids"`
		PriorIssues map[string]string `json:"prior_issues"`
	}
	planBatches := func(t *testing.T, dir string) []batch {
		t.Helper()
		r := rgaaRunNode(t, scripts, "plan_batches", site, env, in(map[string]any{"input.run_dir": dir, "input.batch_size": 8}))
		if r.Code != 0 {
			t.Fatalf("plan_batches died: %s", r.Stderr)
		}
		raw, _ := json.Marshal(r.Out["batches"])
		var batches []batch
		if err := json.Unmarshal(raw, &batches); err != nil {
			t.Fatal(err)
		}
		return batches
	}
	writeFragment := func(t *testing.T, dir string, b batch, verdicts any) map[string]any {
		t.Helper()
		r := rgaaRunNode(t, scripts, "write_fragment", site, env, in(map[string]any{"input.run_dir": dir, "input.batch_id": b.ID, "input.round": b.Round, "input.ids": b.IDs, "input.verdicts": verdicts}))
		if r.Code != 0 {
			t.Fatalf("write_fragment died: %s", r.Stderr)
		}
		return r.Out
	}
	mergeVerdicts := func(t *testing.T, dir string, round int) map[string]any {
		t.Helper()
		return rgaaOK(t, "merge_verdicts", rgaaRunNode(t, scripts, "merge_verdicts", site, env, in(map[string]any{"input.run_dir": dir, "input.round": round})))
	}
	foldProbe := func(t *testing.T, dir string, round int) map[string]any {
		t.Helper()
		return rgaaOK(t, "fold_probe", rgaaRunNode(t, scripts, "fold_probe", site, env, in(map[string]any{"input.run_dir": dir, "input.round": round})))
	}
	readPending := func(t *testing.T, dir string) (int, []string, map[string]string) {
		t.Helper()
		var p struct {
			Round  int               `json:"round"`
			IDs    []string          `json:"ids"`
			Issues map[string]string `json:"issues"`
		}
		rgaaReadJSON(t, filepath.Join(dir, "pending.json"), &p)
		return p.Round, p.IDs, p.Issues
	}

	var round0 []batch
	var deadBatch string
	// The hostile first round, one case per guard.
	var strayID, uncitedID, bareNAID, dupID, malformedID, flaggedID, crossID string
	t.Run("plan_batches_groups_the_pending_criteria_by_theme_in_bounded_batches", func(t *testing.T) {
		round0 = planBatches(t, run)
		seen := map[string]bool{}
		for _, b := range round0 {
			if b.Round != 0 || len(b.IDs) == 0 || len(b.IDs) > 8 {
				t.Fatalf("batch %+v", b)
			}
			for _, id := range b.IDs {
				if strings.Split(id, ".")[0] != b.Theme || seen[id] {
					t.Fatalf("batch %s: id %s outside theme %s or repeated", b.ID, id, b.Theme)
				}
				seen[id] = true
			}
		}
		if len(seen) != len(ids) {
			t.Fatalf("batches cover %d ids, worklist has %d", len(seen), len(ids))
		}
		if len(round0) < 3 {
			t.Fatalf("the fixture yields %d batches, the hostile round needs three", len(round0))
		}
		// A zero or negative batch size falls back to the default, never to
		// one agent per criterion.
		r := rgaaRunNode(t, scripts, "plan_batches", site, env, in(map[string]any{"input.batch_size": 0}))
		if r.Code != 0 || rgaaInt(r.Out["count"]) != len(round0) {
			t.Fatalf("batch_size 0 planned %v batches, the default plans %d", r.Out["count"], len(round0))
		}
	})
	if t.Failed() {
		return
	}

	t.Run("a_hostile_first_round_is_filtered_counted_and_named_without_crashing_the_fold", func(t *testing.T) {
		evidenceIDs := func(b batch) []string {
			var out []string
			for _, id := range b.IDs {
				if len(rgaaEvidenceOf(t, itemsDir, id)) > 0 {
					out = append(out, id)
				}
			}
			return out
		}
		first := round0[0]
		ev := evidenceIDs(first)
		if len(ev) < 4 {
			t.Fatalf("the first batch (%v) carries %d criteria with evidence, the hostile round needs four", first.IDs, len(ev))
		}
		strayID, uncitedID, bareNAID, malformedID = ev[0], ev[1], ev[2], ev[3]
		dupID = first.IDs[len(first.IDs)-1]
		crossID = round0[1].IDs[0]
		flaggedID = flagged[0]
		deadBatch = round0[len(round0)-1].ID
		if rgaaContains(round0[len(round0)-1].IDs, flaggedID) {
			flaggedID = flagged[len(flagged)-1]
		}
		for i, b := range round0 {
			if b.ID == deadBatch {
				continue // this branch "died": no fragment
			}
			var verdicts []any
			for _, id := range b.IDs {
				v := rgaaFixtureVerdict(t, itemsDir, id)
				switch {
				case i == 0 && id == strayID:
					v = map[string]any{"criteriaId": id, "verdict": "C", "justification": "citation hors évidence (fixture)", "citations": []any{map[string]any{"file": "contact.html", "line": 2, "snippet": ""}}}
				case i == 0 && id == uncitedID:
					v = map[string]any{"criteriaId": id, "verdict": "C", "justification": "sans citation (fixture)"}
				case i == 0 && id == bareNAID:
					v = map[string]any{"criteriaId": id, "verdict": "NA", "justification": "NA sans citer l'évidence montrée (fixture)"}
				case i == 0 && id == malformedID:
					e := rgaaEvidenceOf(t, itemsDir, id)[0]
					v = map[string]any{"criteriaId": id, "verdict": "C", "justification": "citations malformées (fixture)", "citations": []any{nil, map[string]any{"file": []any{"index.html"}, "line": e["line"]}, map[string]any{"line": e["line"]}}}
				case id == flaggedID:
					v = map[string]any{"criteriaId": id, "verdict": "manual", "reason": "undecidable", "justification": "manual sur un critère sans sujet (fixture)"}
				}
				verdicts = append(verdicts, v)
			}
			if i == 0 {
				verdicts = append(verdicts,
					map[string]any{"criteriaId": crossID, "verdict": "NA", "justification": "FOREIGN — un verdict pour le lot voisin"},
					map[string]any{"criteriaId": dupID, "verdict": "NC", "justification": "second verdict pour le même id", "findings": []any{map[string]any{"file": "index.html", "line": 9, "message": "x", "normativeRef": dupID}}},
					"not an object",
				)
			}
			out := writeFragment(t, run, b, verdicts)
			if i == 0 {
				// Four malformed: the non-object verdict and the three citations
				// (null, a file that is an array, no file at all) — the C is left
				// without a citation and the engine refuses it by name.
				if rgaaInt(out["foreign"]) != 1 || rgaaInt(out["duplicate"]) != 1 || rgaaInt(out["malformed"]) != 4 || rgaaInt(out["written"]) != len(b.IDs)-1 {
					t.Fatalf("hostile fragment: %v (want foreign 1, duplicate 1, malformed 4, written %d)", out, len(b.IDs)-1)
				}
			} else if rgaaInt(out["written"]) != len(b.IDs) {
				t.Fatalf("batch %s wrote %v of %d", b.ID, out["written"], len(b.IDs))
			}
		}
		out := mergeVerdicts(t, run, 0)
		if rgaaStrings(out["failed_batches"])[0] != deadBatch || rgaaInt(out["citation_rejected"]) != 2 || rgaaInt(out["foreign_dropped"]) != 6 {
			t.Fatalf("merge: failed_batches=%v citation_rejected=%v foreign_dropped=%v (want the dead batch, 2 — the stray C and the bare NA —, 6 = 1 foreign + 1 duplicate + 4 malformed)", out["failed_batches"], out["citation_rejected"], out["foreign_dropped"])
		}
		var adj struct {
			Items []map[string]any `json:"items"`
		}
		rgaaReadJSON(t, filepath.Join(run, "adjudication.json"), &adj)
		for _, it := range adj.Items {
			id := fmt.Sprint(it["criteriaId"])
			if id == strayID || id == bareNAID || id == dupID {
				t.Fatalf("adjudication.json still carries %s", id)
			}
			if id == crossID && strings.Contains(fmt.Sprint(it["justification"]), "FOREIGN") {
				t.Fatalf("the neighbour batch's own verdict on %s was overridden by the foreign one", id)
			}
		}
		before := sha()
		probe := foldProbe(t, run, 0)
		if sha() != before {
			t.Fatal("the probe modified the real audit")
		}
		pending := rgaaStrings(probe["pending_ids"])
		_, pIDs, issues := readPending(t, run)
		if len(pIDs) != len(pending) {
			t.Fatalf("pending.json carries %d ids, the probe reported %d", len(pIDs), len(pending))
		}
		for id, want := range map[string]string{strayID: "outside this criterion's own evidence", uncitedID: "cite", bareNAID: "must cite that evidence", dupID: "two verdicts", malformedID: "cite", flaggedID: "no subject"} {
			if !rgaaContains(pending, id) {
				t.Fatalf("pending %v lacks %s", pending, id)
			}
			if !strings.Contains(issues[id], want) {
				t.Fatalf("the re-ask reason for %s is %q, want it to mention %q", id, issues[id], want)
			}
		}
		for _, id := range round0[len(round0)-1].IDs {
			if !rgaaContains(pending, id) {
				t.Fatalf("the dead batch's %s is not pending", id)
			}
		}
		if rgaaInt(probe["flagged_open"]) < 1 || rgaaInt(probe["rejected"]) < 2 {
			t.Fatalf("probe counters: %v", probe)
		}
	})
	if t.Failed() {
		return
	}

	t.Run("the_re_ask_replans_exactly_the_pending_criteria_with_their_reasons_and_the_next_probe_is_clean", func(t *testing.T) {
		_, pendingIDs, _ := readPending(t, run)
		round1 := planBatches(t, run)
		covered := map[string]bool{}
		for _, b := range round1 {
			if b.Round != 1 {
				t.Fatalf("round %d batch on the re-ask", b.Round)
			}
			var verdicts []any
			for _, id := range b.IDs {
				covered[id] = true
				if b.PriorIssues[id] == "" {
					t.Fatalf("batch %s carries no prior issue for %s", b.ID, id)
				}
				verdicts = append(verdicts, rgaaFixtureVerdict(t, itemsDir, id))
			}
			writeFragment(t, run, b, verdicts)
		}
		for _, id := range pendingIDs {
			if !covered[id] {
				t.Fatalf("the re-ask did not replan %s", id)
			}
		}
		if len(covered) != len(pendingIDs) {
			t.Fatalf("the re-ask planned %d ids for %d pending", len(covered), len(pendingIDs))
		}
		out := mergeVerdicts(t, run, 1)
		if rgaaInt(out["unfilled"]) != 0 || len(rgaaStrings(out["failed_batches"])) != 0 {
			t.Fatalf("merge round 1: %v", out)
		}
		probe1 := foldProbe(t, run, 1)
		if rgaaInt(probe1["pending"]) != 0 || rgaaInt(probe1["flagged_open"]) != 0 {
			t.Fatalf("probe round 1 still pending: %v", probe1)
		}
	})
	if t.Failed() {
		return
	}

	var fold map[string]any
	t.Run("fold_final_folds_the_real_audit_once_records_its_hash_and_re_emits_on_re_entry", func(t *testing.T) {
		// An incomplete adjudication under require_complete is the typed
		// refusal — probed on a COPY of the run so the real chain goes on.
		copyRun := rgaaRunCopy(t, run)
		var adj map[string]any
		rgaaReadJSON(t, filepath.Join(copyRun, "adjudication.json"), &adj)
		adj["items"] = adj["items"].([]any)[:3]
		rgaaWriteJSON(t, filepath.Join(copyRun, "adjudication.json"), adj)
		out := rgaaRefused(t, "fold_final", rgaaRunNode(t, scripts, "fold_final", site, env, in(map[string]any{"input.run_dir": copyRun, "input.require_complete": true})), "ADJUDICATION_INCOMPLETE")
		if rgaaInt(out["applied"]) != 3 || rgaaInt(out["still_open"]) != len(ids)-3 || len(rgaaStrings(out["still_open_ids"])) != len(ids)-3 {
			t.Fatalf("partial fold on the copy: %v", out)
		}
		before := sha()
		fold = rgaaOK(t, "fold_final", rgaaRunNode(t, scripts, "fold_final", site, env, in(map[string]any{"input.require_complete": true})))
		if sha() == before || fold["audit_sha256"] != sha() || recorded(run) != sha() {
			t.Fatal("fold_final did not rewrite the audit, or did not record the hash it left")
		}
		if rgaaInt(fold["applied"]) != len(ids) || rgaaInt(fold["still_open"]) != 0 || rgaaInt(fold["rejected"]) != 0 {
			t.Fatalf("fold_final: %v", fold)
		}
		var ledger struct {
			Entries []map[string]any `json:"entries"`
		}
		rgaaReadJSON(t, fmt.Sprint(fold["ledger_path"]), &ledger)
		if len(ledger.Entries) != len(ids) {
			t.Fatalf("ledger carries %d entries for %d verdicts", len(ledger.Entries), len(ids))
		}
		// Re-entered after its own write (a resume), the node re-emits its
		// recorded output instead of folding decided criteria again.
		again := rgaaOK(t, "fold_final", rgaaRunNode(t, scripts, "fold_final", site, env, in(map[string]any{"input.require_complete": true})))
		if again["audit_sha256"] != fold["audit_sha256"] || rgaaInt(again["applied"]) != rgaaInt(fold["applied"]) || sha() != fold["audit_sha256"] {
			t.Fatalf("re-entry: %v", again)
		}
	})
	if t.Failed() {
		return
	}

	t.Run("a_ledger_from_one_run_is_replayed_by_the_next_and_leaves_nothing_to_adjudicate", func(t *testing.T) {
		base2 := filepath.Join(t.TempDir(), "scratch")
		p2 := rgaaOK(t, "prepare", rgaaRunNode(t, scripts, "prepare", site, env, prepareIn(map[string]any{"input.run_dir": base2})))
		run2 := fmt.Sprint(p2["run_dir"])
		rgaaOK(t, "static_audit", rgaaRunNode(t, scripts, "static_audit", site, env, in(map[string]any{"input.run_dir": run2, "input.max_files": 4000, "input.audit_args": p2["audit_args"]})))
		rep := rgaaOK(t, "ledger_replay", rgaaRunNode(t, scripts, "ledger_replay", site, env, in(map[string]any{"input.run_dir": run2, "input.ledger_present": true, "input.ledger_path": fold["ledger_path"]})))
		if rep["present"] != true || rgaaInt(rep["replayed"]) != len(ids) || rgaaInt(rep["stale"]) != 0 || recorded(run2) != rgaaFileSHA(t, filepath.Join(run2, "audit-latest.json")) {
			t.Fatalf("replay: %v", rep)
		}
		w := rgaaOK(t, "worklist", rgaaRunNode(t, scripts, "worklist", site, env, in(map[string]any{"input.run_dir": run2})))
		if rgaaInt(w["count"]) != 0 {
			t.Fatalf("after a full replay %v criteria are still open", w["count"])
		}
	})

	var draft map[string]any
	t.Run("draft_report_puts_every_claim_on_trial", func(t *testing.T) {
		draft = rgaaOK(t, "draft_report", rgaaRunNode(t, scripts, "draft_report", site, env, in(nil)))
		if rgaaInt(draft["claims"]) != rgaaInt(draft["nc_claims"])+rgaaInt(draft["c_claims"]) || rgaaInt(draft["nc_claims"]) < 4 || rgaaInt(draft["c_claims"]) < 1 {
			t.Fatalf("draft: %v", draft)
		}
		if _, err := os.Stat(fmt.Sprint(draft["todo_path"])); err != nil {
			t.Fatal(err)
		}
	})
	if t.Failed() {
		return
	}

	type rbatch struct {
		ID    string           `json:"id"`
		Round int              `json:"round"`
		Ns    []int            `json:"ns"`
		Items []map[string]any `json:"items"`
	}
	planRefute := func(t *testing.T) []rbatch {
		t.Helper()
		r := rgaaRunNode(t, scripts, "plan_refute", site, env, in(map[string]any{"input.batch_size": 10}))
		if r.Code != 0 {
			t.Fatalf("plan_refute died: %s", r.Stderr)
		}
		raw, _ := json.Marshal(r.Out["batches"])
		var batches []rbatch
		if err := json.Unmarshal(raw, &batches); err != nil {
			t.Fatal(err)
		}
		return batches
	}
	writeRefute := func(t *testing.T, b rbatch, verdicts []map[string]any) map[string]any {
		t.Helper()
		r := rgaaRunNode(t, scripts, "write_refute_fragment", site, env, in(map[string]any{"input.batch_id": b.ID, "input.round": b.Round, "input.ns": b.Ns, "input.verdicts": verdicts}))
		if r.Code != 0 {
			t.Fatalf("write_refute_fragment died: %s", r.Stderr)
		}
		return r.Out
	}
	mergeRefute := func(t *testing.T, round int) map[string]any {
		t.Helper()
		r := rgaaRunNode(t, scripts, "merge_refutations", site, env, in(map[string]any{"input.round": round}))
		if r.Code != 0 {
			t.Fatalf("merge_refutations died: %s", r.Stderr)
		}
		return r.Out
	}

	var refutedC string
	t.Run("an_incomplete_trial_is_re_asked_then_a_complete_one_is_merged", func(t *testing.T) {
		batches := planRefute(t)
		if len(batches) < 2 {
			t.Fatalf("%d refutation batches, the dead-branch case needs two", len(batches))
		}
		for i, b := range batches {
			if i == len(batches)-1 {
				continue // dead branch
			}
			var verdicts []map[string]any
			for _, it := range b.Items {
				v := map[string]any{"n": it["n"], "verdict": "supported", "note": "constat vérifié (fixture)"}
				if refutedC == "" && it["kind"] == "c" {
					refutedC = fmt.Sprint(it["criteriaId"])
					v["verdict"] = "refuted"
				}
				verdicts = append(verdicts, v)
			}
			// A claim of the OTHER batch, and an off-vocabulary verdict on a
			// claim this batch has not answered yet.
			other := batches[len(batches)-1].Items[0]["n"]
			verdicts = append([]map[string]any{{"n": b.Items[0]["n"], "verdict": "maybe"}}, verdicts...)
			verdicts = append(verdicts, map[string]any{"n": other, "verdict": "supported"})
			out := writeRefute(t, b, verdicts)
			if rgaaInt(out["foreign"]) != 1 || rgaaInt(out["invalid"]) != 1 || rgaaInt(out["written"]) != len(b.Items) {
				t.Fatalf("refute fragment: %v (want foreign 1, invalid 1, written %d)", out, len(b.Items))
			}
		}
		out := mergeRefute(t, 0)
		if out["complete"] != false || rgaaInt(out["unfilled"]) != len(batches[len(batches)-1].Items) || len(rgaaStrings(out["failed_batches"])) != 1 {
			t.Fatalf("merge round 0: %v", out)
		}
		// The incomplete trial is not applied: the audit stays as folded.
		before := sha()
		ra := rgaaOK(t, "refute_apply", rgaaRunNode(t, scripts, "refute_apply", site, env, in(map[string]any{"input.run_dir": rgaaRunCopy(t, run), "input.complete": false})))
		if ra["applied"] != false || ra["degraded"] != true || sha() != before {
			t.Fatalf("incomplete trial: %v (audit moved: %v)", ra, sha() != before)
		}
		round1 := planRefute(t)
		n := 0
		for _, b := range round1 {
			n += len(b.Items)
			var verdicts []map[string]any
			for _, it := range b.Items {
				verdicts = append(verdicts, map[string]any{"n": it["n"], "verdict": "supported", "note": "fixture"})
			}
			writeRefute(t, b, verdicts)
		}
		if n != rgaaInt(out["unfilled"]) {
			t.Fatalf("the re-ask replanned %d claims, %v were unfilled", n, out["unfilled"])
		}
		out = mergeRefute(t, 1)
		if out["complete"] != true || rgaaInt(out["refuted"]) != 1 {
			t.Fatalf("merge round 1: %v", out)
		}
		if refutedC == "" {
			t.Fatal("no conformity claim was refuted — the fixture must land at least one C")
		}
	})
	if t.Failed() {
		return
	}

	var applied map[string]any
	t.Run("refute_apply_clears_the_refuted_conformity_records_the_new_hash_and_leaves_the_engine_findings_alone", func(t *testing.T) {
		before := sha()
		applied = rgaaOK(t, "refute_apply", rgaaRunNode(t, scripts, "refute_apply", site, env, in(map[string]any{"input.complete": true})))
		if applied["applied"] != true || sha() == before || applied["audit_sha256"] != sha() || recorded(run) != sha() {
			t.Fatalf("apply: %v", applied)
		}
		if cleared := rgaaStrings(applied["cleared"]); len(cleared) != 1 || cleared[0] != refutedC || rgaaInt(applied["removed_findings"]) != 0 || rgaaStrings(applied["reopened"])[0] != refutedC {
			t.Fatalf("cleared %v reopened %v removed %v, expected only %s", applied["cleared"], applied["reopened"], applied["removed_findings"], refutedC)
		}
		var doc packAuditDoc
		rgaaReadJSON(t, audit, &doc)
		if got := rgaaStatusCounts(doc); got["manual"] != 1 || got["NC"] < 4 {
			t.Fatalf("statuses after the trial: %v", got)
		}
		var ledger struct {
			Entries []map[string]any `json:"entries"`
		}
		rgaaReadJSON(t, fmt.Sprint(fold["ledger_path"]), &ledger)
		if len(ledger.Entries) != len(ids)-1 {
			t.Fatalf("the refuted conformity is still in the ledger (%d entries)", len(ledger.Entries))
		}
	})
	if t.Failed() {
		return
	}

	t.Run("write_undecidable_merges_the_committed_declarations_with_the_reviewer_s_still_open_ones", func(t *testing.T) {
		committed := filepath.Join(t.TempDir(), "undecidable-rgaa.json")
		rgaaWriteJSON(t, committed, map[string]any{"entries": []any{
			map[string]any{"criteriaId": refutedC, "reason": "déclaré dans le dépôt", "recordedBy": "repo"},
			map[string]any{"criteriaId": "2.1", "reason": "décidé par le moteur, ne doit pas rester", "recordedBy": "repo"},
		}})
		out := rgaaRunNode(t, scripts, "write_undecidable", site, env, in(map[string]any{"input.undecidable_ids": []string{refutedC, "2.1", "99.9"}, "input.notes": "non tranchable sans la page rendue", "input.undecidable_present": true, "input.undecidable_path": committed}))
		if out.Code != 0 || rgaaInt(out.Out["written"]) != 1 {
			t.Fatalf("write_undecidable: exit %d %v %s", out.Code, out.Out, out.Stderr)
		}
		var f struct {
			Entries []struct {
				CriteriaID string `json:"criteriaId"`
				RecordedBy string `json:"recordedBy"`
				Reason     string `json:"reason"`
			} `json:"entries"`
		}
		rgaaReadJSON(t, filepath.Join(run, "undecidable-rgaa.json"), &f)
		if len(f.Entries) != 1 || f.Entries[0].CriteriaID != refutedC || f.Entries[0].RecordedBy != "human-review:"+runID || !strings.Contains(f.Entries[0].Reason, "rendue") {
			t.Fatalf("undecidable file: %+v", f)
		}
		// Without the reviewer, the committed declaration alone is kept.
		copyRun := rgaaRunCopy(t, run)
		out = rgaaRunNode(t, scripts, "write_undecidable", site, env, in(map[string]any{"input.run_dir": copyRun, "input.undecidable_ids": []string{}, "input.notes": "", "input.undecidable_present": true, "input.undecidable_path": committed}))
		rgaaReadJSON(t, filepath.Join(copyRun, "undecidable-rgaa.json"), &f)
		if out.Code != 0 || len(f.Entries) != 1 || f.Entries[0].RecordedBy != "repo" {
			t.Fatalf("committed-only: %+v", f)
		}
	})

	var final map[string]any
	t.Run("final_report_cross_checks_the_report_writes_the_annex_and_copies_the_deliverables", func(t *testing.T) {
		reportDir := filepath.Join(t.TempDir(), "audits")
		final = rgaaOK(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, in(map[string]any{"input.html_report": false, "input.report_dir": reportDir, "input.require_complete": true, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": "fixture scope"})))
		if final["complete"] != true || rgaaInt(final["undecided"]) != 0 || final["provisional"] != true {
			t.Fatalf("final: complete=%v undecided=%v provisional=%v", final["complete"], final["undecided"], final["provisional"])
		}
		if d, _ := final["degraded"].(map[string]any); len(d) != 0 {
			t.Fatalf("degraded layers on a consistent report: %v", d)
		}
		var doc packAuditDoc
		rgaaReadJSON(t, audit, &doc)
		counts := rgaaCountDoc(doc)
		if rgaaInt(final["conformance_pct"]) != rgaaRate(counts) {
			t.Fatalf("conformance_pct %v ≠ %d computed from criteria[]", final["conformance_pct"], rgaaRate(counts))
		}
		lines := rgaaReadReportLines(t, fmt.Sprint(final["report_path"]))
		if lines.Rate != rgaaRate(counts) || lines.Decided != counts {
			t.Fatalf("report lines %+v / %d ≠ document %+v / %d", lines.Decided, lines.Rate, counts, rgaaRate(counts))
		}
		annex, err := os.ReadFile(fmt.Sprint(final["annex_path"]))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"# Annexe de couverture", "| Analyse statique | ok |", "| Pages rendues | ignorée |", "| Registre de verdicts | absent |", "| Adjudication par agents |", "| Pliage (gate moteur) | ok |", "| Contre-lecture | ok |", "| Revue humaine | ok |", "| Intégrité du rapport | ok |", "indécidables déclarés : " + refutedC, "## Périmètre demandé", "fixture scope"} {
			if !strings.Contains(string(annex), want) {
				t.Fatalf("annex lacks %q:\n%s", want, annex)
			}
		}
		for _, f := range []string{filepath.Base(fmt.Sprint(final["report_path"])), filepath.Base(fmt.Sprint(final["annex_path"])), filepath.Join("ledger", "rgaa.json")} {
			if _, err := os.Stat(filepath.Join(reportDir, f)); err != nil {
				t.Fatalf("report_dir lacks %s", f)
			}
		}
	})
	if t.Failed() {
		return
	}

	t.Run("final_report_without_the_declaration_is_incomplete_under_require_complete", func(t *testing.T) {
		copyRun := rgaaRunCopy(t, run)
		if err := os.Remove(filepath.Join(copyRun, "undecidable-rgaa.json")); err != nil {
			t.Fatal(err)
		}
		out := rgaaRefused(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, in(map[string]any{"input.run_dir": copyRun, "input.html_report": false, "input.report_dir": "", "input.require_complete": true, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": ""})), "GRID_INCOMPLETE")
		if rgaaInt(out["undecided"]) != 1 || rgaaStrings(out["undecided_ids"])[0] != refutedC {
			t.Fatalf("undecided: %v", out)
		}
	})

	t.Run("final_report_degrades_the_integrity_layer_when_engine_flagged_criteria_stay_open", func(t *testing.T) {
		// The known upstream double count: a C-only fold leaves the flagged
		// criteria open and the report disagrees with the document. The bot
		// reports the layer as degraded and goes on (require_complete off).
		copyRun := rgaaRunCopy(t, run)
		raw := filepath.Join(copyRun, "audit-latest.json")
		rgaaCopyFile(t, filepath.Join(run, "bank", "audit-static.json"), raw)
		var skeleton map[string]any
		rgaaReadJSON(t, fmt.Sprint(wl["verdicts_path"]), &skeleton)
		skeleton["items"] = []any{rgaaFixtureVerdict(t, itemsDir, strayID)}
		rgaaWriteJSON(t, filepath.Join(copyRun, "adj.json"), skeleton)
		var f foldOut
		rgaaEngineJSON(t, bin, site, nil, &f, "verify", "--apply", filepath.Join(copyRun, "adj.json"), "--in", raw, "--out", copyRun, "--json", "--standard", std, "--lang", "fr")
		rgaaWriteJSON(t, filepath.Join(copyRun, "state", "audit.json"), map[string]any{"sha256": rgaaFileSHA(t, raw)})
		if err := os.Remove(filepath.Join(copyRun, "undecidable-rgaa.json")); err != nil {
			t.Fatal(err)
		}
		out := rgaaOK(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, in(map[string]any{"input.run_dir": copyRun, "input.html_report": false, "input.report_dir": "", "input.require_complete": false, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": ""})))
		d, _ := out["degraded"].(map[string]any)
		// The « Décidés » cross-check itself must fire — not only the engine's
		// own refusal, which covers the same case and would hide a dropped check.
		if d["decided_line"] == nil || out["complete"] != false {
			t.Fatalf("a left-open flagged set did not degrade the integrity layer through the « Décidés » cross-check: %v", out)
		}
		annex, _ := os.ReadFile(fmt.Sprint(out["annex_path"]))
		if !strings.Contains(string(annex), "| Intégrité du rapport | dégradée |") {
			t.Fatalf("annex does not say the integrity is degraded:\n%s", annex)
		}
	})

	var plan map[string]any
	t.Run("deliver_builds_the_capped_issue_plan_from_the_engine_tickets", func(t *testing.T) {
		plan = rgaaOK(t, "deliver", rgaaRunNode(t, scripts, "deliver", site, env, in(map[string]any{"input.report_path": final["report_path"], "input.findings_cap": 2, "input.board_state": "inbox", "input.board_epic_label": "epic:a11y"})))
		issues, _ := plan["issues"].([]any)
		if rgaaInt(plan["total"]) < 4 || len(issues) != 2 || rgaaInt(plan["capped"]) != rgaaInt(plan["total"])-2 || rgaaInt(plan["blocking"]) < 1 {
			t.Fatalf("plan: total=%v kept=%d capped=%v blocking=%v", plan["total"], len(issues), plan["capped"], plan["blocking"])
		}
		first := issues[0].(map[string]any)
		labels := rgaaStrings(first["labels"])
		if !rgaaContains(labels, "source:rgaa-audit") || !rgaaContains(labels, "epic:a11y") || !rgaaContains(labels, "criterion:"+fmt.Sprint(first["criteriaId"])) || first["state"] != "inbox" {
			t.Fatalf("issue labels/state: %v %v", labels, first["state"])
		}
		body := fmt.Sprint(first["body"])
		if !strings.Contains(body, "**Reproduction**") || !strings.Contains(body, pin.Version) || !strings.Contains(body, fmt.Sprint(final["report_path"])) {
			t.Fatalf("issue body lacks the reproduction line or the report path:\n%s", body)
		}
		if !strings.HasPrefix(fmt.Sprint(first["title"]), "[a11y] RGAA ") {
			t.Fatalf("the plan is not built from the engine's titles: %v", first["title"])
		}
		// findings_cap 0 files nothing; a negative cap is the default.
		none := rgaaOK(t, "deliver", rgaaRunNode(t, scripts, "deliver", site, env, in(map[string]any{"input.run_dir": rgaaRunCopy(t, run), "input.report_path": final["report_path"], "input.findings_cap": 0, "input.board_state": "inbox", "input.board_epic_label": ""})))
		if n, _ := none["issues"].([]any); len(n) != 0 || rgaaInt(none["capped"]) != rgaaInt(plan["total"]) {
			t.Fatalf("findings_cap 0: %v", none)
		}
	})
	if t.Failed() {
		return
	}

	t.Run("publish_verify_holds_the_judge_s_report_to_the_plan", func(t *testing.T) {
		issues, _ := plan["issues"].([]any)
		var titles []string
		for _, i := range issues {
			titles = append(titles, fmt.Sprint(i.(map[string]any)["title"]))
		}
		out := rgaaOK(t, "publish_verify", rgaaRunNode(t, scripts, "publish_verify", site, env, in(map[string]any{"input.issues": issues, "input.created": titles[:1], "input.existing": titles[1:], "input.failed": []string{}})))
		if rgaaInt(out["created"]) != 1 || rgaaInt(out["existing"]) != 1 {
			t.Fatalf("verify: %v", out)
		}
		probe := rgaaRunCopy(t, run)
		rgaaRefused(t, "publish_verify", rgaaRunNode(t, scripts, "publish_verify", site, env, in(map[string]any{"input.run_dir": probe, "input.issues": issues, "input.created": titles[:1], "input.existing": []string{}, "input.failed": []string{}})), "BOARD_PUBLISH_INCOMPLETE")
		rgaaRefused(t, "publish_verify", rgaaRunNode(t, scripts, "publish_verify", site, env, in(map[string]any{"input.run_dir": probe, "input.issues": issues, "input.created": titles, "input.existing": []string{}, "input.failed": []string{"something"}})), "BOARD_PUBLISH_INCOMPLETE")
	})

	t.Run("every_node_that_reads_the_audit_refuses_a_document_edited_outside_the_engine", func(t *testing.T) {
		// One tampered copy of the finished run, every reader on it: the
		// record its last writer left no longer matches the file.
		inputs := map[string]map[string]any{
			"bank_static":   {},
			"ledger_replay": {"input.ledger_present": false, "input.ledger_path": "/nowhere"},
			"worklist":      {},
			"fold_probe":    {"input.round": 2},
			"fold_final":    {"input.require_complete": false},
			"draft_report":  {},
			"refute_apply":  {"input.complete": true},
			"final_report":  {"input.html_report": false, "input.report_dir": "", "input.require_complete": false, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": ""},
			"deliver":       {"input.report_path": final["report_path"], "input.findings_cap": 80, "input.board_state": "inbox", "input.board_epic_label": ""},
		}
		for node, extra := range inputs {
			copyRun := rgaaRunCopy(t, run)
			rgaaTamper(t, copyRun)
			values := in(extra)
			values["input.run_dir"] = copyRun
			rgaaRefused(t, node, rgaaRunNode(t, scripts, node, site, env, values), "AUDIT_INTEGRITY")
		}
		// And the engine itself: a manifest whose fingerprint the entry file
		// no longer carries stops every call before it runs.
		copyRun := rgaaRunCopy(t, run)
		var meta map[string]any
		rgaaReadJSON(t, filepath.Join(copyRun, "bin", "engine.json"), &meta)
		meta["sha256"] = strings.Repeat("0", 64)
		rgaaWriteJSON(t, filepath.Join(copyRun, "bin", "engine.json"), meta)
		r := rgaaRunNode(t, scripts, "worklist", site, env, in(map[string]any{"input.run_dir": copyRun}))
		if r.Code == 0 || !strings.Contains(r.Stderr, "engine integrity") {
			t.Fatalf("a tampered engine manifest did not stop the call (exit %d): %s", r.Code, r.Stderr)
		}
	})

	t.Run("the_hand_authored_goldens_pass_the_fragment_filters_whole", func(t *testing.T) {
		var adjG struct {
			Input  map[string]any `json:"input"`
			Output map[string]any `json:"output"`
		}
		rgaaReadJSON(t, filepath.Join("..", "pkg", "botreplay", "testdata", "bot-goldens", "rgaa-audit", "adjudicate_batch.json"), &adjG)
		b := batch{ID: "golden", Round: 0, IDs: rgaaStrings(adjG.Input["ids"])}
		out := writeFragment(t, rgaaRunCopy(t, run), b, adjG.Output["verdicts"])
		if rgaaInt(out["written"]) != len(b.IDs) || rgaaInt(out["foreign"]) != 0 || rgaaInt(out["malformed"]) != 0 || rgaaInt(out["duplicate"]) != 0 {
			t.Fatalf("the adjudicate golden does not pass write_fragment whole: %v", out)
		}
		var refG struct {
			Input  map[string]any `json:"input"`
			Output map[string]any `json:"output"`
		}
		rgaaReadJSON(t, filepath.Join("..", "pkg", "botreplay", "testdata", "bot-goldens", "rgaa-audit", "refute_batch.json"), &refG)
		var ns []int
		for _, it := range refG.Input["items"].([]any) {
			ns = append(ns, rgaaInt(it.(map[string]any)["n"]))
		}
		raw, _ := json.Marshal(refG.Output["verdicts"])
		var verdicts []map[string]any
		if err := json.Unmarshal(raw, &verdicts); err != nil {
			t.Fatal(err)
		}
		rb := rbatch{ID: "golden", Round: 0, Ns: ns}
		r := rgaaRunNode(t, scripts, "write_refute_fragment", site, env, in(map[string]any{"input.run_dir": rgaaRunCopy(t, run), "input.batch_id": rb.ID, "input.round": rb.Round, "input.ns": rb.Ns, "input.verdicts": verdicts}))
		if r.Code != 0 || rgaaInt(r.Out["written"]) != len(ns) || rgaaInt(r.Out["invalid"]) != 0 || rgaaInt(r.Out["foreign"]) != 0 {
			t.Fatalf("the refute golden does not pass write_refute_fragment whole: %v %s", r.Out, r.Stderr)
		}
	})

	t.Run("every_layer_the_annex_reads_was_written_by_the_node_that_ran_it", func(t *testing.T) {
		entries, err := os.ReadDir(filepath.Join(run, "layers"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, strings.TrimSuffix(e.Name(), ".json"))
		}
		sort.Strings(names)
		want := []string{"adjudication", "board", "deliver", "engine", "fold", "human", "ledger", "refutation", "report", "static", "worklist"}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("layers %v, want %v", names, want)
		}
	})
}

// KNOWN UPSTREAM DEFECT (ultra11y 5.42.1): the contract says a NA on a
// criterion with harvested evidence must cite it, and the fold accepts a
// bare one — the criterion lands as « conforme faute de sujet » with nothing
// on trial. The bot's merge sends such a NA back by name
// (TestRgaaAuditEngineDrivenNodes). When this test FAILS, upstream enforces
// its own contract and the bot-side check can go.
func TestRgaaAuditKnownUpstreamDefectBareNAOnEvidenceIsAccepted(t *testing.T) {
	bin, _ := rgaaResolveEngine(t)
	site := rgaaCopyFixtureSite(t)
	run := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	audit := filepath.Join(run, "audit-latest.json")
	std := []string{"--standard", "rgaa", "--lang", "fr"}
	if code, _, stderr := rgaaEngineRun(t, bin, site, nil, append([]string{"audit", ".", "--graph", "--jsx", "--out", run, "--json"}, std...)...); code != 0 {
		t.Fatalf("audit exit %d: %s", code, stderr)
	}
	var worklist worklistOut
	rgaaEngineJSON(t, bin, site, nil, &worklist, append([]string{"verify", "--manual", "--in", audit, "--out", run, "--no-web", "--json"}, std...)...)
	var todo struct {
		Items []struct {
			CriteriaID string           `json:"criteriaId"`
			Evidence   []map[string]any `json:"evidence"`
		} `json:"items"`
	}
	rgaaReadJSON(t, worklist.TodoPath, &todo)
	var target string
	for _, it := range todo.Items {
		if len(it.Evidence) > 0 {
			target = it.CriteriaID
			break
		}
	}
	var skeleton map[string]any
	rgaaReadJSON(t, worklist.VerdictsPath, &skeleton)
	skeleton["items"] = []any{map[string]any{"criteriaId": target, "verdict": "NA", "justification": "NA sans citation sur un critère avec évidence (fixture)."}}
	rgaaWriteJSON(t, filepath.Join(run, "adj.json"), skeleton)
	var f foldOut
	rgaaEngineJSON(t, bin, site, nil, &f, append([]string{"verify", "--apply", filepath.Join(run, "adj.json"), "--in", audit, "--out", run, "--json"}, std...)...)
	if f.Applied != 1 || rgaaContains(f.RejectedCriteria, target) {
		t.Fatalf("the fold now refuses a bare NA on %s (applied=%d rejected=%v): upstream enforces its contract — lift the bot-side check in merge_verdicts", target, f.Applied, f.RejectedCriteria)
	}
}

// The graph itself, walked by the dry run on the recorded node outputs:
// every pass must finish or refuse as declared, with the human gate and the
// board on, and only the typed refusals and the browser layer may stay
// unvisited. The node tests prove the bodies; this proves the wiring
// between them is the wiring the fixtures describe.
func TestRgaaAuditDryRunWalksTheGraphOnTheFixtures(t *testing.T) {
	wf := compilePlanPhaseBot(t, "rgaa-audit")
	var fixtures map[string]map[string]any
	rgaaReadJSON(t, filepath.Join("testdata", "rgaa-audit-dryrun-fixtures.json"), &fixtures)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	rep, err := dryrun.Run(ctx, wf, dryrun.Options{Fixtures: fixtures, Inputs: map[string]any{"post_to_board": true, "human_review": true}, Path: filepath.Join("rgaa-audit", "main.bot"), Timeout: 90 * time.Second})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if rep.Failing() {
		raw, _ := json.MarshalIndent(rep, "", "  ")
		t.Fatalf("the dry run is failing:\n%s", raw)
	}
	allowed := map[string]bool{}
	for _, n := range []string{"adjudication_incomplete", "audit_integrity", "unexpected_refusal", "board_publish_incomplete", "empty_scope", "engine_hash_mismatch", "engine_unresolved", "grid_incomplete", "ledger_invalid", "refute_incomplete", "render", "render_unavailable", "report_integrity_failed"} {
		allowed[n] = true
	}
	for _, n := range rep.UnvisitedNodes {
		if !allowed[n] {
			t.Errorf("node %s was not visited by either pass — the fixtures no longer walk the whole graph", n)
		}
	}
	for _, p := range rep.Passes {
		if p.Status != "finished" && p.Status != "failed" {
			t.Errorf("pass bias=%v ended %s", p.Bias, p.Status)
		}
	}
}

// The wiring the chain test cannot see: the human gate and the final
// report read the criteria still open AFTER the trial, from the one node
// that knows them, on BOTH paths into review_gate. An earlier draft fed
// final_report the draft's stale hash on exactly this kind of edge, and
// neither the node tests (hand-chosen inputs) nor the dry run (fixtures
// with an empty residue) could see it.
func TestRgaaAuditReviewGateReadsTheTrialOnBothPaths(t *testing.T) {
	wf := compilePlanPhaseBot(t, "rgaa-audit")
	mapping := func(from, to, key string) string {
		t.Helper()
		for _, e := range wf.Edges {
			if e.From != from || e.To != to {
				continue
			}
			for _, m := range e.With {
				if m.Key == key {
					return m.Raw
				}
			}
			t.Fatalf("edge %s -> %s maps no %q", from, to, key)
		}
		t.Fatalf("no edge %s -> %s", from, to)
		return ""
	}
	for _, from := range []string{"budget_gate", "refute_apply"} {
		if got := mapping(from, "review_gate", "open_ids"); got != "{{outputs.fold_final.still_open_ids}}" {
			t.Errorf("%s -> review_gate maps open_ids from %q, want the fold's still_open_ids", from, got)
		}
	}
	for key, want := range map[string]string{"reopened": "{{outputs.refute_apply.reopened}}", "refute_degraded": "{{outputs.refute_apply.degraded}}", "refuted": "{{outputs.refute_apply.refuted}}"} {
		if got := mapping("refute_apply", "review_gate", key); got != want {
			t.Errorf("refute_apply -> review_gate maps %s from %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]string{"reopened": "{{outputs.budget_gate.no_reopened}}", "refute_degraded": "{{outputs.budget_gate.degraded_floor}}", "refuted": "{{outputs.budget_gate.zero}}"} {
		if got := mapping("budget_gate", "review_gate", key); got != want {
			t.Errorf("budget_gate -> review_gate maps %s from %q, want the gate's own floor %q", key, got, want)
		}
	}
	for _, key := range []string{"undecided_ids", "undecided", "refuted", "refute_degraded"} {
		if got := mapping("review_gate", "human_review", key); got != "{{outputs.review_gate."+key+"}}" {
			t.Errorf("review_gate -> human_review maps %s from %q, want review_gate's own %s", key, got, key)
		}
	}
	// Every later node reads the run directory prepare joined, never the var.
	for _, e := range wf.Edges {
		for _, m := range e.With {
			if m.Key == "run_dir" && m.Raw != "{{outputs.prepare.run_dir}}" {
				t.Errorf("edge %s -> %s maps run_dir from %q: only prepare knows the run's directory", e.From, e.To, m.Raw)
			}
		}
	}
}
