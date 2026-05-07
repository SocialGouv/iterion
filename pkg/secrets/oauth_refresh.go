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
// (2026-05). Operators can override via env on a per-deployment
// basis when an OEM repackages the CLI.
const (
	anthropicTokenURL = "https://console.anthropic.com/v1/oauth/token"
	codexTokenURL     = "https://auth.openai.com/oauth/token"
)

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
	if clientID == "" || refreshToken == "" {
		return RefreshResult{}, fmt.Errorf("secrets: anthropic refresh requires client_id + refresh_token")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return RefreshResult{}, fmt.Errorf("secrets: build refresh req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("secrets: anthropic refresh: %w", err)
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
	if tok.AccessToken == "" {
		return RefreshResult{}, errors.New("secrets: anthropic refresh returned empty access_token")
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

// RefreshCodex mirrors RefreshAnthropic for the OpenAI Codex CLI.
// clientID is the Codex CLI's published OAuth client; deployments
// using a custom Codex fork override it.
func RefreshCodex(ctx context.Context, hc *http.Client, clientID, refreshToken string) (RefreshResult, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if clientID == "" || refreshToken == "" {
		return RefreshResult{}, fmt.Errorf("secrets: codex refresh requires client_id + refresh_token")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return RefreshResult{}, fmt.Errorf("secrets: build codex refresh req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("secrets: codex refresh: %w", err)
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
	if tok.AccessToken == "" {
		return RefreshResult{}, errors.New("secrets: codex refresh returned empty access_token")
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
