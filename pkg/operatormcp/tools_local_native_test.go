package operatormcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestNativeProgressReportsPublicWorkProductsAndUnknownCost(t *testing.T) {
	state := &store.PortExecution{
		Identity: store.PortExecutionIdentity{Source: "private-source-hash"},
		Invocations: map[string]*store.PortInvocation{
			"a": {ID: "a", Status: store.PortSucceeded},
			"b": {ID: "b", Status: store.PortRunning},
		},
		Collections: map[string]*store.PortCollection{
			"render": {Node: "render", Items: []string{"a", "b"}},
			"empty":  {Node: "empty", Items: []string{}, Complete: true},
		},
		Products:     []string{"report", "preview"},
		Exports:      map[string]string{"report": "output:a:report"},
		Publications: map[string]*store.PortValue{"output:a:report": {Revision: "output:a:report"}},
		Budget: store.PortBudgetState{Consumed: store.PortBudgetAmount{Tokens: 420, CostUSD: 0.75, Iterations: 2},
			UnpricedTokens: 100, UnpricedNodes: 1, Reservations: map[string]store.PortBudgetAmount{"private-attempt": {CostUSD: 9}}},
	}
	view := nativeProgress(state)
	if view.Invocations[store.PortSucceeded] != 1 || view.Invocations[store.PortRunning] != 1 ||
		!reflect.DeepEqual(view.Maps, []nativeMapProgress{{Node: "empty", Items: 0, Complete: true}, {Node: "render", Items: 2}}) ||
		view.Products["report"] != "published" || view.Products["preview"] != "waiting" ||
		view.Usage.ReportedTokens != 420 || view.Usage.ReportedCostUSD != 0.75 || view.Usage.TotalCostKnown {
		t.Fatalf("native progress = %+v", view)
	}
	encoded, err := marshalText(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-source-hash", "private-attempt"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("public native view leaked %s", secret)
		}
	}
}

func TestLocalRunGetIncludesNativePublicProgress(t *testing.T) {
	s := newTestServer(t)
	st, err := s.store()
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithRuntimeSemantics(context.Background(), store.RuntimeSemanticsPortsV1)
	const id = "pc1_mcp_native_progress"
	if _, err := st.CreateRun(ctx, id, "native", nil); err != nil {
		t.Fatal(err)
	}
	identity := store.PortExecutionIdentity{Source: "private-source-hash", Graph: "graph", Contract: "contract", Policy: "policy", Inputs: "inputs"}
	state := &store.PortExecution{Version: store.PortExecutionVersion, Revision: 1, Generation: 1, RootRunID: id,
		Identity: identity, Invocations: map[string]*store.PortInvocation{}, Collections: map[string]*store.PortCollection{},
		Publications: map[string]*store.PortValue{}, Exports: map[string]string{}, Products: []string{"report"},
		Budget: store.PortBudgetState{Consumed: store.PortBudgetAmount{CostUSD: 1.25, Iterations: 1}, Reservations: map[string]store.PortBudgetAmount{}},
	}
	if err := store.SavePortExecution(context.Background(), st, id, 0, state); err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, s, "local_run_get", `{"run_id":"pc1_mcp_native_progress"}`)
	if isErr || strings.Contains(out, "private-source-hash") {
		t.Fatalf("native run get leaked technical state or failed: %s", out)
	}
	var result struct {
		RuntimeSemantics string `json:"runtime_semantics"`
		NativeProgress   struct {
			Products map[string]string `json:"products"`
			Usage    struct {
				ReportedCostUSD float64 `json:"reported_cost_usd"`
				TotalCostKnown  bool    `json:"total_cost_known"`
			} `json:"usage"`
		} `json:"native_progress"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.RuntimeSemantics != store.RuntimeSemanticsPortsV1 ||
		result.NativeProgress.Products["report"] != "waiting" || result.NativeProgress.Usage.ReportedCostUSD != 1.25 ||
		!result.NativeProgress.Usage.TotalCostKnown {
		t.Fatalf("native public progress missing: %v %s", err, out)
	}
}
