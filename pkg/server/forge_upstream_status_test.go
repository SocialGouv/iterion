package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// A forge that answers 429 is not iterion breaking. Told 500, the operator,
// the logs, Sentry and every alert read an iterion fault where the truth is
// "the forge is rate-limiting us, retry later" — measured 34 times in 60
// calls on the OAuth-app route.
func TestForgeOAuthAppAutoCreate_UpstreamStatusTaxonomy(t *testing.T) {
	cases := []struct {
		name         string
		upstream     int
		retryAfter   string
		want         int
		wantRetryHdr string
	}{
		{"rate limit answers 429 with the forge's own delay", http.StatusTooManyRequests, "42", http.StatusTooManyRequests, "42"},
		{"rate limit with no delay still answers 429", http.StatusTooManyRequests, "", http.StatusTooManyRequests, ""},
		{"an upstream 5xx is a bad gateway", http.StatusServiceUnavailable, "", http.StatusBadGateway, ""},
		{"an upstream 502 is a bad gateway", http.StatusBadGateway, "", http.StatusBadGateway, ""},
		{"a refusal the operator can act on keeps its own 4xx", http.StatusConflict, "", http.StatusConflict, ""},
		{"a scope refusal stays 403", http.StatusForbidden, "", http.StatusForbidden, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.upstream)
			}))
			defer forgeSrv.Close()

			s := newForgeOAuthAppTestServer(t)
			w := httptest.NewRecorder()
			body := `{"provider":"gitlab","mode":"auto","admin_token":"admintok","forge_base_url":"` + forgeSrv.URL + `"}`
			s.handleRegisterForgeOAuthApp(w, oauthAppReq(superAdminCtx(), "POST", "/api/teams/t1/forge/oauth-apps", body, "t1", ""))

			if w.Code != tc.want {
				t.Fatalf("upstream %d → status %d, want %d; body=%s", tc.upstream, w.Code, tc.want, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got != tc.wantRetryHdr {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetryHdr)
			}
		})
	}
}

// Only an iterion fault answers 500 — the whole point of the taxonomy. A
// failure that is not an answer from the forge keeps the fault status.
func TestWriteForgeOAuthAppError_IterionFaultStays500(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).writeForgeOAuthAppError(w, errors.New("seal client secret: keyring unavailable"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — this one really is iterion's", w.Code)
	}
}

// forgeUpstreamStatus is the ONE table every forge-facing handler's default
// arm consults; a 0 means "not an answer the forge gave", which each handler
// answers with its own fault status.
func TestForgeUpstreamStatus_Table(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"rate limit", &forge.StatusError{Provider: "gitlab", Op: "o", Code: http.StatusTooManyRequests}, http.StatusTooManyRequests},
		{"upstream 500", &forge.StatusError{Code: http.StatusInternalServerError}, http.StatusBadGateway},
		{"upstream 503", &forge.StatusError{Code: http.StatusServiceUnavailable}, http.StatusBadGateway},
		{"upstream 409 reflects the request", &forge.StatusError{Code: http.StatusConflict}, http.StatusConflict},
		{"a status below 400 is no answer to mirror", &forge.StatusError{Code: http.StatusFound}, http.StatusBadGateway},
		{"named permission gap", &forge.PermissionError{Missing: []string{"checks:read"}, Cause: forge.ErrForbidden}, http.StatusUnprocessableEntity},
		{"bare forbidden", forge.ErrForbidden, http.StatusForbidden},
		{"typed 404", &forge.NotFoundError{Op: "GET pull"}, http.StatusNotFound},
		{"rejected credential", forge.ErrUnauthorized, http.StatusUnprocessableEntity},
		{"installation permissions", forge.ErrPermissionsNotGranted, http.StatusUnprocessableEntity},
		{"an iterion fault is not an upstream answer", errors.New("marshal body: unsupported type"), 0},
		{"no error", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := forgeUpstreamStatus(tc.err)
			if got != tc.want {
				t.Errorf("forgeUpstreamStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// A transport failure — no answer at all — is the forge's side, never a 500.
func TestForgeUpstreamStatus_TransportFailureIs502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now
	c := forge.NewAdminHTTP(nil, url, "gitlab", nil)
	err := c.DoTyped(t.Context(), http.MethodGet, "/user", "GET /user", nil, nil)
	if err == nil {
		t.Fatal("expected a dial failure")
	}
	if got, _ := forgeUpstreamStatus(err); got != http.StatusBadGateway {
		t.Fatalf("forgeUpstreamStatus(%v) = %d, want 502 — a forge that could not be reached is not iterion breaking", err, got)
	}
}
