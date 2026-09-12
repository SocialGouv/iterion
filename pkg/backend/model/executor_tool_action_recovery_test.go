// This file is package model_test rather than model on purpose: it imports
// pkg/runtime/recovery, and pkg/runtime imports pkg/backend/model, so an
// internal test file could not reach the dispatcher without an import cycle.
// The external package is what lets the assertion span the whole chain.
package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/permissions"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
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
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))

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
				model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
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
				model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
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
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
	if _, err := e.Execute(context.Background(), node, nil); err != nil {
		t.Fatalf("a deterministic operation in an experimental package must run: %v", err)
	}
	if !touched {
		t.Error("the well-formed case never reached the vendor — the guard is refusing everything")
	}
}

// countingClassifier records every model consultation. A classifier is the
// only component on the tool-policy path that calls an LLM, so counting its
// invocations counts the model requests.
type countingClassifier struct{ calls int }

func (c *countingClassifier) Classify(context.Context, string, map[string]any) (permissions.Decision, error) {
	c.calls++
	return permissions.DecisionAllow, nil
}

// TestNoModelIsConsultedForAnActionEvenWithTheClassifierENABLED is the
// acceptance test for the recipe's central promise, and it is deliberately
// BEHAVIOURAL rather than a reading of the default configuration.
//
// `action:` is documented as the recipe with no model in the path. Two things
// could put one there, and only one of them was closed: the compiler refuses a
// recovery ladder and a postcondition (C261/C262, enforced at runtime since
// the round-two fixes). The other was `ITERION_LLM_CLASSIFIER_MODEL`, which
// chains an LLM classifier over the SHARED tool-node policy check that every
// recipe goes through — so a deployment setting it put a model call in front
// of every action node, with nothing announcing it.
//
// Testing the default configuration would have passed throughout: the
// classifier is off by default. The claim is only worth something when the
// classifier is ON, which is why this test enables it.
func TestNoModelIsConsultedForAnActionEvenWithTheClassifierEnabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer srv.Close()
	pkg, op := mutatingPackage(srv.URL)

	counter := &countingClassifier{}
	policy := &tool.ClassifierChecker{Classifier: counter}

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "comment"}, Action: "probe.issue.comment", Connection: "main",
		Params: []ir.ActionParam{{Key: "body", Value: "ship it"}},
	}
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithToolPolicy(policy),
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))

	if _, err := e.Execute(context.Background(), node, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if counter.calls != 0 {
		t.Errorf("the classifier was consulted %d time(s); an action node is certified to involve NO model, and the classifier is one", counter.calls)
	}

	// The falsifier: the same policy on an ordinary command node DOES consult
	// it. Without this, a broken classifier would satisfy the assertion above
	// and prove nothing.
	shell := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "sh"}, Command: "true"}
	e2 := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{}, model.WithToolPolicy(policy))
	_, _ = e2.Execute(context.Background(), shell, nil)
	if counter.calls == 0 {
		t.Error("the classifier was never consulted at all — the assertion above proves nothing about action nodes")
	}
}

// TestTheOperatorsOwnRulesStillApplyToAnAction is the other half: what the
// action path skips is the MODEL, not the policy. An operator who denied a
// connector must still see it denied.
func TestTheOperatorsOwnRulesStillApplyToAnAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the vendor must not be reached by a denied node")
	}))
	defer srv.Close()
	pkg, op := mutatingPackage(srv.URL)

	// A classifier that would ALLOW, over a deterministic base that denies.
	// The base must win: skipping the model must not skip the rules.
	policy := &tool.ClassifierChecker{
		Classifier: &countingClassifier{},
		Base:       tool.DenyAllPolicy(),
	}
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "comment"}, Action: "probe.issue.comment", Connection: "main",
		Params: []ir.ActionParam{{Key: "body", Value: "x"}},
	}
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithToolPolicy(policy),
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))

	if _, err := e.Execute(context.Background(), node, nil); err == nil {
		t.Fatal("a denied connector action must not execute")
	}
}

// TestASecretReferenceReachesTheVendorAsItsVALUE.
//
// A `{{secrets.NAME}}` ref renders to a PLACEHOLDER, not a value — that is the
// design, so a secret never sits in a command line or a log. Every other
// recipe materialises it before use; the action path did not, so the literal
// text `__ITERION_SECRET_NAME__` travelled to the vendor as the argument. The
// symptom is a 401 that reads like a bad credential, on a run whose secret is
// perfectly valid.
//
// The oracle is the SERVER: what the vendor received is the only thing that
// settles it.
func TestASecretReferenceReachesTheVendorAsItsVALUE(t *testing.T) {
	const secretValue = "gh-pat-not-a-real-token"
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer srv.Close()

	pkg, op := mutatingPackage(srv.URL)
	guard := secretguard.New([]secretguard.Secret{{Name: "MY_SECRET", Value: secretValue}},
		secretguard.DefaultConfig())

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "comment"}, Action: "probe.issue.comment", Connection: "main",
		Params: []ir.ActionParam{{
			Key:   "body",
			Value: "{{secrets.MY_SECRET}}",
			Refs:  []*ir.Ref{{Kind: ir.RefSecrets, Path: []string{"MY_SECRET"}, Raw: "{{secrets.MY_SECRET}}"}},
		}},
	}
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithSecretGuard(guard),
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))

	if _, err := e.Execute(context.Background(), node, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(receivedBody, "__ITERION_SECRET") {
		t.Errorf("the vendor received the PLACEHOLDER instead of the secret: %s", receivedBody)
	}
	if !strings.Contains(receivedBody, secretValue) {
		t.Errorf("the vendor did not receive the secret's value: %s", receivedBody)
	}
}

// TestRetryIsHonouredAndStillCannotDuplicateAnEffect.
//
// `retry:` compiled and was stored on the IR, and nothing read it: an author
// who wrote `retry: 3` got the engine's generic one-shot recovery instead. A
// control that reads as working and does nothing is worse than an absent one,
// because it is documented.
//
// The second half is the part that matters. Making the control real must not
// make it a way to duplicate a mutation, so the SAME `retry: 5` that patiently
// re-drives a throttled read performs a lost-answer POST exactly once.
func TestRetryIsHonouredAndStillCannotDuplicateAnEffect(t *testing.T) {
	t.Run("a retryable failure is re-driven", func(t *testing.T) {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			if calls < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok": true}`))
		}))
		defer srv.Close()

		pkg, op := mutatingPackage(srv.URL)
		op.HTTP.Method, op.HTTP.RequestBody, op.Params, op.Effect = "GET", "", nil, spec.EffectRead
		op.Results = []spec.ResultCase{{Status: 200}}
		pkg.Ops[0].Operations[0] = op

		node := &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "read"}, Action: "probe.issue.comment",
			Connection: "main", RetryPolicy: "5",
		}
		e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
			model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
		if _, err := e.Execute(context.Background(), node, nil); err != nil {
			t.Fatalf("the third attempt succeeded, so the node must: %v", err)
		}
		if calls != 3 {
			t.Errorf("calls = %d, want 3 — `retry: 5` must actually re-drive the call", calls)
		}
	})

	t.Run("an ambiguous mutation is performed exactly once", func(t *testing.T) {
		// ATOMIC, unlike the counter above, and the difference is not style.
		// A handler that answers normally is ordered against the test by the
		// response round trip: the client returns only after reading what the
		// handler wrote. A handler that HIJACKS and closes writes no response
		// at all, so `Do` returns the moment the socket dies — possibly before
		// the handler's own statements finish. There is no happens-before edge
		// left, and the race detector is right to say so.
		var calls atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("cannot hijack")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		}))
		defer srv.Close()

		pkg, op := mutatingPackage(srv.URL)
		node := &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "comment"}, Action: "probe.issue.comment",
			Connection: "main", RetryPolicy: "5",
			Params: []ir.ActionParam{{Key: "body", Value: "ship it"}},
		}
		e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
			model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
		if _, err := e.Execute(context.Background(), node, nil); err == nil {
			t.Fatal("a lost answer must fail the node")
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("the vendor was called %d times; a mutation whose outcome is unknown must be performed EXACTLY ONCE, whatever `retry:` says", n)
		}
	})
}

// TestTheENGINENeverRetriesAnUndecidedMutation covers the CLASS, not the one
// error the first fix reached.
//
// Round two closed `unknown_outcome` — the answer that never arrived. Round
// three found the same defect one error class over: a POST answered 500 is
// `upstream` like any other 500, the connector refuses its own retry, and the
// ENGINE dispatched a second POST two seconds later. The write may well have
// committed before the server failed, so that is a duplicate.
//
// Fixing the site the report names and leaving the class alive is the mistake
// this repository's own rule warns about, and it was made here. The predicate
// is now decided where the facts are — status plus the operation's effect —
// and covers every way a dispatched mutation can end undecided.
func TestTheEngineNeverRetriesAnUndecidedMutation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		handler  http.HandlerFunc
		wantAmbi bool
	}{
		{
			// The write may have committed before the server failed.
			name: "a 500 on a mutation",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			},
			wantAmbi: true,
		},
		{
			// The mutation CERTAINLY happened; only its answer is lost.
			name: "a 201 whose body will not decode",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{not json`))
			},
			wantAmbi: true,
		},
		{
			// A refusal: nothing happened, so a retry duplicates nothing and
			// parking the run would be wrong.
			name: "a 400 on a mutation",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"bad"}`))
			},
			wantAmbi: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			pkg, op := mutatingPackage(srv.URL)

			node := &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "comment"}, Action: "probe.issue.comment",
				Connection: "main",
				Params:     []ir.ActionParam{{Key: "body", Value: "ship it"}},
			}
			e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
				model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
			_, err := e.Execute(context.Background(), node, nil)
			if err == nil {
				t.Fatal("the node must fail")
			}

			got := runtime.IsAmbiguousEffect(err)
			if got != tc.wantAmbi {
				t.Fatalf("IsAmbiguousEffect = %v, want %v", got, tc.wantAmbi)
			}

			// The engine's REAL dispatcher is the oracle: the connector's own
			// opinion was never the thing that failed.
			dispatch := recovery.Dispatch(recovery.DefaultRecipes())
			action, _ := dispatch(context.Background(), err, func(runtime.ErrorCode) int { return 0 })
			retried := action.Kind == runtime.RecoveryRetrySameNode
			if tc.wantAmbi && retried {
				t.Errorf("the engine scheduled a retry (delay %s) of a mutation that may already have landed", action.Delay)
			}
			if !tc.wantAmbi && !retried {
				t.Error("a refused request must still be retryable — parking it would be the opposite defect")
			}
		})
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
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))

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

// TestParameterValuesReachTheVendorINTACT covers the four corruptions the
// second review reproduced. Each is silent — the call succeeds and carries
// something other than what the author wrote — which is why the SERVER is the
// oracle in every case.
func TestParameterValuesReachTheVendorIntact(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared string
		value    string
		input    map[string]any
		want     string // the exact JSON the vendor must receive for `body`
	}{
		{
			// A reference EMBEDDED in text is string interpolation. Rendering
			// it as a JSON literal produced `hello "Alice"` — quotes and all —
			// and the surrounding text made the result un-decodable, so
			// nothing downstream could undo it.
			name: "an embedded reference interpolates as text", declared: "string",
			value: "hello {{input.who}}",
			input: map[string]any{"who": "Alice"},
			want:  `"hello Alice"`,
		},
		{
			// The whole-value case must still deliver the TYPE, which is what
			// lets a template — always text — reach an integer field.
			name: "a whole-value reference keeps its type", declared: "integer",
			value: "{{input.n}}",
			input: map[string]any{"n": 42},
			want:  `42`,
		},
		{
			// float64's 53-bit mantissa silently rewrote a large id to an
			// adjacent one, which addresses a different resource and looks
			// entirely plausible in a log.
			name: "a large integer keeps every digit", declared: "integer",
			value: "9007199254740993",
			want:  `9007199254740993`,
		},
		{
			// An empty rendering used to mean "absent", dropping an author's
			// deliberate empty string — which for several vendors is the value
			// that CLEARS a field.
			name: "an empty string is a value, not an absence", declared: "string",
			value: "",
			want:  `""`,
		},
		{
			// AUTHORED TEXT is not JSON. Reading it as JSON trimmed it, and
			// the whitespace may be the point — a code fence, an indented
			// snippet, a markdown block.
			name: "a literal keeps its whitespace", declared: "string",
			value: "  keep whitespace  ",
			want:  `"  keep whitespace  "`,
		},
		{
			// `null` is an ordinary word. Read as the JSON literal, it made
			// the argument vanish and the request go out without it.
			name: "a literal `null` is the word, not an absence", declared: "string",
			value: "null",
			want:  `"null"`,
		},
		{
			// A literal that merely LOOKS like JSON is still what the author
			// wrote.
			name: "a literal that looks like JSON stays text", declared: "string",
			value: `{"a":1}`,
			want:  `"{\"a\":1}"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				var payload map[string]json.RawMessage
				_ = json.Unmarshal(b, &payload)
				got = string(payload["body"])
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			pkg, op := mutatingPackage(srv.URL)
			op.Params[0].Type = tc.declared
			pkg.Ops[0].Operations[0] = op

			node := &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "n"}, Action: "probe.issue.comment", Connection: "main",
				Params: []ir.ActionParam{{Key: "body", Value: tc.value, Refs: refsOf(tc.value)}},
			}
			e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
				model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
			if _, err := e.Execute(context.Background(), node, tc.input); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got != tc.want {
				t.Errorf("the vendor received %s, want %s", got, tc.want)
			}
		})
	}
}

// TestAValueOfTheWrongTYPEIsRefused. Coercion used to fall through and send
// the value unchanged whenever the type matched no case, so `1.5` reached a
// parameter declared `integer`. The vendor then answers 400, or coerces it
// silently in a way the workflow never sees — on a path whose whole promise is
// that the request is what was declared.
func TestAValueOfTheWrongTypeIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, declared, value string }{
		{"a fraction where an integer is declared", "integer", "1.5"},
		{"a number where a boolean is declared", "boolean", "123"},
		{"an array where a scalar is declared", "boolean", "[1,2]"},
		// NOT listed: a literal that merely LOOKS like JSON under a string
		// parameter. `body: '{"a":1}'` is an author writing that text, and
		// sending it verbatim is right — see the literal cases in
		// TestParameterValuesReachTheVendorIntact.
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("the vendor must not be reached with a value the operation does not declare")
			}))
			defer srv.Close()

			pkg, op := mutatingPackage(srv.URL)
			op.Params[0].Type = tc.declared
			pkg.Ops[0].Operations[0] = op

			node := &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "n"}, Action: "probe.issue.comment", Connection: "main",
				Params: []ir.ActionParam{{Key: "body", Value: tc.value}},
			}
			e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
				model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))
			if _, err := e.Execute(context.Background(), node, nil); err == nil {
				t.Fatal("the node must refuse a value of the wrong type")
			}
		})
	}
}

// refsOf builds the parsed refs a compiled node would carry, so these tests
// exercise the shape the compiler produces rather than a hand-made one.
func refsOf(value string) []*ir.Ref {
	var out []*ir.Ref
	rest := value
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			return out
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return out
		}
		raw := rest[i : i+j+2]
		inner := strings.TrimSpace(raw[2 : len(raw)-2])
		if path, ok := strings.CutPrefix(inner, "input."); ok {
			out = append(out, &ir.Ref{Kind: ir.RefInput, Path: strings.Split(path, "."), Raw: raw})
		}
		rest = rest[i+j+2:]
	}
}

// TestRetriesDoNotHammerAVendorThatNamedNoDelay.
//
// The loop waited only when the vendor sent a Retry-After, and re-issued the
// call IMMEDIATELY otherwise — so a 429 with no header, a 503 and every
// retryable transport failure went back to back. "iterion does not invent a
// backoff" is defensible about the LENGTH of a delay; the alternative chosen
// was zero, which is the hammering pattern.
//
// The oracle is the vendor's own clock: the gap between the attempts it
// actually received. The floor asserted is far below the minimum the backoff
// can produce (250ms at the first attempt, jitter included) and far above
// scheduler noise, so it is neither flaky nor vacuous.
func TestRetriesDoNotHammerAVendorThatNamedNoDelay(t *testing.T) {
	var mu sync.Mutex
	var seen []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		seen = append(seen, time.Now())
		n := len(seen)
		mu.Unlock()
		if n < 3 {
			// No Retry-After header: the case the loop had no answer for.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()

	pkg, op := mutatingPackage(srv.URL)
	op.HTTP.Method, op.HTTP.RequestBody, op.Params, op.Effect = "GET", "", nil, spec.EffectRead
	op.Results = []spec.ResultCase{{Status: 200}}
	pkg.Ops[0].Operations[0] = op

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "read"}, Action: "probe.issue.comment",
		Connection: "main", RetryPolicy: "5",
	}
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))

	if _, err := e.Execute(context.Background(), node, nil); err != nil {
		t.Fatalf("the third attempt succeeds, so the node must: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("the vendor saw %d attempts, want 3", len(seen))
	}
	const floor = 100 * time.Millisecond
	for i := 1; i < len(seen); i++ {
		if gap := seen[i].Sub(seen[i-1]); gap < floor {
			t.Errorf("attempt %d followed attempt %d after %v, under the %v floor — the vendor is being hammered",
				i+1, i, gap, floor)
		}
	}
}

// TestARetriedNodeReportsWhatTheVendorACTUALLYServed.
//
// `requests` exists because a paginated action can spend twenty of a vendor's
// rate-limit slots behind what looks like one call, and nothing said so — "the
// operator found out on the vendor's dashboard". It was read off the LAST
// attempt's result, which the retry loop replaces on every pass: a node that
// failed twice before succeeding reported nothing at all, which is the same
// invisibility one level up.
//
// A READ, deliberately: a retried mutation is refused by Retryable, and the
// point here is the accounting, not the safety rule.
func TestARetriedNodeReportsWhatTheVendorActuallyServed(t *testing.T) {
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		if served < 3 {
			// No Retry-After, so the node takes its own small backoff.
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"slow down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	pkg, op := mutatingPackage(srv.URL)
	op.HTTP.Method, op.HTTP.RequestBody, op.Effect = "GET", "", spec.EffectRead
	op.Params, op.Results = nil, []spec.ResultCase{{Status: 200}}
	pkg.Ops[0].Operations[0] = op

	node := &ir.ToolNode{
		BaseNode:    ir.BaseNode{ID: "list"},
		Action:      "probe.issue.comment",
		Connection:  "main",
		RetryPolicy: "3",
	}
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))

	out, err := e.Execute(context.Background(), node, nil)
	if err != nil {
		t.Fatalf("a retryable 429 that then succeeds must not fail the node: %v", err)
	}
	if served != 3 {
		t.Fatalf("the vendor served %d requests, want 3", served)
	}
	got, ok := out["requests"]
	if !ok {
		t.Fatal("a node that cost the vendor three requests must say so — reading the last attempt alone reported nothing")
	}
	if n, _ := got.(int); n != 3 {
		t.Errorf("requests = %v, want 3 — every attempt the vendor actually served", got)
	}
}

// TestAFailedNodeAlsoReportsWhatTheVendorServed.
//
// The count above was reported by `actionOutput`, which only a SUCCEEDING node
// reaches. So the case an operator actually investigates — a node that
// exhausted its retries against a rate limit — reported nothing at all, while
// having spent the most slots of any shape. The cost belongs on the failure
// too, and the error message is where a failed node's story is read.
func TestAFailedNodeAlsoReportsWhatTheVendorServed(t *testing.T) {
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"slow down"}`))
	}))
	defer srv.Close()

	// A READ: a 429 is retryable for any effect, but keeping it a read means
	// the assertion is about the accounting and not about the safety rule.
	pkg, op := mutatingPackage(srv.URL)
	op.HTTP.Method, op.HTTP.RequestBody, op.Effect = "GET", "", spec.EffectRead
	op.Params, op.Results = nil, []spec.ResultCase{{Status: 200}}
	pkg.Ops[0].Operations[0] = op

	node := &ir.ToolNode{
		BaseNode:    ir.BaseNode{ID: "list"},
		Action:      "probe.issue.comment",
		Connection:  "main",
		RetryPolicy: "2",
	}
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithConnectors(&stubResolver{pkg: pkg, op: op, baseURL: srv.URL}, exec.MarkGuarded(srv.Client())))

	_, err := e.Execute(context.Background(), node, nil)
	if err == nil {
		t.Fatal("every attempt was refused, so the node must fail")
	}
	if served != 3 {
		t.Fatalf("the vendor served %d requests, want 3 (the first attempt plus `retry: 2`)", served)
	}
	if !strings.Contains(err.Error(), "after 3 requests") {
		t.Errorf("error = %v, want it to name the 3 requests the vendor served — a failing node is where those slots are looked for", err)
	}
}
