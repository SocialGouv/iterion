package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestNewPKCE_ChallengeIsS256OfVerifier(t *testing.T) {
	verifier, challenge, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	if verifier == "" || challenge == "" {
		t.Fatalf("empty pkce: verifier=%q challenge=%q", verifier, challenge)
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("challenge mismatch: got %q want %q", challenge, want)
	}
	// Two calls must not collide.
	v2, _, _ := NewPKCE()
	if v2 == verifier {
		t.Fatal("verifier not random across calls")
	}
}

func TestAnthropicAuthorizeURL_Params(t *testing.T) {
	u := AnthropicAuthorizeURL("client-xyz", "https://example.test/cb", "chal-123", "state-abc")
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := parsed.Query()
	checks := map[string]string{
		"client_id":             "client-xyz",
		"response_type":         "code",
		"redirect_uri":          "https://example.test/cb",
		"code_challenge":        "chal-123",
		"code_challenge_method": "S256",
		"state":                 "state-abc",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("query %s: got %q want %q", k, got, want)
		}
	}
	if q.Get("scope") == "" {
		t.Error("scope must be present")
	}
}

func TestExchangeAnthropicCode_HappyPath(t *testing.T) {
	freshRetrySchedule(t)
	var gotForm map[string]string
	srv := newFakeOAuthServer(`{"access_token":"sk-ant-exchanged1234567890abcd","refresh_token":"rf-x","expires_in":3600,"scope":"user:inference"}`, http.StatusOK)
	defer srv.Close()
	// Wrap so we can capture the form the client sent.
	hc := &http.Client{Transport: &capturingTransport{target: srv.URL, capture: &gotForm}}

	res, err := ExchangeAnthropicCode(context.Background(), hc, "client-xyz", "the-code", "the-verifier", "https://example.test/cb", "state-abc")
	if err != nil {
		t.Fatalf("ExchangeAnthropicCode: %v", err)
	}
	if res.AccessToken != "sk-ant-exchanged1234567890abcd" {
		t.Errorf("access token: got %q", res.AccessToken)
	}
	if gotForm["grant_type"] != "authorization_code" {
		t.Errorf("grant_type: got %q", gotForm["grant_type"])
	}
	if gotForm["code"] != "the-code" {
		t.Errorf("code: got %q", gotForm["code"])
	}
	if gotForm["code_verifier"] != "the-verifier" {
		t.Errorf("code_verifier: got %q", gotForm["code_verifier"])
	}
}

// The auth-code exchange POSTs to the same token endpoint as the
// refresh, so an operator who re-points the deployment must move both
// halves of the flow with one variable — not the authorize URL alone.
func TestExchangeAnthropicCode_HonoursTokenURLOverride(t *testing.T) {
	freshRetrySchedule(t)
	srv := newFakeOAuthServer(`{"access_token":"sk-ant-exchanged1234567890abcd","refresh_token":"rf-x","expires_in":3600}`, http.StatusOK)
	defer srv.Close()
	t.Setenv("ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL", srv.URL+"/v1/oauth/token")

	_, err := ExchangeAnthropicCode(context.Background(), pinnedClient(t, srv.URL), "client-xyz", "the-code", "the-verifier", "https://example.test/cb", "state-abc")
	if err != nil {
		t.Fatalf("ExchangeAnthropicCode: %v", err)
	}
	if got := atomic.LoadInt32(&srv.hits); got != 1 {
		t.Fatalf("overridden endpoint hits = %d, want 1", got)
	}
}

func TestExchangeAnthropicCode_MissingArgs(t *testing.T) {
	if _, err := ExchangeAnthropicCode(context.Background(), nil, "", "code", "ver", "", ""); err == nil {
		t.Error("expected error for empty client id")
	}
	if _, err := ExchangeAnthropicCode(context.Background(), nil, "cid", "", "ver", "", ""); err == nil {
		t.Error("expected error for empty code")
	}
	if _, err := ExchangeAnthropicCode(context.Background(), nil, "cid", "code", "", "", ""); err == nil {
		t.Error("expected error for empty verifier")
	}
}

func TestBuildAnthropicCredentials_RoundTrips(t *testing.T) {
	blob, err := BuildAnthropicCredentials(RefreshResult{
		AccessToken:  "sk-ant-built1234567890abcdef",
		RefreshToken: "rf-built",
		Scopes:       []string{"user:inference"},
	})
	if err != nil {
		t.Fatalf("BuildAnthropicCredentials: %v", err)
	}
	v, err := ParseAnthropicView(blob)
	if err != nil {
		t.Fatalf("ParseAnthropicView: %v", err)
	}
	if v.ClaudeAIOauth.AccessToken != "sk-ant-built1234567890abcdef" {
		t.Errorf("access token round-trip: got %q", v.ClaudeAIOauth.AccessToken)
	}
	if v.ClaudeAIOauth.RefreshToken != "rf-built" {
		t.Errorf("refresh token round-trip: got %q", v.ClaudeAIOauth.RefreshToken)
	}
}

func TestSplitAnthropicCode(t *testing.T) {
	cases := []struct{ in, code, state string }{
		{"abc#xyz", "abc", "xyz"},
		{"  abc#xyz  ", "abc", "xyz"},
		{"justcode", "justcode", ""},
		{"abc#", "abc", ""},
	}
	for _, tc := range cases {
		code, state := SplitAnthropicCode(tc.in)
		if code != tc.code || state != tc.state {
			t.Errorf("Split(%q) = (%q,%q) want (%q,%q)", tc.in, code, state, tc.code, tc.state)
		}
	}
}

// capturingTransport rewrites the request to target and records the
// form body it carried (used to assert what the exchange client sent).
type capturingTransport struct {
	target  string
	capture *map[string]string
}

func (t *capturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil && t.capture != nil {
		if b, err := req.GetBody(); err == nil {
			buf := make([]byte, 4096)
			n, _ := b.Read(buf)
			vals, _ := url.ParseQuery(string(buf[:n]))
			m := map[string]string{}
			for k := range vals {
				m[k] = vals.Get(k)
			}
			*t.capture = m
		}
	}
	clone := req.Clone(req.Context())
	clone.URL, _ = clone.URL.Parse(t.target)
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}
