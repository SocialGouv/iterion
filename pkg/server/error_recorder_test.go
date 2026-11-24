package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// errorRecorder captures the real failure log of a directly called handler.
// Body remains the actual public response; Diagnostic checks its correlation
// before returning the logged cause for assertions about recovery or residue.
type errorRecorder struct {
	*httptest.ResponseRecorder
	logs  *lockedBuffer
	bound http.ResponseWriter
}

func newErrorRecorder() *errorRecorder {
	w := &errorRecorder{ResponseRecorder: httptest.NewRecorder(), logs: &lockedBuffer{}}
	w.bound = errtrack.HTTPErrorWriter(w.ResponseRecorder, nil, iterlog.New(iterlog.LevelError, w.logs))
	return w
}

// newServerErrorRecorder also captures request-aware helpers that bind the
// server's logger themselves. Call before starting the tested request.
func newServerErrorRecorder(s *Server) *errorRecorder {
	w := newErrorRecorder()
	w.logs = captureFailureLogs(s)
	w.bound = errtrack.HTTPErrorWriter(w.ResponseRecorder, nil, iterlog.New(iterlog.LevelError, w.logs))
	return w
}

func captureFailureLogs(s *Server) *lockedBuffer {
	logs := &lockedBuffer{}
	if s.logger == nil {
		s.logger = iterlog.New(iterlog.LevelError, logs)
	} else {
		s.logger = s.logger.WithWriter(io.MultiWriter(s.logger.Writer(), logs))
	}
	return logs
}

func (w *errorRecorder) Unwrap() http.ResponseWriter { return w.bound }

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (w *errorRecorder) Diagnostic(t *testing.T) string {
	t.Helper()
	if w.Code < 500 || w.Code > 599 {
		return w.Body.String()
	}
	id := w.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatalf("failure lacks its correlation header: %s", w.Body.String())
	}
	return correlatedFailureLog(t, w.Body.Bytes(), id, w.logs.String())
}

func correlatedFailureLog(t *testing.T, body []byte, headerID, logs string) string {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil || got["error"] != httpx.InternalServerError {
		t.Fatalf("expected a generic failure: %s (%v)", body, err)
	}
	for name, value := range got {
		switch name {
		case "error", "error_code", "request_id", "trace_id":
			if text, ok := value.(string); !ok || text == "" {
				t.Fatalf("invalid public failure field %q: %s", name, body)
			}
		case "retryable":
			if _, ok := value.(bool); !ok {
				t.Fatalf("invalid retryability: %s", body)
			}
		default:
			t.Fatalf("unexpected public failure field %q: %s", name, body)
		}
	}
	id, _ := got["request_id"].(string)
	if id == "" || (headerID != "" && id != headerID) {
		t.Fatalf("failure lacks consistent correlation: %s (header %q)", body, headerID)
	}
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "request_id="+id) {
			if _, payload, ok := strings.Cut(line, "): "); ok {
				return payload
			}
		}
	}
	t.Fatalf("failure %s has no correlated log in %s", body, logs)
	return ""
}
