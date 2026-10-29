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
		if refusal := ssoRefusal(u); refusal != nil {
			return LoginResult{}, refusal
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
		// The stamp is also this path's commit to the status checked above: a
		// deactivation committed since refuses the write, and the login with
		// it. Any other store error leaves LastLoginAt — cosmetic — unwritten
		// and surfaces at the session write on the next line.
		u, err = s.stampLogin(ctx, u, now, ssoStamp())
		if errors.Is(err, ErrAccountDisabled) {
			return LoginResult{}, err
		}
		return s.issueLogin(ctx, u, userAgent, ip)
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return LoginResult{}, err
	}

	email := identity.NormalizeEmail(ext.Email)
	u, err := s.store.GetUserByEmail(ctx, email)
	if err == nil {
		if refusal := ssoRefusal(u); refusal != nil {
			return LoginResult{}, refusal
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
		// The stamp commits to the status checked above — see the returning
		// -user path.
		u, err = s.stampLogin(ctx, u, now, ssoStamp())
		if errors.Is(err, ErrAccountDisabled) {
			return LoginResult{}, err
		}
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
	// Every read the grants need runs BEFORE the account is written, as
	// registerWithInvitation does: a read that fails then leaves no account,
	// no SSO link and no grant behind, and the user retries into the same
	// first login instead of into a half-provisioned one. The id is minted
	// above, so the plan can name the rows it will write.
	plans, err := s.planNewAccountGrants(ctx, u.ID, gh, ext, now)
	if err != nil {
		return LoginResult{}, err
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
	granted, err := s.applyNewAccountGrants(ctx, plans, identity.MembershipSourceGitHubSSO, now)
	if err != nil {
		return LoginResult{}, err
	}
	// The landing defaults this signup resolved, plus the stamp. No
	// precondition: the account was created by this very call, so there is
	// no earlier status for a deactivation to have replaced.
	patch := identity.UserPatch{LastLoginAt: &now}
	preferredTeam := ""
	if len(granted) > 0 {
		preferredTeam = granted[0]
		patch.DefaultTeamID = &granted[0]
		// The default org comes from the grant's OWN team read — no second
		// read of a row this call already resolved, and no window between
		// the two for the team to change under.
		if orgID := plans[0].org.orgID; orgID != "" {
			patch.DefaultOrgID = &orgID
		}
	} else {
		space, terr := s.createPersonalTeam(ctx, u)
		if terr != nil {
			return LoginResult{}, terr
		}
		patch.DefaultOrgID = &space.orgID
		patch.DefaultTeamID = &space.teamID
	}
	if u, err = s.store.PatchUser(ctx, u.ID, patch); err != nil {
		return LoginResult{}, err
	}
	return s.issueNewAccountLogin(ctx, u, preferredTeam, userAgent, ip)
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
	// The stamp alone, on the account this call just created.
	if u, err = s.store.PatchUser(ctx, u.ID, identity.UserPatch{LastLoginAt: &now}); err != nil {
		return LoginResult{}, err
	}
	// No team: the login lands with an empty active team.
	return s.issueNewAccountLogin(ctx, u, "", userAgent, ip)
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
	if errors.Is(err, orgsso.ErrNotFound) {
		return LoginResult{}, oidc.ErrUnknownProvider
	}
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: read SSO provider %s: %w", providerID, err)
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
		if refusal := ssoRefusal(u); refusal != nil {
			return LoginResult{}, refusal
		}
		if err := s.grantMembership(ctx, u.ID, tenantID, role, identity.MembershipSourceOIDCSSO, now); err != nil {
			return LoginResult{}, err
		}
		// The stamp commits to the status checked above — see
		// LoginWithExternal's returning-user path.
		u, err = s.stampLogin(ctx, u, now, ssoStamp())
		if errors.Is(err, ErrAccountDisabled) {
			return LoginResult{}, err
		}
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
		if refusal := ssoRefusal(existing); refusal != nil {
			return LoginResult{}, refusal
		}
		allowed, linkErr := s.canAutoLink(ctx, row, existing, email)
		if linkErr != nil {
			// A read that could not decide is not the policy's refusal: 409
			// link_required is permanent for an account with no password,
			// and its cause used to vanish unlogged.
			return LoginResult{}, linkErr
		}
		if !allowed {
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
		// The stamp commits to the status checked above — see
		// LoginWithExternal's returning-user path.
		existing, err = s.stampLogin(ctx, existing, now, ssoStamp())
		if errors.Is(err, ErrAccountDisabled) {
			return LoginResult{}, err
		}
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
	// The grant's read runs before the account exists — see
	// LoginWithExternal's new-user path: a failure here commits nothing.
	grant, err := s.planOrgGrantForNewAccount(ctx, u.ID, tenantID, role, now)
	if err != nil {
		return LoginResult{}, err
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
	if _, err := s.applyNewAccountGrants(ctx, []newAccountGrant{
		{userID: u.ID, teamID: tenantID, role: role, org: grant},
	}, identity.MembershipSourceOIDCSSO, now); err != nil {
		return LoginResult{}, err
	}
	// The stamp alone, on the account this call just created. Best-effort:
	// LastLoginAt is cosmetic; a store outage surfaces at the session write
	// on the next line.
	if patched, uerr := s.store.PatchUser(ctx, u.ID, identity.UserPatch{LastLoginAt: &now}); uerr == nil {
		u = patched
	}
	return s.issueNewAccountLogin(ctx, u, tenantID, userAgent, ip)
}

// LinkExternalToUser attaches a freshly-authenticated external identity to an
// already-signed-in user (the explicit-consent path that resolves the 409
// ErrLinkRequiresConsent dead-end: log in with your password, then connect SSO
// from settings). Idempotent if the identity is already this user's; refuses
// with ErrLinkAlreadyOwned if it belongs to a different account so an SSO
// identity can never be silently re-pointed.
//
// sessionLineage names the signed-in session that links it: the link records
// it (OIDCLink.SessionLineage), so "sign out everywhere", an admin disable and
// a reuse revocation of that lineage remove it (RemoveSessionSSOLinks). created
// reports whether this call wrote the link — false when the identity was
// already this account's.
func (s *Service) LinkExternalToUser(ctx context.Context, ext oidc.ExternalUser, userID, sessionLineage string) (created bool, err error) {
	if ext.Subject == "" {
		return false, fmt.Errorf("auth: external user missing subject")
	}
	if ext.Email == "" {
		return false, oidc.ErrEmailMissing
	}
	if _, err := s.store.GetUser(ctx, userID); err != nil {
		return false, err
	}
	existing, err := s.store.GetOIDCLink(ctx, ext.Provider, ext.Subject)
	if err == nil {
		if existing.UserID != userID {
			return false, ErrLinkAlreadyOwned
		}
		return false, nil // already linked to this user — no-op
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return false, err
	}
	if err := s.store.UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider:       ext.Provider,
		ProviderUserID: ext.Subject,
		UserID:         userID,
		Email:          identity.NormalizeEmail(ext.Email),
		CreatedAt:      s.now().UTC(),
		SessionLineage: sessionLineage,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// ErrLastSignInMethod refuses the removal of an account's last sign-in
// method: an account with no password and no other SSO identity would have
// no way back in.
var ErrLastSignInMethod = errors.New("auth: this is the account's last sign-in method — connect another provider or set a password first")

// RemoveSessionSSOLinks removes every SSO link of userID a signed-in session
// wrote from the account settings ("sign out everywhere", an admin disable): a
// link an SSO sign-in wrote itself — the account's sign-up through the IdP, its
// first SSO sign-in — stays, so an account that signs in through SSO only keeps
// its sign-in. It answers how many it removed; a failure is returned.
func (s *Service) RemoveSessionSSOLinks(ctx context.Context, userID string) (int64, error) {
	n, err := s.store.DeleteSessionOIDCLinks(ctx, userID, "")
	if err != nil {
		return 0, fmt.Errorf("auth: remove the SSO links the sessions of user %s wrote: %w", userID, err)
	}
	return n, nil
}

// ListSSOLinks returns the SSO identities linked to a user, for the "connected
// accounts" settings view.
func (s *Service) ListSSOLinks(ctx context.Context, userID string) ([]identity.OIDCLink, error) {
	return s.store.ListOIDCLinksByUser(ctx, userID)
}

// UnlinkExternal removes one SSO identity from a user. Ownership is enforced:
// the link must belong to userID (a user can only detach their own
// identities). An account with no password and this one identity keeps its
// sign-in method: the caller connects another provider or sets a password
// first (ErrLastSignInMethod).
func (s *Service) UnlinkExternal(ctx context.Context, userID, provider, providerUserID string) error {
	link, err := s.store.GetOIDCLink(ctx, provider, providerUserID)
	if err != nil {
		return err
	}
	if link.UserID != userID {
		return identity.ErrNotFound
	}
	if u, err := s.store.GetUser(ctx, userID); err == nil && u.PasswordHash == "" {
		links, lerr := s.store.ListOIDCLinksByUser(ctx, userID)
		if lerr != nil {
			return lerr
		}
		if len(links) <= 1 {
			return ErrLastSignInMethod
		}
	} else if err != nil {
		return err
	}
	return s.store.DeleteOIDCLink(ctx, provider, providerUserID)
}

// RemoveFlowSSOLink removes an SSO identity link only when it is the row the
// CALLING FLOW wrote: same account, the flow's session lineage on the row.
// The link callback's undo runs it — a session refused once the link was
// written, or a write whose answer was lost — so the row the flow wrote
// never survives while a row anyone else wrote (another lineage, the
// account's sign-up link) is not ours to remove. The last-sign-in-method
// guard does not apply here: what is undone is the flow's own write, not a
// sign-in method the account chose to keep.
func (s *Service) RemoveFlowSSOLink(ctx context.Context, userID, provider, subject, lineage string) error {
	link, err := s.store.GetOIDCLink(ctx, provider, subject)
	if errors.Is(err, identity.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if link.UserID != userID || lineage == "" || link.SessionLineage != lineage {
		return nil
	}
	return s.store.DeleteOIDCLink(ctx, provider, subject)
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

// newAccountGrant is one team grant an SSO login writes for an account the
// same call creates: the team, the role, and the org membership the grant is
// mirrored up to — all decided from reads taken before the account row.
type newAccountGrant struct {
	userID string
	teamID string
	role   identity.Role
	org    orgGrant
}

// planNewAccountGrants decides every grant the matched GitHub rows take for
// an account this call is about to create. Reads only: nothing is written
// yet, so a read that fails leaves the store as it was.
func (s *Service) planNewAccountGrants(ctx context.Context, userID string, gh githubGate, ext oidc.ExternalUser, now time.Time) ([]newAccountGrant, error) {
	if !gh.provider || len(gh.rows) == 0 {
		return nil, nil
	}
	plans := make([]newAccountGrant, 0, len(gh.rows))
	for _, row := range gh.rows {
		role, ok := row.RoleForGroups(ext.Groups)
		if !ok {
			continue
		}
		grant, err := s.planOrgGrantForNewAccount(ctx, userID, row.TenantID, role, now)
		if err != nil {
			return nil, err
		}
		plans = append(plans, newAccountGrant{userID: userID, teamID: row.TenantID, role: role, org: grant})
	}
	return plans, nil
}

// planOrgGrantForNewAccount is planOrgGrantForTeam for an account THIS call
// creates: the org-membership read is skipped, because a uuid minted a
// moment ago holds no membership row — the read can only answer "not found"
// or fail, and a read that can only fail is one no path should owe its
// outcome to. The team read stays: it resolves the parent org, which the
// landing defaults need too.
func (s *Service) planOrgGrantForNewAccount(ctx context.Context, userID, teamID string, teamRole identity.Role, now time.Time) (orgGrant, error) {
	t, err := s.store.GetTeam(ctx, teamID)
	if err != nil {
		return orgGrant{}, err
	}
	if t.OrgID == "" {
		return orgGrant{}, nil
	}
	want := identity.OrgRoleForTeamRole(teamRole)
	if want == "" {
		// A team capability outside the ladder (config_editor) confers no org
		// membership — see planOrgGrantForTeam. ADR-078.
		return orgGrant{orgID: t.OrgID}, nil
	}
	return orgGrant{orgID: t.OrgID, write: &identity.OrgMembership{
		UserID:   userID,
		OrgID:    t.OrgID,
		Role:     want,
		JoinedAt: now,
	}}, nil
}

// applyNewAccountGrants writes the planned grants, org membership first —
// the order registerWithInvitation states: a grant left standing by a
// failure between the two is live access to a team of an org its holder is
// not a member of. It reads nothing: the account is this call's own, so
// there is no role to preserve and no grant to keep.
func (s *Service) applyNewAccountGrants(ctx context.Context, plans []newAccountGrant, source string, now time.Time) ([]string, error) {
	granted := make([]string, 0, len(plans))
	for _, p := range plans {
		if err := s.applyOrgGrant(ctx, p.org); err != nil {
			return granted, err
		}
		if err := s.store.UpsertMembership(ctx, identity.Membership{
			UserID: p.userID, TeamID: p.teamID, Role: p.role, Source: source, JoinedAt: now,
		}); err != nil {
			return granted, err
		}
		granted = append(granted, p.teamID)
	}
	return granted, nil
}

// reconcileGitHubGrants revokes the user's github_sso-sourced memberships in
// tenants they no longer match (e.g. removed from the allow-listed GitHub team,
// or the allow-list was disabled). matched is the set of tenant IDs the user
// currently matches. Only github_sso-minted memberships are touched —
// human-created ones (Source empty / invitation / manual) are never revoked.
//
// Each revoke withdraws two premises at once. The kept half: the grant is
// the premise a mint's own confirmation keeps a self-addressed invitation on
// — a grant in the team, held by a creator with no org membership row in its
// org (pkg/server standingAfterMintTakesInvitation) — so the rows kept on
// the grant go with it, through the sweep the server registers
// (SetTeamGrantSweep): the one selection the three writes that owe it share.
// The minted half: the revoke is a LOWERING of the creator's team authority
// to no grant at all, so the rows they minted above what that leaves them —
// the cap that would bind their minting NOW refuses them — go too, through
// the reconcile the server registers (SetTeamRankReconcile). A sweep or a
// reconcile that fails fails the login, as every other store error on this
// path does: the row it could not take is a re-entry ticket into the team
// this revoke just closed.
//
// With either seam unregistered, a grant is NOT revoked: taking the grant
// and leaving the rows that rest on it — kept on it, or minted above the
// authority its loss leaves — is the state this exists to prevent, and a
// silent half-revoke is not an answer the caller can act on.
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
		if s.teamGrantSweep == nil {
			return fmt.Errorf("auth: no team-grant invitation sweep is registered, so revoking the %s grant of %s would leave the pending invitations kept on it standing (SetTeamGrantSweep)", m.TeamID, userID)
		}
		if s.teamRankReconcile == nil {
			return fmt.Errorf("auth: no team-rank reconcile is registered, so revoking the %s grant of %s would leave the invitations they minted above the authority its loss leaves standing (SetTeamRankReconcile)", m.TeamID, userID)
		}
		if err := s.store.DeleteMembership(ctx, userID, m.TeamID); err != nil {
			return err
		}
		if err := s.teamGrantSweep(ctx, userID, m.TeamID, MembershipRevokedBySSOReason); err != nil {
			return fmt.Errorf("auth: sweep the invitations kept on the %s grant of %s, revoked with the github allow-list that granted it: %w", m.TeamID, userID, err)
		}
		if err := s.teamRankReconcile(ctx, userID, m.TeamID, ""); err != nil {
			return fmt.Errorf("auth: take the invitations the %s grant of %s minted above the authority its revoke leaves: %w", m.TeamID, userID, err)
		}
	}
	return nil
}

// MembershipRevokedBySSOReason is the audit cause each invitation revoked
// with a github_sso grant carries, naming the write that withdrew the premise
// the row rested on.
const MembershipRevokedBySSOReason = "creator_team_grant_sso_revoked"

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
//
// (false, nil) is the POLICY's refusal, which the caller answers as
// ErrLinkRequiresConsent — "sign in, then link". An error is a read that
// could not decide: the caller owes it its own answer, because a 409 for a
// failed read is permanent (the account the SSO login matched has no
// password, so the "sign in, then link" remedy is a dead end) and the cause
// used to disappear with nothing logged. Policy and failure answer apart.
func (s *Service) canAutoLink(ctx context.Context, row orgsso.OrgSSOProvider, target identity.User, email string) (bool, error) {
	if !row.AutoLinkOnEmail || s.domains == nil || target.IsSuperAdmin {
		return false, nil
	}
	ok, err := s.domains.IsVerifiedForTenant(ctx, row.TenantID, orgsso.EmailDomain(email))
	if err != nil {
		return false, fmt.Errorf("auth: read whether domain %s is verified for tenant %s: %w",
			orgsso.EmailDomain(email), row.TenantID, err)
	}
	if !ok {
		return false, nil
	}
	ms, err := s.store.ListMembershipsByUser(ctx, target.ID)
	if err != nil {
		return false, fmt.Errorf("auth: read the grants of account %s the auto-link would take over: %w", target.ID, err)
	}
	for _, m := range ms {
		if m.TeamID == row.TenantID || !m.Role.AtLeast(identity.RoleAdmin) {
			continue
		}
		// admin/owner of another team — allowed only if it's a personal team
		// (every user owns theirs; that confers no cross-tenant power).
		t, terr := s.store.GetTeam(ctx, m.TeamID)
		if terr != nil {
			// The team that decides whether this grant is privileged cannot
			// be read: the refusal stands (the safe direction), and it says
			// why instead of claiming the policy refused.
			return false, fmt.Errorf("auth: read team %s to decide whether the grant of account %s is privileged: %w", m.TeamID, target.ID, terr)
		}
		if t.Personal {
			continue
		}
		return false, nil
	}
	return true, nil
}

// ssoRefusal is the answer of an SSO sign-in for an account no session may be
// opened on: a disabled one, and one pending a password change — whose exit
// is the forced change for an account that has a password, and the emailed
// reset (or its administrator) for one that signs in through SSO only. Nil
// for an active account.
func ssoRefusal(u identity.User) error {
	switch u.Status {
	case identity.UserStatusDisabled:
		return ErrAccountDisabled
	case identity.UserStatusPendingPasswordChange:
		return ErrPasswordChangeRequired
	}
	return nil
}

// ssoStamp is the precondition an SSO sign-in stamps under: the statuses
// ssoRefusal refused at read time (stampLogin adds disabled), refused again at
// write time.
func ssoStamp() identity.UserPatch {
	return identity.UserPatch{IfNotStatus: []identity.UserStatus{identity.UserStatusPendingPasswordChange}}
}
