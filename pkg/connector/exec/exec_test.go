package exec_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// Every test here runs against a REAL HTTP server. That is deliberate: this
// package's whole job is what actually reaches a vendor, so a stub that
// records intentions would certify the intention and not the bytes. The
// server is the oracle — it asserts on the request it received.

// probe is a package shaped like the pilot connectors: a token in a header
// behind a prefix, an issue resource with a JSON body, and a Slack-shaped
// sibling that reports failure inside a 200.
func probe(base string) *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion,
			ID:            "probe",
			Version:       "1.0.0",
			BaseURL:       spec.BaseURL{Default: base, PathPrefix: "/api/v1"},
			Auth: []spec.AuthScheme{{
				ID: "token", Kind: spec.AuthAPIKey, In: "header",
				Name: "Authorization", ValuePrefix: "token ",
			}},
			Maturity: spec.MaturityExperimental,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "probe", Domain: "issue",
			Operations: []spec.Operation{
				{
					ID: "probe.issue.get", Resource: "issue", Verb: "get",
					HTTP:   spec.HTTPBinding{Method: "GET", Path: "/repos/{owner}/{repo}/issues/{index}"},
					Effect: spec.EffectRead, Deterministic: true,
					Params: []spec.Param{
						{Key: "owner", Name: "owner", In: spec.InPath, Type: "string", Required: true},
						{Key: "repo", Name: "repo", In: spec.InPath, Type: "string", Required: true},
						{Key: "index", Name: "index", In: spec.InPath, Type: "integer", Required: true},
					},
					Results: []spec.ResultCase{{Status: 200}},
				},
				{
					ID: "probe.issue.list", Resource: "issue", Verb: "list",
					HTTP:   spec.HTTPBinding{Method: "GET", Path: "/repos/{owner}/{repo}/issues"},
					Effect: spec.EffectRead, Deterministic: true,
					Params: []spec.Param{
						{Key: "owner", Name: "owner", In: spec.InPath, Type: "string", Required: true},
						{Key: "repo", Name: "repo", In: spec.InPath, Type: "string", Required: true},
						{Key: "state", Name: "state", In: spec.InQuery, Type: "string", Enum: []string{"open", "closed"}},
						{Key: "labels", Name: "labels", In: spec.InQuery, Type: "array", Items: "string", Style: spec.StyleForm, Explode: boolPtr(false)},
						{Key: "page", Name: "page", In: spec.InQuery, Type: "integer"},
						{Key: "limit", Name: "limit", In: spec.InQuery, Type: "integer"},
					},
					// Array: true because every handler in this file answers a
					// bare `[...]` — the declaration has to match what the
					// fixture actually serves, or the package validates a
					// shape no test exercises.
					Results: []spec.ResultCase{{Status: 200, Array: true}},
					Pagination: &spec.Pagination{
						Style: spec.PageNumber, PageParam: "page", SizeParam: "limit",
						DefaultSize: 2, MaxPages: 3,
					},
				},
				{
					ID: "probe.issue.comment", Resource: "issue", Verb: "comment",
					HTTP: spec.HTTPBinding{
						Method: "POST", Path: "/repos/{owner}/{repo}/issues/{index}/comments",
						RequestBody: spec.BodyJSON,
					},
					Effect: spec.EffectCreate, Deterministic: true,
					Params: []spec.Param{
						{Key: "owner", Name: "owner", In: spec.InPath, Type: "string", Required: true},
						{Key: "repo", Name: "repo", In: spec.InPath, Type: "string", Required: true},
						{Key: "index", Name: "index", In: spec.InPath, Type: "integer", Required: true},
						{Key: "body", Name: "body", In: spec.InBody, Type: "string", Required: true},
					},
					Results: []spec.ResultCase{{Status: 201}, {Status: 202, Pending: true}},
					Errors:  []spec.ErrorSpec{{Status: 423, Class: spec.ErrConflict}},
				},
			},
		}},
	}
}

func boolPtr(b bool) *bool { return &b }

func run(t *testing.T, h http.HandlerFunc) (*exec.Executor, *spec.Package, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	// The real executor takes httpdial.SafeClient, which refuses a loopback
	// address on purpose. A test server IS loopback, so the client is the
	// plain one here — what is under test is the request and the reading of
	// the answer, and the guard has its own tests in pkg/secure/httpdial.
	client := srv.Client()
	// httpdial.SafeClient — the client the executor is REQUIRED to run on —
	// refuses to follow redirects (ErrUseLastResponse), because the guarded
	// dialer pins the host it resolved and chasing a 3xx would hand the
	// destination back to the vendor. A test client that quietly followed
	// them would never present the executor with a 3xx, and so could not
	// catch the reader treating one as success. The stub carries every term
	// of the real producer except the loopback guard, which is what makes a
	// test server addressable at all.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	e := &exec.Executor{Client: client, UserAgent: "iterion-test"}
	return e, probe(srv.URL), srv.Close
}

func creds() exec.Credential {
	return exec.Credential{SchemeID: "token", Value: "s3cret"}
}

func opOf(t *testing.T, pkg *spec.Package, id string) spec.Operation {
	t.Helper()
	op, ok := pkg.Operation(id)
	if !ok {
		t.Fatalf("operation %q missing from the fixture", id)
	}
	return op
}

// TestTheRequestIsWhatWasDeclared is the load-bearing test: path templating,
// the credential's prefix, the query serialization and the JSON body, all
// asserted on the bytes the server actually received.
func TestTheRequestIsWhatWasDeclared(t *testing.T) {
	var got *http.Request
	var body []byte
	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = readAll(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id": 7}`))
	})
	defer done()

	res, err := e.Call(context.Background(), pkg, opOf(t, pkg, "probe.issue.comment"),
		map[string]any{"owner": "acme", "repo": "widgets", "index": 42, "body": "hello"}, creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.OK() {
		t.Fatalf("result = %v", res.Err)
	}

	if got.URL.Path != "/api/v1/repos/acme/widgets/issues/42/comments" {
		t.Errorf("path = %q — the prefix and the templating must both apply", got.URL.Path)
	}
	// The prefix comes from the PACKAGE, never from the stored value: this is
	// the difference between a working call and a 401 that reads like a bad
	// credential.
	if h := got.Header.Get("Authorization"); h != "token s3cret" {
		t.Errorf("Authorization = %q, want %q", h, "token s3cret")
	}
	if ct := got.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, body)
	}
	// A path parameter must NOT leak into the body, and a body member must
	// not leak into the query: the flat params map is split by declaration.
	if len(payload) != 1 || payload["body"] != "hello" {
		t.Errorf("body = %v, want only the declared body member", payload)
	}
	if res.Status != 201 || res.Pending {
		t.Errorf("result = %d pending=%v, want 201 not pending", res.Status, res.Pending)
	}
}

// TestPathSegmentsAreEscapedIndividually pins a value that contains a slash.
// Escaping the whole path at once would let an issue named "a/b" address a
// different resource than the one the workflow named.
func TestPathSegmentsAreEscapedIndividually(t *testing.T) {
	var path string
	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is already decoded; EscapedPath is what arrived.
		path = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{}`))
	})
	defer done()

	_, err := e.Call(context.Background(), pkg, opOf(t, pkg, "probe.issue.get"),
		map[string]any{"owner": "acme", "repo": "a/b", "index": 1}, creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(path, "a%2Fb") {
		t.Errorf("escaped path = %q, want the slash inside the value escaped", path)
	}
}

// TestQuerySerializationReachesTheWire pins the difference between `a,b` and
// two repeated pairs — only one of which the vendor parses.
func TestQuerySerializationReachesTheWire(t *testing.T) {
	var raw string
	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	})
	defer done()

	op := opOf(t, pkg, "probe.issue.list")
	_, err := e.Call(context.Background(), pkg, op, map[string]any{
		"owner": "acme", "repo": "widgets",
		"state":  "open",
		"labels": []any{"bug", "p1"},
	}, creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(raw, "labels=bug%2Cp1") {
		t.Errorf("query = %q — explode:false must comma-join into ONE pair", raw)
	}
	if !strings.Contains(raw, "state=open") {
		t.Errorf("query = %q, want state=open", raw)
	}
}

// TestSerializationIsHonouredAtEVERYLocation, not only in the query.
//
// Three shapes reached the wire ignoring what the package declared, each
// producing a well-formed request the vendor cannot parse — the worst kind,
// because it looks correct in a log.
func TestSerializationIsHonouredAtEveryLocation(t *testing.T) {
	t.Run("a path array joins on commas instead of becoming JSON", func(t *testing.T) {
		var path string
		e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			_, _ = w.Write([]byte(`{}`))
		})
		defer done()

		// A path parameter's OpenAPI default style is `simple`, and a path
		// segment can hold nothing else. The templating bypassed styles
		// entirely, so this arrived as escaped JSON.
		op := opOf(t, pkg, "probe.issue.get")
		for i := range op.Params {
			if op.Params[i].Key == "index" {
				op.Params[i].Type = "array"
				op.Params[i].Items = "string"
			}
		}
		_, err := e.Call(context.Background(), pkg, op,
			map[string]any{"owner": "acme", "repo": "widgets", "index": []any{"a", "b"}}, creds())
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !strings.HasSuffix(path, "/a,b") {
			t.Errorf("path = %q, want it to end in the comma-joined segment `a,b`", path)
		}
	})

	t.Run("deepObject expands into query members", func(t *testing.T) {
		var raw string
		e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
			raw = r.URL.RawQuery
			_, _ = w.Write([]byte(`[]`))
		})
		defer done()

		op := opOf(t, pkg, "probe.issue.list")
		op.Params = append(op.Params, spec.Param{
			Key: "filter", Name: "filter", In: spec.InQuery, Type: "object",
			Style: spec.StyleDeepObject,
		})
		_, err := e.Call(context.Background(), pkg, op, map[string]any{
			"owner": "acme", "repo": "widgets",
			"filter": map[string]any{"name": "Ada", "age": 36},
		}, creds())
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		// The defect sent `filter={"name":"Ada"}` — a JSON document in a query
		// value, which no deepObject endpoint parses.
		if !strings.Contains(raw, "filter%5Bname%5D=Ada") {
			t.Errorf("query = %q, want filter[name]=Ada", raw)
		}
		if strings.Contains(raw, "%7B") {
			t.Errorf("query = %q — a JSON object reached the wire", raw)
		}
	})

	t.Run("an object without deepObject is refused, not guessed", func(t *testing.T) {
		e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("the vendor must not be reached")
			_, _ = w.Write([]byte(`[]`))
		})
		defer done()

		op := opOf(t, pkg, "probe.issue.list")
		op.Params = append(op.Params, spec.Param{
			Key: "filter", Name: "filter", In: spec.InQuery, Type: "object",
			Style: spec.StyleForm,
		})
		res, err := e.Call(context.Background(), pkg, op, map[string]any{
			"owner": "acme", "repo": "widgets", "filter": map[string]any{"name": "Ada"},
		}, creds())
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if res.OK() {
			t.Fatal("an object under a style that cannot express one must be refused")
		}
	})
}

// TestAWholeBodyParameterISTheBody, not a member of it.
//
// A vendor whose endpoint takes a bare array or a scalar has no body MEMBERS
// to flatten, and the generator used to invent one called `body`. The builder
// then wrapped the value in an object, so an endpoint documented as taking
// `[1,2]` received `{"body":[1,2]}` — which the vendor rejects, or accepts as
// an empty request.
func TestAWholeBodyParameterIsTheBody(t *testing.T) {
	var got string
	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := readAll(r)
		got = string(b)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{}`))
	})
	defer done()

	op := opOf(t, pkg, "probe.issue.comment")
	op.Params = []spec.Param{
		{Key: "owner", Name: "owner", In: spec.InPath, Type: "string", Required: true},
		{Key: "repo", Name: "repo", In: spec.InPath, Type: "string", Required: true},
		{Key: "index", Name: "index", In: spec.InPath, Type: "integer", Required: true},
		{Key: "body", Name: "body", In: spec.InBody, Type: "array", Items: "integer", Required: true, WholeBody: true},
	}
	_, err := e.Call(context.Background(), pkg, op, map[string]any{
		"owner": "acme", "repo": "widgets", "index": 1,
		"body": []any{1, 2},
	}, creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got != "[1,2]" {
		t.Errorf("the vendor received %s, want the value itself", got)
	}
}

// TestABodyCannotBeBothShapes: a value and a set of members have no combined
// encoding, so the builder would have to pick — and either choice silently
// discards the rest.
func TestABodyCannotBeBothShapes(t *testing.T) {
	pkg := probe("https://probe.example")
	op, _ := pkg.Operation("probe.issue.comment")
	op.Params = append(op.Params, spec.Param{
		Key: "whole", Name: "whole", In: spec.InBody, Type: "array", WholeBody: true,
	})
	pkg.Ops[0].Operations[2] = op
	if err := pkg.Validate(); err == nil {
		t.Fatal("a body that is both a value and a set of members must be refused")
	} else if !strings.Contains(err.Error(), "one shape or the other") {
		t.Errorf("refusal = %v, want it to name the incoherence", err)
	}
}

// TestAFileParameterIsRefusedAtTheCALLSite. Generation refuses these now, so
// reaching the builder means an older package or a hand-written one. It used
// to write the argument as a TEXT field — a part holding a pathname or base64
// text, with no filename and no content — which the vendor rejects, or turns
// into a corrupt attachment.
func TestAFileParameterIsRefusedAtTheCallSite(t *testing.T) {
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the vendor must not be reached")
		w.WriteHeader(201)
	})
	defer done()

	op := opOf(t, pkg, "probe.issue.comment")
	op.HTTP.RequestBody = spec.BodyMultipart
	op.Params = []spec.Param{
		{Key: "owner", Name: "owner", In: spec.InPath, Type: "string", Required: true},
		{Key: "repo", Name: "repo", In: spec.InPath, Type: "string", Required: true},
		{Key: "index", Name: "index", In: spec.InPath, Type: "integer", Required: true},
		{Key: "attachment", Name: "attachment", In: spec.InBody, Type: "file", Required: true},
	}
	res, err := e.Call(context.Background(), pkg, op, map[string]any{
		"owner": "acme", "repo": "widgets", "index": 1, "attachment": "/etc/passwd",
	}, creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.OK() {
		t.Fatal("a file parameter must be refused, not written as a text field")
	}
	if !strings.Contains(res.Err.Error(), "file part") {
		t.Errorf("refusal = %v, want it to name what iterion cannot send", res.Err)
	}
}

// TestTheVendorsOwnMediaTypeIsSENT. `application/json-patch+json` is JSON on
// the wire, so the ENCODING is right — but it is not `application/json`, and a
// vendor declaring one refuses the other at the header. Sending the canonical
// type made an operation the description says exists, that iterion encodes
// correctly, fail before its body was read.
func TestTheVendorsOwnMediaTypeIsSent(t *testing.T) {
	var got string
	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Content-Type")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{}`))
	})
	defer done()

	op := opOf(t, pkg, "probe.issue.comment")
	op.HTTP.ContentType = "application/json-patch+json"
	_, err := e.Call(context.Background(), pkg, op, fullParams("probe.issue.comment"), creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got != "application/json-patch+json" {
		t.Errorf("Content-Type = %q, want the vendor's own media type", got)
	}

	// The falsifier: with none declared, the canonical type is still sent.
	op.HTTP.ContentType = ""
	if _, err := e.Call(context.Background(), pkg, op, fullParams("probe.issue.comment"), creds()); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got != "application/json" {
		t.Errorf("Content-Type = %q, want the canonical type when none is declared", got)
	}
}

// TestLocalRefusalsNeverReachTheVendor covers everything the package can
// decide on its own. Each case would otherwise cost a round trip, a rate
// limit slot, and — for the unknown key — a call that succeeds while doing
// something other than what the author wrote.
func TestLocalRefusalsNeverReachTheVendor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		op      string
		params  map[string]any
		cred    exec.Credential
		wantMsg string
	}{
		{
			name: "an argument the operation does not declare",
			op:   "probe.issue.get",
			// A misspelling. Dropping it silently is the failure.
			params:  map[string]any{"owner": "acme", "repo": "w", "index": 1, "indx": 2},
			cred:    creds(),
			wantMsg: `"indx"`,
		},
		{
			name:    "a required argument that is missing",
			op:      "probe.issue.get",
			params:  map[string]any{"owner": "acme", "repo": "w"},
			cred:    creds(),
			wantMsg: `"index"`,
		},
		{
			name:    "a value outside a declared enum",
			op:      "probe.issue.list",
			params:  map[string]any{"owner": "a", "repo": "w", "state": "sideways"},
			cred:    creds(),
			wantMsg: "sideways",
		},
		{
			name:    "a connection holding no value",
			op:      "probe.issue.get",
			params:  map[string]any{"owner": "a", "repo": "w", "index": 1},
			cred:    exec.Credential{SchemeID: "token"},
			wantMsg: "no key",
		},
		{
			name:    "a scheme the connector does not declare",
			op:      "probe.issue.get",
			params:  map[string]any{"owner": "a", "repo": "w", "index": 1},
			cred:    exec.Credential{SchemeID: "ghost", Value: "x"},
			wantMsg: "ghost",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				_, _ = w.Write([]byte(`{}`))
			})
			defer done()

			res, err := e.Call(context.Background(), pkg, opOf(t, pkg, tc.op), tc.params, tc.cred)
			if err != nil {
				t.Fatalf("a local refusal must be a typed RESULT, not an error: %v", err)
			}
			if res.OK() {
				t.Fatal("the call must be refused")
			}
			if reached {
				t.Error("the request reached the vendor — a local refusal must never leave")
			}
			if !strings.Contains(res.Err.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to name %q", res.Err.Message, tc.wantMsg)
			}
		})
	}
}

// TestErrorsAreTyped pins the classification a `.bot` branches on, including
// the package's own declaration winning over the status range.
func TestErrorsAreTyped(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		op        string
		wantClass spec.ErrorClass
		retryable bool
	}{
		{"not found", 404, "probe.issue.get", spec.ErrNotFound, false},
		{"unauthorized", 401, "probe.issue.get", spec.ErrUnauthorized, false},
		{"forbidden", 403, "probe.issue.get", spec.ErrForbidden, false},
		{"upstream", 503, "probe.issue.get", spec.ErrUpstream, true},
		// 423 is Locked, which the range would call bad_request; the package
		// declares it a conflict, and the declaration wins.
		{"a status the package typed itself", 423, "probe.issue.comment", spec.ErrConflict, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message": "nope"}`))
			})
			defer done()

			op := opOf(t, pkg, tc.op)
			res, err := e.Call(context.Background(), pkg, op, fullParams(tc.op), creds())
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if res.OK() {
				t.Fatal("want a failure")
			}
			if res.Err.Class != tc.wantClass {
				t.Errorf("class = %q, want %q", res.Err.Class, tc.wantClass)
			}
			if got := res.Err.Retryable(op, fullParams(tc.op)); got != tc.retryable {
				t.Errorf("retryable = %v, want %v", got, tc.retryable)
			}
		})
	}
}

// TestRateLimitCarriesItsDelay pins that a 429 keeps what the vendor said,
// and stays retryable even for a mutation — a refused request performed
// nothing, so repeating it cannot duplicate an effect.
func TestRateLimitCarriesItsDelay(t *testing.T) {
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	})
	defer done()

	op := opOf(t, pkg, "probe.issue.comment")
	res, err := e.Call(context.Background(), pkg, op, fullParams("probe.issue.comment"), creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Err == nil || res.Err.Class != spec.ErrRateLimited {
		t.Fatalf("err = %v, want rate_limited", res.Err)
	}
	if res.Err.RetryAfter != 30*time.Second {
		t.Errorf("retry-after = %v, want 30s", res.Err.RetryAfter)
	}
	if !res.Err.Retryable(op, fullParams("probe.issue.comment")) {
		t.Error("a 429 refused the request, so even a mutation may be repeated")
	}
}

// TestAMutationWithNoAnswerIsUnknown is the honesty rule. The request was
// sent, no answer came back, the operation creates something and the vendor
// offers no idempotency key — so iterion says it cannot tell, rather than
// reporting a failure a caller would retry into a duplicate.
func TestAMutationWithNoAnswerIsUnknown(t *testing.T) {
	// A handler that hijacks and closes: the request is delivered, the answer
	// never is. This is the real shape of the ambiguity, not a simulated one.
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("the test server must support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close()
	})
	defer done()

	mutate := opOf(t, pkg, "probe.issue.comment")
	res, err := e.Call(context.Background(), pkg, mutate, fullParams("probe.issue.comment"), creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Err == nil || res.Err.Class != spec.ErrUnknownOutcome {
		t.Fatalf("err = %v, want unknown_outcome", res.Err)
	}
	if res.Err.Retryable(mutate, fullParams("probe.issue.comment")) {
		t.Error("an unknown outcome must NEVER be retried automatically — that is the whole point of the class")
	}

	// The same failure on a READ is an ordinary transport error: nothing was
	// changed, so repeating it is safe.
	read := opOf(t, pkg, "probe.issue.get")
	res, err = e.Call(context.Background(), pkg, read, fullParams("probe.issue.get"), creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Err == nil || res.Err.Class != spec.ErrTransport {
		t.Fatalf("err = %v, want transport for a read", res.Err)
	}
	if !res.Err.Retryable(read, fullParams("probe.issue.get")) {
		t.Error("a read that got no answer may be repeated")
	}
}

// TestAMutationWithAnIdempotencyKeyIsRetryable pins the other half: the rule
// is about the call being safe to repeat, not about mutations being
// untouchable.
//
// The distinction that matters is between a key DECLARED and a key SENT. A
// vendor deduplicates on the value it received; a parameter the package
// mentions and the caller omitted protects nothing, and treating it as
// protection licensed exactly the duplicate the rule exists to forbid.
func TestAMutationWithAnIdempotencyKeyIsRetryable(t *testing.T) {
	pkg := probe("http://example.invalid")
	op := opOf(t, pkg, "probe.issue.comment")
	op.IdempotencyKeyParam = "body"
	sent := map[string]any{"body": "abc-123"}

	e := &exec.Error{Class: spec.ErrUpstream, Status: 503}
	if !e.Retryable(op, sent) {
		t.Error("a mutation carrying an idempotency key may be repeated")
	}
	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{"the key was never sent", map[string]any{}},
		{"the key was sent empty", map[string]any{"body": ""}},
		{"the key was sent blank", map[string]any{"body": "   "}},
		{"the key was sent null", map[string]any{"body": nil}},
	} {
		if e.Retryable(op, tc.params) {
			t.Errorf("%s: a declared key that did not reach the vendor deduplicates nothing — the mutation must not be repeated", tc.name)
		}
	}
	op.IdempotencyKeyParam = ""
	if e.Retryable(op, sent) {
		t.Error("without an idempotency key, a mutation must not be repeated on a 5xx")
	}
}

// TestALostAnswerIsAmbiguousUnlessTheKeyWasSENT is the transport-side twin of
// the test above. The two decisions — "may this be repeated?" and "is this
// outcome undecided?" — are the same promise read from opposite ends, so they
// share one predicate and must never disagree.
func TestALostAnswerIsAmbiguousUnlessTheKeyWasSENT(t *testing.T) {
	// A server that takes the request and hangs up without answering.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	e := &exec.Executor{Client: srv.Client(), UserAgent: "iterion-test"}
	pkg := probe(srv.URL)
	op := opOf(t, pkg, "probe.issue.comment")
	op.IdempotencyKeyParam = "body"
	args := map[string]any{"owner": "acme", "repo": "widgets", "index": 1, "body": "hello"}

	res, err := e.Call(context.Background(), pkg, op, args, creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Err == nil {
		t.Fatal("a lost answer must produce an error")
	}
	if res.Err.Class == spec.ErrUnknownOutcome {
		t.Error("the key WAS sent, so a repeat is safe — this is an ordinary transport failure, not an undecided one")
	}

	// Same operation, same lost answer, key omitted: now it is undecided.
	delete(args, "body")
	op.Params = append([]spec.Param{}, op.Params...)
	for i := range op.Params {
		if op.Params[i].Key == "body" {
			op.Params[i].Required = false
		}
	}
	res, err = e.Call(context.Background(), pkg, op, args, creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Err == nil || res.Err.Class != spec.ErrUnknownOutcome {
		t.Errorf("class = %v, want %q — a mutation with no key sent and no answer back is undecided", res.Err, spec.ErrUnknownOutcome)
	}
}

// TestPendingIsNotSuccess pins the 202 distinction: a workflow that reads
// "accepted" as "done" acts on work that has not happened.
// TestAnUnknownGrantDoesNotDefeatTheSchemeCONJUNCTION.
//
// An unstated grant must not be enforced as "no scopes" — most providers never
// enumerate what a PAT carries, so that would refuse every token-backed
// connection. But the escape used to accept ANY requirement holding a term
// with a matching scheme, which defeats the conjunction rule outright: a
// requirement of `A AND B` contains a term naming A, so a connection holding
// only A was authorised for an operation needing both.
func TestAnUnknownGrantDoesNotDefeatTheSchemeConjunction(t *testing.T) {
	reached := false
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{}`))
	})
	defer done()

	op := opOf(t, pkg, "probe.issue.get")
	// The operation needs BOTH schemes at once.
	op.Security = []spec.SecurityRequirement{{Terms: []spec.SecurityTerm{
		{SchemeID: "token"},
		{SchemeID: "second"},
	}}}

	// A credential holding only "token", with an UNKNOWN grant.
	res, err := e.Call(context.Background(), pkg, op, fullParams("probe.issue.get"),
		exec.Credential{SchemeID: "token", Value: "s3cret"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.OK() {
		t.Fatal("a connection holding one of two required schemes must be refused")
	}
	if reached {
		t.Error("the vendor was reached by a call iterion should have refused locally")
	}

	// The falsifier: with a single-scheme requirement, the same unknown grant
	// still works — the escape it exists for is intact.
	op.Security = []spec.SecurityRequirement{{Terms: []spec.SecurityTerm{
		{SchemeID: "token", Scopes: []string{"read:issue"}},
	}}}
	res, err = e.Call(context.Background(), pkg, op, fullParams("probe.issue.get"),
		exec.Credential{SchemeID: "token", Value: "s3cret"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.OK() {
		t.Errorf("an unstated grant must not refuse a single-scheme requirement: %v", res.Err)
	}
}

// TestACredentialNeverAppearsInAnError.
//
// A transport failure's text is Go's *url.Error, which prints the FULL request
// URL. An auth scheme placed `in: query` puts the credential there, so the
// token appeared verbatim in an error that travels to the run's events, the
// tool hooks and error tracking — none of which can redact what they cannot
// recognise. Here is the only place the secret bytes are still known.
func TestACredentialNeverAppearsInAnError(t *testing.T) {
	const token = "s3cret-token-value"

	// A server that hangs up, so the failure is a transport error carrying
	// the URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	pkg := probe(srv.URL)
	// The leaking shape: the credential travels in the QUERY STRING.
	pkg.Connector.Auth = []spec.AuthScheme{{
		ID: "token", Kind: spec.AuthAPIKey, In: "query", Name: "access_token",
	}}
	e := &exec.Executor{Client: srv.Client(), UserAgent: "iterion-test"}

	res, err := e.Call(context.Background(), pkg, opOf(t, pkg, "probe.issue.get"),
		fullParams("probe.issue.get"), exec.Credential{SchemeID: "token", Value: token})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Err == nil {
		t.Fatal("a lost connection must produce an error")
	}
	text := res.Err.Error()
	if strings.Contains(text, token) {
		t.Errorf("the credential appears verbatim in an error that reaches the run's events: %s", text)
	}
	// The message must still be USEFUL: redaction that erased the diagnosis
	// would trade one silent failure for another.
	if !strings.Contains(text, "redacted") {
		t.Errorf("the redaction must be visible, so a reader knows something was removed: %s", text)
	}
}

// TestASecretParameterIsNotEchoedByARefusal covers the other direction: the
// value being refused is itself the secret. `Secret: true` is the package
// saying "this must never be logged", and a local refusal is still a log.
func TestASecretParameterIsNotEchoedByARefusal(t *testing.T) {
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	defer done()

	op := opOf(t, pkg, "probe.issue.list")
	for i := range op.Params {
		if op.Params[i].Key == "state" {
			op.Params[i].Secret = true
		}
	}
	res, err := e.Call(context.Background(), pkg, op,
		map[string]any{"owner": "acme", "repo": "widgets", "state": "hunter2"}, creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Err == nil {
		t.Fatal("a value outside the declared enum must be refused")
	}
	if strings.Contains(res.Err.Error(), "hunter2") {
		t.Errorf("a secret parameter's value must not be echoed by its own refusal: %s", res.Err.Error())
	}
	// The refusal still has to say WHICH parameter and what was allowed.
	if !strings.Contains(res.Err.Error(), "state") {
		t.Errorf("the refusal must still name the parameter: %s", res.Err.Error())
	}
}

// TestARedirectIsNotAnAnswer.
//
// iterion's client deliberately does not follow redirects: the guarded dialer
// pins the host it resolved, and chasing a 3xx would hand the destination back
// to the vendor. So a 3xx means the call never reached the resource — but the
// reader only refused statuses ≥400, so a 302 came back OK with an empty body
// and a workflow branched on an answer nobody gave.
func TestARedirectIsNotAnAnswer(t *testing.T) {
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://elsewhere.example/moved")
		w.WriteHeader(http.StatusFound)
	})
	defer done()

	res, err := e.Call(context.Background(), pkg, opOf(t, pkg, "probe.issue.get"),
		fullParams("probe.issue.get"), creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.OK() {
		t.Fatal("a 302 the client will not follow is not a successful call")
	}
	// The diagnostic points at the likely cause, since a redirect on a
	// connector call almost always means a stale base_url.
	if !strings.Contains(res.Err.Error(), "base_url") {
		t.Errorf("error = %v, want it to name the stale base_url as the likely cause", res.Err)
	}
}

// TestAnUndeclaredAcceptedIsStillPending.
//
// A generator only sees what a vendor wrote down, and most do not document
// their 202s. Keying "pending" on the DECLARATION therefore made the common
// case — an undeclared 202 — read as completed work. 202 means accepted, not
// done, whoever remembered to say so.
func TestAnUndeclaredAcceptedIsStillPending(t *testing.T) {
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job": "q-1"}`))
	})
	defer done()

	// probe.issue.get declares only a 200 — the 202 is undeclared.
	res, err := e.Call(context.Background(), pkg, opOf(t, pkg, "probe.issue.get"),
		fullParams("probe.issue.get"), creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.Pending {
		t.Error("an undeclared 202 must still read as pending — the work has not happened")
	}

	// A package may still override it: a vendor that misuses 202 as plain
	// success declares the case and says so.
	op := opOf(t, pkg, "probe.issue.get")
	op.Results = []spec.ResultCase{{Status: 200}, {Status: 202, Pending: false}}
	res, err = e.Call(context.Background(), pkg, op, fullParams("probe.issue.get"), creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Pending {
		t.Error("an explicit declaration must win over the default")
	}
}

func TestPendingIsNotSuccess(t *testing.T) {
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"queued": true}`))
	})
	defer done()

	res, err := e.Call(context.Background(), pkg, opOf(t, pkg, "probe.issue.comment"), fullParams("probe.issue.comment"), creds())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.OK() {
		t.Fatalf("a 202 is not a failure: %v", res.Err)
	}
	if !res.Pending {
		t.Error("a 202 the package declared pending must surface as pending")
	}
}

// TestPaginationWalksAndSaysWhenItStopped covers both halves: the items are
// accumulated across pages, and a walk that hit its ceiling reports
// incomplete rather than looking finished — a silent truncation is how a
// workflow concludes an issue does not exist because it was on page 21.
func TestPaginationWalksAndSaysWhenItStopped(t *testing.T) {
	var pagesServed []string
	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		pagesServed = append(pagesServed, r.URL.Query().Get("page"))
		// Always a full page, so the walk can only end at MaxPages (3).
		_, _ = w.Write([]byte(`[{"n": 1}, {"n": 2}]`))
	})
	defer done()

	items, complete, _, err := e.CallPaged(context.Background(), pkg, opOf(t, pkg, "probe.issue.list"),
		map[string]any{"owner": "acme", "repo": "widgets"}, creds())
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if complete {
		t.Error("the walk stopped at its ceiling and must NOT report complete")
	}
	if len(items) != 6 {
		t.Errorf("items = %d, want 6 (3 pages of 2)", len(items))
	}
	if strings.Join(pagesServed, ",") != "1,2,3" {
		t.Errorf("pages requested = %v, want 1,2,3", pagesServed)
	}

	// A short page ends the walk, and THAT one is complete.
	calls := 0
	e2, pkg2, done2 := run(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`[{"n": 1}, {"n": 2}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"n": 3}]`))
	})
	defer done2()

	items, complete, _, err = e2.CallPaged(context.Background(), pkg2, opOf(t, pkg2, "probe.issue.list"),
		map[string]any{"owner": "acme", "repo": "widgets"}, creds())
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if !complete {
		t.Error("a short page means the collection ended — the walk is complete")
	}
	if len(items) != 3 {
		t.Errorf("items = %d, want 3", len(items))
	}
}

// TestAMissingCollectionIsAnErrorNotAnEmptyWalk covers the failure that makes
// pagination dangerous rather than merely wrong.
//
// The vendor answers an ENVELOPE (`{"ok": true, "data": [...]}`) where the
// package expects the body to be the array. Extracting nothing then looks
// exactly like a short page, which is how every style signals the end — so the
// walk used to return zero items and report the collection COMPLETE. A
// workflow reading that concludes the repository has no issues.
//
// This is the shipped Forgejo `repository.search` shape, not an invention.
func TestAMissingCollectionIsAnErrorNotAnEmptyWalk(t *testing.T) {
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok": true, "data": [{"n": 1}, {"n": 2}]}`))
	})
	defer done()

	// The fixture's list operation declares no items_field, so the envelope
	// hides the array from it.
	items, complete, _, err := e.CallPaged(context.Background(), pkg, opOf(t, pkg, "probe.issue.list"),
		map[string]any{"owner": "acme", "repo": "widgets"}, creds())
	if err == nil {
		t.Fatalf("a body carrying no array at the declared address must be an error, got items=%d complete=%v", len(items), complete)
	}
	if complete {
		t.Error("a walk that could not find its collection must never report complete")
	}
	// The diagnostic has to name the fix, since the reader's next question is
	// always "where should it have looked?".
	if !strings.Contains(err.Error(), "items_field") {
		t.Errorf("error must point at items_field, got: %v", err)
	}
	if !strings.Contains(err.Error(), "data") {
		t.Errorf("error must list the body's keys so the field is visible, got: %v", err)
	}

	// Naming the field makes the same response walk correctly — proof the
	// refusal is about the address, not about envelopes as such.
	op := opOf(t, pkg, "probe.issue.list")
	withField := *op.Pagination
	withField.ItemsField = "data"
	op.Pagination = &withField
	items, complete, _, err = e.CallPaged(context.Background(), pkg, op,
		map[string]any{"owner": "acme", "repo": "widgets"}, creds())
	if err != nil {
		t.Fatalf("with items_field set: %v", err)
	}
	// The handler serves a full page (2 = the declared size) every time, so
	// the walk runs to its ceiling of 3 and honestly reports it stopped there.
	if len(items) != 6 || complete {
		t.Errorf("items = %d complete = %v, want 6 and false (3 full pages, stopped at MaxPages)", len(items), complete)
	}
}

// TestAnEmptyPageIsStillAnEndedWalk guards the other side of the distinction
// above: an HONEST empty collection must keep ending the walk quietly. A guard
// that turned every empty result into an error would be worse than the defect
// it replaced.
// TestACursorWalkEndsOnTheCURSOR, not on a short page.
//
// A cursor API is free to hand back a partial page together with a next
// cursor — it pages by position, not by count, so "fewer rows than asked for"
// carries no meaning. Applying the page/offset rule here stopped the walk
// after one call while the vendor was still explicitly saying "there is more",
// and reported the result COMPLETE.
func TestACursorWalkEndsOnTheCURSOR(t *testing.T) {
	pages := []string{
		`{"items": [{"n": 1}], "next": "c2"}`, // SHORT, but more to come
		`{"items": [{"n": 2}], "next": "c3"}`,
		`{"items": [{"n": 3}], "next": ""}`, // the vendor says: that is all
	}
	var seen []string
	call := 0
	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("after"))
		if call < len(pages) {
			_, _ = w.Write([]byte(pages[call]))
		}
		call++
	})
	defer done()

	op := opOf(t, pkg, "probe.issue.list")
	op.Params = append(op.Params, spec.Param{Key: "after", Name: "after", In: spec.InQuery, Type: "string"})
	op.Pagination = &spec.Pagination{
		Style: spec.PageCursor, CursorParam: "after", CursorField: "next",
		ItemsField: "items", DefaultSize: 50, MaxPages: 5,
	}

	items, complete, _, err := e.CallPaged(context.Background(), pkg, op,
		map[string]any{"owner": "acme", "repo": "widgets"}, creds())
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("items = %d, want 3 — a short page with a next cursor is not the end", len(items))
	}
	if !complete {
		t.Error("the vendor handed back an empty cursor, which IS the end — the walk is complete")
	}
	if strings.Join(seen, ",") != ",c2,c3" {
		t.Errorf("cursors sent = %v, want the first call bare then c2, c3", seen)
	}
}

// TestAHandPickedPageIsNeverTheWholeCollection.
//
// Complete means "these items are the whole collection". One page an author
// asked for by name is not: page 7 says nothing about pages 1-6, and a short
// page 7 says nothing about page 8. Reporting it complete told a workflow it
// had seen everything right after it deliberately looked at one slice.
func TestAHandPickedPageIsNeverTheWholeCollection(t *testing.T) {
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		// SHORT (1 < the declared size of 2) — the shape that used to read as
		// "the collection ended".
		_, _ = w.Write([]byte(`[{"n": 1}]`))
	})
	defer done()

	items, complete, _, err := e.CallPaged(context.Background(), pkg, opOf(t, pkg, "probe.issue.list"),
		map[string]any{"owner": "acme", "repo": "widgets", "page": 7}, creds())
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("items = %d, want 1", len(items))
	}
	if complete {
		t.Error("one hand-picked page is never the whole collection, however short it is")
	}
}

// TestTheWalkMeasuresPagesAgainstTheSizeACTUALLYREQUESTED.
//
// A caller may set the page size themselves. Comparing their pages against the
// package's DEFAULT then reads every page as short — ending the walk after one
// call — or as full forever. The offset arithmetic has the same dependency:
// stepping by the default while asking for another size skips rows or returns
// them twice.
func TestTheWalkMeasuresPagesAgainstTheSizeACTUALLYREQUESTED(t *testing.T) {
	var offsets []string
	call := 0
	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		offsets = append(offsets, r.URL.Query().Get("page"))
		call++
		// The caller asked for 1 per page. Two full pages, then an empty one.
		if call <= 2 {
			_, _ = w.Write([]byte(`[{"n": 1}]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	defer done()

	op := opOf(t, pkg, "probe.issue.list")
	off := *op.Pagination
	off.Style = spec.PageOffset
	op.Pagination = &off

	items, complete, _, err := e.CallPaged(context.Background(), pkg, op,
		map[string]any{"owner": "acme", "repo": "widgets", "limit": 1}, creds())
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("items = %d, want 2 — a page of 1 is FULL when 1 is what was asked for", len(items))
	}
	if !complete {
		t.Error("the empty third page ended the collection")
	}
	// Offsets step by the requested size (1), not the package default (2).
	if strings.Join(offsets, ",") != "0,1,2" {
		t.Errorf("offsets = %v, want 0,1,2 — stepping by the size actually requested", offsets)
	}
}

func TestAnEmptyPageIsStillAnEndedWalk(t *testing.T) {
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	defer done()

	items, complete, _, err := e.CallPaged(context.Background(), pkg, opOf(t, pkg, "probe.issue.list"),
		map[string]any{"owner": "acme", "repo": "widgets"}, creds())
	if err != nil {
		t.Fatalf("an empty array is a valid empty collection: %v", err)
	}
	if !complete {
		t.Error("an empty collection is a complete one")
	}
	if len(items) != 0 {
		t.Errorf("items = %d, want 0", len(items))
	}
}

// TestAnExplicitPageMeansOnePage pins the semantics of an author paging by
// hand: they asked for page 7, so page 7 is what happens. Walking on from
// there would silently return three pages where one was requested.
func TestAnExplicitPageMeansOnePage(t *testing.T) {
	var seen []string
	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("page"))
		// A FULL page, so a walk would keep going if one had started.
		_, _ = w.Write([]byte(`[{"n": 1}, {"n": 2}]`))
	})
	defer done()

	items, complete, _, err := e.CallPaged(context.Background(), pkg, opOf(t, pkg, "probe.issue.list"),
		map[string]any{"owner": "a", "repo": "w", "page": 7}, creds())
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if strings.Join(seen, ",") != "7" {
		t.Errorf("pages requested = %v, want exactly the author's 7", seen)
	}
	if len(items) != 2 {
		t.Errorf("items = %d, want the one page's 2", len(items))
	}
	if complete {
		t.Error("a full page means the collection may continue — not complete")
	}
}

// --- Slack's shape: failure inside a 200 -----------------------------------

// slackShaped is the pilot case a status-only reader gets wrong.
func slackShaped(base string) *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion,
			ID:            "chat", Version: "1.0.0",
			BaseURL: spec.BaseURL{Default: base, PathPrefix: "/api"},
			Auth:    []spec.AuthScheme{{ID: "bearer", Kind: spec.AuthBearer}},
			Outcome: &spec.OutcomePolicy{
				SuccessWhen:       "body.ok == true",
				ErrorCodeField:    "error",
				ErrorMessageField: "error",
				ErrorCodeMap: map[string]spec.ErrorClass{
					"missing_scope":     spec.ErrForbidden,
					"channel_not_found": spec.ErrNotFound,
					"ratelimited":       spec.ErrRateLimited,
				},
			},
			Maturity: spec.MaturityExperimental,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "chat", Domain: "chat",
			Operations: []spec.Operation{{
				ID: "chat.chat.post_message", Resource: "chat", Verb: "post_message",
				HTTP:   spec.HTTPBinding{Method: "POST", Path: "/chat.postMessage", RequestBody: spec.BodyForm},
				Effect: spec.EffectCreate, Deterministic: true,
				Params: []spec.Param{
					{Key: "channel", Name: "channel", In: spec.InBody, Type: "string", Required: true},
					{Key: "text", Name: "text", In: spec.InBody, Type: "string"},
				},
				Results: []spec.ResultCase{{Status: 200}},
			}},
		}},
	}
}

func TestFailureInsideATwoHundredIsAFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantOK    bool
		wantClass spec.ErrorClass
		wantCode  string
	}{
		{"a genuine success", `{"ok": true, "ts": "1"}`, true, "", ""},
		{"a mapped failure", `{"ok": false, "error": "missing_scope"}`, false, spec.ErrForbidden, "missing_scope"},
		{"another mapped failure", `{"ok": false, "error": "channel_not_found"}`, false, spec.ErrNotFound, "channel_not_found"},
		// An unmapped code must NOT become a success: the status says nothing
		// (200 classifies to ""), so the fallback has to be a failure or the
		// policy leaks exactly the case it exists to catch.
		{"an unmapped failure", `{"ok": false, "error": "some_new_code"}`, false, spec.ErrBadRequest, "some_new_code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			e := &exec.Executor{Client: srv.Client()}
			pkg := slackShaped(srv.URL)
			op, _ := pkg.Operation("chat.chat.post_message")

			res, err := e.Call(context.Background(), pkg, op,
				map[string]any{"channel": "C1", "text": "hi"},
				exec.Credential{SchemeID: "bearer", Value: "xoxb"})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if res.Status != 200 {
				t.Fatalf("status = %d, want 200 — the vendor answered success", res.Status)
			}
			if tc.wantOK {
				if !res.OK() {
					t.Fatalf("want success, got %v", res.Err)
				}
				return
			}
			if res.OK() {
				t.Fatal("a 200 whose body reports a failure must NOT be a success")
			}
			if res.Err.Class != tc.wantClass {
				t.Errorf("class = %q, want %q", res.Err.Class, tc.wantClass)
			}
			if res.Err.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", res.Err.Code, tc.wantCode)
			}
		})
	}
}

// TestFormBodyReachesTheWire pins Slack's encoding: the arguments go as
// form-urlencoded, which is what its Web API reads.
func TestFormBodyReachesTheWire(t *testing.T) {
	var ct, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		b, _ := readAll(r)
		body = string(b)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()

	e := &exec.Executor{Client: srv.Client()}
	pkg := slackShaped(srv.URL)
	op, _ := pkg.Operation("chat.chat.post_message")
	if _, err := e.Call(context.Background(), pkg, op,
		map[string]any{"channel": "C1", "text": "hello world"},
		exec.Credential{SchemeID: "bearer", Value: "xoxb"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want form-urlencoded", ct)
	}
	if !strings.Contains(body, "channel=C1") || !strings.Contains(body, "text=hello+world") {
		t.Errorf("body = %q, want the form fields", body)
	}
}

// TestABrokenPredicateIsNotASuccess pins the failure mode of the policy
// itself: a typo in `success_when` must not silently restore the status-only
// behaviour the policy exists to replace.
func TestABrokenPredicateIsNotASuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()

	pkg := slackShaped(srv.URL)
	// Not a boolean: `body.error` is a STRING, which a truthy reading would
	// happily accept.
	pkg.Connector.Outcome.SuccessWhen = "body.error"

	e := &exec.Executor{Client: srv.Client()}
	op, _ := pkg.Operation("chat.chat.post_message")
	res, err := e.Call(context.Background(), pkg, op,
		map[string]any{"channel": "C1"}, exec.Credential{SchemeID: "bearer", Value: "x"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.OK() {
		t.Fatal("a predicate that does not answer a boolean must not read as success")
	}
	if !strings.Contains(res.Err.Message, "not a boolean") {
		t.Errorf("message = %q, want it to say the predicate did not answer a boolean", res.Err.Message)
	}
}

// TestNoClientIsRefused pins that the guarded client is not optional: a
// connector calls hosts a tenant chose.
func TestNoClientIsRefused(t *testing.T) {
	e := &exec.Executor{}
	pkg := probe("https://example.invalid")
	op, _ := pkg.Operation("probe.issue.get")
	_, err := e.Call(context.Background(), pkg, op, map[string]any{"owner": "a", "repo": "w", "index": 1}, creds())
	if err == nil {
		t.Fatal("an executor with no HTTP client must refuse")
	}
	if !strings.Contains(err.Error(), "guarded") {
		t.Errorf("error = %v, want it to say why a default client is not acceptable", err)
	}
}

// --- helpers ---------------------------------------------------------------

func fullParams(op string) map[string]any {
	switch op {
	case "probe.issue.comment":
		return map[string]any{"owner": "acme", "repo": "widgets", "index": 42, "body": "hello"}
	case "probe.issue.list":
		return map[string]any{"owner": "acme", "repo": "widgets"}
	default:
		return map[string]any{"owner": "acme", "repo": "widgets", "index": 42}
	}
}

func readAll(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, nil
		}
	}
}
