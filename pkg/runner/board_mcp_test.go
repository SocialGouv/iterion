package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The pod's executor must carry the sandboxed host-MCP transport (C082 on
// the cloud path, #2632): BoardRegister mints the per-node token the
// delegate hands to the sandboxed MCP client, BoardRevoke ends it. Without
// the pair the delegate falls back to "board capabilities granted but
// workflow is sandboxed … board MCP disabled for this node" and a
// sandboxed board-cap node on iterion.cloud writes nothing.
func TestExecutorSpecWiresBoardRegister(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	r.boardMCP, r.boardTokens = boardMCPWiring(Config{
		Store: st,
		BoardFor: func(tenantID string) native.BoardStore {
			if tenantID != "team-1" {
				return nil
			}
			return board
		},
	})
	if r.boardMCP == nil || r.boardTokens == nil {
		t.Fatal("boardMCPWiring with a BoardFor must build the mux and the registry")
	}
	msg := &queue.RunMessage{RunID: "run-board", WorkflowName: "wf", TenantID: "team-1", Trust: store.RunTrustFork}

	spec, _, err := r.executorSpec(context.Background(), msg, &ir.Workflow{}, "/work/run-board", iterlog.Nop(), nil, nil)
	if err != nil {
		t.Fatalf("executorSpec: %v", err)
	}
	if spec.BoardRegister == nil || spec.BoardRevoke == nil {
		t.Fatal("ExecutorSpec.BoardRegister/BoardRevoke not wired — sandboxed board-emit stays disabled on the pod")
	}

	// The minted grant is scoped to the node's capabilities and pinned to
	// the MESSAGE's tenant + trust + the run's workdir — never to anything
	// the caller supplies.
	token := spec.BoardRegister([]string{"board.create"}, "issue-9", msg.RunID)
	if token == "" {
		t.Fatal("BoardRegister minted an empty token")
	}
	grant, ok := r.boardTokens.Lookup(token)
	if !ok {
		t.Fatal("the minted token must resolve in the pod's registry")
	}
	if grant.TenantID != "team-1" {
		t.Fatalf("grant tenant = %q, want the message's team-1", grant.TenantID)
	}
	if !grant.Capabilities["board.create"] || grant.Capabilities["board.read"] {
		t.Fatalf("grant capabilities = %v, want exactly the node's [board.create]", grant.Capabilities)
	}
	if grant.SourceIssueID != "issue-9" || grant.WorkDir != "/work/run-board" {
		t.Fatalf("grant = %+v — source ticket and workdir must ride the grant", grant)
	}
	if grant.Trust != string(store.RunTrustFork) || !grant.TrustStated {
		t.Fatalf("grant trust = %q (stated=%t), want the message's fork, stated", grant.Trust, grant.TrustStated)
	}

	// Revocation ends the grant as the node that held it returns.
	spec.BoardRevoke(token)
	if _, ok := r.boardTokens.Lookup(token); ok {
		t.Fatal("a revoked token must not resolve")
	}
}

// A run message with no tenant must refuse to mint: an unpinned grant would
// resolve no board, so the node degrades to capability-disabled (the
// delegate's loud warning) instead of handing out a token that 404s per call.
func TestExecutorSpecBoardRegisterRefusesATenantlessRun(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	r.boardMCP, r.boardTokens = boardMCPWiring(Config{
		Store:    st,
		BoardFor: func(string) native.BoardStore { return nil },
	})
	msg := &queue.RunMessage{RunID: "run-notenant", WorkflowName: "wf"}

	spec, _, err := r.executorSpec(context.Background(), msg, &ir.Workflow{}, "", iterlog.Nop(), nil, nil)
	if err != nil {
		t.Fatalf("executorSpec: %v", err)
	}
	if spec.BoardRegister == nil {
		t.Fatal("BoardRegister must still be wired — the refusal is per mint, not per spec")
	}
	if token := spec.BoardRegister([]string{"board.create"}, "", msg.RunID); token != "" {
		t.Fatalf("a tenantless run must mint no token, got %q", token)
	}
}

// Without Config.BoardFor the pod wires nothing: the spec keeps nil
// register/revoke and the delegate's disable-with-warning path (CLI parity)
// is byte-identical to before #2632.
func TestExecutorSpecWithoutBoardForLeavesBoardDisabled(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	r.boardMCP, r.boardTokens = boardMCPWiring(r.cfg)
	if r.boardMCP != nil || r.boardTokens != nil {
		t.Fatal("boardMCPWiring without BoardFor must build nothing")
	}
	msg := &queue.RunMessage{RunID: "run-plain", WorkflowName: "wf", TenantID: "team-1"}
	spec, _, err := r.executorSpec(context.Background(), msg, &ir.Workflow{}, "", iterlog.Nop(), nil, nil)
	if err != nil {
		t.Fatalf("executorSpec: %v", err)
	}
	if spec.BoardRegister != nil || spec.BoardRevoke != nil {
		t.Fatal("BoardRegister/BoardRevoke must stay nil without a board plane — the pre-#2632 behaviour")
	}
}

// The pod's mux serves board + runs ONLY (the connection plane lives on the
// server) — and the engine is wired WithBoardMCPScoped(h, false) to match
// (loop.go, subbot.go), so a `connectors`-capability node is never handed a
// ConnectorHTTPEndpoint here: the delegate's documented warning fires
// instead of an AlwaysLoad MCP server 404ing at every call (adverse review
// MED-1). This witnesses the mux half of the pair: /api/v1/mcp/connectors
// MUST 404 — a route added later without the engine flag flips the runtime
// test TestConnectorEndpointFollowsWhatTheHandlerServes red.
func TestBoardMCPWiringDoesNotServeTheConnectorFacade(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	mux, _ := boardMCPWiring(Config{Store: st, BoardFor: func(string) native.BoardStore { return nil }})
	if mux == nil {
		t.Fatal("the mux must exist when BoardFor is wired")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/connectors", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("X-Iterion-Run", "tok")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/api/v1/mcp/connectors = %d, want 404 — the pod does not serve the facade", rec.Code)
	}
}
