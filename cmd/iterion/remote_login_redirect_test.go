package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRemoteLogin_ARedirectedIdentityProbeNamesWhereToLogIn drives a login
// against a legitimate iterion behind a front that REDIRECTS
// GET /api/auth/me — to its canonical host (the usual case: an explicit
// `http://host` behind an https-forcing or www-normalising proxy). The probe
// follows nothing, so the token is never sent to the target; what the refusal
// owes the operator is the URL that does serve the API, since logging in to
// THAT is what every later command needs. A target on the same base names no
// other instance, and the refusal does not invent one.
func TestRemoteLogin_ARedirectedIdentityProbeNamesWhereToLogIn(t *testing.T) {
	identity := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer iap_legit" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"authentication required"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]string{"id": "u1", "email": "ops@example.test"}})
	}

	t.Run("a 302 to the canonical host names it", func(t *testing.T) {
		var canonicalHits atomic.Int64
		canonical := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			canonicalHits.Add(1)
			identity(w, r)
		}))
		defer canonical.Close()
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, canonical.URL+r.URL.Path, http.StatusFound)
		}))
		defer front.Close()

		out, saved, err := runRemoteLoginForTest(t, front.URL, "", "iap_legit")
		if err == nil {
			t.Fatalf("a login whose identity probe was redirected succeeded: %s", out)
		}
		msg := err.Error()
		if !strings.Contains(msg, `redirects to "`+canonical.URL+`/api/auth/me"`) {
			t.Errorf("the refusal does not name the redirect target: %v", msg)
		}
		if want := "if that is this iterion instance, log in to " + canonical.URL + " instead"; !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say what to type instead (%q): %v", want, msg)
		}
		if !strings.Contains(msg, "nothing was saved") {
			t.Errorf("the refusal does not say nothing was saved: %v", msg)
		}
		if saved.Token != "" {
			t.Errorf("the token was saved for a URL that does not serve the API: %+v", saved)
		}
		if n := canonicalHits.Load(); n != 0 {
			t.Errorf("the token was sent to the redirect target %d time(s)", n)
		}
	})

	t.Run("a 308 on the same host names no other instance", func(t *testing.T) {
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/") {
				identity(w, r)
				return
			}
			http.Redirect(w, r, r.URL.Path+"/", http.StatusPermanentRedirect)
		}))
		defer front.Close()

		out, saved, err := runRemoteLoginForTest(t, front.URL, "", "iap_legit")
		if err == nil {
			t.Fatalf("a login whose identity probe was redirected succeeded: %s", out)
		}
		msg := err.Error()
		if !strings.Contains(msg, "not an iterion API answer") {
			t.Errorf("the refusal of a same-host redirect: %v", msg)
		}
		if strings.Contains(msg, "log in to") {
			t.Errorf("the refusal tells the operator to log in to the URL they already typed: %v", msg)
		}
		if saved.Token != "" {
			t.Errorf("the token was saved: %+v", saved)
		}
	})

	t.Run("a self-minted login does not call a redirect transient", func(t *testing.T) {
		canonical := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity(w, r)
		}))
		defer canonical.Close()
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/auth/login":
				_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "access-1"})
			case "/api/me/tokens":
				_ = json.NewEncoder(w).Encode(map[string]string{"token": "iap_minted"})
			default:
				http.Redirect(w, r, canonical.URL+r.URL.Path, http.StatusFound)
			}
		}))
		defer front.Close()

		out, saved, err := runRemoteLoginForTest(t, front.URL, "ops@example.test", "")
		if err != nil {
			t.Fatalf("a login that minted its own token was refused: %v", err)
		}
		if saved.Token != "iap_minted" {
			t.Fatalf("the minted token was not saved: %+v", saved)
		}
		if strings.Contains(out, "right now") {
			t.Errorf("the login calls a permanent redirect a passing failure: %q", out)
		}
		if !strings.Contains(out, "log in to "+canonical.URL+" instead") {
			t.Errorf("the login does not name the URL that serves the API: %q", out)
		}
	})
}

// TestRedirectOrigin_NamesAnotherInstanceOnly pins the predicate the refusal
// reads: a target elsewhere is a URL to log in to, a target on the base
// itself is not.
func TestRedirectOrigin_NamesAnotherInstanceOnly(t *testing.T) {
	for _, tc := range []struct {
		name, base, location, want string
	}{
		{"an absolute target on another host", "http://front.test", "https://cloud.test/api/auth/me", "https://cloud.test"},
		{"the same host over https", "http://cloud.test", "https://cloud.test/api/auth/me", "https://cloud.test"},
		{"an absolute target on the same base", "https://cloud.test", "https://cloud.test/api/auth/me/", ""},
		{"a relative target", "https://cloud.test", "/api/auth/me/", ""},
		{"no Location header", "https://cloud.test", "", ""},
		{"an unparseable target", "https://cloud.test", "http://[::1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redirectOrigin(tc.base, tc.location); got != tc.want {
				t.Fatalf("redirectOrigin(%q, %q) = %q, want %q", tc.base, tc.location, got, tc.want)
			}
		})
	}
	// A base the operator typed is always absolute (runRemoteLogin prefixes a
	// scheme), which is what makes a relative Location resolvable at all.
	if _, err := url.Parse("https://cloud.test"); err != nil {
		t.Fatal(err)
	}
}
