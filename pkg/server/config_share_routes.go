package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	shareCommitAuthorName  = "iterion-share-editor[bot]"
	shareCommitAuthorEmail = "iterion-share-editor@bot.iterion.invalid"
)

// registerConfigSharePublicRoutes wires the self-authenticating editor surface.
// Public to the JWT layer (isPublicPath); each handler is gated by
// configShareAuth (Bearer iws_ token → synthetic KindShare identity).
func (s *Server) registerConfigSharePublicRoutes() {
	s.mux.Handle("GET /api/config-share/{id}/meta", s.configShareAuth(http.HandlerFunc(s.handleConfigShareMeta)))
	s.mux.Handle("GET /api/config-share/{id}/config", s.configShareAuth(http.HandlerFunc(s.handleConfigShareGet)))
	s.mux.Handle("PATCH /api/config-share/{id}/config", s.configShareAuth(http.HandlerFunc(s.handleConfigSharePatch)))
}

func (s *Server) handleConfigShareMeta(w http.ResponseWriter, r *http.Request) {
	sh, ok := configShareFromContext(r.Context())
	if !ok {
		httpError(w, http.StatusUnauthorized, "invalid_share")
		return
	}
	writeJSON(w, map[string]any{
		"bot_id":        sh.BotID,
		"label":         sh.Label,
		"config_path":   sh.ConfigPath,
		"category":      sh.Category,
		"schema_ref":    sh.SchemaRef,
		"allowed_paths": sh.AllowedPaths,
		"visible_paths": sh.VisiblePaths,
		"read_only":     sh.ReadOnly,
	})
}

func (s *Server) handleConfigShareGet(w http.ResponseWriter, r *http.Request) {
	sh, ok := configShareFromContext(r.Context())
	if !ok {
		httpError(w, http.StatusUnauthorized, "invalid_share")
		return
	}
	fc, err := s.resolveShareFC(r.Context(), sh)
	if err != nil {
		httpError(w, http.StatusBadGateway, "config source unavailable")
		return
	}
	proj, sha, err := s.configShareSvc.ProjectedRead(r.Context(), fc, sh)
	if err != nil {
		httpError(w, http.StatusBadGateway, "read failed")
		return
	}
	writeJSON(w, map[string]any{"config": proj, "sha": sha})
}

type configSharePatchReq struct {
	Patch map[string]any `json:"patch"`
	SHA   string         `json:"sha"`
}

func (s *Server) handleConfigSharePatch(w http.ResponseWriter, r *http.Request) {
	sh, ok := configShareFromContext(r.Context())
	if !ok {
		httpError(w, http.StatusUnauthorized, "invalid_share")
		return
	}
	if sh.ReadOnly {
		httpError(w, http.StatusForbidden, "read_only")
		return
	}
	var req configSharePatchReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(req.Patch) == 0 {
		httpError(w, http.StatusBadRequest, "patch has no editable field")
		return
	}
	// The sha the editor read is mandatory — an omitted sha must not blind-write
	// over a concurrent change (e.g. the bot's own state commit to the file).
	if req.SHA == "" {
		httpError(w, http.StatusBadRequest, "sha required")
		return
	}
	msg := "chore(config-share): edit " + sh.ConfigPath + " via share " + sh.TokenLast4
	s.applyShareEditAndRespond(w, r, sh, req, msg)
}

// applyShareEditAndRespond runs ApplyEdit for BOTH the public token path and
// the authenticated config-editor path (ADR-078), recording the delivery and
// writing the response identically so the two surfaces can never drift. The
// caller validates read_only / patch / sha and supplies the commit message.
func (s *Server) applyShareEditAndRespond(w http.ResponseWriter, r *http.Request, sh *configshare.Share, req configSharePatchReq, msg string) {
	fc, err := s.resolveShareFC(r.Context(), sh)
	if err != nil {
		httpError(w, http.StatusBadGateway, "config source unavailable")
		return
	}
	newSHA, changed, err := s.configShareSvc.ApplyEdit(r.Context(), fc, sh, req.Patch, req.SHA, msg, shareCommitAuthorName, shareCommitAuthorEmail)
	switch {
	case errors.Is(err, forge.ErrFileConflict):
		// 409: return the fresh projection so the editor can diff — never clobber.
		s.recordShareDelivery(r, sh, http.StatusConflict, req.SHA, "", nil, "conflict")
		if proj, sha, rerr := s.configShareSvc.ProjectedRead(r.Context(), fc, sh); rerr == nil {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]any{"error": "conflict", "config": proj, "sha": sha})
			return
		}
		httpError(w, http.StatusConflict, "conflict")
	case errors.Is(err, configshare.ErrValidation):
		// A field/patch rejection — 400. Never echo the offending path to the
		// client (recorded server-side in the delivery row for the audit).
		s.recordShareDelivery(r, sh, http.StatusBadRequest, req.SHA, "", nil, err.Error())
		httpError(w, http.StatusBadRequest, "field not editable")
	case err != nil:
		// A forge / transport failure (GitHub down, token invalid) — not the
		// visitor's fault, so 502 rather than a misleading "field not editable".
		s.recordShareDelivery(r, sh, http.StatusBadGateway, req.SHA, "", nil, err.Error())
		httpError(w, http.StatusBadGateway, "config write failed")
	default:
		s.recordShareDelivery(r, sh, http.StatusOK, req.SHA, newSHA, changed, "")
		writeJSON(w, map[string]any{"sha": newSHA, "changed": changed})
	}
}

func (s *Server) recordShareDelivery(r *http.Request, sh *configshare.Share, status int, beforeSHA, afterSHA string, changed []string, errMsg string) {
	// Attribute the edit from the request principal: the token middleware
	// stamps a "share:<id>" UserID (KindShare); an authenticated config-editor
	// is a real user (ADR-078).
	actor := ""
	if id, ok := auth.FromContext(r.Context()); ok {
		if id.Kind == auth.KindShare {
			actor = id.UserID
		} else if id.UserID != "" {
			actor = "user:" + id.UserID
		}
	}
	d := &configshare.Delivery{
		ID: uuid.NewString(), ShareID: sh.ID, TenantID: sh.TenantID, At: time.Now().UTC(),
		SourceIP: s.clientIP(r), UserAgent: r.UserAgent(), Method: r.Method, Actor: actor,
		Status: status, BeforeSHA: beforeSHA, AfterSHA: afterSHA, ChangedPaths: changed, Error: errMsg,
	}
	// Synchronous + detached ctx: the audit trail is the forensic record after
	// a token leak, so it must land even if the client disconnects.
	if err := s.configShares.RecordDelivery(context.Background(), d); err != nil {
		s.logger.Warn("configshare: record delivery for share %s: %v", sh.ID, err)
	}
}

// resolveShareFC returns the forge FileClient for a share — the test hook when
// set, else the real forge_token-backed client.
func (s *Server) resolveShareFC(ctx context.Context, sh *configshare.Share) (forge.FileClient, error) {
	if s.configShareFC != nil {
		return s.configShareFC(ctx, sh)
	}
	return s.shareFileClient(ctx, sh)
}

// shareFileClient resolves the team's forge_token (the same PAT the bot pushes
// with) and builds a FileClient for the share's repo. MVP: GitHub over the team
// generic secret; a repo-narrowed github-app token is a follow-up hardening.
func (s *Server) shareFileClient(ctx context.Context, sh *configshare.Share) (forge.FileClient, error) {
	if s.genericSecrets == nil || s.sealer == nil {
		return nil, fmt.Errorf("forge credentials not configured")
	}
	ctx = store.WithTenant(ctx, sh.TenantID)
	res, err := secrets.ResolveGenericWithBindings(ctx, s.genericSecrets, s.botBindings, sh.TenantID, "", sh.BotID, []string{"forge_token"}, nil, s.sealer, s.logger)
	if err != nil {
		return nil, err
	}
	tok, ok := res["forge_token"]
	if !ok || len(tok.Plaintext) == 0 {
		return nil, fmt.Errorf("no forge_token bound for team/bot")
	}
	base, err := shareRepoBase(sh.RepoURL)
	if err != nil {
		return nil, err
	}
	return forgegithub.New(s.forgeHTTPClient(), base, string(tok.Plaintext)), nil
}

func shareRepoBase(repoURL string) (string, error) {
	u, err := url.Parse(repoURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("bad repo url %q", repoURL)
	}
	return u.Scheme + "://" + u.Host, nil
}
