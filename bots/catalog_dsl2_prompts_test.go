package bots

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/migrate"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The first catalogue lot has no backslash-bearing literals: the migrator
// adds only a header. Re-reading the same source without that header gives
// the old profile, whose rendered prompts are frozen separately. These
// fixtures capture the production executor's request, before any model call;
// they are not snapshots of source indentation or an alternative renderer.
func TestCatalogDSL2RenderedPrompts(t *testing.T) {
	for _, bot := range []string{"docs-refresh", "adr-cartograph"} {
		t.Run(bot, func(t *testing.T) {
			path := filepath.Join(bot, "main.bot")
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if parser.ReadPreamble(string(src)).Profile != 2 {
				t.Fatal("this reviewed catalogue lot must declare dsl: 2")
			}
			legacy := strings.Replace(string(src), "dsl: 2\n\n", "", 1)
			if parser.ReadPreamble(legacy).Profile != 0 {
				t.Fatal("legacy rendering must really use the implicit profile 1")
			}
			migration, err := migrate.Bytes(path, []byte(legacy), migrate.Options{To: 2})
			if err != nil || string(migration.Migrated) != string(src) {
				t.Fatalf("lot differs from the surgical migrator's output: %v", err)
			}
			if len(migration.Changes) != 1 || migration.Changes[0].Kind != "header" {
				t.Fatalf("this lot must change only the header: %+v", migration.Changes)
			}
			before := compileDSL2PromptFixture(t, path, legacy)
			after := compileDSL2PromptFixture(t, path, string(src))
			// The full IR, including executable gates, schemas, graph, tools,
			// budgets and loop limits, must match after paragraph normalization.
			normalized := *after
			normalized.Prompts = make(map[string]*ir.Prompt, len(after.Prompts))
			for name, p := range after.Prompts {
				q := *p
				q.Body = parser.CanonicalPromptBody(q.Body)
				normalized.Prompts[name] = &q
			}
			if !reflect.DeepEqual(before, &normalized) {
				t.Fatal("migration changed compiled behavior beyond named-prompt paragraph breaks")
			}
			ids := make([]string, 0)
			for id, node := range after.Nodes {
				if _, ok := node.(*ir.AgentNode); ok {
					ids = append(ids, id)
				}
			}
			slices.Sort(ids)
			if len(ids) == 0 {
				t.Fatal("no real agent requests captured")
			}
			for _, id := range ids {
				t.Run(id, func(t *testing.T) {
					v1 := captureDSL2Prompt(t, before, id, "src/**")
					v2 := captureDSL2Prompt(t, after, id, "src/**")
					if v1 == v2 || parser.CanonicalPromptBody(v1) != parser.CanonicalPromptBody(v2) {
						t.Fatal("rendered requests must differ only by paragraph breaks")
					}
					for profile, body := range map[int]string{1: v1, 2: v2} {
						golden := filepath.Join("testdata", "dsl2-lot1", bot, fmt.Sprintf("%s.v%d.txt", id, profile))
						if os.Getenv("UPDATE_DSL2_PROMPTS") == "1" {
							if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(golden, []byte(body), 0o644); err != nil {
								t.Fatal(err)
							}
						}
						want, err := os.ReadFile(golden)
						if err != nil {
							t.Fatal(err)
						}
						if string(want) != body {
							t.Errorf("rendered prompt differs from %s; review the before/after diff before intentionally updating the fixture", golden)
						}
					}
				})
			}
			if bot == "adr-cartograph" {
				// Keep the empty-default case covered without trimming the real
				// renderer or checking trailing spaces into the review artifact.
				for _, wf := range []*ir.Workflow{before, after} {
					body := captureDSL2Prompt(t, wf, "survey_code", "")
					if !strings.Contains(body, " - code_scope_globs: \n") {
						t.Error("an empty code scope must render as empty, preserving the authored space and newline")
					}
				}
			}
		})
	}
}

func compileDSL2PromptFixture(t *testing.T, path, src string) *ir.Workflow {
	t.Helper()
	parsed := parser.Parse(path, src)
	for _, d := range parsed.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatal(d)
		}
	}
	compiled := ir.Compile(parsed.File)
	for _, d := range compiled.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatal(d)
		}
	}
	if compiled.Workflow == nil {
		t.Fatal("no compiled workflow")
	}
	return compiled.Workflow
}

var errDSL2RequestCaptured = errors.New("rendered request captured; no model call")

type dsl2PromptCapture struct {
	calls int
	task  delegate.Task
}

func (c *dsl2PromptCapture) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	c.calls++
	c.task = task
	return delegate.Result{}, errDSL2RequestCaptured
}

func captureDSL2Prompt(t *testing.T, wf *ir.Workflow, id, codeScope string) string {
	t.Helper()
	capture := &dsl2PromptCapture{}
	backends := delegate.NewRegistry()
	backends.Register("prompt_capture", capture)
	node := *wf.Nodes[id].(*ir.AgentNode)
	node.Backend = "prompt_capture"
	node.Fallbacks = nil
	// No ambient credential, memory, permission or model request enters this
	// deterministic render. Only the backend is replaced; authored templates,
	// input refs, interaction contract and rendering run through ClawExecutor.
	executor := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(backends), model.WithDefaultBackend("prompt_capture"),
		model.WithWorkDir(t.TempDir()), model.WithAutoMemoryOverride("off"),
		model.WithPermissionOverride("off"), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}),
	)
	t.Cleanup(func() {
		if err := executor.Close(); err != nil {
			t.Error(err)
		}
	})
	vars := make(map[string]any, len(wf.Vars))
	for key, v := range wf.Vars {
		if v.HasDefault {
			vars[key] = v.Default
		}
	}
	vars["workspace_dir"] = "/fixture/repo"
	vars["scratch_dir"] = "/fixture/scratch"
	vars["bundle_self_path"] = "/fixture/bundle/main.bot"
	vars["code_scope_globs"] = codeScope
	executor.SetVars(vars)
	// Visible sentinels make every input substitution reviewable. They
	// deliberately contain a paragraph themselves: input values must not be
	// re-canonicalized or interpreted as template source by the v2 migration.
	input := map[string]any{}
	for _, name := range []string{node.SystemPrompt, node.UserPrompt} {
		for _, ref := range wf.Prompts[name].TemplateRefs {
			if ref.Kind == ir.RefInput {
				if len(ref.Path) != 1 {
					t.Fatalf("new nested input needs a representative fixture: %s", ref.Raw)
				}
				input[ref.Path[0]] = "<" + ref.Raw + ">\n\n<end input>"
			}
		}
	}
	_, err := executor.Execute(context.Background(), &node, input)
	if !errors.Is(err, errDSL2RequestCaptured) || capture.calls != 1 {
		t.Fatalf("expected one captured backend request, got calls=%d err=%v", capture.calls, err)
	}
	return "SYSTEM\n" + capture.task.SystemPrompt + "\n\nUSER\n" + capture.task.UserPrompt + "\n"
}

// Output replay validates the termination schema in pkg/botreplay. This
// truth table additionally proves that the compiled, migrated gate cannot
// declare convergence when any required fact is false.
func TestCatalogDSL2TerminationGates(t *testing.T) {
	for _, tc := range []struct {
		bot   string
		facts []string
	}{
		{"docs-refresh", []string{"scope_check.scope_ok", "campaign.docs_aligned"}},
		{"adr-cartograph", []string{"verify_run.passed", "scope_check.scope_ok", "campaign.adrs_aligned", "build_manifest.coverage_pct"}},
	} {
		t.Run(tc.bot, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(tc.bot, "main.bot"))
			if err != nil {
				t.Fatal(err)
			}
			wf := compileDSL2PromptFixture(t, tc.bot, string(src))
			gate := wf.Nodes["gate"].(*ir.ComputeNode)
			var converged *expr.AST
			for _, field := range gate.Exprs {
				if field.Key == "converged" {
					converged = field.AST
				}
			}
			if converged == nil {
				t.Fatal("no deterministic convergence expression")
			}
			for bits := 0; bits < 1<<len(tc.facts); bits++ {
				values := map[string]any{}
				for i, fact := range tc.facts {
					set := bits&(1<<i) != 0
					values[fact] = set
					if fact == "build_manifest.coverage_pct" {
						values[fact] = int64(99)
						if set {
							values[fact] = int64(100)
						}
					}
				}
				got, err := converged.EvalBool(&expr.Context{
					Outputs: func(path []string) any { return values[strings.Join(path, ".")] },
					Vars: func(path []string) any {
						if strings.Join(path, ".") == "coverage_target_pct" {
							return int64(100)
						}
						return nil
					},
				})
				want := bits == (1<<len(tc.facts))-1
				if err != nil || got != want {
					t.Errorf("facts=%v: converged=%v err=%v, want %v", values, got, err, want)
				}
			}
		})
	}
}
