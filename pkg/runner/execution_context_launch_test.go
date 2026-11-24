package runner

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestExecutionContextSubbotRefusesBeforeParentBinding(t *testing.T) {
	r, st := subbotTestRunner(t)
	dir := t.TempDir()
	parentDir := filepath.Join(dir, "parent")
	writeSubbotFixture(t, parentDir, "main.bot", subbotTestParent)
	writeSubbotFixture(t, parentDir, "child.bot", subbotTestChild)
	parent, err := st.CreateRun(t.Context(), "context-parent", "parent", nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := &queue.RunMessage{RunID: parent.ID, TenantID: "t1", OwnerID: "u1", BotID: "parent", ExecutionContext: &store.ExecutionContext{Version: 2}}
	run := r.subbotRunnerFor(msg, parentDir, dir, iterlog.Nop())
	_, err = run(boundLegacySubbotFixture(t, t.Context(), parent.ID), runtime.SubbotRequest{Source: "child.bot", ParentRunID: parent.ID, NodeID: "child", ReattachKey: "child"})
	if err == nil || !strings.Contains(err.Error(), "unsupported execution context version") {
		t.Fatalf("expected context refusal, got %v", err)
	}
	parent, err = st.LoadRun(t.Context(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.SubbotChildren) != 0 {
		t.Fatalf("context refusal recorded an absent child: %v", parent.SubbotChildren)
	}
}

func TestBusinessStoresPodChildKeepsParentPolicyAndAddsOwnDeclaration(t *testing.T) {
	r, st := subbotTestRunner(t)
	dir := t.TempDir()
	t.Chdir(t.TempDir())
	writeSubbotFixture(t, dir, "main.bot", subbotTestParent)
	childSource := strings.Replace(subbotTestChild, "  entry: work", "  worktree: none\n  business_stores:\n    child:\n      kind: db\n      namespace: \"child-scope\"\n  entry: work", 1)
	writeSubbotFixture(t, dir, "child.bot", childSource)
	ctx := store.WithIdentity(t.Context(), "t1", "u1")
	parent, err := st.CreateRun(ctx, "stores-parent", "parent", nil)
	if err != nil {
		t.Fatal(err)
	}
	inherited := store.ContextRef{ID: "parent", Kind: "db", Namespace: "parent-scope", Required: true}
	declaration := &store.ExecutionContext{Policy: store.ContextPolicyReport, BusinessStores: []store.ContextRef{inherited}}
	parent.ExecutionContext = declaration
	if err := st.SaveRun(ctx, parent); err != nil {
		t.Fatal(err)
	}
	msg := &queue.RunMessage{RunID: parent.ID, TenantID: "t1", OwnerID: "u1", BotID: "parent", ExecutionContext: declaration}
	run := r.subbotRunnerFor(msg, dir, dir, iterlog.Nop())
	if _, err := run(boundLegacySubbotFixture(t, ctx, parent.ID), runtime.SubbotRequest{Source: "child.bot", ParentRunID: parent.ID, NodeID: "child", ReattachKey: "child", WorkDir: dir}); err != nil {
		t.Fatal(err)
	}
	parent, err = st.LoadRun(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := st.ListRuns(ctx)
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
	child, err := st.LoadRun(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.ContextRef{{ID: "child", Kind: "db", Namespace: "child-scope"}, inherited}
	if child.ExecutionContext == nil || child.ExecutionContext.Policy != store.ContextPolicyReport || !reflect.DeepEqual(child.ExecutionContext.BusinessStores, want) {
		t.Fatalf("child loses inherited policy/stores or own declaration: %+v", child.ExecutionContext)
	}
	if !reflect.DeepEqual(declaration.BusinessStores, []store.ContextRef{inherited}) || !reflect.DeepEqual(parent.ExecutionContext.BusinessStores, []store.ContextRef{inherited}) {
		t.Fatal("child declaration mutated parent")
	}
}
