package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/wsticket"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// wsTicketTTL bounds how long a minted WS ticket is redeemable. A client mints
// one and dials the WS within a second; a short TTL caps replay of a leaked
// ?ticket= URL — and single-use invalidation kills replay outright.
const wsTicketTTL = time.Minute

// handleWSTicket mints a single-use ticket bound to the caller's identity so
// the client can authenticate a WebSocket upgrade with ?ticket=<t> instead of
// carrying a long-lived JWT in the URL. Authenticated (runs behind requireAuth
// via the non-public route), so the identity is already in context.
//
// The ticket is minted only from a live credential (checkDerivation): never
// from another ticket (403), never from the access token of a revoked session
// or of a disabled or pending account (401). It carries the minting
// credential's expiry and session, and its redemption asks the same question
// again (wsTicketIdentity): a ticket never opens a WebSocket past the
// credential that minted it, and expires_in says how long it can.
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
	d, err := s.checkDerivation(r.Context(), id, auth.MintWSTicket)
	if err != nil {
		s.derivationError(w, r, err)
		return
	}
	expiresIn := wsTicketTTL
	if !d.NotAfter.IsZero() {
		if left := time.Until(d.NotAfter); left < expiresIn {
			expiresIn = left
		}
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
		"expires_in": int(expiresIn.Seconds()),
	})
}

// checkDerivation is auth.Service.CheckDerivation for this server: the
// dev-mode principal of a server without authentication derives without it.
func (s *Server) checkDerivation(ctx context.Context, id auth.Identity, mint auth.Mint) (auth.Derivation, error) {
	if s.authSvc == nil {
		if id.Credential == auth.CredentialDevMode {
			return auth.Derivation{}, nil
		}
		return auth.Derivation{}, errors.New("auth not configured")
	}
	return s.authSvc.CheckDerivation(ctx, id, mint)
}

// checkStanding is auth.Service.CheckStanding for this server, asked by an
// open WebSocket about the credential it was opened with
// (auth.StandingOfOpenSocket: its expiry left out): the dev-mode principal of
// a server without authentication stands without it. It answers the account
// as the store reads it NOW (the super-admin flag included), so a caller
// re-deriving a scope per envelope reads the flag from it, and both the
// refusal and a read that did not complete are returned;
// mapAuthErrorStatus tells them apart (a status of 500 or above is the
// server's own failure).
func (s *Server) checkStanding(ctx context.Context, id auth.Identity) (identity.User, error) {
	if s.authSvc == nil {
		if id.Credential == auth.CredentialDevMode {
			return identity.User{ID: id.UserID, Email: id.Email, IsSuperAdmin: id.IsSuperAdmin}, nil
		}
		return identity.User{}, errors.New("auth not configured")
	}
	return s.authSvc.CheckStanding(ctx, auth.StandingOfOpenSocket(id))
}

// isWSTicketPath reports whether a path is a WebSocket-upgrade endpoint that
// accepts single-use ?ticket= auth: the run-console / event WS under /api/ws,
// and the per-run Browser-Live CDP bridge. Ticket auth is safe to extend here
// (single-use, TTL-bounded, bound to the minter's identity) and is what lets a
// desktop workspace pane authenticate a cross-origin WS without a JWT in the URL.
func isWSTicketPath(path string) bool {
	if path == "/api/ws" || strings.HasPrefix(path, "/api/ws/") {
		return true
	}
	// /api/runs/{id}/browser/cdp — the CDP bridge dialed by the Browser-Live pane.
	return strings.HasPrefix(path, "/api/runs/") && strings.HasSuffix(path, "/browser/cdp")
}

// errWSTicketRefused answers a presented ticket that authenticates nobody:
// one the store refuses — unknown, expired or already redeemed — or one that
// names no user.
var errWSTicketRefused = errors.New("invalid or expired ws ticket")

// wsTicketIdentity redeems a ?ticket= on a WS path, returning the bound
// identity. It is consulted by requireAuth BEFORE the JWT/PAT path so a ticket
// resolves directly to an identity (there is no token to verify). presented
// is false when the request is not a ticket-authenticated WS upgrade; the
// caller then falls through to the normal bearer/cookie/?t= paths.
//
// A presented ticket is never a fall-through, and its redemption has two
// failing outcomes the caller keeps apart: errWSTicketRefused when the
// ticket authenticates nobody (wsticket.ErrTicketNotFound is the store's one
// refusal), and any other error when the redemption did not complete — the
// store's failure, not a verdict on a ticket that may still be live.
//
// A redeemed ticket is checked again as the credential that minted it
// (auth.Service.CheckDerivation): past that credential's expiry, or once its
// session is revoked or its account disabled or pending, it is refused
// (errWSTicketRefused, the refusal logged). The identity it answers names
// the ticket as its credential, so it mints nothing.
func (s *Server) wsTicketIdentity(r *http.Request) (id auth.Identity, presented bool, err error) {
	if s.wsTickets == nil {
		return auth.Identity{}, false, nil
	}
	if !isWSTicketPath(r.URL.Path) {
		return auth.Identity{}, false, nil
	}
	ticket := r.URL.Query().Get("ticket")
	if ticket == "" {
		return auth.Identity{}, false, nil
	}
	id, err = s.wsTickets.Redeem(r.Context(), ticket)
	switch {
	case errors.Is(err, wsticket.ErrTicketNotFound):
		return auth.Identity{}, true, errWSTicketRefused
	case err != nil:
		return auth.Identity{}, true, fmt.Errorf("redeem a ws ticket: %w", err)
	case id.UserID == "":
		return auth.Identity{}, true, errWSTicketRefused
	}
	if id.Credential == "" {
		// Minted by a server that did not record the minting credential:
		// nothing to check it against.
		return auth.Identity{}, true, errWSTicketRefused
	}
	d, derr := s.checkDerivation(r.Context(), id, auth.MintWSTicket)
	if derr != nil {
		if mapAuthErrorStatus(derr) >= http.StatusInternalServerError {
			return auth.Identity{}, true, fmt.Errorf("check a redeemed ws ticket against its minting credential: %w", derr)
		}
		if s.logger != nil {
			s.logger.Warn("ws ticket of user %s refused at its redemption: %v", id.UserID, derr)
		}
		return auth.Identity{}, true, errWSTicketRefused
	}
	// The ticket serializes the identity it was minted from, claim included:
	// its super-admin flag is re-read from the account at redemption, so a
	// demotion between the mint and the dial does not ride the ticket.
	id.IsSuperAdmin = d.User.IsSuperAdmin
	id.MintedBy = id.Credential
	id.Credential = auth.CredentialWSTicket
	return id, true, nil
}
