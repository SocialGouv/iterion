package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/modelprefs"
)

func newPrefsServer(t *testing.T, store modelprefs.Store) *Server {
	t.Helper()
	srv := New(Config{
		DisableAuth:             true,
		WorkDir:                 t.TempDir(),
		StoreDir:                t.TempDir(),
		SkipProjectRegistration: true,
		ModelPrefs:              store,
	}, iterlog.New(iterlog.LevelError, nil))
	return srv
}

func prefRequest(t *testing.T, srv *Server, method, path, body string) (*httptest.ResponseRecorder, modelPrefResponse) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, r)
	var out modelPrefResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("bad response body: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, out
}

// The whole point: the choice survives the session. Recording it and reading it
// back on a later request is the contract the assistant relies on.
func TestModelPref_RoundTrips(t *testing.T) {
	srv := newPrefsServer(t, modelprefs.NewMemStore())

	rec, got := prefRequest(t, srv, http.MethodGet, "/api/v1/preferences/model?key=whats-next", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rec.Code, rec.Body.String())
	}
	// "Not recorded" has to be distinguishable from "recorded and empty",
	// otherwise the caller cannot tell a deliberate default from a fresh host.
	if got.Set {
		t.Errorf("a never-recorded preference must report set=false, got %+v", got)
	}

	rec, got = prefRequest(t, srv, http.MethodPut, "/api/v1/preferences/model",
		`{"key":"whats-next","model":"anthropic/claude-opus-5","backend":"claude_code","effort":"ultracode"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", rec.Code, rec.Body.String())
	}
	if !got.Set || got.Model != "anthropic/claude-opus-5" || got.Effort != "ultracode" {
		t.Errorf("PUT echo = %+v", got)
	}

	_, got = prefRequest(t, srv, http.MethodGet, "/api/v1/preferences/model?key=whats-next", "")
	if !got.Set || got.Model != "anthropic/claude-opus-5" || got.Backend != "claude_code" || got.Effort != "ultracode" {
		t.Errorf("GET after PUT = %+v", got)
	}

	// A different scope key is a different preference — that is what lets a
	// second conversational bot exist without an engine change.
	_, got = prefRequest(t, srv, http.MethodGet, "/api/v1/preferences/model?key=some-other-bot", "")
	if got.Set {
		t.Errorf("preference leaked across scope keys: %+v", got)
	}

	rec, _ = prefRequest(t, srv, http.MethodDelete, "/api/v1/preferences/model?key=whats-next", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d: %s", rec.Code, rec.Body.String())
	}
	_, got = prefRequest(t, srv, http.MethodGet, "/api/v1/preferences/model?key=whats-next", "")
	if got.Set {
		t.Errorf("DELETE left a preference behind: %+v", got)
	}
}

// A preference is re-applied on EVERY future session, so a bad effort stored
// here would keep breaking runs long after the operator forgot typing it.
func TestModelPref_RejectsAnUnknownEffort(t *testing.T) {
	srv := newPrefsServer(t, modelprefs.NewMemStore())

	rec, _ := prefRequest(t, srv, http.MethodPut, "/api/v1/preferences/model",
		`{"key":"whats-next","effort":"turbo"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "xhigh") {
		t.Errorf("the error should list the valid levels: %s", rec.Body.String())
	}
}

func TestModelPref_RejectsAnUnknownBackend(t *testing.T) {
	srv := newPrefsServer(t, modelprefs.NewMemStore())

	rec, _ := prefRequest(t, srv, http.MethodPut, "/api/v1/preferences/model",
		`{"key":"whats-next","backend":"clua"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "claude_code") {
		t.Errorf("the error should list the valid backends: %s", rec.Body.String())
	}
}

func TestModelPref_RejectsABlankKey(t *testing.T) {
	srv := newPrefsServer(t, modelprefs.NewMemStore())

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/preferences/model", ""},
		{http.MethodGet, "/api/v1/preferences/model?key=%20", ""},
		{http.MethodPut, "/api/v1/preferences/model", `{"model":"anthropic/claude-opus-5"}`},
		{http.MethodDelete, "/api/v1/preferences/model", ""},
	} {
		rec, _ := prefRequest(t, srv, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s status = %d, want 400", tc.method, tc.path, rec.Code)
		}
	}
}

func TestModelPref_RejectsOversizedAndMalformedKeys(t *testing.T) {
	srv := newPrefsServer(t, modelprefs.NewMemStore())
	for _, key := range []string{strings.Repeat("a", modelprefs.MaxKeyLength+1), "two words"} {
		rec, _ := prefRequest(t, srv, http.MethodPut, "/api/v1/preferences/model",
			fmt.Sprintf(`{"key":%q}`, key))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("key %q status = %d, want 400: %s", key, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "key") {
			t.Errorf("key %q error is not actionable: %s", key, rec.Body.String())
		}
	}
}

func TestModelPref_CardinalityLimitIsAConflict(t *testing.T) {
	st := modelprefs.NewMemStore()
	ctx := context.Background()
	for i := 0; i < modelprefs.MaxPreferencesPerScope; i++ {
		if err := st.Set(ctx, &modelprefs.Pref{UserID: "dev", Key: fmt.Sprintf("bot-%d", i)}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	srv := newPrefsServer(t, st)
	rec, _ := prefRequest(t, srv, http.MethodPut, "/api/v1/preferences/model", `{"key":"one-too-many"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "maximum") {
		t.Errorf("limit error is not actionable: %s", rec.Body.String())
	}
}

// With no store wired the routes must not exist at all rather than pretending
// to save — the studio then falls back to a per-launch choice.
func TestModelPref_RoutesAbsentWithoutAStore(t *testing.T) {
	srv := newPrefsServer(t, nil)

	rec, _ := prefRequest(t, srv, http.MethodGet, "/api/v1/preferences/model?key=whats-next", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("expected no 200 without a prefs store, got %s", rec.Body.String())
	}
}
