package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// dslToolWorkflow parses, compiles, and unparses cleanly. Chosen over an
// agent-only fixture so the tests stay independent of any LLM registry.
const dslToolWorkflow = `tool echo:
  command: "echo hi"

workflow main:
  entry: echo
  echo -> done
`

// dslInvalidWorkflow parses cleanly but its edge targets a node that does
// not exist, which the IR compiler must flag as C001 (DiagUnknownNode).
// It is the smallest workflow that separates a valid /api/parse from a
// failed /api/validate on the same document.
const dslInvalidWorkflow = `tool echo:
  command: "echo hi"

workflow main:
  entry: echo
  echo -> notdefined
`

// dslUnparseableSource is not a valid iterion workflow — used to exercise
// the parser diagnostic path of /api/parse. The unbalanced bracket makes
// the lexer emit a token that the top-level dispatch cannot consume.
const dslUnparseableSource = "workflow main:\n  [ this is not a workflow"

// postDSLJSON sends a JSON body to the server and returns the *http.Response
// for direct status inspection. Callers are responsible for closing the
// body via decodeJSONResp or resp.Body.Close(). Distinct name from the
// existing postJSON in runs_merge_conflict_test.go, which takes a
// different signature (body any + Marshal-ing).
func postDSLJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// TestParse_ValidWorkflow proves /api/parse returns a non-empty document
// and no diagnostics for a syntactically valid source.
func TestParse_ValidWorkflow(t *testing.T) {
	_, hs := newTestServer(t)

	resp := postDSLJSON(t, hs.URL+"/api/parse", `{"source":`+jsonString(dslToolWorkflow)+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var out parseResponse
	decodeJSONResp(t, resp, &out)

	if len(out.Document) == 0 {
		t.Fatal("Document is empty on a valid source")
	}
	if len(out.Diagnostics) != 0 {
		t.Fatalf("expected no parse diagnostics, got %v", out.Diagnostics)
	}
	// The document must be a well-formed JSON object. A regression that
	// swaps in a nil AST would produce a bare "null" here.
	var probe map[string]any
	if err := json.Unmarshal(out.Document, &probe); err != nil {
		t.Fatalf("Document is not a JSON object: %v (raw=%s)", err, string(out.Document))
	}
	if len(probe) == 0 {
		t.Fatalf("Document decoded to an empty map (raw=%s)", string(out.Document))
	}
}

// TestParse_UnparseableSource proves the parser diagnostic channel is
// wired: an obviously invalid source must produce at least one
// non-empty diagnostic string.
func TestParse_UnparseableSource(t *testing.T) {
	_, hs := newTestServer(t)

	resp := postDSLJSON(t, hs.URL+"/api/parse", `{"source":`+jsonString(dslUnparseableSource)+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
	var out parseResponse
	decodeJSONResp(t, resp, &out)

	if len(out.Diagnostics) == 0 {
		t.Fatalf("expected at least one parse diagnostic, got none; document=%s", string(out.Document))
	}
	for _, d := range out.Diagnostics {
		if strings.TrimSpace(d) == "" {
			t.Fatalf("empty diagnostic string in %v", out.Diagnostics)
		}
	}
}

// TestValidate_ValidWorkflow proves the compile-time validator returns
// Valid=true, no errors, and honest node/edge counts on a good AST.
// It also proves that /api/parse and /api/validate compose (the AST
// coming out of parse is accepted by validate without any coercion).
func TestValidate_ValidWorkflow(t *testing.T) {
	_, hs := newTestServer(t)

	doc := parseDocument(t, hs.URL, dslToolWorkflow)

	resp := postDSLJSON(t, hs.URL+"/api/validate", `{"document":`+string(doc)+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
	var out validateResponse
	decodeJSONResp(t, resp, &out)

	if !out.Valid {
		t.Fatalf("expected Valid=true; diagnostics=%v, issues=%+v", out.Diagnostics, out.Issues)
	}
	if len(out.Diagnostics) != 0 {
		t.Fatalf("expected no diagnostics, got %v", out.Diagnostics)
	}
	// The IR always synthesises done + fail alongside the declared nodes, so
	// the minimum node count for our fixture is 3 (echo + done + fail).
	if out.NodeCount < 3 {
		t.Fatalf("NodeCount=%d, want >= 3 (echo+done+fail)", out.NodeCount)
	}
	if out.EdgeCount < 1 {
		t.Fatalf("EdgeCount=%d, want >= 1", out.EdgeCount)
	}
	// A verdict without any error-severity issue should also carry no
	// error-severity DiagnosticDTO — otherwise the studio would render a
	// red badge on a bot the endpoint just called valid.
	for _, iss := range out.Issues {
		if iss.Severity == "error" {
			t.Fatalf("Valid=true but Issues carries an error: %+v", iss)
		}
	}
}

// TestValidate_UnknownNodeReferenceIsC001 proves the validator produces
// a structured DiagnosticDTO on a semantic failure — specifically the
// canonical "edge to unknown node" case that maps to C001. This asserts
// the CONTRACT the studio uses to render inline error badges: code,
// severity, and non-empty message. A regression that drops the code or
// downgrades the severity would be caught here.
func TestValidate_UnknownNodeReferenceIsC001(t *testing.T) {
	_, hs := newTestServer(t)

	doc := parseDocument(t, hs.URL, dslInvalidWorkflow)

	resp := postDSLJSON(t, hs.URL+"/api/validate", `{"document":`+string(doc)+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
	var out validateResponse
	decodeJSONResp(t, resp, &out)

	if out.Valid {
		t.Fatalf("expected Valid=false for edge-to-unknown-node; got %+v", out)
	}
	if len(out.Diagnostics) == 0 {
		t.Fatal("expected at least one error-severity diagnostic string")
	}
	// Locate the C001 DiagnosticDTO. We do NOT match on message text
	// (which is intentionally free-form and free to change); we match
	// on the machine-readable code the studio keys off.
	var got *DiagnosticDTO
	for i := range out.Issues {
		if out.Issues[i].Code == "C001" {
			got = &out.Issues[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("expected an Issue with Code=C001, got %+v", out.Issues)
	}
	if got.Severity != "error" {
		t.Fatalf("C001 Severity=%q, want %q", got.Severity, "error")
	}
	if strings.TrimSpace(got.Message) == "" {
		t.Fatalf("C001 Message is empty in %+v", got)
	}
}

// TestValidate_RejectsGarbageDocument proves the endpoint refuses a
// non-AST body with a 400 rather than silently returning Valid=true.
func TestValidate_RejectsGarbageDocument(t *testing.T) {
	_, hs := newTestServer(t)

	resp := postDSLJSON(t, hs.URL+"/api/validate", `{"document":{"agents":"not-a-list"}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
}

// TestUnparse_RoundTripIsSemanticallyStable proves the round-trip
// parse→unparse→parse produces the SAME document, byte-for-byte. This
// is the studio's implicit contract: an editor save must not lose
// information. A regression that drops a field on the unparse path
// would flip the second document and be caught here.
func TestUnparse_RoundTripIsSemanticallyStable(t *testing.T) {
	_, hs := newTestServer(t)

	// Pass 1: parse the original source → doc1.
	doc1 := parseDocument(t, hs.URL, dslToolWorkflow)

	// Pass 2: unparse doc1 → source2 (must be non-empty).
	unparseResp := postDSLJSON(t, hs.URL+"/api/unparse", `{"document":`+string(doc1)+`}`)
	if unparseResp.StatusCode != http.StatusOK {
		t.Fatalf("unparse status=%d, want 200; body=%s", unparseResp.StatusCode, mustReadBody(t, unparseResp))
	}
	var up unparseResponse
	decodeJSONResp(t, unparseResp, &up)
	if strings.TrimSpace(up.Source) == "" {
		t.Fatal("unparse produced empty source")
	}
	// Unparse must mention the declared node id — the strongest text-level
	// signal that it did not silently emit an empty document.
	if !strings.Contains(up.Source, "echo") {
		t.Fatalf("unparsed source missing 'echo': %q", up.Source)
	}

	// Pass 3: re-parse source2 → doc2. Must be a valid workflow with no
	// diagnostics, and doc2 must equal doc1 byte-for-byte after
	// canonicalising via json.Compact (MarshalIndent already emits
	// deterministic output, but Compact protects against a future
	// change to the indentation).
	doc2 := parseDocument(t, hs.URL, up.Source)

	if !jsonEqual(t, doc1, doc2) {
		t.Fatalf("round-trip is not stable\ndoc1=%s\ndoc2=%s", string(doc1), string(doc2))
	}
}

// TestUnparse_RejectsInvalidDocument proves /api/unparse validates its
// input before invoking the unparser. A regression that fed garbage
// straight into ast.UnmarshalFile could panic; instead we require a
// clean 400.
func TestUnparse_RejectsInvalidDocument(t *testing.T) {
	_, hs := newTestServer(t)

	resp := postDSLJSON(t, hs.URL+"/api/unparse", `{"document":{"agents":"not-a-list"}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// parseDocument POSTs source to /api/parse and returns the raw document
// bytes. Fails the test if parse produced diagnostics — the callers
// expect a clean parse before they feed the doc into validate/unparse.
func parseDocument(t *testing.T, base, source string) json.RawMessage {
	t.Helper()
	resp := postDSLJSON(t, base+"/api/parse", `{"source":`+jsonString(source)+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("parse status=%d, want 200; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
	var pr parseResponse
	decodeJSONResp(t, resp, &pr)
	if len(pr.Diagnostics) != 0 {
		t.Fatalf("parse produced diagnostics on source we expected to be clean: %v", pr.Diagnostics)
	}
	if len(pr.Document) == 0 {
		t.Fatal("parse returned empty document on clean source")
	}
	return pr.Document
}

// jsonEqual reports whether a and b encode the same JSON value.
// Compares via json.Compact so a stray whitespace difference does not
// mask a real change.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var ca, cb bytes.Buffer
	if err := json.Compact(&ca, a); err != nil {
		t.Fatalf("compact a: %v", err)
	}
	if err := json.Compact(&cb, b); err != nil {
		t.Fatalf("compact b: %v", err)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// mustReadBody reads the response body into a string for error
// messages. Closes the body as a side effect — safe because the caller
// is failing the test immediately after.
func mustReadBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b.String()
}
