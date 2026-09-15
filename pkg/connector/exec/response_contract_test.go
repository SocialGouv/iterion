package exec_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// withContract opts ONE status of one operation into a v2 response contract
// and hands back the operation the call must use.
//
// It fails when the status is not declared, rather than attaching nothing: a
// helper that quietly opted nothing in would leave every test below green
// while exercising the v1 path.
func withContract(t *testing.T, pkg *spec.Package, id string, status int, schema spec.ResponseSchema) spec.Operation {
	t.Helper()
	op := opOf(t, pkg, id)
	pkg.ResponseSchemas = map[string]spec.ResponseSchema{"result": schema}
	results := append([]spec.ResultCase(nil), op.Results...)
	opted := false
	for i := range results {
		if results[i].Status == status {
			results[i].ResponseSchemaRef = "result"
			opted = true
		}
	}
	if !opted {
		t.Fatalf("operation %q declares no status %d to opt in", id, status)
	}
	op.Results = results
	// A package a launch would have refused is no oracle for what a launch does.
	if err := pkg.ValidateResponseContracts(op); err != nil {
		t.Fatalf("contract preflight refused the fixture: %v", err)
	}
	return op
}

func commentArgs() map[string]any {
	return map[string]any{"owner": "acme", "repo": "widgets", "index": 1, "body": "abc-123"}
}

// TestAResponseBreakingItsContractIsRefusedAndNotReplayed is the whole ticket
// in one call: the vendor answers 201 with a shape it promised not to send,
// and the mutation must be reported as done-but-unreadable rather than retried.
func TestAResponseBreakingItsContractIsRefusedAndNotReplayed(t *testing.T) {
	const secret = "vendor-private-token-DO-NOT-PUBLISH"
	calls := 0
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"not-an-integer","note":"` + secret + `"}`))
	})
	defer done()

	op := withContract(t, pkg, "probe.issue.comment", 201, spec.ResponseSchema{
		Type: "object", Required: []string{"id"},
		Properties: map[string]spec.ResponseSchema{"id": {Type: "integer"}},
	})
	// The key is sent ON PURPOSE. Without it the refusal would be explained by
	// the pre-existing no-key rule and this test would prove nothing new.
	op.IdempotencyKeyParam = "body"
	args := commentArgs()

	res, err := e.Call(context.Background(), pkg, op, args, creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Err == nil {
		t.Fatal("a 201 whose body breaks the declared contract must not read as success")
	}
	if calls != 1 {
		t.Errorf("HTTP calls = %d, want 1 — Call performs exactly one attempt", calls)
	}
	if res.Data != nil {
		t.Errorf("Data = %v, want nil — a body the contract refused must not stay readable", res.Data)
	}
	if !res.Err.Ambiguous {
		t.Error("the vendor answered 2xx, so the mutation happened: the failure is undecided")
	}
	if !res.Err.AmbiguousEffect() {
		t.Error("the engine's recovery dispatcher must refuse to replay this on resume")
	}
	if res.Err.Retryable(op, args) {
		t.Error("a mutation the vendor CONFIRMED must not be repeated, idempotency key or not")
	}
	if strings.Contains(res.Err.Error(), secret) {
		t.Errorf("the refusal echoed the response body: %v", res.Err)
	}
}

// TestOnlyAConfirmedMutationLosesTheRetryItsKeyBought mutates the distinction
// in BOTH directions.
//
// Every case below is a mutating operation carrying a real idempotency key, so
// the ONLY thing that may change the answer is whether the vendor's reply
// proves the mutation happened. Keying on the 2xx range alone would take the
// retry away from Slack-shaped `ok:false`; keying on Ambiguous alone would take
// it away from the 5xx and 3xx cases, where a key is exactly what makes the
// second chance safe.
func TestOnlyAConfirmedMutationLosesTheRetryItsKeyBought(t *testing.T) {
	pkg := probe("http://example.invalid")
	op := opOf(t, pkg, "probe.issue.comment")
	op.IdempotencyKeyParam = "body"
	sent := commentArgs()

	for _, tc := range []struct {
		name      string
		err       *exec.Error
		retryable bool
	}{
		{
			"a 2xx iterion could not read: the mutation is CONFIRMED",
			&exec.Error{Class: spec.ErrUpstream, Status: 201, Ambiguous: true},
			false,
		},
		{
			"a 2xx the vendor sent to report its OWN failure: nothing happened",
			&exec.Error{Class: spec.ErrUpstream, Status: 200},
			true,
		},
		{
			"a 5xx: the mutation may never have happened",
			&exec.Error{Class: spec.ErrUpstream, Status: 503, Ambiguous: true},
			true,
		},
		{
			"a 303: the same may-have, and the request may need re-sending",
			&exec.Error{Class: spec.ErrUpstream, Status: 303, Ambiguous: true},
			true,
		},
	} {
		if got := tc.err.Retryable(op, sent); got != tc.retryable {
			t.Errorf("%s: Retryable = %v, want %v", tc.name, got, tc.retryable)
		}
	}

	// The exemption is about a CONFIRMED effect, not about mutations at large:
	// a read answering the same unreadable 2xx stays repeatable.
	read := opOf(t, pkg, "probe.issue.get")
	confirmed := &exec.Error{Class: spec.ErrUpstream, Status: 200, Ambiguous: true}
	if !confirmed.Retryable(read, nil) {
		t.Error("a read changed nothing, so repeating it duplicates nothing")
	}
}

// TestAResponseContractIsOptInAndLeavesEveryOtherAnswerAlone pins the blast
// radius: the identical body is accepted where no contract was declared, and a
// 202 keeps meaning "pending" when its contract holds.
func TestAResponseContractIsOptInAndLeavesEveryOtherAnswerAlone(t *testing.T) {
	const wrongShape = `{"id":"not-an-integer"}`
	integerID := spec.ResponseSchema{
		Type: "object", Required: []string{"id"},
		Properties: map[string]spec.ResponseSchema{"id": {Type: "integer"}},
	}

	t.Run("no contract declared: the v1 path is untouched", func(t *testing.T) {
		e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(201)
			_, _ = w.Write([]byte(wrongShape))
		})
		defer done()
		// Contracts exist in the package, but this STATUS opted into none.
		pkg.ResponseSchemas = map[string]spec.ResponseSchema{"result": integerID}
		res, err := e.Call(context.Background(), pkg, opOf(t, pkg, "probe.issue.comment"), commentArgs(), creds())
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !res.OK() {
			t.Fatalf("an un-opted status must keep its historical reading: %v", res.Err)
		}
		if res.Data == nil {
			t.Error("Data must survive where nothing refused it")
		}
	})

	t.Run("a 202 whose contract holds is still pending", func(t *testing.T) {
		e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":7}`))
		})
		defer done()
		op := withContract(t, pkg, "probe.issue.comment", 202, integerID)
		res, err := e.Call(context.Background(), pkg, op, commentArgs(), creds())
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if res.Err != nil {
			t.Fatalf("a body satisfying its contract must pass: %v", res.Err)
		}
		if !res.Pending {
			t.Error("202 means accepted, not done — validating its body must not settle it")
		}
	})

	t.Run("a 202 whose contract breaks is a failure, not a pending success", func(t *testing.T) {
		e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(202)
			_, _ = w.Write([]byte(wrongShape))
		})
		defer done()
		op := withContract(t, pkg, "probe.issue.comment", 202, integerID)
		res, err := e.Call(context.Background(), pkg, op, commentArgs(), creds())
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if res.Err == nil {
			t.Fatal("pending must not launder a body the contract refused")
		}
	})
}

// TestAResponseContractSeesExactIntegersTheFloatPathLoses runs the collision
// through the REAL server, client and reader.
//
// 9007199254740993 is the first integer float64 cannot hold: it rounds to
// ...992. A validator decoding through the historical float64 projection would
// accept either value for a contract naming one, and the test that proved the
// contract "worked" would have proved nothing.
func TestAResponseContractSeesExactIntegersTheFloatPathLoses(t *testing.T) {
	const declared = "9007199254740993"
	const collides = "9007199254740992"

	for _, tc := range []struct {
		body   string
		accept bool
	}{
		{declared, true},
		// Same float64. Different integer.
		{collides, false},
		// The same value written another way is the SAME number.
		{"90071992547409930e-1", true},
	} {
		e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":` + tc.body + `}`))
		})
		op := withContract(t, pkg, "probe.issue.comment", 201, spec.ResponseSchema{
			Type: "object", Required: []string{"id"},
			Properties: map[string]spec.ResponseSchema{"id": {
				Type: "integer", Enum: []json.RawMessage{json.RawMessage(declared)},
			}},
		})
		res, err := e.Call(context.Background(), pkg, op, commentArgs(), creds())
		done()
		if err != nil {
			t.Fatalf("%s: call: %v", tc.body, err)
		}
		if accepted := res.Err == nil; accepted != tc.accept {
			t.Errorf("body id=%s accepted=%v, want %v (err=%v)", tc.body, accepted, tc.accept, res.Err)
		}
	}
}
