package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GenericConnector implements Connector against any OpenID Connect-
// compliant provider via the discovery document. It fetches the
// .well-known/openid-configuration on first use and caches it.
//
// We do not validate the ID token signature here: the userinfo
// endpoint is authenticated with the just-minted access token, and
// the token endpoint already authenticated the request via
// client_secret_basic. For tighter validation a future iteration
// can add JWKS fetching + ID-token signature checking.
type GenericConnector struct {
	name         string
	display      string
	issuerURL    string
	clientID     string
	clientSecret string
	scopes       []string
	httpClient   *http.Client

	once   sync.Once
	doc    discoveryDoc
	docErr error
}

type discoveryDoc struct {
	Issuer       string `json:"issuer"`
	AuthorizeURL string `json:"authorization_endpoint"`
	TokenURL     string `json:"token_endpoint"`
	UserInfoURL  string `json:"userinfo_endpoint"`
	JWKSURL      string `json:"jwks_uri"`
}

// NewGenericConnector returns a discovery-based connector under the
// fixed slug "sso" (so the routes are /auth/oidc/sso/start, etc.).
// Operators bring exactly one generic provider per deployment in V1.
func NewGenericConnector(issuerURL, clientID, clientSecret, display string, scopes []string) *GenericConnector {
	if display == "" {
		display = "SSO"
	}
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	return &GenericConnector{
		name:         "sso",
		display:      display,
		issuerURL:    strings.TrimRight(issuerURL, "/"),
		clientID:     clientID,
		clientSecret: clientSecret,
		scopes:       scopes,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *GenericConnector) Name() string       { return c.name }
func (c *GenericConnector) Display() string    { return c.display }
func (c *GenericConnector) SupportsPKCE() bool { return true }

func (c *GenericConnector) discover(ctx context.Context) (discoveryDoc, error) {
	c.once.Do(func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.issuerURL+"/.well-known/openid-configuration", nil)
		if err != nil {
			c.docErr = fmt.Errorf("oidc/generic: build discovery req: %w", err)
			return
		}
		req.Header.Set("Accept", "application/json")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			c.docErr = fmt.Errorf("oidc/generic: discovery: %w", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			c.docErr = fmt.Errorf("oidc/generic: discovery %d", resp.StatusCode)
			return
		}
		var doc discoveryDoc
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			c.docErr = fmt.Errorf("oidc/generic: decode discovery: %w", err)
			return
		}
		if doc.AuthorizeURL == "" || doc.TokenURL == "" || doc.UserInfoURL == "" {
			c.docErr = fmt.Errorf("oidc/generic: discovery missing endpoints")
			return
		}
		c.doc = doc
	})
	return c.doc, c.docErr
}

func (c *GenericConnector) AuthorizeURL(ctx context.Context, redirectURI, state, codeVerifier string) (string, error) {
	doc, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("client_id", c.clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(c.scopes, " "))
	q.Set("state", state)
	if codeVerifier != "" {
		q.Set("code_challenge", deriveS256(codeVerifier))
		q.Set("code_challenge_method", "S256")
	}
	sep := "?"
	if strings.Contains(doc.AuthorizeURL, "?") {
		sep = "&"
	}
	return doc.AuthorizeURL + sep + q.Encode(), nil
}

func (c *GenericConnector) ExchangeCode(ctx context.Context, code, redirectURI, codeVerifier string) (ExternalUser, error) {
	doc, err := c.discover(ctx)
	if err != nil {
		return ExternalUser{}, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/generic: build token req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/generic: exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ExternalUser{}, fmt.Errorf("oidc/generic: token endpoint %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/generic: decode token: %w", err)
	}
	if tok.AccessToken == "" {
		return ExternalUser{}, fmt.Errorf("oidc/generic: empty access token")
	}

	uReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, doc.UserInfoURL, nil)
	uReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uReq.Header.Set("Accept", "application/json")
	uResp, err := c.httpClient.Do(uReq)
	if err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/generic: userinfo: %w", err)
	}
	defer uResp.Body.Close()
	if uResp.StatusCode/100 != 2 {
		return ExternalUser{}, fmt.Errorf("oidc/generic: userinfo %d", uResp.StatusCode)
	}
	var info struct {
		Sub               string `json:"sub"`
		Email             string `json:"email"`
		EmailVerified     bool   `json:"email_verified"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := json.NewDecoder(uResp.Body).Decode(&info); err != nil {
		return ExternalUser{}, fmt.Errorf("oidc/generic: decode userinfo: %w", err)
	}
	if info.Email == "" {
		return ExternalUser{}, ErrEmailMissing
	}
	name := info.Name
	if name == "" {
		name = info.PreferredUsername
	}
	return ExternalUser{
		Provider: c.Name(),
		Subject:  info.Sub,
		Email:    info.Email,
		Name:     name,
	}, nil
}
