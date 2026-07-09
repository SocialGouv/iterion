//go:build desktop

package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// httpBaseToWs converts an http(s):// base URL into its ws(s):// equivalent,
// stripped to scheme://host (no path, no trailing slash) — the base a pane's
// WS dialer appends /api/ws... to.
func httpBaseToWs(httpBase string) (string, error) {
	u, err := url.Parse(httpBase)
	if err != nil {
		return "", fmt.Errorf("invalid backend URL %q: %w", httpBase, err)
	}
	switch u.Scheme {
	case "http", "":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported backend scheme %q", u.Scheme)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host, "/"), nil
}

// connections.go implements the multi-backend connection REGISTRY that powers
// the desktop workspace (local + cloud panes shown simultaneously). Each open
// connection is a live backend the demux asset proxy routes a pane's
// /x/<connID>/* traffic to. This is the layer that lifts the historical
// single-active-connection constraint: opening a cloud connection no longer
// tears down the local one — both stay live, each in its own workspace pane.
//
// A pane is an iframe at /x/<connID>/ served the same studio SPA bundle,
// scoped by an injected window.__ITERION_SCOPE__. The iframe never touches the
// Wails IPC (window.go) — its result callbacks would be evaluated into the
// main frame, not the iframe — so everything a pane needs (API, WS base, WS
// ticket) is reached over HTTP through the demux proxy. The native bindings
// stay owned by the main frame (the workspace shell).

// activeConn is one open backend in the workspace registry. A local entry
// points at a per-project daemon/embedded server (jar == nil, DisableAuth);
// a cloud entry points at a remote instance and carries the token jar the
// demux proxy injects a Bearer from, plus the cancel for its own background
// refresh loop (each cloud connection refreshes independently).
type activeConn struct {
	id        string
	kind      string // "local" | "cloud"
	serverURL string // trailing-slash base: http://127.0.0.1:<port>/ or https://<cloud>/
	jar       *cloudTokenJar
	cancel    context.CancelFunc // cloud refresh loop; nil for local
}

// lookupConn returns the open connection with the given id, or nil.
func (a *App) lookupConn(id string) *activeConn {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.conns == nil {
		return nil
	}
	return a.conns[id]
}

// registerConnLocked inserts (replacing any prior entry, tearing down its
// refresh loop) a connection into the registry. Caller holds a.mu.
func (a *App) registerConnLocked(c *activeConn) {
	if a.conns == nil {
		a.conns = make(map[string]*activeConn)
	}
	if prev, ok := a.conns[c.id]; ok && prev.cancel != nil {
		prev.cancel()
	}
	a.conns[c.id] = c
}

// OpenConnection activates the connection with the given id (a local project
// or a cloud connection from the MRU) into the workspace registry and returns
// its Project. Idempotent: re-opening an already-open connection refreshes it.
// The workspace shell calls this before mounting the pane iframe at
// /x/<connID>/; the pane then reaches the backend over the demux proxy.
//
// Opening does NOT change the "current" connection or reload the window — the
// workspace holds several panes at once; there is no single active backend.
func (a *App) OpenConnection(id string) (*Project, error) {
	a.mu.RLock()
	p := a.config.ProjectByID(id)
	a.mu.RUnlock()
	if p == nil {
		return nil, fmt.Errorf("connection not found: %s", id)
	}
	if p.IsCloud() {
		if err := a.openCloudConn(a.ctx, p); err != nil {
			return nil, err
		}
	} else {
		if err := a.openLocalConn(p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// openLocalConn attaches (or spawns) the per-project daemon for a local
// project and registers it. Reuses the existing daemon lifecycle so a project
// already open in another pane shares the one daemon.
func (a *App) openLocalConn(p *Project) error {
	// Already open — reuse the live entry (e.g. the startup-registered current
	// connection, which may point at the in-process embedded server rather than
	// a daemon). Re-deriving here would spawn a redundant daemon and shadow it.
	if existing := a.lookupConn(p.ID); existing != nil && existing.serverURL != "" {
		return nil
	}
	dir := p.Dir
	if dir == "" {
		dir = defaultFallbackProjectDir()
	}
	url, ok := findDaemonForProject(dir)
	if !ok {
		spawned, err := spawnDaemonForProject(dir)
		if err != nil {
			return fmt.Errorf("spawn daemon for %s: %w", dir, err)
		}
		url = spawned
	}
	a.mu.Lock()
	a.registerConnLocked(&activeConn{id: p.ID, kind: ProjectKindLocal, serverURL: url})
	a.mu.Unlock()
	return nil
}

// openCloudConn hydrates the cloud connection's token jar, mints a fresh
// access token, starts an independent background refresh loop, and registers
// it. A failed rehydrate leaves the entry unauthenticated: the demux proxy has
// no Bearer, the cloud 401s, and the pane's SPA shows its login view.
func (a *App) openCloudConn(ctx context.Context, p *Project) error {
	// Reuse an already-open jar so re-opening doesn't spawn a second refresh
	// loop or drop a live token.
	if existing := a.lookupConn(p.ID); existing != nil && existing.jar != nil {
		return nil
	}
	jar := newCloudTokenJar(p.ID, p.CloudURL, a.keychain)
	hydrated, err := jar.hydrate()
	if err != nil {
		log.Printf("desktop: cloud hydrate for %s failed: %v", p.ID, err)
	}
	if hydrated {
		if err := jar.refreshNow(ctx); err != nil {
			log.Printf("desktop: cloud rehydrate refresh for %s failed: %v", p.ID, err)
		}
	}
	a.mu.Lock()
	c := &activeConn{id: p.ID, kind: ProjectKindCloud, serverURL: cloudServerURL(p.CloudURL), jar: jar}
	if a.ctx != nil {
		rctx, cancel := context.WithCancel(a.ctx)
		c.cancel = cancel
		go a.runCloudRefreshLoop(rctx, jar)
	}
	a.registerConnLocked(c)
	a.mu.Unlock()
	return nil
}

// CloseConnection removes a connection from the workspace registry (the pane
// is closing). Local daemons are LEFT RUNNING by design so their in-flight
// runs survive — other panes or a later re-open re-attach to them (matching
// the pre-workspace SwitchProject policy). A cloud connection's background
// refresh loop is stopped, but the persisted refresh token is kept (closing a
// pane is not a logout).
func (a *App) CloseConnection(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conns == nil {
		return nil
	}
	if c, ok := a.conns[id]; ok {
		if c.cancel != nil {
			c.cancel()
		}
		delete(a.conns, id)
	}
	return nil
}

// GetOpenConnections returns the ids of the connections currently open in the
// workspace, so the shell can restore its panes after a reload.
func (a *App) GetOpenConnections() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.conns))
	for id := range a.conns {
		out = append(out, id)
	}
	return out
}

// connWsInfo is the JSON a pane fetches from /x/<connID>/_ws/info to learn how
// to open a WebSocket to its backend directly (WS can't traverse the Wails
// asset origin, which 501s upgrades). ws_base is the absolute ws://|wss:// base
// (no trailing slash); needs_ticket is true for cloud (the pane then POSTs
// /x/<connID>/_ws/ticket per dial).
type connWsInfo struct {
	WsBase      string `json:"ws_base"`
	NeedsTicket bool   `json:"needs_ticket"`
}

// wsInfoForConn derives the WS dial info for an open connection.
func wsInfoForConn(c *activeConn) (connWsInfo, error) {
	base, err := httpBaseToWs(c.serverURL)
	if err != nil {
		return connWsInfo{}, err
	}
	return connWsInfo{WsBase: base, NeedsTicket: c.kind == ProjectKindCloud}, nil
}

// mintWsTicketForConn mints a single-use WS ticket for a cloud connection so
// the pane can authenticate wss://<cloud>/api/ws?ticket=. Returns "" for a
// local connection (its daemon runs DisableAuth) or when no live token exists.
func (a *App) mintWsTicketForConn(ctx context.Context, c *activeConn) (string, error) {
	if c.jar == nil {
		return "", nil
	}
	tok := c.jar.AccessToken()
	if tok == "" {
		return "", nil
	}
	return cloudMintWSTicket(ctx, newCloudHTTPClient(), c.jar.baseURL, tok)
}

// emitProjectsChanged notifies the workspace shell that the connection list or
// registry changed so it can re-read ListConnections / GetOpenConnections.
func (a *App) emitProjectsChanged() {
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, eventProjectsChanged)
	}
}
