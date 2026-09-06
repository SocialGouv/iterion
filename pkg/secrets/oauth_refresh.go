package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Anthropic / Codex token endpoints. These are the public OAuth
// surfaces the CLIs themselves consume on refresh; iterion drives
// the same request shape so a stored credentials.json can be
// rotated server-side before its access_token expires.
//
// The values are the documented endpoints at the time of writing
// (2026-05).
const (
	defaultAnthropicTokenURL = "https://console.anthropic.com/v1/oauth/token"
	defaultCodexTokenURL     = "https://auth.openai.com/oauth/token"
)

// anthropicTokenURL is the endpoint both halves of the Anthropic flow
// POST to — the auth-code exchange and the server-side refresh. It goes
// through envOr like its three siblings (authorize URL, redirect URI,
// scopes) so an OEM-repackaged CLI or a proxying deployment moves the
// whole flow, not three quarters of it.
func anthropicTokenURL() string {
	return envOr("ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL", defaultAnthropicTokenURL)
}

// codexTokenURL is the Codex half of the same override family.
func codexTokenURL() string {
	return envOr("ITERION_OAUTH_FORFAIT_CODEX_TOKEN_URL", defaultCodexTokenURL)
}

// ErrNotRefreshable marks a credential whose sealed payload carries no
// refresh token: no refresh exchange can ever succeed for it, so callers
// must skip it (worker) or tell the user to re-connect (HTTP) instead of
// retrying forever.
var ErrNotRefreshable = errors.New("secrets: credential has no refresh token")

// RefreshResult carries the bits a successful refresh produces.
// Pass them through ApplyAnthropicRefresh / ApplyCodexRefresh to
// rebuild the credentials JSON the CLI expects.
type RefreshResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scopes       []string
	IDToken      string
}

// RefreshAnthropic exchanges a refresh_token for a new access_token
// against the Anthropic OAuth endpoint. clientID is provided per
// deployment (the publicly-known Claude Code OAuth client).
func RefreshAnthropic(ctx context.Context, hc *http.Client, clientID, refreshToken string) (RefreshResult, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if refreshToken == "" {
		return RefreshResult{}, fmt.Errorf("anthropic refresh: %w", ErrNotRefreshable)
	}
	if clientID == "" {
		return RefreshResult{}, fmt.Errorf("secrets: anthropic refresh requires client_id")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicTokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return RefreshResult{}, fmt.Errorf("secrets: build refresh req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := doWithRetry(hc, req, "anthropic refresh")
	if err != nil {
		return RefreshResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return RefreshResult{}, fmt.Errorf("secrets: anthropic refresh %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		TokenType    string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return RefreshResult{}, fmt.Errorf("secrets: decode anthropic refresh: %w", err)
	}
	if err := validateAccessToken("anthropic", tok.AccessToken); err != nil {
		return RefreshResult{}, err
	}
	out := RefreshResult{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
	}
	if out.RefreshToken == "" {
		// Some servers omit refresh_token on refresh and expect the
		// caller to keep using the old one.
		out.RefreshToken = refreshToken
	}
	if tok.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC()
	}
	if tok.Scope != "" {
		out.Scopes = strings.Fields(tok.Scope)
	}
	return out, nil
}

// ApplyAnthropicRefresh updates a credentials.json blob with fresh
// tokens. Returns the new JSON to seal back into the OAuthRecord.
func ApplyAnthropicRefresh(payload []byte, r RefreshResult) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("secrets: parse credentials.json: %w", err)
	}
	if raw == nil {
		// A literal JSON `null` (or an empty payload) unmarshals into a nil
		// map *without* an error; the `raw[...] = ...` writes below would
		// then panic with "assignment to entry in nil map". Treat it as
		// corrupt input and fail gracefully instead of crashing the caller.
		return nil, fmt.Errorf("secrets: parse credentials.json: empty or null payload")
	}
	inner, ok := raw["claudeAiOauth"].(map[string]any)
	if !ok {
		inner = map[string]any{}
	}
	inner["accessToken"] = r.AccessToken
	if r.RefreshToken != "" {
		inner["refreshToken"] = r.RefreshToken
	}
	if !r.ExpiresAt.IsZero() {
		inner["expiresAt"] = r.ExpiresAt.UnixMilli()
	}
	if len(r.Scopes) > 0 {
		inner["scopes"] = r.Scopes
	}
	raw["claudeAiOauth"] = inner
	return json.MarshalIndent(raw, "", "  ")
}

// RefreshRecord drives the refresh exchange for one OAuthRecord and
// rewrites its sealed payload in place with the new tokens. It is the
// single refresh primitive shared by the HTTP handler (manual refresh)
// and the background OAuthRefreshWorker — keep them on this function so
// the two paths can never drift.
//
// The (userID, kind) AAD is derived from rec, so an org-scoped record
// (rec.UserID == OrgOwnerKey(tenantID)) refreshes identically to a
// personal one. Returns an error when the provider rejects the refresh
// or no client_id is configured for the record's kind.
func RefreshRecord(ctx context.Context, sealer Sealer, hc *http.Client, anthropicClientID, codexClientID string, rec *OAuthRecord) error {
	if rec == nil {
		return fmt.Errorf("secrets: RefreshRecord nil record")
	}
	payload, err := OpenOAuthPayload(sealer, rec.UserID, rec.Kind, rec.SealedPayload)
	if err != nil {
		return fmt.Errorf("secrets: unseal: %w", err)
	}
	// Legacy records predate the subscription fingerprint: stamp it from
	// the CURRENT payload once — every later refresh preserves it, so the
	// identity is stable from here on. Records stamped at connect time
	// are left untouched: a refresh is the same subscription. Same
	// derivation as the connect path, so a self-healed record and a
	// re-connected one land on the SAME meter wherever the payload names
	// an account.
	if rec.Fingerprint == "" {
		rec.Fingerprint = SubscriptionFingerprint(rec.Kind, payload)
	}
	now := time.Now().UTC()
	switch rec.Kind {
	case OAuthKindClaudeCode:
		view, perr := ParseAnthropicView(payload)
		if perr != nil {
			return perr
		}
		if strings.TrimSpace(view.ClaudeAIOauth.RefreshToken) == "" {
			return fmt.Errorf("anthropic: %w", ErrNotRefreshable)
		}
		clientID := strings.TrimSpace(anthropicClientID)
		if clientID == "" {
			return fmt.Errorf("secrets: anthropic oauth client id not configured")
		}
		res, rerr := RefreshAnthropic(ctx, hc, clientID, view.ClaudeAIOauth.RefreshToken)
		if rerr != nil {
			return rerr
		}
		updated, uerr := ApplyAnthropicRefresh(payload, res)
		if uerr != nil {
			return uerr
		}
		sealed, serr := SealOAuthPayload(sealer, rec.UserID, rec.Kind, updated)
		if serr != nil {
			return serr
		}
		rec.SealedPayload = sealed
		if !res.ExpiresAt.IsZero() {
			t := res.ExpiresAt
			rec.AccessTokenExpiresAt = &t
		}
		if len(res.Scopes) > 0 {
			rec.Scopes = res.Scopes
		}
	case OAuthKindCodex:
		view, perr := ParseCodexView(payload)
		if perr != nil {
			return perr
		}
		if strings.TrimSpace(view.Tokens.RefreshToken) == "" {
			return fmt.Errorf("codex: %w", ErrNotRefreshable)
		}
		clientID := strings.TrimSpace(codexClientID)
		if clientID == "" {
			return fmt.Errorf("secrets: codex oauth client id not configured")
		}
		res, rerr := RefreshCodex(ctx, hc, clientID, view.Tokens.RefreshToken)
		if rerr != nil {
			return rerr
		}
		updated, uerr := ApplyCodexRefresh(payload, res)
		if uerr != nil {
			return uerr
		}
		sealed, serr := SealOAuthPayload(sealer, rec.UserID, rec.Kind, updated)
		if serr != nil {
			return serr
		}
		rec.SealedPayload = sealed
		if !res.ExpiresAt.IsZero() {
			t := res.ExpiresAt
			rec.AccessTokenExpiresAt = &t
		}
	default:
		return fmt.Errorf("secrets: RefreshRecord unsupported kind %q", rec.Kind)
	}
	rec.LastRefreshedAt = &now
	rec.UpdatedAt = now
	return nil
}

// RefreshCodex mirrors RefreshAnthropic for the OpenAI Codex CLI.
// clientID is the Codex CLI's published OAuth client; deployments
// using a custom Codex fork override it.
func RefreshCodex(ctx context.Context, hc *http.Client, clientID, refreshToken string) (RefreshResult, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if refreshToken == "" {
		return RefreshResult{}, fmt.Errorf("codex refresh: %w", ErrNotRefreshable)
	}
	if clientID == "" {
		return RefreshResult{}, fmt.Errorf("secrets: codex refresh requires client_id")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return RefreshResult{}, fmt.Errorf("secrets: build codex refresh req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := doWithRetry(hc, req, "codex refresh")
	if err != nil {
		return RefreshResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return RefreshResult{}, fmt.Errorf("secrets: codex refresh %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return RefreshResult{}, fmt.Errorf("secrets: decode codex refresh: %w", err)
	}
	if err := validateAccessToken("codex", tok.AccessToken); err != nil {
		return RefreshResult{}, err
	}
	out := RefreshResult{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		IDToken:      tok.IDToken,
	}
	if out.RefreshToken == "" {
		out.RefreshToken = refreshToken
	}
	if tok.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC()
	}
	if tok.Scope != "" {
		out.Scopes = strings.Fields(tok.Scope)
	}
	return out, nil
}

// ApplyCodexRefresh updates an auth.json blob with fresh tokens.
func ApplyCodexRefresh(payload []byte, r RefreshResult) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("secrets: parse auth.json: %w", err)
	}
	if raw == nil {
		// A literal JSON `null` (or an empty payload) unmarshals into a nil
		// map *without* an error; the `raw[...] = ...` writes below would
		// then panic with "assignment to entry in nil map". Treat it as
		// corrupt input and fail gracefully instead of crashing the caller.
		return nil, fmt.Errorf("secrets: parse auth.json: empty or null payload")
	}
	tokens, ok := raw["tokens"].(map[string]any)
	if !ok {
		tokens = map[string]any{}
	}
	tokens["access_token"] = r.AccessToken
	if r.RefreshToken != "" {
		tokens["refresh_token"] = r.RefreshToken
	}
	if r.IDToken != "" {
		tokens["id_token"] = r.IDToken
	}
	raw["tokens"] = tokens
	raw["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	return json.MarshalIndent(raw, "", "  ")
}

// refreshRetrySchedule defines the per-attempt backoff for OAuth
// refresh on transient failures. Three attempts total (0/200ms/600ms);
// total wall-clock ceiling is ~800ms so the studio stays responsive
// when the IdP is briefly flaky. Only 5xx and connection-level errors
// retry — 4xx responses (invalid_grant, unauthorized_client, …) are
// terminal and propagated immediately.
var refreshRetrySchedule = []time.Duration{0, 200 * time.Millisecond, 600 * time.Millisecond}

func doWithRetry(hc *http.Client, req *http.Request, opName string) (*http.Response, error) {
	var (
		lastResp *http.Response
		lastErr  error
	)
	// closeLast drains the response we're abandoning before moving on.
	// A 5xx attempt yields a non-nil *http.Response whose Body must be
	// closed or its TCP connection is pinned until GC — leaking a
	// connection/fd on every retry-after-5xx. We must close it on EVERY
	// path that abandons it: the next retry, the early success return,
	// the body-clone-error return, the cancellation return, and the
	// final error return.
	closeLast := func() {
		if lastResp != nil {
			lastResp.Body.Close()
			lastResp = nil
		}
	}
	for i, delay := range refreshRetrySchedule {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-req.Context().Done():
				closeLast()
				return nil, fmt.Errorf("secrets: %s cancelled: %w", opName, req.Context().Err())
			}
		}
		// Make sure the body is replayable across retries — OAuth
		// refresh bodies are small form-encoded payloads stored in
		// the Request via NewRequestWithContext(..., strings.NewReader),
		// which sets GetBody. Calling it on attempt 0 is a no-op clone.
		if i > 0 && req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				closeLast()
				return nil, fmt.Errorf("secrets: %s body clone: %w", opName, err)
			}
			req.Body = b
		}
		resp, err := hc.Do(req)
		if err == nil && resp.StatusCode < 500 {
			// Success (2xx/3xx/4xx). Hand resp to the caller, but first
			// release any earlier 5xx attempt's body.
			closeLast()
			return resp, nil
		}
		// This attempt failed (5xx or transport error). Drop the prior
		// failed response, then remember this one for the next loop / the
		// final return.
		closeLast()
		lastResp, lastErr = resp, err
	}
	if lastErr != nil {
		// A transport error never yields a usable response (and the
		// redirect-failure edge case, where it can, must not leak it).
		closeLast()
		return nil, fmt.Errorf("secrets: %s after %d attempts: %w", opName, len(refreshRetrySchedule), lastErr)
	}
	// Retries exhausted on persistent 5xx: hand the last response to the
	// caller, which inspects StatusCode and closes the body via defer.
	return lastResp, nil
}

// validateAccessToken is the cheap shape check we run before storing a
// refreshed token. A successful 200 response from a malformed gateway
// can otherwise hand us a 0-byte or 8-byte "token" that overwrites a
// good one in the credentials file — far better to fail the refresh
// than to brick the next CLI invocation. The 16-byte floor is well
// below the shortest format we've seen in the wild (Anthropic sk-…
// tokens are ~100B, Codex OAuth access tokens ~32-64B).
func validateAccessToken(provider, token string) error {
	if token == "" {
		return fmt.Errorf("secrets: %s refresh returned empty access_token", provider)
	}
	if len(token) < 16 {
		return fmt.Errorf("secrets: %s refresh returned implausibly short access_token (%d bytes)", provider, len(token))
	}
	return nil
}
