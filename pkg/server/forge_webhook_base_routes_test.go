package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// patchWebhookBase drives the real handler with a webhook-base-only body.
func patchWebhookBase(t *testing.T, s *Server, connID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := forgeReq(superAdminCtx(), "PATCH", "/api/teams/t1/forge/connections/"+connID, body, "t1")
	req.SetPathValue("conn_id", connID)
	w := httptest.NewRecorder()
	s.handlePatchForgeConnection(w, req)
	return w
}

// TestWebhookBasePatch_PersistsAndDoesNotTouchTheTokenPath: pinning a hook
// base mints and withdraws nothing. If it shared the security-read path, a
// deployment-topology change would move a live org token as a side effect.
func TestWebhookBasePatch_PersistsAndDoesNotTouchTheTokenPath(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "SocialGouv", "", false)
	minted := 0
	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, time.Time, error) {
		minted++
		return "ghs_minted", time.Time{}, nil
	}

	w := patchWebhookBase(t, s, "c1", `{"webhook_base_url":"https://iterion.fabrique.social.gouv.fr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if minted != 0 {
		t.Fatalf("mint calls = %d, want 0 — pinning a hook base is not a token operation", minted)
	}
	conn, err := s.forgeConnections.Get(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if conn.WebhookBaseURL != "https://iterion.fabrique.social.gouv.fr" {
		t.Fatalf("WebhookBaseURL = %q, want it persisted", conn.WebhookBaseURL)
	}
	if conn.SecurityReadEnabled {
		t.Fatal("a webhook-base patch must not flip the security-read flag")
	}
}

// TestWebhookBasePatch_ClearsWithAnExplicitEmptyString: absent and cleared
// are different intents, which is why the field is a pointer.
func TestWebhookBasePatch_ClearsWithAnExplicitEmptyString(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "SocialGouv", "", false)

	if w := patchWebhookBase(t, s, "c1", `{"webhook_base_url":"https://pinned.example"}`); w.Code != http.StatusOK {
		t.Fatalf("set: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := patchWebhookBase(t, s, "c1", `{"webhook_base_url":""}`); w.Code != http.StatusOK {
		t.Fatalf("clear: code=%d body=%s", w.Code, w.Body.String())
	}
	conn, _ := s.forgeConnections.Get(context.Background(), "c1")
	if conn.WebhookBaseURL != "" {
		t.Fatalf("WebhookBaseURL = %q, want cleared", conn.WebhookBaseURL)
	}
}

// TestWebhookBasePatch_RefusesWhatCannotBeDialed: a bad base does not fail
// here — it fails as hooks that register fine and never arrive. Refuse it
// at the only moment an operator is watching.
func TestWebhookBasePatch_RefusesWhatCannotBeDialed(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no scheme", `{"webhook_base_url":"iterion.example.com"}`},
		{"not http", `{"webhook_base_url":"ftp://iterion.example.com"}`},
		{"carries a path", `{"webhook_base_url":"https://iterion.example.com/hooks"}`},
		{"carries credentials", `{"webhook_base_url":"https://u:p@iterion.example.com"}`},
		{"carries a query", `{"webhook_base_url":"https://iterion.example.com?a=1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newForgeTestServer(t)
			seedAppConn(t, s, "c1", "SocialGouv", "", false)
			w := patchWebhookBase(t, s, "c1", tc.body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("code=%d body=%s, want 422", w.Code, w.Body.String())
			}
			conn, _ := s.forgeConnections.Get(context.Background(), "c1")
			if conn.WebhookBaseURL != "" {
				t.Fatalf("a refused value was still persisted: %q", conn.WebhookBaseURL)
			}
		})
	}
}

// TestConnectionPatch_StillRefusesAnEmptyBody: adding a second patchable
// field must not turn "nothing to update" into a silent 200.
func TestConnectionPatch_StillRefusesAnEmptyBody(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "SocialGouv", "", false)
	if w := patchWebhookBase(t, s, "c1", `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400", w.Code, w.Body.String())
	}
}

// TestConnectionPatch_RefusesACombinedBody: the two fields act on different
// systems — one pins a URL, the other mints or withdraws a live org token on
// GitHub — and nothing makes them atomic. Accepted together, a failure of
// the security-read half returns before the persist: the URL change is
// dropped while the error names only security-read, so the caller reads one
// failure and cannot tell half its intent was discarded.
func TestConnectionPatch_RefusesACombinedBody(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "SocialGouv", "", false)
	minted := 0
	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, time.Time, error) {
		minted++
		return "ghs_minted", time.Time{}, nil
	}

	body := `{"security_read_enabled":true,"webhook_base_url":"https://pinned.example"}`
	w := patchWebhookBase(t, s, "c1", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400", w.Code, w.Body.String())
	}
	if minted != 0 {
		t.Fatalf("mint calls = %d, want 0 — the refusal must happen before either side acts", minted)
	}
	conn, _ := s.forgeConnections.Get(context.Background(), "c1")
	if conn.WebhookBaseURL != "" || conn.SecurityReadEnabled {
		t.Fatalf("a refused combined patch applied something: base=%q security_read=%v",
			conn.WebhookBaseURL, conn.SecurityReadEnabled)
	}
}

func TestCanonicalWebhookBaseURL(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"https://iterion.cloud", "https://iterion.cloud", false},
		{"https://iterion.cloud/", "https://iterion.cloud", false},
		{"http://localhost:8080", "http://localhost:8080", false},
		{"iterion.cloud", "", true},
		{"https://", "", true},
	} {
		got, err := canonicalWebhookBaseURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("canonicalWebhookBaseURL(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("canonicalWebhookBaseURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("canonicalWebhookBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
