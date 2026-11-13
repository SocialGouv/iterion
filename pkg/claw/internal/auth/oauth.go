package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

const (
	// ClientID is the OAuth 2.0 client ID for Claude Code.
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// TokenURL is the OAuth token endpoint.
	TokenURL = "https://platform.claude.com/v1/oauth/token"
	// AuthorizeURL is the OAuth authorization endpoint (Claude.ai flow).
	AuthorizeURL = "https://claude.com/cai/oauth/authorize"
	// DefaultScopes are the OAuth scopes requested by default.
	DefaultScopes = "user:inference user:profile"
)

// PKCEPair holds an OAuth 2.0 PKCE code verifier and its SHA-256 challenge.
type PKCEPair struct {
	Verifier  string // random 32-byte value, base64url-encoded
	Challenge string // SHA-256(verifier), base64url-encoded
}

// GeneratePKCEPair generates a fresh PKCE verifier and challenge pair.
func GeneratePKCEPair() (PKCEPair, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return PKCEPair{}, fmt.Errorf("generate verifier bytes: %w", err)
	}
	verifier := base64URLEncode(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64URLEncode(sum[:])
	return PKCEPair{Verifier: verifier, Challenge: challenge}, nil
}

// generateState generates a random OAuth state parameter for CSRF protection.
func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate state bytes: %w", err)
	}
	return base64URLEncode(b), nil
}

func base64URLEncode(b []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
}

// StartOAuthFlow opens the browser to the Anthropic OAuth authorization page,
// starts a loopback HTTP listener for the redirect, and returns the resulting
// token data on success. The caller should save the tokens with SaveTokens.
func StartOAuthFlow() (*TokenData, error) {
	pkce, err := GeneratePKCEPair()
	if err != nil {
		return nil, err
	}
	state, err := generateState()
	if err != nil {
		return nil, err
	}

	// Bind to an OS-assigned port to avoid races.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start loopback listener: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	type callbackResult struct {
		code  string
		state string
		err   error
	}
	resultCh := make(chan callbackResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errCode := q.Get("error"); errCode != "" {
			desc := q.Get("error_description")
			resultCh <- callbackResult{err: fmt.Errorf("authorization error %q: %s", errCode, desc)}
			http.Error(w, "Authentication failed. You may close this window.", http.StatusBadRequest)
			return
		}
		resultCh <- callbackResult{code: q.Get("code"), state: q.Get("state")}
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;max-width:480px;margin:4rem auto">
<h2>Authentication successful!</h2>
<p>You may close this window and return to claw-code.</p>
</body></html>`)
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)                   //nolint:errcheck
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	authURL := buildAuthURL(redirectURI, pkce.Challenge, state)

	fmt.Printf("Opening browser for Anthropic OAuth login...\n")
	fmt.Printf("If the browser does not open automatically, visit:\n  %s\n\n", authURL)
	openBrowser(authURL)

	// Wait up to 5 minutes for the callback.
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()

	var result callbackResult
	select {
	case result = <-resultCh:
	case <-timer.C:
		return nil, fmt.Errorf("oauth timeout: authorization not completed within 5 minutes")
	}

	if result.err != nil {
		return nil, result.err
	}
	if result.state != state {
		return nil, fmt.Errorf("oauth state mismatch: possible CSRF attack, aborting")
	}

	return ExchangeCode(result.code, pkce.Verifier, redirectURI)
}

// buildAuthURL constructs the OAuth 2.0 authorization URL with PKCE parameters.
func buildAuthURL(redirectURI, codeChallenge, state string) string {
	v := url.Values{}
	v.Set("client_id", ClientID)
	v.Set("response_type", "code")
	v.Set("redirect_uri", redirectURI)
	v.Set("scope", DefaultScopes)
	v.Set("code_challenge", codeChallenge)
	v.Set("code_challenge_method", "S256")
	v.Set("state", state)
	return AuthorizeURL + "?" + v.Encode()
}

// ExchangeCode exchanges an authorization code for access + refresh tokens.
func ExchangeCode(code, verifier, redirectURI string) (*TokenData, error) {
	v := url.Values{}
	v.Set("grant_type", "authorization_code")
	v.Set("client_id", ClientID)
	v.Set("code", code)
	v.Set("redirect_uri", redirectURI)
	v.Set("code_verifier", verifier)
	return postTokenRequest(v)
}

// RefreshToken uses a stored refresh token to obtain a new access token.
func RefreshToken(refreshToken string) (*TokenData, error) {
	v := url.Values{}
	v.Set("grant_type", "refresh_token")
	v.Set("client_id", ClientID)
	v.Set("refresh_token", refreshToken)
	return postTokenRequest(v)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

func postTokenRequest(v url.Values) (*TokenData, error) {
	resp, err := http.PostForm(TokenURL, v)
	if err != nil {
		return nil, fmt.Errorf("token endpoint request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}

	expiresAt := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return &TokenData{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    expiresAt,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
	}, nil
}

// openBrowser opens url in the system browser (best-effort, Linux xdg-open).
func openBrowser(u string) {
	exec.Command("xdg-open", u).Start() //nolint:errcheck
}

// OAuthSession holds state for an in-progress OAuth flow that has been prepared
// (PKCE generated, loopback listener started) but not yet completed.
// Use PrepareOAuthFlow to create one, then call Complete to open the browser
// and wait for the redirect.
type OAuthSession struct {
	// AuthURL is the full authorization URL. The TUI displays this so users
	// can copy-paste it if the browser does not open automatically.
	AuthURL     string
	listener    net.Listener
	redirectURI string
	pkce        PKCEPair
	state       string
}

// PrepareOAuthFlow initialises a new OAuth session: generates a PKCE pair,
// starts a loopback TCP listener on an OS-assigned port, and builds the
// authorization URL — without opening the browser yet.
func PrepareOAuthFlow() (*OAuthSession, error) {
	pkce, err := GeneratePKCEPair()
	if err != nil {
		return nil, err
	}
	state, err := generateState()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start loopback listener: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	authURL := buildAuthURL(redirectURI, pkce.Challenge, state)
	return &OAuthSession{
		AuthURL:     authURL,
		listener:    listener,
		redirectURI: redirectURI,
		pkce:        pkce,
		state:       state,
	}, nil
}

// Complete opens the browser to AuthURL and blocks until the OAuth callback is
// received or a 5-minute timeout elapses. The caller should persist the
// returned TokenData with SetProviderOAuth or SaveTokens.
func (s *OAuthSession) Complete() (*TokenData, error) {
	type callbackResult struct {
		code  string
		state string
		err   error
	}
	resultCh := make(chan callbackResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errCode := q.Get("error"); errCode != "" {
			desc := q.Get("error_description")
			resultCh <- callbackResult{err: fmt.Errorf("authorization error %q: %s", errCode, desc)}
			http.Error(w, "Authentication failed. You may close this window.", http.StatusBadRequest)
			return
		}
		resultCh <- callbackResult{code: q.Get("code"), state: q.Get("state")}
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;max-width:480px;margin:4rem auto">
<h2>Authentication successful!</h2>
<p>You may close this window and return to claw-code.</p>
</body></html>`)
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(s.listener)                 //nolint:errcheck
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	openBrowser(s.AuthURL)

	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()

	var result callbackResult
	select {
	case result = <-resultCh:
	case <-timer.C:
		return nil, fmt.Errorf("oauth timeout: authorization not completed within 5 minutes")
	}
	if result.err != nil {
		return nil, result.err
	}
	if result.state != s.state {
		return nil, fmt.Errorf("oauth state mismatch: possible CSRF attack, aborting")
	}
	return ExchangeCode(result.code, s.pkce.Verifier, s.redirectURI)
}
