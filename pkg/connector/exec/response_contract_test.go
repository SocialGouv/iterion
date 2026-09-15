package exec_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
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
			// The one 2xx that is not a proof: readResponse makes 202 pending
			// by default precisely because the work has not happened yet.
			"a 202: accepted is not performed, so a key still licenses the repeat",
			&exec.Error{Class: spec.ErrUpstream, Status: 202, Ambiguous: true},
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

// TestAPageBreakingItsContractStopsTheWalkInsteadOfContributing.
//
// A paginated walk is where a lenient reading costs the most: the refused page
// would otherwise add its rows to a collection the caller then reads as whole.
// So the walk ends, reports `complete=false`, and the refused page contributes
// NOTHING.
//
// The pages already gathered still come back — that is CallPaged's documented
// posture for every mid-walk failure, so a caller that logs both can see how
// far it got, and `complete=false` is the signal that says the collection is
// partial. What must not happen is the refused page's own rows appearing in it.
func TestAPageBreakingItsContractStopsTheWalkInsteadOfContributing(t *testing.T) {
	pages := 0
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		pages++
		w.Header().Set("Content-Type", "application/json")
		if pages == 1 {
			// A FULL page, so the walk has a reason to ask for a second.
			_, _ = w.Write([]byte(`[{"id":1},{"id":2}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":"not-an-integer"}]`))
	})
	defer done()

	op := withContract(t, pkg, "probe.issue.list", 200, spec.ResponseSchema{
		Type: "array",
		Items: &spec.ResponseSchema{
			Type: "object", Required: []string{"id"},
			Properties: map[string]spec.ResponseSchema{"id": {Type: "integer"}},
		},
	})
	items, complete, last, err := e.CallPaged(context.Background(), pkg, op,
		map[string]any{"owner": "acme", "repo": "widgets"}, creds())
	if err == nil && last.Err == nil {
		t.Fatal("a page that breaks its contract must fail the walk")
	}
	if complete {
		t.Error("a walk that stopped on a refused page is not a complete collection")
	}
	if len(items) != 2 {
		t.Errorf("items = %v, want the two rows page one legitimately delivered", items)
	}
	// The count above is what proves non-contribution today, since CallPaged
	// returns before it collects a refused page's rows. This names the row
	// itself so a walk that later kept the page WHILE dropping a legitimate one
	// cannot satisfy the count and pass.
	for _, item := range items {
		row, _ := item.(map[string]any)
		if id, _ := row["id"].(string); id == "not-an-integer" {
			t.Errorf("the refused page contributed a row anyway: %v", items)
		}
	}
	if pages != 2 {
		t.Errorf("pages fetched = %d, want 2: page one valid, page two refused and the walk over", pages)
	}
}

// TestAResponseContractCertifiesTheNumberTheWorkflowRECEIVES runs the collision
// through the REAL server, client and reader.
//
// 9007199254740992 is the largest integer float64 holds exactly; ...993 shares
// its float64. A validator decoding through the delivered projection would
// accept either for a contract naming one — so the exact decode is what makes
// the contract discriminate at all.
//
// The second half is the part a test of the validator alone cannot see: what
// the contract vouched for has to be what the node hands on. A contract naming
// a value the float64 projection cannot carry is refused when the package is
// admitted, so the only contracts that reach a call are ones whose certified
// value and delivered value are the same number.
func TestAResponseContractCertifiesTheNumberTheWorkflowRECEIVES(t *testing.T) {
	const declared = "9007199254740992"
	const collides = "9007199254740993"

	for _, tc := range []struct {
		body   string
		accept bool
	}{
		{declared, true},
		// Same float64. Different integer.
		{collides, false},
		// The same value written another way is the SAME number.
		{"90071992547409920e-1", true},
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
		if !tc.accept {
			continue
		}
		// What the contract certified is what the node hands on.
		row, _ := res.Data.(map[string]any)
		got, ok := row["id"].(float64)
		if !ok {
			t.Fatalf("body id=%s: Data.id = %#v, want the decoded number", tc.body, row["id"])
		}
		if strconv.FormatFloat(got, 'f', -1, 64) != declared {
			t.Errorf("body id=%s: the contract certified %s and the workflow receives %s",
				tc.body, declared, strconv.FormatFloat(got, 'f', -1, 64))
		}
	}
}

// TestAVendorReportingItsOwnFailureKeepsThatDiagnosisOverTheContract is the
// only witness reachable ONLY by the order in which readResponse judges.
//
// The package declares both an outcome policy and a contract on the same
// status, and the vendor answers 201 with the Slack shape: a body that reports
// its own failure AND satisfies no contract, since the contract describes the
// SUCCESS shape. Judging the contract first turns every such business failure
// into an ambiguous mutation, parked instead of branched on — a total change of
// meaning that no other test can see, because with either order alone the call
// still fails.
func TestAVendorReportingItsOwnFailureKeepsThatDiagnosisOverTheContract(t *testing.T) {
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
	})
	defer done()

	op := withContract(t, pkg, "probe.issue.comment", 201, spec.ResponseSchema{
		Type: "object", Required: []string{"id"},
		Properties: map[string]spec.ResponseSchema{"id": {Type: "integer"}},
	})
	op.Outcome = &spec.OutcomePolicy{
		SuccessWhen:    "body.ok == true",
		ErrorCodeField: "error",
		ErrorCodeMap:   map[string]spec.ErrorClass{"ratelimited": spec.ErrRateLimited},
	}
	// Keyed on purpose: the retry this case keeps is the one a key licenses,
	// and the contract path is precisely what would take it away.
	op.IdempotencyKeyParam = "body"
	args := commentArgs()

	res, err := e.Call(context.Background(), pkg, op, args, creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Err == nil {
		t.Fatal("a body reporting ok:false is a failure, contract or not")
	}
	if res.Err.Class != spec.ErrRateLimited {
		t.Errorf("Class = %q, want %q — the package authored this diagnosis and must keep it",
			res.Err.Class, spec.ErrRateLimited)
	}
	if res.Err.Ambiguous {
		t.Error("the vendor said it did NOT act: that is a decided failure, not an undecided mutation")
	}
	if res.Data == nil {
		t.Error("the body carrying the vendor's own error code must stay readable")
	}
	if !res.Err.Retryable(op, args) {
		t.Error("a business failure under an idempotency key keeps the retry it has always had")
	}
}
