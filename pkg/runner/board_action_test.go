package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The native board action on a cloud runner (ADR-151, #2834): the runner
// owes the TENANT's board (#2465), and a resolver that answers nil for the
// run's tenant is exactly the case the refusal exists for — BoardFor's
// contract INCLUDES answering nil (board_mcp_test.go wires one that does).
// Without the refusal the fs default in BuildExecutor would serve a
// pod-local board that is not the instance's, and a read would answer from
// an empty board with nobody told.

func boardActionWorkflow() *ir.Workflow {
	node := &ir.ToolNode{Action: "board.issue.list", Capabilities: []string{"board.read"}}
	node.ID = "plan"
	return &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{"plan": node}}
}

func TestExecutorSpecRefusesABoardActionWhenBoardForAnswersNil(t *testing.T) {
	t.Parallel()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r := &Runner{cfg: Config{
		Store:  st,
		Logger: iterlog.Nop(),
		// WIRED — and still answers nil for this tenant: the grant names a
		// board this process does not serve.
		BoardFor: func(tenantID string) native.BoardStore { return nil },
	}}
	msg := &queue.RunMessage{RunID: "run-board-nil", WorkflowName: "wf", TenantID: "team-gone"}

	_, _, err = r.executorSpec(context.Background(), msg, boardActionWorkflow(), "", iterlog.Nop(), nil, nil)
	if err == nil {
		t.Fatal("a nil board answer must refuse the spec — the fs default would read a pod-local board that is not the tenant's")
	}
	if !strings.Contains(err.Error(), "team-gone") {
		t.Errorf("the refusal must name the tenant whose board resolved to nil: %v", err)
	}
}

func TestExecutorSpecWiresTheTenantBoardForANativeAction(t *testing.T) {
	t.Parallel()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	defer board.Close()
	r := &Runner{cfg: Config{
		Store:  st,
		Logger: iterlog.Nop(),
		BoardFor: func(tenantID string) native.BoardStore {
			if tenantID != "team-1" {
				return nil
			}
			return board
		},
	}}
	msg := &queue.RunMessage{RunID: "run-board-ok", WorkflowName: "wf", TenantID: "team-1"}

	spec, _, err := r.executorSpec(context.Background(), msg, boardActionWorkflow(), "", iterlog.Nop(), nil, nil)
	if err != nil {
		t.Fatalf("executorSpec: %v", err)
	}
	found := false
	for _, tr := range spec.ActionTransports {
		if tr != nil && tr.Connector() == "board" {
			found = true
		}
	}
	if !found {
		t.Fatal("no board transport in ExecutorSpec.ActionTransports — the tenant's Mongo board never reaches the run")
	}
}
