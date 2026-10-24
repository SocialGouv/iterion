package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Only transport is replaced: the command reaches a real running engine,
// whose acknowledgement and persisted raise are the test's oracles.
type ceilingSteerer struct {
	runview.LaunchPublisher
	overrides chan *runtime.OverrideMsg
	release   chan struct{}
}

func (p *ceilingSteerer) SteerRun(ctx context.Context, runID string, cmd runview.SteerCommand) (runview.SteerReply, error) {
	if cmd.Kind != runview.SteerRaiseBudget || cmd.Budget == nil {
		return runview.SteerReply{}, fmt.Errorf("unexpected steering command: %+v", cmd)
	}
	msg := runtime.NewRaiseBudgetOverride(*cmd.Budget, cmd.IssuedBy)
	select {
	case p.overrides <- msg:
	case <-ctx.Done():
		return runview.SteerReply{}, ctx.Err()
	}
	close(p.release)
	res, err := msg.Await(ctx, 5*time.Second)
	if err != nil {
		return runview.SteerReply{}, err
	}
	if res.Err != nil {
		return runview.SteerReply{}, res.Err
	}
	return runview.SteerReply{RunID: runID, Effective: res.Effective, Applied: res.Applied, Noop: res.Noop}, nil
}

type ceilingSteerExecutor struct{ started, release chan struct{} }

func (e ceilingSteerExecutor) Execute(ctx context.Context, _ ir.Node, _ map[string]any) (map[string]any, error) {
	close(e.started)
	select {
	case <-e.release:
		return map[string]any{"ok": true}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// W3: remove the shared RaiseCaps clamp and both HTTP and WS return and
// persist the tenant's 1000/20h ask instead of the authority's 10/2h.
func TestRaiseBudgetHTTPAndWSClampToCeiling(t *testing.T) {
	for _, transport := range []string{"HTTP", "WS"} {
		t.Run(transport, func(t *testing.T) {
			workDir := t.TempDir()
			pub := &ceilingSteerer{overrides: make(chan *runtime.OverrideMsg, 1), release: make(chan struct{})}
			st, err := store.New(filepath.Join(workDir, ".iterion"))
			if err != nil {
				t.Fatal(err)
			}
			srv := New(Config{WorkDir: workDir, StoreDir: filepath.Join(workDir, ".iterion"), Store: st, SkipProjectRegistration: true, LaunchPublisher: pub}, iterlog.Nop())
			t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
			wf := &ir.Workflow{Name: "ceiling", Entry: "work", Worktree: "none",
				Nodes:  map[string]ir.Node{"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}}, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
				Edges:  []*ir.Edge{{From: "work", To: "done"}},
				Budget: &ir.Budget{MaxCostUSD: 1, MaxDuration: "1h", MaxTokens: 10, MaxIterations: 5},
			}
			wf.Budget.ClampToCeiling(&ir.Budget{MaxCostUSD: 10, MaxDuration: "2h", MaxTokens: 100, MaxIterations: 10})
			started := make(chan struct{})
			eng := runtime.New(wf, st, ceilingSteerExecutor{started: started, release: pub.release}, runtime.WithWorkDir(workDir), runtime.WithOverrideChannel(pub.overrides))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			done := make(chan error, 1)
			const runID = "raise-ceiling"
			go func() { done <- eng.Run(ctx, runID, nil) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("engine did not stop")
				}
			})
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("engine before request: %v", err)
			case <-ctx.Done():
				t.Fatal("engine never starts")
			}
			ask := map[string]any{"max_cost_usd": 1000, "max_duration": "20h", "max_tokens": 1000, "max_iterations": 1000}
			var reply runview.RaiseBudgetResponse
			if transport == "HTTP" {
				body, err := json.Marshal(map[string]any{"budget": ask})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/raise-budget", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				srv.mux.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Fatalf("HTTP status %d: %s", recorder.Code, recorder.Body.String())
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &reply); err != nil {
					t.Fatal(err)
				}
			} else {
				c := newRunConn(srv, nil, runID)
				raw, err := json.Marshal(ask)
				if err != nil {
					t.Fatal(err)
				}
				c.dispatch(runWSEnvelope{Type: wsTypeRaiseBudget, AckID: "raise-1", Payload: raw})
				var env runWSEnvelope
				select {
				case response := <-c.sendCh:
					if err := json.Unmarshal(response, &env); err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("WS never acknowledges")
				}
				if env.Type != wsTypeAck || env.AckID != "raise-1" {
					t.Fatalf("WS reply: %+v", env)
				}
				if err := json.Unmarshal(env.Payload, &reply); err != nil {
					t.Fatal(err)
				}
			}
			if reply.Effective["max_cost_usd"] != float64(10) || reply.Effective["max_duration"] != "2h0m0s" || reply.Effective["max_tokens"] != float64(100) || reply.Effective["max_iterations"] != float64(10) {
				t.Fatalf("unclamped acknowledgement: %+v", reply)
			}
			run, err := st.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.BudgetRaises == nil || run.BudgetRaises.MaxCostUSD != 10 || run.BudgetRaises.MaxDuration != "2h0m0s" || run.BudgetRaises.MaxTokens != 100 || run.BudgetRaises.MaxIterations != 10 {
				t.Fatalf("unclamped persisted raise: %+v", run.BudgetRaises)
			}
		})
	}
}
