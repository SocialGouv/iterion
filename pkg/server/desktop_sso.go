package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
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

// desktopTicketTTL bounds how long a minted exchange ticket is redeemable.
// The desktop redeems within a second of the browser redirect; a short TTL
// caps the window a leaked loopback URL could be replayed.
const desktopTicketTTL = 2 * time.Minute

// desktopTicket is a minted, not-yet-redeemed SSO result.
type desktopTicket struct {
	result auth.LoginResult
	expiry time.Time
}

// desktopTicketStore is a single-use, TTL-bounded in-memory map from an
// opaque ticket to the LoginResult the OIDC callback produced.
//
// Caveat (cloud multi-replica): this is in-memory, so a mint on replica A and
// a redeem routed to replica B would miss. It is correct for the single-binary
// / single-replica case (desktop + one cloud instance). Sharing it across
// replicas — like the OIDC StateStore's Mongo backend — is the same follow-on
// as the WS single-use ticket (Phase 3).
type desktopTicketStore struct {
	mu  sync.Mutex
	m   map[string]desktopTicket
	ttl time.Duration
}

func newDesktopTicketStore(ttl time.Duration) *desktopTicketStore {
	return &desktopTicketStore{m: make(map[string]desktopTicket), ttl: ttl}
}

// mint stores res under a fresh random ticket and returns it. Expired entries
// are swept opportunistically.
func (s *desktopTicketStore) mint(res auth.LoginResult) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	ticket := base64.RawURLEncoding.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.m[ticket] = desktopTicket{result: res, expiry: time.Now().Add(s.ttl)}
	return ticket, nil
}

// redeem returns the LoginResult for ticket and deletes it (single-use).
// Returns false when the ticket is unknown or expired.
func (s *desktopTicketStore) redeem(ticket string) (auth.LoginResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[ticket]
	if !ok {
		return auth.LoginResult{}, false
	}
	delete(s.m, ticket)
	if time.Now().After(t.expiry) {
		return auth.LoginResult{}, false
	}
	return t.result, true
}

func (s *desktopTicketStore) sweepLocked() {
	now := time.Now()
	for k, v := range s.m {
		if now.After(v.expiry) {
			delete(s.m, k)
		}
	}
}

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
	res, ok := s.desktopTickets.redeem(req.Ticket)
	if !ok {
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
