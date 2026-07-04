//go:build desktop

package main

import (
	"context"
	"fmt"
	"log"
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

// deactivateCloud tears down the active cloud session (stops the refresh
// loop, drops the jar). Called when switching to a local connection. Does
// NOT clear the persisted refresh token — switching away is not logout.
func (a *App) deactivateCloud() {
	a.mu.Lock()
	if a.cloudCancel != nil {
		a.cloudCancel()
		a.cloudCancel = nil
	}
	a.cloudJar = nil
	a.mu.Unlock()
}

// runCloudRefreshLoop refreshes the access token slightly before it expires.
// On a transient error it retries after a short floor; on an auth error
// (refresh revoked/expired) it emits cloud:auth-expired and stops — the SPA
// shell then prompts for re-login.
func (a *App) runCloudRefreshLoop(ctx context.Context, jar *cloudTokenJar) {
	const floor = 15 * time.Second
	for {
		wait := time.Until(jar.Expiry().Add(-cloudRefreshSkew))
		if wait < floor {
			wait = floor
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := jar.refreshNow(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			if ae := asCloudAuthError(err); ae != nil && (ae.Status == 401 || ae.Status == 403) {
				log.Printf("desktop: cloud refresh rejected for %s: %v", jar.connID, err)
				a.emitCloudAuthExpired(jar.connID)
				return
			}
			log.Printf("desktop: cloud refresh transient error for %s: %v (will retry)", jar.connID, err)
			// Expiry is unchanged (still past), so the next iteration floors
			// to `floor` and retries.
		}
	}
}

func (a *App) emitCloudAuthExpired(connID string) {
	if a.ctx == nil {
		return
	}
	wruntime.EventsEmit(a.ctx, eventCloudAuthExpired, connID)
}

// activateCloudConnection points the desktop at an existing cloud connection
// (used by the project switcher). It hydrates the stored refresh token and
// mints a fresh access token; if none is stored or the refresh fails, the
// connection is left unauthenticated (serverURL points at the cloud, the
// proxy has no Bearer, the cloud 401s, and the SPA shell shows login).
func (a *App) activateCloudConnection(ctx context.Context, p *Project) error {
	if p == nil || !p.IsCloud() {
		return fmt.Errorf("activateCloudConnection: not a cloud connection")
	}
	jar := newCloudTokenJar(p.ID, p.CloudURL, a.keychain)
	hydrated, err := jar.hydrate()
	if err != nil {
		log.Printf("desktop: cloud hydrate for %s failed: %v", p.ID, err)
	}
	if hydrated {
		if err := jar.refreshNow(ctx); err != nil {
			// Non-fatal: fall through unauthenticated so the shell can prompt.
			log.Printf("desktop: cloud rehydrate refresh for %s failed: %v", p.ID, err)
		}
	}
	a.mu.Lock()
	a.setActiveCloudLocked(jar, p.CloudURL)
	a.startCloudRefreshLoopLocked(jar)
	a.mu.Unlock()
	return nil
}

// ── Bindings (window.go.main.App.*) ──────────────────────────────────────

// ConnectCloud validates a remote URL, performs a native password login,
// registers (or refreshes) the cloud connection, makes it current, and
// reloads the SPA against it. This is the one-shot "add + login + switch"
// the shell's Connect-to-Cloud modal calls for password auth.
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

	res, err := cloudLogin(a.ctx, hc, cloudURL, email, password)
	if err != nil {
		return nil, mapCloudLoginError(err)
	}

	// Register/refresh the connection now that we have the user identity.
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
		return nil, saveErr
	}

	// Seed a jar keyed on the freshly-created connection id and make active.
	jar := newCloudTokenJar(p.ID, cloudURL, a.keychain)
	if err := jar.seed(res); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.setActiveCloudLocked(jar, cloudURL)
	a.startCloudRefreshLoopLocked(jar)
	a.mu.Unlock()

	wruntime.EventsEmit(a.ctx, eventProjectsChanged)
	a.reloadWindowApp(a.ctx)
	return &p, nil
}

// LoginCloud re-authenticates an EXISTING cloud connection (e.g. after
// cloud:auth-expired) with a password, re-seeding its jar. It does not
// change the current connection or reload — the shell decides when to.
func (a *App) LoginCloud(connID, email, password string) (*cloudUserSummary, error) {
	a.mu.RLock()
	p := a.config.ProjectByID(connID)
	a.mu.RUnlock()
	if p == nil || !p.IsCloud() {
		return nil, fmt.Errorf("cloud connection not found: %s", connID)
	}
	res, err := cloudLogin(a.ctx, newCloudHTTPClient(), p.CloudURL, email, password)
	if err != nil {
		return nil, mapCloudLoginError(err)
	}
	jar := newCloudTokenJar(connID, p.CloudURL, a.keychain)
	if err := jar.seed(res); err != nil {
		return nil, err
	}
	a.mu.Lock()
	// Only replace the active jar if this connection is the current one.
	if cur := a.config.CurrentProject(); cur != nil && cur.ID == connID {
		a.setActiveCloudLocked(jar, p.CloudURL)
		a.startCloudRefreshLoopLocked(jar)
	}
	a.mu.Unlock()
	summary := res.User
	return &summary, nil
}

// LogoutCloud revokes the connection's refresh token server-side and clears
// its local session (jar + keychain). If it was the current connection the
// SPA will start 401ing until the operator switches away or logs back in;
// the shell typically follows LogoutCloud with a switch to a local project.
func (a *App) LogoutCloud(connID string) error {
	a.mu.RLock()
	p := a.config.ProjectByID(connID)
	isCurrent := a.cloudJar != nil && a.cloudJar.connID == connID
	jar := a.cloudJar
	a.mu.RUnlock()
	if p == nil || !p.IsCloud() {
		return fmt.Errorf("cloud connection not found: %s", connID)
	}

	// Best-effort server-side revoke using whatever refresh token we hold.
	if jar == nil || jar.connID != connID {
		jar = newCloudTokenJar(connID, p.CloudURL, a.keychain)
		_, _ = jar.hydrate()
	}
	if refresh := jar.storedRefresh(); refresh != "" {
		if err := cloudLogout(a.ctx, newCloudHTTPClient(), p.CloudURL, refresh); err != nil {
			log.Printf("desktop: cloud logout revoke for %s failed (clearing locally anyway): %v", connID, err)
		}
	}
	if err := jar.clear(); err != nil {
		return err
	}
	if isCurrent {
		a.deactivateCloud()
	}
	return nil
}

// RemoveConnection removes a cloud connection: it clears the keychain refresh
// token and then delegates to RemoveProject for the config/MRU/daemon
// cascade. For a local project it is a straight alias of RemoveProject.
func (a *App) RemoveConnection(id string) error {
	a.mu.RLock()
	p := a.config.ProjectByID(id)
	a.mu.RUnlock()
	if p != nil && p.IsCloud() {
		jar := newCloudTokenJar(id, p.CloudURL, a.keychain)
		if err := jar.clear(); err != nil {
			log.Printf("desktop: clearing keychain for removed cloud connection %s failed: %v", id, err)
		}
		a.mu.RLock()
		isCurrent := a.cloudJar != nil && a.cloudJar.connID == id
		a.mu.RUnlock()
		if isCurrent {
			a.deactivateCloud()
		}
	}
	return a.RemoveProject(id)
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
