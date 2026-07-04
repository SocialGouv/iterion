package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/wsticket"
)

// wsTicketTTL bounds how long a minted WS ticket is redeemable. A client mints
// one and dials the WS within a second; a short TTL caps replay of a leaked
// ?ticket= URL — and single-use invalidation kills replay outright.
const wsTicketTTL = time.Minute

// handleWSTicket mints a single-use ticket bound to the caller's identity so
// the client can authenticate a WebSocket upgrade with ?ticket=<t> instead of
// carrying a long-lived JWT in the URL. Authenticated (runs behind requireAuth
// via the non-public route), so the identity is already in context.
func (s *Server) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	if s.wsTickets == nil {
		httpError(w, http.StatusNotFound, "ws tickets not available")
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok || id.UserID == "" {
		httpError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ticket, err := s.wsTickets.Mint(r.Context(), id)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("ws ticket mint failed: %v", err)
		}
		httpError(w, http.StatusInternalServerError, "mint ws ticket")
		return
	}
	writeJSON(w, map[string]any{
		"ticket":     ticket,
		"expires_in": int(wsTicketTTL.Seconds()),
	})
}

// wsTicketIdentity redeems a ?ticket= on a WS path, returning the bound
// identity. It is consulted by requireAuth BEFORE the JWT/PAT path so a ticket
// resolves directly to an identity (there is no token to verify). Returns
// (_, false) when the request is not a ticket-authenticated WS upgrade; the
// caller then falls through to the normal bearer/cookie/?t= paths.
func (s *Server) wsTicketIdentity(r *http.Request) (auth.Identity, bool) {
	if s.wsTickets == nil {
		return auth.Identity{}, false
	}
	if r.URL.Path != "/api/ws" && !strings.HasPrefix(r.URL.Path, "/api/ws/") {
		return auth.Identity{}, false
	}
	ticket := r.URL.Query().Get("ticket")
	if ticket == "" {
		return auth.Identity{}, false
	}
	id, err := s.wsTickets.Redeem(r.Context(), ticket)
	if err != nil {
		// An invalid/expired/used ticket is not a fall-through to other auth:
		// the client explicitly presented a ticket. Signal "handled, rejected"
		// by returning ok=true with a zero identity so requireAuth 401s rather
		// than trying the JWT path with an empty token.
		if !errors.Is(err, wsticket.ErrTicketNotFound) && s.logger != nil {
			s.logger.Warn("ws ticket redeem error: %v", err)
		}
		return auth.Identity{}, true
	}
	return id, true
}
