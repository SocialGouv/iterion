package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/pat"
)

// registerPATRoutes wires the personal-access-token CRUD. URL space is
// /api/me/tokens — deliberately distinct from /api/me/api-keys (BYOK
// LLM provider keys).
func (s *Server) registerPATRoutes() {
	s.mux.Handle("GET /api/me/tokens", s.requireAuth(http.HandlerFunc(s.handleListPATs)))
	s.mux.Handle("POST /api/me/tokens", s.requireAuth(http.HandlerFunc(s.handleCreatePAT)))
	s.mux.Handle("DELETE /api/me/tokens/{token_id}", s.requireAuth(s.gateDerivation(auth.ChangeCredential, s.handleRevokePAT)))
}

type createPATReq struct {
	Name string `json:"name"`
	// TeamID optionally pins the token to one of the user's teams.
	TeamID string `json:"team_id,omitempty"`
	// ExpiresInDays sets an expiry relative to now. 0 = no expiry
	// (unless the platform enforces ITERION_PAT_MAX_TTL).
	ExpiresInDays int `json:"expires_in_days,omitempty"`
}

func (s *Server) handleListPATs(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	toks, err := s.pats.ListByUser(r.Context(), id.UserID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if toks == nil {
		toks = []pat.Token{}
	}
	writeJSON(w, struct {
		Tokens []pat.Token `json:"tokens"`
	}{Tokens: toks})
}

func (s *Server) handleCreatePAT(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	// A token is minted from a live session only: never from a personal
	// access token (403 — it is pinned, and a child would escape its pin and
	// its expiry), never from the access token of a revoked session, a
	// disabled or a pending account (401). The token is the one credential a
	// session mints that is meant to outlive it: its own expiry and the
	// account's "sign out everywhere" bound it, not the session's.
	if _, err := s.checkDerivation(r.Context(), id, auth.MintPAT); err != nil {
		s.derivationError(w, r, err)
		return
	}
	var req createPATReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name required")
		return
	}
	if req.ExpiresInDays < 0 {
		httpError(w, http.StatusBadRequest, "expires_in_days must be >= 0")
		return
	}
	if req.TeamID != "" && !s.canViewTeam(r.Context(), id, req.TeamID) {
		httpError(w, http.StatusForbidden, "not a member of team %q", req.TeamID)
		return
	}
	// Pin the creator's ACTIVE team when none is requested — and refuse
	// outright when there is neither: a PAT identity is fixed, so an
	// unpinned token whose owner has no team could never authenticate
	// (identityFromPAT refuses it), yet it would still be minted 201
	// with its plaintext shown once — a dead credential and a support
	// ticket. Super-admins keep unpinned tokens (the /api/admin
	// surfaces are tenant-free by design). Gate F1 of the PR review.
	if req.TeamID == "" && !id.IsSuperAdmin {
		if id.TeamID == "" {
			httpError(w, http.StatusBadRequest,
				"team_id required — your account has no active team, so an unpinned token could never authenticate")
			return
		}
		req.TeamID = id.TeamID
	}
	// The pin is stored verbatim and a PAT identity resolves its team on
	// every use — a team id that names no team (a slug, a typo, a team
	// deleted after the caller's access token was issued) minted 201 and
	// then never authenticated. Membership and existence are different
	// facts, and a super-admin passes the first for any string — so the
	// gate sits AFTER the default pin: it judges the team that will
	// actually ride the token, from whichever path set it. Only
	// "not found" is the caller's fault; a store outage says so.
	if req.TeamID != "" {
		if _, err := s.authStore().GetTeam(r.Context(), req.TeamID); err != nil {
			if errors.Is(err, identity.ErrNotFound) {
				httpError(w, http.StatusBadRequest, "unknown team %q — pass the team UUID (see `iterion remote teams list`)", req.TeamID)
				return
			}
			httpError(w, http.StatusInternalServerError, "check team %q: %v", req.TeamID, err)
			return
		}
	}
	now := time.Now().UTC()
	var expiresAt *time.Time
	if req.ExpiresInDays > 0 {
		e := now.AddDate(0, 0, req.ExpiresInDays)
		expiresAt = &e
	}
	// Platform ceiling: when ITERION_PAT_MAX_TTL is configured, a
	// missing or longer expiry is clamped to it.
	if max := s.cfg.PATMaxTTL; max > 0 {
		ceiling := now.Add(max)
		if expiresAt == nil || expiresAt.After(ceiling) {
			expiresAt = &ceiling
		}
	}
	plaintext, hash, last4, fp, err := pat.MintToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "mint token: %v", err)
		return
	}
	t := pat.Token{
		ID:          uuid.NewString(),
		UserID:      id.UserID,
		Name:        req.Name,
		TokenHash:   hash,
		TokenLast4:  last4,
		Fingerprint: fp,
		TeamID:      req.TeamID,
		CreatedAt:   now,
		ExpiresAt:   expiresAt,
	}
	if err := s.pats.Create(r.Context(), t); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	// Asked again once the row exists: a "sign out everywhere" or a disable
	// whose record landed after the check above and before this row was
	// written may have run its token revocation without seeing it. A
	// revocation recorded after this second check sees the row. A refusal —
	// or a check that could not complete — revokes the token before its
	// plaintext is ever shown.
	if _, err := s.checkDerivation(r.Context(), id, auth.MintPAT); err != nil {
		if rerr := s.pats.Revoke(context.WithoutCancel(r.Context()), t.ID, time.Now().UTC()); rerr != nil {
			s.identityFailure(w, r, fmt.Errorf("revoke the personal access token %s minted from a credential refused since: %w (refusal: %v)", t.ID, rerr, err))
			return
		}
		s.derivationError(w, r, err)
		return
	}
	s.auditTenant(r, id.TeamID, "pat.created", "token", t.ID, map[string]any{"name": t.Name, "team_pin": t.TeamID, "expires_at": expiresAt})
	// A team-pinned token binds the caller's future org scope too — the
	// server owns the team→org fact, so state it in the response instead
	// of leaving clients to re-derive it from the org tree.
	// The token is already minted and its plaintext exists only in the
	// response below, so a failed lookup here cannot abort the request
	// without stranding a token the caller will never see. It reports the
	// scope it could not resolve instead of letting an empty org_id read
	// as "this token has no org".
	orgID := ""
	if req.TeamID != "" {
		team, terr := s.authStore().GetTeam(r.Context(), req.TeamID)
		if terr != nil {
			s.logger.Warn("pat create: token %s is pinned to team %s but its org is unresolved (%v) — org_id omitted from the response", t.ID, req.TeamID, terr)
		} else {
			orgID = team.OrgID
		}
	}
	w.WriteHeader(http.StatusCreated)
	// The plaintext is never recoverable after this response.
	writeJSON(w, struct {
		PAT   pat.Token `json:"pat"`
		Token string    `json:"token"`
		OrgID string    `json:"org_id,omitempty"`
	}{PAT: t, Token: plaintext, OrgID: orgID})
}

func (s *Server) handleRevokePAT(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	tokenID := r.PathValue("token_id")
	t, err := s.pats.Get(r.Context(), tokenID)
	if err != nil {
		if errors.Is(err, pat.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if t.UserID != id.UserID && !id.IsSuperAdmin {
		httpError(w, http.StatusForbidden, "cannot revoke this token")
		return
	}
	if err := s.pats.Revoke(r.Context(), tokenID, time.Now().UTC()); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, id.TeamID, "pat.revoked", "token", tokenID, map[string]any{"name": t.Name})
	w.WriteHeader(http.StatusNoContent)
}

// patIdentityFailure marks a PAT resolution that FAILED — a row the server
// could not read — as opposed to one that REFUSED the credential. The
// middleware answers a refusal 401 with its own message and a failure
// through identityFailure (500, the generic body, the cause logged): a store
// outage answered 401 says "this token is dead" to a client that then throws
// away a valid credential (cmd/iterion runRemoteLogin discards the token it
// just minted on a 401 from /api/auth/me), and the driver's text names the
// identity cluster's hosts to anyone holding any token.
type patIdentityFailure struct{ err error }

func (e patIdentityFailure) Error() string { return e.err.Error() }
func (e patIdentityFailure) Unwrap() error { return e.err }

// patReadFailure wraps a store error as a failure unless the store answered
// "no such row", which IS a fact about the credential: that stays a refusal,
// and carries `refusal` as its message.
func patReadFailure(what string, err error, refusal string) error {
	if errors.Is(err, identity.ErrNotFound) || errors.Is(err, pat.ErrNotFound) {
		return errors.New(refusal)
	}
	return patIdentityFailure{fmt.Errorf("%s: %w", what, err)}
}

// identityFromPAT resolves an `iap_` bearer into an authenticated
// Identity: hash lookup → usable (not revoked/expired) → owning user
// must be active → team resolution (pin or default team) with the
// membership re-checked at every use so a removed member's PAT dies
// with the membership. Inherits the user's role + super-admin flag —
// the v1 PAT security model documented on pkg/pat.
//
// Four rows are read, and each read has two outcomes the caller must keep
// apart: a row the store says is NOT THERE (or a hash that does not verify,
// a revoked token, an inactive owner) refuses the credential; a row the
// store could not READ is a patIdentityFailure, the server's own failure.
func (s *Server) identityFromPAT(ctx context.Context, presented string) (auth.Identity, error) {
	if s.pats == nil {
		return auth.Identity{}, errors.New("personal access tokens not enabled")
	}
	t, err := s.pats.GetByTokenHash(ctx, pat.HashToken(presented))
	if err != nil {
		// Same message either way: the refusal says nothing about whether a
		// row exists for this hash.
		return auth.Identity{}, patReadFailure("read the token row", err, "token invalid")
	}
	if !pat.VerifyToken(presented, t.TokenHash) {
		return auth.Identity{}, errors.New("token invalid")
	}
	now := time.Now().UTC()
	if !t.Usable(now) {
		return auth.Identity{}, errors.New("token revoked or expired")
	}
	st := s.authStore()
	if st == nil {
		return auth.Identity{}, errors.New("auth not configured")
	}
	u, err := st.GetUser(ctx, t.UserID)
	if err != nil {
		return auth.Identity{}, patReadFailure(fmt.Sprintf("read the owner %s of token %s", t.UserID, t.ID), err, "token owner unavailable")
	}
	if u.Status != identity.UserStatusActive {
		return auth.Identity{}, errors.New("token owner unavailable")
	}
	teamID := t.TeamID
	if teamID == "" {
		teamID = u.DefaultTeamID
	}
	// A PAT identity is FIXED — no session to switch teams on — so a
	// token that resolves to no team can never do tenant-scoped work.
	// Refuse it explicitly here rather than 403 it per-request (super-
	// admins excepted: the /api/admin surfaces are tenant-free by
	// design). This is the mint-side half of the requireAuth tenant
	// choke (Sentry ITERION-13/-1W/-1Z).
	if teamID == "" && !u.IsSuperAdmin {
		return auth.Identity{}, errors.New("token carries no team scope (owner has no default team) — re-create it with an explicit team")
	}
	var role identity.Role
	var orgID string
	if teamID != "" {
		mb, err := st.GetMembership(ctx, u.ID, teamID)
		if err != nil {
			return auth.Identity{}, patReadFailure(fmt.Sprintf("read the membership of %s in team %s", u.ID, teamID), err, "token team unavailable")
		}
		role = mb.Role
		// The parent org, which the browser path carries on the JWT. A PAT
		// identity is FIXED for the token's whole life — there is no
		// session to switch scope on — so an org dropped because the read
		// failed is a lie the token then carries everywhere, silently
		// failing every org-scoped lookup that has no team fallback. Refuse
		// like the membership read above rather than mint a narrower
		// identity than the token was granted.
		team, err := st.GetTeam(ctx, teamID)
		if err != nil {
			return auth.Identity{}, patReadFailure(fmt.Sprintf("read the org scope of team %s", teamID), err, "token org scope unavailable")
		}
		orgID = team.OrgID
	}
	// last_used_at is observability — detached write off the hot path.
	tokenID := t.ID
	s.goSafe("pat-markused", func() {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.pats.MarkUsed(bg, tokenID, now)
	})
	var expires time.Time
	if t.ExpiresAt != nil {
		expires = t.ExpiresAt.UTC()
	}
	return auth.Identity{
		UserID: u.ID,
		Email:  u.Email,
		TeamID: teamID,
		// OrgID without OrgRole: the token grants what its TEAM membership
		// grants, never an org-level role the user did not put on it.
		OrgID:        orgID,
		Role:         role,
		IsSuperAdmin: u.IsSuperAdmin,
		Credential:   auth.CredentialPAT,
		CredentialID: t.ID,
		ExpiresAt:    expires,
	}, nil
}

// wirePATState registers, on the auth service this server is built on, the
// reader its chokepoint asks about a personal access token (auth.PATState):
// the row as this server's token store reads it now, so a token revoked or
// expired since it was presented — or since it minted a WS ticket — is
// refused. A row that is gone is a revoked token; a read that fails is
// returned.
func (s *Server) wirePATState() {
	if s.authSvc == nil {
		return
	}
	s.authSvc.SetPATState(func(ctx context.Context, id string, now time.Time) (bool, error) {
		if s.pats == nil {
			return false, errors.New("personal access tokens not enabled")
		}
		t, err := s.pats.Get(ctx, id)
		if errors.Is(err, pat.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return t.Usable(now), nil
	})
}

// revokeAccountPATs revokes every personal access token of userID ("sign
// out everywhere", an admin disable) and returns how many it revoked; none
// when personal access tokens are not enabled.
func (s *Server) revokeAccountPATs(ctx context.Context, userID string) (int64, error) {
	if s.pats == nil {
		return 0, nil
	}
	return s.pats.RevokeAllByUser(ctx, userID, time.Now().UTC())
}
