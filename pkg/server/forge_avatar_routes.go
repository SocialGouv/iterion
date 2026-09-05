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

// applyBotAvatar uploads the iterion-bot avatar onto the account behind conn
// and records the outcome on the connection. The policy is the point:
//   - an OAuth connection is a person's authorization → refused, no override;
//   - GitHub has no avatar/logo API → refused, pointing at the App's settings
//     page where the logo is uploaded by hand;
//   - a PAT connection is applied when the forge flags the account as a bot
//     (GitLab group/project tokens, service accounts), or when the operator
//     forces it for a dedicated account the forge cannot flag (Forgejo).
//
// A refusal comes back as *avatarRefusal and persists nothing. A forge-side
// failure is persisted on AvatarError — so the card can name it and offer a
// retry — and returned as-is.
func (s *Server) applyBotAvatar(ctx context.Context, conn forge.Connection, variant brand.Variant, force bool) (forge.Connection, string, error) {
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
			if u := s.githubAppLogoUploadURL(ctx, conn); u != "" {
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
	case conn.AccountKind != forge.AccountKindBot && !force:
		return conn, "", &avatarRefusal{status: http.StatusConflict,
			msg:    fmt.Sprintf("%s does not flag @%s as a bot account; if it is a dedicated account for iterion (not a person's), apply with force", conn.Host(), conn.AccountLogin),
			fields: map[string]any{"needs_force": true, "account_login": conn.AccountLogin}}
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return conn, "", err
	}
	setter, ok := admin.(forge.AvatarSetter)
	if !ok {
		return conn, "", &avatarRefusal{status: http.StatusUnprocessableEntity,
			msg: fmt.Sprintf("%s connections cannot set an avatar through iterion", conn.Provider)}
	}
	now := time.Now().UTC()
	tctx := store.WithTenant(ctx, conn.TenantID)
	avatarURL, err := setter.SetAvatar(ctx, brand.BotAvatar(variant))
	if err != nil {
		conn.AvatarError = err.Error()
		conn.UpdatedAt = now
		if uerr := s.forgeConnections.Update(tctx, conn); uerr != nil && s.logger != nil {
			s.logger.Error("forge avatar: record the failure on connection %s: %v", conn.ID, uerr)
		}
		return conn, "", err
	}
	conn.AvatarAppliedAt = &now
	conn.AvatarError = ""
	conn.UpdatedAt = now
	if err := s.forgeConnections.Update(tctx, conn); err != nil {
		return conn, avatarURL, fmt.Errorf("avatar uploaded but could not be recorded on connection %s: %w", conn.ID, err)
	}
	return conn, avatarURL, nil
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
	if err != nil {
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
