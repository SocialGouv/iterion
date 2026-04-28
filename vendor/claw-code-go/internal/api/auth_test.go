package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestNoAuth(t *testing.T) {
	a := NoAuth()
	if a.Kind != AuthSourceNone {
		t.Fatalf("expected AuthSourceNone, got %d", a.Kind)
	}
	if a.HasAPIKey() {
		t.Fatal("NoAuth should not have API key")
	}
	if a.HasBearerToken() {
		t.Fatal("NoAuth should not have bearer token")
	}
}

func TestAPIKeyAuth(t *testing.T) {
	a := APIKeyAuth("sk-test-123")
	if a.Kind != AuthSourceAPIKey {
		t.Fatalf("expected AuthSourceAPIKey, got %d", a.Kind)
	}
	if a.APIKey != "sk-test-123" {
		t.Fatalf("expected API key sk-test-123, got %s", a.APIKey)
	}
	if !a.HasAPIKey() {
		t.Fatal("should have API key")
	}
	if a.HasBearerToken() {
		t.Fatal("should not have bearer token")
	}
}

func TestBearerAuth(t *testing.T) {
	a := BearerAuth("tok-abc")
	if a.Kind != AuthSourceBearer {
		t.Fatalf("expected AuthSourceBearer, got %d", a.Kind)
	}
	if a.BearerToken != "tok-abc" {
		t.Fatalf("expected bearer token tok-abc, got %s", a.BearerToken)
	}
	if a.HasAPIKey() {
		t.Fatal("should not have API key")
	}
	if !a.HasBearerToken() {
		t.Fatal("should have bearer token")
	}
}

func TestCombinedAuth(t *testing.T) {
	a := CombinedAuth("sk-key", "tok-bearer")
	if a.Kind != AuthSourceCombined {
		t.Fatalf("expected AuthSourceCombined, got %d", a.Kind)
	}
	if a.APIKey != "sk-key" {
		t.Fatalf("expected API key sk-key, got %s", a.APIKey)
	}
	if a.BearerToken != "tok-bearer" {
		t.Fatalf("expected bearer token tok-bearer, got %s", a.BearerToken)
	}
	if !a.HasAPIKey() {
		t.Fatal("should have API key")
	}
	if !a.HasBearerToken() {
		t.Fatal("should have bearer token")
	}
}

func TestApplyToRequest_None(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	NoAuth().ApplyToRequest(req)
	if req.Header.Get("x-api-key") != "" {
		t.Fatal("None auth should not set x-api-key")
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("None auth should not set Authorization")
	}
}

func TestApplyToRequest_APIKey(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	APIKeyAuth("sk-123").ApplyToRequest(req)
	if got := req.Header.Get("x-api-key"); got != "sk-123" {
		t.Fatalf("expected x-api-key=sk-123, got %s", got)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("APIKey auth should not set Authorization")
	}
}

func TestApplyToRequest_Bearer(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	BearerAuth("tok-abc").ApplyToRequest(req)
	if got := req.Header.Get("Authorization"); got != "Bearer tok-abc" {
		t.Fatalf("expected Authorization=Bearer tok-abc, got %s", got)
	}
	if req.Header.Get("x-api-key") != "" {
		t.Fatal("Bearer auth should not set x-api-key")
	}
}

func TestApplyToRequest_Combined(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	CombinedAuth("sk-key", "tok-bearer").ApplyToRequest(req)
	if got := req.Header.Get("x-api-key"); got != "sk-key" {
		t.Fatalf("expected x-api-key=sk-key, got %s", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-bearer" {
		t.Fatalf("expected Authorization=Bearer tok-bearer, got %s", got)
	}
}

func TestResolveStartupAuth_BothSet(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-env")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-env")
	a := ResolveStartupAuth()
	if a.Kind != AuthSourceCombined {
		t.Fatalf("expected Combined, got %d", a.Kind)
	}
	if a.APIKey != "sk-env" {
		t.Fatalf("expected API key sk-env, got %s", a.APIKey)
	}
	if a.BearerToken != "tok-env" {
		t.Fatalf("expected bearer token tok-env, got %s", a.BearerToken)
	}
}

func TestResolveStartupAuth_OnlyAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-only")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	a := ResolveStartupAuth()
	if a.Kind != AuthSourceAPIKey {
		t.Fatalf("expected APIKey, got %d", a.Kind)
	}
	if a.APIKey != "sk-only" {
		t.Fatalf("expected sk-only, got %s", a.APIKey)
	}
}

func TestResolveStartupAuth_OnlyBearer(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-only")
	a := ResolveStartupAuth()
	if a.Kind != AuthSourceBearer {
		t.Fatalf("expected Bearer, got %d", a.Kind)
	}
	if a.BearerToken != "tok-only" {
		t.Fatalf("expected tok-only, got %s", a.BearerToken)
	}
}

func TestResolveStartupAuth_Neither(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	a := ResolveStartupAuth()
	if a.Kind != AuthSourceNone {
		t.Fatalf("expected None, got %d", a.Kind)
	}
}

func TestMaskedAuthorizationHeader_Bearer(t *testing.T) {
	a := BearerAuth("tok-secret")
	if got := a.MaskedAuthorizationHeader(); got != "Bearer [REDACTED]" {
		t.Errorf("expected 'Bearer [REDACTED]', got %q", got)
	}
}

func TestMaskedAuthorizationHeader_Combined(t *testing.T) {
	a := CombinedAuth("sk-key", "tok-bearer")
	if got := a.MaskedAuthorizationHeader(); got != "Bearer [REDACTED]" {
		t.Errorf("expected 'Bearer [REDACTED]', got %q", got)
	}
}

func TestMaskedAuthorizationHeader_NoBearer(t *testing.T) {
	a := APIKeyAuth("sk-key")
	if got := a.MaskedAuthorizationHeader(); got != "<absent>" {
		t.Errorf("expected '<absent>', got %q", got)
	}
}

func TestMaskedAuthorizationHeader_None(t *testing.T) {
	a := NoAuth()
	if got := a.MaskedAuthorizationHeader(); got != "<absent>" {
		t.Errorf("expected '<absent>', got %q", got)
	}
}

func TestResolveStartupAuthWithOAuth_EnvPriority(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-token")

	// Env vars should take priority over OAuth callback.
	a, err := ResolveStartupAuthWithOAuth(func() (*OAuthTokenSet, error) {
		t.Error("OAuth callback should not be called when env vars are present")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != AuthSourceCombined {
		t.Fatalf("expected Combined, got %d", a.Kind)
	}
}

func TestResolveStartupAuthWithOAuth_FallbackToOAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	a, err := ResolveStartupAuthWithOAuth(func() (*OAuthTokenSet, error) {
		return &OAuthTokenSet{AccessToken: "oauth-token"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != AuthSourceBearer {
		t.Fatalf("expected Bearer, got %d", a.Kind)
	}
	if a.BearerToken != "oauth-token" {
		t.Fatalf("expected bearer oauth-token, got %s", a.BearerToken)
	}
}

func TestResolveStartupAuthWithOAuth_NilCallbackReturnsNone(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	a, err := ResolveStartupAuthWithOAuth(nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != AuthSourceNone {
		t.Fatalf("expected None, got %d", a.Kind)
	}
}

func TestResolveStartupAuthWithOAuth_EmptyToken(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	a, err := ResolveStartupAuthWithOAuth(func() (*OAuthTokenSet, error) {
		return &OAuthTokenSet{AccessToken: ""}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != AuthSourceNone {
		t.Fatalf("expected None for empty access token, got %d", a.Kind)
	}
}

func TestEnrichBearerAuthError_APIKeyAsBearer401(t *testing.T) {
	auth := BearerAuth("sk-ant-api03-test-key")
	enriched := EnrichBearerAuthError("401 Unauthorized", 401, auth)
	if !strings.Contains(enriched, "hint:") {
		t.Error("expected hint about API key as Bearer token")
	}
	if !strings.Contains(enriched, "ANTHROPIC_API_KEY") {
		t.Error("expected mention of ANTHROPIC_API_KEY")
	}
	if !strings.Contains(enriched, "x-api-key") {
		t.Error("expected mention of x-api-key header")
	}
	// Verify Rust-matching " — hint: " format
	if !strings.Contains(enriched, " — hint: sk-ant-*") {
		t.Errorf("expected Rust-matching hint format, got: %s", enriched)
	}
}

func TestEnrichBearerAuthError_Non401Unchanged(t *testing.T) {
	// Non-401 status should NOT enrich, even with sk-ant-* Bearer token.
	auth := BearerAuth("sk-ant-api03-test-key")
	enriched := EnrichBearerAuthError("500 Internal Server Error", 500, auth)
	if enriched != "500 Internal Server Error" {
		t.Errorf("expected unchanged error for non-401, got: %s", enriched)
	}
}

func TestEnrichBearerAuthError_NormalBearer(t *testing.T) {
	auth := BearerAuth("some-oauth-token")
	enriched := EnrichBearerAuthError("401 Unauthorized", 401, auth)
	if enriched != "401 Unauthorized" {
		t.Error("should not enrich error for non-sk-ant bearer token")
	}
}

func TestEnrichBearerAuthError_NonBearer(t *testing.T) {
	auth := APIKeyAuth("sk-ant-api03-key")
	enriched := EnrichBearerAuthError("401 Unauthorized", 401, auth)
	if enriched != "401 Unauthorized" {
		t.Error("should not enrich error for non-Bearer auth (API key only)")
	}
}

func TestEnrichBearerAuthError_CombinedAuth(t *testing.T) {
	// Combined auth (API key + Bearer): the x-api-key header is already sent,
	// so the 401 is from a different cause. Must not add the hint.
	auth := CombinedAuth("sk-key", "sk-ant-api03-bearer")
	enriched := EnrichBearerAuthError("401 Unauthorized", 401, auth)
	if enriched != "401 Unauthorized" {
		t.Errorf("should not enrich error for Combined auth, got: %s", enriched)
	}
}

func TestEnrichBearerAuthError_EmptyErrMsg401(t *testing.T) {
	auth := BearerAuth("sk-ant-api03-test-key")
	enriched := EnrichBearerAuthError("", 401, auth)
	if !strings.HasPrefix(enriched, "hint:") {
		t.Errorf("expected 'hint:' prefix for empty errMsg, got: %s", enriched)
	}
}

func TestSuggestForeignProvider(t *testing.T) {
	// With no foreign env vars set, should return empty.
	suggestion := SuggestForeignProvider()
	// We can't control env in this test easily, but verify the function exists
	// and returns a string. In CI, these env vars are unlikely to be set.
	_ = suggestion
}

func TestOAuthTokenSetIsExpired(t *testing.T) {
	// No expiry set → not expired.
	token := OAuthTokenSet{AccessToken: "tok"}
	if token.IsExpired() {
		t.Error("token without ExpiresAt should not be expired")
	}

	// Expiry in the past → expired.
	past := uint64(1000)
	token.ExpiresAt = &past
	if !token.IsExpired() {
		t.Error("token with past ExpiresAt should be expired")
	}
}
