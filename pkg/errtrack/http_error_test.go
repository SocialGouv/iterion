package errtrack

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/log"
	sentry "github.com/getsentry/sentry-go"
)

func TestHTTPFailureCorrelatesWithTheRequestTraceAndLog(t *testing.T) {
	enable(t, Config{TracesSampleRate: rate(1)})
	var logs bytes.Buffer
	logger := log.New(log.LevelError, &logs)
	var ids map[string]string
	wrapped := HTTPMiddleware(HTTPOptions{})(HTTPErrorMiddleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = HTTPFailure(w, 503, "database private-host refused")
		span := sentry.SpanFromContext(r.Context())
		if span == nil || ids["trace_id"] != span.TraceID.String() {
			t.Fatalf("error is not tied to its request trace: %v", ids)
		}
	})))
	req := httptest.NewRequest("GET", "/api/resource?ticket=private-ticket", nil)
	req.Header.Set("X-Request-ID", "client-controlled")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if ids["request_id"] == "" || ids["request_id"] == "client-controlled" || w.Header().Get("X-Request-ID") != ids["request_id"] {
		t.Fatalf("invalid correlation: %v", ids)
	}
	for _, want := range []string{ids["trace_id"], ids["request_id"], "private-host", "/api/resource"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log lost %q: %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "private-ticket") {
		t.Fatal("request query leaked into the log")
	}
}

func TestHTTPErrorWriterKeepsStreamingAndUpgradeOperations(t *testing.T) {
	w := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
	bound := HTTPErrorWriter(w, httptest.NewRequest("GET", "/", nil), nil)
	bound.(http.Flusher).Flush()
	if !w.Flushed {
		t.Fatal("SSE flush lost")
	}
	if _, _, err := bound.(http.Hijacker).Hijack(); err != nil || !w.hijacked {
		t.Fatalf("WebSocket upgrade lost: %v", err)
	}
}
