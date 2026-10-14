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
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
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

	t.Run("render_fails_the_layer_on_a_refused_sample_lint_before_any_scan", func(t *testing.T) {
		// A sample-less rc: the engine's lint refuses it. The layer is failed
		// (required) or degraded (auto) on THAT refusal — never read as an
		// empty list of missing page kinds and carried on into the scan.
		ws := rgaaCopyFixtureSite(t)
		rgaaWriteJSON(t, filepath.Join(ws, ".ultra11yrc.json"), map[string]any{"standard": "rgaa"})
		renderIn := func(dir, mode string) map[string]any {
			return in(map[string]any{"input.run_dir": dir, "input.workspace_dir": ws, "input.rendered": mode, "input.engine_pkg_dir": prep["engine_pkg_dir"], "input.serve_command": "", "input.serve_ready_url": "", "input.serve_timeout_seconds": 5})
		}
		req := rgaaRefused(t, "render", rgaaRunNode(t, scripts, "render", ws, env, renderIn(rgaaRunCopy(t, run), "required")), "RENDER_UNAVAILABLE")
		if !strings.HasPrefix(fmt.Sprint(req["reason"]), "sample check exit 2") {
			t.Fatalf("the required layer failed on %q, not on the refused sample lint", req["reason"])
		}
		autoRun := rgaaRunCopy(t, run)
		auto := rgaaOK(t, "render", rgaaRunNode(t, scripts, "render", ws, env, renderIn(autoRun, "auto")))
		var layer map[string]any
		rgaaReadJSON(t, filepath.Join(autoRun, "layers", "rendered.json"), &layer)
		if auto["status"] != "degraded" || !strings.HasPrefix(fmt.Sprint(auto["reason"]), "sample check exit 2") || layer["status"] != "degraded" {
			t.Fatalf("auto: status=%v reason=%q layer=%v (want the layer degraded on the refused sample lint)", auto["status"], auto["reason"], layer)
		}
		// An invalid block: the lint exits 2 WITH a JSON body naming the
		// issues, which the reason carries; the scan never runs.
		rgaaWriteJSON(t, filepath.Join(ws, ".ultra11yrc.json"), map[string]any{"standard": "rgaa", "sample": map[string]any{"pages": []any{map[string]any{"id": "accueil"}}}})
		bad := rgaaRefused(t, "render", rgaaRunNode(t, scripts, "render", ws, env, renderIn(rgaaRunCopy(t, run), "required")), "RENDER_UNAVAILABLE")
		if reason := fmt.Sprint(bad["reason"]); !strings.HasPrefix(reason, "sample check exit 2") || !strings.Contains(reason, "sample.pages[0].name") {
			t.Fatalf("an invalid sample block failed the layer on %q, want the lint's refusal naming the issue", reason)
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

	t.Run("a_recommendation_never_costs_its_verdict_the_unanchored_ones_are_dropped_and_counted", func(t *testing.T) {
		// The engine refuses a whole verdict over one recommendation that is
		// not an object naming a file, or that does not ground. One guard per
		// criterion: the fragment drops what is not an anchored object, the
		// merge drops what stands on no anchor of the criterion, and a kept
		// recommendation takes its anchor's own snippet.
		dir := rgaaRunCopy(t, run)
		var b batch
		var withEv []string
		for _, cand := range round0 {
			withEv = withEv[:0]
			for _, id := range cand.IDs {
				if len(rgaaEvidenceOf(t, itemsDir, id)) > 0 {
					withEv = append(withEv, id)
				}
			}
			if len(withEv) >= 3 {
				b = cand
				break
			}
		}
		if len(withEv) < 3 {
			t.Fatal("no batch carries three criteria with evidence")
		}
		freeID, offID, anchoredID := withEv[0], withEv[1], withEv[2]
		anchor := rgaaEvidenceOf(t, itemsDir, anchoredID)[0]
		off := rgaaEvidenceOf(t, itemsDir, offID)[0]
		const advice = "Recommandation ancrée (fixture)."
		var verdicts []any
		for _, id := range b.IDs {
			v := rgaaFixtureVerdict(t, itemsDir, id)
			switch id {
			case freeID:
				v["recommendations"] = []any{"Ajouter un lien d'évitement (texte libre, sans ancre)."}
			case offID:
				v["recommendations"] = []any{map[string]any{"file": off["file"], "line": rgaaInt(off["line"]) + 1000, "message": "Hors de l'évidence du critère (fixture)."}}
			case anchoredID:
				v["recommendations"] = []any{
					map[string]any{"file": anchor["file"], "line": anchor["line"], "snippet": "un extrait qui n'existe pas dans le fichier", "message": advice},
					map[string]any{"file": anchor["file"], "line": anchor["line"]},
				}
			}
			verdicts = append(verdicts, v)
		}
		frag := writeFragment(t, dir, b, verdicts)
		merged := mergeVerdicts(t, dir, 0)
		// The guarantees first, on what the engine folded: no verdict refused
		// over a recommendation, and exactly the anchored one reported.
		probe := foldProbe(t, dir, 0)
		pending := rgaaStrings(probe["pending_ids"])
		_, _, issues := readPending(t, dir)
		for _, id := range []string{freeID, offID, anchoredID} {
			if rgaaContains(pending, id) {
				t.Fatalf("the engine refused %s's verdict over a recommendation: %s", id, issues[id])
			}
		}
		var probed struct {
			Findings []map[string]any `json:"findings"`
		}
		rgaaReadJSON(t, filepath.Join(dir, "probe", "r0", "audit-latest.json"), &probed)
		var advisories []map[string]any
		for _, f := range probed.Findings {
			if f["advisory"] == true && rgaaContains([]string{freeID, offID, anchoredID}, fmt.Sprint(f["criteriaId"])) {
				advisories = append(advisories, f)
			}
		}
		if len(advisories) != 1 || advisories[0]["criteriaId"] != anchoredID || advisories[0]["message"] != advice {
			t.Fatalf("the folded audit carries %d advisory finding(s) on the three criteria (%v), want exactly %s's anchored recommendation", len(advisories), advisories, anchoredID)
		}
		if rgaaInt(frag["recommendations_dropped"]) != 2 || rgaaInt(frag["malformed"]) != 0 || rgaaInt(frag["written"]) != len(b.IDs) {
			t.Fatalf("fragment: %v (want 2 recommendations dropped — the free text and the one without a message —, nothing malformed, %d written)", frag, len(b.IDs))
		}
		if rgaaInt(merged["recommendations_dropped"]) != 3 || rgaaInt(merged["citation_rejected"]) != 0 {
			t.Fatalf("merge: %v (want 3 recommendations dropped — the fragment's two and the one off the criterion's evidence —, no citation rejected)", merged)
		}
		var adj struct {
			Items []struct {
				CriteriaID      string           `json:"criteriaId"`
				Recommendations []map[string]any `json:"recommendations"`
			} `json:"items"`
		}
		rgaaReadJSON(t, filepath.Join(dir, "adjudication.json"), &adj)
		for _, it := range adj.Items {
			want := 0
			if it.CriteriaID == anchoredID {
				want = 1
			}
			if len(it.Recommendations) != want {
				t.Fatalf("adjudication.json carries %d recommendation(s) on %s, want %d", len(it.Recommendations), it.CriteriaID, want)
			}
			if want == 1 && (it.Recommendations[0]["snippet"] != anchor["snippet"] || it.Recommendations[0]["message"] != advice) {
				t.Fatalf("the kept recommendation is %v, want the anchor's own snippet and the agent's message", it.Recommendations[0])
			}
		}
	})
	if t.Failed() {
		return
	}

	// recommendationRound writes one batch whose verdict on `target` carries
	// `rec` as its recommendations value, merges and probes it on a copy, and
	// returns the copy, the fragment and merge outputs and the advisories the
	// folded copy carries for `target`.
	recommendationRound := func(t *testing.T, b batch, target string, rec any) (string, map[string]any, map[string]any, []map[string]any) {
		t.Helper()
		dir := rgaaRunCopy(t, run)
		var verdicts []any
		for _, id := range b.IDs {
			v := rgaaFixtureVerdict(t, itemsDir, id)
			if id == target {
				v["recommendations"] = rec
			}
			verdicts = append(verdicts, v)
		}
		frag := writeFragment(t, dir, b, verdicts)
		merged := mergeVerdicts(t, dir, 0)
		probe := foldProbe(t, dir, 0)
		if rgaaContains(rgaaStrings(probe["pending_ids"]), target) {
			_, _, issues := readPending(t, dir)
			t.Fatalf("the engine refused %s's verdict over a recommendation: %s", target, issues[target])
		}
		var probed struct {
			Findings []map[string]any `json:"findings"`
		}
		rgaaReadJSON(t, filepath.Join(dir, "probe", "r0", "audit-latest.json"), &probed)
		var advisories []map[string]any
		for _, f := range probed.Findings {
			if f["advisory"] == true && f["criteriaId"] == target {
				advisories = append(advisories, f)
			}
		}
		return dir, frag, merged, advisories
	}

	t.Run("a_recommendation_that_is_not_an_array_of_messaged_objects_is_dropped_and_counted_never_folded", func(t *testing.T) {
		// What a model actually returns beside the contract: one string, one
		// lone object, an object whose message is blank. Each is dropped by
		// the fragment and counted; none reaches the engine.
		b := round0[0]
		var target string
		for _, id := range b.IDs {
			if len(rgaaEvidenceOf(t, itemsDir, id)) > 0 {
				target = id
				break
			}
		}
		if target == "" {
			t.Fatalf("batch %s carries no criterion with evidence", b.ID)
		}
		a := rgaaEvidenceOf(t, itemsDir, target)[0]
		for name, rec := range map[string]any{
			"a string":           "Ajouter un lien d'évitement (une chaîne, pas un tableau).",
			"a lone object":      map[string]any{"file": a["file"], "line": a["line"], "snippet": a["snippet"], "message": "Un objet seul, pas un tableau."},
			"a blank message":    []any{map[string]any{"file": a["file"], "line": a["line"], "snippet": a["snippet"], "message": "   "}},
			"a message not text": []any{map[string]any{"file": a["file"], "line": a["line"], "snippet": a["snippet"], "message": 42}},
		} {
			_, frag, merged, advisories := recommendationRound(t, b, target, rec)
			if len(advisories) != 0 || rgaaInt(frag["recommendations_dropped"]) != 1 || rgaaInt(merged["recommendations_dropped"]) != 1 {
				t.Fatalf("%s: %d advisory finding(s) folded, fragment dropped %v, merge counted %v (want none folded, 1 dropped and counted)", name, len(advisories), frag["recommendations_dropped"], merged["recommendations_dropped"])
			}
		}
	})

	t.Run("a_recommendation_on_another_criterion_s_anchor_is_dropped_by_the_merge_itself", func(t *testing.T) {
		// A real, groundable anchor of a SIBLING criterion: the engine would
		// fold it as an advisory of the wrong criterion. Only the merge's own
		// evidence check stands between them.
		var b batch
		var target string
		var sibling map[string]any
		for _, cand := range round0 {
			for _, x := range cand.IDs {
				own := map[string]bool{}
				for _, e := range rgaaEvidenceOf(t, itemsDir, x) {
					own[fmt.Sprint(e["file"])+":"+fmt.Sprint(e["line"])] = true
				}
				if len(own) == 0 {
					continue
				}
				for _, y := range cand.IDs {
					for _, e := range rgaaEvidenceOf(t, itemsDir, y) {
						if y != x && sibling == nil && !own[fmt.Sprint(e["file"])+":"+fmt.Sprint(e["line"])] {
							b, target, sibling = cand, x, e
						}
					}
				}
				if sibling != nil {
					break
				}
			}
			if sibling != nil {
				break
			}
		}
		if sibling == nil {
			t.Fatal("no batch holds a criterion and a sibling anchor outside its own evidence")
		}
		_, _, merged, advisories := recommendationRound(t, b, target, []any{map[string]any{"file": sibling["file"], "line": sibling["line"], "snippet": sibling["snippet"], "message": "Sur l'ancre d'un autre critère (fixture)."}})
		if len(advisories) != 0 || rgaaInt(merged["recommendations_dropped"]) != 1 {
			t.Fatalf("a recommendation on another criterion's anchor (%v:%v) was kept on %s: %d advisory finding(s), %v dropped", sibling["file"], sibling["line"], target, len(advisories), merged["recommendations_dropped"])
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

	// The run as the adjudication left it, before the real fold: the start
	// of the agent-non-conformity trial below.
	preFold := rgaaRunCopy(t, run)

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

	t.Run("a_stored_recommendation_never_refuses_its_replayed_verdict_and_a_verdict_the_gate_refuses_is_named", func(t *testing.T) {
		// The engine re-grounds a stored recommendation at replay with no
		// fallback: one that no longer grounds (its element edited, as advised)
		// would send a still-valid verdict back to « à évaluer ». And a fresh
		// entry the engine's gate refuses is named and leaves the copy.
		var ledger map[string]any
		rgaaReadJSON(t, fmt.Sprint(fold["ledger_path"]), &ledger)
		entries, _ := ledger["entries"].([]any)
		var advised, broken, hand, lone, nothing string
		for _, e := range entries {
			m := e.(map[string]any)
			cites, _ := m["citations"].([]any)
			if m["verdict"] != "C" || len(cites) == 0 {
				continue
			}
			c := cites[0].(map[string]any)
			switch {
			case advised == "":
				advised = fmt.Sprint(m["criteriaId"])
				m["recommendations"] = []any{
					map[string]any{"file": c["file"], "line": c["line"], "snippet": "<p>un élément depuis modifié</p>", "message": "Recommandation (fixture)."},
					map[string]any{"file": c["file"], "line": c["line"], "snippet": "<p>un autre élément depuis modifié</p>", "message": "Seconde recommandation (fixture)."},
					map[string]any{"file": c["file"], "line": c["line"], "snippet": "<p>un troisième élément depuis modifié</p>", "message": "Troisième recommandation (fixture)."},
				}
			case broken == "":
				broken = fmt.Sprint(m["criteriaId"])
				m["verdict"] = "NC"
				delete(m, "citations")
			case hand == "":
				// A hand-edited ledger: a recommendation that is not an array.
				hand = fmt.Sprint(m["criteriaId"])
				m["recommendations"] = "Ajouter un texte alternatif (édité à la main)."
			case lone == "":
				lone = fmt.Sprint(m["criteriaId"])
				m["recommendations"] = map[string]any{"file": c["file"], "line": c["line"], "message": "Un objet seul (édité à la main)."}
			case nothing == "":
				nothing = fmt.Sprint(m["criteriaId"])
				m["recommendations"] = nil
			}
		}
		if advised == "" || broken == "" || hand == "" || lone == "" || nothing == "" {
			t.Fatal("the ledger carries fewer than five cited conformities")
		}
		path := filepath.Join(t.TempDir(), "rgaa.json")
		rgaaWriteJSON(t, path, ledger)
		base3 := filepath.Join(t.TempDir(), "scratch")
		p3 := rgaaOK(t, "prepare", rgaaRunNode(t, scripts, "prepare", site, env, prepareIn(map[string]any{"input.run_dir": base3})))
		run3 := fmt.Sprint(p3["run_dir"])
		rgaaOK(t, "static_audit", rgaaRunNode(t, scripts, "static_audit", site, env, in(map[string]any{"input.run_dir": run3, "input.max_files": 4000, "input.audit_args": p3["audit_args"]})))
		rep := rgaaOK(t, "ledger_replay", rgaaRunNode(t, scripts, "ledger_replay", site, env, in(map[string]any{"input.run_dir": run3, "input.ledger_present": true, "input.ledger_path": path})))
		w := rgaaOK(t, "worklist", rgaaRunNode(t, scripts, "worklist", site, env, in(map[string]any{"input.run_dir": run3})))
		open := rgaaStrings(w["pending_ids"])
		if rgaaContains(open, advised) || !rgaaContains(open, broken) || len(open) != 1 {
			t.Fatalf("after the replay %v is open; want only %s (the gate refused it), never %s (its recommendation no longer grounds)", open, broken, advised)
		}
		// Five recommendations over four entries carrying the key: a count of
		// entries, or a null counted as one, would read otherwise.
		if rgaaInt(rep["refused"]) != 1 || rgaaInt(rep["recommendations_stripped"]) != 5 || rgaaInt(rep["replayed"]) != len(ids)-1 || rep["reason"] != "" {
			t.Fatalf("replay: %v (want 1 refused, 5 recommendations stripped — three, a string, a lone object, none for null —, %d replayed, no reason on success)", rep, len(ids)-1)
		}
		// The annex reads the layer: it names the refused verdict.
		var layer struct {
			Status   string   `json:"status"`
			Refused  []string `json:"refused"`
			Stripped int      `json:"recommendations_stripped"`
		}
		rgaaReadJSON(t, filepath.Join(run3, "layers", "ledger.json"), &layer)
		if layer.Status != "degraded" || len(layer.Refused) != 1 || layer.Refused[0] != broken || layer.Stripped != 5 {
			t.Fatalf("ledger layer %+v, want degraded, refused [%s], 5 recommendations stripped", layer, broken)
		}
		var copyLedger struct {
			Entries []map[string]any `json:"entries"`
		}
		rgaaReadJSON(t, filepath.Join(run3, "ledger", "rgaa.json"), &copyLedger)
		for _, e := range copyLedger.Entries {
			if _, has := e["recommendations"]; e["criteriaId"] == broken || has {
				t.Fatalf("the replayed copy keeps %v (a refused verdict, or a recommendation)", e["criteriaId"])
			}
		}
	})

	t.Run("a_ledger_the_engine_cannot_replay_is_LEDGER_INVALID_with_its_error_and_one_it_writes_replays", func(t *testing.T) {
		// The guarantee, not a list of field shapes: whatever stored value the
		// engine's replay cannot read kills it without JSON, and the node turns
		// that into LEDGER_INVALID carrying the engine's own message — never an
		// untyped death, never a refusal of a ledger the engine reads, never a
		// copy left for the final fold to merge.
		// A fresh run, before any replay: every entry is open, so the engine
		// reads every stored field (a folded run would set them all aside).
		base5 := filepath.Join(t.TempDir(), "scratch")
		p5 := rgaaOK(t, "prepare", rgaaRunNode(t, scripts, "prepare", site, env, prepareIn(map[string]any{"input.run_dir": base5})))
		probe := fmt.Sprint(p5["run_dir"])
		rgaaOK(t, "static_audit", rgaaRunNode(t, scripts, "static_audit", site, env, in(map[string]any{"input.run_dir": probe, "input.max_files": 4000, "input.audit_args": p5["audit_args"]})))
		var ledger map[string]any
		rgaaReadJSON(t, fmt.Sprint(fold["ledger_path"]), &ledger)
		// `typed`: a shape the engine reads on some entries and not on others —
		// whatever it does here, the node answers typed, never dies.
		const crashes, bot, replays, typed = "engine crash", "bot refusal", "replays", "typed"
		for name, c := range map[string]struct {
			edit  func(m map[string]any)
			add   any // an entry to add: first when `first`, else last
			first bool
			want  string
		}{
			"citations as a string":             {edit: func(m map[string]any) { m["citations"] = "index.html:12" }, want: crashes},
			"findings as a string":              {edit: func(m map[string]any) { m["findings"] = "voir index.html" }, want: crashes},
			"a justification that is an object": {edit: func(m map[string]any) { m["justification"] = map[string]any{"fr": "conforme"} }, want: crashes},
			"a non-conformity whose reference is a number": {edit: func(m map[string]any) {
				c := m["citations"].([]any)[0].(map[string]any)
				m["verdict"] = "NC"
				m["findings"] = []any{map[string]any{"file": c["file"], "line": c["line"], "snippet": c["snippet"], "message": "x", "normativeRef": 5}}
				delete(m, "citations")
			}, want: crashes},
			"citations as bare refs":          {edit: func(m map[string]any) { m["citations"] = []any{"index.html:12"} }, want: typed},
			"citations holding a null":        {edit: func(m map[string]any) { m["citations"] = []any{nil} }, want: typed},
			"an entry that is a string":       {add: "1.1", want: bot},
			"an entry without a criterion id": {add: map[string]any{"verdict": "C", "justification": "x"}, want: bot},
			"a first entry that is a number":  {add: 5, first: true, want: bot},
			"null findings":                   {edit: func(m map[string]any) { m["findings"] = nil }, want: replays},
			"a finding with an empty file on a conformity": {edit: func(m map[string]any) {
				m["findings"] = []any{map[string]any{"file": "", "line": 1, "message": "écrite par un autre orchestrateur"}}
			}, want: replays},
			"a citation whose reference is a number": {edit: func(m map[string]any) {
				m["citations"].([]any)[0].(map[string]any)["normativeRef"] = 5
			}, want: replays},
			"a citation whose snippet is null": {edit: func(m map[string]any) {
				m["citations"].([]any)[0].(map[string]any)["snippet"] = nil
			}, want: replays},
		} {
			var doc map[string]any
			raw, _ := json.Marshal(ledger)
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			entries := doc["entries"].([]any)
			at := len(entries)
			switch {
			case c.add != nil && c.first:
				doc["entries"], at = append([]any{c.add}, entries...), 0
			case c.add != nil:
				doc["entries"] = append(entries, c.add)
			default:
				c.edit(entries[0].(map[string]any))
			}
			path := filepath.Join(t.TempDir(), "rgaa.json")
			rgaaWriteJSON(t, path, doc)
			dir := rgaaRunCopy(t, probe)
			r := rgaaRunNode(t, scripts, "ledger_replay", site, env, in(map[string]any{"input.run_dir": dir, "input.ledger_present": true, "input.ledger_path": path}))
			if c.want == typed {
				if r.Code != 0 || (r.Out["ok"] != true && r.Out["fail_code"] != "LEDGER_INVALID") {
					t.Fatalf("%s: exit %d, %v — want a replay or the typed refusal, never an untyped death\n%s", name, r.Code, r.Out, r.Stderr)
				}
				continue
			}
			if c.want == replays {
				if out := rgaaOK(t, "ledger_replay", r); rgaaInt(out["refused"]) != 0 {
					t.Fatalf("%s: a ledger the engine reads was refused in part: %v", name, out)
				}
				continue
			}
			if r.Code != 0 || r.Out["ok"] != false || r.Out["fail_code"] != "LEDGER_INVALID" {
				t.Fatalf("%s: exit %d, %v %v — want the typed refusal LEDGER_INVALID\n%s", name, r.Code, r.Out["fail_code"], r.Out["reason"], r.Stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, "ledger", "rgaa.json")); err == nil {
				t.Fatalf("%s: a refused replay left a ledger copy for the final fold to merge", name)
			}
			// The ledger's own path names the test directory: match the rest.
			reason := strings.ReplaceAll(fmt.Sprint(r.Out["reason"]), path, "<ledger>")
			switch c.want {
			case bot:
				if !strings.Contains(reason, fmt.Sprintf("entry #%d ", at)) {
					t.Fatalf("%s: the refusal %q does not name entry #%d", name, reason, at)
				}
			case crashes:
				if !strings.Contains(reason, "could not replay the ledger at <ledger> (exit 1, no JSON): ") || !strings.Contains(reason, "is not a function") {
					t.Fatalf("%s: the refusal %q does not carry the engine's own message", name, reason)
				}
			}
		}

		// A system error is the environment's, not the ledger's: an untyped,
		// resumable death that leaves the real audit whole, never LEDGER_INVALID
		// on a ledger the engine reads. Two codes: the replay's output directory
		// taken by a file (EEXIST), then unwritable (EACCES).
		dir := rgaaRunCopy(t, probe)
		before := rgaaFileSHA(t, filepath.Join(dir, "audit-latest.json"))
		if err := os.WriteFile(filepath.Join(dir, "replay"), []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		replay := func() rgaaNodeResult {
			return rgaaRunNode(t, scripts, "ledger_replay", site, env, in(map[string]any{"input.run_dir": dir, "input.ledger_present": true, "input.ledger_path": fold["ledger_path"]}))
		}
		if r := replay(); r.Code == 0 || !strings.Contains(r.Stderr, "failed on its environment, not on the ledger") {
			t.Fatalf("a replay directory taken by a file was reported as %v (exit %d): %s", r.Out, r.Code, r.Stderr)
		}
		if err := os.Remove(filepath.Join(dir, "replay")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "replay"), 0o555); err != nil {
			t.Fatal(err)
		}
		// root writes through the mode bits: the EACCES half needs a user.
		if os.Geteuid() != 0 {
			if r := replay(); r.Code == 0 || !strings.Contains(r.Stderr, "failed on its environment, not on the ledger") {
				t.Fatalf("an unwritable replay directory was reported as %v (exit %d): %s", r.Out, r.Code, r.Stderr)
			}
		}
		if rgaaFileSHA(t, filepath.Join(dir, "audit-latest.json")) != before {
			t.Fatal("a replay that died on its environment changed the real audit")
		}
		// The resume, once the environment is fixed, replays whole.
		if err := os.Chmod(filepath.Join(dir, "replay"), 0o755); err != nil {
			t.Fatal(err)
		}
		if out := rgaaOK(t, "ledger_replay", replay()); rgaaInt(out["replayed"]) != len(ids) || recorded(dir) != rgaaFileSHA(t, filepath.Join(dir, "audit-latest.json")) {
			t.Fatalf("the resumed replay: %v", out)
		}
		// A run rewound to the replay keeps the copy its own fold wrote: the
		// audit is still folded, and the trial and the delivery read that copy.
		left := rgaaRunCopy(t, run)
		folded := rgaaFileSHA(t, filepath.Join(left, "ledger", "rgaa.json"))
		rgaaOK(t, "ledger_replay", rgaaRunNode(t, scripts, "ledger_replay", site, env, in(map[string]any{"input.run_dir": left, "input.ledger_present": false, "input.ledger_path": "/nowhere"})))
		if _, err := os.Stat(filepath.Join(left, "ledger", "rgaa.json")); err != nil || rgaaFileSHA(t, filepath.Join(left, "ledger", "rgaa.json")) != folded {
			t.Fatalf("a replay that found no ledger dropped or rewrote the copy the run's own fold wrote: %v", err)
		}
		// A fresh audit starts a fresh generation: the copy an earlier fold of
		// the run merged goes with the audit it belonged to.
		rgaaOK(t, "static_audit", rgaaRunNode(t, scripts, "static_audit", site, env, in(map[string]any{"input.run_dir": left, "input.max_files": 4000, "input.audit_args": prep["audit_args"]})))
		if _, err := os.Stat(filepath.Join(left, "ledger", "rgaa.json")); err == nil {
			t.Fatal("a fresh audit kept the ledger copy of the audit it replaced")
		}
	})

	t.Run("a_replay_whose_every_fresh_verdict_the_gate_refuses_still_names_its_stale_and_obsolete_entries", func(t *testing.T) {
		// Every fresh entry refused: the engine answers exit 1 and nests its
		// replay accounting under `replay`. The node reads it there, and the
		// copy keeps nothing that did not replay.
		var ledger map[string]any
		rgaaReadJSON(t, fmt.Sprint(fold["ledger_path"]), &ledger)
		entries, _ := ledger["entries"].([]any)
		var broken, stale map[string]any
		for _, e := range entries {
			m := e.(map[string]any)
			cites, _ := m["citations"].([]any)
			if m["verdict"] != "C" || len(cites) == 0 {
				continue
			}
			if broken == nil {
				broken = m
			} else if stale == nil {
				stale = m
			}
		}
		var doc packAuditDoc
		rgaaReadJSON(t, filepath.Join(run, "bank", "audit-static.json"), &doc)
		decided := ""
		for _, c := range doc.Criteria {
			if !rgaaContains(ids, c.ID) {
				decided = c.ID
				break
			}
		}
		if broken == nil || stale == nil || decided == "" {
			t.Fatal("the fixture lacks two cited conformities or an engine-decided criterion")
		}
		broken["verdict"] = "NC"
		delete(broken, "citations")
		stale["evidenceFingerprint"] = "sha256:00000000000000000000000000000000"
		delete(stale, "evidenceAnchors")
		obsolete := maps.Clone(stale)
		obsolete["criteriaId"] = decided
		ledger["entries"] = []any{broken, stale, obsolete}
		path := filepath.Join(t.TempDir(), "rgaa.json")
		rgaaWriteJSON(t, path, ledger)
		base4 := filepath.Join(t.TempDir(), "scratch")
		p4 := rgaaOK(t, "prepare", rgaaRunNode(t, scripts, "prepare", site, env, prepareIn(map[string]any{"input.run_dir": base4})))
		run4 := fmt.Sprint(p4["run_dir"])
		rgaaOK(t, "static_audit", rgaaRunNode(t, scripts, "static_audit", site, env, in(map[string]any{"input.run_dir": run4, "input.max_files": 4000, "input.audit_args": p4["audit_args"]})))
		// An earlier attempt's replay output is still on disk: exit 1 writes no
		// audit, so nothing may be moved over the real one.
		if err := os.MkdirAll(filepath.Join(run4, "replay"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(run4, "replay", "audit-latest.json"), []byte(`{"stale":"an earlier attempt"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		before := rgaaFileSHA(t, filepath.Join(run4, "audit-latest.json"))
		rep := rgaaOK(t, "ledger_replay", rgaaRunNode(t, scripts, "ledger_replay", site, env, in(map[string]any{"input.run_dir": run4, "input.ledger_present": true, "input.ledger_path": path})))
		if rgaaFileSHA(t, filepath.Join(run4, "audit-latest.json")) != before || recorded(run4) != before {
			t.Fatal("an exit-1 replay moved an earlier attempt's output over the real audit")
		}
		if rgaaInt(rep["refused"]) != 1 || rgaaInt(rep["stale"]) != 1 || rgaaInt(rep["obsolete"]) != 1 || rgaaInt(rep["missing"]) != len(ids)-2 || rgaaInt(rep["replayed"]) != 0 {
			t.Fatalf("replay: %v (want 1 refused, 1 stale, 1 obsolete, %d missing, 0 replayed)", rep, len(ids)-2)
		}
		var copyLedger struct {
			Entries []map[string]any `json:"entries"`
		}
		rgaaReadJSON(t, filepath.Join(run4, "ledger", "rgaa.json"), &copyLedger)
		if len(copyLedger.Entries) != 0 {
			t.Fatalf("the replayed copy keeps %d entries that did not replay", len(copyLedger.Entries))
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

	t.Run("a_trial_the_engine_refuses_in_part_is_reported_as_pruned_and_its_write_chained", func(t *testing.T) {
		// With --prune the engine rewrites the audit and repairs the ledger
		// BEFORE it judges grounding: one retained claim whose anchor does not
		// ground makes it answer ok:false over a pruned document. The node
		// chains the engine's write and reports what the engine pruned.
		dir := rgaaRunCopy(t, run)
		var d map[string]any
		rgaaReadJSON(t, filepath.Join(dir, "draft", "draft.json"), &d)
		for _, k := range []string{"todo_path", "draft_path"} {
			rel, err := filepath.Rel(run, fmt.Sprint(d[k]))
			if err != nil || strings.HasPrefix(rel, "..") {
				t.Fatalf("draft.json %s %v is not under the run directory", k, d[k])
			}
			d[k] = filepath.Join(dir, rel)
		}
		rgaaWriteJSON(t, filepath.Join(dir, "draft", "draft.json"), d)
		var todo []map[string]any
		rgaaReadJSON(t, fmt.Sprint(d["todo_path"]), &todo)
		ungrounded := -1
		for i, it := range todo {
			if it["kind"] != "c" && it["verdict"] == "supported" && fmt.Sprint(it["file"]) != "" {
				ungrounded = i
				break
			}
		}
		if ungrounded < 0 {
			t.Fatal("the trial carries no supported non-conformity")
		}
		todo[ungrounded]["snippet"] = "<p>un extrait qui n'existe nulle part dans le fichier</p>"
		rgaaWriteJSON(t, fmt.Sprint(d["todo_path"]), todo)
		before := rgaaFileSHA(t, filepath.Join(dir, "audit-latest.json"))
		out := rgaaOK(t, "refute_apply", rgaaRunNode(t, scripts, "refute_apply", site, env, in(map[string]any{"input.run_dir": dir, "input.complete": true})))
		if out["applied"] != true || out["degraded"] != true || !strings.Contains(fmt.Sprint(out["reason"]), "does not ground") {
			t.Fatalf("refute_apply: applied=%v degraded=%v reason=%q (want the prune reported as applied and the grounding refusal named)", out["applied"], out["degraded"], out["reason"])
		}
		if cleared := rgaaStrings(out["cleared"]); len(cleared) != 1 || cleared[0] != refutedC || !rgaaContains(rgaaStrings(out["reopened"]), refutedC) {
			t.Fatalf("cleared %v reopened %v, want the refuted conformity %s the engine cleared", out["cleared"], out["reopened"], refutedC)
		}
		// The next reader of the audit accepts the engine's write.
		final := rgaaOK(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, in(map[string]any{"input.run_dir": dir, "input.html_report": false, "input.report_dir": "", "input.require_complete": false, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": ""})))
		after := rgaaFileSHA(t, filepath.Join(dir, "audit-latest.json"))
		if after == before || out["audit_sha256"] != after {
			t.Fatalf("the trial did not move the audit, or the node reported another hash: before=%s after=%s out=%v", before, after, out["audit_sha256"])
		}
		annex, err := os.ReadFile(fmt.Sprint(final["annex_path"]))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"| Contre-lecture | degraded | 1 réfutée(s)", "conformités retirées : " + refutedC, "refus partiel du moteur"} {
			if !strings.Contains(string(annex), want) {
				t.Fatalf("the annex lacks %q for an applied, partly refused trial:\n%s", want, annex)
			}
		}
	})

	t.Run("the_annex_names_a_trial_that_was_not_applied_by_its_reason", func(t *testing.T) {
		dir := rgaaRunCopy(t, run)
		out := rgaaOK(t, "refute_apply", rgaaRunNode(t, scripts, "refute_apply", site, env, in(map[string]any{"input.run_dir": dir, "input.complete": false})))
		final := rgaaOK(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, in(map[string]any{"input.run_dir": dir, "input.html_report": false, "input.report_dir": "", "input.require_complete": false, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": ""})))
		annex, err := os.ReadFile(fmt.Sprint(final["annex_path"]))
		if err != nil {
			t.Fatal(err)
		}
		row := ""
		for _, l := range strings.Split(string(annex), "\n") {
			if strings.HasPrefix(l, "| Contre-lecture |") {
				row = l
			}
		}
		if out["applied"] != false || strings.Contains(row, "undefined") || strings.Contains(row, "procès appliqué") || !strings.Contains(row, fmt.Sprint(out["reason"])) {
			t.Fatalf("the annex misreports the trial that was not applied: %s", row)
		}
		// A layer that does not say it was applied — written by an earlier
		// version of the node, on a run resumed across an upgrade — renders
		// its reason, never counts it does not carry.
		rgaaWriteJSON(t, filepath.Join(dir, "layers", "refutation.json"), map[string]any{"layer": "refutation", "status": "degraded", "reason": "the engine did not apply the trial"})
		final = rgaaOK(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, in(map[string]any{"input.run_dir": dir, "input.html_report": false, "input.report_dir": "", "input.require_complete": false, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": ""})))
		annex, _ = os.ReadFile(fmt.Sprint(final["annex_path"]))
		if !strings.Contains(string(annex), "| Contre-lecture | degraded | the engine did not apply the trial |") {
			t.Fatalf("a layer without `applied` is not rendered by its reason:\n%s", annex)
		}
		// No layer at all: the trial was skipped, for one of its three causes.
		if err := os.Remove(filepath.Join(dir, "layers", "refutation.json")); err != nil {
			t.Fatal(err)
		}
		final = rgaaOK(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, in(map[string]any{"input.run_dir": dir, "input.html_report": false, "input.report_dir": "", "input.require_complete": false, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": ""})))
		annex, _ = os.ReadFile(fmt.Sprint(final["annex_path"]))
		if !strings.Contains(string(annex), "| Contre-lecture | ignorée | non lancée (désactivée, budget, ou aucune revendication à contre-lire) |") {
			t.Fatalf("a skipped trial does not name its three causes:\n%s", annex)
		}
		// A degraded layer that carries no reason renders the fallback, never
		// `undefined`.
		rgaaWriteJSON(t, filepath.Join(dir, "layers", "refutation.json"), map[string]any{"layer": "refutation", "status": "degraded"})
		final = rgaaOK(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, in(map[string]any{"input.run_dir": dir, "input.html_report": false, "input.report_dir": "", "input.require_complete": false, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": ""})))
		annex, _ = os.ReadFile(fmt.Sprint(final["annex_path"]))
		if !strings.Contains(string(annex), "| Contre-lecture | degraded | procès non appliqué (raison non consignée) |") {
			t.Fatalf("a reasonless degraded layer does not render the fallback:\n%s", annex)
		}
		// The same era's ok layer — an applied trial, no `applied`, no reason —
		// renders its counts.
		rgaaWriteJSON(t, filepath.Join(dir, "layers", "refutation.json"), map[string]any{"layer": "refutation", "status": "ok", "refuted": 3, "unsupported": 1, "cleared": []any{"3.1"}, "removed_findings": 0, "reopened": []any{"3.1"}, "skipped_engine": 2})
		final = rgaaOK(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, in(map[string]any{"input.run_dir": dir, "input.html_report": false, "input.report_dir": "", "input.require_complete": false, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": ""})))
		annex, _ = os.ReadFile(fmt.Sprint(final["annex_path"]))
		if !strings.Contains(string(annex), "| Contre-lecture | ok | 3 réfutée(s), 1 non tranchée(s), conformités retirées : 3.1, constats moteur non touchés : 2 |") {
			t.Fatalf("an ok layer without `applied` does not render its counts:\n%s", annex)
		}
	})

	t.Run("refuted_engine_non_conformities_are_reported_as_left_untouched", func(t *testing.T) {
		dir := rgaaRunCopy(t, run)
		var d map[string]any
		rgaaReadJSON(t, filepath.Join(dir, "draft", "draft.json"), &d)
		for _, k := range []string{"todo_path", "draft_path"} {
			rel, err := filepath.Rel(run, fmt.Sprint(d[k]))
			if err != nil || strings.HasPrefix(rel, "..") {
				t.Fatalf("draft.json %s %v is not under the run directory", k, d[k])
			}
			d[k] = filepath.Join(dir, rel)
		}
		rgaaWriteJSON(t, filepath.Join(dir, "draft", "draft.json"), d)
		var todo []map[string]any
		rgaaReadJSON(t, fmt.Sprint(d["todo_path"]), &todo)
		n := 0
		for _, it := range todo {
			if it["kind"] != "c" && it["verdict"] == "supported" {
				it["verdict"], it["note"] = "refuted", "réfuté (fixture)"
				n++
			}
		}
		if n == 0 {
			t.Fatal("the trial carries no supported non-conformity")
		}
		rgaaWriteJSON(t, fmt.Sprint(d["todo_path"]), todo)
		out := rgaaOK(t, "refute_apply", rgaaRunNode(t, scripts, "refute_apply", site, env, in(map[string]any{"input.run_dir": dir, "input.complete": true})))
		if rgaaInt(out["skipped_engine"]) != n || rgaaInt(out["removed_findings"]) != 0 {
			t.Fatalf("%d refuted engine non-conformities: skipped_engine=%v removed_findings=%v (the engine never prunes its own findings on a reader's say-so)", n, out["skipped_engine"], out["removed_findings"])
		}
	})

	t.Run("a_refuted_agent_non_conformity_is_reported_as_removed_and_its_criterion_reopened", func(t *testing.T) {
		// One conformity of the adjudication turned into an agent NC anchored
		// on its own evidence, folded, drafted, then refuted by the trial: the
		// engine removes the agent's finding and reopens the criterion.
		var ncID string
		var dir string
		var adj map[string]any
		rgaaReadJSON(t, filepath.Join(preFold, "adjudication.json"), &adj)
		for _, it := range adj["items"].([]any) {
			m := it.(map[string]any)
			cites, _ := m["citations"].([]any)
			if m["verdict"] != "C" || len(cites) == 0 {
				continue
			}
			c := cites[0].(map[string]any)
			cand := rgaaRunCopy(t, preFold)
			var a map[string]any
			rgaaReadJSON(t, filepath.Join(cand, "adjudication.json"), &a)
			for _, x := range a["items"].([]any) {
				y := x.(map[string]any)
				if y["criteriaId"] == m["criteriaId"] {
					y["verdict"] = "NC"
					y["findings"] = []any{map[string]any{"file": c["file"], "line": c["line"], "snippet": c["snippet"], "message": "Non-conformité relevée par l'agent (fixture).", "normativeRef": m["criteriaId"]}}
					delete(y, "citations")
				}
			}
			rgaaWriteJSON(t, filepath.Join(cand, "adjudication.json"), a)
			rgaaOK(t, "fold_final", rgaaRunNode(t, scripts, "fold_final", site, env, in(map[string]any{"input.run_dir": cand, "input.require_complete": false})))
			var doc packAuditDoc
			rgaaReadJSON(t, filepath.Join(cand, "audit-latest.json"), &doc)
			for _, cr := range doc.Criteria {
				if cr.ID == m["criteriaId"] && cr.Status == "NC" {
					ncID, dir = cr.ID, cand
				}
			}
			if ncID != "" {
				break
			}
		}
		if ncID == "" {
			t.Fatal("the engine folded none of the agent non-conformities tried")
		}
		dr := rgaaOK(t, "draft_report", rgaaRunNode(t, scripts, "draft_report", site, env, in(map[string]any{"input.run_dir": dir})))
		var todo []map[string]any
		rgaaReadJSON(t, fmt.Sprint(dr["todo_path"]), &todo)
		hit := 0
		for _, it := range todo {
			it["verdict"] = "supported"
			if fmt.Sprint(it["criteriaId"]) == ncID && it["kind"] != "c" {
				it["verdict"] = "refuted"
				hit++
			}
		}
		if hit == 0 {
			t.Fatalf("the trial does not put the agent non-conformity on %s on trial", ncID)
		}
		rgaaWriteJSON(t, fmt.Sprint(dr["todo_path"]), todo)
		out := rgaaOK(t, "refute_apply", rgaaRunNode(t, scripts, "refute_apply", site, env, in(map[string]any{"input.run_dir": dir, "input.complete": true})))
		if rgaaInt(out["removed_findings"]) != hit || !rgaaContains(rgaaStrings(out["reopened"]), ncID) || rgaaContains(rgaaStrings(out["cleared"]), ncID) {
			t.Fatalf("refuted agent NC on %s: removed_findings=%v reopened=%v cleared=%v (want %d removed, the criterion reopened, no conformity cleared)", ncID, out["removed_findings"], out["reopened"], out["cleared"], hit)
		}
	})

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

	t.Run("final_report_delivers_the_ledger_with_workspace_relative_anchors", func(t *testing.T) {
		// A page capture is anchored by its absolute path in the run's
		// workspace. The delivered ledger is the one a project commits: it
		// names the capture relative to the workspace (the engine
		// canonicalises capture paths on replay), never where this run lived.
		copyRun := rgaaRunCopy(t, run)
		ledgerPath := filepath.Join(copyRun, "ledger", "rgaa.json")
		var ledger map[string]any
		rgaaReadJSON(t, ledgerPath, &ledger)
		entries, _ := ledger["entries"].([]any)
		if len(entries) == 0 {
			t.Fatal("the run's ledger is empty")
		}
		// Every anchored field, from this run's workspace (given through a
		// symlink: the engine writes physical paths) and from an earlier run's
		// worktree that a replayed entry carried over.
		link := filepath.Join(t.TempDir(), "ws-link")
		if err := os.Symlink(site, link); err != nil {
			t.Fatal(err)
		}
		real, err := filepath.EvalSymlinks(site)
		if err != nil {
			t.Fatal(err)
		}
		capture := filepath.Join(real, ".ultra11y", "pages", "accueil", "dom.html")
		// The engine canonicalises from the LAST `.ultra11y/pages/`.
		elsewhere := "/srv/old-store/.ultra11y/pages/x/worktrees/old-run/.ultra11y/pages/contact/dom.html"
		first := entries[0].(map[string]any)
		cites, _ := first["citations"].([]any)
		windows := `C:\runs\old-run\.ultra11y\pages\aide\dom.html`
		first["citations"] = append(cites, map[string]any{"file": capture, "line": 1, "snippet": "<html>"}, map[string]any{"file": elsewhere, "line": 2, "snippet": "<html>"}, map[string]any{"file": windows, "line": 3, "snippet": "<html>"})
		first["findings"] = []any{map[string]any{"file": capture, "line": 3, "message": "constat (fixture)", "normativeRef": "1.1"}}
		first["recommendations"] = []any{map[string]any{"file": filepath.Join(real, "index.html"), "line": 4, "message": "reco (fixture)"}}
		rgaaWriteJSON(t, ledgerPath, ledger)
		reportDir := filepath.Join(t.TempDir(), "audits")
		rgaaOK(t, "final_report", rgaaRunNode(t, scripts, "final_report", site, env, in(map[string]any{"input.run_dir": copyRun, "input.workspace_dir": link, "input.html_report": false, "input.report_dir": reportDir, "input.require_complete": false, "input.undecidable_present": false, "input.undecidable_path": "", "input.scope_notes": ""})))
		raw, err := os.ReadFile(filepath.Join(reportDir, "ledger", "rgaa.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{site, real, link, "/srv/old-store", `C:\\runs`} {
			if strings.Contains(string(raw), leak) {
				t.Fatalf("the delivered ledger still names %s:\n%s", leak, raw)
			}
		}
		var delivered struct {
			Entries []map[string]any `json:"entries"`
		}
		rgaaReadJSON(t, filepath.Join(reportDir, "ledger", "rgaa.json"), &delivered)
		got := map[string]bool{}
		for _, k := range []string{"citations", "findings", "recommendations"} {
			list, _ := delivered.Entries[0][k].([]any)
			for _, x := range list {
				got[fmt.Sprint(x.(map[string]any)["file"])] = true
			}
		}
		for _, want := range []string{".ultra11y/pages/accueil/dom.html", ".ultra11y/pages/contact/dom.html", ".ultra11y/pages/aide/dom.html", "index.html"} {
			if !got[want] {
				t.Fatalf("the delivered ledger lacks the relative anchor %s: %v", want, got)
			}
		}
	})

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
	for key, want := range map[string]string{"reopened": "{{outputs.refute_apply.reopened}}", "refute_applied": "{{outputs.refute_apply.applied}}", "refute_degraded": "{{outputs.refute_apply.degraded}}", "refuted": "{{outputs.refute_apply.refuted}}"} {
		if got := mapping("refute_apply", "review_gate", key); got != want {
			t.Errorf("refute_apply -> review_gate maps %s from %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]string{"reopened": "{{outputs.budget_gate.no_reopened}}", "refute_applied": "{{outputs.budget_gate.applied_floor}}", "refute_degraded": "{{outputs.budget_gate.degraded_floor}}", "refuted": "{{outputs.budget_gate.zero}}"} {
		if got := mapping("budget_gate", "review_gate", key); got != want {
			t.Errorf("budget_gate -> review_gate maps %s from %q, want the gate's own floor %q", key, got, want)
		}
	}
	for _, key := range []string{"undecided_ids", "undecided", "refuted", "refute_applied", "refute_degraded"} {
		if got := mapping("review_gate", "human_review", key); got != "{{outputs.review_gate."+key+"}}" {
			t.Errorf("review_gate -> human_review maps %s from %q, want review_gate's own %s", key, got, key)
		}
	}
	// And the values the edges carry cross two compute nodes: evaluate their
	// expressions as written, on both paths, with the trial's two flags apart.
	eval := func(node string, input, vars map[string]any) map[string]any {
		t.Helper()
		cn, ok := wf.Nodes[node].(*ir.ComputeNode)
		if !ok {
			t.Fatalf("%s is not a compute node", node)
		}
		lookup := func(m map[string]any) func([]string) any {
			return func(p []string) any {
				if len(p) == 0 {
					return nil
				}
				return m[p[0]]
			}
		}
		ctx := &expr.Context{Input: lookup(input), Vars: lookup(vars), Run: lookup(map[string]any{"cost_usd": 1.0, "max_cost_usd": 30.0})}
		out := map[string]any{}
		for _, e := range cn.Exprs {
			v, err := e.AST.Eval(ctx)
			if err != nil {
				t.Fatalf("%s.%s: %v", node, e.Key, err)
			}
			out[e.Key] = v
		}
		return out
	}
	// The three ways the trial is skipped: disabled, nothing on trial, the
	// budget share spent (25 $ of a 30 $ cap past a 0.6 share).
	for name, c := range map[string]struct {
		input map[string]any
		cost  float64
	}{
		"disabled":   {map[string]any{"refute_enabled": false, "claims": int64(4), "share": 0.6}, 1},
		"no claims":  {map[string]any{"refute_enabled": true, "claims": int64(0), "share": 0.6}, 1},
		"over share": {map[string]any{"refute_enabled": true, "claims": int64(4), "share": 0.6}, 25},
	} {
		cn := wf.Nodes["budget_gate"].(*ir.ComputeNode)
		ctx := &expr.Context{
			Input: func(p []string) any { return c.input[p[0]] },
			Run:   func(p []string) any { return map[string]any{"cost_usd": c.cost, "max_cost_usd": 30.0}[p[0]] },
		}
		out := map[string]any{}
		for _, e := range cn.Exprs {
			v, err := e.AST.Eval(ctx)
			if err != nil {
				t.Fatalf("budget_gate.%s: %v", e.Key, err)
			}
			out[e.Key] = v
		}
		if out["run_refuter"] != false || out["applied_floor"] != false || out["degraded_floor"] != false {
			t.Errorf("budget_gate skipped (%s) hands review_gate run_refuter=%v applied=%v degraded=%v, want all false", name, out["run_refuter"], out["applied_floor"], out["degraded_floor"])
		}
	}
	for _, flags := range [][2]bool{{true, false}, {false, true}, {false, false}, {true, true}} {
		gate := eval("review_gate", map[string]any{"refute_applied": flags[0], "refute_degraded": flags[1], "refuted": int64(1), "reopened": []any{}, "open_ids": []any{}}, map[string]any{"require_complete": false, "human_review": true})
		if gate["refute_applied"] != flags[0] || gate["refute_degraded"] != flags[1] {
			t.Errorf("review_gate hands the human gate applied=%v degraded=%v for a trial applied=%v degraded=%v", gate["refute_applied"], gate["refute_degraded"], flags[0], flags[1])
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
