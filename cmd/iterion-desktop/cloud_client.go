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
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/SocialGouv/iterion/pkg/auth"
	iserver "github.com/SocialGouv/iterion/pkg/server"
)

const (
	// cloudRefreshKeyPrefix namespaces a connection's refresh token in the
	// OS keychain: "cloud_refresh:<connID>".
	cloudRefreshKeyPrefix = "cloud_refresh:"

	// Cookie names the cloud sets on every auth response. Taken FROM the
	// server rather than mirrored as literals: the client harvests the
	// refresh token out of Set-Cookie (it is deliberately not echoed in the
	// JSON body), so a name it does not recognise is a token it silently
	// fails to rotate. Recognition goes through iserver.SessionCookieMatches,
	// because a deployment may emit either spelling.
	cloudAuthCookieName    = iserver.AuthCookieName
	cloudRefreshCookieName = iserver.RefreshCookieName

	// cloudHTTPTimeout bounds a single auth request. Login/refresh are
	// small round-trips; a generous ceiling covers a slow remote.
	cloudHTTPTimeout = 30 * time.Second

	// cloudRefreshTimeout bounds one presentation of the refresh token,
	// tighter than cloudHTTPTimeout: a refresh whose answer is lost is
	// presented again with the same token, and the server redelivers that
	// token's successor only inside its reuse grace window (30 s by
	// default), measured from the rotation. Giving up here is what makes the
	// retry start inside that window. A refresh that succeeded after the
	// client gave up costs nothing: the retry receives the same successor.
	cloudRefreshTimeout = 10 * time.Second

	// cloudRefreshRetryTimeout bounds the ONE extra presentation a refresh
	// makes AT ONCE when its first answer is lost (cloudRefreshAnswerLost).
	// The retry must outlast the cloud's slowest SUCCESSFUL refresh: the
	// server bounds a refresh as a whole by auth.RefreshTimeout, and the
	// retry waits cloudRefreshRetryMargin longer, for the network both ways
	// and the server's own scheduling. A refusal that revokes runs its
	// revocation past that bound; losing its answer costs one more
	// presentation, which the revoked session refuses as well. The jar's
	// HTTP client (cloudHTTPTimeout) must not cut the retry first.
	//
	// The server's reuse window is not what this timeout fits inside: the
	// retry is JUDGED when it arrives, at most cloudRefreshTimeout after the
	// rotation, and the 30 s default window covers that; a successor the
	// cloud handed to nobody is completed by a presentation within five
	// minutes. A retry whose answer is lost after the cloud handed the
	// successor out leaves the next presentation — the loop's,
	// cloudRefreshRetryFloor later — past the window: the session ends there
	// and the shell asks for a new sign-in.
	cloudRefreshRetryTimeout = auth.RefreshTimeout + cloudRefreshRetryMargin

	// cloudRefreshRetryMargin is how much longer than the cloud's own bound
	// on a refresh (auth.RefreshTimeout) the retry waits for its answer.
	cloudRefreshRetryMargin = 10 * time.Second

	// cloudRefreshRetryFloor is the background loop's shortest wait between
	// two refresh attempts — after a transient failure, the next one. It is
	// also the first step of every back-off below.
	cloudRefreshRetryFloor = 15 * time.Second

	// cloudRefreshSkew is how long before access-token expiry the background
	// loop refreshes, and the floor used when an expiry can't be parsed.
	cloudRefreshSkew = 2 * time.Minute

	// cloudKeychainReadBackoffCap bounds the wait between two reads of a store
	// whose reads fail: the floor doubles per consecutive failure up to it.
	// Each read of a locked keyring is one OS unlock prompt (go-keyring's Get
	// unlocks the collection), so the rate is what the operator feels.
	cloudKeychainReadBackoffCap = 4 * time.Minute

	// cloudKeychainUnpromptedReads is how many consecutive failed reads the
	// jar makes on its own before it makes none until a user action
	// (cloudKeychainRetryHeader, or a sign-in). An operator who left six
	// unlock prompts unanswered over eight minutes has answered; what bounds
	// the prompts is this count, not the rate.
	cloudKeychainUnpromptedReads = 6

	// cloudSignOutBackoffCap bounds the wait between two presentations of a
	// sign-out the cloud has not confirmed. Nothing of the session is used
	// while it stands, so the presentations only have to outlive an outage,
	// not be quick.
	cloudSignOutBackoffCap = 5 * time.Minute

	// cloudSignOutTimeout bounds ONE presentation of a sign-out the jar makes
	// itself: the one a sign-in presents before it replaces the session
	// (prepareForSignIn), and each presentation of a sign-out held for a
	// session this connection replaced (presentHeldSignOuts). A sign-in waits
	// on the first, so it is tighter than cloudHTTPTimeout; a presentation
	// that does not answer inside it leaves the sign-out held and is made
	// again.
	cloudSignOutTimeout = 10 * time.Second
)

// refreshStore is the minimal secret-persistence surface the token jar
// needs. *Keychain (keychain.go) satisfies it structurally; tests inject a
// fake.
//
// Get answers ("", nil) when nothing is stored under the key. An error is a
// read that FAILED — a locked keyring, a Secret Service not up yet — and
// says nothing about what is stored: the jar never reads it as "no session".
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

// errNoCloudSession is what a refresh of an EMPTY jar returns: the desktop
// holds no credential, the same dead end as one the cloud rejected (a
// refused rehydrate clears the jar, and so does a sign-out from another
// device). It carries 401 so cloudRefreshRefused reads it without a second
// rule. Only a jar that READ its store and found nothing is empty: one whose
// read failed answers errCloudSessionUnread instead.
var errNoCloudSession = &cloudAuthError{Status: http.StatusUnauthorized, Message: "no refresh token; login required"}

// errCloudSessionUnread is what a refresh of a jar whose store read FAILED
// returns, wrapping the store's error: the desktop does not know whether it
// holds a session, so it can neither present one nor declare it over. Every
// caller reads it as a transient failure — the loop retries, the tunnel
// answers 503 — and the next refresh reads the store again.
var errCloudSessionUnread = errors.New("the stored cloud session could not be read from the keychain")

// errCloudSignOutPending is what a refresh of a jar whose sign-out the cloud
// has not confirmed returns: the operator ended that session, so the desktop
// presents it no more — only a new sign-in opens one. It carries 401 so every
// caller classifies it as a refusal (cloudRefreshRefused) without a second
// rule: the tunnel passes the cloud's own 401 to the pane and the shell asks
// for a sign-in, which is the way out.
var errCloudSignOutPending = &cloudAuthError{
	Status:  http.StatusUnauthorized,
	Message: "this session's sign-out was not confirmed by the cloud; sign in again",
}

// cloudSignOutPendingMark prefixes a connection's store entry while its
// sign-out waits for the cloud's confirmation. The mark and the token are ONE
// value on purpose: a restart reads them together, so it can neither adopt a
// session whose sign-out is pending nor lose the token that still has to end
// it. The prefix cannot be the start of a refresh token — the server's are
// base64url, which has no ':'.
const cloudSignOutPendingMark = "signout-pending:"

// encodeStoredSession renders a store entry: the connection's refresh token
// on the first line — marked when its sign-out is pending — then one line per
// session this connection REPLACED whose sign-out the cloud has not confirmed
// yet. They share the entry because they are one fact per connection, read and
// written together: a restart that reads the live session reads what still has
// to be signed out, and every write carries both, so no write can drop a token
// a live session on the cloud is waiting for. A refresh token is base64url, so
// it holds neither '\n' nor ':'.
func encodeStoredSession(refresh string, signOutPending bool, heldSignOuts []string) string {
	if signOutPending {
		refresh = cloudSignOutPendingMark + refresh
	}
	if len(heldSignOuts) == 0 {
		return refresh
	}
	return refresh + "\n" + strings.Join(heldSignOuts, "\n")
}

// decodeStoredSession reads a store entry written by encodeStoredSession. A
// single-line value is the token alone, which is what every entry written
// before the held sign-outs existed holds.
func decodeStoredSession(v string) (refresh string, signOutPending bool, heldSignOuts []string) {
	first, rest, _ := strings.Cut(v, "\n")
	for _, tok := range strings.Split(rest, "\n") {
		if tok != "" {
			heldSignOuts = append(heldSignOuts, tok)
		}
	}
	if token, ok := strings.CutPrefix(first, cloudSignOutPendingMark); ok {
		return token, true, heldSignOuts
	}
	return first, false, heldSignOuts
}

// mergeHeldSignOuts appends every token of add that into does not hold yet,
// keeping the order they were held in: one session is never presented twice
// in one pass, and reading the store does not duplicate what this process
// already holds.
func mergeHeldSignOuts(into, add []string) []string {
	for _, tok := range add {
		if tok != "" && !slices.Contains(into, tok) {
			into = append(into, tok)
		}
	}
	return into
}

// removeHeldSignOuts drops the presented tokens from the held sign-outs:
// their presentations are confirmed, so the jar and its entry carry them no
// more. A hold taken while the presentations ran — absent from presented —
// stays.
func removeHeldSignOuts(held, presented []string) []string {
	if len(presented) == 0 {
		return held
	}
	return slices.DeleteFunc(held, func(tok string) bool {
		return slices.Contains(presented, tok)
	})
}

// cloudBackoff is the floor doubled per attempt already made, capped: the
// first attempt waits cloudRefreshRetryFloor, the next twice that, and so on
// up to limit. attempts counts the attempts BEHIND the wait (0 → the floor).
func cloudBackoff(attempts int, limit time.Duration) time.Duration {
	d := cloudRefreshRetryFloor
	for i := 0; i < attempts && d < limit; i++ {
		d *= 2
	}
	if d > limit {
		return limit
	}
	return d
}

// cloudKeychainReadBackoff is how long after its failures-th consecutive
// failed read the jar asks the store again.
func cloudKeychainReadBackoff(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	return cloudBackoff(failures-1, cloudKeychainReadBackoffCap)
}

// cloudRefreshRefused reports whether err is the cloud REFUSING the
// credential presented: the session is over, and only a new sign-in reopens
// it. Every desktop caller that reacts to a refresh error classifies it
// here — the authenticating tunnel (pass the cloud's 401 to the pane and ask
// the shell for a re-login), the background refresh loop (emit
// cloud:auth-expired and stop) and the two activation paths (leave the
// connection unauthenticated) — so a jar with nothing to present and a jar
// whose token the cloud rejected end the same way. Anything else is a
// failure a later presentation may still be served.
func cloudRefreshRefused(err error) bool {
	ae := asCloudAuthError(err)
	return ae != nil && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden)
}

// cloudRefreshAnswerLost reports whether err leaves the cloud's answer to a
// refresh UNKNOWN — the server may have rotated the token presented and
// recorded its successor, with nothing the desktop holds saying so. Such a
// refresh presents the same token again at once (refreshOnce): the
// rotation's successor is redelivered only inside the server's reuse window,
// measured from the rotation.
//
//   - the deadline the desktop puts on a presentation, or the HTTP client's
//     own: the answer may be on its way;
//   - a transport error — an answer that never arrived says nothing about
//     what the server did with the request;
//   - 500 / 502 / 503 / 504: the cloud answers 500 for a session-store
//     failure after a rotation it already recorded, and a gateway emits the
//     other three for an answer it lost.
//
// Everything else is an answer the desktop received: a refusal (401/403), a
// rate limit (429 — it asks for fewer requests, not one more), a 4xx on the
// request itself. An answer that did not PARSE is received too, and a retry
// is served the same successor in the same shape, so it fails the same way.
func cloudRefreshAnswerLost(err error) bool {
	if err == nil {
		return false
	}
	if ae := asCloudAuthError(err); ae != nil {
		switch ae.Status {
		case http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ue *url.Error
	return errors.As(err, &ue)
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

// cloudMintWSTicket mints a single-use WS ticket so the SPA can open a cloud
// WebSocket with ?ticket=<t> instead of the access JWT in the URL. Authenticated
// with the current access token via the native client (Bearer header).
func cloudMintWSTicket(ctx context.Context, hc *http.Client, baseURL, accessToken string) (string, error) {
	if accessToken == "" {
		return "", errors.New("cloudMintWSTicket: no access token")
	}
	endpoint, err := cloudEndpoint(baseURL, "/api/ws/ticket")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("mint ws ticket: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ws ticket endpoint returned %d: %s", resp.StatusCode, parseCloudErrorBody(raw))
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Ticket == "" {
		return "", errors.New("ws ticket response missing ticket")
	}
	return body.Ticket, nil
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

// cloudLogout revokes a refresh token server-side. Only a 2xx ends the
// session: a non-2xx (the cloud answers 500 "identity unavailable" when its
// revocation failed) or no answer at all is returned as an error, and the
// session is still live — the caller keeps the token to present again.
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("logout returned %d: %s", resp.StatusCode, parseCloudErrorBody(raw))
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

// harvestRefreshCookie returns the value of the refresh cookie, or "" if
// absent (a refresh call may not rotate it on every hop).
//
// Matching goes through SessionCookieMatches, never a literal: the server
// emits the `__Host-` spelling wherever it can, and a miss here does not
// fail — it returns "", which seed() reads as "not rotated this hop" and
// keeps the previous token. The next hop replays it, the server reads a
// replay as token theft, and every session that user holds is revoked.
func harvestRefreshCookie(cookies []*http.Cookie) string {
	for _, c := range cookies {
		if iserver.SessionCookieMatches(c.Name, cloudRefreshCookieName) {
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
	// read reports whether refresh is what the store holds, or what the
	// cloud answered since: set by a hydrate that answered (a token, or
	// nothing stored), a seed, a rotated refresh cookie and a clear. A jar
	// whose read FAILED is not read — its empty refresh is not "no session".
	read bool
	// readErr, readFailedAt and readFailures are the last failed store read
	// and how many failed in a row: while reads fail, a refresh asks the store
	// again on a back-off (cloudKeychainReadBackoff), and after
	// cloudKeychainUnpromptedReads of them it asks only when a user action
	// does (readOnUserAction). A locked keyring answers each read with an
	// unlock prompt.
	readErr      error
	readFailedAt time.Time
	readFailures int
	// readOnUserAction lets the next read happen at once, back-off or budget
	// notwithstanding: the operator asked for it (the pane's retry control),
	// so the unlock prompt it costs is one they expect.
	readOnUserAction bool
	// signOutPending marks a sign-out the cloud did not confirm: the jar
	// presents the session no more (no access token, no renewal) and the
	// sign-out is presented again until a 2xx. The clear a confirmed sign-out
	// ends with drops it; a sign-in presents it first and, unconfirmed, moves
	// its token to heldSignOuts (prepareForSignIn).
	signOutPending bool
	// heldSignOuts are the refresh tokens of sessions this connection
	// REPLACED and whose sign-out the cloud has not confirmed: a sign-in over
	// a held sign-out keeps the token here instead of overwriting it, since it
	// is the only thing that can still end that session, and the background
	// loop presents them until the cloud confirms them.
	heldSignOuts []string
	// epoch counts the moments the jar stopped holding the session it held —
	// a clear (a confirmed sign-out, a refusal the cloud answered) and a
	// sign-out held: a refresh in flight across one does not seed the jar
	// (seedAt).
	epoch uint64
	// retired marks a jar another jar of the same connection replaced — a
	// sign-in installed a new one over the same keychain entry (the ONE JAR
	// PER CONNECTION transition). Its writes stand down (writeStoredSession-
	// Locked, syncStoredSession, seedLocked): an answer still in flight when
	// it was retired must not land on the entry the new jar owns. Its loop
	// is stopped by the installer.
	retired bool
	// refreshing makes refreshNow single-flight: callers that overlap share
	// one refresh, so the stored token is presented once.
	refreshing singleflight.Group
	// wake carries a request to refresh now from a caller that cannot retry
	// on its own cadence (the authenticating tunnel) to the background
	// refresh loop. Buffered, and a send that would block is dropped: a
	// flood of failing pane requests leaves ONE pending wake, and the loop's
	// floor bounds what it does with it.
	wake chan struct{}
}

// newCloudTokenJar builds a jar for a connection. It does not touch the
// store; call hydrate() to load a persisted refresh token or seed() after a
// fresh login. A jar that did neither reads the store at its first refresh.
// It is called only by resolveConnectionJar: every path that needs a
// connection's jar resolves through it, so no path builds a second live jar
// of one connection (one jar per connection).
func newCloudTokenJar(connID, baseURL string, store refreshStore) *cloudTokenJar {
	return &cloudTokenJar{
		connID:  connID,
		baseURL: baseURL,
		store:   store,
		hc:      newCloudHTTPClient(),
		wake:    make(chan struct{}, 1),
	}
}

// resolveConnectionJar resolves the connection's ONE jar: prev — the live jar
// the connection registry holds, the one the pane tunnel reads — when there
// is one, or a fresh jar over the same keychain entry (a restart, a
// connection no pane has open). Every path that acts for a connection
// resolves through here instead of building its own jar, so no second jar
// writes the connection's keychain key or presents from a session another
// jar rotated: a second live jar's next presentation lands outside the
// server's reuse grace of the live jar's rotation, which revokes the lineage
// and deletes the entry the live jar owns.
func resolveConnectionJar(prev *cloudTokenJar, connID, baseURL string, store refreshStore) *cloudTokenJar {
	if prev != nil {
		return prev
	}
	return newCloudTokenJar(connID, baseURL, store)
}

// activateConnectionJar is the machine's startup/switch activation: it
// resolves the connection's ONE jar and renews the session through it — that
// renewal is the fresh access token the activation hands the shell. A jar it
// just built is hydrated first (its renewal is then the startup
// rehydration); the registry's jar is already read, so only the renewal runs,
// through the jar the registry and the pane tunnel hold. A jar whose sign-out
// is pending renews nothing (presentRefresh refuses) — the loop presents the
// sign-out instead, and logCloudRehydrate says so.
func activateConnectionJar(ctx context.Context, prev *cloudTokenJar, connID, baseURL string, store refreshStore) *cloudTokenJar {
	jar := resolveConnectionJar(prev, connID, baseURL, store)
	if prev != nil {
		logCloudRehydrate(connID, jar.refreshNow(ctx))
		return jar
	}
	hydrated, err := jar.hydrate()
	if err != nil {
		log.Printf("desktop: cloud hydrate for %s failed: %v (the session is kept: its refreshes read the keychain again)", connID, err)
	}
	if hydrated {
		logCloudRehydrate(connID, jar.refreshNow(ctx))
	}
	return jar
}

// kickRefresh asks the background refresh loop to refresh as soon as its
// floor allows, instead of waiting for its expiry-based timer. The tunnel
// calls it when a refresh it started was not served: the token the jar still
// holds may have been rotated by that attempt, and the loop's retry is what
// presents it again while the server still redelivers its successor. Never
// blocks, and never queues more than one wake.
func (j *cloudTokenJar) kickRefresh() {
	select {
	case j.wake <- struct{}{}:
	default:
	}
}

// woken is the channel the background refresh loop selects on beside its
// own timer. Nil for a jar built without newCloudTokenJar, which the loop
// reads as "never woken".
func (j *cloudTokenJar) woken() <-chan struct{} { return j.wake }

func (j *cloudTokenJar) refreshKey() string { return cloudRefreshKeyPrefix + j.connID }

// retire ends a jar's writes: a sign-in installed another jar over the same
// keychain entry (the ONE JAR PER CONNECTION transition), and an answer of
// this jar's still in flight must not land on the entry the new one owns.
// Reads finish; their answers are dropped at the write.
func (j *cloudTokenJar) retire() {
	j.mu.Lock()
	j.retired = true
	j.mu.Unlock()
}

// AccessToken returns the current access JWT (may be "" before login /
// after clear). Used by the proxy injector and GetSessionToken.
//
// It is the one place every credential the desktop sends for this connection
// comes from — the tunnel's Bearer, the pane's WS ticket, the shell's
// GetSessionToken — so a session whose sign-out is pending is withheld here
// and nowhere else: those requests reach the cloud unauthenticated and are
// answered 401, which is what the operator asked for.
func (j *cloudTokenJar) AccessToken() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if j.signOutPending {
		return ""
	}
	return j.access
}

// LoggedIn reports whether the jar currently holds a usable session.
func (j *cloudTokenJar) LoggedIn() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.loggedIn && j.access != "" && !j.signOutPending
}

// SignOutPending reports whether a sign-out of this connection is waiting for
// the cloud to confirm it. Read by the background loop, which presents it
// again instead of renewing the session.
func (j *cloudTokenJar) SignOutPending() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.signOutPending
}

// Expiry returns the current access-token expiry (zero if unknown).
func (j *cloudTokenJar) Expiry() time.Time {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.expiry
}

// seed installs the session a SIGN-IN opened and persists its refresh token
// to the store. A blank refresh token (an un-rotated refresh hop) keeps the
// previously stored one.
//
// It is the one door every sign-in path goes through, so the sign-out this
// connection owes the cloud is presented HERE (prepareForSignIn) and not at
// each path: the store entry a sign-in replaces may be the only copy of the
// token that can end the session the operator signed out of. A path that
// prepared the jar itself — with the app's context and the connection's live
// jar — finds this a no-op.
func (j *cloudTokenJar) seed(res cloudAuthResult) error {
	if err := j.prepareForSignIn(context.Background(), nil); err != nil {
		return err
	}
	j.mu.Lock()
	return j.seedLocked(res)
}

// seedAt is seed for what a refresh that started at epoch was answered: a
// clear since then — a confirmed sign-out, a refusal — or a sign-out held
// wins, and the answer is dropped (errNoCloudSession). A refresh's answer is
// the SAME session renewed, so installing it would put back in use the very
// session the jar stopped holding.
func (j *cloudTokenJar) seedAt(res cloudAuthResult, epoch uint64) error {
	j.mu.Lock()
	if j.epoch != epoch {
		j.mu.Unlock()
		return errNoCloudSession
	}
	return j.seedLocked(res)
}

// seedLocked is seed's body; it releases j.mu, which the caller holds.
//
// A seed installs the session a SIGN-IN opened, so it clears a pending
// sign-out: the sign-in presented it first (prepareForSignIn), and what it
// could not confirm is held in heldSignOuts, which this entry carries on. A
// refresh's answer renews the same session instead and reaches the jar
// through seedAt, which drops it when a sign-out of that session stands.
//
// The entry is written under the SAME lock the state changed under, so a
// hold taken between the caller's decision and this write is re-encoded
// into it; seedAt's epoch check under the same critical section is what
// drops a whole answer decided before a hold.
func (j *cloudTokenJar) seedLocked(res cloudAuthResult) error {
	j.access = res.AccessToken
	j.expiry = res.AccessExpiry
	if res.RefreshToken != "" {
		j.refresh = res.RefreshToken
	}
	j.loggedIn = j.access != ""
	j.read = true
	j.signOutPending = false
	if j.retired {
		// Another jar was installed over this connection's entry; nothing
		// this jar was answered goes to the keychain.
		j.mu.Unlock()
		return nil
	}
	if j.refresh == "" && len(j.heldSignOuts) == 0 {
		j.mu.Unlock()
		return nil
	}
	err := j.store.Set(j.refreshKey(), encodeStoredSession(j.refresh, j.signOutPending, j.heldSignOuts))
	j.mu.Unlock()
	if err != nil {
		return fmt.Errorf("persist refresh token: %w", err)
	}
	return nil
}

// writeStoredSession writes the connection's store entry from the jar's
// state: the refresh token with the pending-sign-out mark when one stands,
// and the tokens of the sessions whose sign-out is held beside it. Every
// write of that entry goes through here (under the write lock) or through
// seedLocked, so no write can drop a mark or a held sign-out while the token
// that can still end its session lives on.
//
// It never deletes: a jar that holds nothing writes nothing, so a seed that
// harvested no refresh cookie leaves the stored token alone. Where a token
// LEAVES the jar, syncStoredSession is what removes the entry.
func (j *cloudTokenJar) writeStoredSession() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.writeStoredSessionLocked()
}

// writeStoredSessionLocked is writeStoredSession under the write lock the
// caller holds: it encodes the entry from the state ITS turn sees, so two
// writes of one entry are serialised and a write snapshotted before a hold
// never lands after it (the hold's own write, or the entry it encodes, wins).
func (j *cloudTokenJar) writeStoredSessionLocked() error {
	if j.retired {
		return nil
	}
	if j.refresh == "" && len(j.heldSignOuts) == 0 {
		return nil
	}
	if err := j.store.Set(j.refreshKey(), encodeStoredSession(j.refresh, j.signOutPending, j.heldSignOuts)); err != nil {
		return fmt.Errorf("persist refresh token: %w", err)
	}
	return nil
}

// syncStoredSession makes the store entry match a jar a token LEFT — a
// confirmed sign-out, a refusal, a sign-out the cloud confirmed late: it
// deletes the entry when the jar has nothing left to keep, and rewrites it
// when it still holds a token or a held sign-out.
func (j *cloudTokenJar) syncStoredSession() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.retired {
		return nil
	}
	empty := j.refresh == "" && len(j.heldSignOuts) == 0
	if empty {
		return j.store.Delete(j.refreshKey())
	}
	return j.writeStoredSessionLocked()
}

// hydrate loads a persisted refresh token from the store (on app startup)
// without minting an access token. Returns false when none is stored, and
// the store's error when the read failed: the jar then stays unread, and
// its refreshes answer errCloudSessionUnread and read the store again
// (ensureRead).
func (j *cloudTokenJar) hydrate() (bool, error) {
	v, err := j.store.Get(j.refreshKey())
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.read {
		// A seed, a rotated refresh cookie or a clear landed while the store
		// was being read: what it installed is newer than what was read.
		return j.refresh != "", nil
	}
	if err != nil {
		j.readErr, j.readFailedAt = err, time.Now()
		j.readFailures++
		return false, fmt.Errorf("%w: %w", errCloudSessionUnread, err)
	}
	j.read, j.readErr, j.readFailedAt, j.readFailures = true, nil, time.Time{}, 0
	if v == "" {
		return false, nil
	}
	refresh, pending, held := decodeStoredSession(v)
	j.refresh = refresh
	// The mark only ever goes ON here: a sign-out this process holds in memory
	// — one the store could not be written for, because the keyring that
	// failed to be read cannot be written either — is not unmarked by reading
	// an entry that predates it.
	if pending {
		j.signOutPending = true
	}
	// Likewise the sign-outs held for sessions this connection replaced: a
	// restart adopts the stored ones, and a hold this process took is kept.
	j.heldSignOuts = mergeHeldSignOuts(j.heldSignOuts, held)
	// A tokenless in-memory hold — the keychain was locked when the sign-out
	// was asked, so no token was ever read — is bound to the ENTRY: this read
	// is its adoption. An entry that carries the hold's named form (a mark,
	// or the held sign-outs a sign-in carried beside the session it opened)
	// covers it: the memory hold stands down for the named one. A bare entry
	// is the session the hold ends: it names the token just read, and only
	// that one — a token a sign-in wrote after the hold reaches this jar only
	// beside a named line, which stood the hold down above (F12).
	if j.signOutPending && !pending {
		if len(held) > 0 {
			j.signOutPending = false
		}
	}
	return refresh != "", nil
}

// readIfUnread reads the store when the jar has not read it yet.
func (j *cloudTokenJar) readIfUnread() error {
	j.mu.RLock()
	read := j.read
	j.mu.RUnlock()
	if read {
		return nil
	}
	_, err := j.hydrate()
	return err
}

// ensureRead is readIfUnread for a refresh: while the store's reads fail, it
// is asked again on a back-off (cloudKeychainReadBackoff), and only
// cloudKeychainUnpromptedReads times in all before it waits for a user
// action. A refresh that asks for none answers the last failure without
// touching the store.
//
// Both bounds are load-bearing and neither implies the other: the back-off
// spaces the OS unlock prompts a locked keyring charges per read, and the
// budget ends them — a prompt dismissed six times is a decision, and a
// connection nobody is signing into has nothing to gain from a seventh.
func (j *cloudTokenJar) ensureRead() error { return j.ensureReadWithBudget(true) }

// ensureReadForSignOut is ensureRead for presenting a sign-out the operator
// asked for: the back-off still spaces the reads, since each one is an unlock
// prompt, but the unprompted budget does NOT end them. The budget says a
// connection nobody is signing into gains nothing from another prompt; a held
// sign-out is the opposite — the operator ended that session, the app promised
// to present the sign-out until the cloud confirms it, and the keychain holds
// the only credential that can. The prompts stay bounded by the sign-out
// back-off the loop presents on (cloudSignOutBackoffCap).
func (j *cloudTokenJar) ensureReadForSignOut() error { return j.ensureReadWithBudget(false) }

// ensureReadWithBudget is the body of both: budgeted applies
// cloudKeychainUnpromptedReads, and the back-off applies either way.
func (j *cloudTokenJar) ensureReadWithBudget(budgeted bool) error {
	j.mu.Lock()
	if j.read {
		j.mu.Unlock()
		return nil
	}
	asked := j.readOnUserAction
	j.readOnUserAction = false
	lastErr, failedAt, failures := j.readErr, j.readFailedAt, j.readFailures
	j.mu.Unlock()
	if lastErr != nil && !asked {
		spent := budgeted && failures >= cloudKeychainUnpromptedReads
		if spent || time.Since(failedAt) < cloudKeychainReadBackoff(failures) {
			return fmt.Errorf("%w: %w", errCloudSessionUnread, lastErr)
		}
	}
	return j.readIfUnread()
}

// allowKeychainRead lets the next read of a store whose reads fail happen at
// once (ensureRead): the operator asked for it. It does not forget the
// failures — the back-off keeps growing — it only lifts what they put in
// front of the next read.
func (j *cloudTokenJar) allowKeychainRead() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.readOnUserAction = true
}

// refreshNow exchanges the stored refresh token for a fresh access token and
// re-seeds the jar. On an auth failure it clears the jar so callers surface
// re-login. Returns the auth error (if any) so the caller can decide.
//
// It is single-flight: the background loop and every request the proxy
// retries on a 401 may call it at once, and callers that overlap share ONE
// refresh rather than each presenting the stored token. The shared refresh
// runs detached from any one caller's cancellation (the HTTP client's
// timeout bounds it): a caller that gives up stops waiting, never the
// refresh, whose rotated token the jar would otherwise lose. ctx bounds only
// this caller's wait — a refresh whose first answer is lost presents the
// token once more at once (refreshOnce), so a caller can wait
// cloudRefreshTimeout + cloudRefreshRetryTimeout on one call.
func (j *cloudTokenJar) refreshNow(ctx context.Context) error {
	done := j.refreshing.DoChan("refresh", func() (any, error) {
		return nil, j.refreshOnce(context.WithoutCancel(ctx))
	})
	select {
	case res := <-done:
		return res.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// refreshOnce is one refresh of the stored token (see refreshNow): one
// presentation bounded by cloudRefreshTimeout and, when its answer is lost
// (cloudRefreshAnswerLost), ONE more AT ONCE under
// cloudRefreshRetryTimeout. The retry is here, inside the single flight, so
// every caller gets it — the background loop, whose own retries wait
// cloudRefreshRetryFloor, and the tunnel, which schedules nothing. It
// presents a token the server may have rotated while that rotation's
// successor is still redeliverable, and it is one attempt, not a cadence.
//
// Both presentations carry the SAME token: only a presentation the cloud
// answered seeds the jar, and such a presentation is never made again — a
// second presentation of a successor would be a second rotation, which is
// what turns another client's in-flight duplicate into theft.
func (j *cloudTokenJar) refreshOnce(ctx context.Context) error {
	err := j.presentRefresh(ctx, cloudRefreshTimeout)
	if err == nil || !cloudRefreshAnswerLost(err) {
		return err
	}
	return j.presentRefresh(ctx, cloudRefreshRetryTimeout)
}

// presentRefresh presents the stored refresh token once, bounded by timeout,
// and seeds the jar with what comes back. A refusal clears the jar: the
// credential is dead and only a new sign-in replaces it. A jar whose store
// read failed presents nothing and answers errCloudSessionUnread, and one
// whose sign-out the cloud has not confirmed answers errCloudSignOutPending —
// renewing that session is what the operator asked to end.
func (j *cloudTokenJar) presentRefresh(ctx context.Context, timeout time.Duration) error {
	if err := j.ensureRead(); err != nil {
		return err
	}
	j.mu.RLock()
	refresh := j.refresh
	baseURL := j.baseURL
	epoch := j.epoch
	pending := j.signOutPending
	j.mu.RUnlock()
	if pending {
		// Not through clear(): the token is the only thing that can still end
		// the session on the cloud, and the pending sign-out presents it.
		return errCloudSignOutPending
	}
	if refresh == "" {
		return errNoCloudSession
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := cloudRefresh(ctx, j.hc, baseURL, refresh)
	if err != nil {
		if cloudRefreshRefused(err) {
			j.clearIfStillHolds(refresh)
		}
		return err
	}
	return j.seedAt(res, epoch)
}

// clearIfStillHolds clears the jar and its store entry only when the jar
// still holds the token a refusal answered: the refusal ends the SESSION it
// named, and a token the jar no longer holds — a sign-in or a rotation
// replaced it while the refusal was in flight — says nothing about what the
// jar holds now (F13). The comparison runs under the lock the state changes
// under; a session a sign-in carried beside the refused one (heldSignOuts)
// stays held, and the entry keeps it.
func (j *cloudTokenJar) clearIfStillHolds(refresh string) {
	j.mu.Lock()
	if j.refresh != refresh {
		j.mu.Unlock()
		return
	}
	j.access = ""
	j.refresh = ""
	j.expiry = time.Time{}
	j.loggedIn = false
	j.read = true
	j.signOutPending = false
	j.epoch++
	j.mu.Unlock()
	_ = j.syncStoredSession()
}

// clear zeroes the in-memory session and its store entry when nothing is
// held beside it. Idempotent. The held sign-outs — sessions this connection
// replaced, whose sign-out the cloud has not confirmed — are NOT zeroed, and
// syncStoredSession writes the entry on from them alone: for a jar that READ
// its entry, they are the only copy of the token that can still end those
// sessions. A jar that never read the entry must not end a sign-out here:
// that is RemoveConnection's machine transition, removeConnection — the
// presents-what-it-owes-then-deletes this used to skip (F5).
func (j *cloudTokenJar) clear() error {
	j.mu.Lock()
	j.access = ""
	j.refresh = ""
	j.expiry = time.Time{}
	j.loggedIn = false
	j.read = true
	j.signOutPending = false
	j.epoch++
	j.mu.Unlock()
	return j.syncStoredSession()
}

// removeConnection is the machine's CONNECTION REMOVED transition: it reads
// the entry and presents what the connection owes the cloud — its pending
// sign-out, and the sign-outs held for sessions it replaced — then ends the
// jar and deletes the entry. A presentation that cannot be made (the keychain
// cannot be read, the cloud does not answer, it answers with an error that
// decides nothing) refuses the removal with the reason: the entry is the only
// copy of the token that can still end a session the operator ended, and
// deleting it would leave that session live until the refresh TTL with
// nothing able to end it (F5, invariant I2). A refusal that names the
// credential dead (401/403) is the session already being over. What the
// removal never does is discard a hold.
func (j *cloudTokenJar) removeConnection(ctx context.Context) error {
	if err := j.readIfUnread(); err != nil {
		return fmt.Errorf("remove connection %s: the keychain entry could not be read (%w) — it may carry a sign-out this computer still has to present, or the only copy of a session that stays live: unlock your keychain and remove the connection again", j.baseURL, err)
	}
	j.mu.RLock()
	refresh, pending, held, baseURL := j.refresh, j.signOutPending, slices.Clone(j.heldSignOuts), j.baseURL
	j.mu.RUnlock()
	var errs []error
	present := func(tok string) error {
		lctx, cancel := context.WithTimeout(ctx, cloudSignOutTimeout)
		defer cancel()
		err := cloudLogout(lctx, j.hc, baseURL, tok)
		switch {
		case err == nil, cloudRefreshRefused(err):
			// Confirmed, or the session it ends is already over.
			return nil
		default:
			return fmt.Errorf("the sign-out of a session this connection still owes the cloud was not confirmed (%w) — remove the connection again once the cloud answers, or sign in and sign out once more to present it", err)
		}
	}
	if pending && refresh != "" {
		if err := present(refresh); err != nil {
			errs = append(errs, err)
		}
	}
	for _, tok := range held {
		if err := present(tok); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	j.mu.Lock()
	j.access = ""
	j.refresh = ""
	j.expiry = time.Time{}
	j.loggedIn = false
	j.read = true
	j.signOutPending = false
	j.heldSignOuts = nil
	j.epoch++
	j.mu.Unlock()
	return j.syncStoredSession()
}

// closeForPane is the machine's PANE CLOSED transition: the pane is closing
// and the registry entry — this jar and the loop presenting what it owes —
// goes with it. A jar that owes the cloud nothing closes as today: the
// keychain entry is the persisted state, and a reopen presents any mark it
// carries before adopting the session it names. A jar that OWES a
// presentation (a sign-out the cloud has not confirmed — the mark, or sign-
// outs held beside the live session) does not let the close drop it: the
// keychain entry is first made to carry the hold — read, then rewritten; a
// jar whose read failed is never written from — and an entry the keychain
// still refuses carries the hold by presentation: what the jar owes is
// presented once (the connection-removed discipline), and only a close whose
// presentations are all confirmed proceeds. What the close presents, and
// what it refuses on, is the state the entry read left — not the snapshot
// the early return decided on. Once every presentation is confirmed the jar
// ends exactly what the presentations named: the pending mark's own session,
// and the held tokens presented; a session no presentation named — a live
// session the stood-down mark rode beside — stays. Anything else refuses the
// close with the reason: the jar and its loop are the last presenter of that
// sign-out, and the pane stays open with them.
func (j *cloudTokenJar) closeForPane(ctx context.Context) error {
	j.mu.RLock()
	pending, held := j.signOutPending, slices.Clone(j.heldSignOuts)
	j.mu.RUnlock()
	if !pending && len(held) == 0 {
		return nil
	}
	// A reopen reads the keychain entry, not this jar's memory: the hold
	// survives the close only ON the entry.
	if err := j.readIfUnread(); err != nil {
		return j.refusePaneClose(pending, held, fmt.Errorf("the keychain entry could not be read: %w", err))
	}
	// The read moves the jar's state: an entry that carries held sign-outs
	// stands a memory mark down, and one that carries a mark stands it up.
	// Presenting the snapshot above could send a session that snapshot named
	// and the read state does not — the live session beside a stood-down
	// mark.
	j.mu.RLock()
	pending, held = j.signOutPending, slices.Clone(j.heldSignOuts)
	j.mu.RUnlock()
	if err := j.writeStoredSession(); err == nil {
		// The entry carries the hold; the close cannot lose it, and a reopen
		// presents it before adopting the session it names.
		return nil
	}
	// The keychain does not carry the hold and cannot be made to: present
	// what the jar owes, once. Everything confirmed → the jar ends exactly
	// what the presentations named. Anything else → the close is refused.
	if unconfirmed := j.presentOwedForClose(ctx); unconfirmed != nil {
		return j.refusePaneClose(pending, held, errors.New("the keychain entry could not be written"), unconfirmed)
	}
	j.mu.Lock()
	if pending {
		// The mark named this jar's own session and the presentation ended
		// it; the jar serves it no more.
		j.access, j.refresh, j.expiry, j.loggedIn = "", "", time.Time{}, false
	}
	j.signOutPending = false
	j.heldSignOuts = removeHeldSignOuts(j.heldSignOuts, held)
	j.epoch++
	j.mu.Unlock()
	// The entry follows the jar: deleted when nothing remains, rewritten
	// around what stays. A write it still refuses leaves the entry as it
	// was, and the answer names the remedies instead of the raw store error.
	if err := j.syncStoredSession(); err != nil {
		j.mu.RLock()
		stillPending, stillHeld := j.signOutPending, len(j.heldSignOuts)
		j.mu.RUnlock()
		if stillPending || stillHeld > 0 {
			return j.refusePaneClose(pending, held, err)
		}
		return fmt.Errorf("the pane stays open: the keychain entry could not be written (%v) — keep this pane open, retry once the keychain answers, or remove the connection", err)
	}
	return nil
}

// presentOwedForClose presents the sign-outs the jar owes at its close — the
// pending one and the held ones — once each, and returns what the cloud did
// not confirm. It reads what the jar owes under one lock at its top: the
// caller's pre-read snapshot describes the jar before its entry read, and
// presenting from it can send a session that snapshot named and the read
// state does not (the live session beside a stood-down mark). A refusal that
// names the credential dead is a session already over: confirmed, as the
// connection-removed presentation reads it.
func (j *cloudTokenJar) presentOwedForClose(ctx context.Context) error {
	j.mu.RLock()
	refresh, pending, held, baseURL := j.refresh, j.signOutPending, slices.Clone(j.heldSignOuts), j.baseURL
	j.mu.RUnlock()
	var errs []error
	present := func(tok string) {
		lctx, cancel := context.WithTimeout(ctx, cloudSignOutTimeout)
		defer cancel()
		if err := cloudLogout(lctx, j.hc, baseURL, tok); err != nil && !cloudRefreshRefused(err) {
			errs = append(errs, err)
		}
	}
	if pending && refresh != "" {
		present(refresh)
	}
	for _, tok := range held {
		present(tok)
	}
	return errors.Join(errs...)
}

// refusePaneClose is the error a pane close answers with when closing would
// drop a sign-out the keychain does not carry: it names what the connection
// owes, why the close cannot proceed, and what the operator can do — keep
// the pane open, retry once the keychain answers, or remove the connection
// to present what it owes.
func (j *cloudTokenJar) refusePaneClose(pending bool, held []string, cause error, unconfirmed ...error) error {
	what := j.owesDescription(pending, len(held))
	if len(unconfirmed) > 0 {
		cause = fmt.Errorf("%v (presenting it now was answered: %v)", cause, errors.Join(unconfirmed...))
	}
	return fmt.Errorf("the pane stays open: closing it would drop %s, and %v — keep this pane open, retry once the keychain answers, or remove the connection to present what it owes", what, cause)
}

// owesDescription names what a jar still owes the cloud, for an error the
// operator reads.
func (j *cloudTokenJar) owesDescription(pending bool, held int) string {
	switch {
	case pending && held > 0:
		return fmt.Sprintf("a sign-out the cloud has not confirmed (this session's, and %d of ones this connection replaced)", held)
	case pending:
		return "this session's sign-out, which the cloud has not confirmed"
	default:
		return fmt.Sprintf("%d sign-out(s) of sessions this connection replaced, which the cloud has not confirmed", held)
	}
}

// errSignOutHeldInMemory is what marking a sign-out answers when the token it
// is for was never read: there is nothing to write the mark onto, so the hold
// lives in this process alone until a read answers (retrySignOut writes it
// then). Its own message is what the operator is told.
var errSignOutHeldInMemory = errors.New("the keychain could not be read, so the hold could not be written to it")

// markSignOutPending records that the cloud did not confirm this session's
// sign-out: the jar presents it no more (AccessToken, presentRefresh) and the
// background loop presents the sign-out again until the cloud answers 2xx. It
// bumps the epoch, so a refresh already in flight does not install the
// session's own successor over the hold (seedAt) — a refresh renews the very
// session the operator ended.
//
// The mark travels with the store entry, so a restart does not adopt the
// session either — unless the token was never read, which is the case of a
// keyring that could not be read: it then answers errSignOutHeldInMemory, and
// the first read that answers writes the mark (retrySignOut).
func (j *cloudTokenJar) markSignOutPending() error {
	j.mu.Lock()
	j.signOutPending = true
	j.epoch++
	if j.refresh == "" {
		read := j.read
		j.mu.Unlock()
		j.kickRefresh()
		if !read {
			return errSignOutHeldInMemory
		}
		// Nothing is stored: there is no session to end, and nothing to mark.
		return nil
	}
	err := j.writeStoredSessionLocked()
	j.mu.Unlock()
	j.kickRefresh()
	return err
}

// clearSignOutPending lifts a pending-sign-out mark whose answer said the
// named ask was not this jar's session (X-Iterion-Logout: other-session):
// the ask named a lineage the jar's token does not belong to, so the session
// it marks was never asked to end. Keeping the mark — in memory or on the
// entry — would end the KEPT session at the next presentation (the loop
// presents a mark's token, and a restart presents a marked entry before it
// adopts it). The entry is rewritten without the mark and the session stays
// exactly as it was. A rewrite the keychain refuses leaves the entry marked
// while the jar serves the session: the next renewal rewrites the entry
// without it, and the tunnel logs the window.
func (j *cloudTokenJar) clearSignOutPending() error {
	j.mu.Lock()
	j.signOutPending = false
	err := j.writeStoredSessionLocked()
	j.mu.Unlock()
	if err != nil {
		return fmt.Errorf("persist refresh token: %w", err)
	}
	return nil
}

// retrySignOut presents a pending sign-out again. A 2xx ends it — the jar and
// the store entry go with the session; anything else keeps the mark for the
// next presentation. A jar with nothing stored has nothing to revoke: the
// sign-out is over.
//
// The mark is written to the store BEFORE the presentation: a hold taken
// while the keychain could not be read lives in this process only, and this
// read — the first that answers, since the loop presents on its own back-off
// — is the first chance to make it survive a restart.
func (j *cloudTokenJar) retrySignOut(ctx context.Context) error {
	if err := j.ensureReadForSignOut(); err != nil {
		return err
	}
	j.mu.RLock()
	pending, refresh, baseURL := j.signOutPending, j.refresh, j.baseURL
	j.mu.RUnlock()
	if !pending {
		return nil
	}
	if refresh == "" {
		return j.clear()
	}
	holdErr := j.writeStoredSession()
	ctx, cancel := context.WithTimeout(ctx, cloudSignOutTimeout)
	defer cancel()
	if err := cloudLogout(ctx, j.hc, baseURL, refresh); err != nil {
		return fmt.Errorf("%w%s", err, signOutHoldSuffix(holdErr))
	}
	return j.clear()
}

// holdsSignOut reports whether this connection holds the sign-out of a
// session it replaced, waiting for the cloud to confirm it.
func (j *cloudTokenJar) holdsSignOut() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.heldSignOuts) > 0
}

// owesRenewalOrPresentation reports whether a renewal/presentation loop has
// something to do for this jar: a live session's refresh token to renew, a
// sign-out the cloud has not confirmed, or sign-outs held for sessions this
// connection replaced. A jar with none of these holds no loop: a renewal of
// an empty jar answers errNoCloudSession, which the loop would chase as a
// transient failure.
func (j *cloudTokenJar) owesRenewalOrPresentation() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.refresh != "" || j.signOutPending || len(j.heldSignOuts) > 0
}

// prepareForSignIn presents the sign-out this connection holds BEFORE a
// sign-in replaces its store entry: once the new session's token is written,
// nothing here can end the old one, and the cloud keeps it live for the whole
// refresh TTL. j is the jar the sign-in is about to seed; prev is the
// connection's live jar, which holds the mark and the token in memory —
// including a hold the keychain could not be written for — or nil (a restart,
// a connection no pane has open).
//
// A 2xx ends the old session and the sign-in proceeds clean. Anything else
// leaves its token HELD beside the new session (heldSignOuts), and the
// background loop presents it until the cloud confirms it: the operator's
// sign-out is not abandoned by signing in again. A store that cannot be read
// refuses the sign-in instead — the entry it would overwrite may be a held
// sign-out's only copy.
func (j *cloudTokenJar) prepareForSignIn(ctx context.Context, prev *cloudTokenJar) error {
	if prev != nil && prev != j && prev.connID == j.connID {
		j.adoptSignOutHold(prev)
	}
	if err := j.readIfUnread(); err != nil {
		return fmt.Errorf("sign in to %s: the session stored for this connection could not be read (%w) — it may be one whose sign-out this computer still has to present, and signing in would overwrite it: unlock your keychain and sign in again", j.baseURL, err)
	}
	j.mu.Lock()
	pending, refresh := j.signOutPending, j.refresh
	if pending && refresh == "" {
		// A mark with no token: nothing is left that could end that session.
		j.signOutPending = false
	}
	j.mu.Unlock()
	if !pending || refresh == "" {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, cloudSignOutTimeout)
	defer cancel()
	if err := cloudLogout(lctx, j.hc, j.baseURL, refresh); err == nil {
		j.mu.Lock()
		j.signOutPending = false
		if j.refresh == refresh {
			j.refresh, j.access, j.loggedIn = "", "", false
		}
		j.mu.Unlock()
		return nil
	}
	return j.holdSignOutForLater(refresh)
}

// adoptSignOutHold takes over what another jar of the SAME connection holds
// for the cloud: a pending sign-out — tokenless ones too (a hold is bound to
// the ENTRY: the read that follows names its token, F12) — and the sign-outs
// already held for sessions this connection replaced. A sign-in builds a new
// jar over the same store key, and a hold that lives only in the old jar's
// memory — the keychain could not be written — would be lost with it.
func (j *cloudTokenJar) adoptSignOutHold(prev *cloudTokenJar) {
	prev.mu.RLock()
	pending, refresh, held := prev.signOutPending, prev.refresh, slices.Clone(prev.heldSignOuts)
	prev.mu.RUnlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.heldSignOuts = mergeHeldSignOuts(j.heldSignOuts, held)
	if pending && !j.read {
		j.signOutPending = true
		if refresh != "" {
			j.refresh = refresh
		}
	}
}

// holdSignOutForLater keeps refresh for the presentations that follow a
// sign-in: the session it names is live on the cloud, the operator ended it,
// and this token is the only thing that can. It leaves the jar ready to be
// seeded — no mark, no token — and writes the entry at once, so a crash
// between here and the seed does not lose it.
func (j *cloudTokenJar) holdSignOutForLater(refresh string) error {
	j.mu.Lock()
	j.heldSignOuts = mergeHeldSignOuts(j.heldSignOuts, []string{refresh})
	j.signOutPending = false
	if j.refresh == refresh {
		j.refresh, j.access, j.loggedIn = "", "", false
	}
	j.mu.Unlock()
	if err := j.writeStoredSession(); err != nil {
		return fmt.Errorf("sign in to %s: the sign-out this computer still has to present could not be kept in the keychain (%w) — signing in would leave that session live with nothing able to end it", j.baseURL, err)
	}
	return nil
}

// presentHeldSignOuts presents the sign-out of every session this connection
// replaced and whose sign-out the cloud has not confirmed. Each 2xx drops its
// token from the jar and the store entry; the rest stay for the next
// presentation. Nothing of those sessions is used meanwhile — the jar holds
// another one — so the presentations only have to outlive an outage.
//
// The tokens come from memory, never from a fresh store read: a hydrate
// brought them in, and a locked keyring must not be charged an unlock prompt
// per presentation.
func (j *cloudTokenJar) presentHeldSignOuts(ctx context.Context) error {
	j.mu.RLock()
	held, baseURL := slices.Clone(j.heldSignOuts), j.baseURL
	j.mu.RUnlock()
	if len(held) == 0 {
		return nil
	}
	var errs []error
	confirmed := make([]string, 0, len(held))
	for _, tok := range held {
		lctx, cancel := context.WithTimeout(ctx, cloudSignOutTimeout)
		err := cloudLogout(lctx, j.hc, baseURL, tok)
		cancel()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		confirmed = append(confirmed, tok)
	}
	if len(confirmed) > 0 {
		j.mu.Lock()
		j.heldSignOuts = slices.DeleteFunc(j.heldSignOuts, func(tok string) bool {
			return slices.Contains(confirmed, tok)
		})
		j.mu.Unlock()
		if err := j.syncStoredSession(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// logout signs the jar's session out of the cloud and, once the cloud
// answered 2xx, clears the jar and the keychain entry. A logout the cloud did
// not confirm (cloudLogout), or a token it could not read, keeps both and
// returns the error: the session is still live server-side, and the
// keychain's copy is the only one that can still end it. What the desktop
// does NOT do is keep using it — the sign-out is marked pending, so nothing
// presents or renews the session and the background loop presents the
// sign-out again. Removing the connection deletes the local copy without the
// cloud.
func (j *cloudTokenJar) logout(ctx context.Context) error {
	if err := j.readIfUnread(); err != nil {
		mark := j.markSignOutPending()
		return fmt.Errorf("sign out of %s: %w — nothing was signed out; it is presented once the keychain answers%s", j.baseURL, err, signOutHoldSuffix(mark))
	}
	if refresh := j.storedRefresh(); refresh != "" {
		if err := cloudLogout(ctx, j.hc, j.baseURL, refresh); err != nil {
			mark := j.markSignOutPending()
			return fmt.Errorf("sign out of %s: the cloud did not confirm it (%w) — this computer uses the session no more and presents the sign-out again until the cloud confirms it%s", j.baseURL, err, signOutHoldSuffix(mark))
		}
	}
	return j.clear()
}

// signOutHoldSuffix names a pending sign-out the store does not carry yet:
// the mark lives in this process alone, so a restart before it is written
// reads an entry that says nothing of it. The jar writes it at the first
// presentation whose store read answers (retrySignOut), and from then on a
// restart keeps it; a pane close before that is refused instead of dropping
// the hold (closeForPane).
func signOutHoldSuffix(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf(" (the hold is in this app's memory only: %v — it is written to the keychain at the first presentation the keychain answers; until then a restart loses it, and closing the pane is refused so the hold is not dropped with it)", err)
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
		if j.signOutPending && j.refresh != "" && j.refresh != refreshCookie {
			// The pending sign-out names the token this rotation replaces: it
			// moves beside the entry's next one, named (a hold is never lost
			// with the live token it ends — I4).
			j.heldSignOuts = mergeHeldSignOuts(j.heldSignOuts, []string{j.refresh})
			j.signOutPending = false
		}
		j.refresh = refreshCookie
		j.read = true
	}
	write := refreshCookie != "" && j.refresh != ""
	err := j.writeStoredSessionLocked()
	j.mu.Unlock()
	if write {
		return err
	}
	return nil
}

// desktopCallbackHandler is the SSO loopback listener's handler, named so a
// test drives it directly. A cloud refusal arrives as ?sso_error=<code> (the
// callback's redirectCallbackError): it is answered to the refusal channel at
// once, mapped by desktopSSORefusal. An accepted sign-in arrives as the
// one-time ticket.
func desktopCallbackHandler(cloudURL string, ticketCh chan<- string, refusalCh chan<- error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/desktop-callback" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if code := r.URL.Query().Get("sso_error"); code != "" {
			msg := desktopSSORefusal(code, r.URL.Query().Get("sso_provider"))
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, ssoClosePage("Sign-in refused: "+msg+" You can close this window."))
			select {
			case refusalCh <- fmt.Errorf("sso sign-in refused by %s: %s", cloudURL, msg):
			default:
			}
			return
		}
		ticket := r.URL.Query().Get("ticket")
		if ticket == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, ssoClosePage("Sign-in failed — no ticket returned. You can close this window."))
		} else {
			_, _ = io.WriteString(w, ssoClosePage("Signed in. You can close this window and return to Iterion."))
		}
		select {
		case ticketCh <- ticket:
		default:
		}
	}
}

// ssoClosePage renders the minimal HTML the system browser shows after the SSO
// round-trip completes on the loopback listener.
//
// msg is ESCAPED here, at the one sink every caller goes through: a refusal's
// sentence carries the provider's label, which travels in the callback's
// ?sso_provider= and is an org admin's free-text display_name upstream
// (auth.OrgSSOProvider.DisplayName). Rendered raw it would run that admin's
// script on http://127.0.0.1:<port>, where a cookie is written for every
// port of the loopback host.
func ssoClosePage(msg string) string {
	return "<!doctype html><html><head><meta charset=\"utf-8\"><title>Iterion</title></head>" +
		"<body style=\"font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0\">" +
		"<p style=\"font-size:1rem;color:#333\">" + html.EscapeString(msg) + "</p></body></html>"
}

// desktopSSORefusal maps the sso_error code the cloud's OIDC callback
// redirects to the loopback listener (pkg/server redirectCallbackError) to
// the operator-friendly sentence the shell shows, mirroring the studio
// login page's own mapping (ssoErrorNotice).
func desktopSSORefusal(code, provider string) string {
	if provider == "" {
		provider = "the SSO provider"
	}
	switch code {
	case "link_required":
		return "an account already exists with this email: sign in with your password, then connect " + provider + " from Account settings"
	case "restricted":
		return provider + " sign-in is limited to allow-listed teams, and yours didn't match — ask your administrator for access"
	case "disabled":
		return "that SSO provider is currently turned off — contact your administrator or sign in another way"
	case "unknown_provider":
		return "that SSO provider is no longer available — try signing in with your email and password"
	case "provider_unavailable":
		return provider + " sign-in is temporarily unavailable — please try again in a moment"
	case "password_change_required":
		return "your administrator requires a password change before your next sign-in — set one with the emailed reset link, or ask your administrator"
	case "state_expired":
		return "the sign-in took too long or the link was reused — please start again"
	case "agent_binding":
		return "the sign-in could not be completed safely (it may have started in another browser) — please try again"
	default:
		return provider + " sign-in didn't complete — please try again, or use your email and password"
	}
}
