package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// TestRemoteLogin_ATokenLoginNeedsAnIterionIdentity drives `iterion remote
// login --token <PAT> <url>` against URLs whose GET /api/auth/me answers
// something other than an iterion identity: a static host serving its SPA
// index for every path, an SSO proxy redirecting to its sign-in page, an
// unrelated JSON API. The probe is the only check that URL ever gets: none
// of these may become the credential every later `iterion remote` command
// sends there, and a redirect must not carry the PAT to its target.
func TestRemoteLogin_ATokenLoginNeedsAnIterionIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		// cause is what the refusal must name: why the answer is not an
		// identity.
		cause string
	}{
		{"a static host serving its SPA index for every path", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><html><head><title>Studio</title></head><body><div id=root></div></body></html>"))
		}, "invalid character '<'"},
		{"an SSO proxy redirecting to its sign-in page", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/oauth2/sign_in" {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<html><body>Sign in with your corporate account</body></html>"))
				return
			}
			http.Redirect(w, r, "/oauth2/sign_in?rd="+r.URL.Path, http.StatusFound)
		}, "redirects to"},
		{"an unrelated JSON API answering {}", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}, "no user id"},
		{"a JSON user without an id", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]string{"email": "ops@example.test"}})
		}, "no user id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
				mu.Unlock()
				tc.handler(w, r)
			}))
			defer srv.Close()
			out, saved, err := runRemoteLoginForTest(t, srv.URL, "", "iap_the_users_own_pat")
			if err == nil {
				t.Fatalf("a --token login against a host that is not an iterion API succeeded: %q", out)
			}
			if saved.Token != "" {
				t.Fatalf("the PAT was saved for a host that is not an iterion API: %+v", saved)
			}
			if !strings.Contains(err.Error(), "nothing was saved") {
				t.Errorf("the refusal does not say nothing was saved: %v", err)
			}
			if !strings.Contains(err.Error(), tc.cause) {
				t.Errorf("the refusal does not name its cause %q: %v", tc.cause, err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, req := range seen {
				if strings.Contains(req, "iap_the_users_own_pat") && !strings.HasPrefix(req, "GET /api/auth/me ") {
					t.Errorf("the PAT was sent beyond the probe: %s", req)
				}
			}
		})
	}
}

// TestRemoteLogin_AServerErrorSaysNothingWasSavedAndToRetry: a legitimate
// --token against an instance that cannot describe the account right now is
// refused — the probe is the only check the URL gets — and the refusal says
// that nothing was saved and to retry.
func TestRemoteLogin_AServerErrorSaysNothingWasSavedAndToRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"identity unavailable"}`))
	}))
	defer srv.Close()
	_, saved, err := runRemoteLoginForTest(t, srv.URL, "", "iap_the_users_own_pat")
	if err == nil || saved.Token != "" {
		t.Fatalf("a --token login on a 503 was not refused (err %v, saved %+v)", err, saved)
	}
	for _, want := range []string{"503", "nothing was saved", "retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal of a 503 does not say %q: %v", want, err)
		}
	}
}

// TestRemoteLogin_ATokenOfARealInstanceLogsIn is the positive control on the
// real server: a personal access token it minted logs in with --token, the
// probe reads the server's own identity and the token is saved.
func TestRemoteLogin_ATokenOfARealInstanceLogsIn(t *testing.T) {
	const email, password = "ops@example.test", "password-forever-9"
	base, _ := realIterionServer(t, email, password)
	pat, _, err := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: base}).LoginWithPassword(context.Background(), email, password, "probe", "")
	if err != nil {
		t.Fatalf("mint a token on the real server: %v", err)
	}
	out, saved, err := runRemoteLoginForTest(t, base, "", pat)
	if err != nil {
		t.Fatalf("a --token login with a token the real server minted: %v (stdout %q)", err, out)
	}
	if saved.Token != pat || saved.BaseURL != base {
		t.Fatalf("the token was not saved for the instance: %+v", saved)
	}
	if !strings.Contains(out, "as "+email) {
		t.Errorf("the login does not name the account: %q", out)
	}
}
