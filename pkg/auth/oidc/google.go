package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GoogleConnector implements Connector against Google's OIDC
// endpoints. We hardcode the canonical URLs (no .well-known fetch
// at every login) — Google has not changed these in years.
type GoogleConnector struct {
	clientID     string
	clientSecret string
	display      string
	httpClient   *http.Client
}

// NewGoogleConnector returns a Google OIDC connector. clientID and
// clientSecret are issued by the Google Cloud Console (OAuth 2.0
// client of type "Web application").
func NewGoogleConnector(clientID, clientSecret, display string) *GoogleConnector {
	if display == "" {
		display = "Google"
	}
	return &GoogleConnector{
		clientID:     clientID,
		clientSecret: clientSecret,
		display:      display,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (g *GoogleConnector) Name() string       { return "google" }
func (g *GoogleConnector) Display() string    { return g.display }
func (g *GoogleConnector) SupportsPKCE() bool { return true }

const (
	googleAuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL     = "https://oauth2.googleapis.com/token"
	googleUserInfoURL  = "https://openidconnect.googleapis.com/v1/userinfo"
	googleScopes       = "openid email profile"
)

func (g *GoogleConnector) AuthorizeURL(_ context.Context, redirectURI, state, codeVerifier string) (string, error) {
	if g.clientID == "" {
		return "", fmt.Errorf("oidc/google: missing client id")
	}
	q := url.Values{}
	q.Set("client_id", g.clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", googleScopes)
	q.Set("state", state)
	q.Set("access_type", "online")
	q.Set("include_granted_scopes", "true")
	if codeVerifier != "" {
		q.Set("code_challenge", deriveS256(codeVerifier))
		q.Set("code_challenge_method", "S256")
	}
	return googleAuthorizeURL + "?" + q.Encode(), nil
}

func (g *GoogleConnector) ExchangeCode(ctx context.Context, code, redirectURI, codeVerifier string) (ExternalUser, error) {
	form := url.Values{}
	form.Set("client_id", g.clientID)
	form.Set("client_secret", g.clientSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirectURI)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/google: build token req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/google: exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ExternalUser{}, fmt.Errorf("oidc/google: token endpoint %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/google: decode token: %w", err)
	}
	if tok.AccessToken == "" {
		return ExternalUser{}, fmt.Errorf("oidc/google: empty access token")
	}

	// Fetch userinfo with the access token. We don't validate the
	// id_token signature here because the userinfo endpoint is
	// authenticated by the Bearer access_token; Google validates the
	// caller's identity on its side.
	uReq, err := http.NewRequestWithContext(ctx, http.MethodGet, googleUserInfoURL, nil)
	if err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/google: userinfo build: %w", err)
	}
	uReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uReq.Header.Set("Accept", "application/json")
	uResp, err := g.httpClient.Do(uReq)
	if err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/google: userinfo: %w", err)
	}
	defer uResp.Body.Close()
	if uResp.StatusCode/100 != 2 {
		return ExternalUser{}, fmt.Errorf("oidc/google: userinfo endpoint %d", uResp.StatusCode)
	}
	var info struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := json.NewDecoder(uResp.Body).Decode(&info); err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/google: decode userinfo: %w", err)
	}
	if info.Email == "" {
		return ExternalUser{}, ErrEmailMissing
	}
	return ExternalUser{
		Provider: g.Name(),
		Subject:  info.Sub,
		Email:    info.Email,
		Name:     info.Name,
	}, nil
}
