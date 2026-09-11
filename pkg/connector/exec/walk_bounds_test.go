package exec

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// A WALK had no bound of its own. `max_pages` is taken verbatim from the
// package (only <= 0 falls back to the default 20), and spec validation
// cross-checks the pagination block's parameter names without ever looking at
// that number — while each page reads up to the 32 MiB LimitReader and every
// page's items are held until the whole call returns and is marshalled into
// the artifact. At the DEFAULT twenty pages that is already ~640 MiB with no
// malicious package in sight, and packages arrive from tiers the workflow
// never named.
//
// In-package, because what is under test is the bound itself.

// walkFixture is a cursor-paginated collection that NEVER ends: the vendor
// always answers a next cursor, so nothing but iterion's own ceiling stops
// the walk.
func walkFixture(base string, maxPages int) (*spec.Package, spec.Operation) {
	op := spec.Operation{
		ID: "probe.thing.list", Resource: "thing", Verb: "list",
		HTTP:   spec.HTTPBinding{Method: "GET", Path: "/things"},
		Effect: spec.EffectRead, Deterministic: true,
		Params: []spec.Param{
			{Key: "cursor", Name: "cursor", In: spec.InQuery, Type: "string"},
		},
		Results: []spec.ResultCase{{Status: 200}},
		Pagination: &spec.Pagination{
			Style: spec.PageCursor, CursorParam: "cursor", CursorField: "next",
			ItemsField: "items", MaxPages: maxPages,
		},
	}
	pkg := &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probe", Version: "1.0.0",
			BaseURL:  spec.BaseURL{Default: base},
			Auth:     []spec.AuthScheme{{ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization"}},
			Maturity: spec.MaturityExperimental,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion, Connector: "probe", Domain: "thing",
			Operations: []spec.Operation{op},
		}},
	}
	return pkg, op
}

// endlessPage is one page of a collection that never ends, carrying a cursor
// that ADVANCES.
//
// That is what a real vendor does, and iterion's no-progress guard requires
// it: a cursor repeated verbatim says nothing, so re-sending it would re-read
// one page, and the walk stops rather than spending the ceiling on it. A
// fixture that models an endless collection has to model an endless PROTOCOL.
//
// The counter is fixed-width so every page is the same size on the wire, which
// is what the byte-budget test's arithmetic rests on.
func endlessPage(n int, pad string) string {
	return fmt.Sprintf(`{"items":[{"id":1,"s":%q}],"next":"p%06d"}`, pad, n)
}

// TestAPackagesOwnMaxPagesIsClamped.
//
// The number is the PACKAGE's, and a package is not the workflow's to trust
// with an unbounded one: `max_pages: 5000` would spend five thousand of the
// vendor's rate-limit slots behind a single node.
func TestAPackagesOwnMaxPagesIsClamped(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = fmt.Fprint(w, endlessPage(requests, ""))
	}))
	defer srv.Close()

	e := &Executor{Client: srv.Client()}
	pkg, op := walkFixture(srv.URL, 5000)
	items, complete, last, err := e.CallPaged(context.Background(), pkg, op, nil, Credential{SchemeID: "token", Value: "x"})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if requests != maxWalkPages {
		t.Errorf("the vendor was called %d times, want the walk clamped at %d", requests, maxWalkPages)
	}
	if complete {
		t.Error("a walk stopped at iterion's ceiling is NOT complete — the collection may well continue")
	}
	if len(items) != maxWalkPages {
		t.Errorf("items = %d, want the pages actually walked", len(items))
	}
	if last.Requests != maxWalkPages {
		t.Errorf("Requests = %d, want the walk's total so the node's real cost is visible", last.Requests)
	}
}

// TestACursorThatDoesNotAdvanceStopsTheWalk.
//
// `walkFixture`'s vendor always answers the same cursor, which is what the two
// bound tests exploit — and it is also a real shape: a vendor that echoes its
// cursor on the last page, or a package naming a field that happens to be
// constant. Re-sending it fetches the SAME page again, so the walk spent up to
// maxWalkPages of the vendor's rate-limit slots and returned 500 copies of one
// page. The duplicated items are the worse half: a workflow acts on them.
//
// Incomplete rather than an error — the pages gathered are real, and the
// protocol never said the collection had ended.
func TestACursorThatDoesNotAdvanceStopsTheWalk(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = fmt.Fprint(w, `{"items":[{"id":1}],"next":"stuck"}`)
	}))
	defer srv.Close()

	e := &Executor{Client: srv.Client()}
	pkg, op := walkFixture(srv.URL, 50)
	items, complete, last, err := e.CallPaged(context.Background(), pkg, op, nil, Credential{SchemeID: "token", Value: "x"})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Two: the first page sets the cursor, the second answers the same one.
	if requests != 2 {
		t.Errorf("the vendor was called %d times, want 2 — a cursor that does not advance says nothing, and re-sending it re-reads one page", requests)
	}
	if len(items) != 2 {
		t.Errorf("items = %d, want the pages actually walked", len(items))
	}
	if complete {
		t.Error("the protocol never said the collection had ended, so this walk is NOT complete")
	}
	if last.Requests != 2 {
		t.Errorf("Requests = %d, want the walk's total", last.Requests)
	}
}

// TestAWalkStopsOnItsByteBudget.
//
// The page ceiling alone is not a bound: the bytes are what the pod pays, and
// they accumulate across pages. Stopping is reported the way running out of
// pages already is — the items gathered so far, complete=false, which is
// exactly what CallPaged's contract tells a caller to read.
func TestAWalkStopsOnItsByteBudget(t *testing.T) {
	// One item per page, large, so the DECODED size stays close to the wire
	// size and the test's own memory is the budget and not a multiple of it.
	// The cursor advances (fixed-width, so every page weighs the same), which
	// is what keeps the stop attributable to the BYTES.
	const pageSize = 8 << 20
	pad := strings.Repeat("x", pageSize)
	pageLen := len(endlessPage(1, pad))

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = fmt.Fprint(w, endlessPage(requests, pad))
	}))
	defer srv.Close()

	e := &Executor{Client: srv.Client()}
	// A page ceiling far above what the byte budget allows, so the stop can
	// only come from the bytes.
	pkg, op := walkFixture(srv.URL, 400)
	items, complete, last, err := e.CallPaged(context.Background(), pkg, op, nil, Credential{SchemeID: "token", Value: "x"})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if complete {
		t.Error("a walk stopped on its byte budget is NOT complete")
	}
	wantPages := maxWalkBytes / pageLen
	if pageLen*wantPages < maxWalkBytes {
		wantPages++
	}
	if requests != wantPages {
		t.Errorf("the vendor was called %d times (%d bytes), want the walk stopped at %d — the budget is %d",
			requests, requests*pageLen, wantPages, maxWalkBytes)
	}
	if len(items) != wantPages {
		t.Errorf("items = %d, want one per page walked", len(items))
	}
	if last.Bytes != requests*pageLen {
		t.Errorf("Bytes = %d, want the walk's TOTAL (%d) — it is what the budget is measured in", last.Bytes, requests*pageLen)
	}
}

// TestAWalkThatFailsMidWayStillReportsWhatItSpENT.
//
// The two tests above walk to a clean stop, which is the only shape the
// accounting held for: the cumulative count was assigned AFTER the two failure
// returns, so a walk that died on page 18 handed back the failing page's own
// count — 1 for an HTTP error, 0 for a transport failure.
//
// That is the exact under-reporting `Result.Requests`' own doc comment says it
// exists to end, still live on the path an operator most needs it: a `retry: 3`
// node adds `res.Requests` per attempt, so eighteen spent pages were billed as
// one and the vendor's rate limit was hit from a node reporting a handful of
// calls.
func TestAWalkThatFailsMidWayStillReportsWhatItSpent(t *testing.T) {
	const failOn = 4
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == failOn {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"error":"boom"}`)
			return
		}
		_, _ = fmt.Fprint(w, endlessPage(requests, ""))
	}))
	defer srv.Close()

	e := &Executor{Client: srv.Client()}
	pkg, op := walkFixture(srv.URL, 50)
	items, complete, last, err := e.CallPaged(context.Background(), pkg, op, nil, Credential{SchemeID: "token", Value: "x"})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if complete {
		t.Error("a walk that died mid-way is not a complete collection")
	}
	if len(items) != failOn-1 {
		t.Errorf("items = %d, want the %d pages that answered", len(items), failOn-1)
	}
	if last.Requests != failOn {
		t.Errorf("Requests = %d, want %d — the vendor served every page before the one that failed, and the node's accounting is the only place that says so",
			last.Requests, failOn)
	}
	if last.Bytes <= 0 {
		t.Error("Bytes is dropped on the same path and for the same reason — the walk's total must survive the failure")
	}
}

// The transport half of the same promise, where the old code was worse still:
// a page that lost its answer reported ZERO, so the whole walk vanished from
// the accounting rather than merely shrinking to one.
//
// The pages the vendor ANSWERED are the oracle here, not the handler's own
// call count: net/http replays a GET whose reused connection died, so the
// handler runs more times than the walk asked for pages.
func TestAWalkThatDiesOnTransportReportsThePagesItSpent(t *testing.T) {
	const answerPages = 2
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if served < answerPages {
			served++
			_, _ = fmt.Fprint(w, endlessPage(served, ""))
			return
		}
		// From here the vendor takes the request and never answers.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test server must support hijacking")
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

	e := &Executor{Client: srv.Client()}
	pkg, op := walkFixture(srv.URL, 50)
	_, complete, last, err := e.CallPaged(context.Background(), pkg, op, nil, Credential{SchemeID: "token", Value: "x"})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if complete {
		t.Error("a walk whose page lost its answer is not a complete collection")
	}
	if last.Err == nil {
		t.Fatal("the walk must carry the failure that stopped it")
	}
	// The operation READS, so a lost answer is an ordinary transport failure —
	// and the page that got no answer is not counted as one the walk got.
	if last.Requests != answerPages {
		t.Errorf("Requests = %d, want %d — the pages the vendor answered, and not the one it did not",
			last.Requests, answerPages)
	}
}
