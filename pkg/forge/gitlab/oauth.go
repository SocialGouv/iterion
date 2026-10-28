package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// DefaultScopes is the OAuth scope set a GitLab connection requests. GitLab
// has no granular "manage webhooks" scope, so `api` (full read+write) is
// the practical floor for creating project hooks and posting MR comments.
var DefaultScopes = []string{"api"}

// OAuthApp drives the GitLab OAuth authorization-code (+ PKCE) flow for one
// GitLab instance and refreshes the resulting tokens. It implements
// forge.TokenRefresher so the refresh worker can renew it.
type OAuthApp struct {
	HTTP         *http.Client
	BaseURL      string // "https://gitlab.example.com"
	ClientID     string
	ClientSecret string
}

func (a *OAuthApp) httpClient() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return http.DefaultClient
}

// AuthorizeURL builds the URL the operator is redirected to. codeChallenge
// is the S256 PKCE challenge (pass "" to omit PKCE).
func (a *OAuthApp) AuthorizeURL(redirectURI, state, codeChallenge string, scopes []string) string {
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}
	v := url.Values{}
	v.Set("client_id", a.ClientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("response_type", "code")
	v.Set("state", state)
	v.Set("scope", strings.Join(scopes, " "))
	if codeChallenge != "" {
		v.Set("code_challenge", codeChallenge)
		v.Set("code_challenge_method", "S256")
	}
	return strings.TrimRight(a.BaseURL, "/") + "/oauth/authorize?" + v.Encode()
}

// Exchange trades an authorization code for tokens.
func (a *OAuthApp) Exchange(ctx context.Context, code, redirectURI, codeVerifier string) (forge.RefreshedToken, error) {
	v := url.Values{}
	v.Set("grant_type", "authorization_code")
	v.Set("code", code)
	v.Set("redirect_uri", redirectURI)
	v.Set("client_id", a.ClientID)
	v.Set("client_secret", a.ClientSecret)
	if codeVerifier != "" {
		v.Set("code_verifier", codeVerifier)
	}
	return a.postToken(ctx, v)
}

// Refresh renews the access token from a refresh token (forge.TokenRefresher).
func (a *OAuthApp) Refresh(ctx context.Context, _ forge.Connection, refreshToken string) (forge.RefreshedToken, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return forge.RefreshedToken{}, forge.ErrUnauthorized // nothing to refresh with → force reconnect
	}
	v := url.Values{}
	v.Set("grant_type", "refresh_token")
	v.Set("refresh_token", refreshToken)
	v.Set("client_id", a.ClientID)
	v.Set("client_secret", a.ClientSecret)
	return a.postToken(ctx, v)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
}

func (a *OAuthApp) postToken(ctx context.Context, v url.Values) (forge.RefreshedToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+"/oauth/token", strings.NewReader(v.Encode()))
	if err != nil {
		return forge.RefreshedToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := forge.DoHTTP(a.httpClient(), req, forge.OAuthRedirectSecrets(v)...)
	if err != nil {
		return forge.RefreshedToken{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var tr tokenResponse
	uErr := json.Unmarshal(raw, &tr)
	// A revoked/expired refresh token returns 400 invalid_grant (or 401) —
	// surface as ErrUnauthorized so the worker flips the connection to
	// needs_reauth rather than retrying forever.
	if resp.StatusCode == http.StatusUnauthorized || tr.Error == "invalid_grant" {
		return forge.RefreshedToken{}, forge.ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		if tr.Error != "" {
			return forge.RefreshedToken{}, fmt.Errorf("gitlab: token endpoint: %s (HTTP %d)", tr.Error, resp.StatusCode)
		}
		return forge.RefreshedToken{}, fmt.Errorf("gitlab: token endpoint: HTTP %d", resp.StatusCode)
	}
	// A 2xx with an unparseable body would otherwise surface as the generic
	// "no access_token" below; report the parse failure so the cause is clear.
	if uErr != nil {
		return forge.RefreshedToken{}, fmt.Errorf("gitlab: token endpoint returned a non-JSON body (HTTP %d): %w", resp.StatusCode, uErr)
	}
	if tr.AccessToken == "" {
		return forge.RefreshedToken{}, fmt.Errorf("gitlab: token endpoint returned no access_token")
	}
	out := forge.RefreshedToken{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		Scopes:       strings.Fields(tr.Scope),
	}
	if tr.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().UTC().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return out, nil
}

// compile-time assertion that OAuthApp satisfies forge.TokenRefresher.
var _ forge.TokenRefresher = (*OAuthApp)(nil)
