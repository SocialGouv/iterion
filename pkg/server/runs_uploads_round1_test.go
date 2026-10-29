package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An upload named like the record sidecar would be read back as its own
// record: refused at the door, typed.
func TestUploadAttachment_RefusesTheSidecarName(t *testing.T) {
	srv := uploadTestServer(t)
	for _, name := range []string{"meta.json", "META.JSON"} {
		body, ct := multipartBody(t, name, "application/json", []byte(`{"user":"payload"}`))
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/runs/uploads", body)
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		srv.handleUploadAttachment(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, w.Code)
		}
	}
}

// An attachment name becomes a directory entry a driver hands to tar by
// name; one that reads as an option is refused before anything moves.
func TestPromoteStagedRefusesAnOptionLikeName(t *testing.T) {
	srv := uploadTestServer(t)
	id := stageOne(t, srv, "spec.md", "text/markdown", []byte("# spec"))
	_, _, err := srv.promoteStaged(context.Background(), "run-opt-001", map[string]string{"--checkpoint=1": id})
	if err == nil || !strings.Contains(err.Error(), "must not start with a dash") {
		t.Fatalf("err = %v, want the dash refusal", err)
	}
}
