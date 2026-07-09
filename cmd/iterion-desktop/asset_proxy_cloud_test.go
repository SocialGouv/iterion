//go:build desktop

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAssetProxy_CloudBearerInjectionAndRotation exercises the authenticating
// tunnel end-to-end without a GUI: a request through the asset proxy to a
// "cloud" that requires a Bearer must (1) carry the injected token, (2) never
// leak the wails-origin Cookie, and (3) capture a rotated auth cookie back
// into the jar while stripping it from the response reaching the webview.
func TestAssetProxy_CloudBearerInjectionAndRotation(t *testing.T) {
	initialTok := mkJWT(time.Now().Add(15 * time.Minute).Unix())
	rotatedTok := mkJWT(time.Now().Add(30 * time.Minute).Unix())

	var sawAuth, sawCookie string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawCookie = r.Header.Get("Cookie")
		if r.Header.Get("Authorization") != "Bearer "+initialTok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Simulate an org/team switch rotating the access cookie.
		http.SetCookie(w, &http.Cookie{Name: cloudAuthCookieName, Value: rotatedTok, Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "unrelated", Value: "keep", Path: "/"})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cloud.Close()

	store := newFakeStore()
	jar := newCloudTokenJar("conn-proxy", cloud.URL, store)
	if err := jar.applyRotation(initialTok, "refresh-seed"); err != nil {
		t.Fatalf("seed jar: %v", err)
	}

	app := &App{serverURL: cloud.URL + "/", cloudJar: jar}
	h := &assetProxyHandler{app: app}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	// The webview sends junk cookies on the wails origin; they must be stripped.
	req.Header.Set("Cookie", "wails_junk=1")
	req.Header.Set("Origin", "wails://wails")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxied request status = %d, want 200 (auth not accepted?)", rec.Code)
	}
	if sawAuth != "Bearer "+initialTok {
		t.Errorf("cloud saw Authorization %q, want injected Bearer", sawAuth)
	}
	if sawCookie != "" {
		t.Errorf("wails-origin Cookie leaked to cloud: %q", sawCookie)
	}
	// The rotated auth cookie must land in the jar…
	if jar.AccessToken() != rotatedTok {
		t.Errorf("jar access token = %q, want rotated %q", jar.AccessToken(), rotatedTok)
	}
	// …and be stripped from the response the webview sees, while unrelated
	// cookies pass through.
	setCookies := rec.Result().Header["Set-Cookie"]
	for _, sc := range setCookies {
		if len(sc) >= len(cloudAuthCookieName) && sc[:len(cloudAuthCookieName)] == cloudAuthCookieName {
			t.Errorf("auth Set-Cookie leaked to webview: %q", sc)
		}
	}
	foundUnrelated := false
	for _, sc := range setCookies {
		if len(sc) >= 10 && sc[:10] == "unrelated=" {
			foundUnrelated = true
		}
	}
	if !foundUnrelated {
		t.Errorf("unrelated Set-Cookie was dropped: %v", setCookies)
	}
}

// TestAssetProxy_LocalNoInjection confirms the historical local path is
// unchanged: with no cloud jar, no Authorization header is added.
func TestAssetProxy_LocalNoInjection(t *testing.T) {
	var sawAuth string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()

	app := &App{serverURL: local.URL + "/"} // cloudJar nil → local mode
	h := &assetProxyHandler{app: app}

	req := httptest.NewRequest(http.MethodGet, "/api/server/info", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("local proxied status = %d, want 200", rec.Code)
	}
	if sawAuth != "" {
		t.Errorf("local mode injected Authorization %q, want none", sawAuth)
	}
}
