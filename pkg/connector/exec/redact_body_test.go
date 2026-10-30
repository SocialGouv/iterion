package exec_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// TestACredentialNeverAppearsInABodyTheCallerReads is the other end of
// TestACredentialNeverAppearsInAnError.
//
// The error path was redacted and the SUCCESS path was not: `Result.Data` is
// the vendor's decoded body, and a vendor that echoes the request URI — a
// `next`/`self` pagination link, a debug envelope, an audit field — hands a
// query-placed credential straight back. That body is what a `tool … action:`
// node writes to its output and what the MCP facade serialises into the
// agent's tool answer, so a 2xx carried the token into a run's artefacts and
// into a model's context with nothing between.
func TestACredentialNeverAppearsInABodyTheCallerReads(t *testing.T) {
	const token = "s3cret-token-value"

	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The vendor's self link: the request URI, verbatim, in a 200.
		body, _ := json.Marshal(map[string]any{
			"items": []any{map[string]any{"self": "http://" + r.Host + r.URL.RequestURI()}},
			"next":  r.URL.RequestURI(),
		})
		_, _ = w.Write(body)
	})
	defer done()

	// The leaking shape: the credential travels in the QUERY STRING.
	pkg.Connector.Auth = []spec.AuthScheme{{
		ID: "token", Kind: spec.AuthAPIKey, In: "query", Name: "access_token",
	}}

	res, err := e.Call(context.Background(), pkg, opOf(t, pkg, "probe.issue.list"),
		fullParams("probe.issue.list"), exec.Credential{SchemeID: "token", Value: token})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.OK() {
		t.Fatalf("result = %v", res.Err)
	}
	rendered, err := json.Marshal(res.Data)
	if err != nil {
		t.Fatalf("marshal the data the caller reads: %v", err)
	}
	if strings.Contains(string(rendered), token) {
		t.Errorf("the credential appears verbatim in the body the caller reads: %s", rendered)
	}
	if esc := url.QueryEscape(token); esc != token && strings.Contains(string(rendered), esc) {
		t.Errorf("the credential appears URL-escaped in the body the caller reads: %s", rendered)
	}
	// The body must still be USABLE: redaction that erased the answer would
	// trade a leak for a broken node.
	if !strings.Contains(string(rendered), "redacted") {
		t.Errorf("the redaction must be visible, so a reader knows something was removed: %s", rendered)
	}
	if !strings.Contains(string(rendered), "items") {
		t.Errorf("the vendor's own fields must survive the redaction: %s", rendered)
	}
}

// TestASecretBodyIsRedactedWhereverItSitsInTheJSON pins the SHAPE of the walk:
// a secret echoed as a map KEY, inside a nested object, inside an array, and
// as a bare string body are each a place a vendor has been seen to put it.
// Asserting only the top level would leave the walk free to stop at depth one.
func TestASecretBodyIsRedactedWhereverItSitsInTheJSON(t *testing.T) {
	const token = "s3cret-token-value"

	var payload string
	e, pkg, done := run(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	})
	defer done()
	pkg.Connector.Auth = []spec.AuthScheme{{
		ID: "token", Kind: spec.AuthAPIKey, In: "query", Name: "access_token",
	}}
	op := opOf(t, pkg, "probe.issue.list")

	for _, tc := range []struct{ name, body string }{
		{"map_key", `{"` + token + `": 1}`},
		{"nested_object", `{"a":{"b":{"c":"` + token + `"}}}`},
		{"array_element", `["ok","` + token + `"]`},
		{"array_of_objects", `[{"echo":"` + token + `"}]`},
		{"bare_string", `"` + token + `"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload = tc.body
			res, err := e.Call(context.Background(), pkg, op, fullParams("probe.issue.list"),
				exec.Credential{SchemeID: "token", Value: token})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if !res.OK() {
				t.Fatalf("result = %v", res.Err)
			}
			rendered, err := json.Marshal(res.Data)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(rendered), token) {
				t.Errorf("the credential survived at %s: %s", tc.name, rendered)
			}
		})
	}
}

// TestAWalkedPageIsRedactedToo: CallPaged gathers N bodies, and the items it
// returns are the pages' own values. A redaction applied to the LAST result
// only would hand back N-1 unredacted pages.
func TestAWalkedPageIsRedactedToo(t *testing.T) {
	const token = "s3cret-token-value"

	page := 0
	e, pkg, done := run(t, func(w http.ResponseWriter, r *http.Request) {
		page++
		w.Header().Set("Content-Type", "application/json")
		if page == 1 {
			_, _ = w.Write([]byte(`[{"uri":"` + r.URL.RequestURI() + `"},{"n":2}]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	defer done()
	pkg.Connector.Auth = []spec.AuthScheme{{
		ID: "token", Kind: spec.AuthAPIKey, In: "query", Name: "access_token",
	}}
	op := opOf(t, pkg, "probe.issue.list")
	op.Pagination = &spec.Pagination{Style: spec.PageNumber, PageParam: "page", SizeParam: "limit", DefaultSize: 2}

	items, _, _, err := e.CallPaged(context.Background(), pkg, op,
		fullParams("probe.issue.list"), exec.Credential{SchemeID: "token", Value: token})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	rendered, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(rendered), token) {
		t.Errorf("the credential appears in a walked page's items: %s", rendered)
	}
	if esc := url.QueryEscape(token); esc != token && strings.Contains(string(rendered), esc) {
		t.Errorf("the credential appears URL-escaped in a walked page's items: %s", rendered)
	}
}
