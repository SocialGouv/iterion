//go:build desktop

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// cloud.go wires the DESKTOP-independent cloud core (cloud_client.go) to the
// Wails *App: connection bindings the SPA shell calls, the token jar's
// lifecycle (seed/hydrate/refresh/clear), the background refresh loop, and
// the startup rehydration. All secret persistence goes through a.keychain
// (the OS keychain); the refresh token never touches config.json.

// cloudServerURL normalizes a cloud base URL into the trailing-slash form
// a.serverURL uses (so the WS dialer's URL parsing and the proxy target
// stay consistent with the local loopback convention).
func cloudServerURL(cloudURL string) string {
	return strings.TrimRight(cloudURL, "/") + "/"
}

// activeCloudJar returns the live cloud jar, or nil when the current
// connection is local. Read by the asset proxy and GetSessionToken.
func (a *App) activeCloudJar() *cloudTokenJar {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cloudJar
}

// cloudJarFor returns the live jar of one cloud connection — the current
// connection's, or an open pane's — or nil when no jar of it is loaded. It is
// the one resolver: a sign-out and a sign-in must both find the jar that
// holds what this connection owes the cloud (a pending sign-out and its
// token), whichever way the connection is open.
func (a *App) cloudJarFor(connID string) *cloudTokenJar {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.cloudJar != nil && a.cloudJar.connID == connID {
		return a.cloudJar
	}
	if c := a.conns[connID]; c != nil && c.jar != nil {
		return c.jar
	}
	return nil
}

// setActiveCloudLocked installs a cloud jar as the current connection and
// points the proxy/WS target at the remote. Caller holds a.mu.
func (a *App) setActiveCloudLocked(jar *cloudTokenJar, cloudURL string) {
	a.cloudJar = jar
	a.serverURL = cloudServerURL(cloudURL)
	// A cloud connection is neither an embedded server nor a per-project
	// daemon — clear the daemon flag so onShutdown doesn't treat the remote
	// as an owned local server.
	a.usingDaemon = false
}

// startCloudRefreshLoopLocked cancels any running refresh loop and starts a
// fresh one bound to the given jar. Caller holds a.mu.
func (a *App) startCloudRefreshLoopLocked(jar *cloudTokenJar) {
	if a.cloudCancel != nil {
		a.cloudCancel()
		a.cloudCancel = nil
	}
	if a.ctx == nil {
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.cloudCancel = cancel
	go a.runCloudRefreshLoop(ctx, jar)
}

// cloudConnIDForURL returns the id of the cloud connection this app holds at
// cloudURL — its MRU one when several accounts' connections share the URL —
// or "" when none is registered yet. The add paths resolve their sign-in's
// keychain entry with it: AddCloudConnection keys a connection on
// (URL, account), and the account is only known after the mint.
func (a *App) cloudConnIDForURL(cloudURL string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for i := range a.config.RecentProjects {
		p := &a.config.RecentProjects[i]
		if p.IsCloud() && p.CloudURL == cloudURL {
			// RecentProjects is MRU-ordered (sortByMRU): the first hit is the
			// connection the shell is most likely signing back into.
			return p.ID
		}
	}
	return ""
}

// prepareSignInJar is the machine's pre-mint half of SIGN-IN: it resolves the
// connection's ONE jar — the live jar the registry holds (the pane tunnel's),
// or a fresh one over the same keychain entry — and runs prepareForSignIn:
// the entry is read BEFORE the cloud mints the new session (a keychain that
// cannot be read refuses here, and a refused sign-in leaves no minted
// session — F7), and the sign-out the entry owes is presented, or carried
// beside the new session when the cloud does not confirm it. connID is ""
// for a connection the sign-in cannot name yet (an add path): the MRU cloud
// connection at cloudURL stands in for it, and an empty one reads a key that
// does not exist — which still refuses a locked keychain before the mint.
func (a *App) prepareSignInJar(connID, cloudURL string) (*cloudTokenJar, error) {
	if connID == "" {
		connID = a.cloudConnIDForURL(cloudURL)
	}
	prev := a.cloudJarFor(connID)
	jar := resolveConnectionJar(prev, connID, cloudURL, a.keychain)
	if err := jar.prepareForSignIn(a.ctx, prev); err != nil {
		return nil, err
	}
	return jar, nil
}

// installSignInJarLocked makes jar the connection's ONE jar: the active jar
// when the connection is current, and the registry entry's — the pane
// tunnel's — either way. The jar it replaces in the registry is retired (its
// writes stand down; an answer of its own still in flight cannot land on the
// entry the sign-in wrote) and its loop stops, so no second jar of this
// connection writes one keychain key or presents from a session the sign-in
// replaced (F4). Caller holds a.mu.
func (a *App) installSignInJarLocked(jar *cloudTokenJar, cloudURL string) {
	c := a.conns[jar.connID]
	isCurrent := false
	if cur := a.config.CurrentProject(); cur != nil && cur.ID == jar.connID {
		isCurrent = true
	}
	if c != nil && c.jar != nil && c.jar != jar {
		c.jar.retire()
		if c.cancel != nil {
			c.cancel()
			c.cancel = nil
		}
	}
	if isCurrent {
		a.setActiveCloudLocked(jar, cloudURL)
		a.startCloudRefreshLoopLocked(jar)
		if c != nil {
			// The active loop is this connection's one renewal loop: the
			// entry borrows it (cancel nil, as the startup registration
			// does), so its own loop — a pane's — stops with it.
			if c.cancel != nil {
				c.cancel()
				c.cancel = nil
			}
			c.jar = jar
			c.serverURL = cloudServerURL(cloudURL)
		}
		return
	}
	if c != nil {
		c.jar = jar
		c.serverURL = cloudServerURL(cloudURL)
		// One renewal loop per connection: the entry's own loop — a pane's —
		// stops before the fresh one starts, or the loop it replaced runs
		// untracked until app exit.
		if c.cancel != nil {
			c.cancel()
			c.cancel = nil
		}
		if a.ctx != nil {
			rctx, cancel := context.WithCancel(a.ctx)
			c.cancel = cancel
			go a.runCloudRefreshLoop(rctx, jar)
		}
	}
}

// deactivateCloud tears down the active cloud session (stops the refresh
// loop, drops the jar). Called when switching to a local connection. Does NOT
// clear the persisted refresh token — switching away is not logout.
//
// The current connection's registry entry borrows the active loop (its own
// cancel is nil), so the teardown leaves its open pane without renewal and
// without the presenter its sign-out presentation rides on. Every registered
// cloud connection whose entry holds a jar of its own, no loop of its own,
// and something to renew or present gets its own loop back here: one renewal
// loop per open pane. A jar with nothing to do holds no loop — a renewal of
// an empty jar would answer an expiry the shell would prompt on.
func (a *App) deactivateCloud() {
	a.mu.Lock()
	if a.cloudCancel != nil {
		a.cloudCancel()
		a.cloudCancel = nil
	}
	a.cloudJar = nil
	if a.ctx != nil {
		for _, c := range a.conns {
			if c == nil || c.kind != ProjectKindCloud || c.jar == nil || c.cancel != nil {
				continue
			}
			jar := c.jar
			if !jar.owesRenewalOrPresentation() {
				continue
			}
			rctx, cancel := context.WithCancel(a.ctx)
			c.cancel = cancel
			go a.runCloudRefreshLoop(rctx, jar)
		}
	}
	a.mu.Unlock()
}

// runCloudRefreshLoop runs cloudRefreshLoop (cloud_refresh_loop.go, untagged
// so its cadence is unit-testable) against this App's event emitter.
func (a *App) runCloudRefreshLoop(ctx context.Context, jar *cloudTokenJar) {
	cloudRefreshLoop(ctx, jar, a.emitCloudAuthExpired)
}

func (a *App) emitCloudAuthExpired(connID string) {
	if a.ctx == nil {
		return
	}
	wruntime.EventsEmit(a.ctx, eventCloudAuthExpired, connID)
}

// activateCloudConnection points the desktop at an existing cloud connection
// (used by the project switcher, and at startup). The machine's ONE JAR PER
// CONNECTION runs first: activateConnectionJar resolves the connection's
// ONE jar — the live jar the registry holds (the pane tunnel's) when the
// connection is open, else a fresh one over the same keychain entry — and
// renews the session through IT; a switch that built its own jar would let
// the registry jar's next presentation land outside the rotation's reuse
// grace, which revokes the switched-to lineage and deletes the entry. The
// jar is then installed everywhere the connection lives
// (installSignInJarLocked): the active jar, the registry entry's, its one
// loop restarted. If nothing is stored or the refresh fails, the connection
// is left unauthenticated (serverURL points at the cloud, the proxy has no
// Bearer, the cloud 401s, and the SPA shell shows login).
func (a *App) activateCloudConnection(ctx context.Context, p *Project) error {
	if p == nil || !p.IsCloud() {
		return fmt.Errorf("activateCloudConnection: not a cloud connection")
	}
	jar := activateConnectionJar(ctx, a.cloudJarFor(p.ID), p.ID, p.CloudURL, a.keychain)
	a.mu.Lock()
	a.installSignInJarLocked(jar, p.CloudURL)
	a.mu.Unlock()
	return nil
}

// ── Bindings (window.go.main.App.*) ──────────────────────────────────────

// ConnectCloud validates a remote URL, performs a native password login,
// registers (or refreshes) the cloud connection, makes it current, and
// reloads the SPA against it. This is the one-shot "add + login + switch"
// the shell's Connect-to-Cloud modal calls for password auth.
//
// The machine's SIGN-IN runs its pre-mint half first (prepareSignInJar): the
// keychain entry is read before the cloud mints (F7).
func (a *App) ConnectCloud(cloudURL, email, password string) (*Project, error) {
	cloudURL = strings.TrimSpace(cloudURL)
	if cloudURL == "" {
		return nil, fmt.Errorf("cloud URL is required")
	}
	hc := newCloudHTTPClient()

	// Confirm the URL is a real, auth-enabled iterion cloud before logging in.
	info, err := cloudFetchServerInfo(a.ctx, hc, cloudURL)
	if err != nil {
		return nil, err
	}
	if !info.AuthRequired {
		return nil, fmt.Errorf("%s is not an authenticated cloud instance (auth_required=false)", cloudURL)
	}

	prepared, err := a.prepareSignInJar("", cloudURL)
	if err != nil {
		return nil, err
	}
	res, err := cloudLogin(a.ctx, hc, cloudURL, email, password)
	if err != nil {
		return nil, mapCloudLoginError(err)
	}
	return a.finishCloudConnect(cloudURL, res, prepared)
}

// finishCloudConnect registers/refreshes a cloud connection from an auth
// result, persists it, seeds + activates its token jar, and reloads the SPA
// against it. Shared by the password (ConnectCloud) and SSO (ConnectCloudSSO)
// entry points, which ran the machine's pre-mint half first (prepareSignInJar)
// and hand the jar it prepared in: when the mint landed on the connection
// that jar was resolved for, it IS the jar the sign-in seeds — one jar per
// connection (F4); otherwise the mint opened a connection that had none.
//
// Like LoginCloud, it presents the sign-out this connection holds before the
// new session replaces its keychain entry (prepareForSignIn): the entry is
// the only copy of the token that can end the session the operator signed out
// of. A post-mint refusal (the keychain degraded between the two moments)
// revokes the minted session: a sign-in that cannot land leaves no session
// live behind it (F7).
func (a *App) finishCloudConnect(cloudURL string, res cloudAuthResult, prepared *cloudTokenJar) (*Project, error) {
	a.mu.Lock()
	p := a.config.AddCloudConnection(cloudURL, res.User.ID, res.User.Email, "")
	if res.User.ActiveOrgID != "" || res.User.ActiveTeamID != "" {
		if cur := a.config.CurrentProject(); cur != nil && cur.ID == p.ID {
			cur.ActiveOrgID = res.User.ActiveOrgID
			cur.ActiveTeamID = res.User.ActiveTeamID
			p = *cur
		}
	}
	saveErr := a.config.Save()
	a.mu.Unlock()
	if saveErr != nil {
		a.abandonMintedSession(cloudURL, res, prepared)
		return nil, saveErr
	}

	jar := prepared
	if jar == nil || jar.connID != p.ID {
		// The mint landed on a connection the pre-mint half could not name
		// (its MRU stand-in resolved another connection at the URL): resolve
		// THIS connection's ONE jar — the live one the registry holds, else a
		// fresh one over its entry — and seed that.
		jar = resolveConnectionJar(a.cloudJarFor(p.ID), p.ID, cloudURL, a.keychain)
	}
	if err := jar.prepareForSignIn(a.ctx, a.cloudJarFor(p.ID)); err != nil {
		a.abandonMintedSession(cloudURL, res, jar)
		return nil, err
	}
	if err := jar.seed(res); err != nil {
		a.abandonMintedSession(cloudURL, res, jar)
		return nil, err
	}
	a.mu.Lock()
	a.installSignInJarLocked(jar, cloudURL)
	a.mu.Unlock()

	wruntime.EventsEmit(a.ctx, eventProjectsChanged)
	a.reloadWindowApp(a.ctx)
	return &p, nil
}

// abandonMintedSession ends the session a sign-in minted when the sign-in
// cannot land on the connection: its refresh token is presented to the
// cloud's logout (nothing here holds it any more, and a session nobody
// presents stays live until the refresh TTL — F7's leftover), and the jar
// that may have taken it drops it.
func (a *App) abandonMintedSession(cloudURL string, res cloudAuthResult, jar *cloudTokenJar) {
	if res.RefreshToken == "" {
		return
	}
	lctx, cancel := context.WithTimeout(a.ctx, cloudSignOutTimeout)
	defer cancel()
	if err := cloudLogout(lctx, newCloudHTTPClient(), cloudURL, res.RefreshToken); err != nil {
		log.Printf("desktop: a refused sign-in's minted session could not be revoked: %v (it stays live until the refresh TTL)", err)
	}
	if jar != nil {
		jar.clearIfStillHolds(res.RefreshToken)
	}
}

// ConnectCloudSSO connects to a remote cloud via SSO: it starts a transient
// loopback listener, asks the cloud for the IdP authorize URL bound to a
// desktop flow, opens it in the SYSTEM browser, waits for the cloud to 302 a
// single-use ticket back to the loopback, redeems it for tokens, and finishes
// the connection exactly like the password path. No refresh token ever passes
// through a URL — the loopback only carries the one-time ticket.
func (a *App) ConnectCloudSSO(cloudURL, provider string) (*Project, error) {
	cloudURL = strings.TrimSpace(cloudURL)
	if cloudURL == "" || provider == "" {
		return nil, fmt.Errorf("cloud URL and provider are required")
	}
	hc := newCloudHTTPClient()

	info, err := cloudFetchServerInfo(a.ctx, hc, cloudURL)
	if err != nil {
		return nil, err
	}
	if !info.AuthRequired {
		return nil, fmt.Errorf("%s is not an authenticated cloud instance", cloudURL)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start loopback listener: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://127.0.0.1:%d/desktop-callback", port)

	ticketCh := make(chan string, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/desktop-callback" {
				http.NotFound(w, r)
				return
			}
			ticket := r.URL.Query().Get("ticket")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	authURL, err := cloudOIDCAuthorizeURL(a.ctx, hc, cloudURL, provider, redirect)
	if err != nil {
		return nil, err
	}
	if err := a.OpenExternal(authURL); err != nil {
		return nil, fmt.Errorf("open system browser: %w", err)
	}

	var ticket string
	select {
	case ticket = <-ticketCh:
	case <-time.After(5 * time.Minute):
		// The cloud hands this listener a ticket only for a sign-in it
		// ACCEPTED: a refusal (an account whose administrator requires a
		// password change, an identity linked to no account, a cancelled
		// consent) redirects the browser to the instance's own sign-in page
		// with the reason, and nothing reaches the desktop. So the only true
		// thing to say is that no sign-in came back, and where the reason is.
		return nil, fmt.Errorf("no SSO sign-in came back from the browser within 5 minutes — the browser tab says whether %s refused it (an account that must change its password, an identity to link first) or the sign-in was left unfinished", cloudURL)
	case <-a.ctx.Done():
		return nil, a.ctx.Err()
	}
	if ticket == "" {
		return nil, fmt.Errorf("SSO sign-in did not complete")
	}

	// The machine's pre-mint half, before the ticket is exchanged for tokens:
	// the keychain entry this sign-in will land on is read first (F7).
	prepared, err := a.prepareSignInJar("", cloudURL)
	if err != nil {
		return nil, err
	}
	res, err := cloudDesktopExchange(a.ctx, hc, cloudURL, ticket)
	if err != nil {
		return nil, mapCloudLoginError(err)
	}
	return a.finishCloudConnect(cloudURL, res, prepared)
}

// ssoClosePage renders the minimal HTML the system browser shows after the SSO
// round-trip completes on the loopback listener.
func ssoClosePage(msg string) string {
	return "<!doctype html><html><head><meta charset=\"utf-8\"><title>Iterion</title></head>" +
		"<body style=\"font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0\">" +
		"<p style=\"font-size:1rem;color:#333\">" + msg + "</p></body></html>"
}

// LoginCloud re-authenticates an EXISTING cloud connection (e.g. after
// cloud:auth-expired) with a password, re-seeding its jar. It does not
// change the current connection or reload — the shell decides when to.
//
// The machine's SIGN-IN: the connection's ONE jar — the live one the registry
// holds (the pane tunnel's) — is resolved first and the machine's pre-mint
// half runs on it (prepareSignInJar): the keychain entry is read before the
// cloud mints (F7), and the sign-out it owes is presented, or carried beside
// the new session when the cloud does not confirm it (I4). The minted
// session is then seeded into that SAME jar, which installSignInJarLocked
// makes the one jar everywhere the connection lives (F4).
func (a *App) LoginCloud(connID, email, password string) (*cloudUserSummary, error) {
	a.mu.RLock()
	p := a.config.ProjectByID(connID)
	a.mu.RUnlock()
	if p == nil || !p.IsCloud() {
		return nil, fmt.Errorf("cloud connection not found: %s", connID)
	}
	jar, err := a.prepareSignInJar(connID, p.CloudURL)
	if err != nil {
		return nil, err
	}
	res, err := cloudLogin(a.ctx, newCloudHTTPClient(), p.CloudURL, email, password)
	if err != nil {
		return nil, mapCloudLoginError(err)
	}
	if err := jar.seed(res); err != nil {
		a.abandonMintedSession(p.CloudURL, res, jar)
		return nil, err
	}
	a.mu.Lock()
	isCurrent := false
	if cur := a.config.CurrentProject(); cur != nil && cur.ID == connID {
		isCurrent = true
	}
	a.installSignInJarLocked(jar, p.CloudURL)
	a.mu.Unlock()
	// Re-authenticating the CURRENT connection (the cloud:auth-expired recovery
	// path) reloads the SPA so any views stuck on 401s during the lapse
	// re-bootstrap cleanly against the freshly-armed proxy. A JS
	// window.location.reload is a no-op in this Wails setup (the page origin
	// doesn't change), so the reload must be driven from here.
	if isCurrent {
		a.reloadWindowApp(a.ctx)
	}
	summary := res.User
	return &summary, nil
}

// LogoutCloud revokes the connection's refresh token server-side and clears
// its local session (jar + keychain). If it was the current connection the
// SPA will start 401ing until the operator switches away or logs back in;
// the shell typically follows LogoutCloud with a switch to a local project.
// It fails, and keeps the session, when the token could not be read or the
// cloud did not confirm the sign-out (cloudTokenJar.logout).
func (a *App) LogoutCloud(connID string) error {
	a.mu.RLock()
	p := a.config.ProjectByID(connID)
	isCurrent := a.cloudJar != nil && a.cloudJar.connID == connID
	a.mu.RUnlock()
	jar := a.cloudJarFor(connID)
	if p == nil || !p.IsCloud() {
		return fmt.Errorf("cloud connection not found: %s", connID)
	}
	jar = resolveConnectionJar(jar, connID, p.CloudURL, a.keychain)
	if err := jar.logout(a.ctx); err != nil {
		return err
	}
	if isCurrent {
		a.deactivateCloud()
	}
	return nil
}

// RemoveConnection removes a cloud connection: the machine's CONNECTION
// REMOVED transition runs first — the connection's jar reads its entry and
// presents what it owes the cloud (its pending sign-out, and the sign-outs
// held for sessions it replaced; a presentation that cannot be made refuses
// the removal with the reason) — then the keychain entry is deleted and the
// config/MRU/daemon cascade of RemoveProject follows. For a local project it
// is a straight alias of RemoveProject.
func (a *App) RemoveConnection(id string) error {
	a.mu.RLock()
	p := a.config.ProjectByID(id)
	a.mu.RUnlock()
	if p == nil || !p.IsCloud() {
		return a.RemoveProject(id)
	}
	jar := a.cloudJarFor(id)
	jar = resolveConnectionJar(jar, id, p.CloudURL, a.keychain)
	if err := jar.removeConnection(a.ctx); err != nil {
		return err
	}
	a.mu.RLock()
	isCurrent := a.cloudJar != nil && a.cloudJar.connID == id
	a.mu.RUnlock()
	if isCurrent {
		a.deactivateCloud()
	}
	return a.RemoveProject(id)
}

// GetWsTicket mints a single-use WS ticket for the CURRENT cloud connection so
// the SPA can authenticate a cloud WebSocket with ?ticket= instead of the
// access JWT in the URL. Returns "" for a local connection (the embedded server
// runs DisableAuth) or when there is no live token — the SPA then falls back to
// the ?t=<token> path. Called per WS dial (tickets are single-use).
func (a *App) GetWsTicket() (string, error) {
	jar := a.activeCloudJar()
	if jar == nil {
		return "", nil
	}
	tok := jar.AccessToken()
	if tok == "" {
		return "", nil
	}
	return cloudMintWSTicket(a.ctx, newCloudHTTPClient(), jar.baseURL, tok)
}

// ListCloudProviders returns the SSO providers a cloud instance offers, for
// the login modal's SSO buttons (Phase 2 activates LoginSSO). The email is
// optional and enables per-org (tenant Keycloak) discovery.
func (a *App) ListCloudProviders(cloudURL, email string) (*cloudProviders, error) {
	out, err := cloudListProviders(a.ctx, newCloudHTTPClient(), cloudURL, email)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// mapCloudLoginError converts a *cloudAuthError into an operator-friendly
// message the shell can show directly, mirroring the SPA's own mapping.
func mapCloudLoginError(err error) error {
	ae := asCloudAuthError(err)
	if ae == nil {
		return err
	}
	switch ae.Status {
	case 401:
		return fmt.Errorf("invalid email or password")
	case 403:
		if ae.Message != "" {
			return fmt.Errorf("sign-in not allowed: %s", ae.Message)
		}
		return fmt.Errorf("sign-in not allowed (password change required or SSO-only account)")
	case 409:
		return fmt.Errorf("this account must be linked before password sign-in: %s", ae.Message)
	default:
		return err
	}
}
