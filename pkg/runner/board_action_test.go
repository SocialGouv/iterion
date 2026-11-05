package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
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

// The cloud runner's native transport carries the run's owning ticket like
// every board transport (ADR-151): a card filed through board.issue.create on
// iterion.cloud is linked to the card that launched the run, exactly as the
// same `.bot` run locally links it. The ticket is read from the RUN RECORD
// (Source.IssueID) through the spec's run loader — the message carries none.
func TestExecutorSpecStampsTheOwningTicketOnTheNativeBoardTransport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	defer board.Close()
	owning, err := board.Create(native.Issue{Title: "the card that launched the run"})
	if err != nil {
		t.Fatalf("Create owning card: %v", err)
	}
	run, err := st.CreateRun(ctx, "run-board-src", "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: owning.ID}
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	r := &Runner{cfg: Config{
		Store:    st,
		Logger:   iterlog.Nop(),
		BoardFor: func(string) native.BoardStore { return board },
	}}
	msg := &queue.RunMessage{RunID: "run-board-src", WorkflowName: "wf", TenantID: "team-1"}

	spec, _, err := r.executorSpec(ctx, msg, boardActionWorkflow(), "", iterlog.Nop(), nil, nil)
	if err != nil {
		t.Fatalf("executorSpec: %v", err)
	}
	var tr model.ActionTransport
	for _, candidate := range spec.ActionTransports {
		if candidate != nil && candidate.Connector() == "board" {
			tr = candidate
		}
	}
	if tr == nil {
		t.Fatal("no board transport wired")
	}
	op, ok := tr.Package().Operation("board.issue.create")
	if !ok {
		t.Fatal("the facade exposes no board.issue.create")
	}
	if _, err := tr.Call(ctx, op, map[string]any{"title": "filed by the run"}, []string{"board.create"}); err != nil {
		t.Fatalf("Call board.issue.create: %v", err)
	}
	issues, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, iss := range issues {
		if iss.Title != "filed by the run" {
			continue
		}
		if iss.ParentID != owning.ID {
			t.Fatalf("the filed card's parent_id = %q, want the run's owning ticket %q — the cloud transport stamped no provenance", iss.ParentID, owning.ID)
		}
		return
	}
	t.Fatal("the filed card is not on the board")
}
