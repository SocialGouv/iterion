package mcp

import (
	"testing"
)

func TestNewOAuthBroker_DefaultPath(t *testing.T) {
	b, err := NewOAuthBroker("")
	if err != nil {
		t.Fatalf("NewOAuthBroker: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil broker")
	}
}

func TestNewOAuthBroker_StoreDir(t *testing.T) {
	dir := t.TempDir()
	b, err := NewOAuthBroker(dir)
	if err != nil {
		t.Fatalf("NewOAuthBroker(%q): %v", dir, err)
	}
	if b == nil {
		t.Fatal("expected non-nil broker")
	}
}

func TestAuthFuncFor_RequiresOAuth2(t *testing.T) {
	b, _ := NewOAuthBroker(t.TempDir())
	cfg := &AuthConfig{
		Type:     "bearer",
		AuthURL:  "https://example.com/auth",
		TokenURL: "https://example.com/token",
		ClientID: "abc",
	}
	if _, err := b.AuthFuncFor(cfg, "srv"); err == nil {
		t.Errorf("expected error for non-oauth2 type")
	}
}

func TestAuthFuncFor_RequiresEndpoints(t *testing.T) {
	b, _ := NewOAuthBroker(t.TempDir())
	cfg := &AuthConfig{Type: "oauth2", ClientID: "abc"} // missing URLs
	if _, err := b.AuthFuncFor(cfg, "srv"); err == nil {
		t.Errorf("expected error for missing AuthURL/TokenURL")
	}
}

func TestAuthFuncFor_NilConfig(t *testing.T) {
	b, _ := NewOAuthBroker(t.TempDir())
	if _, err := b.AuthFuncFor(nil, "srv"); err == nil {
		t.Errorf("expected error for nil cfg")
	}
}

func TestAuthFuncFor_ValidConfig(t *testing.T) {
	b, _ := NewOAuthBroker(t.TempDir())
	cfg := &AuthConfig{
		Type:     "oauth2",
		AuthURL:  "https://example.com/auth",
		TokenURL: "https://example.com/token",
		ClientID: "abc",
		Scopes:   []string{"read", "write"},
	}
	fn, err := b.AuthFuncFor(cfg, "srv")
	if err != nil {
		t.Fatalf("AuthFuncFor: %v", err)
	}
	if fn == nil {
		t.Errorf("expected non-nil closure")
	}
	// Don't actually call fn() — that would block on browser opener.
}

func TestPrepareAuth_NilBrokerNoop(t *testing.T) {
	cat := map[string]*ServerConfig{
		"a": {Name: "a", Auth: &AuthConfig{Type: "oauth2", AuthURL: "x", TokenURL: "y", ClientID: "z"}},
	}
	if err := PrepareAuth(cat, nil); err != nil {
		t.Errorf("nil broker should be no-op, got %v", err)
	}
	if cat["a"].AuthFunc != nil {
		t.Errorf("AuthFunc should remain nil under no-op broker")
	}
}

func TestPrepareAuth_PopulatesAuthFunc(t *testing.T) {
	b, _ := NewOAuthBroker(t.TempDir())
	cat := map[string]*ServerConfig{
		"a": {Name: "a", Auth: &AuthConfig{
			Type:     "oauth2",
			AuthURL:  "https://example.com/auth",
			TokenURL: "https://example.com/token",
			ClientID: "abc",
		}},
		"b": {Name: "b"}, // no auth → unchanged
	}
	if err := PrepareAuth(cat, b); err != nil {
		t.Fatalf("PrepareAuth: %v", err)
	}
	if cat["a"].AuthFunc == nil {
		t.Errorf("server a should have AuthFunc populated")
	}
	if cat["b"].AuthFunc != nil {
		t.Errorf("server b without Auth should remain unchanged")
	}
}

func TestPrepareAuth_SurfacesMalformedAuth(t *testing.T) {
	b, _ := NewOAuthBroker(t.TempDir())
	cat := map[string]*ServerConfig{
		"a": {Name: "a", Auth: &AuthConfig{Type: "oauth2", ClientID: "abc"}}, // missing URLs
	}
	if err := PrepareAuth(cat, b); err == nil {
		t.Errorf("expected error for malformed Auth, got nil")
	}
}

func TestAuthFuncFor_RejectsPlainHTTP(t *testing.T) {
	b, _ := NewOAuthBroker(t.TempDir())
	cfg := &AuthConfig{
		Type:     "oauth2",
		AuthURL:  "http://attacker.example/auth", // non-loopback http://
		TokenURL: "https://example.com/token",
		ClientID: "abc",
	}
	if _, err := b.AuthFuncFor(cfg, "srv"); err == nil {
		t.Fatal("expected error for non-loopback http:// AuthURL")
	}
}

func TestAuthFuncFor_AllowsLocalhostHTTP(t *testing.T) {
	b, _ := NewOAuthBroker(t.TempDir())
	for _, host := range []string{"http://localhost:8080/auth", "http://127.0.0.1/auth", "http://[::1]:9000/auth"} {
		cfg := &AuthConfig{
			Type:     "oauth2",
			AuthURL:  host,
			TokenURL: "https://example.com/token",
			ClientID: "abc",
		}
		if _, err := b.AuthFuncFor(cfg, "srv"); err != nil {
			t.Errorf("AuthFuncFor(%q): unexpected error %v", host, err)
		}
	}
}

func TestAuthFuncFor_RejectsBogusScheme(t *testing.T) {
	b, _ := NewOAuthBroker(t.TempDir())
	cfg := &AuthConfig{
		Type:     "oauth2",
		AuthURL:  "ftp://example.com/auth",
		TokenURL: "https://example.com/token",
		ClientID: "abc",
	}
	if _, err := b.AuthFuncFor(cfg, "srv"); err == nil {
		t.Fatal("expected error for unsupported URL scheme")
	}
}
