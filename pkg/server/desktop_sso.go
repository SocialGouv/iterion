package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/desktopsso"
)

// desktop_sso.go implements the DESKTOP SSO exchange: the server side of the
// "cloud→loopback single-use ticket" flow that lets the Wails desktop app
// complete an OIDC sign-in in the system browser without the IdP ever seeing
// a loopback redirect_uri and without a refresh token ever landing in a URL.
//
// Flow:
//  1. Desktop calls /api/auth/oidc/<p>/start?format=json&desktop=1&next=<loopback>
//     → PendingAuth.Desktop + DesktopRedirect are stored; the desktop opens the
//     returned authorize_url in the system browser.
//  2. The IdP redirects to the STABLE cloud callback (handleOIDCCallback). For a
//     desktop flow the callback DEFERS the login: it stores what the redemption
//     needs to mint the session — the account, the lineage it will be born
//     under, the landing team (auth.DesktopLogin) — mints a single-use ticket
//     for it and 302s to DesktopRedirect?ticket=… (a loopback URL). No session
//     row is written and no token exists yet (#2635), so the ticket store never
//     rests a credential and an unredeemed ticket leaves no live session.
//  3. The desktop's loopback listener captures the ticket and redeems it at
//     POST /api/auth/desktop/exchange over its native client: the account's
//     standing is re-asked against the lineage and generation the callback
//     named, the session is minted NOW, and the answer is a password login's
//     (access token in body, refresh in the Set-Cookie the native client
//     harvests).
//
// The ticket store (pkg/auth/desktopsso) is in-memory by default and
// Mongo-backed in the cloud control plane, so the mint (callback) and redeem
// (exchange) can land on different replicas.

// desktopTicketTTL bounds how long a minted exchange ticket is redeemable.
// The desktop redeems within a second of the browser redirect; a short TTL
// caps the window a leaked loopback URL could be replayed.
const desktopTicketTTL = 2 * time.Minute

// desktopExchangeReq is the body of POST /api/auth/desktop/exchange.
type desktopExchangeReq struct {
	Ticket string `json:"ticket"`
}

// handleDesktopExchange redeems a single-use SSO ticket for tokens. Public
// (no session yet) and used ONLY by the desktop native client. The response
// is a password login's (writeSession, an AuthSessionResponse), so the
// desktop reuses the same parsing: access token in the JSON body (non-browser
// client), refresh token in the Set-Cookie it harvests.
func (s *Server) handleDesktopExchange(w http.ResponseWriter, r *http.Request) {
	if s.authSvc == nil || s.desktopTickets == nil {
		httpError(w, http.StatusNotFound, "desktop exchange not available")
		return
	}
	var req desktopExchangeReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Ticket == "" {
		httpError(w, http.StatusBadRequest, "missing ticket")
		return
	}
	d, err := s.desktopTickets.Redeem(r.Context(), req.Ticket)
	if errors.Is(err, desktopsso.ErrTicketNotFound) {
		httpError(w, http.StatusUnauthorized, "invalid or expired ticket")
		return
	}
	if err != nil {
		// The ticket store failed: the ticket may be valid, and the desktop
		// reads a 401 as "redo the sign-in".
		s.identityFailure(w, r, fmt.Errorf("redeem the desktop ticket: %w", err))
		return
	}
	// The login the ticket defers was resolved at the IdP callback, up to
	// desktopTicketTTL ago: a sign-out, "sign out everywhere", a password
	// change or a disable landing since refuses it — the redemption asks the
	// account's standing against the lineage and generation the callback
	// named (auth.Service.RedeemDesktopLogin), as any presented credential
	// is asked (auth.Service.CheckStanding) — 401 when it no longer stands —
	// and only then mints the session: the refresh token exists from here
	// on, handed to the desktop, never stored.
	res, err := s.authSvc.RedeemDesktopLogin(r.Context(), d, r.UserAgent(), s.clientIP(r))
	if err != nil {
		if mapAuthErrorStatus(err) >= http.StatusInternalServerError {
			s.identityFailure(w, r, fmt.Errorf("mint the session a desktop ticket defers: %w", err))
			return
		}
		s.authError(w, r, err)
		return
	}
	s.writeSession(w, r, loginSession(res))
}

// safeDesktopRedirect validates a desktop loopback redirect target: it must be
// an absolute http URL on a loopback host (127.0.0.1 / [::1] / localhost) with
// an explicit port. This keeps the callback from being coerced into 302-ing a
// minted ticket to an arbitrary external origin. Returns "" when invalid.
func safeDesktopRedirect(v string) string {
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "http" || u.Port() == "" {
		return ""
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
	default:
		return ""
	}
	// Disallow embedded credentials / fragments that could confuse the desktop
	// listener; a plain scheme://host:port/path[?query] is all we emit.
	if u.User != nil || strings.Contains(v, "#") {
		return ""
	}
	return v
}
