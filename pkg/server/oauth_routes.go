package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/credpool"
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
	s.mux.Handle("PATCH /api/me/oauth/{kind}", s.requireAuth(http.HandlerFunc(s.handleRenameOAuth)))
	s.mux.Handle("DELETE /api/me/oauth/{kind}", s.requireAuth(http.HandlerFunc(s.handleDeleteOAuth)))
}

// oauthConnectionView is the safe-to-display projection of an
// OAuthRecord. Plaintext / sealed payload never leave the server.
type oauthConnectionView struct {
	Kind string `json:"kind"`
	// AccountLabel is the operator's name for the account behind this
	// credential ("jothedev"). Empty on records connected before labels
	// existed — rename them with PATCH.
	AccountLabel string `json:"account_label,omitempty"`
	// Fingerprint is the credential's stable id, and the SAME value the
	// runtime prints when it picks a credential
	// ("oauth-forfait(org) used … fp=700acc7b…"). Exposing it is what
	// lets an operator answer "whose subscription served that run?"
	// from the API instead of grepping server logs. Non-secret by
	// construction: a hash, never the token.
	Fingerprint          string   `json:"fingerprint,omitempty"`
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
		AccountLabel:         r.AccountLabel,
		Fingerprint:          r.Fingerprint,
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

// handleRenameOAuth names the account behind the caller's own forfait.
func (s *Server) handleRenameOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	s.renameOAuthForOwner(w, r, id.UserID, secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleDeleteOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	s.deleteOAuthForOwner(w, r, id.UserID, secrets.OAuthKind(r.PathValue("kind")))
}

// ---- owner-keyed helpers (shared by /me and /teams) ----
//
// ownerKey is the OAuthStore "user_id" partition: the authenticated
// user's id for the personal scope, secrets.OrgOwnerKey(teamID) for the
// org scope, or secrets.PlatformOwnerKey for the platform scope.
// Everything below is owner-agnostic.

// auditOAuthByOwner records an oauth-forfait mutation in the log the owner
// key belongs to — and ONLY on the store-write success path, so a rejected
// connect/delete never forges a "connected"/"deleted" event (the log used
// to check on admins must not lie). Placed inside the shared helpers,
// after the write, so team AND platform surfaces audit identically:
// keeping the audit at each caller meant it fired even on a 400/404/500
// return from the helper. A personal (/me) owner key is not audited —
// unchanged from the per-user endpoints, which never did. verb is
// "connected" or "deleted".
func (s *Server) auditOAuthByOwner(r *http.Request, ownerKey, verb string, kind secrets.OAuthKind, meta map[string]any) {
	switch {
	case ownerKey == secrets.PlatformOwnerKey:
		s.auditPlatform(r, "", "platform.llm_oauth."+verb, "platform_llm_oauth", string(kind), meta)
	case strings.HasPrefix(ownerKey, secrets.OrgOwnerPrefix):
		s.auditTenant(r, strings.TrimPrefix(ownerKey, secrets.OrgOwnerPrefix), "oauth.org."+verb, "oauth_forfait", string(kind), meta)
	}
}

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
	// Validate the name BEFORE consuming the pending authorization: a
	// refused label must not cost the operator a restarted connect.
	accountLabel, err := normalizeOAuthAccountLabel(r.URL.Query().Get("account_label"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
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
	rec, err := s.sealOAuthRecord(r.Context(), ownerKey, kind, blob, accountLabel)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.logger.Info("oauth: owner=%s kind=%s connected via browser flow (account=%q fp=%s expires=%v)", ownerKey, kind, rec.AccountLabel, rec.Fingerprint, rec.AccessTokenExpiresAt)
	s.auditOAuthByOwner(r, ownerKey, "connected", kind, map[string]any{"flow": "browser", "account_label": rec.AccountLabel, "fingerprint": rec.Fingerprint})
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
	accountLabel, err := normalizeOAuthAccountLabel(r.URL.Query().Get("account_label"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	rec, err := s.sealOAuthRecord(r.Context(), ownerKey, kind, body, accountLabel)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	s.logger.Info("oauth: owner=%s kind=%s connected (sealed payload, account=%q fp=%s expires=%v)", ownerKey, kind, rec.AccountLabel, rec.Fingerprint, rec.AccessTokenExpiresAt)
	s.auditOAuthByOwner(r, ownerKey, "connected", kind, map[string]any{"flow": "paste", "account_label": rec.AccountLabel, "fingerprint": rec.Fingerprint})
	writeJSON(w, toOAuthView(rec))
}

// maxOAuthAccountLabel bounds the account name. It is a display string —
// a handle a human recalls ("jothedev", "SocialGouv Revi") — and the
// connect paths take it as a query parameter, where nothing but the
// server's header limit would otherwise bound it.
const maxOAuthAccountLabel = 120

// normalizeOAuthAccountLabel is the one place a label is accepted from a
// caller (both connect paths and the rename), so the bound holds everywhere.
func normalizeOAuthAccountLabel(raw string) (string, error) {
	label := strings.TrimSpace(raw)
	if n := utf8.RuneCountInString(label); n > maxOAuthAccountLabel {
		return "", fmt.Errorf("account_label is %d characters, max %d", n, maxOAuthAccountLabel)
	}
	return label, nil
}

// sealOAuthRecord validates a credentials blob, extracts expiry/scope
// metadata, seals it bound to (ownerKey, kind), and upserts the record.
// Shared by the browser flow and the paste path; accountLabel arrives
// already normalized by the handler.
func (s *Server) sealOAuthRecord(ctx context.Context, ownerKey string, kind secrets.OAuthKind, blob []byte, accountLabel string) (secrets.OAuthRecord, error) {
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
		// Ingestion gate — the runtime backstop is #624's evidence-based skip,
		// this end catches the garbage before it ever reaches a run: an
		// accessToken with a newline/tab/ANSI escape is a transcript, not a
		// bearer token, and every downstream call would die with a legible
		// "Header has invalid value" for hours before the cause was found.
		if err := secrets.ValidateTokenShape("claudeAiOauth.accessToken", v.ClaudeAIOauth.AccessToken); err != nil {
			return secrets.OAuthRecord{}, err
		}
		// A claude_code record without an expiresAt or scopes is what the CLI
		// reads as "Not logged in" — the credential exists on the server side
		// and can never serve a run. Refusing it here means the operator sees
		// the problem at paste time, not on a paid fleet of dead-on-arrival
		// runs. The clock is stamped at the same time the caller stamps
		// CreatedAt so a nearly-expired paste is caught by the same call.
		if v.ClaudeAIOauth.ExpiresAt <= 0 {
			return secrets.OAuthRecord{}, errors.New("credentials.json missing claudeAiOauth.expiresAt — a claude_code record without it is read by the CLI as 'Not logged in'; re-run `claude login` and paste the fresh credentials.json")
		}
		exp := time.UnixMilli(v.ClaudeAIOauth.ExpiresAt).UTC()
		if !exp.After(now) {
			return secrets.OAuthRecord{}, fmt.Errorf("credentials.json accessToken already expired at %s — re-run `claude login` and paste the fresh credentials.json", exp.Format(time.RFC3339))
		}
		if len(v.ClaudeAIOauth.Scopes) == 0 {
			return secrets.OAuthRecord{}, errors.New("credentials.json missing claudeAiOauth.scopes — a claude_code record without any scope is read by the CLI as 'Not logged in'; re-run `claude login` and paste the fresh credentials.json")
		}
		rec.AccessTokenExpiresAt = &exp
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
		// Same shape gate as claude_code — a whitespace/control char in a
		// bearer token is a paste accident, not a legal credential.
		if err := secrets.ValidateTokenShape("tokens.access_token", v.Tokens.AccessToken); err != nil {
			return secrets.OAuthRecord{}, err
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
	// A human connecting/pasting credentials is the act that says "this
	// subscription": stamp its audit identity here, once. The refresh
	// worker rewrites tokens for the SAME subscription and preserves it.
	// Derived from the account the payload names where it names one, so
	// connecting ONE subscription twice does not open two meters.
	rec.Fingerprint = secrets.SubscriptionFingerprint(kind, blob)
	// The name follows the fingerprint. A re-connect that names no account
	// keeps the previous label ONLY when it provably re-connects the same
	// subscription (codex: same account id; claude_code: the same blob).
	// Any other re-connect may be an account SWAP — the same owner key
	// re-pointed at somebody else's forfait — and inheriting the old name
	// there would answer "whose subscription paid?" with the wrong person.
	// Absent beats wrong: the operator names it (`account_label`) or the
	// listing shows no name.
	rec.AccountLabel = accountLabel
	if rec.AccountLabel == "" {
		if prev, err := s.oauthStore.Get(ctx, ownerKey, kind); err == nil && prev.Fingerprint == rec.Fingerprint {
			rec.AccountLabel = prev.AccountLabel
		}
	}
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

// renameOAuthForOwner sets (or clears) the account label on an existing
// connection. It exists because the label is the only thing that maps a
// credential back to a human account, and every record connected before
// labels existed carries none — so the feature would be inert on exactly
// the credentials an operator most needs to identify today. Renaming
// touches metadata only: the sealed payload, fingerprint and expiry are
// untouched, so a rename can never rotate or invalidate a live key.
func (s *Server) renameOAuthForOwner(w http.ResponseWriter, r *http.Request, ownerKey string, kind secrets.OAuthKind) {
	if !kind.Valid() {
		httpError(w, http.StatusBadRequest, "unknown oauth kind")
		return
	}
	var req struct {
		AccountLabel *string `json:"account_label"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	if req.AccountLabel == nil {
		httpError(w, http.StatusBadRequest, "account_label required (send \"\" to clear it)")
		return
	}
	label, err := normalizeOAuthAccountLabel(*req.AccountLabel)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	// Store-level metadata write, not Get → Upsert: the latter would carry
	// the sealed payload this handler read back over whatever a concurrent
	// refresh committed in between.
	if err := s.oauthStore.SetAccountLabel(r.Context(), ownerKey, kind, label); err != nil {
		if errors.Is(err, secrets.ErrOAuthNotFound) {
			httpError(w, http.StatusNotFound, "no %s connection", kind)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	rec, err := s.oauthStore.Get(r.Context(), ownerKey, kind)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditOAuthByOwner(r, ownerKey, "renamed", kind, map[string]any{"account_label": label, "fingerprint": rec.Fingerprint})
	writeJSON(w, toOAuthView(rec))
}

func (s *Server) deleteOAuthForOwner(w http.ResponseWriter, r *http.Request, ownerKey string, kind secrets.OAuthKind) {
	if !kind.Valid() {
		httpError(w, http.StatusBadRequest, "unknown oauth kind")
		return
	}
	// Consent is given for a SPECIFIC connected subscription. Disconnecting
	// it withdraws that consent, so the pledge goes with it — otherwise the
	// terms would sit there waiting to be rebound to whatever credential is
	// connected next under the same key, which could be a different account
	// entirely.
	//
	// Best-effort: disconnecting is the user's actual request, and a
	// degraded pool store must not trap them into keeping a credential
	// connected. A pledge left behind is caught at acquisition, which parks
	// it the first time its credential turns up missing. Only PERSONAL
	// scopes can hold a pledge — neither an org owner key nor the platform
	// owner key ever does, so skip the guaranteed-miss store call for both.
	if s.credPoolPledges != nil && ownerKey != secrets.PlatformOwnerKey && !strings.HasPrefix(ownerKey, secrets.OrgOwnerPrefix) {
		if err := s.credPoolPledges.Delete(r.Context(), credpool.PledgeID(ownerKey, credpool.SourceOAuth, string(kind))); err != nil && !errors.Is(err, credpool.ErrNotFound) {
			s.logger.Warn("credential pool: could not withdraw %s's %s contribution on disconnect: %v (it is parked at the next acquisition)", ownerKey, kind, err)
		}
	}
	if err := s.oauthStore.Delete(r.Context(), ownerKey, kind); err != nil {
		if errors.Is(err, secrets.ErrOAuthNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	// Audited only here — the ErrOAuthNotFound path above returns 204 without
	// deleting anything, and a "deleted" event for a no-op would be a lie.
	s.auditOAuthByOwner(r, ownerKey, "deleted", kind, nil)
	w.WriteHeader(http.StatusNoContent)
}
