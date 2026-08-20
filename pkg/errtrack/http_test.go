package errtrack

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// hijackableRecorder mimics the shape of net/http's own response
// writer — Flusher + Hijacker + io.ReaderFrom — so a test can prove the
// middleware does not cost the server its WebSocket upgrades.
//
// All three matter: the SDK's response-writer proxy only forwards
// Hijack when the writer it wraps implements the whole trio, and
// degrades to a flush-only proxy otherwise. Being the OUTERMOST
// middleware is what guarantees it sees the real writer.
type hijackableRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func (h *hijackableRecorder) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(h.Body, r)
}

func TestHTTPMiddlewareIsTheIdentityWhenTracingIsOff(t *testing.T) {
	t.Setenv(EnvTracesSampleRate, "")
	tr := enable(t, Config{})

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	wrapped := HTTPMiddleware(HTTPOptions{RouteName: func(*http.Request) string { return "GET /never" }})(next)

	// Not "behaves the same" — the SAME handler value: with tracing off
	// there is no wrapper at all on the server's hot path.
	if reflect.ValueOf(wrapped).Pointer() != reflect.ValueOf(next).Pointer() {
		t.Fatal("HTTPMiddleware wrapped the handler while tracing is off")
	}

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/things/42", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d", rec.Code)
	}
	Flush()
	if got := len(transactions(tr.all())); got != 0 {
		t.Fatalf("tracing off produced %d transaction(s)", got)
	}
}

func TestHTTPMiddlewareRecordsOneTransactionPerRequest(t *testing.T) {
	tr := enable(t, Config{TracesSampleRate: rate(1)})

	wrapped := HTTPMiddleware(HTTPOptions{})(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
	))

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/things/42", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d — the middleware changed the response", rec.Code)
	}
	Flush()

	txns := transactions(tr.all())
	if len(txns) != 1 {
		t.Fatalf("want 1 transaction, got %d", len(txns))
	}
	if txns[0].Transaction != "GET /api/things/42" {
		t.Errorf("transaction name = %q", txns[0].Transaction)
	}
}

func TestRouteNameKeepsTransactionCardinalityLow(t *testing.T) {
	tr := enable(t, Config{TracesSampleRate: rate(1)})

	wrapped := HTTPMiddleware(HTTPOptions{
		RouteName: func(*http.Request) string { return "GET /api/things/{id}" },
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	for _, path := range []string{"/api/things/42", "/api/things/1337"} {
		wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	Flush()

	txns := transactions(tr.all())
	if len(txns) != 2 {
		t.Fatalf("want 2 transactions, got %d", len(txns))
	}
	for _, txn := range txns {
		if txn.Transaction != "GET /api/things/{id}" {
			t.Errorf("transaction name = %q — two URLs must share one route name", txn.Transaction)
		}
	}
}

func TestWrappedHandlerKeepsItsRequestAndCanStillHijack(t *testing.T) {
	enable(t, Config{TracesSampleRate: rate(1)})

	var gotPath, gotHeader, gotValue string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Get("X-Iterion-Run")
		gotValue = r.PathValue("id")
		// The run console upgrades to WebSocket here; a wrapper that
		// hides http.Hijacker would break every live run view.
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Errorf("Hijack: %v", err)
		}
	})

	mux := http.NewServeMux()
	mux.Handle("GET /api/things/{id}", next)
	wrapped := HTTPMiddleware(HTTPOptions{
		RouteName: func(*http.Request) string { return "GET /api/things/{id}" },
	})(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/things/42", nil)
	req.Header.Set("X-Iterion-Run", "run-token")
	rec := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
	wrapped.ServeHTTP(rec, req)

	if !rec.hijacked {
		t.Fatal("the handler could not hijack the connection")
	}
	if gotPath != "/api/things/42" {
		t.Errorf("path = %q", gotPath)
	}
	if gotHeader != "run-token" {
		t.Errorf("header = %q — the request copy lost its headers", gotHeader)
	}
	// The clone sets Pattern; the mux must still resolve wildcards.
	if gotValue != "42" {
		t.Errorf("PathValue(id) = %q — route matching broke", gotValue)
	}
}

func TestAPanicInAHandlerIsCapturedAndRepanicked(t *testing.T) {
	tr := enable(t, Config{TracesSampleRate: rate(1)})

	wrapped := HTTPMiddleware(HTTPOptions{})(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { panic("handler exploded") },
	))

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the panic was swallowed — the server's panic semantics changed")
			}
		}()
		wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))
	}()
	Flush()

	// A string panic value is reported as a message event by the SDK.
	var found bool
	for _, ev := range tr.all() {
		if ev.Type == "transaction" {
			continue
		}
		if strings.Contains(ev.Message, "handler exploded") {
			found = true
		}
		for _, ex := range ev.Exception {
			if strings.Contains(ex.Value, "handler exploded") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the handler panic was not reported")
	}
}
