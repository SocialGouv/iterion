package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAuthServer simulates the authorization endpoint. The handler
// records the latest authorization request, lets tests grab the state
// + redirect_uri, and replies with a 302 to the callback URL.
type fakeAuthServer struct {
	*httptest.Server
	lastReq atomic.Value // map[string]string
}

func newFakeAuthServer() *fakeAuthServer {
	f := &fakeAuthServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		params := map[string]string{}
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				params[k] = v[0]
			}
		}
		f.lastReq.Store(params)
		// We don't actually redirect — tests drive the callback themselves
		// to keep the flow deterministic. Just acknowledge.
		fmt.Fprintln(w, "ok")
	}))
	return f
}

func (f *fakeAuthServer) Params() map[string]string {
	if v := f.lastReq.Load(); v != nil {
		return v.(map[string]string)
	}
	return nil
}

// fakeTokenServer issues tokens. Each test configures the response
// body. exchanges accumulates posted forms for assertion.
type fakeTokenServer struct {
	*httptest.Server
	exchanges []url.Values
	response  func(form url.Values) (int, tokenResponse)
}

func newFakeTokenServer() *fakeTokenServer {
	f := &fakeTokenServer{
		response: func(_ url.Values) (int, tokenResponse) {
			return http.StatusOK, tokenResponse{
				AccessToken:  "fresh-access",
				RefreshToken: "fresh-refresh",
				TokenType:    "Bearer",
				ExpiresIn:    3600,
				Scope:        "read",
			}
		},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.exchanges = append(f.exchanges, r.PostForm)
		status, tr := f.response(r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(tr)
	}))
	return f
}

func TestBroker_AuthCodeFlow_HappyPath(t *testing.T) {
	authSrv := newFakeAuthServer()
	defer authSrv.Close()
	tokSrv := newFakeTokenServer()
	defer tokSrv.Close()

	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))

	openerCalled := make(chan struct{}, 1)
	opener := func(authURL string) error {
		// Parse the URL and extract the redirect_uri + state, then
		// drive the callback ourselves. This mimics what a real
		// browser would do once the user authorizes.
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		go func() {
			cb, _ := url.Parse(q.Get("redirect_uri"))
			cbq := url.Values{}
			cbq.Set("code", "the-code")
			cbq.Set("state", q.Get("state"))
			cb.RawQuery = cbq.Encode()
			_, _ = http.Get(cb.String())
		}()
		openerCalled <- struct{}{}
		return nil
	}

	b := NewBroker(
		WithStorage(storage),
		WithHTTPClient(tokSrv.Client()),
		WithAuthOpener(opener),
		WithCallbackTimeout(3*time.Second),
		WithClock(func() time.Time { return time.Unix(1700000000, 0) }),
	)

	cfg := ServerConfig{
		Name:     "github",
		AuthURL:  authSrv.URL + "/authorize",
		TokenURL: tokSrv.URL,
		ClientID: "client-123",
		Scopes:   []string{"read", "write"},
	}

	tok, err := b.Acquire(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	<-openerCalled
	if tok != "fresh-access" {
		t.Errorf("expected fresh-access token, got %q", tok)
	}

	// Storage must contain the persisted token.
	stored, ok, err := storage.Load("github")
	if err != nil || !ok {
		t.Fatalf("expected stored token, ok=%v err=%v", ok, err)
	}
	if stored.RefreshToken != "fresh-refresh" {
		t.Errorf("refresh token not persisted: %q", stored.RefreshToken)
	}
	if stored.ExpiresAt.IsZero() {
		t.Errorf("expected ExpiresAt set from expires_in")
	}

	// Token endpoint must have received PKCE verifier.
	if len(tokSrv.exchanges) != 1 {
		t.Fatalf("expected 1 token exchange, got %d", len(tokSrv.exchanges))
	}
	form := tokSrv.exchanges[0]
	if form.Get("grant_type") != "authorization_code" {
		t.Errorf("expected grant_type=authorization_code, got %q", form.Get("grant_type"))
	}
	if form.Get("code") != "the-code" {
		t.Errorf("expected code=the-code, got %q", form.Get("code"))
	}
	if form.Get("code_verifier") == "" {
		t.Errorf("missing code_verifier in token request")
	}
}

func TestBroker_StateMismatch_Rejects(t *testing.T) {
	authSrv := newFakeAuthServer()
	defer authSrv.Close()
	tokSrv := newFakeTokenServer()
	defer tokSrv.Close()

	opener := func(authURL string) error {
		u, _ := url.Parse(authURL)
		q := u.Query()
		go func() {
			cb, _ := url.Parse(q.Get("redirect_uri"))
			cbq := url.Values{}
			cbq.Set("code", "the-code")
			cbq.Set("state", "WRONG-STATE")
			cb.RawQuery = cbq.Encode()
			_, _ = http.Get(cb.String())
		}()
		return nil
	}

	b := NewBroker(
		WithHTTPClient(tokSrv.Client()),
		WithAuthOpener(opener),
		WithCallbackTimeout(2*time.Second),
	)
	cfg := ServerConfig{
		Name:     "github",
		AuthURL:  authSrv.URL + "/authorize",
		TokenURL: tokSrv.URL,
		ClientID: "client-123",
	}
	if _, err := b.Acquire(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("expected state mismatch error, got %v", err)
	}
}

func TestBroker_RejectsIncompleteConfig(t *testing.T) {
	b := NewBroker()
	if _, err := b.Acquire(context.Background(), ServerConfig{}); err == nil {
		t.Fatal("expected error on empty config")
	}
}

func TestBroker_AcquireUsesCachedTokenWhenFresh(t *testing.T) {
	tokSrv := newFakeTokenServer()
	defer tokSrv.Close()

	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	_ = storage.Save("github", Token{
		AccessToken: "cached-access",
		ExpiresAt:   time.Now().Add(time.Hour),
	})

	b := NewBroker(
		WithStorage(storage),
		WithHTTPClient(tokSrv.Client()),
	)
	cfg := ServerConfig{
		Name:     "github",
		AuthURL:  "http://nowhere/authorize",
		TokenURL: tokSrv.URL,
		ClientID: "client-123",
	}
	tok, err := b.Acquire(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if tok != "cached-access" {
		t.Errorf("expected cached token, got %q", tok)
	}
	if len(tokSrv.exchanges) != 0 {
		t.Errorf("expected no token endpoint hits when cache is fresh, got %d", len(tokSrv.exchanges))
	}
}

func TestBroker_RefreshOnExpiringToken(t *testing.T) {
	tokSrv := newFakeTokenServer()
	defer tokSrv.Close()

	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	_ = storage.Save("github", Token{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(10 * time.Second), // within 30s skew
	})

	b := NewBroker(
		WithStorage(storage),
		WithHTTPClient(tokSrv.Client()),
	)
	cfg := ServerConfig{
		Name:     "github",
		AuthURL:  "http://nowhere/authorize",
		TokenURL: tokSrv.URL,
		ClientID: "client-123",
	}
	tok, err := b.Acquire(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if tok != "fresh-access" {
		t.Errorf("expected refreshed token, got %q", tok)
	}
	if len(tokSrv.exchanges) != 1 {
		t.Fatalf("expected 1 refresh exchange, got %d", len(tokSrv.exchanges))
	}
	form := tokSrv.exchanges[0]
	if form.Get("grant_type") != "refresh_token" {
		t.Errorf("expected grant_type=refresh_token, got %q", form.Get("grant_type"))
	}
	if form.Get("refresh_token") != "old-refresh" {
		t.Errorf("expected refresh_token=old-refresh, got %q", form.Get("refresh_token"))
	}
}

func TestBroker_RefreshPreservesOldTokenWhenNoneReturned(t *testing.T) {
	// RFC 6749 §6: refresh response MAY omit a new refresh_token. The
	// broker must keep the existing one in that case so the next
	// refresh still works.
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "rotated-access",
			TokenType:   "Bearer",
			ExpiresIn:   3600,
			// No refresh_token in response.
		})
	}))
	defer tokSrv.Close()

	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	_ = storage.Save("github", Token{
		AccessToken:  "expiring",
		RefreshToken: "long-lived-refresh",
		ExpiresAt:    time.Now().Add(5 * time.Second),
	})

	b := NewBroker(WithStorage(storage), WithHTTPClient(tokSrv.Client()))
	tok, err := b.Acquire(context.Background(), ServerConfig{
		Name:     "github",
		AuthURL:  "http://nowhere/authorize",
		TokenURL: tokSrv.URL,
		ClientID: "client-123",
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if tok != "rotated-access" {
		t.Errorf("expected rotated-access, got %q", tok)
	}
	stored, _, _ := storage.Load("github")
	if stored.RefreshToken != "long-lived-refresh" {
		t.Errorf("expected old refresh_token preserved, got %q", stored.RefreshToken)
	}
}

func TestBroker_RefreshPropagatesNewRotatedToken(t *testing.T) {
	// Counterpart: when the AS rotates the refresh_token, the broker
	// must persist the new one.
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "new-access",
			RefreshToken: "rotated-refresh",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
		})
	}))
	defer tokSrv.Close()

	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	_ = storage.Save("github", Token{
		AccessToken:  "expiring",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(5 * time.Second),
	})

	b := NewBroker(WithStorage(storage), WithHTTPClient(tokSrv.Client()))
	if _, err := b.Acquire(context.Background(), ServerConfig{
		Name: "github", AuthURL: "http://nowhere", TokenURL: tokSrv.URL, ClientID: "c",
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	stored, _, _ := storage.Load("github")
	if stored.RefreshToken != "rotated-refresh" {
		t.Errorf("expected rotated refresh persisted, got %q", stored.RefreshToken)
	}
}

func TestBroker_TokenEndpointRejectsWithStructuredError(t *testing.T) {
	// RFC 6749 §5.2 says error responses use status 400 + a JSON
	// body with "error" and "error_description". Make sure we surface
	// both fields rather than dropping context.
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(tokenResponse{
			Error:     "invalid_grant",
			ErrorDesc: "The refresh token is malformed",
		})
	}))
	defer tokSrv.Close()

	b := NewBroker(WithHTTPClient(tokSrv.Client()))
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	_, err := b.postTokenForm(context.Background(), tokSrv.URL, form)
	if err == nil {
		t.Fatal("expected error from rejected exchange")
	}
	if !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("expected error to surface RFC 6749 error+desc, got %v", err)
	}
}

func TestBroker_TokenEndpointRejectsWithGarbageBody(t *testing.T) {
	// Some misconfigured providers return HTML error pages on 5xx.
	// We must not panic on JSON decode and must include the status.
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("<html><body>500 Server Error</body></html>"))
	}))
	defer tokSrv.Close()

	b := NewBroker(WithHTTPClient(tokSrv.Client()))
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	_, err := b.postTokenForm(context.Background(), tokSrv.URL, form)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error mentioning 500, got %v", err)
	}
}

func TestBroker_AcquireRejectsExpiredCachedTokenWithoutRefresh(t *testing.T) {
	// When the cached token is expired AND no refresh_token is
	// present, Acquire must fall through to a fresh interactive flow.
	// In a unit test we can't run the browser, so we expect
	// authOpener to be invoked.
	tokSrv := newFakeTokenServer()
	defer tokSrv.Close()

	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	_ = storage.Save("github", Token{
		AccessToken:  "expired",
		RefreshToken: "", // No refresh available
		ExpiresAt:    time.Now().Add(-1 * time.Hour),
	})

	openerCalled := make(chan struct{}, 1)
	opener := func(authURL string) error {
		openerCalled <- struct{}{}
		return errors.New("user cancelled")
	}

	b := NewBroker(
		WithStorage(storage),
		WithHTTPClient(tokSrv.Client()),
		WithAuthOpener(opener),
		WithCallbackTimeout(100*time.Millisecond),
	)
	_, err := b.Acquire(context.Background(), ServerConfig{
		Name:     "github",
		AuthURL:  "http://nowhere/authorize",
		TokenURL: tokSrv.URL,
		ClientID: "c",
	})
	if err == nil {
		t.Fatal("expected error when interactive flow is short-circuited")
	}
	select {
	case <-openerCalled:
		// Good — we did fall through to the interactive path.
	case <-time.After(time.Second):
		t.Fatal("authOpener was never invoked; expired-no-refresh token didn't fall through")
	}
}

func TestBroker_RevokeStillClearsLocalOnRemoteFailure(t *testing.T) {
	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	_ = storage.Save("github", Token{AccessToken: "x"})

	revokeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer revokeSrv.Close()

	b := NewBroker(WithStorage(storage), WithHTTPClient(revokeSrv.Client()))
	cfg := ServerConfig{Name: "github", RevokeURL: revokeSrv.URL, ClientID: "c"}
	err := b.Revoke(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400 error, got %v", err)
	}
	// Critical invariant: local cache must be cleared even when the
	// revoke endpoint rejects the request, so callers can re-Acquire.
	if _, ok, _ := storage.Load("github"); ok {
		t.Errorf("expected token cleared from local cache despite revoke 4xx")
	}
}

func TestBroker_AcquireNoninteractive_NoCacheReturnsReauthRequired(t *testing.T) {
	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	b := NewBroker(WithStorage(storage))
	_, err := b.AcquireNoninteractive(context.Background(), ServerConfig{
		Name: "github", TokenURL: "http://nowhere", ClientID: "c",
	})
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("expected ErrReauthRequired on empty cache, got %v", err)
	}
}

func TestBroker_AcquireNoninteractive_FreshCacheReturnsToken(t *testing.T) {
	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	_ = storage.Save("github", Token{
		AccessToken: "still-good",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	b := NewBroker(WithStorage(storage))
	tok, err := b.AcquireNoninteractive(context.Background(), ServerConfig{
		Name: "github", TokenURL: "http://nowhere", ClientID: "c",
	})
	if err != nil {
		t.Fatalf("AcquireNoninteractive: %v", err)
	}
	if tok != "still-good" {
		t.Errorf("expected still-good, got %q", tok)
	}
}

func TestBroker_AcquireNoninteractive_ExpiredWithoutRefreshIsReauth(t *testing.T) {
	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	_ = storage.Save("github", Token{
		AccessToken:  "expired",
		RefreshToken: "",
		ExpiresAt:    time.Now().Add(-time.Hour),
	})
	b := NewBroker(WithStorage(storage))
	_, err := b.AcquireNoninteractive(context.Background(), ServerConfig{
		Name: "github", TokenURL: "http://nowhere", ClientID: "c",
	})
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("expected ErrReauthRequired, got %v", err)
	}
}

func TestBroker_AcquireNoninteractive_InvalidGrantSurfacesAsReauth(t *testing.T) {
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(tokenResponse{
			Error:     "invalid_grant",
			ErrorDesc: "refresh token expired",
		})
	}))
	defer tokSrv.Close()

	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	_ = storage.Save("github", Token{
		AccessToken:  "expiring",
		RefreshToken: "stale-refresh",
		ExpiresAt:    time.Now().Add(5 * time.Second),
	})
	b := NewBroker(WithStorage(storage), WithHTTPClient(tokSrv.Client()))
	_, err := b.AcquireNoninteractive(context.Background(), ServerConfig{
		Name: "github", TokenURL: tokSrv.URL, ClientID: "c",
	})
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("expected invalid_grant to surface as ErrReauthRequired, got %v", err)
	}
}

func TestBroker_AcquireNoninteractive_TransientErrorIsNotReauth(t *testing.T) {
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer tokSrv.Close()

	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	_ = storage.Save("github", Token{
		AccessToken:  "expiring",
		RefreshToken: "ok-refresh",
		ExpiresAt:    time.Now().Add(5 * time.Second),
	})
	b := NewBroker(WithStorage(storage), WithHTTPClient(tokSrv.Client()))
	_, err := b.AcquireNoninteractive(context.Background(), ServerConfig{
		Name: "github", TokenURL: tokSrv.URL, ClientID: "c",
	})
	if err == nil {
		t.Fatal("expected error from 500")
	}
	if errors.Is(err, ErrReauthRequired) {
		t.Errorf("transient 5xx must NOT be classified as ErrReauthRequired (got %v)", err)
	}
}

func TestBroker_RevokeDropsToken(t *testing.T) {
	storage := NewStorage(filepath.Join(t.TempDir(), "tokens.json"))
	_ = storage.Save("github", Token{AccessToken: "x"})

	revoked := atomic.Bool{}
	revokeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("token") == "x" {
			revoked.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer revokeSrv.Close()

	b := NewBroker(WithStorage(storage), WithHTTPClient(revokeSrv.Client()))
	cfg := ServerConfig{Name: "github", RevokeURL: revokeSrv.URL, ClientID: "c"}
	if err := b.Revoke(context.Background(), cfg); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !revoked.Load() {
		t.Errorf("expected revocation endpoint to receive the token")
	}
	if _, ok, _ := storage.Load("github"); ok {
		t.Errorf("expected token cache cleared after revoke")
	}
}
