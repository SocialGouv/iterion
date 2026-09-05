package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/brand"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// forgeAvatarReq is the body of POST /api/teams/{id}/forge/connections/{conn_id}/avatar.
type forgeAvatarReq struct {
	// Variant is the mascot rendering to upload: "plain" (default — the
	// iterion-bot account's own avatar) or "circle" (the badge).
	Variant string `json:"variant,omitempty"`
	// Force applies the avatar to a PAT connection whose account the forge
	// does NOT flag as a bot — the operator vouching it is a dedicated
	// account (a Forgejo bot user, a hand-made GitLab user), not a person.
	Force bool `json:"force,omitempty"`
}

// forgeAvatarResp is what a successful apply returns.
type forgeAvatarResp struct {
	Connection *forge.Connection `json:"connection"`
	// AvatarURL is the forge's own URL for the new avatar, when it reports one.
	AvatarURL string `json:"avatar_url,omitempty"`
}

// avatarRefusal is a policy refusal the operator can act on: the HTTP status
// the endpoint answers with, and the fields the studio renders next to the
// message (where to upload by hand, whether a forced retry is allowed).
type avatarRefusal struct {
	status int
	msg    string
	fields map[string]any
}

func (r *avatarRefusal) Error() string { return r.msg }

// brandLogoPath is the public route serving a mascot variant, for the uploads
// an operator has to do by hand.
func brandLogoPath(v brand.Variant) string { return "/brand/" + v.Filename() }

// avatarApplyTimeout bounds one apply's forge round-trips (a WhoAmI for a
// connection older than AccountKind, then the upload). The apply rides its
// own context, detached from the caller's: the connection exists, so a caller
// that gives up must not leave its avatar state half-written, and a hanging
// forge must not hold a connect or a click for longer than this. A variable
// so a test can shorten it against a forge that hangs.
var avatarApplyTimeout = 20 * time.Second

// avatarRecordTimeout bounds one write of the outcome onto the connection —
// on a budget of its OWN: the failure worth recording is a slow forge, i.e.
// exactly when the round-trips' deadline has expired.
const avatarRecordTimeout = 10 * time.Second

// applyBotAvatar uploads the iterion-bot avatar onto the account behind conn
// and records the outcome on the connection. The policy is the point:
//   - an OAuth connection is a person's authorization → refused, no override;
//   - GitHub has no avatar/logo API → refused, pointing at the App's settings
//     page where the logo is uploaded by hand;
//   - a PAT connection is applied when the forge flags the account as a bot
//     (GitLab group/project tokens, service accounts), or when the operator
//     forces it for a dedicated account the forge cannot flag (Forgejo).
//
// A connection older than AccountKind learns it here, from the forge, so the
// bot gate judges the account and not the field's absence — the kind is
// recorded either way. A refusal comes back as *avatarRefusal and persists
// nothing else. A forge-side failure is persisted on AvatarError — so the card
// can name it and offer a retry — and returned as-is.
func (s *Server) applyBotAvatar(parent context.Context, conn forge.Connection, variant brand.Variant, force bool) (forge.Connection, string, error) {
	switch {
	case conn.Kind == forge.KindOAuthApp:
		return conn, "", &avatarRefusal{status: http.StatusUnprocessableEntity,
			msg: fmt.Sprintf("connection %s authenticates as the person who authorized it (@%s) — iterion never rebrands a personal account; connect a dedicated bot account (a group/project access token) instead", conn.ID, conn.AccountLogin)}
	case conn.Provider == forge.ProviderGitHub:
		fields := map[string]any{
			"logo_url":        brandLogoPath(brand.VariantPlain),
			"logo_circle_url": brandLogoPath(brand.VariantCircle),
		}
		msg := "GitHub exposes no API for an account's avatar or an App's logo"
		if conn.Kind == forge.KindGitHubApp {
			if u := s.githubAppLogoUploadURL(parent, conn); u != "" {
				fields["manage_url"] = u
			}
			msg += " — upload the logo on the App's settings page (Display information)"
		} else {
			msg += " — set it on the account's profile page"
		}
		return conn, "", &avatarRefusal{status: http.StatusUnprocessableEntity, msg: msg, fields: fields}
	case conn.Kind != forge.KindPAT:
		return conn, "", &avatarRefusal{status: http.StatusUnprocessableEntity,
			msg: fmt.Sprintf("connection kind %q cannot carry an avatar", conn.Kind)}
	case conn.Status == forge.StatusRevoked:
		// A dead credential would come back as a 401 recorded on AvatarError,
		// dressing a reconnect problem as an avatar one.
		return conn, "", &avatarRefusal{status: http.StatusUnprocessableEntity,
			msg: fmt.Sprintf("%s rejected this connection's token (status %s) — reconnect it first", conn.Host(), conn.Status)}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), avatarApplyTimeout)
	defer cancel()
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return conn, "", err
	}
	setter, ok := admin.(forge.AvatarSetter)
	if !ok {
		return conn, "", &avatarRefusal{status: http.StatusUnprocessableEntity,
			msg: fmt.Sprintf("%s connections cannot set an avatar through iterion", conn.Provider)}
	}
	// The upload is a forge round-trip, and another writer (the orchestrator
	// stamping ManagedSecretID on a provision) may touch the document
	// meanwhile — so every outcome is written onto a FRESH read, on a record
	// budget independent of the round-trips' deadline, carrying only what this
	// apply learned: the account kind, and — when asked — the avatar fields.
	learned := ""
	record := func(mutate func(*forge.Connection)) (forge.Connection, error) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(parent), avatarRecordTimeout)
		defer cancel()
		rctx = store.WithTenant(rctx, conn.TenantID)
		fresh, err := s.forgeConnections.Get(rctx, conn.ID)
		if err != nil {
			return conn, err
		}
		if learned != "" && fresh.AccountKind == "" {
			fresh.AccountKind = learned
		}
		mutate(&fresh)
		fresh.UpdatedAt = time.Now().UTC()
		if err := s.forgeConnections.Update(rctx, fresh); err != nil {
			return conn, err
		}
		return fresh, nil
	}
	persist := func(appliedAt *time.Time, avatarErr string) (forge.Connection, error) {
		return record(func(c *forge.Connection) { c.AvatarAppliedAt, c.AvatarError = appliedAt, avatarErr })
	}
	if conn.AccountKind == "" {
		// Older than the field: ask the forge who the token is, and remember.
		// A forced apply is the operator vouching for the account — it does
		// not hang on /user being readable.
		ident, err := admin.WhoAmI(ctx)
		switch {
		case err == nil:
			conn.AccountKind, learned = ident.Kind, ident.Kind
		case !force:
			return conn, "", fmt.Errorf("could not read the account behind connection %s on %s: %w", conn.ID, conn.Host(), err)
		}
	}
	if conn.AccountKind != forge.AccountKindBot && !force {
		if learned != "" {
			// Remember the kind only; the avatar fields are the fresh document's.
			if updated, err := record(func(*forge.Connection) {}); err == nil {
				conn = updated
			}
		}
		return conn, "", &avatarRefusal{status: http.StatusConflict,
			msg:    fmt.Sprintf("%s does not flag @%s as a bot account; if it is a dedicated account for iterion (not a person's), apply with force", conn.Host(), conn.AccountLogin),
			fields: map[string]any{"needs_force": true, "account_login": conn.AccountLogin}}
	}
	avatarURL, err := setter.SetAvatar(ctx, brand.BotAvatar(variant))
	if err != nil {
		recorded, perr := persist(conn.AvatarAppliedAt, err.Error())
		if perr != nil && s.logger != nil {
			s.logger.Error("forge avatar: record the failure on connection %s: %v", conn.ID, perr)
		}
		return recorded, "", err
	}
	now := time.Now().UTC()
	recorded, err := persist(&now, "")
	if err != nil {
		return conn, avatarURL, fmt.Errorf("avatar uploaded but could not be recorded on connection %s: %w", conn.ID, err)
	}
	return recorded, avatarURL, nil
}

// githubAppLogoUploadURL resolves the settings page of the App behind a
// github_app connection, when that App is one iterion created (its record
// carries the manage URL). Empty otherwise — a guessed link that 404s on
// GitHub is worse than none.
func (s *Server) githubAppLogoUploadURL(ctx context.Context, conn forge.Connection) string {
	if s.forgeOAuthApps == nil || conn.OAuthAppID == "" {
		return ""
	}
	app, err := s.forgeOAuthApps.Get(ctx, conn.OAuthAppID)
	if err != nil || app.TenantID != conn.TenantID {
		return ""
	}
	return app.DeriveLogoUploadURL()
}

// handleForgeConnectionAvatar is the explicit apply action: the operator asks
// for the iterion-bot avatar on one connection's account (the auto path at
// connect time only covers bot identities).
func (s *Server) handleForgeConnectionAvatar(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	if s.forgeConnections == nil {
		httpError(w, http.StatusNotFound, "forge integrations disabled")
		return
	}
	conn, ok := s.forgeConnForTenant(w, r, teamID, r.PathValue("conn_id"))
	if !ok {
		return
	}
	var req forgeAvatarReq // body optional: the plain variant, no force
	if err := decodeJSONOptional(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	variant, err := brand.ParseVariant(req.Variant)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	updated, avatarURL, err := s.applyBotAvatar(r.Context(), conn, variant, req.Force)
	var refusal *avatarRefusal
	if errors.As(err, &refusal) {
		body := map[string]any{"error": refusal.msg}
		for k, v := range refusal.fields {
			body[k] = v
		}
		writeJSONStatus(w, refusal.status, body)
		return
	}
	if err != nil {
		httpError(w, http.StatusBadGateway, "%v", err)
		return
	}
	s.auditTenant(r, teamID, "forge.connection.avatar_applied", "forge_connection", conn.ID,
		map[string]any{"provider": conn.Provider, "variant": string(variant), "forced": req.Force})
	updated.SealedPayload = nil // never serialise
	writeJSON(w, forgeAvatarResp{Connection: &updated, AvatarURL: avatarURL})
}
