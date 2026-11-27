package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/pat"
)

// registerPATRoutes wires the personal-access-token lifecycle. URL space is
// /api/me/tokens — deliberately distinct from /api/me/api-keys (BYOK
// LLM provider keys).
//
// The routes that put a credential in the caller's hands — the regenerate,
// which mints one, and the enable, which re-arms one — are gated as the mint
// of a personal access token (auth.MintPAT): a session does them, a personal
// access token is refused (403) like at POST /api/me/tokens. The routes that
// only take a credential away — revoke, disable, purge — are a change of the
// account's credentials (auth.ChangeCredential), which a personal access
// token may make.
func (s *Server) registerPATRoutes() {
	s.mux.Handle("GET /api/me/tokens", s.requireAuth(http.HandlerFunc(s.handleListPATs)))
	s.mux.Handle("POST /api/me/tokens", s.requireAuth(http.HandlerFunc(s.handleCreatePAT)))
	s.mux.Handle("DELETE /api/me/tokens/{token_id}", s.requireAuth(s.gateDerivation(auth.ChangeCredential, s.handleRevokePAT)))
	s.mux.Handle("POST /api/me/tokens/{token_id}/regenerate", s.requireAuth(s.gateDerivation(auth.MintPAT, s.handleRegeneratePAT)))
	s.mux.Handle("POST /api/me/tokens/{token_id}/disable", s.requireAuth(s.gateDerivation(auth.ChangeCredential, s.handleDisablePAT)))
	s.mux.Handle("POST /api/me/tokens/{token_id}/enable", s.requireAuth(s.gateDerivation(auth.MintPAT, s.handleEnablePAT)))
	s.mux.Handle("POST /api/me/tokens/{token_id}/purge", s.requireAuth(s.gateDerivation(auth.ChangeCredential, s.handlePurgePAT)))
}

// includeRevokedParam is the query parameter GET /api/me/tokens reads to
// list the tokens that no longer authenticate.
const includeRevokedParam = "include_revoked"

type createPATReq struct {
	Name string `json:"name"`
	// TeamID optionally pins the token to one of the user's teams.
	TeamID string `json:"team_id,omitempty"`
	// ExpiresInDays sets an expiry relative to now. 0 = no expiry
	// (unless the platform enforces ITERION_PAT_MAX_TTL).
	ExpiresInDays int `json:"expires_in_days,omitempty"`
}

// handleListPATs lists the caller's tokens. By default it lists the ones
// that can still be used again: live and disabled tokens. A revoked or
// expired token authenticates nothing ever again, so it is listed only with
// ?include_revoked=1 — where a revoked row is purged.
func (s *Server) handleListPATs(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	includeRevoked := false
	if raw := r.URL.Query().Get(includeRevokedParam); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			httpError(w, http.StatusBadRequest, "%s must be a boolean (1, true, 0, false), got %q", includeRevokedParam, raw)
			return
		}
		includeRevoked = v
	}
	toks, err := s.pats.ListByUser(r.Context(), id.UserID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	out := make([]pat.Token, 0, len(toks))
	now := time.Now().UTC()
	for _, t := range toks {
		if includeRevoked || !patIsDead(t, now) {
			out = append(out, t)
		}
	}
	writeJSON(w, struct {
		Tokens []pat.Token `json:"tokens"`
	}{Tokens: out})
}

// patIsDead reports whether t will never authenticate again: revoked, or past
// its expiry. A disabled token is not dead — an enable restores it.
func patIsDead(t pat.Token, now time.Time) bool {
	return t.RevokedAt != nil || patExpired(t, now)
}

// patExpired reads the expiry the way pat.Token.Usable does.
func patExpired(t pat.Token, now time.Time) bool {
	return t.ExpiresAt != nil && now.After(*t.ExpiresAt)
}

// clampPATExpiry applies the platform ceiling: when ITERION_PAT_MAX_TTL is
// configured, a missing or longer expiry is clamped to now + that TTL.
func (s *Server) clampPATExpiry(now time.Time, expiresAt *time.Time) *time.Time {
	if max := s.cfg.PATMaxTTL; max > 0 {
		ceiling := now.Add(max)
		if expiresAt == nil || expiresAt.After(ceiling) {
			return &ceiling
		}
	}
	return expiresAt
}

// mintPATRow mints a token for id's own account, writes its row, and asks the
// chokepoint again once the row exists: a "sign out everywhere" or a disable
// whose record landed after the route's first check and before this row was
// written may have run its token revocation without seeing it. A revocation
// recorded after this second check sees the row. A refusal — or a check that
// could not complete — revokes the token before its plaintext is ever shown;
// the response is then written here and ok is false.
func (s *Server) mintPATRow(w http.ResponseWriter, r *http.Request, id auth.Identity, t pat.Token) (plaintext string, minted pat.Token, ok bool) {
	plaintext, hash, last4, fp, err := pat.MintToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "mint token: %v", err)
		return "", pat.Token{}, false
	}
	t.ID = uuid.NewString()
	t.UserID = id.UserID
	t.TokenHash, t.TokenLast4, t.Fingerprint = hash, last4, fp
	if err := s.pats.Create(r.Context(), t); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return "", pat.Token{}, false
	}
	if _, err := s.checkDerivation(r.Context(), id, auth.MintPAT); err != nil {
		if rerr := s.pats.Revoke(context.WithoutCancel(r.Context()), t.ID, time.Now().UTC()); rerr != nil {
			s.identityFailure(w, r, fmt.Errorf("revoke the personal access token %s minted from a credential refused since: %w (refusal: %v)", t.ID, rerr, err))
			return "", pat.Token{}, false
		}
		s.derivationError(w, r, err)
		return "", pat.Token{}, false
	}
	return plaintext, t, true
}

// pinnedOrgOf answers the org of the team a minted token is pinned to, for
// the response. The token is already minted and its plaintext exists only in
// that response, so a failed lookup cannot abort the request without
// stranding a token the caller will never see: it is logged and the org
// omitted, rather than an empty org_id read as "this token has no org".
func (s *Server) pinnedOrgOf(r *http.Request, t pat.Token) string {
	if t.TeamID == "" {
		return ""
	}
	team, err := s.authStore().GetTeam(r.Context(), t.TeamID)
	if err != nil {
		s.logger.Warn("pat mint: token %s is pinned to team %s but its org is unresolved (%v) — org_id omitted from the response", t.ID, t.TeamID, err)
		return ""
	}
	return team.OrgID
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
		holds, err := s.pinMembershipStands(r.Context(), id.UserID, req.TeamID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "check your membership of team %q: %v", req.TeamID, err)
			return
		}
		if !holds {
			httpError(w, http.StatusForbidden, "not a member of team %q", req.TeamID)
			return
		}
	}
	now := time.Now().UTC()
	var expiresAt *time.Time
	if req.ExpiresInDays > 0 {
		e := now.AddDate(0, 0, req.ExpiresInDays)
		expiresAt = &e
	}
	expiresAt = s.clampPATExpiry(now, expiresAt)
	plaintext, t, ok := s.mintPATRow(w, r, id, pat.Token{
		Name:      req.Name,
		TeamID:    req.TeamID,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	})
	if !ok {
		return
	}
	s.auditTenant(r, id.TeamID, "pat.created", "token", t.ID, map[string]any{"name": t.Name, "team_pin": t.TeamID, "expires_at": expiresAt})
	// A team-pinned token binds the caller's future org scope too — the
	// server owns the team→org fact, so state it in the response instead
	// of leaving clients to re-derive it from the org tree.
	orgID := s.pinnedOrgOf(r, t)
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
		// A row purged between the read and the revoke was revoked before it
		// went: the token is gone, which is what a revoke asks for.
		if errors.Is(err, pat.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, id.TeamID, "pat.revoked", "token", tokenID, map[string]any{"name": t.Name})
	s.answerPATSweep(w, s.sweepRevokedPAT(r, id, tokenID))
}

// sweepRevokedPAT ends what a revoked token sent by email (ADR-174): an
// accept refuses an invitation whose minting token no longer stands, so the
// sweep takes those rows out of the pending lists and NAMES them — a rotation
// (the CLI's `teams switch`, a regenerate) must show who to invite again, not
// end their invitations in silence. The revoke reaches any token of the
// account, but a credential pinned to one team reads no other team's
// invitations: those it ended are counted, not named. Every path that
// revokes one token — DELETE, regenerate, purge — answers through it.
func (s *Server) sweepRevokedPAT(r *http.Request, id auth.Identity, tokenID string) patRevokedView {
	ended, sweepErr := s.revokeInvitationsMintedWith(r, tokenID)
	view := patRevokedView{InvitationsRevoked: []revokedInvitationView{}, InvitationsError: sweepErr}
	for _, inv := range ended {
		if id.PinRefusesTeam(inv.TeamID) {
			view.InvitationsRevokedElsewhere++
			continue
		}
		view.InvitationsRevoked = append(view.InvitationsRevoked, inv)
	}
	return view
}

// endedNothing reports whether a sweep took nothing out and failed nowhere.
func (v patRevokedView) endedNothing() bool {
	return len(v.InvitationsRevoked) == 0 && v.InvitationsRevokedElsewhere == 0 && v.InvitationsError == ""
}

// answerPATSweep answers a revoke or a purge: 204 when its sweep ended
// nothing, else 200 with what it ended.
func (s *Server) answerPATSweep(w http.ResponseWriter, view patRevokedView) {
	if view.endedNothing() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, view)
}

// patRegeneratedView answers a regenerate: the replacement token, its
// plaintext (shown once, like a mint's), the token it replaced, and what
// revoking that one ended. RevokeError is set when the replaced token could
// NOT be revoked: the replacement is minted and handed over all the same —
// its plaintext exists nowhere else — and the replaced one still
// authenticates until a DELETE /api/me/tokens/{token_id} of it lands.
type patRegeneratedView struct {
	PAT         pat.Token `json:"pat"`
	Token       string    `json:"token"`
	OrgID       string    `json:"org_id,omitempty"`
	Replaced    string    `json:"replaced"`
	RevokeError string    `json:"revoke_error,omitempty"`
	// What revoking the replaced token ended — patRevokedView's fields,
	// spelled out so the published schema carries them.
	InvitationsRevoked          []revokedInvitationView `json:"invitations_revoked"`
	InvitationsRevokedElsewhere int                     `json:"invitations_revoked_elsewhere,omitempty"`
	InvitationsError            string                  `json:"invitations_error,omitempty"`
}

// handleRegeneratePAT replaces a token by a new one with the same name, team
// pin and expiry instant, then revokes it the way DELETE does — its pending
// invitations end and are named.
//
// The replacement keeps the replaced token's absolute expiry (clamped to the
// platform ceiling from now), never its duration re-applied from now: a
// duration would let a regenerate renew a token indefinitely, a life its
// owner never chose. An expired token is therefore refused (409) — its
// replacement would be born dead.
//
// It mints BEFORE it revokes. Revoking first would leave the owner with no
// credential at all when the mint then fails, and a regenerate could not be
// re-issued on the revoked token; minting first leaves, on a revoke that
// fails, two live tokens — the response says so (revoke_error) and still
// hands the replacement over, so no minted secret is lost and the caller
// knows the one act left: revoke the replaced token. The revoke is a
// compare-and-set (pat.Store.RevokeIfLive): a regenerate that finds the token
// already retired by a concurrent call — another regenerate, a revoke, a
// purge — revokes its own replacement and answers 409 pat_revoked, so one
// token is replaced at most once.
//
// Only the owner regenerates a token: the replacement authenticates as its
// account, and nobody mints a credential of another account. A disabled token
// regenerates into a live replacement: the owner, from a session, could mint
// one anyway.
func (s *Server) handleRegeneratePAT(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	tokenID := r.PathValue("token_id")
	old, err := s.pats.Get(r.Context(), tokenID)
	if err != nil {
		if errors.Is(err, pat.ErrNotFound) {
			httpError(w, http.StatusNotFound, "token %s not found", tokenID)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if old.UserID != id.UserID {
		httpError(w, http.StatusForbidden, "only the token's owner regenerates it")
		return
	}
	now := time.Now().UTC()
	if old.RevokedAt != nil {
		s.httpErrorCode(w, r, http.StatusConflict, "pat_revoked", "token %s is revoked: create a new token instead", tokenID)
		return
	}
	if patExpired(old, now) {
		s.httpErrorCode(w, r, http.StatusConflict, "pat_expired", "token %s expired at %s: its replacement would keep that expiry, so create a new token instead", tokenID, old.ExpiresAt.Format(time.RFC3339))
		return
	}
	// The pin is what the replacement keeps, so it is judged as the mint
	// judges one: an unpinned token only a super-admin mints, and a pin on a
	// team its owner no longer reaches (or that is gone) would mint a token
	// that never authenticates.
	if old.TeamID == "" && !id.IsSuperAdmin {
		s.httpErrorCode(w, r, http.StatusConflict, "pat_unpinned", "token %s is pinned to no team, which only a super-admin mints: create a token pinned to a team instead", tokenID)
		return
	}
	if old.TeamID != "" {
		holds, err := s.pinMembershipStands(r.Context(), old.UserID, old.TeamID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "check your membership of team %q, the pin of token %s: %v", old.TeamID, tokenID, err)
			return
		}
		if !holds {
			s.httpErrorCode(w, r, http.StatusConflict, "pat_pin_unreachable", "token %s is pinned to team %q, which you are no longer a member of: its replacement could never authenticate — revoke it", tokenID, old.TeamID)
			return
		}
		if _, err := s.authStore().GetTeam(r.Context(), old.TeamID); err != nil {
			if errors.Is(err, identity.ErrNotFound) {
				s.httpErrorCode(w, r, http.StatusConflict, "pat_pin_unreachable", "token %s is pinned to team %q, which no longer exists: revoke it", tokenID, old.TeamID)
				return
			}
			httpError(w, http.StatusInternalServerError, "check team %q: %v", old.TeamID, err)
			return
		}
	}
	var expiresAt *time.Time
	if old.ExpiresAt != nil {
		e := *old.ExpiresAt
		expiresAt = &e
	}
	expiresAt = s.clampPATExpiry(now, expiresAt)
	plaintext, t, ok := s.mintPATRow(w, r, id, pat.Token{
		Name:      old.Name,
		TeamID:    old.TeamID,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	})
	if !ok {
		return
	}
	s.auditTenant(r, id.TeamID, "pat.created", "token", t.ID, map[string]any{"name": t.Name, "team_pin": t.TeamID, "expires_at": expiresAt, "regenerated_from": old.ID})
	view := patRegeneratedView{PAT: t, Token: plaintext, OrgID: s.pinnedOrgOf(r, t), Replaced: old.ID, InvitationsRevoked: []revokedInvitationView{}}
	// The replacement exists and its plaintext is only in this response: the
	// revoke runs even when the client has gone. It is a compare-and-set:
	// only the call that retires the token hands a replacement over, so two
	// regenerates of one token never leave two live replacements.
	detached := context.WithoutCancel(r.Context())
	retired, err := s.pats.RevokeIfLive(detached, old.ID, time.Now().UTC())
	switch {
	case err == nil && !retired, errors.Is(err, pat.ErrNotFound):
		// Revoked — or revoked and purged — by another call since it was
		// read: this regenerate replaced nothing, and its replacement goes.
		if rerr := s.pats.Revoke(detached, t.ID, time.Now().UTC()); rerr != nil {
			s.identityFailure(w, r, fmt.Errorf("token %s was revoked by another call during its regenerate, and revoking the replacement %s minted for it failed: %w", old.ID, t.ID, rerr))
			return
		}
		s.auditTenant(r, id.TeamID, "pat.revoked", "token", t.ID, map[string]any{"name": t.Name, "reason": "regenerate_superseded", "replaced": old.ID})
		s.httpErrorCode(w, r, http.StatusConflict, "pat_revoked", "token %s was revoked by another call during this regenerate: nothing was replaced — create a new token if you need one", old.ID)
		return
	case err != nil:
		s.logger.Error("pat regenerate: token %s replaced by %s, but revoking it failed: %v", old.ID, t.ID, err)
		view.RevokeError = fmt.Sprintf("revoking token %s, which this one replaces, failed (%v): it still authenticates — revoke it (DELETE /api/me/tokens/%s)", old.ID, err, old.ID)
	default:
		s.auditTenant(r, id.TeamID, "pat.revoked", "token", old.ID, map[string]any{"name": old.Name, "reason": "regenerated", "replaced_by": t.ID})
		swept := s.sweepRevokedPAT(r, id, old.ID)
		view.InvitationsRevoked, view.InvitationsRevokedElsewhere, view.InvitationsError = swept.InvitationsRevoked, swept.InvitationsRevokedElsewhere, swept.InvitationsError
	}
	w.WriteHeader(http.StatusCreated)
	// The plaintext is never recoverable after this response.
	writeJSON(w, view)
}

// handleDisablePAT suspends a token: it authenticates nothing and the
// invitations it emailed are refused at accept until it is enabled — they are
// NOT swept, so an enable gives them back. The owner or a super-admin may
// disable it, as they may revoke it. A revoked token answers 409.
func (s *Server) handleDisablePAT(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	t, ok := s.patForChange(w, r, id, "disable", true)
	if !ok {
		return
	}
	if err := s.pats.Disable(r.Context(), t.ID, time.Now().UTC()); err != nil {
		s.patStateError(w, r, t.ID, err)
		return
	}
	if t.DisabledAt == nil {
		s.auditTenant(r, id.TeamID, "pat.disabled", "token", t.ID, map[string]any{"name": t.Name})
	}
	s.answerPATRow(w, r, t.ID)
}

// handleEnablePAT lifts a disable: the token authenticates again, and the
// invitations it emailed stand again. Only the owner enables a token — it
// re-arms a credential of their account — and a revoked token answers 409:
// a revocation is final.
func (s *Server) handleEnablePAT(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	t, ok := s.patForChange(w, r, id, "enable", false)
	if !ok {
		return
	}
	if err := s.pats.Enable(r.Context(), t.ID); err != nil {
		s.patStateError(w, r, t.ID, err)
		return
	}
	if t.DisabledAt != nil {
		s.auditTenant(r, id.TeamID, "pat.enabled", "token", t.ID, map[string]any{"name": t.Name})
	}
	s.answerPATRow(w, r, t.ID)
}

// handlePurgePAT removes the row of a REVOKED token — the list then shows it
// nowhere; its audit lines (pat.created, pat.revoked, and the pat.purged this
// writes) stay. A live or disabled token answers 409: revoke it first. It
// finishes the revoke's invitation sweep before the row goes, so no pending
// invitation outlives the row that names its token — a sweep that cannot
// finish answers 409 pat_sweep_unfinished, naming what it did end, and keeps
// the row for the purge to be re-issued. A token already gone answers 204,
// like a revoke of one.
func (s *Server) handlePurgePAT(w http.ResponseWriter, r *http.Request) {
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
		httpError(w, http.StatusForbidden, "cannot purge this token")
		return
	}
	if t.RevokedAt == nil {
		s.httpErrorCode(w, r, http.StatusConflict, "pat_not_revoked", "token %s is not revoked: revoke it first (DELETE /api/me/tokens/%s), then purge it", tokenID, tokenID)
		return
	}
	view := s.sweepRevokedPAT(r, id, tokenID)
	if view.InvitationsError != "" {
		// The row stays so a re-issued purge (or revoke) finishes the sweep;
		// what this one did end is named, as on DELETE.
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, patSweepUnfinishedView{
			Error:              fmt.Sprintf("token %s is kept: %s", tokenID, view.InvitationsError),
			ErrorCode:          "pat_sweep_unfinished",
			InvitationsRevoked: view.InvitationsRevoked, InvitationsRevokedElsewhere: view.InvitationsRevokedElsewhere,
			InvitationsError: view.InvitationsError,
		})
		return
	}
	if err := s.pats.Purge(r.Context(), tokenID); err != nil && !errors.Is(err, pat.ErrNotFound) {
		s.patStateError(w, r, tokenID, err)
		return
	}
	s.auditTenant(r, id.TeamID, "pat.purged", "token", tokenID, map[string]any{"name": t.Name})
	s.answerPATSweep(w, view)
}

// patSweepUnfinishedView answers a purge whose invitation sweep could not
// finish: the coded refusal, and what the sweep did end, as a revoke names it.
type patSweepUnfinishedView struct {
	Error                       string                  `json:"error"`
	ErrorCode                   string                  `json:"error_code"`
	InvitationsRevoked          []revokedInvitationView `json:"invitations_revoked"`
	InvitationsRevokedElsewhere int                     `json:"invitations_revoked_elsewhere,omitempty"`
	InvitationsError            string                  `json:"invitations_error"`
}

// pinMembershipStands reads from the store, not from the presented
// credential's claims, whether userID holds a membership in teamID — the row
// a token pinned there resolves at every use (identityFromPAT). An access
// token's team and role were minted with it and may be stale: a pin judged on
// them could mint a token that never authenticates. A row the store says is
// not there answers false; a read that fails is returned.
func (s *Server) pinMembershipStands(ctx context.Context, userID, teamID string) (bool, error) {
	if _, err := s.authStore().GetMembership(ctx, userID, teamID); err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// patForChange reads the token a disable or an enable names: 404 when there
// is none, 403 when it is another account's — a super-admin passes where
// superAdmin says so. The response is written here when ok is false.
func (s *Server) patForChange(w http.ResponseWriter, r *http.Request, id auth.Identity, act string, superAdmin bool) (pat.Token, bool) {
	tokenID := r.PathValue("token_id")
	t, err := s.pats.Get(r.Context(), tokenID)
	if err != nil {
		if errors.Is(err, pat.ErrNotFound) {
			httpError(w, http.StatusNotFound, "token %s not found", tokenID)
			return pat.Token{}, false
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return pat.Token{}, false
	}
	// A super-admin acts on another account's token only where this change
	// admits it (superAdmin); the owner always may.
	platformMay := superAdmin && id.IsSuperAdmin
	if t.UserID != id.UserID && !platformMay {
		httpError(w, http.StatusForbidden, "cannot %s this token", act)
		return pat.Token{}, false
	}
	return t, true
}

// patStateError answers a store refusal of a lifecycle change.
func (s *Server) patStateError(w http.ResponseWriter, r *http.Request, tokenID string, err error) {
	switch {
	case errors.Is(err, pat.ErrRevoked):
		s.httpErrorCode(w, r, http.StatusConflict, "pat_revoked", "token %s is revoked: a revocation is final — create a new token instead", tokenID)
	case errors.Is(err, pat.ErrNotRevoked):
		s.httpErrorCode(w, r, http.StatusConflict, "pat_not_revoked", "token %s is not revoked: revoke it first (DELETE /api/me/tokens/%s), then purge it", tokenID, tokenID)
	case errors.Is(err, pat.ErrNotFound):
		httpError(w, http.StatusNotFound, "token %s not found", tokenID)
	default:
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
	}
}

// answerPATRow answers the token's row as the store reads it after a change.
func (s *Server) answerPATRow(w http.ResponseWriter, r *http.Request, tokenID string) {
	t, err := s.pats.Get(r.Context(), tokenID)
	if err != nil {
		s.patStateError(w, r, tokenID, err)
		return
	}
	writeJSON(w, t)
}

// patRevokedView answers a revoke that ended invitations: the pending ones
// the token minted, now revoked — named where the caller's own pin reaches
// their team, counted otherwise — and, when the sweep could not finish, why,
// the rows it left being refused at accept all the same.
type patRevokedView struct {
	InvitationsRevoked          []revokedInvitationView `json:"invitations_revoked"`
	InvitationsRevokedElsewhere int                     `json:"invitations_revoked_elsewhere,omitempty"`
	InvitationsError            string                  `json:"invitations_error,omitempty"`
}

type revokedInvitationView struct {
	ID     string `json:"id"`
	Email  string `json:"email"`
	TeamID string `json:"team_id"`
	Role   string `json:"role"`
}

// revokeInvitationsMintedWith deletes the pending invitations personal
// access token tokenID minted, with one invitation.deleted audit line each
// (reason minting_pat_revoked), and answers the ones it took and the first
// failure in words ("" when none). It runs on a context detached from the
// request: the token is already revoked, and the sweep is its consequence.
func (s *Server) revokeInvitationsMintedWith(r *http.Request, tokenID string) ([]revokedInvitationView, string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer cancel()
	// Never null on the wire: a client reads an empty list beside the error.
	ended := []revokedInvitationView{}
	invs, err := s.authStore().ListPendingInvitationsMintedWith(ctx, tokenID)
	if err != nil {
		return ended, fmt.Sprintf("listing the invitations this token sent failed (%v): they stand, an accept refuses them since the token no longer stands, and re-issuing this revoke takes them out", err)
	}
	failure := ""
	for _, inv := range invs {
		if err := s.authStore().DeleteInvitation(ctx, inv.ID); err != nil && !errors.Is(err, identity.ErrNotFound) {
			if failure == "" {
				failure = fmt.Sprintf("revoking invitation %s failed (%v): it stands, an accept refuses it since the token no longer stands, and re-issuing this revoke takes it out", inv.ID, err)
			}
			continue
		}
		s.auditTenant(r, inv.TeamID, "invitation.deleted", "invitation", inv.ID,
			map[string]any{"email": inv.Email, "reason": "minting_pat_revoked", "minted_with": tokenID})
		ended = append(ended, revokedInvitationView{ID: inv.ID, Email: inv.Email, TeamID: inv.TeamID, Role: string(inv.Role)})
	}
	return ended, failure
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
// Identity: hash lookup → usable (not revoked/disabled/expired) → owning user
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
		return auth.Identity{}, errors.New("token revoked, disabled or expired")
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
	// The pin is a property of the TOKEN: an explicit team on the row bounds
	// every team- and org-addressed route this bearer reaches
	// (auth.Identity.PinRefusesTeam / PinRefusesOrg), not merely the team it
	// authenticates with. A token with NO explicit team pins nothing — it
	// follows whatever default team its owner has at each use — so it keeps
	// the owner's full reach, and the two cases stay distinguishable here
	// where t.TeamID is still readable (teamID above has already collapsed
	// them onto the resolved team).
	var teamPin, orgPin string
	if t.TeamID != "" {
		teamPin, orgPin = t.TeamID, orgID
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
		TeamPin:      teamPin,
		OrgPin:       orgPin,
	}, nil
}

// wirePATState registers, on the auth service this server is built on, the
// reader its chokepoint asks about a personal access token (auth.PATState):
// the row as this server's token store reads it now, so a token revoked,
// disabled or expired since it was presented — or since it minted a WS ticket — is
// refused. A row that is gone is a revoked token; a read that fails is
// returned.
func (s *Server) wirePATState() {
	if s.authSvc == nil {
		return
	}
	s.authSvc.SetPATState(func(ctx context.Context, id string, now time.Time) (bool, error) {
		// With personal access tokens off, none authenticates
		// (resolvePATIdentity refuses every one), so none stands: what such
		// a token minted while they were on — an emailed invitation — is
		// refused as the token's own request would be, not left failing.
		if s.pats == nil {
			return false, nil
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
