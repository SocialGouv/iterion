package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// LoginWithExternal completes an OIDC/OAuth flow. It either:
//   - finds an existing user via OIDCLink → logs them in,
//   - finds a user by email → links the new identity to them,
//   - in SignupOpen mode, creates a fresh user (+ personal team),
//   - in SignupInviteOnly mode, returns ErrSignupClosed unless the
//     user was already provisioned.
func (s *Service) LoginWithExternal(ctx context.Context, ext oidc.ExternalUser, userAgent, ip string) (LoginResult, error) {
	if ext.Subject == "" {
		return LoginResult{}, fmt.Errorf("auth: external user missing subject")
	}
	if ext.Email == "" {
		return LoginResult{}, oidc.ErrEmailMissing
	}
	now := s.now().UTC()

	// GitHub team-gating context — a no-op for non-github providers or when no
	// github allow-list is configured on this deployment.
	gh, err := s.githubGateContext(ctx, ext)
	if err != nil {
		return LoginResult{}, err
	}

	link, err := s.store.GetOIDCLink(ctx, ext.Provider, ext.Subject)
	if err == nil {
		u, err := s.store.GetUser(ctx, link.UserID)
		if err != nil {
			return LoginResult{}, err
		}
		if u.Status == identity.UserStatusDisabled {
			return LoginResult{}, ErrAccountDisabled
		}
		// Returning user: re-evaluate GitHub grants — pick up newly-matched
		// orgs AND revoke github_sso memberships in orgs no longer matched
		// (left the team / allow-list disabled). Human-created memberships are
		// untouched (see reconcileGitHubGrants).
		granted, err := s.applyGitHubGrants(ctx, u.ID, gh, ext, now)
		if err != nil {
			return LoginResult{}, err
		}
		if gh.provider {
			matched := make(map[string]struct{}, len(granted))
			for _, t := range granted {
				matched[t] = struct{}{}
			}
			if err := s.reconcileGitHubGrants(ctx, u.ID, matched, now); err != nil {
				return LoginResult{}, err
			}
		}
		u.LastLoginAt = &now
		// Best-effort: LastLoginAt is cosmetic; a store outage surfaces at
		// the session write on the next line.
		_ = s.store.UpdateUser(ctx, u)
		return s.issueLogin(ctx, u, userAgent, ip)
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return LoginResult{}, err
	}

	email := identity.NormalizeEmail(ext.Email)
	u, err := s.store.GetUserByEmail(ctx, email)
	if err == nil {
		if u.Status == identity.UserStatusDisabled {
			return LoginResult{}, ErrAccountDisabled
		}
		// Auto-link the new external identity onto the existing user
		// only when the operator has explicitly trusted this OIDC
		// provider's email verification. Otherwise return
		// ErrLinkRequiresConsent so the UI prompts the user to link
		// the connection from settings — the previous code
		// auto-linked unconditionally, which let any IdP that happened
		// to claim a registered user's email take over the account
		// (only mitigated by hoping every IdP rigorously verifies
		// email, which is not always the case for self-hosted /
		// emerging providers).
		if _, ok := s.trustedAutoLinkProviders[ext.Provider]; !ok {
			return LoginResult{}, ErrLinkRequiresConsent
		}
		if err := s.store.UpsertOIDCLink(ctx, identity.OIDCLink{
			Provider:       ext.Provider,
			ProviderUserID: ext.Subject,
			UserID:         u.ID,
			Email:          email,
			CreatedAt:      now,
		}); err != nil {
			return LoginResult{}, err
		}
		if _, err := s.applyGitHubGrants(ctx, u.ID, gh, ext, now); err != nil {
			return LoginResult{}, err
		}
		u.LastLoginAt = &now
		// Best-effort: LastLoginAt is cosmetic; a store outage surfaces at
		// the session write on the next line.
		_ = s.store.UpdateUser(ctx, u)
		return s.issueLogin(ctx, u, userAgent, ip)
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return LoginResult{}, err
	}

	// New user via SSO. When GitHub team-gating is active, a new GitHub user is
	// admitted ONLY if their teams matched an allow-list — and then bypasses
	// SignupMode (the org admin allow-listed them). A non-matching new GitHub
	// user is, by policy, either refused (default) or admitted as a teamless
	// submitter (marketplace-submit only) — see GitHubUngrantedPolicy.
	gated := gh.provider && gh.active
	if gated && len(gh.rows) == 0 {
		if s.githubUngrantedPolicy == GitHubUngrantedSubmitter {
			return s.provisionSubmitter(ctx, ext, email, now, userAgent, ip)
		}
		return LoginResult{}, ErrSSORestricted
	}
	if !gated && s.signupMode != SignupOpen {
		return LoginResult{}, ErrSignupClosed
	}
	u = identity.User{
		ID:        uuid.NewString(),
		Email:     email,
		Name:      ext.Name,
		Status:    identity.UserStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	u, err = s.store.CreateUser(ctx, u)
	if err != nil {
		return LoginResult{}, err
	}
	if err := s.store.UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider:       ext.Provider,
		ProviderUserID: ext.Subject,
		UserID:         u.ID,
		Email:          email,
		CreatedAt:      now,
	}); err != nil {
		return LoginResult{}, err
	}
	// GitHub-gated users join the allow-listed org(s) and land there (no
	// personal team); every other SSO signup gets a personal team as before.
	granted, err := s.applyGitHubGrants(ctx, u.ID, gh, ext, now)
	if err != nil {
		return LoginResult{}, err
	}
	preferredTeam := ""
	if len(granted) > 0 {
		preferredTeam = granted[0]
		u.DefaultTeamID = granted[0]
		if orgID, derr := s.resolveOrgIDForTeam(ctx, granted[0]); derr == nil && orgID != "" {
			u.DefaultOrgID = orgID
		}
	} else {
		orgID, teamID, terr := s.createPersonalTeam(ctx, u)
		if terr != nil {
			return LoginResult{}, terr
		}
		u.DefaultOrgID = orgID
		u.DefaultTeamID = teamID
	}
	u.LastLoginAt = &now
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	return s.issueLoginInTeam(ctx, u, preferredTeam, userAgent, ip)
}

// provisionSubmitter creates a teamless account for a GitHub login that is
// admitted but matched no allow-listed team (GitHubUngrantedPolicy=submitter).
// Unlike the normal new-user path it does NOT create a personal team, so the
// account can authenticate + submit to the marketplace (auth-only) but has no
// team-scoped rights. If the user is later added to an allow-listed team, the
// returning-user grant re-evaluation in LoginWithExternal upgrades them.
func (s *Service) provisionSubmitter(ctx context.Context, ext oidc.ExternalUser, email string, now time.Time, userAgent, ip string) (LoginResult, error) {
	u := identity.User{
		ID:        uuid.NewString(),
		Email:     email,
		Name:      ext.Name,
		Status:    identity.UserStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	u, err := s.store.CreateUser(ctx, u)
	if err != nil {
		return LoginResult{}, err
	}
	if err := s.store.UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider:       ext.Provider,
		ProviderUserID: ext.Subject,
		UserID:         u.ID,
		Email:          email,
		CreatedAt:      now,
	}); err != nil {
		return LoginResult{}, err
	}
	u.LastLoginAt = &now
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	// No team: issueLogin issues a session with an empty active team.
	return s.issueLogin(ctx, u, userAgent, ip)
}

// LoginWithExternalForOrg completes a per-org OIDC flow (a tenant's own
// Keycloak, slug "oidc-org-<providerID>"). The org admin has explicitly
// designated this IdP as the org's trust root, so a NEW email is onboarded
// into the org regardless of the global SignupMode — but an SSO identity is
// NEVER auto-linked onto a pre-existing iterion account by email (that needs
// JWKS ID-token verification + explicit consent, a Phase-3 hardening; until
// then → ErrLinkRequiresConsent). The resolved user is granted membership in
// tenantID at the provider's DefaultRole (grant-only, never downgraded, capped
// below owner) and lands in that org.
func (s *Service) LoginWithExternalForOrg(ctx context.Context, ext oidc.ExternalUser, tenantID, providerID, userAgent, ip string) (LoginResult, error) {
	if s.orgSSO == nil {
		return LoginResult{}, oidc.ErrUnknownProvider
	}
	if ext.Subject == "" {
		return LoginResult{}, fmt.Errorf("auth: external user missing subject")
	}
	if ext.Email == "" {
		return LoginResult{}, oidc.ErrEmailMissing
	}
	row, err := s.orgSSO.Get(ctx, providerID)
	if err != nil {
		return LoginResult{}, oidc.ErrUnknownProvider
	}
	// Defence in depth: the provider must belong to the claimed tenant and be
	// an enabled OIDC row. The resolver already checked, but the login path
	// must not trust the slug→tenant mapping blindly.
	if row.TenantID != tenantID || row.Kind != orgsso.KindOIDC || !row.Enabled {
		return LoginResult{}, oidc.ErrUnknownProvider
	}
	role := row.DefaultRole
	if !role.Valid() || role == identity.RoleOwner {
		role = identity.RoleMember
	}
	now := s.now().UTC()

	// 1) Returning user — existing link for this org's provider slug.
	link, err := s.store.GetOIDCLink(ctx, ext.Provider, ext.Subject)
	if err == nil {
		u, err := s.store.GetUser(ctx, link.UserID)
		if err != nil {
			return LoginResult{}, err
		}
		if u.Status == identity.UserStatusDisabled {
			return LoginResult{}, ErrAccountDisabled
		}
		if err := s.grantMembership(ctx, u.ID, tenantID, role, identity.MembershipSourceOIDCSSO, now); err != nil {
			return LoginResult{}, err
		}
		u.LastLoginAt = &now
		// Best-effort: LastLoginAt is cosmetic; a store outage surfaces at
		// the session write on the next line.
		_ = s.store.UpdateUser(ctx, u)
		return s.issueLoginInTeam(ctx, u, tenantID, userAgent, ip)
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return LoginResult{}, err
	}

	// 2) Existing iterion account by email. Auto-link only when the org opted
	// in AND the email's domain is verified for the org AND the target isn't a
	// privileged account elsewhere (see canAutoLink); otherwise require explicit
	// consent (409).
	email := identity.NormalizeEmail(ext.Email)
	existing, err := s.store.GetUserByEmail(ctx, email)
	if err == nil {
		if existing.Status == identity.UserStatusDisabled {
			return LoginResult{}, ErrAccountDisabled
		}
		if !s.canAutoLink(ctx, row, existing, email) {
			return LoginResult{}, ErrLinkRequiresConsent
		}
		if err := s.store.UpsertOIDCLink(ctx, identity.OIDCLink{
			Provider:       ext.Provider,
			ProviderUserID: ext.Subject,
			UserID:         existing.ID,
			Email:          email,
			CreatedAt:      now,
		}); err != nil {
			return LoginResult{}, err
		}
		if err := s.grantMembership(ctx, existing.ID, tenantID, role, identity.MembershipSourceOIDCSSO, now); err != nil {
			return LoginResult{}, err
		}
		existing.LastLoginAt = &now
		// Best-effort: LastLoginAt is cosmetic; a store outage surfaces at
		// the session write on the next line.
		_ = s.store.UpdateUser(ctx, existing)
		return s.issueLoginInTeam(ctx, existing, tenantID, userAgent, ip)
	} else if !errors.Is(err, identity.ErrNotFound) {
		return LoginResult{}, err
	}

	// 3) New user — onboard into the org (the admin opted this IdP in). No
	// personal team: the user belongs to the configuring org.
	u := identity.User{
		ID:            uuid.NewString(),
		Email:         email,
		Name:          ext.Name,
		Status:        identity.UserStatusActive,
		CreatedAt:     now,
		UpdatedAt:     now,
		DefaultTeamID: tenantID,
	}
	u, err = s.store.CreateUser(ctx, u)
	if err != nil {
		return LoginResult{}, err
	}
	if err := s.store.UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider:       ext.Provider,
		ProviderUserID: ext.Subject,
		UserID:         u.ID,
		Email:          email,
		CreatedAt:      now,
	}); err != nil {
		return LoginResult{}, err
	}
	if err := s.grantMembership(ctx, u.ID, tenantID, role, identity.MembershipSourceOIDCSSO, now); err != nil {
		return LoginResult{}, err
	}
	u.LastLoginAt = &now
	// Best-effort: LastLoginAt is cosmetic; a store outage surfaces at
	// the session write on the next line.
	_ = s.store.UpdateUser(ctx, u)
	return s.issueLoginInTeam(ctx, u, tenantID, userAgent, ip)
}

// LinkExternalToUser attaches a freshly-authenticated external identity to an
// already-signed-in user (the explicit-consent path that resolves the 409
// ErrLinkRequiresConsent dead-end: log in with your password, then connect SSO
// from settings). Idempotent if the identity is already this user's; refuses
// with ErrLinkAlreadyOwned if it belongs to a different account so an SSO
// identity can never be silently re-pointed.
func (s *Service) LinkExternalToUser(ctx context.Context, ext oidc.ExternalUser, userID string) error {
	if ext.Subject == "" {
		return fmt.Errorf("auth: external user missing subject")
	}
	if ext.Email == "" {
		return oidc.ErrEmailMissing
	}
	if _, err := s.store.GetUser(ctx, userID); err != nil {
		return err
	}
	existing, err := s.store.GetOIDCLink(ctx, ext.Provider, ext.Subject)
	if err == nil {
		if existing.UserID != userID {
			return ErrLinkAlreadyOwned
		}
		return nil // already linked to this user — no-op
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return err
	}
	return s.store.UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider:       ext.Provider,
		ProviderUserID: ext.Subject,
		UserID:         userID,
		Email:          identity.NormalizeEmail(ext.Email),
		CreatedAt:      s.now().UTC(),
	})
}

// ListSSOLinks returns the SSO identities linked to a user, for the "connected
// accounts" settings view.
func (s *Service) ListSSOLinks(ctx context.Context, userID string) ([]identity.OIDCLink, error) {
	return s.store.ListOIDCLinksByUser(ctx, userID)
}

// UnlinkExternal removes one SSO identity from a user. Ownership is enforced:
// the link must belong to userID (a user can only detach their own identities).
func (s *Service) UnlinkExternal(ctx context.Context, userID, provider, providerUserID string) error {
	link, err := s.store.GetOIDCLink(ctx, provider, providerUserID)
	if err != nil {
		return err
	}
	if link.UserID != userID {
		return identity.ErrNotFound
	}
	return s.store.DeleteOIDCLink(ctx, provider, providerUserID)
}

// grantMembership ensures userID has at least `role` in teamID. Grant-only: an
// existing membership at an equal-or-higher role is left untouched (never
// downgrade a manually-promoted user); a lower one is upgraded.
func (s *Service) grantMembership(ctx context.Context, userID, teamID string, role identity.Role, source string, now time.Time) error {
	// Mirror the team grant up to an org membership so an SSO-granted user
	// is a first-class member of the team's parent org.
	if _, err := s.grantOrgMembershipForTeam(ctx, userID, teamID, role, now); err != nil {
		return err
	}
	existing, err := s.store.GetMembership(ctx, userID, teamID)
	if errors.Is(err, identity.ErrNotFound) {
		return s.store.UpsertMembership(ctx, identity.Membership{UserID: userID, TeamID: teamID, Role: role, Source: source, JoinedAt: now})
	}
	if err != nil {
		return err
	}
	if existing.Role.AtLeast(role) {
		return nil
	}
	// Upgrade the role but PRESERVE the existing Source: a membership a human
	// created (invitation/manual) must not become SSO-revocable just because an
	// SSO grant later matched it.
	existing.Role = role
	return s.store.UpsertMembership(ctx, existing)
}

// resolveOrgIDForTeam returns the parent org id of a team (empty if the
// team is missing or has no parent org yet).
func (s *Service) resolveOrgIDForTeam(ctx context.Context, teamID string) (string, error) {
	t, err := s.store.GetTeam(ctx, teamID)
	if err != nil {
		return "", err
	}
	return t.OrgID, nil
}

// reconcileGitHubGrants revokes the user's github_sso-sourced memberships in
// tenants they no longer match (e.g. removed from the allow-listed GitHub team,
// or the allow-list was disabled). matched is the set of tenant IDs the user
// currently matches. Only github_sso-minted memberships are touched —
// human-created ones (Source empty / invitation / manual) are never revoked.
func (s *Service) reconcileGitHubGrants(ctx context.Context, userID string, matched map[string]struct{}, now time.Time) error {
	memberships, err := s.store.ListMembershipsByUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, m := range memberships {
		if m.Source != identity.MembershipSourceGitHubSSO {
			continue
		}
		if _, ok := matched[m.TeamID]; ok {
			continue
		}
		if err := s.store.DeleteMembership(ctx, userID, m.TeamID); err != nil {
			return err
		}
	}
	return nil
}

// githubGate captures the per-login GitHub team-gating context: whether the
// flow is a GitHub login, whether any allow-list is configured deployment-wide
// (active), and the enabled rows whose allow-list this user's groups matched.
type githubGate struct {
	provider bool
	active   bool
	rows     []orgsso.OrgSSOProvider
}

// githubGateContext resolves the GitHub team-gating context for a login. It is
// a no-op (zero githubGate) for non-github providers or when the orgsso store
// is unwired. The matched rows are looked up via the multikey reverse index —
// a single positive-only query, never a cross-tenant scan.
func (s *Service) githubGateContext(ctx context.Context, ext oidc.ExternalUser) (githubGate, error) {
	var g githubGate
	if ext.Provider != "github" || s.orgSSO == nil {
		return g, nil
	}
	g.provider = true
	active, err := s.orgSSO.GitHubGatingActive(ctx)
	if err != nil {
		return githubGate{}, err
	}
	g.active = active
	if active && len(ext.Groups) > 0 {
		rows, err := s.orgSSO.FindGitHubGrantingOrgs(ctx, ext.Groups)
		if err != nil {
			return githubGate{}, err
		}
		g.rows = rows
	}
	return g, nil
}

// applyGitHubGrants upserts a membership for userID in each matched GitHub
// row's tenant at the row's (capped-at-member) granted role. Grant-only — it
// never downgrades a manually-set higher role, and never revokes. Returns the
// tenant IDs granted, for active-team selection.
func (s *Service) applyGitHubGrants(ctx context.Context, userID string, gh githubGate, ext oidc.ExternalUser, now time.Time) ([]string, error) {
	if !gh.provider || len(gh.rows) == 0 {
		return nil, nil
	}
	granted := make([]string, 0, len(gh.rows))
	for _, row := range gh.rows {
		role, ok := row.RoleForGroups(ext.Groups)
		if !ok {
			continue
		}
		if err := s.grantMembership(ctx, userID, row.TenantID, role, identity.MembershipSourceGitHubSSO, now); err != nil {
			return granted, err
		}
		granted = append(granted, row.TenantID)
	}
	return granted, nil
}

// canAutoLink reports whether a fresh per-org OIDC identity may be auto-linked
// onto an existing user matched by email — only when the org opted in
// (row.AutoLinkOnEmail), the email's domain is VERIFIED for the org (so the
// org's IdP has authority over that address — JWKS alone is insufficient since
// the org controls its own signing keys), and the target is not a privileged
// account whose takeover would escalate beyond the org: never a super-admin,
// and never an admin/owner of a non-personal org other than the linking one.
func (s *Service) canAutoLink(ctx context.Context, row orgsso.OrgSSOProvider, target identity.User, email string) bool {
	if !row.AutoLinkOnEmail || s.domains == nil || target.IsSuperAdmin {
		return false
	}
	ok, err := s.domains.IsVerifiedForTenant(ctx, row.TenantID, orgsso.EmailDomain(email))
	if err != nil || !ok {
		return false
	}
	ms, err := s.store.ListMembershipsByUser(ctx, target.ID)
	if err != nil {
		return false
	}
	for _, m := range ms {
		if m.TeamID == row.TenantID || !m.Role.AtLeast(identity.RoleAdmin) {
			continue
		}
		// admin/owner of another team — allowed only if it's a personal team
		// (every user owns theirs; that confers no cross-tenant power).
		if t, terr := s.store.GetTeam(ctx, m.TeamID); terr == nil && t.Personal {
			continue
		}
		return false
	}
	return true
}
