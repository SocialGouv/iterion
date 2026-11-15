package runview

import (
	"context"
	"errors"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

type contextLaunchIdentityFailure struct{ store.RunStore }

func (s contextLaunchIdentityFailure) ContextIdentity(context.Context) (store.ContextRef, error) {
	return store.ContextRef{}, errors.New("test fixture identity failure")
}

func contextLaunchService(t *testing.T, failIdentity bool, extra ...ServiceOption) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "child.bot")
	if err := os.WriteFile(path, []byte(contextLaunchSource), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	var rs store.RunStore = st
	if failIdentity {
		rs = contextLaunchIdentityFailure{st}
	}
	opts := []ServiceOption{WithStore(rs), WithLogger(iterlog.Nop()), WithWorkDir(dir), WithSandboxDefault("none"), WithRecoveryPassive()}
	opts = append(opts, extra...)
	svc, err := NewService(st.Root(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, id := range svc.manager.ActiveRuns() {
			svc.manager.Deregister(id)
		}
		for id := range svc.runLogs {
			svc.dropRunLog(id)
		}
		stopService(t, svc)
	})
	return svc, path
}

func TestExecutionContextIdentityErrorReleasesSubbot(t *testing.T) {
	svc, path := contextLaunchService(t, true)
	if _, err := svc.store.CreateRun(context.Background(), "context-launch-parent", "parent", nil); err != nil {
		t.Fatal(err)
	}
	run := svc.subbotRunnerFor(filepath.Join(filepath.Dir(path), "parent.bot"), iterlog.Nop())
	_, err := run(boundLegacySubbotFixture(t, context.Background(), "context-launch-parent"), runtime.SubbotRequest{Source: "child.bot", NodeID: "child", WorkDir: filepath.Dir(path), ParentRunID: "context-launch-parent", ReattachKey: "child"})
	active := svc.manager.ActiveRuns()
	parent, _ := svc.store.LoadRun(context.Background(), "context-launch-parent")
	t.Logf("parent child bindings after refused launch=%v", parent.SubbotChildren)
	t.Logf("error=%v; active manager runs after refusal=%v", err, active)
	if err == nil || !strings.Contains(err.Error(), "test fixture identity failure") {
		t.Fatalf("wrong failure: %v", err)
	}
	if len(parent.SubbotChildren) != 0 {
		t.Error("identity refusal recorded a child with no document")
	}
	if len(active) != 0 {
		t.Fatal("identity refusal leaked a registered child with no running engine")
	}
}

func TestExecutionContextInvalidContextReleasesLaunchLog(t *testing.T) {
	svc, path := contextLaunchService(t, false)
	_, err := svc.startInProcess(context.Background(), "context-launch-refused", LaunchSpec{FilePath: path, ExecutionContext: &store.ExecutionContext{Version: 2}}, true)
	t.Logf("error=%v; retained run logs after refusal=%d", err, len(svc.runLogs))
	if err == nil || !strings.Contains(err.Error(), "unsupported execution context version") {
		t.Fatalf("wrong failure: %v", err)
	}
	if len(svc.runLogs) != 0 {
		t.Fatal("context refusal retained the run log buffer and its open file")
	}
}

func TestExecutionContextQueuedInvalidContextFailsBeforePersist(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	svc, path := contextLaunchService(t, false, WithMaxConcurrentPipelines(1))
	svc.pipelineQueue = newPipelineQueue(1) // production queue, passive service avoids async drain in fixture
	if ok, _ := svc.pipelineQueue.admitOrEnqueue("already-held", LaunchSpec{}); !ok {
		t.Fatal("fixture cannot reserve slot")
	}
	defer svc.pipelineQueue.slotFreed("already-held")
	res, err := svc.Launch(context.Background(), LaunchSpec{RunID: "context-launch-invalid-queued", FilePath: path, ExecutionContext: &store.ExecutionContext{Version: 2}})
	doc, loadErr := svc.store.LoadRun(context.Background(), "context-launch-invalid-queued")
	t.Logf("launch=%+v; err=%v; persisted=%v; loadErr=%v", res, err, doc != nil, loadErr)
	if doc != nil {
		t.Logf("persisted status=%s context=%+v", doc.Status, doc.ExecutionContext)
	}
	if err == nil || doc != nil {
		t.Fatal("invalid context accepted and persisted when concurrency queue is full")
	}
}

func TestExecutionContextDetachedInvalidContextFailsBeforePersist(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "1")
	svc, path := contextLaunchService(t, false)
	promoted := false
	_, err := svc.Launch(context.Background(), LaunchSpec{RunID: "context-launch-invalid-detached", FilePath: path, ExecutionContext: &store.ExecutionContext{Version: 2}, AttachmentPromote: func(context.Context, string) error {
		promoted = true
		return errors.New("test fixture stop before subprocess")
	}})
	doc, loadErr := svc.store.LoadRun(context.Background(), "context-launch-invalid-detached")
	t.Logf("error=%v; promotion reached=%t; persisted=%t; loadErr=%v", err, promoted, doc != nil, loadErr)
	if doc != nil {
		t.Logf("persisted status=%s context=%+v", doc.Status, doc.ExecutionContext)
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported execution context version") || promoted || doc != nil {
		t.Fatal("invalid execution context bypasses detached preflight and is persisted before subprocess")
	}
}

func TestExecutionContextQueuedDeclarationSurvivesRestart(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	svc, path := contextLaunchService(t, false, WithMaxConcurrentPipelines(1))
	svc.pipelineQueue = newPipelineQueue(1)
	if ok, _ := svc.pipelineQueue.admitOrEnqueue("held", LaunchSpec{}); !ok {
		t.Fatal("fixture cannot reserve slot")
	}
	decl := &store.ExecutionContext{Policy: store.ContextPolicyReport, BusinessStores: []store.ContextRef{{ID: "z", Kind: "db"}, {ID: "a", Kind: "db", Required: true}}}
	res, err := svc.Launch(t.Context(), LaunchSpec{RunID: "context-queued-valid", FilePath: path, ExecutionContext: decl})
	if err != nil || res.QueuePosition != 1 {
		t.Fatalf("queue launch=%+v err=%v", res, err)
	}
	doc, err := svc.store.LoadRun(t.Context(), res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	assertContext := func(c *store.ExecutionContext) {
		t.Helper()
		if c == nil || c.Policy != store.ContextPolicyReport || !store.HasStoreContextIdentity(c.RunStore) || len(c.BusinessStores) != 2 || c.BusinessStores[0].ID != "a" || !c.BusinessStores[0].Required {
			t.Fatalf("declaration lost during queue handoff: %+v", c)
		}
	}
	assertContext(doc.ExecutionContext)
	if decl.BusinessStores[0].ID != "z" {
		t.Fatal("launch normalization mutated the caller's declaration")
	}
	svc.pipelineQueue = newPipelineQueue(1)
	svc.rebuildPipelineQueue()
	ready := svc.pipelineQueue.dequeueReady()
	if len(ready) != 1 {
		t.Fatalf("recovered queue entries=%d", len(ready))
	}
	assertContext(ready[0].spec.ExecutionContext)
}

func TestExecutionContextDetachedDeclarationPrecedesHandoff(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "1")
	svc, path := contextLaunchService(t, false)
	stop := errors.New("fixture stops before subprocess")
	promoted := false
	_, err := svc.Launch(t.Context(), LaunchSpec{RunID: "context-detached-valid", FilePath: path,
		ExecutionContext: &store.ExecutionContext{Policy: store.ContextPolicyEnforce, BusinessStores: []store.ContextRef{{ID: "ledger", Kind: "db", Required: true}}},
		AttachmentPromote: func(ctx context.Context, id string) error {
			promoted = true
			doc, loadErr := svc.store.LoadRun(ctx, id)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			c := doc.ExecutionContext
			if c == nil || c.Policy != store.ContextPolicyEnforce || len(c.BusinessStores) != 1 || c.BusinessStores[0].ID != "ledger" || !store.HasStoreContextIdentity(c.RunStore) {
				t.Errorf("detached handoff lost the admitted declaration: %+v", c)
			}
			return stop
		},
	})
	if !promoted || !errors.Is(err, stop) {
		t.Fatalf("did not reach the controlled handoff stop: promoted=%v err=%v", promoted, err)
	}
}

func TestBusinessStoresWorkflowPersistsSortedAndRefusesDuplicates(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "duplicate"}[duplicate], func(t *testing.T) {
			t.Setenv("ITERION_RUNS_DETACHED", "0")
			svc, path := contextLaunchService(t, false, WithMaxConcurrentPipelines(1))
			svc.pipelineQueue = newPipelineQueue(1)
			if ok, _ := svc.pipelineQueue.admitOrEnqueue("held", LaunchSpec{}); !ok {
				t.Fatal("slot fixture")
			}
			firstID := "z"
			if duplicate {
				firstID = "a"
			}
			block := "  business_stores:\n    " + firstID + ":\n      kind: db\n      namespace: team\n    a:\n      kind: db\n      namespace: team\n      required: true\n"
			source := strings.Replace(contextLaunchSource, "  entry: check", block+"  entry: check", 1)
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := svc.Launch(t.Context(), LaunchSpec{RunID: "workflow-stores", FilePath: path})
			doc, loadErr := svc.store.LoadRun(t.Context(), "workflow-stores")
			if duplicate {
				if err == nil || !strings.Contains(err.Error(), "duplicate") || doc != nil {
					t.Fatalf("duplicate declaration admitted: result=%+v err=%v doc=%+v loadErr=%v", result, err, doc, loadErr)
				}
				return
			}
			want := []store.ContextRef{{ID: "a", Kind: "db", Namespace: "team", Required: true}, {ID: "z", Kind: "db", Namespace: "team"}}
			if err != nil || loadErr != nil || doc == nil || doc.ExecutionContext == nil || !reflect.DeepEqual(doc.ExecutionContext.BusinessStores, want) {
				t.Fatalf("workflow stores lost: result=%+v err=%v loadErr=%v doc=%+v", result, err, loadErr, doc)
			}
		})
	}
}

func TestBusinessStoresRunviewChildKeepsParentPolicyAndAddsOwnDeclaration(t *testing.T) {
	svc, path := contextLaunchService(t, false)
	dir := filepath.Dir(path)
	t.Chdir(t.TempDir())
	childSource := strings.Replace(contextLaunchSource, "  entry: check", "  business_stores:\n    child:\n      kind: db\n      namespace: \"child-scope\"\n  entry: check", 1)
	if err := os.WriteFile(path, []byte(childSource), 0600); err != nil {
		t.Fatal(err)
	}
	parent, err := svc.store.CreateRun(t.Context(), "stores-parent", "parent", nil)
	if err != nil {
		t.Fatal(err)
	}
	inherited := store.ContextRef{ID: "parent", Kind: "db", Namespace: "parent-scope", Required: true}
	parent.ExecutionContext = &store.ExecutionContext{Policy: store.ContextPolicyReport, BusinessStores: []store.ContextRef{inherited}}
	if err := svc.store.SaveRun(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	run := svc.subbotRunnerFor(filepath.Join(dir, "parent.bot"), iterlog.Nop())
	if _, err := run(boundLegacySubbotFixture(t, t.Context(), parent.ID), runtime.SubbotRequest{Source: "child.bot", ParentRunID: parent.ID, NodeID: "child", ReattachKey: "child", WorkDir: dir}); err != nil {
		t.Fatal(err)
	}
	parent, err = svc.store.LoadRun(t.Context(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := svc.store.ListRuns(t.Context())
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
	child, err := svc.store.LoadRun(t.Context(), childID)
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
