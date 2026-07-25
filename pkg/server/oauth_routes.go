package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// registerOAuthForfaitRoutes wires the per-user OAuth subscription
// management endpoints. The team-scoped (org) mirror lives in
// oauth_team_routes.go; both delegate to the *ForOwner helpers below so
// the personal and org flows can never diverge.
func (s *Server) registerOAuthForfaitRoutes() {
	s.mux.Handle("GET /api/me/oauth/connections", s.requireAuth(http.HandlerFunc(s.handleListOAuthConnections)))
	// Browser OAuth (authorization-code + PKCE), the cloud-viable way to
	// connect without `claude login` or pasting a credentials.json file.
	s.mux.Handle("POST /api/me/oauth/{kind}/authorize/start", s.requireAuth(http.HandlerFunc(s.handleStartOAuthAuthorize)))
	s.mux.Handle("POST /api/me/oauth/{kind}/authorize/complete", s.requireAuth(http.HandlerFunc(s.handleCompleteOAuthAuthorize)))
	// Raw blob paste — kept as a fallback (power users / Codex).
	s.mux.Handle("POST /api/me/oauth/{kind}/credentials", s.requireAuth(http.HandlerFunc(s.handleUploadOAuthCredentials)))
	s.mux.Handle("POST /api/me/oauth/{kind}/refresh", s.requireAuth(http.HandlerFunc(s.handleRefreshOAuth)))
	s.mux.Handle("DELETE /api/me/oauth/{kind}", s.requireAuth(http.HandlerFunc(s.handleDeleteOAuth)))
}

// oauthConnectionView is the safe-to-display projection of an
// OAuthRecord. Plaintext / sealed payload never leave the server.
type oauthConnectionView struct {
	Kind                 string   `json:"kind"`
	Scopes               []string `json:"scopes,omitempty"`
	AccessTokenExpiresAt *string  `json:"access_token_expires_at,omitempty"`
	LastRefreshedAt      *string  `json:"last_refreshed_at,omitempty"`
	// Refreshable is false when the sealed payload has no refresh token:
	// the token will expire and only a manual re-connect renews it.
	Refreshable bool   `json:"refreshable"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func toOAuthView(r secrets.OAuthRecord) oauthConnectionView {
	return oauthConnectionView{
		Kind:                 string(r.Kind),
		Scopes:               r.Scopes,
		CreatedAt:            r.CreatedAt.Format(time.RFC3339),
		UpdatedAt:            r.UpdatedAt.Format(time.RFC3339),
		AccessTokenExpiresAt: optRFC3339(r.AccessTokenExpiresAt),
		LastRefreshedAt:      optRFC3339(r.LastRefreshedAt),
		Refreshable:          !r.NotRefreshable,
	}
}

// ---- per-user (/me) HTTP handlers ----

func (s *Server) handleListOAuthConnections(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	s.listOAuthForOwner(w, r, id.UserID)
}

func (s *Server) handleStartOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	s.startOAuthForOwner(w, r, id.UserID, secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleCompleteOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	s.completeOAuthForOwner(w, r, id.UserID, secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleUploadOAuthCredentials(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	s.uploadOAuthForOwner(w, r, id.UserID, secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleRefreshOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	s.refreshOAuthForOwner(w, r, id.UserID, secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleDeleteOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	s.deleteOAuthForOwner(w, r, id.UserID, secrets.OAuthKind(r.PathValue("kind")))
}

// ---- owner-keyed helpers (shared by /me and /teams) ----
//
// ownerKey is the OAuthStore "user_id" partition: the authenticated
// user's id for the personal scope, or secrets.OrgOwnerKey(teamID) for
// the org scope. Everything below is owner-agnostic.

func (s *Server) listOAuthForOwner(w http.ResponseWriter, r *http.Request, ownerKey string) {
	records, err := s.oauthStore.ListByUser(r.Context(), ownerKey)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	views := make([]oauthConnectionView, 0, len(records))
	for _, rec := range records {
		views = append(views, toOAuthView(rec))
	}
	writeJSON(w, struct {
		Connections []oauthConnectionView `json:"connections"`
	}{Connections: views})
}

// startOAuthForOwner kicks off the browser OAuth flow: it mints PKCE +
// state, stashes them server-side, and returns the claude.ai authorize
// URL for the studio to open. Only claude_code supports the browser flow
// today (Codex keeps the paste fallback).
func (s *Server) startOAuthForOwner(w http.ResponseWriter, r *http.Request, ownerKey string, kind secrets.OAuthKind) {
	if !kind.Valid() {
		httpError(w, http.StatusBadRequest, "unknown oauth kind")
		return
	}
	if kind != secrets.OAuthKindClaudeCode {
		httpError(w, http.StatusBadRequest, "browser oauth is only supported for claude_code; use the credentials paste for %s", kind)
		return
	}
	if s.oauthPending == nil {
		httpError(w, http.StatusServiceUnavailable, "browser oauth not configured")
		return
	}
	clientID := s.cfg.AnthropicOAuthClientID
	if clientID == "" {
		httpError(w, http.StatusServiceUnavailable, "anthropic oauth client id not configured")
		return
	}
	verifier, challenge, err := secrets.NewPKCE()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "pkce: %v", err)
		return
	}
	state, err := secrets.NewOAuthState()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "state: %v", err)
		return
	}
	sealedVerifier, err := secrets.SealOAuthVerifier(s.sealer, ownerKey, kind, verifier)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "seal verifier: %v", err)
		return
	}
	redirectURI := secrets.AnthropicRedirectURI()
	now := time.Now().UTC()
	if err := s.oauthPending.Put(r.Context(), secrets.OAuthPending{
		OwnerKey:       ownerKey,
		Kind:           kind,
		SealedVerifier: sealedVerifier,
		State:          state,
		RedirectURI:    redirectURI,
		CreatedAt:      now,
		ExpiresAt:      now.Add(secrets.DefaultOAuthPendingTTL),
	}); err != nil {
		httpError(w, http.StatusInternalServerError, "persist pending: %v", err)
		return
	}
	writeJSON(w, struct {
		AuthorizeURL string `json:"authorize_url"`
		State        string `json:"state"`
	}{
		AuthorizeURL: secrets.AnthropicAuthorizeURL(clientID, redirectURI, challenge, state),
		State:        state,
	})
}

// completeOAuthForOwner finishes the browser flow: it consumes the
// pending PKCE state, exchanges the pasted code for tokens, builds the
// credentials.json blob, and seals it into the OAuthRecord — the exact
// same stored shape the paste path produces.
func (s *Server) completeOAuthForOwner(w http.ResponseWriter, r *http.Request, ownerKey string, kind secrets.OAuthKind) {
	if !kind.Valid() || kind != secrets.OAuthKindClaudeCode {
		httpError(w, http.StatusBadRequest, "browser oauth is only supported for claude_code")
		return
	}
	if s.oauthPending == nil {
		httpError(w, http.StatusServiceUnavailable, "browser oauth not configured")
		return
	}
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad json: %v", err)
		return
	}
	// The headless page shows `code#state`; accept either the full string
	// or a pre-split code, and prefer an explicit state field.
	code, frag := secrets.SplitAnthropicCode(req.Code)
	if code == "" {
		httpError(w, http.StatusBadRequest, "missing authorization code")
		return
	}
	pasteState := req.State
	if pasteState == "" {
		pasteState = frag
	}
	pending, err := s.oauthPending.Take(r.Context(), ownerKey, kind)
	if err != nil {
		httpError(w, http.StatusBadRequest, "no pending authorization (expired? restart the connect)")
		return
	}
	// CSRF: when the page returned a state, it must match the one we
	// minted. (Some headless flows drop the fragment — then we fall back
	// to the single-pending-per-owner guarantee.)
	if pasteState != "" && pasteState != pending.State {
		httpError(w, http.StatusBadRequest, "state mismatch")
		return
	}
	verifier, err := secrets.OpenOAuthVerifier(s.sealer, ownerKey, kind, pending.SealedVerifier)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "unseal verifier: %v", err)
		return
	}
	res, err := secrets.ExchangeAnthropicCode(r.Context(), s.httpClient, s.cfg.AnthropicOAuthClientID, code, verifier, pending.RedirectURI, pending.State)
	if err != nil {
		httpError(w, http.StatusBadGateway, "code exchange: %v", err)
		return
	}
	blob, err := secrets.BuildAnthropicCredentials(res)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "build credentials: %v", err)
		return
	}
	rec, err := s.sealOAuthRecord(r.Context(), ownerKey, kind, blob)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.logger.Info("oauth: owner=%s kind=%s connected via browser flow (expires=%v)", ownerKey, kind, rec.AccessTokenExpiresAt)
	writeJSON(w, toOAuthView(rec))
}

// uploadOAuthForOwner ingests a raw credentials.json / auth.json blob
// (the fallback to the browser flow).
func (s *Server) uploadOAuthForOwner(w http.ResponseWriter, r *http.Request, ownerKey string, kind secrets.OAuthKind) {
	if !kind.Valid() {
		httpError(w, http.StatusBadRequest, "unknown oauth kind")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		httpError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}
	if len(body) == 0 {
		httpError(w, http.StatusBadRequest, "empty body — paste the credentials.json / auth.json content")
		return
	}
	rec, err := s.sealOAuthRecord(r.Context(), ownerKey, kind, body)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	s.logger.Info("oauth: owner=%s kind=%s connected (sealed payload, expires=%v)", ownerKey, kind, rec.AccessTokenExpiresAt)
	writeJSON(w, toOAuthView(rec))
}

// sealOAuthRecord validates a credentials blob, extracts expiry/scope
// metadata, seals it bound to (ownerKey, kind), and upserts the record.
// Shared by the browser flow and the paste path.
func (s *Server) sealOAuthRecord(ctx context.Context, ownerKey string, kind secrets.OAuthKind, blob []byte) (secrets.OAuthRecord, error) {
	now := time.Now().UTC()
	rec := secrets.OAuthRecord{
		// ID is derived in the OAuth store's Upsert (memory + Mongo
		// agree on `<ownerKey>|<kind>`), so we leave it empty here.
		UserID:    ownerKey,
		Kind:      kind,
		CreatedAt: now,
		UpdatedAt: now,
	}
	switch kind {
	case secrets.OAuthKindClaudeCode:
		v, err := secrets.ParseAnthropicView(blob)
		if err != nil {
			return secrets.OAuthRecord{}, err
		}
		if v.ClaudeAIOauth.AccessToken == "" {
			return secrets.OAuthRecord{}, errors.New("credentials.json missing claudeAiOauth.accessToken")
		}
		if v.ClaudeAIOauth.ExpiresAt > 0 {
			t := time.UnixMilli(v.ClaudeAIOauth.ExpiresAt).UTC()
			rec.AccessTokenExpiresAt = &t
		}
		rec.NotRefreshable = v.ClaudeAIOauth.RefreshToken == ""
		rec.Scopes = v.ClaudeAIOauth.Scopes
	case secrets.OAuthKindCodex:
		v, err := secrets.ParseCodexView(blob)
		if err != nil {
			return secrets.OAuthRecord{}, err
		}
		if v.Tokens.AccessToken == "" {
			return secrets.OAuthRecord{}, errors.New("auth.json missing tokens.access_token")
		}
		if v.Tokens.ExpiresIn > 0 {
			t := time.Now().Add(time.Duration(v.Tokens.ExpiresIn) * time.Second).UTC()
			rec.AccessTokenExpiresAt = &t
		}
		rec.NotRefreshable = v.Tokens.RefreshToken == ""
	}
	sealed, err := secrets.SealOAuthPayload(s.sealer, ownerKey, kind, blob)
	if err != nil {
		return secrets.OAuthRecord{}, fmt.Errorf("seal: %w", err)
	}
	rec.SealedPayload = sealed
	if err := s.oauthStore.Upsert(ctx, rec); err != nil {
		return secrets.OAuthRecord{}, err
	}
	return rec, nil
}

func (s *Server) refreshOAuthForOwner(w http.ResponseWriter, r *http.Request, ownerKey string, kind secrets.OAuthKind) {
	if !kind.Valid() {
		httpError(w, http.StatusBadRequest, "unknown oauth kind")
		return
	}
	rec, err := s.oauthStore.Get(r.Context(), ownerKey, kind)
	if err != nil {
		if errors.Is(err, secrets.ErrOAuthNotFound) {
			httpError(w, http.StatusNotFound, "no oauth connection of kind %s", kind)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if err := secrets.RefreshRecord(r.Context(), s.sealer, s.httpClient, s.cfg.AnthropicOAuthClientID, s.cfg.CodexOAuthClientID, &rec); err != nil {
		if errors.Is(err, secrets.ErrNotRefreshable) {
			// Self-heal the record so the background worker stops
			// attempting it; surface an actionable message instead of
			// the raw exchange error.
			if !rec.NotRefreshable {
				rec.NotRefreshable = true
				rec.UpdatedAt = time.Now().UTC()
				if uerr := s.oauthStore.Upsert(r.Context(), rec); uerr != nil {
					s.logger.Warn("oauth: mark not-refreshable %s/%s: %v", ownerKey, kind, uerr)
				}
			}
			httpError(w, http.StatusConflict, "this connection has no refresh token and can't auto-refresh — reconnect it to renew")
			return
		}
		httpError(w, http.StatusBadGateway, "refresh: %v", err)
		return
	}
	if err := s.oauthStore.Upsert(r.Context(), rec); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, toOAuthView(rec))
}

func (s *Server) deleteOAuthForOwner(w http.ResponseWriter, r *http.Request, ownerKey string, kind secrets.OAuthKind) {
	if !kind.Valid() {
		httpError(w, http.StatusBadRequest, "unknown oauth kind")
		return
	}
	if err := s.oauthStore.Delete(r.Context(), ownerKey, kind); err != nil {
		if errors.Is(err, secrets.ErrOAuthNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
