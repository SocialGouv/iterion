package main

// cloud_client.go is the DESKTOP-independent core of the "connect to a
// remote iterion cloud" feature: the native HTTP client that talks to a
// cloud instance's auth API and the token jar that holds the resulting
// JWT + refresh token. It carries NO build tag on purpose — it depends
// only on the standard library and an injected refreshStore, so it can be
// unit-tested (cloud_client_test.go) without the Wails/webkit toolchain.
// The Wails *App bindings that wire this to the keychain, the proxy, and
// the SPA live in cloud.go (//go:build desktop).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// cloudRefreshKeyPrefix namespaces a connection's refresh token in the
	// OS keychain: "cloud_refresh:<connID>".
	cloudRefreshKeyPrefix = "cloud_refresh:"

	// Cookie names the cloud sets on every auth response. Mirror of
	// pkg/server/middleware.go's authCookieName / refreshCookieName — the
	// native client harvests the refresh token from the Set-Cookie header
	// (it is deliberately NOT echoed in the JSON body).
	cloudAuthCookieName    = "iterion_auth"
	cloudRefreshCookieName = "iterion_refresh"

	// cloudHTTPTimeout bounds a single auth request. Login/refresh are
	// small round-trips; a generous ceiling covers a slow remote.
	cloudHTTPTimeout = 30 * time.Second

	// cloudRefreshSkew is how long before access-token expiry the background
	// loop refreshes, and the floor used when an expiry can't be parsed.
	cloudRefreshSkew = 2 * time.Minute
)

// refreshStore is the minimal secret-persistence surface the token jar
// needs. *Keychain (keychain.go, desktop-tagged) satisfies it structurally;
// tests inject a fake. Kept distinct from keychain.go's secretStore so this
// file stays build-tag-free.
type refreshStore interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
}

// cloudUserSummary is the identity subset the bindings return to the SPA
// shell after a successful login. It is NOT the full org tree (the SPA gets
// that from /api/auth/me through the proxy) — just enough to name the
// connection and cache the active org/team.
type cloudUserSummary struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	Name         string `json:"name,omitempty"`
	IsSuperAdmin bool   `json:"is_super_admin"`
	ActiveOrgID  string `json:"active_org_id,omitempty"`
	ActiveTeamID string `json:"active_team_id,omitempty"`
}

// cloudAuthResult is the parsed outcome of a login/refresh call.
type cloudAuthResult struct {
	AccessToken  string
	AccessExpiry time.Time
	RefreshToken string // harvested from the iterion_refresh Set-Cookie
	User         cloudUserSummary
}

// cloudAuthError carries the HTTP status of a failed auth call so the
// bindings can map it to a friendly message (401 invalid credentials, 403
// password-change/SSO-restricted, 409 link-required, …), mirroring the
// SPA's own error mapping.
type cloudAuthError struct {
	Status  int
	Message string
}

func (e *cloudAuthError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("cloud auth failed (%d): %s", e.Status, e.Message)
	}
	return fmt.Sprintf("cloud auth failed (%d)", e.Status)
}

// asCloudAuthError extracts a *cloudAuthError from err, or nil.
func asCloudAuthError(err error) *cloudAuthError {
	var ae *cloudAuthError
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}

// cloudServerInfo is the subset of GET /api/server/info the desktop needs
// to validate that a URL points at a real, auth-enabled cloud instance.
type cloudServerInfo struct {
	Mode         string `json:"mode"`
	AuthRequired bool   `json:"auth_required"`
}

// cloudProvider is one SSO provider offered by a cloud instance.
type cloudProvider struct {
	Name    string `json:"name"`
	Display string `json:"display"`
}

// cloudProviders is the GET /api/auth/providers response.
type cloudProviders struct {
	SignupMode string          `json:"signup_mode"`
	Providers  []cloudProvider `json:"providers"`
}

// newCloudHTTPClient returns the native client used for all cloud auth
// calls. It carries NO cookie jar (so resp.Cookies() surfaces the rotated
// refresh cookie for the caller to harvest) and sends no Origin/Sec-Fetch
// headers, so the cloud's isBrowserClient() is false and it returns the
// access token in the JSON body.
func newCloudHTTPClient() *http.Client {
	return &http.Client{Timeout: cloudHTTPTimeout}
}

// cloudEndpoint joins a cloud base URL and an API path safely, tolerating a
// trailing slash on the base.
func cloudEndpoint(baseURL, apiPath string) (string, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", fmt.Errorf("invalid cloud URL %q: %w", baseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("cloud URL must be absolute (scheme + host): %q", baseURL)
	}
	u.Path = strings.TrimRight(u.Path, "/") + apiPath
	return u.String(), nil
}

// cloudFetchServerInfo probes GET /api/server/info to confirm the URL is an
// auth-enabled cloud instance before a connection is registered.
func cloudFetchServerInfo(ctx context.Context, hc *http.Client, baseURL string) (cloudServerInfo, error) {
	var info cloudServerInfo
	endpoint, err := cloudEndpoint(baseURL, "/api/server/info")
	if err != nil {
		return info, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return info, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return info, fmt.Errorf("reach cloud %q: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("cloud %q returned %d for /api/server/info", baseURL, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return info, fmt.Errorf("decode server info: %w", err)
	}
	return info, nil
}

// cloudListProviders fetches the SSO providers a cloud instance offers,
// optionally scoped to the user's email (for per-org Keycloak discovery).
func cloudListProviders(ctx context.Context, hc *http.Client, baseURL, email string) (cloudProviders, error) {
	var out cloudProviders
	endpoint, err := cloudEndpoint(baseURL, "/api/auth/providers")
	if err != nil {
		return out, err
	}
	if email != "" {
		endpoint += "?email=" + url.QueryEscape(email)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return out, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return out, fmt.Errorf("list providers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("providers endpoint returned %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return out, fmt.Errorf("decode providers: %w", err)
	}
	return out, nil
}

// cloudOIDCAuthorizeURL kicks off a DESKTOP SSO flow: it asks the cloud for the
// IdP authorize URL (format=json) bound to a desktop flow whose callback will
// 302 a single-use ticket to loopbackRedirect. The desktop opens the returned
// URL in the system browser.
func cloudOIDCAuthorizeURL(ctx context.Context, hc *http.Client, baseURL, provider, loopbackRedirect string) (string, error) {
	endpoint, err := cloudEndpoint(baseURL, "/api/auth/oidc/"+url.PathEscape(provider)+"/start")
	if err != nil {
		return "", err
	}
	q := url.Values{
		"format":  {"json"},
		"desktop": {"1"},
		"next":    {loopbackRedirect},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc start: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc start returned %d: %s", resp.StatusCode, parseCloudErrorBody(raw))
	}
	var body struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.AuthorizeURL == "" {
		return "", fmt.Errorf("oidc start: no authorize_url in response")
	}
	return body.AuthorizeURL, nil
}

// cloudDesktopExchange redeems a single-use SSO ticket for tokens. The response
// is shaped like a login (access token in body, refresh in Set-Cookie), so it
// reuses cloudAuthCall's parsing.
func cloudDesktopExchange(ctx context.Context, hc *http.Client, baseURL, ticket string) (cloudAuthResult, error) {
	if ticket == "" {
		return cloudAuthResult{}, errors.New("cloudDesktopExchange: empty ticket")
	}
	return cloudAuthCall(ctx, hc, http.MethodPost, baseURL, "/api/auth/desktop/exchange", map[string]string{"ticket": ticket}, nil)
}

// cloudLogin performs a native password login against a cloud instance.
func cloudLogin(ctx context.Context, hc *http.Client, baseURL, email, password string) (cloudAuthResult, error) {
	body := map[string]string{"email": email, "password": password}
	return cloudAuthCall(ctx, hc, http.MethodPost, baseURL, "/api/auth/login", body, nil)
}

// cloudRefresh exchanges a refresh token for a fresh access token. The
// refresh token travels in the X-Iterion-Refresh header (the cloud reads it
// there or from the cookie); the rotated refresh comes back as a Set-Cookie.
func cloudRefresh(ctx context.Context, hc *http.Client, baseURL, refreshToken string) (cloudAuthResult, error) {
	if refreshToken == "" {
		return cloudAuthResult{}, errors.New("cloudRefresh: empty refresh token")
	}
	headers := map[string]string{"X-Iterion-Refresh": refreshToken}
	return cloudAuthCall(ctx, hc, http.MethodPost, baseURL, "/api/auth/refresh", nil, headers)
}

// cloudLogout revokes a refresh token server-side. Best-effort: a non-2xx
// is returned as an error but callers still clear local state.
func cloudLogout(ctx context.Context, hc *http.Client, baseURL, refreshToken string) error {
	endpoint, err := cloudEndpoint(baseURL, "/api/auth/logout")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	if refreshToken != "" {
		req.Header.Set("X-Iterion-Refresh", refreshToken)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("logout returned %d", resp.StatusCode)
	}
	return nil
}

// cloudAuthCall is the shared login/refresh request path: it marshals the
// optional JSON body, applies extra headers, and on 2xx parses the auth
// response (access token from the body, refresh token from the Set-Cookie,
// expiry from expires_at or the JWT). A non-2xx yields a *cloudAuthError.
func cloudAuthCall(ctx context.Context, hc *http.Client, method, baseURL, apiPath string, body any, headers map[string]string) (cloudAuthResult, error) {
	var out cloudAuthResult
	endpoint, err := cloudEndpoint(baseURL, apiPath)
	if err != nil {
		return out, err
	}

	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return out, err
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return out, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return out, fmt.Errorf("%s %s: %w", method, apiPath, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode/100 != 2 {
		return out, &cloudAuthError{Status: resp.StatusCode, Message: parseCloudErrorBody(raw)}
	}

	var parsed struct {
		User struct {
			ID           string `json:"id"`
			Email        string `json:"email"`
			Name         string `json:"name"`
			IsSuperAdmin bool   `json:"is_super_admin"`
		} `json:"user"`
		ActiveOrg   string `json:"active_org_id"`
		ActiveTeam  string `json:"active_team_id"`
		AccessToken string `json:"access_token"`
		ExpiresAt   string `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return out, fmt.Errorf("decode auth response: %w", err)
	}
	if parsed.AccessToken == "" {
		// The cloud only omits access_token for browser clients; a native
		// client that gets none means the server mistook us for a browser
		// (or an unexpected shape). Fail loudly rather than silently.
		return out, errors.New("cloud auth response carried no access_token (is this a browser-scoped endpoint?)")
	}

	out.AccessToken = parsed.AccessToken
	out.AccessExpiry = parseCloudExpiry(parsed.ExpiresAt, parsed.AccessToken)
	out.RefreshToken = harvestRefreshCookie(resp.Cookies())
	out.User = cloudUserSummary{
		ID:           parsed.User.ID,
		Email:        parsed.User.Email,
		Name:         parsed.User.Name,
		IsSuperAdmin: parsed.User.IsSuperAdmin,
		ActiveOrgID:  parsed.ActiveOrg,
		ActiveTeamID: parsed.ActiveTeam,
	}
	return out, nil
}

// stripSetCookies removes Set-Cookie response headers whose cookie name is in
// names, so the cloud's auth cookies never reach (or are re-sent from) the
// wails webview — the desktop tunnels auth via the Bearer header instead.
// Used by the cloud proxy's ModifyResponse; kept here (build-tag-free) so it
// is unit-testable without the webkit toolchain.
func stripSetCookies(resp *http.Response, names ...string) {
	lines := resp.Header["Set-Cookie"]
	if len(lines) == 0 {
		return
	}
	kept := lines[:0]
	for _, line := range lines {
		drop := false
		for _, n := range names {
			if strings.HasPrefix(line, n+"=") {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		resp.Header.Del("Set-Cookie")
		return
	}
	resp.Header["Set-Cookie"] = kept
}

// harvestRefreshCookie returns the value of the iterion_refresh cookie, or
// "" if absent (a refresh call may not rotate it on every hop).
func harvestRefreshCookie(cookies []*http.Cookie) string {
	for _, c := range cookies {
		if c.Name == cloudRefreshCookieName {
			return c.Value
		}
	}
	return ""
}

// parseCloudErrorBody pulls a human message out of an error response body,
// tolerating {"error":...} / {"message":...} shapes, else returns a trimmed
// snippet.
func parseCloudErrorBody(raw []byte) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"error", "message", "detail"} {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// parseCloudExpiry resolves the access-token expiry: prefer the RFC3339
// expires_at from the body, fall back to decoding the JWT's exp claim, and
// finally to now + (15m - skew) so the refresh loop still has a sane target.
func parseCloudExpiry(expiresAt, accessToken string) time.Time {
	if expiresAt != "" {
		if t, err := time.Parse(time.RFC3339, expiresAt); err == nil {
			return t
		}
	}
	if t, ok := jwtExpiry(accessToken); ok {
		return t
	}
	return time.Now().Add(15*time.Minute - cloudRefreshSkew)
}

// jwtExpiry decodes (without verifying) the exp claim of a JWT. Verification
// is the server's job; the desktop only needs the expiry to schedule a
// refresh.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// ── cloudTokenJar ────────────────────────────────────────────────────────

// cloudTokenJar holds the live access token + refresh token for one cloud
// connection. The proxy's Bearer injector and GetSessionToken read access();
// the background loop and the 401-retry call refresh(). It persists the
// refresh token to the injected store (the OS keychain in production) and
// never to config.json.
type cloudTokenJar struct {
	mu       sync.RWMutex
	connID   string
	baseURL  string
	store    refreshStore
	hc       *http.Client
	access   string
	expiry   time.Time
	refresh  string
	loggedIn bool
}

// newCloudTokenJar builds a jar for a connection. It does not touch the
// store; call hydrate() to load a persisted refresh token or seed() after a
// fresh login.
func newCloudTokenJar(connID, baseURL string, store refreshStore) *cloudTokenJar {
	return &cloudTokenJar{
		connID:  connID,
		baseURL: baseURL,
		store:   store,
		hc:      newCloudHTTPClient(),
	}
}

func (j *cloudTokenJar) refreshKey() string { return cloudRefreshKeyPrefix + j.connID }

// AccessToken returns the current access JWT (may be "" before login /
// after clear). Used by the proxy injector and GetSessionToken.
func (j *cloudTokenJar) AccessToken() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.access
}

// LoggedIn reports whether the jar currently holds a usable session.
func (j *cloudTokenJar) LoggedIn() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.loggedIn && j.access != ""
}

// Expiry returns the current access-token expiry (zero if unknown).
func (j *cloudTokenJar) Expiry() time.Time {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.expiry
}

// seed installs a fresh login/refresh result and persists the refresh token
// to the store. A blank refresh token (an un-rotated refresh hop) keeps the
// previously stored one.
func (j *cloudTokenJar) seed(res cloudAuthResult) error {
	j.mu.Lock()
	j.access = res.AccessToken
	j.expiry = res.AccessExpiry
	if res.RefreshToken != "" {
		j.refresh = res.RefreshToken
	}
	j.loggedIn = j.access != ""
	refresh := j.refresh
	j.mu.Unlock()

	if refresh == "" {
		return nil
	}
	if err := j.store.Set(j.refreshKey(), refresh); err != nil {
		return fmt.Errorf("persist refresh token: %w", err)
	}
	return nil
}

// hydrate loads a persisted refresh token from the store (on app startup)
// without minting an access token. Returns false when none is stored.
func (j *cloudTokenJar) hydrate() (bool, error) {
	v, err := j.store.Get(j.refreshKey())
	if err != nil || v == "" {
		// A missing key is not an error condition for the caller.
		return false, nil
	}
	j.mu.Lock()
	j.refresh = v
	j.mu.Unlock()
	return true, nil
}

// refreshNow exchanges the stored refresh token for a fresh access token and
// re-seeds the jar. On an auth failure it clears the jar so callers surface
// re-login. Returns the auth error (if any) so the caller can decide.
func (j *cloudTokenJar) refreshNow(ctx context.Context) error {
	j.mu.RLock()
	refresh := j.refresh
	baseURL := j.baseURL
	j.mu.RUnlock()
	if refresh == "" {
		return errors.New("no refresh token; login required")
	}
	res, err := cloudRefresh(ctx, j.hc, baseURL, refresh)
	if err != nil {
		if ae := asCloudAuthError(err); ae != nil && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
			_ = j.clear()
		}
		return err
	}
	return j.seed(res)
}

// clear zeroes the in-memory session and deletes the persisted refresh
// token. Idempotent.
func (j *cloudTokenJar) clear() error {
	j.mu.Lock()
	j.access = ""
	j.refresh = ""
	j.expiry = time.Time{}
	j.loggedIn = false
	j.mu.Unlock()
	return j.store.Delete(j.refreshKey())
}

// storedRefresh returns the current refresh token (for a logout call before
// clearing). Empty when none.
func (j *cloudTokenJar) storedRefresh() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.refresh
}

// applyRotation updates the jar from tokens observed on a PROXIED response's
// Set-Cookie headers (e.g. an SPA-driven org/team switch rotates iterion_auth
// / iterion_refresh). Blank values are ignored, so a response that rotates
// only one cookie keeps the other. The access-token value is itself the JWT,
// so its expiry is decoded from it. Returns any store-persist error.
func (j *cloudTokenJar) applyRotation(accessCookie, refreshCookie string) error {
	j.mu.Lock()
	if accessCookie != "" {
		j.access = accessCookie
		if exp, ok := jwtExpiry(accessCookie); ok {
			j.expiry = exp
		}
		j.loggedIn = true
	}
	if refreshCookie != "" {
		j.refresh = refreshCookie
	}
	refresh := j.refresh
	j.mu.Unlock()

	if refreshCookie != "" && refresh != "" {
		return j.store.Set(j.refreshKey(), refresh)
	}
	return nil
}
