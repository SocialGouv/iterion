// This file is package model_test rather than model on purpose: it imports
// pkg/runtime/recovery, and pkg/runtime imports pkg/backend/model, so an
// internal test file could not reach the dispatcher without an import cycle.
// The external package is what lets the assertion span the whole chain.
package model_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
)

// stubResolver hands the action executor a one-operation package pointed at a
// test server. It is the seam pkg/connection will implement for real.
type stubResolver struct {
	pkg     *spec.Package
	op      spec.Operation
	baseURL string
}

func (s *stubResolver) ResolveAction(_ context.Context, _, _ string) (*spec.Package, spec.Operation, exec.Credential, string, error) {
	return s.pkg, s.op, exec.Credential{SchemeID: "token", Value: "s3cret"}, s.baseURL, nil
}

// mutatingPackage is a package whose single operation is a POST with NO
// idempotency key — the shape for which a repeat is a second effect.
func mutatingPackage(base string) (*spec.Package, spec.Operation) {
	op := spec.Operation{
		ID: "probe.issue.comment", Resource: "issue", Verb: "comment",
		HTTP: spec.HTTPBinding{
			Method: "POST", Path: "/comments", RequestBody: spec.BodyJSON,
		},
		Effect: spec.EffectCreate, Deterministic: true,
		Params:  []spec.Param{{Key: "body", Name: "body", In: spec.InBody, Type: "string", Required: true}},
		Results: []spec.ResultCase{{Status: 201}},
	}
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probe", Version: "1.0.0",
			BaseURL: spec.BaseURL{Default: base},
			Auth: []spec.AuthScheme{{
				ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization",
			}},
			Maturity: spec.MaturityExperimental,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "probe", Domain: "issue",
			Operations: []spec.Operation{op},
		}},
	}, op
}

// TestAnAmbiguousMutationIsNeverRetriedAutomatically is the end-to-end guard
// for the executor's central safety promise.
//
// The promise is that a mutating call whose ANSWER was lost — the request left,
// the remote may well have committed it, nothing says whether it did — is never
// repeated on iterion's own initiative. The executor classified that correctly
// all along. What made the promise false was the NODE BOUNDARY: the typed error
// was formatted into a string, so the engine's recovery dispatcher (which
// classifies by type) saw unremarkable text, bucketed it as EXECUTION_FAILED,
// and scheduled a retry two seconds later.
//
// So the assertion deliberately spans both halves. Testing the executor alone
// certifies a component that was never wrong; only running the real dispatcher
// over the real node error certifies the property an operator cares about.
func TestAnAmbiguousMutationIsNeverRetriedAutomatically(t *testing.T) {
	// A server that accepts the request and then hangs up WITHOUT answering:
	// the remote has seen the mutation, the caller learns nothing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server cannot hijack; cannot simulate a lost answer")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	}))
	defer srv.Close()

	pkg, op := mutatingPackage(srv.URL)
	node := &ir.ToolNode{
		BaseNode:   ir.BaseNode{ID: "comment"},
		Action:     "probe.issue.comment",
		Connection: "main",
		Params:     []ir.ActionParam{{Key: "body", Value: "ship it"}},
	}

	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, srv.Client()))

	_, err := e.Execute(context.Background(), node, nil)
	if err == nil {
		t.Fatal("a lost answer to a POST must fail the node, not report success")
	}

	// Half one: the typed error survived the node boundary.
	var cerr *exec.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("the connector error must survive the node boundary as a TYPED error, got %T: %v", err, err)
	}
	if cerr.Class != spec.ErrUnknownOutcome {
		t.Fatalf("class = %q, want %q — a POST that left with no answer is undecided, not a plain transport failure", cerr.Class, spec.ErrUnknownOutcome)
	}

	// Half two: the REAL recovery dispatcher refuses to retry it. This is the
	// assertion that was false, and no amount of executor-side correctness
	// would have caught it.
	if got := recovery.Classify(err); got != runtime.ErrCodeAmbiguousEffect {
		t.Errorf("recovery classified it as %q, want %q", got, runtime.ErrCodeAmbiguousEffect)
	}
	dispatch := recovery.Dispatch(recovery.DefaultRecipes())
	action, code := dispatch(context.Background(), err, func(runtime.ErrorCode) int { return 0 })
	if action.Kind == runtime.RecoveryRetrySameNode {
		t.Fatalf("the engine scheduled a RETRY of an ambiguous mutation (delay %s, reason %q) — this is the duplicate the class exists to prevent", action.Delay, action.Reason)
	}
	if action.Kind != runtime.RecoveryFailTerminal {
		t.Errorf("action = %v, want FailTerminal", action.Kind)
	}
	if code != runtime.ErrCodeAmbiguousEffect {
		t.Errorf("dispatched code = %q, want %q", code, runtime.ErrCodeAmbiguousEffect)
	}
	// The operator reads this reason at 3am with a possibly-duplicated
	// mutation in front of them; it has to say what to do.
	if action.Reason == "" {
		t.Error("the refusal must carry a reason naming the reconciliation the operator has to do")
	}
}

// TestRuntimeEnforcesTheActionInvariants covers the gap between what the
// COMPILER refuses and what the RUNTIME accepts.
//
// C261/C262 reject a postcondition or a recovery block on an action node, and
// that is where an author meets the rule. It is not where the rule protects
// anything: this executor runs whatever IR it is handed, and an IR can be
// hand-built, restored, or produced by a path written later. Each case below
// is a shape the compiler refuses, handed straight to the executor.
//
// The postcondition case is the one that mattered: it did not merely slip
// through, it took the ADR-044 ladder, whose first rung is an idempotent-skip
// — so the node reported SUCCESS while no HTTP request was ever made. A server
// that fails the test if it is ever touched is what proves it.
func TestRuntimeEnforcesTheActionInvariants(t *testing.T) {
	var touched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		touched = true
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer srv.Close()
	pkg, op := mutatingPackage(srv.URL)

	for _, tc := range []struct {
		name    string
		node    *ir.ToolNode
		wantMsg string
	}{
		{
			name: "a postcondition would let the ladder skip the call entirely",
			node: &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "n"}, Action: "probe.issue.comment", Connection: "main",
				Params:        []ir.ActionParam{{Key: "body", Value: "x"}},
				Postcondition: "true",
			},
			wantMsg: "postcondition",
		},
		{
			name: "a recovery block ends in an LLM agent",
			node: &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "n"}, Action: "probe.issue.comment", Connection: "main",
				Params:   []ir.ActionParam{{Key: "body", Value: "x"}},
				Recovery: &ir.RecoverySpec{},
			},
			wantMsg: "recovery",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			touched = false
			e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
				model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, srv.Client()))
			out, err := e.Execute(context.Background(), tc.node, nil)
			if err == nil {
				t.Fatalf("the runtime must refuse this node, got output %v", out)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refusal = %v, want it to name %q", err, tc.wantMsg)
			}
			if touched {
				t.Error("the vendor was called by a node the runtime should have refused before executing")
			}
		})
	}
}

// TestRuntimeRefusesAnOperationTheResolverShouldNotHaveOffered guards the two
// properties a `.bot` author cannot see, and which arrive at run time from a
// tier the workflow never named.
func TestRuntimeRefusesAnOperationTheResolverShouldNotHaveOffered(t *testing.T) {
	var touched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		touched = true
		w.WriteHeader(201)
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name    string
		mutate  func(*spec.Package, *spec.Operation)
		wantMsg string
	}{
		{
			// `action:` IS the determinism claim; an operation that does not
			// make it may still be reached through an agent's capability.
			name:    "the operation is not deterministic",
			mutate:  func(_ *spec.Package, op *spec.Operation) { op.Deterministic = false },
			wantMsg: "deterministic",
		},
		{
			// `spotted` is how a catalog says "recorded, promising nothing".
			name:    "the package is merely spotted",
			mutate:  func(p *spec.Package, _ *spec.Operation) { p.Connector.Maturity = spec.MaturitySpotted },
			wantMsg: "maturity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			touched = false
			pkg, op := mutatingPackage(srv.URL)
			tc.mutate(pkg, &op)
			pkg.Ops[0].Operations[0] = op

			node := &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "n"}, Action: "probe.issue.comment", Connection: "main",
				Params: []ir.ActionParam{{Key: "body", Value: "x"}},
			}
			e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
				model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, srv.Client()))
			if _, err := e.Execute(context.Background(), node, nil); err == nil {
				t.Fatal("the runtime must refuse it")
			} else if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refusal = %v, want it to name %q", err, tc.wantMsg)
			}
			if touched {
				t.Error("the vendor was called for an operation the node may not run")
			}
		})
	}

	// The falsifier: a well-formed operation still runs. A guard that refused
	// everything would pass both cases above.
	touched = false
	pkg, op := mutatingPackage(srv.URL)
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "n"}, Action: "probe.issue.comment", Connection: "main",
		Params: []ir.ActionParam{{Key: "body", Value: "x"}},
	}
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, srv.Client()))
	if _, err := e.Execute(context.Background(), node, nil); err != nil {
		t.Fatalf("a deterministic operation in an experimental package must run: %v", err)
	}
	if !touched {
		t.Error("the well-formed case never reached the vendor — the guard is refusing everything")
	}
}

// TestAnOrdinaryFailureStillRetries is the falsifier for the guard above: a
// blanket "connector errors never retry" rule would pass that test while
// making every transient blip terminal. A GET refused with a 503 never
// happened, so repeating it is free.
func TestAnOrdinaryFailureStillRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message": "upstream is restarting"}`))
	}))
	defer srv.Close()

	pkg, op := mutatingPackage(srv.URL)
	op.HTTP.Method = "GET"
	op.HTTP.RequestBody = ""
	op.Params = nil
	op.Effect = spec.EffectRead
	pkg.Ops[0].Operations[0] = op

	node := &ir.ToolNode{
		BaseNode:   ir.BaseNode{ID: "read"},
		Action:     "probe.issue.comment",
		Connection: "main",
	}
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, srv.Client()))

	_, err := e.Execute(context.Background(), node, nil)
	if err == nil {
		t.Fatal("a 503 must fail the node")
	}
	if runtime.IsAmbiguousEffect(err) {
		t.Fatal("a REFUSED request never took effect — marking it ambiguous would park runs that should retry")
	}
	if got := recovery.Classify(err); got == runtime.ErrCodeAmbiguousEffect {
		t.Errorf("classified as %q; a 503 is a transient failure, not an undecided one", got)
	}
}
