package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// DefaultScopes is the OAuth-App scope set a GitHub connection requests.
// `repo` covers the whole job iterion's OAuth-App path needs: read the diff,
// post PR comments, and manage repo webhooks (it subsumes admin:repo_hook).
//
// Least-privilege: `read:org` is deliberately NOT requested. ListRepos
// already surfaces org repositories via `/user/repos?affiliation=…,
// organization_member`, which `repo` alone authorizes; `read:org` only adds
// org/team-membership *enumeration* (the user's full org graph) that iterion
// does not use here. Dropping it keeps the connection token off the user's
// org-membership data. Operators wanting curated org-repo access should use
// the least-privilege GitHub-App connect flow (operator-selected repos).
var DefaultScopes = []string{"repo"}

// OAuthApp drives the GitHub OAuth-App authorization-code flow for one
// GitHub instance (github.com or GHE). Classic OAuth Apps issue
// non-expiring tokens and don't support PKCE, so there is no refresh path
// (the connection carries no expiry → the refresh worker skips it).
type OAuthApp struct {
	HTTP         *http.Client
	BaseURL      string // WEB base ("https://github.com" or a GHE host)
	ClientID     string
	ClientSecret string
}

func (a *OAuthApp) httpClient() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return http.DefaultClient
}

// AuthorizeURL builds the redirect URL. codeChallenge is ignored — classic
// GitHub OAuth Apps don't support PKCE.
func (a *OAuthApp) AuthorizeURL(redirectURI, state, _ string, scopes []string) string {
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}
	v := url.Values{}
	v.Set("client_id", a.ClientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("state", state)
	v.Set("scope", strings.Join(scopes, " "))
	return strings.TrimRight(a.BaseURL, "/") + "/login/oauth/authorize?" + v.Encode()
}

// Exchange trades the authorization code for an access token.
func (a *OAuthApp) Exchange(ctx context.Context, code, redirectURI, _ string) (forge.RefreshedToken, error) {
	v := url.Values{}
	v.Set("client_id", a.ClientID)
	v.Set("client_secret", a.ClientSecret)
	v.Set("code", code)
	v.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+"/login/oauth/access_token", strings.NewReader(v.Encode()))
	if err != nil {
		return forge.RefreshedToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return forge.RefreshedToken{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var tr struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
	}
	uErr := json.Unmarshal(raw, &tr)
	// GitHub returns 200 with an `error` field (e.g. bad_verification_code)
	// on a failed exchange rather than a non-2xx status.
	if tr.Error != "" {
		return forge.RefreshedToken{}, fmt.Errorf("github: token exchange: %s", tr.Error)
	}
	if resp.StatusCode/100 != 2 {
		return forge.RefreshedToken{}, fmt.Errorf("github: token endpoint: HTTP %d", resp.StatusCode)
	}
	// A 2xx with an unparseable body (e.g. an HTML rate-limit/error page)
	// would otherwise surface as the generic "no access_token" below; report
	// the parse failure instead so the cause is diagnosable.
	if uErr != nil {
		return forge.RefreshedToken{}, fmt.Errorf("github: token endpoint returned a non-JSON body (HTTP %d): %w", resp.StatusCode, uErr)
	}
	if tr.AccessToken == "" {
		return forge.RefreshedToken{}, fmt.Errorf("github: token endpoint returned no access_token")
	}
	return forge.RefreshedToken{
		AccessToken: tr.AccessToken,
		Scopes:      splitScopes(tr.Scope),
		// classic OAuth-App tokens don't expire → no ExpiresAt/RefreshToken.
	}, nil
}

// splitScopes parses GitHub's comma-separated scope string ("repo,read:org").
func splitScopes(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
