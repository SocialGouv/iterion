package bots

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// workflow-kick's one tool node, run by the engine's own tool executor — the
// var defaults the compiled workflow seeds, the engine's template rendering,
// the real python body — against a stub of the dispatch endpoint. Nothing
// here re-renders the command by hand: an unset var reaches the program the
// way the engine leaves it.

// kickStub is the dispatch endpoint: it answers with the status and body the
// case sets, and records every request it received.
type kickStub struct {
	srv *httptest.Server

	mu       sync.Mutex
	status   int
	answer   string
	requests []map[string]any
	bearers  []string
}

func newKickStub(t *testing.T, status int, answer string) *kickStub {
	t.Helper()
	k := &kickStub{status: status, answer: answer}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		k.mu.Lock()
		k.requests = append(k.requests, body)
		k.bearers = append(k.bearers, r.Header.Get("X-Iterion-Run"))
		status, answer := k.status, k.answer
		k.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(k.srv.Close)
	return k
}

func (k *kickStub) calls() ([]map[string]any, []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]map[string]any(nil), k.requests...), append([]string(nil), k.bearers...)
}

// runKick executes the kick node with launch vars as a run would carry them,
// in workDir, and returns its output.
func runKick(t *testing.T, workDir string, launch map[string]any) map[string]any {
	t.Helper()
	pr := parseBotUnit("workflow-kick/main.bot")
	if pr.File == nil {
		t.Fatal("parse produced no File")
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatal("compile produced no Workflow")
	}
	node, ok := cr.Workflow.Nodes["kick"].(*ir.ToolNode)
	if !ok {
		t.Fatalf("kick is %T, want *ir.ToolNode", cr.Workflow.Nodes["kick"])
	}
	// The run's secret guard, built as the engine builds it from the launch
	// vars: the minted bearer reaches the program through its environment.
	text := map[string]string{}
	for k, v := range launch {
		if s, ok := v.(string); ok {
			text[k] = s
		}
	}
	guard := model.BuildSecretGuard(context.Background(), cr.Workflow, text, nil)
	e := model.NewClawExecutor(model.NewRegistry(), cr.Workflow, model.WithWorkDir(workDir), model.WithSecretGuard(guard))
	e.SetLaunchVars(launch)
	const runID = "01kick"
	runURL := "https://iterion.example/studio/runs/" + runID
	ctx := model.WithRunID(context.Background(), runID)
	ctx = model.WithTemplateData(ctx, &model.TemplateData{RunID: runID, Run: map[string]any{"id": runID, "url": runURL}})
	out, err := e.Execute(ctx, node, map[string]any{})
	if err != nil {
		t.Fatalf("kick: %v", err)
	}
	if out["run_url"] != runURL {
		t.Errorf("kick output run_url = %v, want the run's page %q — every report of this bot links its run", out["run_url"], runURL)
	}
	return out
}

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, name := range tools {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not on PATH", name)
		}
	}
}

// The workflow var is the operator's to set on the schedule, and until it is
// set each tick ends NO_WORKFLOW — before any call, whether the var is
// absent from the launch (the schedule provisioning creates) or empty.
//
// Mutation witnessed: the var's `= ""` default removed from main.bot → the
// engine leaves {{vars.workflow}} in the command as text, the guard never
// fires and the program POSTs → red on the code and on the stub.
func TestWorkflowKick_NoWorkflowEndsBeforeAnyCall(t *testing.T) {
	requireTools(t, "python3")
	for name, launch := range map[string]map[string]any{
		"unset": {},
		"empty": {"workflow": ""},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newKickStub(t, http.StatusOK, `{"dispatched":true}`)
			launch["forge_dispatch_url"] = stub.srv.URL + "/api/v1/forge/dispatch-workflow"
			launch["forge_publish_token"] = "tok-run"
			launch["repo"] = "acme/widgets"
			out := runKick(t, t.TempDir(), launch)
			if out["code"] != "NO_WORKFLOW" || out["dispatched"] != false {
				t.Fatalf("kick with the workflow var %s = %v, want NO_WORKFLOW", name, out)
			}
			if reqs, _ := stub.calls(); len(reqs) != 0 {
				t.Fatalf("kick with the workflow var %s called the endpoint %v", name, reqs)
			}
		})
	}
}

// What the server answers reaches the run's output typed: a 4xx keeps its
// code and reason, a 5xx (reduced to its error_code at the server's HTTP
// boundary) keeps its code, and a success names the dispatched ref. The
// request carries the run id, the repository and the workflow, and the bearer
// rides the header.
func TestWorkflowKick_TheServersAnswerIsReportedTyped(t *testing.T) {
	requireTools(t, "python3")
	for _, tc := range []struct {
		name     string
		status   int
		answer   string
		wantCode string
		wantText string
		wantDone bool
	}{
		{"dispatched", http.StatusOK, `{"dispatched":true,"repo":"acme/widgets","workflow":"renovate.yml","ref":"main","workflow_url":"https://github.com/acme/widgets/actions/workflows/renovate.yml"}`, "", "acme/widgets@main", true},
		{"a typed 4xx", http.StatusForbidden, `{"dispatched":false,"code":"workflow_dispatch_workflow_outside_grant","reason":"the grant is bound to workflow \"renovate.yml\""}`, "workflow_dispatch_workflow_outside_grant", "bound to workflow", false},
		{"a reduced 5xx", http.StatusBadGateway, `{"error":"internal server error","error_code":"workflow_dispatch_unconfirmed","retryable":false}`, "workflow_dispatch_unconfirmed", "server log", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newKickStub(t, tc.status, tc.answer)
			out := runKick(t, t.TempDir(), map[string]any{
				"forge_dispatch_url":  stub.srv.URL + "/api/v1/forge/dispatch-workflow",
				"forge_publish_token": "tok-run",
				"workflow":            "renovate.yml",
				"repo":                "acme/widgets",
			})
			if out["code"] != tc.wantCode || out["dispatched"] != tc.wantDone {
				t.Fatalf("kick after a %d = %v, want code %q dispatched %v", tc.status, out, tc.wantCode, tc.wantDone)
			}
			if reason, _ := out["reason"].(string); !strings.Contains(reason, tc.wantText) {
				t.Fatalf("kick reason = %q, want it to carry %q", reason, tc.wantText)
			}
			reqs, bearers := stub.calls()
			if len(reqs) != 1 || reqs[0]["run_id"] != "01kick" || reqs[0]["repo"] != "acme/widgets" || reqs[0]["workflow"] != "renovate.yml" || bearers[0] != "tok-run" {
				t.Fatalf("requests = %v (bearers %v), want one {run_id 01kick, repo acme/widgets, workflow renovate.yml} bearing tok-run", reqs, bearers)
			}
		})
	}
}

// An empty repo var reads owner/name from the checkout's origin remote, in
// both URL shapes a clone carries.
func TestWorkflowKick_TheRepositoryComesFromTheOrigin(t *testing.T) {
	requireTools(t, "python3", "git")
	for _, origin := range []string{"https://github.com/acme/widgets.git", "git@github.com:acme/widgets.git"} {
		t.Run(origin, func(t *testing.T) {
			dir := t.TempDir()
			for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
				if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			stub := newKickStub(t, http.StatusOK, `{"dispatched":true,"ref":"main"}`)
			out := runKick(t, dir, map[string]any{
				"forge_dispatch_url":  stub.srv.URL + "/api/v1/forge/dispatch-workflow",
				"forge_publish_token": "tok-run",
				"workflow":            "renovate.yml",
			})
			if reqs, _ := stub.calls(); len(reqs) != 1 || reqs[0]["repo"] != "acme/widgets" || out["dispatched"] != true {
				t.Fatalf("kick from an origin %s = %v (requests %v), want acme/widgets dispatched", origin, out, reqs)
			}
		})
	}
}

// No grant bound — any launch but a repo-bound schedule's naming its
// workflow — ends NO_DISPATCH_GRANT, before any call.
func TestWorkflowKick_NoGrantEndsBeforeAnyCall(t *testing.T) {
	requireTools(t, "python3")
	out := runKick(t, t.TempDir(), map[string]any{"workflow": "renovate.yml", "repo": "acme/widgets"})
	if out["code"] != "NO_DISPATCH_GRANT" || out["dispatched"] != false {
		t.Fatalf("kick with no grant = %v, want NO_DISPATCH_GRANT", out)
	}
}
