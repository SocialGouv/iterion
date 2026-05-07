package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// registerOAuthForfaitRoutes wires the per-user OAuth subscription
// management endpoints. Phase D.
func (s *Server) registerOAuthForfaitRoutes() {
	s.mux.Handle("GET /api/me/oauth/connections", s.requireAuth(http.HandlerFunc(s.handleListOAuthConnections)))
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
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
}

func toOAuthView(r secrets.OAuthRecord) oauthConnectionView {
	v := oauthConnectionView{
		Kind:      string(r.Kind),
		Scopes:    r.Scopes,
		CreatedAt: r.CreatedAt.Format(time.RFC3339),
		UpdatedAt: r.UpdatedAt.Format(time.RFC3339),
	}
	if r.AccessTokenExpiresAt != nil {
		t := r.AccessTokenExpiresAt.Format(time.RFC3339)
		v.AccessTokenExpiresAt = &t
	}
	if r.LastRefreshedAt != nil {
		t := r.LastRefreshedAt.Format(time.RFC3339)
		v.LastRefreshedAt = &t
	}
	return v
}

func (s *Server) handleListOAuthConnections(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	records, err := s.oauthStore.ListByUser(r.Context(), id.UserID)
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

func (s *Server) handleUploadOAuthCredentials(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	kind := secrets.OAuthKind(r.PathValue("kind"))
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

	now := time.Now().UTC()
	rec := secrets.OAuthRecord{
		ID:        fmt.Sprintf("%s|%s", id.UserID, kind),
		UserID:    id.UserID,
		Kind:      kind,
		CreatedAt: now,
		UpdatedAt: now,
	}
	switch kind {
	case secrets.OAuthKindClaudeCode:
		v, err := secrets.ParseAnthropicView(body)
		if err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		if v.ClaudeAIOauth.AccessToken == "" {
			httpError(w, http.StatusBadRequest, "credentials.json missing claudeAiOauth.accessToken")
			return
		}
		if v.ClaudeAIOauth.ExpiresAt > 0 {
			t := time.UnixMilli(v.ClaudeAIOauth.ExpiresAt).UTC()
			rec.AccessTokenExpiresAt = &t
		}
		rec.Scopes = v.ClaudeAIOauth.Scopes
	case secrets.OAuthKindCodex:
		v, err := secrets.ParseCodexView(body)
		if err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		if v.Tokens.AccessToken == "" {
			httpError(w, http.StatusBadRequest, "auth.json missing tokens.access_token")
			return
		}
		if v.Tokens.ExpiresIn > 0 {
			t := time.Now().Add(time.Duration(v.Tokens.ExpiresIn) * time.Second).UTC()
			rec.AccessTokenExpiresAt = &t
		}
	}

	sealed, err := secrets.SealOAuthPayload(s.sealer, id.UserID, kind, body)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "seal: %v", err)
		return
	}
	rec.SealedPayload = sealed

	if err := s.oauthStore.Upsert(r.Context(), rec); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.logger.Info("oauth: user=%s kind=%s connected (sealed payload, expires=%v)", id.UserID, kind, rec.AccessTokenExpiresAt)
	writeJSON(w, toOAuthView(rec))
}

func (s *Server) handleRefreshOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	kind := secrets.OAuthKind(r.PathValue("kind"))
	if !kind.Valid() {
		httpError(w, http.StatusBadRequest, "unknown oauth kind")
		return
	}
	rec, err := s.oauthStore.Get(r.Context(), id.UserID, kind)
	if err != nil {
		if errors.Is(err, secrets.ErrOAuthNotFound) {
			httpError(w, http.StatusNotFound, "no oauth connection of kind %s", kind)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if err := s.refreshOAuthRecord(r.Context(), &rec); err != nil {
		httpError(w, http.StatusBadGateway, "refresh: %v", err)
		return
	}
	if err := s.oauthStore.Upsert(r.Context(), rec); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, toOAuthView(rec))
}

func (s *Server) handleDeleteOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	kind := secrets.OAuthKind(r.PathValue("kind"))
	if !kind.Valid() {
		httpError(w, http.StatusBadRequest, "unknown oauth kind")
		return
	}
	if err := s.oauthStore.Delete(r.Context(), id.UserID, kind); err != nil {
		if errors.Is(err, secrets.ErrOAuthNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refreshOAuthRecord drives the refresh exchange for one record and
// rewrites its sealed payload with the new tokens. Returns an error
// when the upstream provider rejects the refresh.
func (s *Server) refreshOAuthRecord(ctx context.Context, rec *secrets.OAuthRecord) error {
	payload, err := secrets.OpenOAuthPayload(s.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
	if err != nil {
		return fmt.Errorf("unseal: %w", err)
	}
	now := time.Now().UTC()
	switch rec.Kind {
	case secrets.OAuthKindClaudeCode:
		view, perr := secrets.ParseAnthropicView(payload)
		if perr != nil {
			return perr
		}
		clientID := strings.TrimSpace(s.cfg.AnthropicOAuthClientID)
		if clientID == "" {
			return fmt.Errorf("ITERION_OAUTH_FORFAIT_ANTHROPIC_CLIENT_ID not configured")
		}
		res, rerr := secrets.RefreshAnthropic(ctx, s.httpClient, clientID, view.ClaudeAIOauth.RefreshToken)
		if rerr != nil {
			return rerr
		}
		updated, uerr := secrets.ApplyAnthropicRefresh(payload, res)
		if uerr != nil {
			return uerr
		}
		sealed, serr := secrets.SealOAuthPayload(s.sealer, rec.UserID, rec.Kind, updated)
		if serr != nil {
			return serr
		}
		rec.SealedPayload = sealed
		if !res.ExpiresAt.IsZero() {
			t := res.ExpiresAt
			rec.AccessTokenExpiresAt = &t
		}
		if len(res.Scopes) > 0 {
			rec.Scopes = res.Scopes
		}
	case secrets.OAuthKindCodex:
		view, perr := secrets.ParseCodexView(payload)
		if perr != nil {
			return perr
		}
		clientID := strings.TrimSpace(s.cfg.CodexOAuthClientID)
		if clientID == "" {
			return fmt.Errorf("ITERION_OAUTH_FORFAIT_OPENAI_CLIENT_ID not configured")
		}
		res, rerr := secrets.RefreshCodex(ctx, s.httpClient, clientID, view.Tokens.RefreshToken)
		if rerr != nil {
			return rerr
		}
		updated, uerr := secrets.ApplyCodexRefresh(payload, res)
		if uerr != nil {
			return uerr
		}
		sealed, serr := secrets.SealOAuthPayload(s.sealer, rec.UserID, rec.Kind, updated)
		if serr != nil {
			return serr
		}
		rec.SealedPayload = sealed
		if !res.ExpiresAt.IsZero() {
			t := res.ExpiresAt
			rec.AccessTokenExpiresAt = &t
		}
	}
	rec.LastRefreshedAt = &now
	rec.UpdatedAt = now
	return nil
}
