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
	s.mux.Handle("DELETE /api/me/tokens/{token_id}", s.requireAuth(http.HandlerFunc(s.handleRevokePAT)))
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
	s.auditTenant(r, id.TeamID, "pat.created", "token", t.ID, map[string]any{"name": t.Name, "team_pin": t.TeamID, "expires_at": expiresAt})
	// A team-pinned token binds the caller's future org scope too — the
	// server owns the team→org fact, so state it in the response instead
	// of leaving clients to re-derive it from the org tree.
	orgID := ""
	if req.TeamID != "" {
		if team, terr := s.authStore().GetTeam(r.Context(), req.TeamID); terr == nil {
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

// identityFromPAT resolves an `iap_` bearer into an authenticated
// Identity: hash lookup → usable (not revoked/expired) → owning user
// must be active → team resolution (pin or default team) with the
// membership re-checked at every use so a removed member's PAT dies
// with the membership. Inherits the user's role + super-admin flag —
// the v1 PAT security model documented on pkg/pat.
func (s *Server) identityFromPAT(ctx context.Context, presented string) (auth.Identity, error) {
	if s.pats == nil {
		return auth.Identity{}, errors.New("personal access tokens not enabled")
	}
	t, err := s.pats.GetByTokenHash(ctx, pat.HashToken(presented))
	if err != nil || !pat.VerifyToken(presented, t.TokenHash) {
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
	if err != nil || u.Status != identity.UserStatusActive {
		return auth.Identity{}, errors.New("token owner unavailable")
	}
	teamID := t.TeamID
	if teamID == "" {
		teamID = u.DefaultTeamID
	}
	var role identity.Role
	var orgID string
	if teamID != "" {
		mb, err := st.GetMembership(ctx, u.ID, teamID)
		if err != nil {
			return auth.Identity{}, fmt.Errorf("token team unavailable")
		}
		role = mb.Role
		// The parent org, which the browser path carries on the JWT. A PAT
		// identity without it silently fails every org-scoped lookup that
		// has no team fallback of its own.
		if t, err := st.GetTeam(ctx, teamID); err == nil {
			orgID = t.OrgID
		}
	}
	// last_used_at is observability — detached write off the hot path.
	tokenID := t.ID
	s.goSafe("pat-markused", func() {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.pats.MarkUsed(bg, tokenID, now)
	})
	return auth.Identity{
		UserID: u.ID,
		Email:  u.Email,
		TeamID: teamID,
		// OrgID without OrgRole: the token grants what its TEAM membership
		// grants, never an org-level role the user did not put on it.
		OrgID:        orgID,
		Role:         role,
		IsSuperAdmin: u.IsSuperAdmin,
	}, nil
}
