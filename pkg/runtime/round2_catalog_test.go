package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/internal/gittest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/store"
)

func r2repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gittest.Run(t, dir, "init", "-q")
	gittest.Run(t, dir, "config", "user.email", "probe@example.invalid")
	gittest.Run(t, dir, "config", "user.name", "probe")
	gittest.Run(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, dir, "add", "a.txt")
	gittest.Run(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// One real catalog tool node, verbatim (same IR node, same schemas), as the
// entry of a one-node workflow, executed by the production ClawExecutor.
func r2RunCatalogNode(t *testing.T, botPath, nodeID string, vars map[string]any, inputs map[string]any, strict bool) (*store.Run, error) {
	t.Helper()
	u, err := unit.LoadDir(botPath)
	if err != nil {
		t.Fatalf("load %s: %v", botPath, err)
	}
	cr := ir.Compile(u.Merged)
	if cr.HasErrors() {
		t.Fatalf("compile %s", botPath)
	}
	real := cr.Workflow
	tn, ok := real.Nodes[nodeID].(*ir.ToolNode)
	if !ok {
		t.Fatalf("%s is not a tool node", nodeID)
	}
	wfVars := map[string]*ir.Var{}
	for k, v := range vars {
		wfVars[k] = &ir.Var{Name: k, Type: ir.VarString, HasDefault: true, Default: v}
	}
	wf := &ir.Workflow{
		Name: "r2_" + nodeID, Entry: nodeID,
		Nodes:   map[string]ir.Node{nodeID: tn, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Edges:   []*ir.Edge{{From: nodeID, To: "done"}},
		Schemas: real.Schemas, Prompts: real.Prompts, Vars: wfVars,
	}
	ws := t.TempDir()
	execu := model.NewClawExecutor(model.NewRegistry(), wf, model.WithWorkDir(ws))
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := []EngineOption{WithWorkDir(ws)}
	if strict {
		opts = append(opts, WithOutputValidation(true))
	}
	runErr := New(wf, st, execu, opts...).Run(context.Background(), "r2-"+nodeID, inputs)
	events, loadErr := st.LoadEvents(context.Background(), "r2-"+nodeID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	warnings := 0
	for _, ev := range events {
		if ev.Type == "node_output_schema_mismatch" {
			warnings++
			if ev.Data["schema"] != tn.OutputSchema || ev.Data["fields"] == nil {
				t.Errorf("incomplete schema warning: %v", ev.Data)
			}
		}
	}
	if !strict && warnings != 1 {
		t.Errorf("warning count=%d want 1", warnings)
	}
	r, _ := st.LoadRun(context.Background(), "r2-"+nodeID)
	return r, runErr
}

func TestR2CatalogToolNodesUnderDefaultValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	root := "../.."
	type tc struct {
		name, bot, node string
		vars            func(t *testing.T) map[string]any
		inputs          func(t *testing.T) map[string]any
	}
	cases := []tc{
		{"adr-cartograph/mark_issue_for_review (manual run, no issue_id)", "bots/adr-cartograph/main.bot", "mark_issue_for_review",
			func(*testing.T) map[string]any { return map[string]any{"issue_id": ""} }, func(*testing.T) map[string]any { return nil }},
		{"adr-rechallenge/commit_changes (a real change committed)", "bots/adr-rechallenge/main.bot", "commit_changes", nil,
			func(t *testing.T) map[string]any {
				repo := r2repo(t)
				_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two\n"), 0o644)
				return map[string]any{"workspace_dir": repo, "files": "a.txt", "full_message": "docs(adr): rechallenge"}
			}},
		{"bmady/commit_changes (a real change committed)", "bots/bmady/main.bot", "commit_changes", nil,
			func(t *testing.T) map[string]any {
				repo := r2repo(t)
				_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two\n"), 0o644)
				return map[string]any{"workspace_dir": repo, "message": "feat: story"}
			}},
		{"test-coverage/verify_run (NET DIRTY refusal loop-back)", "bots/test-coverage/main.bot", "verify_run",
			func(t *testing.T) map[string]any {
				repo := r2repo(t)
				_ = os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("wip\n"), 0o644)
				scratch := t.TempDir()
				_ = os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
				return map[string]any{"workspace_dir": repo, "scratch_dir": scratch}
			}, func(*testing.T) map[string]any { return nil }},
		{"e2e-coverage/verify_run (NET DIRTY refusal loop-back)", "bots/e2e-coverage/main.bot", "verify_run",
			func(t *testing.T) map[string]any {
				repo := r2repo(t)
				_ = os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("wip\n"), 0o644)
				scratch := t.TempDir()
				_ = os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
				return map[string]any{"workspace_dir": repo, "scratch_dir": scratch}
			}, func(*testing.T) map[string]any { return nil }},
		{"sec-audit-source/run_generic_scanners (scanners absent -> errors listed)", "bots/sec-audit-source/main.bot", "run_generic_scanners",
			func(t *testing.T) map[string]any { return map[string]any{"workspace_dir": r2repo(t)} },
			func(t *testing.T) map[string]any { return map[string]any{"scan_dir": t.TempDir()} }},
		{"dep-update-guard/verify_run (MASKED EXIT STATUS refusal)", "bots/dep-update-guard/main.bot", "verify_run",
			func(t *testing.T) map[string]any {
				scratch := t.TempDir()
				_ = os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte("#!/bin/sh\nmake test | tail -5\n"), 0o755)
				return map[string]any{"workspace_dir": r2repo(t), "scratch_dir": scratch, "verify_timeout_s": "60"}
			},
			func(*testing.T) map[string]any { return nil }},
	}
	for _, c := range cases {
		for _, off := range []bool{false, true} {
			var vars, inputs map[string]any
			if c.vars != nil {
				vars = c.vars(t)
			}
			if c.inputs != nil {
				inputs = c.inputs(t)
			}
			r, err := r2RunCatalogNode(t, filepath.Join(root, c.bot), c.node, vars, inputs, off)
			mode := "engine default (cloud runner/studio/dispatcher/resume)"
			if off {
				mode = "strict validation"
			}
			var re *RuntimeError
			code := ""
			if errors.As(err, &re) {
				code = string(re.Code)
			}
			status, fcode := "", ""
			if r != nil {
				status, fcode = string(r.Status), string(r.FailureCode)
			}
			if !off && (err != nil || status != "finished") {
				t.Errorf("default rejected catalog output: %s: %v", c.name, err)
			}
			if off && code != string(ErrCodeSchemaValidation) {
				t.Errorf("strict accepted catalog output: %s: %v", c.name, err)
			}
			t.Logf("%s | %s: code=%s status=%s storedFailureCode=%s err=%.240v", c.name, mode, code, status, fcode, err)
		}
	}
}

func TestR2WarningOmitsValuesAndStrictEnvRejects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, strict := range []string{"", "true"} {
		t.Setenv(EnvOutputValidation, strict)
		wf := computeGaugeWorkflow(t, ir.FieldTypeInt, "'private-output-sentinel'")
		st := tmpStore(t)
		err := New(wf, st, newStubExecutor()).Run(context.Background(), "warning", nil)
		if strict == "true" {
			var typed *RuntimeError
			if !errors.As(err, &typed) || typed.Code != ErrCodeSchemaValidation {
				t.Fatalf("strict env accepted: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		events, err := st.LoadEvents(context.Background(), "warning")
		if err != nil {
			t.Fatal(err)
		}
		warnings := 0
		for _, event := range events {
			if event.Type == store.EventNodeOutputSchemaMismatch {
				warnings++
				raw, _ := json.Marshal(event.Data)
				if strings.Contains(string(raw), "private-output-sentinel") || !strings.Contains(string(raw), "used_pct") {
					t.Errorf("unsafe or incomplete warning: %s", raw)
				}
			}
		}
		if warnings != 1 {
			t.Errorf("warnings=%d", warnings)
		}
	}
}
