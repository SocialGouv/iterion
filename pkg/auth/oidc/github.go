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

// GitHubConnector implements Connector against GitHub's OAuth2
// (NOT OIDC) endpoints. We retrieve the user profile from the v3
// API and resolve the verified primary email separately.
type GitHubConnector struct {
	clientID     string
	clientSecret string
	display      string
	httpClient   *http.Client
}

func NewGitHubConnector(clientID, clientSecret, display string) *GitHubConnector {
	if display == "" {
		display = "GitHub"
	}
	return &GitHubConnector{
		clientID:     clientID,
		clientSecret: clientSecret,
		display:      display,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (g *GitHubConnector) Name() string       { return "github" }
func (g *GitHubConnector) Display() string    { return g.display }
func (g *GitHubConnector) SupportsPKCE() bool { return true }

const (
	githubAuthorizeURL = "https://github.com/login/oauth/authorize"
	githubTokenURL     = "https://github.com/login/oauth/access_token"
	githubUserURL      = "https://api.github.com/user"
	githubEmailsURL    = "https://api.github.com/user/emails"
	githubScopes       = "read:user user:email"
)

func (g *GitHubConnector) AuthorizeURL(_ context.Context, redirectURI, state, codeVerifier string) (string, error) {
	if g.clientID == "" {
		return "", fmt.Errorf("oidc/github: missing client id")
	}
	q := url.Values{}
	q.Set("client_id", g.clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", githubScopes)
	q.Set("state", state)
	q.Set("allow_signup", "true")
	if codeVerifier != "" {
		q.Set("code_challenge", deriveS256(codeVerifier))
		q.Set("code_challenge_method", "S256")
	}
	return githubAuthorizeURL + "?" + q.Encode(), nil
}

func (g *GitHubConnector) ExchangeCode(ctx context.Context, code, redirectURI, codeVerifier string) (ExternalUser, error) {
	form := url.Values{}
	form.Set("client_id", g.clientID)
	form.Set("client_secret", g.clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/github: build token req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/github: exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ExternalUser{}, fmt.Errorf("oidc/github: token endpoint %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/github: decode token: %w", err)
	}
	if tok.Error != "" {
		return ExternalUser{}, fmt.Errorf("oidc/github: %s: %s", tok.Error, tok.ErrorDesc)
	}
	if tok.AccessToken == "" {
		return ExternalUser{}, fmt.Errorf("oidc/github: empty access token")
	}

	user, err := g.fetchUser(ctx, tok.AccessToken)
	if err != nil {
		return ExternalUser{}, err
	}
	if user.Email == "" {
		// Some accounts hide their email; fetch the verified primary.
		email, err := g.fetchPrimaryEmail(ctx, tok.AccessToken)
		if err != nil {
			return ExternalUser{}, err
		}
		user.Email = email
	}
	if user.Email == "" {
		return ExternalUser{}, ErrEmailMissing
	}
	return user, nil
}

func (g *GitHubConnector) fetchUser(ctx context.Context, accessToken string) (ExternalUser, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, githubUserURL, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/github: user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ExternalUser{}, fmt.Errorf("oidc/github: user endpoint %d", resp.StatusCode)
	}
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/github: decode user: %w", err)
	}
	subject := fmt.Sprintf("%d", u.ID)
	name := u.Name
	if name == "" {
		name = u.Login
	}
	return ExternalUser{
		Provider: g.Name(),
		Subject:  subject,
		Email:    u.Email,
		Name:     name,
	}, nil
}

func (g *GitHubConnector) fetchPrimaryEmail(ctx context.Context, accessToken string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, githubEmailsURL, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc/github: emails: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("oidc/github: emails endpoint %d", resp.StatusCode)
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return "", fmt.Errorf("oidc/github: decode emails: %w", err)
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email, nil
		}
	}
	for _, e := range emails {
		if e.Verified {
			return e.Email, nil
		}
	}
	return "", ErrEmailMissing
}
