package runtimecatalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/internal/sweepwalk"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/runtime"
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
	r, _, warnings, err := r2RunCatalogNodeWarnings(t, botPath, nodeID, vars, inputs, strict)
	if !strict && warnings != 1 {
		t.Errorf("warning count=%d want 1", warnings)
	}
	return r, err
}

// r2RunCatalogNodeWarnings is r2RunCatalogNode with the count of schema
// warnings the run raised handed back instead of judged — a node that violates
// its schema raises one in the default mode, a node that conforms raises none
// — beside the output the node published (nil when it did not finish).
func r2RunCatalogNodeWarnings(t *testing.T, botPath, nodeID string, vars map[string]any, inputs map[string]any, strict bool) (*store.Run, map[string]any, int, error) {
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
	opts := []runtime.EngineOption{runtime.WithWorkDir(ws)}
	if strict {
		opts = append(opts, runtime.WithOutputValidation(true))
	}
	runErr := runtime.New(wf, st, execu, opts...).Run(context.Background(), "r2-"+nodeID, inputs)
	events, loadErr := st.LoadEvents(context.Background(), "r2-"+nodeID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	warnings := 0
	var published map[string]any
	for _, ev := range events {
		if ev.Type == store.EventNodeFinished && ev.NodeID == nodeID {
			published, _ = ev.Data["output"].(map[string]any)
		}
		if ev.Type == "node_output_schema_mismatch" {
			warnings++
			if ev.Data["schema"] != tn.OutputSchema || ev.Data["fields"] == nil {
				t.Errorf("incomplete schema warning: %v", ev.Data)
			}
		}
	}
	r, _ := st.LoadRun(context.Background(), "r2-"+nodeID)
	return r, published, warnings, runErr
}

func TestR2CatalogToolNodesUnderDefaultValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	scannerCalls := r2ControlledCatalogPATH(t)
	defer func() {
		raw, err := os.ReadFile(scannerCalls)
		if err != nil {
			t.Errorf("controlled scanners never ran: %v", err)
			return
		}
		for _, name := range []string{"gitleaks", "trivy", "semgrep"} {
			if strings.Count(string(raw), name+"\n") != 2 {
				t.Errorf("scanner %s not stubbed twice: %q", name, raw)
			}
		}
	}()
	root := sweepwalk.ModuleRoot(t)
	type tc struct {
		name, bot, node string
		vars            func(t *testing.T) map[string]any
		inputs          func(t *testing.T) map[string]any
	}
	cases := []tc{
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
			var re *runtime.RuntimeError
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
			if off && code != string(runtime.ErrCodeToolFailedPermanent) {
				t.Errorf("strict accepted catalog output: %s: %v", c.name, err)
			}
			t.Logf("%s | %s: code=%s status=%s storedFailureCode=%s err=%.240v", c.name, mode, code, status, fcode, err)
		}
	}
}

// The catalog nodes whose stdout was not the JSON object their `output:`
// declares, each on the paths that print: a `git commit` summary where the
// answer belongs (adr-rechallenge, bmady), the dispatcher's own response or a
// `{"skipped": …}` under a schema that declares `text` (adr-cartograph). The
// engine fails such a node where it stands (#3421), so each now prints its
// declared fields and nothing else — proven here by running the real node
// under the production executor in both modes: finished, and not one schema
// warning, which is what conforming means.
func TestR2CatalogNodesThatPrintedBesideTheirAnswerNowConform(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(runtime.EnvOutputValidation, "")
	dirty := func(t *testing.T) string {
		repo := r2repo(t)
		if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return repo
	}
	head := func(t *testing.T, repo string) string {
		return strings.TrimSpace(gittest.Run(t, repo, "rev-parse", "HEAD"))
	}
	cases := []struct {
		name, bot, node string
		vars            map[string]any
		// dispatcher is the HTTP status a stub dispatcher answers the
		// node's request with; 0 points the node at a port nothing
		// listens on.
		dispatcher int
		inputs     func(t *testing.T) (in map[string]any, repo string)
		// want reads the published output against the repository the node
		// acted on: the commit it says it made is the one that is there.
		want func(t *testing.T, out map[string]any, repo, before string)
	}{
		{"adr-rechallenge/commit_changes commits a real change", "bots/adr-rechallenge/main.bot", "commit_changes", nil, 0,
			func(t *testing.T) (map[string]any, string) {
				repo := dirty(t)
				return map[string]any{"workspace_dir": repo, "files": "a.txt", "full_message": "docs(adr): rechallenge"}, repo
			},
			func(t *testing.T, out map[string]any, repo, before string) {
				if now := head(t, repo); now == before || out["success"] != true || out["output"] != "committed "+now {
					t.Errorf("output = %v, HEAD %s -> %s", out, before, now)
				}
			}},
		{"adr-rechallenge/commit_changes has nothing to commit", "bots/adr-rechallenge/main.bot", "commit_changes", nil, 0,
			func(t *testing.T) (map[string]any, string) {
				repo := r2repo(t)
				return map[string]any{"workspace_dir": repo, "files": "a.txt", "full_message": "docs(adr): rechallenge"}, repo
			},
			func(t *testing.T, out map[string]any, repo, before string) {
				if now := head(t, repo); now != before || out["success"] != true || out["output"] != "nothing to commit" {
					t.Errorf("output = %v, HEAD %s -> %s", out, before, now)
				}
			}},
		{"bmady/commit_changes commits a real change", "bots/bmady/main.bot", "commit_changes", nil, 0,
			func(t *testing.T) (map[string]any, string) {
				repo := dirty(t)
				return map[string]any{"workspace_dir": repo, "message": "feat: story"}, repo
			},
			func(t *testing.T, out map[string]any, repo, before string) {
				if now := head(t, repo); now == before || out["success"] != true || out["output"] != "committed "+now {
					t.Errorf("output = %v, HEAD %s -> %s", out, before, now)
				}
			}},
		{"bmady/commit_changes has nothing to commit", "bots/bmady/main.bot", "commit_changes", nil, 0,
			func(t *testing.T) (map[string]any, string) {
				repo := r2repo(t)
				return map[string]any{"workspace_dir": repo, "message": "feat: story"}, repo
			},
			func(t *testing.T, out map[string]any, repo, before string) {
				if now := head(t, repo); now != before || out["success"] != true || out["output"] != "nothing to commit" {
					t.Errorf("output = %v, HEAD %s -> %s", out, before, now)
				}
			}},
		{"adr-cartograph/mark_issue_for_review on a manual run", "bots/adr-cartograph/main.bot", "mark_issue_for_review",
			map[string]any{"issue_id": ""}, 0,
			func(*testing.T) (map[string]any, string) { return nil, "" },
			func(t *testing.T, out map[string]any, _, _ string) {
				if out["text"] != "skipped: no issue_id" {
					t.Errorf("output = %v", out)
				}
			}},
		// No dispatcher listens on the port: curl fails, which is the path
		// that used to print curl's own error where the answer belongs.
		{"adr-cartograph/mark_issue_for_review with the dispatcher down", "bots/adr-cartograph/main.bot", "mark_issue_for_review",
			map[string]any{"issue_id": "native:42"}, 0,
			func(*testing.T) (map[string]any, string) { return nil, "" },
			func(t *testing.T, out map[string]any, _, _ string) {
				if out["text"] != "skipped: the transition request failed or the dispatcher is down" {
					t.Errorf("output = %v", out)
				}
			}},
		{"adr-cartograph/mark_issue_for_review accepted by the dispatcher", "bots/adr-cartograph/main.bot", "mark_issue_for_review",
			map[string]any{"issue_id": "native:42"}, http.StatusOK,
			func(*testing.T) (map[string]any, string) { return nil, "" },
			func(t *testing.T, out map[string]any, _, _ string) {
				if out["text"] != "transition to review requested" {
					t.Errorf("output = %v", out)
				}
			}},
		// A refusal is an HTTP status, which curl reports as a failed
		// request only when asked to (`-f`): without it this path read
		// "transition requested" on a transition the dispatcher refused.
		{"adr-cartograph/mark_issue_for_review refused by the dispatcher", "bots/adr-cartograph/main.bot", "mark_issue_for_review",
			map[string]any{"issue_id": "native:42"}, http.StatusConflict,
			func(*testing.T) (map[string]any, string) { return nil, "" },
			func(t *testing.T, out map[string]any, _, _ string) {
				if out["text"] != "skipped: the transition request failed or the dispatcher is down" {
					t.Errorf("output = %v", out)
				}
			}},
	}
	for _, c := range cases {
		for _, strict := range []bool{false, true} {
			mode := "default"
			if strict {
				mode = "strict"
			}
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				if c.node == "mark_issue_for_review" && c.vars["issue_id"] != "" {
					if _, err := exec.LookPath("curl"); err != nil {
						t.Skipf("curl not on PATH: %v", err)
					}
				}
				// The dispatcher the node asks: a stub answering with the
				// case's status and recording the request, or a port
				// nothing listens on.
				port, asked := "9", ""
				if c.dispatcher != 0 {
					stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						asked = r.Method + " " + r.URL.Path
						w.WriteHeader(c.dispatcher)
						_, _ = w.Write([]byte(`{"state": "CANARY-dispatcher-answer"}`))
					}))
					defer stub.Close()
					port = stub.URL[strings.LastIndexByte(stub.URL, ':')+1:]
				}
				t.Setenv("ITERION_DISPATCHER_PORT", port)
				defer func() {
					if c.dispatcher != 0 && asked != "POST /api/v1/native/issues/42/transition" {
						t.Errorf("the dispatcher was asked %q", asked)
					}
				}()
				inputs, repo := c.inputs(t)
				before := ""
				if repo != "" {
					before = head(t, repo)
				}
				r, out, warnings, err := r2RunCatalogNodeWarnings(t, filepath.Join(sweepwalk.ModuleRoot(t), c.bot), c.node, c.vars, inputs, strict)
				if err != nil || r == nil || r.Status != "finished" {
					t.Fatalf("the node did not finish: %v (%+v)", err, r)
				}
				if warnings != 0 {
					t.Errorf("%d schema warning(s): the output still misses its schema", warnings)
				}
				c.want(t, out, repo, before)
			})
		}
	}
}

// Vetty's MASKED EXIT STATUS refusal used to leave precheck_rejected and
// precheck_reason out of verify_result, so it sat in the list above. It is an
// authorship defect that loops back to verify_build, and the loop-back edge
// reads precheck_rejected: the refusal now carries the whole schema, and strict
// validation accepts it under the production executor.
func TestR2VettyMaskedRefusalConformsUnderStrictValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte("#!/bin/sh\nmake test | tail -5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"workspace_dir": r2repo(t), "scratch_dir": scratch, "verify_timeout_s": "60"}
	r, err := r2RunCatalogNode(t, filepath.Join(sweepwalk.ModuleRoot(t), "bots", "dep-update-guard", "main.bot"), "verify_run", vars, nil, true)
	if err != nil {
		t.Fatalf("strict validation rejected the masked-exit refusal: %v", err)
	}
	if r == nil || r.Status != "finished" {
		t.Fatalf("the run did not finish: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(scratch, "verify.sh.rejected")); err != nil {
		t.Errorf("the refusal did not run: the masked script was not moved aside (%v)", err)
	}
}

// Only pinned, explicitly selected utilities enter this test's PATH. Scanner
// names are local stubs, so neither an installed scanner nor its DB can run.
func r2ControlledCatalogPATH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"bash", "sh", "git", "python3", "mkdir", "rm", "mktemp", "find", "awk", "sed", "grep", "wc", "head", "tail", "timeout", "basename", "dirname", "date", "cat", "touch", "sort", "tr", "cut", "xargs", "tee", "cp", "realpath", "readlink", "env", "uname", "sleep"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(dir, "scanner-calls")
	for _, name := range []string{"gitleaks", "trivy", "semgrep"} {
		script := "#!/bin/sh\nprintf '%s\\n' '" + name + "' >> '" + calls + "'\nexit 127\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return calls
}
