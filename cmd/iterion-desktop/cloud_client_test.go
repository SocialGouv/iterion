package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory refreshStore for jar tests.
type fakeStore struct {
	mu sync.Mutex
	m  map[string]string
}

func newFakeStore() *fakeStore { return &fakeStore{m: map[string]string{}} }

func (s *fakeStore) Get(k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	if !ok {
		return "", nil
	}
	return v, nil
}
func (s *fakeStore) Set(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}
func (s *fakeStore) Delete(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, k)
	return nil
}

// mkJWT builds an unsigned JWT with the given exp claim (header.payload.sig).
func mkJWT(exp int64) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := enc(map[string]string{"alg": "none", "typ": "JWT"})
	payload := enc(map[string]int64{"exp": exp})
	return header + "." + payload + ".sig"
}

// fakeCloud is a minimal auth server: /api/auth/login, /api/auth/refresh,
// /api/server/info. It rotates the refresh cookie on each call so tests can
// assert rotation capture.
type fakeCloud struct {
	mu        sync.Mutex
	rev       int
	badLogin  bool
	badExpAt  bool // send empty expires_at (force JWT fallback)
	lastRefIn string
}

func (f *fakeCloud) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/server/info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"mode": "cloud", "auth_required": true})
	})
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if f.badLogin {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid credentials"})
			return
		}
		f.writeAuth(w)
	})
	mux.HandleFunc("POST /api/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastRefIn = r.Header.Get("X-Iterion-Refresh")
		f.mu.Unlock()
		if r.Header.Get("X-Iterion-Refresh") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.writeAuth(w)
	})
	return mux
}

func (f *fakeCloud) writeAuth(w http.ResponseWriter) {
	f.mu.Lock()
	f.rev++
	rev := f.rev
	f.mu.Unlock()
	refresh := "refresh-tok-" + itoa(rev)
	http.SetCookie(w, &http.Cookie{Name: cloudRefreshCookieName, Value: refresh, Path: "/api/auth", HttpOnly: true})
	http.SetCookie(w, &http.Cookie{Name: cloudAuthCookieName, Value: "access-cookie", Path: "/", HttpOnly: true})
	exp := time.Now().Add(15 * time.Minute)
	body := map[string]any{
		"user":           map[string]any{"id": "u1", "email": "a@b.io", "name": "Alice", "is_super_admin": false},
		"active_org_id":  "org1",
		"active_team_id": "team1",
		"access_token":   mkJWT(exp.Unix()),
	}
	if !f.badExpAt {
		body["expires_at"] = exp.Format(time.RFC3339)
	}
	json.NewEncoder(w).Encode(body)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestCloudEndpoint(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://c.io", "/api/auth/login", "https://c.io/api/auth/login"},
		{"https://c.io/", "/api/auth/login", "https://c.io/api/auth/login"},
		{"https://c.io/base/", "/api/x", "https://c.io/base/api/x"},
	}
	for _, c := range cases {
		got, err := cloudEndpoint(c.base, c.path)
		if err != nil {
			t.Fatalf("cloudEndpoint(%q): %v", c.base, err)
		}
		if got != c.want {
			t.Errorf("cloudEndpoint(%q,%q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
	if _, err := cloudEndpoint("not-absolute", "/x"); err == nil {
		t.Error("expected error for non-absolute URL")
	}
}

func TestJWTExpiry(t *testing.T) {
	exp := time.Now().Add(10 * time.Minute).Unix()
	got, ok := jwtExpiry(mkJWT(exp))
	if !ok || got.Unix() != exp {
		t.Errorf("jwtExpiry = %v,%v want %d", got, ok, exp)
	}
	if _, ok := jwtExpiry("not-a-jwt"); ok {
		t.Error("jwtExpiry should fail on non-JWT")
	}
}

func TestParseCloudExpiry_JWTFallback(t *testing.T) {
	exp := time.Now().Add(9 * time.Minute).Truncate(time.Second)
	got := parseCloudExpiry("", mkJWT(exp.Unix()))
	if got.Unix() != exp.Unix() {
		t.Errorf("expiry via JWT = %v, want %v", got, exp)
	}
	// Body expires_at wins over JWT.
	rfc := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	got = parseCloudExpiry(rfc.Format(time.RFC3339), mkJWT(exp.Unix()))
	if got.Unix() != rfc.Unix() {
		t.Errorf("expiry via body = %v, want %v", got, rfc)
	}
}

func TestParseCloudErrorBody(t *testing.T) {
	if got := parseCloudErrorBody([]byte(`{"error":"nope"}`)); got != "nope" {
		t.Errorf("got %q", got)
	}
	if got := parseCloudErrorBody([]byte(`{"message":"bad"}`)); got != "bad" {
		t.Errorf("got %q", got)
	}
	if got := parseCloudErrorBody([]byte(`plain text`)); got != "plain text" {
		t.Errorf("got %q", got)
	}
}

func TestCloudLogin_Success(t *testing.T) {
	f := &fakeCloud{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	res, err := cloudLogin(context.Background(), newCloudHTTPClient(), srv.URL, "a@b.io", "pw")
	if err != nil {
		t.Fatalf("cloudLogin: %v", err)
	}
	if res.AccessToken == "" {
		t.Error("no access token")
	}
	if res.RefreshToken != "refresh-tok-1" {
		t.Errorf("refresh = %q, want refresh-tok-1", res.RefreshToken)
	}
	if res.User.Email != "a@b.io" || res.User.ActiveOrgID != "org1" || res.User.ActiveTeamID != "team1" {
		t.Errorf("user summary wrong: %+v", res.User)
	}
	if res.AccessExpiry.Before(time.Now().Add(10 * time.Minute)) {
		t.Errorf("expiry too soon: %v", res.AccessExpiry)
	}
}

func TestCloudLogin_BadCredentials(t *testing.T) {
	f := &fakeCloud{badLogin: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	_, err := cloudLogin(context.Background(), newCloudHTTPClient(), srv.URL, "a@b.io", "wrong")
	ae := asCloudAuthError(err)
	if ae == nil {
		t.Fatalf("expected *cloudAuthError, got %v", err)
	}
	if ae.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", ae.Status)
	}
	if ae.Message != "invalid credentials" {
		t.Errorf("message = %q", ae.Message)
	}
}

func TestJar_SeedHydrateRefreshClear(t *testing.T) {
	f := &fakeCloud{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	store := newFakeStore()
	jar := newCloudTokenJar("conn-1", srv.URL, store)

	// Login + seed.
	res, err := cloudLogin(context.Background(), jar.hc, srv.URL, "a@b.io", "pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := jar.seed(res); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !jar.LoggedIn() || jar.AccessToken() == "" {
		t.Fatal("jar not logged in after seed")
	}
	if store.m["cloud_refresh:conn-1"] != "refresh-tok-1" {
		t.Errorf("refresh not persisted: %v", store.m)
	}

	// Refresh rotates the token and re-persists.
	if err := jar.refreshNow(context.Background()); err != nil {
		t.Fatalf("refreshNow: %v", err)
	}
	if f.lastRefIn != "refresh-tok-1" {
		t.Errorf("server saw refresh %q, want refresh-tok-1", f.lastRefIn)
	}
	if got := store.m["cloud_refresh:conn-1"]; got != "refresh-tok-2" {
		t.Errorf("rotated refresh = %q, want refresh-tok-2", got)
	}

	// A fresh jar hydrates the persisted refresh and can mint an access token.
	jar2 := newCloudTokenJar("conn-1", srv.URL, store)
	ok, err := jar2.hydrate()
	if err != nil || !ok {
		t.Fatalf("hydrate = %v,%v", ok, err)
	}
	if jar2.AccessToken() != "" {
		t.Error("hydrate must not mint an access token")
	}
	if err := jar2.refreshNow(context.Background()); err != nil {
		t.Fatalf("post-hydrate refresh: %v", err)
	}
	if !jar2.LoggedIn() {
		t.Error("jar2 not logged in after hydrate+refresh")
	}

	// Clear wipes memory + store.
	if err := jar.clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if jar.LoggedIn() || jar.AccessToken() != "" {
		t.Error("jar still logged in after clear")
	}
	if _, ok := store.m["cloud_refresh:conn-1"]; ok {
		t.Error("refresh token not deleted from store after clear")
	}
}

func TestJar_RefreshUnauthorizedClears(t *testing.T) {
	// Server that always 401s the refresh → jar must clear itself.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	store := newFakeStore()
	store.m["cloud_refresh:conn-x"] = "stale"
	jar := newCloudTokenJar("conn-x", srv.URL, store)
	if _, err := jar.hydrate(); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	err := jar.refreshNow(context.Background())
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if jar.LoggedIn() {
		t.Error("jar should be cleared after 401 refresh")
	}
	if _, ok := store.m["cloud_refresh:conn-x"]; ok {
		t.Error("stale refresh token should be deleted after 401")
	}
}

func TestStripSetCookies(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "iterion_auth=jwt123; Path=/; HttpOnly")
	resp.Header.Add("Set-Cookie", "iterion_refresh=ref456; Path=/api/auth; HttpOnly")
	resp.Header.Add("Set-Cookie", "other=keepme; Path=/")

	stripSetCookies(resp, cloudAuthCookieName, cloudRefreshCookieName)

	got := resp.Header["Set-Cookie"]
	if len(got) != 1 || got[0] != "other=keepme; Path=/" {
		t.Errorf("Set-Cookie after strip = %v, want only [other=keepme; Path=/]", got)
	}

	// Stripping all cookies removes the header entirely.
	resp2 := &http.Response{Header: http.Header{}}
	resp2.Header.Add("Set-Cookie", "iterion_auth=x; Path=/")
	stripSetCookies(resp2, cloudAuthCookieName, cloudRefreshCookieName)
	if _, ok := resp2.Header["Set-Cookie"]; ok {
		t.Errorf("Set-Cookie header should be gone, got %v", resp2.Header["Set-Cookie"])
	}
}

func TestJar_ApplyRotation(t *testing.T) {
	store := newFakeStore()
	jar := newCloudTokenJar("conn-r", "https://c.io", store)

	exp := time.Now().Add(12 * time.Minute)
	access := mkJWT(exp.Unix())
	// Full rotation: both cookies present.
	if err := jar.applyRotation(access, "new-refresh"); err != nil {
		t.Fatalf("applyRotation: %v", err)
	}
	if jar.AccessToken() != access || !jar.LoggedIn() {
		t.Error("access token not applied")
	}
	if jar.Expiry().Unix() != exp.Unix() {
		t.Errorf("expiry = %v, want %v", jar.Expiry(), exp)
	}
	if store.m["cloud_refresh:conn-r"] != "new-refresh" {
		t.Errorf("refresh not persisted: %v", store.m)
	}

	// Partial rotation: only access cookie present → refresh unchanged.
	exp2 := time.Now().Add(20 * time.Minute)
	access2 := mkJWT(exp2.Unix())
	if err := jar.applyRotation(access2, ""); err != nil {
		t.Fatalf("applyRotation partial: %v", err)
	}
	if jar.AccessToken() != access2 {
		t.Error("access not updated on partial rotation")
	}
	if store.m["cloud_refresh:conn-r"] != "new-refresh" {
		t.Errorf("refresh should be unchanged on partial rotation: %v", store.m)
	}

	// Empty rotation is a no-op.
	if err := jar.applyRotation("", ""); err != nil {
		t.Fatalf("applyRotation empty: %v", err)
	}
	if jar.AccessToken() != access2 {
		t.Error("empty rotation must not change state")
	}
}

func TestCloudFetchServerInfo(t *testing.T) {
	f := &fakeCloud{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	info, err := cloudFetchServerInfo(context.Background(), newCloudHTTPClient(), srv.URL)
	if err != nil {
		t.Fatalf("server info: %v", err)
	}
	if info.Mode != "cloud" || !info.AuthRequired {
		t.Errorf("info = %+v, want cloud/auth_required", info)
	}
}
