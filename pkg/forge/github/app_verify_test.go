package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// verifyStub serves the two endpoints VerifyInstallationOwnership hits:
// the user-auth token exchange and GET /user/installations. installs is the
// set of installation ids the (single) test user can see.
func verifyStub(t *testing.T, installs []int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/login/oauth/access_token"):
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse form: %v", err)
			}
			if r.Form.Get("code") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "bad_verification_code"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "ghu_user"})
		case strings.HasSuffix(r.URL.Path, "/user/installations"):
			if got := r.Header.Get("Authorization"); got != "Bearer ghu_user" {
				t.Errorf("authorization = %q, want Bearer ghu_user", got)
			}
			insts := make([]map[string]any, 0, len(installs))
			for _, id := range installs {
				insts = append(insts, map[string]any{"id": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(installs), "installations": insts})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestVerifyInstallationOwnership_Owned(t *testing.T) {
	srv := verifyStub(t, []int64{7, 99, 42})
	defer srv.Close()
	cfg := AppConfig{AppID: 1, ClientID: "cid", ClientSecret: "sec"}
	if err := VerifyInstallationOwnership(context.Background(), srv.Client(), srv.URL, cfg, "abc", 99); err != nil {
		t.Fatalf("expected owned, got %v", err)
	}
}

func TestVerifyInstallationOwnership_NotOwned(t *testing.T) {
	srv := verifyStub(t, []int64{7, 42}) // 99 absent — the IDOR attempt
	defer srv.Close()
	cfg := AppConfig{AppID: 1, ClientID: "cid", ClientSecret: "sec"}
	err := VerifyInstallationOwnership(context.Background(), srv.Client(), srv.URL, cfg, "abc", 99)
	if !errors.Is(err, ErrInstallationNotOwned) {
		t.Fatalf("expected ErrInstallationNotOwned, got %v", err)
	}
}

func TestVerifyInstallationOwnership_MissingCode(t *testing.T) {
	cfg := AppConfig{AppID: 1, ClientID: "cid", ClientSecret: "sec"}
	// No HTTP call should happen — a missing code fails closed before exchange.
	if err := VerifyInstallationOwnership(context.Background(), http.DefaultClient, "https://github.com", cfg, "", 99); err == nil {
		t.Fatal("expected an error for a missing code")
	}
}

func TestVerifyInstallationOwnership_NoClientCreds(t *testing.T) {
	cfg := AppConfig{AppID: 1} // UserAuthConfigured()==false
	if err := VerifyInstallationOwnership(context.Background(), http.DefaultClient, "https://github.com", cfg, "abc", 99); err == nil {
		t.Fatal("expected an error when client creds are absent")
	}
}

func TestVerifyInstallationOwnership_ExchangeFailure(t *testing.T) {
	// The stub returns bad_verification_code when code is empty; force a failed
	// exchange by having the server reject the code.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "bad_verification_code"})
	}))
	defer srv.Close()
	cfg := AppConfig{AppID: 1, ClientID: "cid", ClientSecret: "sec"}
	err := VerifyInstallationOwnership(context.Background(), srv.Client(), srv.URL, cfg, "abc", 99)
	if err == nil || errors.Is(err, ErrInstallationNotOwned) {
		t.Fatalf("expected a non-ownership exchange error, got %v", err)
	}
}
