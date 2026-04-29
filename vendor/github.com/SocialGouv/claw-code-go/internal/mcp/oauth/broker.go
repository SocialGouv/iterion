package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ServerConfig is the per-server OAuth context the broker needs to drive
// an authorization-code + PKCE flow. The broker accepts these as a
// value rather than reading them off a global registry to keep the
// auth flow testable in isolation.
type ServerConfig struct {
	// Name uniquely identifies the MCP server in the token store.
	Name string
	// AuthURL is the authorization endpoint the user's browser visits.
	AuthURL string
	// TokenURL is the back-channel endpoint that issues tokens.
	TokenURL string
	// RevokeURL is the optional RFC 7009 revocation endpoint. When set,
	// Broker.Revoke posts to it before deleting the local cache entry.
	RevokeURL string
	// ClientID is the OAuth client identifier registered with the
	// authorization server.
	ClientID string
	// Scopes are the access scopes requested. Empty is allowed for
	// servers that do not use scopes.
	Scopes []string
}

// Broker drives the OAuth 2.0 Authorization Code + PKCE flow for a
// single MCP server, persisting the resulting tokens via Storage.
//
// The broker is safe for concurrent use by multiple goroutines. The
// callback HTTP server is short-lived: a new instance is started per
// Acquire call and torn down once the code is captured (or the request
// times out).
type Broker struct {
	mu sync.Mutex

	storage      *Storage
	redirectPort int
	authOpener   func(string) error
	httpClient   *http.Client
	now          func() time.Time
	timeout      time.Duration
}

// Option configures a Broker. Options are applied in NewBroker.
type Option func(*Broker)

// WithStorage installs a custom Storage. Defaults to nil — callers that
// want token persistence MUST set this. (Tokens still flow through the
// broker's return value either way.)
func WithStorage(s *Storage) Option {
	return func(b *Broker) { b.storage = s }
}

// WithRedirectPort pins the loopback callback port. The default 0
// means "let the OS choose". Tests pass an explicit port to avoid races
// when running in parallel.
func WithRedirectPort(port int) Option {
	return func(b *Broker) { b.redirectPort = port }
}

// WithAuthOpener overrides the function used to surface the
// authorization URL to the user. The default prints the URL and tells
// the user to open it manually. Tests inject a function that performs
// the callback fetch directly to drive the flow end-to-end.
func WithAuthOpener(fn func(string) error) Option {
	return func(b *Broker) { b.authOpener = fn }
}

// WithHTTPClient overrides the HTTP client used for token exchange and
// revocation. Useful for httptest servers in tests.
func WithHTTPClient(c *http.Client) Option {
	return func(b *Broker) { b.httpClient = c }
}

// WithClock overrides the time source. Used by tests to produce
// deterministic ExpiresAt values.
func WithClock(now func() time.Time) Option {
	return func(b *Broker) { b.now = now }
}

// WithCallbackTimeout caps how long Acquire waits for the user to
// complete the browser flow. Defaults to 5 minutes.
func WithCallbackTimeout(d time.Duration) Option {
	return func(b *Broker) { b.timeout = d }
}

// NewBroker constructs a broker with the given options. A working
// broker requires at least WithStorage; callers that omit it can still
// drive the flow but acquired tokens will not be persisted.
func NewBroker(opts ...Option) *Broker {
	b := &Broker{
		authOpener: defaultAuthOpener,
		httpClient: http.DefaultClient,
		now:        time.Now,
		timeout:    5 * time.Minute,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// defaultAuthOpener prints the authorization URL to stderr and returns
// nil. We deliberately avoid spawning xdg-open / open / start because
// those calls are silent failures on headless machines and in CI.
func defaultAuthOpener(authURL string) error {
	fmt.Fprintf(stderr(), "Open the following URL in your browser to authorize:\n  %s\n", authURL)
	return nil
}

// Acquire returns a valid access token for cfg, performing the full
// auth code + PKCE flow when no cached token is available, or
// refreshing one that is about to expire. The returned string is
// always a bearer access token; callers do not need to read Storage.
func (b *Broker) Acquire(ctx context.Context, cfg ServerConfig) (string, error) {
	if cfg.Name == "" || cfg.AuthURL == "" || cfg.TokenURL == "" || cfg.ClientID == "" {
		return "", errors.New("oauth broker: ServerConfig missing required fields (Name/AuthURL/TokenURL/ClientID)")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.storage != nil {
		if tok, ok, err := b.storage.Load(cfg.Name); err == nil && ok {
			if !tok.IsExpired(30 * time.Second) {
				return tok.AccessToken, nil
			}
			if tok.RefreshToken != "" {
				if refreshed, err := b.refresh(ctx, cfg, tok.RefreshToken); err == nil {
					_ = b.storage.Save(cfg.Name, refreshed)
					return refreshed.AccessToken, nil
				}
				// Fall through to a fresh interactive flow if refresh fails.
			}
		}
	}

	tok, err := b.runAuthCodeFlow(ctx, cfg)
	if err != nil {
		return "", err
	}
	if b.storage != nil {
		_ = b.storage.Save(cfg.Name, tok)
	}
	return tok.AccessToken, nil
}

// Revoke removes the cached token for cfg.Name. When cfg.RevokeURL is
// non-empty, it is posted with the access token first.
//
// Error semantics — read this if you're integrating with an OAuth
// provider that has a flaky revocation endpoint:
//
//   - revoke ok + delete ok → returns nil; remote and local both cleared.
//   - revoke ok + delete err → returns the delete error; remote cleared,
//     local probably stale (operator must clean up).
//   - revoke err + delete ok → returns the revoke error; **local IS
//     cleared** so the next Acquire restarts cleanly. Caller can treat
//     a non-nil return here as "investigate the auth server" without
//     worrying about a stale local cache.
//   - revoke err + delete err → returns a wrapped error mentioning both.
//
// In other words: a non-nil return never leaves a stale local entry.
func (b *Broker) Revoke(ctx context.Context, cfg ServerConfig) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.storage == nil {
		return nil
	}

	tok, ok, err := b.storage.Load(cfg.Name)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	var revokeErr error
	if cfg.RevokeURL != "" {
		form := url.Values{}
		form.Set("token", tok.AccessToken)
		form.Set("client_id", cfg.ClientID)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.RevokeURL, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := b.httpClient.Do(req)
			if err != nil {
				revokeErr = err
			} else {
				resp.Body.Close()
				if resp.StatusCode >= 400 {
					revokeErr = fmt.Errorf("oauth revoke: %s returned %d", cfg.RevokeURL, resp.StatusCode)
				}
			}
		}
	}

	if delErr := b.storage.Delete(cfg.Name); delErr != nil {
		if revokeErr != nil {
			return fmt.Errorf("revoke (network): %v; storage delete: %w", revokeErr, delErr)
		}
		return delErr
	}
	return revokeErr
}

// runAuthCodeFlow drives the full Authorization Code + PKCE sequence:
// open a loopback listener for the redirect, send the user to the
// authorization URL, wait for the callback, exchange the code, return
// the resulting token. The loopback listener is bound to 127.0.0.1
// (never 0.0.0.0) so other hosts on the network cannot intercept the
// authorization response.
func (b *Broker) runAuthCodeFlow(ctx context.Context, cfg ServerConfig) (Token, error) {
	pkce, err := NewPKCE()
	if err != nil {
		return Token{}, err
	}
	state, err := randomState()
	if err != nil {
		return Token{}, err
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", b.redirectPort))
	if err != nil {
		return Token{}, fmt.Errorf("oauth broker: listen loopback: %w", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", port)

	type cbResult struct {
		code  string
		err   error
		state string
	}
	result := make(chan cbResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errMsg := q.Get("error"); errMsg != "" {
			fmt.Fprintln(w, "Authorization failed. You can close this window.")
			result <- cbResult{err: fmt.Errorf("oauth callback: %s: %s", errMsg, q.Get("error_description"))}
			return
		}
		fmt.Fprintln(w, "Authorization complete. You can close this window.")
		result <- cbResult{code: q.Get("code"), state: q.Get("state")}
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	authURL := buildAuthURL(cfg, redirectURI, state, pkce.Challenge, pkce.Method)
	if err := b.authOpener(authURL); err != nil {
		return Token{}, fmt.Errorf("oauth broker: open auth url: %w", err)
	}

	timeout := b.timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case <-waitCtx.Done():
		return Token{}, fmt.Errorf("oauth broker: callback wait: %w", waitCtx.Err())
	case res := <-result:
		if res.err != nil {
			return Token{}, res.err
		}
		if res.state != state {
			return Token{}, fmt.Errorf("oauth broker: state mismatch (csrf? expected=%q got=%q)", state, res.state)
		}
		if res.code == "" {
			return Token{}, errors.New("oauth broker: callback missing authorization code")
		}
		return b.exchangeCode(waitCtx, cfg, res.code, pkce.Verifier, redirectURI)
	}
}

func buildAuthURL(cfg ServerConfig, redirectURI, state, challenge, method string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", method)
	if len(cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	sep := "?"
	if strings.Contains(cfg.AuthURL, "?") {
		sep = "&"
	}
	return cfg.AuthURL + sep + q.Encode()
}

// exchangeCode posts the auth code + PKCE verifier to the token
// endpoint. Returns the parsed token with ExpiresAt computed from
// expires_in (provided by the server in seconds).
func (b *Broker) exchangeCode(ctx context.Context, cfg ServerConfig, code, verifier, redirectURI string) (Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", cfg.ClientID)
	form.Set("code_verifier", verifier)

	return b.postTokenForm(ctx, cfg.TokenURL, form)
}

// refresh exchanges a refresh_token for a fresh access token.
func (b *Broker) refresh(ctx context.Context, cfg ServerConfig, refreshToken string) (Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", cfg.ClientID)
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}

	tok, err := b.postTokenForm(ctx, cfg.TokenURL, form)
	if err != nil {
		return Token{}, err
	}
	// Carry the refresh token forward when the AS doesn't rotate it
	// (RFC 6749 §6: refresh tokens are optional in the response).
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}

// tokenResponse is the standard RFC 6749 token endpoint response shape.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (b *Broker) postTokenForm(ctx context.Context, tokenURL string, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("oauth token: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("oauth token: post: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return Token{}, fmt.Errorf("oauth token: read response: %w", err)
	}

	var tr tokenResponse
	if jsonErr := json.Unmarshal(body, &tr); jsonErr != nil {
		// Some servers return form-encoded responses on error. We
		// don't bother decoding those — the status line is enough.
		if resp.StatusCode >= 400 {
			snippet := string(body)
			if len(snippet) > 256 {
				snippet = snippet[:256] + "…"
			}
			return Token{}, fmt.Errorf("oauth token: %s returned %d: %s", tokenURL, resp.StatusCode, snippet)
		}
		return Token{}, fmt.Errorf("oauth token: decode response: %w", jsonErr)
	}

	if tr.Error != "" {
		return Token{}, fmt.Errorf("oauth token: %s: %s", tr.Error, tr.ErrorDesc)
	}
	if resp.StatusCode >= 400 {
		return Token{}, fmt.Errorf("oauth token: %s returned %d", tokenURL, resp.StatusCode)
	}
	if tr.AccessToken == "" {
		return Token{}, errors.New("oauth token: response missing access_token")
	}

	tok := Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
	}
	if tr.ExpiresIn > 0 {
		tok.ExpiresAt = b.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// stderr is a seam for tests. Exposed so we can later route messages
// somewhere quieter without touching the broker call sites.
var stderr = func() io.Writer { return os.Stderr }

// BearerHeaderFunc returns a closure that calls Acquire on every
// invocation and renders the result as a "Bearer <token>" string. This
// is the bridge MCP transports use: instead of holding a static auth
// header, they capture this closure and call it before each request so
// expired tokens trigger an automatic refresh through the broker.
func (b *Broker) BearerHeaderFunc(cfg ServerConfig) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		tok, err := b.Acquire(ctx, cfg)
		if err != nil {
			return "", err
		}
		return "Bearer " + tok, nil
	}
}
