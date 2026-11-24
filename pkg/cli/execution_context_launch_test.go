package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

const contextLaunchSource = `schema checked:
  ok: bool

compute check:
  output: checked
  expr:
    ok: "true"

workflow probe:
  worktree: none
  sandbox: none
  entry: check
  check -> done
`

type contextLaunchCloser struct {
	closed bool
}

func (e *contextLaunchCloser) Close() error { e.closed = true; return nil }

func (e *contextLaunchCloser) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	return nil, errors.New("unexpected node execution after context refusal")
}

func TestExecutionContextCLIRefusalPrecedesExecutorAcquisition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "child.bot")
	writeFile(t, path, contextLaunchSource)
	s, err := store.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(t.Context(), "context-cli", "probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{Version: 2}
	if err := s.SaveRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	exec := &contextLaunchCloser{}
	err = RunRun(t.Context(), RunOptions{File: path, RunID: run.ID, Background: true, StoreDir: s.Root(), Executor: exec, NoInteractive: true, Sandbox: "none"}, &Printer{W: io.Discard, Format: OutputJSON})
	if err == nil || !strings.Contains(err.Error(), "unsupported execution context version") {
		t.Fatalf("expected context refusal, got %v", err)
	}
	if exec.closed {
		t.Fatal("executor was acquired before pure context refusal")
	}
}

func TestExecutionContextCLISubbotRefusesBeforeParentBinding(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "child.bot"), contextLaunchSource)
	s, err := store.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.CreateRun(t.Context(), "context-parent", "parent", nil)
	if err != nil {
		t.Fatal(err)
	}
	parent.ExecutionContext = &store.ExecutionContext{Version: 2}
	if err := s.SaveRun(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	run := subbotRunnerForCLI(filepath.Join(dir, "parent.bot"), s.Root(), s, iterlog.Nop(), RunOptions{Sandbox: "none"})
	_, err = run(boundLegacySubbotFixture(t, context.Background(), parent.ID), runtime.SubbotRequest{Source: "child.bot", ParentRunID: parent.ID, NodeID: "child", ReattachKey: "child", WorkDir: dir})
	if err == nil || !strings.Contains(err.Error(), "unsupported execution context version") {
		t.Fatalf("expected context refusal, got %v", err)
	}
	parent, err = s.LoadRun(t.Context(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.SubbotChildren) != 0 {
		t.Fatalf("context refusal recorded an absent child: %v", parent.SubbotChildren)
	}
}

func TestExecutionContextCLIKeepsPrecreatedPolicy(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("ITERION_RELIABILITY_MODE", "enforce")
	path := filepath.Join(dir, "child.bot")
	writeFile(t, path, contextLaunchSource)
	s, err := store.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	wf, hash, _, err := runview.CompileSubbotWorkflow(path, nil, dir, store.RunTrustDefault)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := runview.ResolveExecutionContext(t.Context(), s, "context-handoff", runview.LaunchSpec{FilePath: path,
		ExecutionContext: &store.ExecutionContext{Policy: store.ContextPolicyReport, BusinessStores: []store.ContextRef{{ID: "ledger", Kind: "db", Required: true}}},
	}, wf, hash, store.ContextPolicyEnforce, "")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(t.Context(), "context-handoff", wf.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = contract
	if err := s.SaveRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := RunRun(t.Context(), RunOptions{File: path, RunID: run.ID, Background: true, StoreDir: s.Root(), NoInteractive: true, Sandbox: "none"}, &Printer{W: io.Discard, Format: OutputJSON}); err != nil {
		t.Fatal(err)
	}
	run, err = s.LoadRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Admission == nil || run.Admission.Code != "context_match" || run.Admission.Policy != store.ContextPolicyReport {
		t.Fatalf("CLI replaced the launcher's policy with its own default: %+v", run.Admission)
	}
	if len(run.ExecutionContext.BusinessStores) != 1 || run.ExecutionContext.BusinessStores[0].ID != "ledger" {
		t.Fatalf("CLI handoff lost business-store declaration: %+v", run.ExecutionContext)
	}
}

func TestBusinessStoresCLIChildKeepsParentPolicyAndAddsOwnDeclaration(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(t.TempDir())
	childSource := strings.Replace(contextLaunchSource, "  entry: check", "  business_stores:\n    child:\n      kind: db\n      namespace: \"child-scope\"\n  entry: check", 1)
	writeFile(t, filepath.Join(dir, "child.bot"), childSource)
	st, err := store.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := st.CreateRun(t.Context(), "stores-parent", "parent", nil)
	if err != nil {
		t.Fatal(err)
	}
	inherited := store.ContextRef{ID: "parent", Kind: "db", Namespace: "parent-scope", Required: true}
	parent.ExecutionContext = &store.ExecutionContext{Policy: store.ContextPolicyReport, BusinessStores: []store.ContextRef{inherited}}
	if err := st.SaveRun(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	run := subbotRunnerForCLI(filepath.Join(dir, "parent.bot"), st.Root(), st, iterlog.Nop(), RunOptions{Sandbox: "none"})
	if _, err := run(boundLegacySubbotFixture(t, t.Context(), parent.ID), runtime.SubbotRequest{Source: "child.bot", ParentRunID: parent.ID, NodeID: "child", ReattachKey: "child", WorkDir: dir}); err != nil {
		t.Fatal(err)
	}
	parent, err = st.LoadRun(t.Context(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := st.ListRuns(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected parent and one finished child, got %v", ids)
	}
	var childID string
	for _, id := range ids {
		if id != parent.ID {
			childID = id
		}
	}
	if childID == "" {
		t.Fatal("child run missing")
	}
	child, err := st.LoadRun(t.Context(), childID)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.ContextRef{{ID: "child", Kind: "db", Namespace: "child-scope"}, inherited}
	if child.ExecutionContext == nil || child.ExecutionContext.Policy != store.ContextPolicyReport || !reflect.DeepEqual(child.ExecutionContext.BusinessStores, want) {
		t.Fatalf("child loses inherited policy/stores or own declaration: %+v", child.ExecutionContext)
	}
	if !reflect.DeepEqual(parent.ExecutionContext.BusinessStores, []store.ContextRef{inherited}) {
		t.Fatal("child declaration mutated parent")
	}
}
