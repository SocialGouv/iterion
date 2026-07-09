package server

import (
	"encoding/json"
	"errors"
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
//     desktop flow the callback mints a single-use ticket holding the
//     LoginResult and 302s to DesktopRedirect?ticket=… (a loopback URL).
//  3. The desktop's loopback listener captures the ticket and redeems it at
//     POST /api/auth/desktop/exchange over its native client, receiving the
//     same response shape as a password login (access token in body, refresh
//     in the Set-Cookie the native client harvests).
//
// The ticket store (pkg/auth/desktopsso) is in-memory by default and
// Mongo-backed in the cloud control plane, so the mint (callback) and redeem
// (exchange) can land on different replicas.

// desktopTicketTTL bounds how long a minted exchange ticket is redeemable.
// The desktop redeems within a second of the browser redirect; a short TTL
// caps the window a leaked loopback URL could be replayed.
const desktopTicketTTL = 2 * time.Minute

// handleDesktopExchange redeems a single-use SSO ticket for tokens. Public
// (no session yet) and used ONLY by the desktop native client. The response
// shape is identical to a password login (renderAuthResponse), so the desktop
// reuses the same parsing: access token in the JSON body (non-browser client),
// refresh token in the Set-Cookie it harvests.
func (s *Server) handleDesktopExchange(w http.ResponseWriter, r *http.Request) {
	if s.authSvc == nil || s.desktopTickets == nil {
		httpError(w, http.StatusNotFound, "desktop exchange not available")
		return
	}
	var req struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Ticket == "" {
		httpError(w, http.StatusBadRequest, "missing ticket")
		return
	}
	res, err := s.desktopTickets.Redeem(r.Context(), req.Ticket)
	if err != nil {
		if !errors.Is(err, desktopsso.ErrTicketNotFound) && s.logger != nil {
			s.logger.Warn("desktop exchange redeem error: %v", err)
		}
		httpError(w, http.StatusUnauthorized, "invalid or expired ticket")
		return
	}
	s.renderAuthResponse(w, r, res)
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
