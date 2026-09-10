package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The App's identity is what every loop guard compares a commenter against:
// the bot posts as "<slug>[bot]". A client whose Cfg carried no slug answered
// "github-app[bot]" — a login that never posts — so the guard compared
// against nobody and the App's own comments cleared it. The slug is one
// App-JWT call away (GET /app); the client resolves it once and keeps it.
func TestAppClientWhoAmIResolvesTheSlugFromTheApp(t *testing.T) {
	var appCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/app" {
			atomic.AddInt32(&appCalls, 1)
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				t.Errorf("GET /app must be App-JWT authenticated, got %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "slug": "iterion-forge-x", "name": "iterion forge"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	a := &AppClient{HTTP: srv.Client(), WebBaseURL: srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: testKeyPEMOnce(t)}, InstallationID: 99}
	for i := 0; i < 2; i++ {
		id, err := a.WhoAmI(context.Background())
		if err != nil {
			t.Fatalf("WhoAmI: %v", err)
		}
		if id.Login != "iterion-forge-x[bot]" || id.Namespace != "iterion-forge-x" || id.Kind != forge.AccountKindInstallation {
			t.Fatalf("identity = %+v, want the App's real slug, not a placeholder", id)
		}
	}
	if n := atomic.LoadInt32(&appCalls); n != 1 {
		t.Errorf("GET /app called %d times over two WhoAmI calls, want once (memoized on the client)", n)
	}
}

// A configured slug is authoritative and costs no round trip.
func TestAppClientWhoAmIUsesTheConfiguredSlugWithoutAProbe(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a := &AppClient{HTTP: srv.Client(), WebBaseURL: srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: testKeyPEMOnce(t), AppSlug: "iterion"}, InstallationID: 99}
	id, err := a.WhoAmI(context.Background())
	if err != nil || id.Login != "iterion[bot]" {
		t.Fatalf("identity = %+v err=%v, want iterion[bot] from the configured slug", id, err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Error("a configured slug must not be re-resolved over the network")
	}
}

// No slug configured and GitHub not answering: an error, never a placeholder
// login — a guard fed a login that never posts is worse than a guard that
// says it could not resolve one.
func TestAppClientWhoAmIFailsLoudWithoutASlug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a := &AppClient{HTTP: srv.Client(), WebBaseURL: srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: testKeyPEMOnce(t)}, InstallationID: 99}
	id, err := a.WhoAmI(context.Background())
	if err == nil {
		t.Fatalf("WhoAmI = %+v, want an error when the slug cannot be resolved", id)
	}
	if !strings.Contains(err.Error(), "/app") {
		t.Errorf("error = %q, want it to name the GET /app probe", err.Error())
	}
	if id.Login != "" {
		t.Errorf("Login = %q, want empty on failure (no placeholder)", id.Login)
	}
}
