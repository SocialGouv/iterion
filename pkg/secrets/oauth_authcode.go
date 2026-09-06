package secrets

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Browser OAuth (authorization-code + PKCE) for the Claude Code
// subscription "forfait". This reproduces exactly what `claude login`
// does — same public client, same PKCE S256, same endpoints — but
// driven from the studio so a cloud operator never has to run the CLI
// in a pod nor paste a credentials.json file.
//
// Why the code-paste (headless) redirect and not a silent callback:
// the public Claude Code OAuth client only permits two redirect URIs —
// Anthropic's own code-display page, or a localhost loopback. A remote
// studio is neither, so it cannot register itself as a redirect target.
// The headless page shows the user a `code#state` string they paste
// back; iterion exchanges it server-side. This is the only flow that
// works from a cloud-hosted studio.
//
// All values are env-overridable because this is an undocumented,
// reverse-engineered surface: if Anthropic rotates the client/flow,
// an operator can re-point it without a rebuild. The family is
// ITERION_OAUTH_FORFAIT_ANTHROPIC_{CLIENT_ID,AUTHORIZE_URL,
// REDIRECT_URI,SCOPES,TOKEN_URL} — the token endpoint (shared with the
// refresh, pkg/secrets/oauth_refresh.go) included.
// DefaultAnthropicOAuthClientID is the public Claude Code OAuth client
// id. It is a PUBLIC PKCE client (no client secret), the same id the
// `claude` CLI embeds — not a confidential value. Shipping it as a
// default lets the browser connect flow work out of the box; an operator
// can still override it via ITERION_OAUTH_FORFAIT_ANTHROPIC_CLIENT_ID if
// Anthropic rotates the client.
const DefaultAnthropicOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

const (
	defaultAnthropicAuthorizeURL = "https://claude.ai/oauth/authorize"
	// The headless redirect that displays the code for copy/paste. This
	// is the ONLY redirect usable from a remote (non-loopback) studio.
	defaultAnthropicRedirectURI = "https://platform.claude.com/oauth/code/callback"
	defaultAnthropicScopes      = "user:profile user:inference user:sessions:claude_code user:mcp_servers"
)

// AnthropicAuthorizeURL builds the claude.ai authorization URL the
// studio opens in a new tab. challenge is the PKCE S256 challenge;
// state is round-tripped and validated on completion.
func AnthropicAuthorizeURL(clientID, redirectURI, challenge, state string) string {
	base := envOr("ITERION_OAUTH_FORFAIT_ANTHROPIC_AUTHORIZE_URL", defaultAnthropicAuthorizeURL)
	if redirectURI == "" {
		redirectURI = AnthropicRedirectURI()
	}
	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", envOr("ITERION_OAUTH_FORFAIT_ANTHROPIC_SCOPES", defaultAnthropicScopes))
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	return base + "?" + q.Encode()
}

// AnthropicRedirectURI returns the redirect URI used for the headless
// code-paste flow (env-overridable).
func AnthropicRedirectURI() string {
	return envOr("ITERION_OAUTH_FORFAIT_ANTHROPIC_REDIRECT_URI", defaultAnthropicRedirectURI)
}

// NewPKCE returns a fresh (verifier, challenge) pair. verifier is a
// 43-char base64url-encoded 32-byte random value; challenge is
// base64url(SHA256(verifier)), per RFC 7636 S256.
func NewPKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("secrets: pkce rand: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// NewOAuthState returns a random URL-safe state token for CSRF binding.
func NewOAuthState() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secrets: state rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// SplitAnthropicCode splits the `code#state` string Anthropic's headless
// page shows the user. When no `#` is present the whole input is the code
// and state is empty (the caller then relies on its server-side pending
// record for CSRF binding).
func SplitAnthropicCode(pasted string) (code, state string) {
	pasted = strings.TrimSpace(pasted)
	if i := strings.IndexByte(pasted, '#'); i >= 0 {
		return strings.TrimSpace(pasted[:i]), strings.TrimSpace(pasted[i+1:])
	}
	return pasted, ""
}

// ExchangeAnthropicCode trades an authorization code for tokens against
// the Anthropic OAuth token endpoint (grant_type=authorization_code).
// It mirrors RefreshAnthropic's request/response handling and reuses the
// same retry + validation primitives. redirectURI must match the one
// used to obtain the code; state is sent when non-empty.
func ExchangeAnthropicCode(ctx context.Context, hc *http.Client, clientID, code, verifier, redirectURI, state string) (RefreshResult, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if clientID == "" || code == "" || verifier == "" {
		return RefreshResult{}, fmt.Errorf("secrets: anthropic code exchange requires client_id + code + verifier")
	}
	if redirectURI == "" {
		redirectURI = AnthropicRedirectURI()
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)
	if state != "" {
		form.Set("state", state)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicTokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return RefreshResult{}, fmt.Errorf("secrets: build code-exchange req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := doWithRetry(hc, req, "anthropic code exchange")
	if err != nil {
		return RefreshResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return RefreshResult{}, fmt.Errorf("secrets: anthropic code exchange %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		TokenType    string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return RefreshResult{}, fmt.Errorf("secrets: decode anthropic code exchange: %w", err)
	}
	if err := validateAccessToken("anthropic", tok.AccessToken); err != nil {
		return RefreshResult{}, err
	}
	out := RefreshResult{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken}
	if tok.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC()
	}
	if tok.Scope != "" {
		out.Scopes = strings.Fields(tok.Scope)
	}
	return out, nil
}

// BuildAnthropicCredentials renders a fresh credentials.json blob (the
// `{claudeAiOauth:{…}}` shape the Claude Code CLI reads) from an
// exchange/refresh result, so the browser flow produces the exact same
// sealed payload the file-paste path would. Reuses ApplyAnthropicRefresh
// over an empty object — no duplicate serialisation.
func BuildAnthropicCredentials(r RefreshResult) ([]byte, error) {
	return ApplyAnthropicRefresh([]byte("{}"), r)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
