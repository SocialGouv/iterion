package server

import (
	"errors"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/desktopsso"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The sign-in events of the audit log (ADR-157). A success is a tenant row
// on the org the session opened in, readable by that org's admins; a
// refusal is a platform row — it names no tenant, and the identifier it
// was attempted with is kept only as a peppered digest. Every handler that
// mints or refuses a session writes exactly one of them per attempt
// (TestEverySessionMintingHandlerIsAudited); a session refresh writes none.
const (
	actionLoginSucceeded = "auth.login.succeeded"
	actionLoginFailed    = "auth.login.failed"
)

// The method of a sign-in, meta.method of both events. oidc is every
// deployment-wide connector but the two built-in ones — the one the
// deployment names "sso" included; org_sso is an org's own IdP, whatever
// its slug.
const (
	loginMethodPassword = "password"
	loginMethodGitHub   = "github"
	loginMethodGoogle   = "google"
	loginMethodOIDC     = "oidc"
	loginMethodOrgSSO   = "org_sso"
	loginMethodDesktop  = "desktop"
)

// The flow a sign-in rode when the session is the side effect of another
// act, meta.flow; absent on a plain sign-in.
const (
	loginFlowRegister       = "register"
	loginFlowPasswordChange = "password_change"
	loginFlowPasswordReset  = "password_reset"
	// loginFlowSSOLink is an SSO round trip that attaches an identity to a
	// signed-in account rather than opening a session; it is audited only
	// where the callback's login-CSRF guard refuses it.
	loginFlowSSOLink = "sso_link"
)

// errAgentBindingMismatch is the OIDC callback's login-CSRF refusal: the
// user-agent binding cookie does not match the one the flow's start set.
var errAgentBindingMismatch = errors.New("oidc callback: the user-agent binding cookie does not match the flow's")

// callbackAttempt describes, before any identity resolves, the attempt an
// OIDC callback serves: the flow its state names and the connector.
func callbackAttempt(p oidc.PendingAuth, connector string) loginAttempt {
	a := loginAttempt{method: ssoLoginMethod(connector, p.TenantID != ""), provider: connector}
	switch {
	case p.LinkUserID != "":
		a.flow, a.account = loginFlowSSOLink, p.LinkUserID
	case p.Desktop:
		a.method = loginMethodDesktop
	}
	return a
}

// loginAttempt describes one sign-in attempt for the audit log.
type loginAttempt struct {
	// method is the credential presented: one of the loginMethod* tokens.
	method string
	// flow names the act the session is a side effect of (loginFlow*), or
	// is empty for a plain sign-in.
	flow string
	// provider is the connector slug of an SSO attempt (never a display
	// name, never an IdP subject).
	provider string
	// subject is the identifier the attempt presented (an e-mail address).
	// It is NEVER written: the row carries its peppered digest
	// (auth.Service.SubjectDigest) or nothing.
	subject string
	// account is the account a refused attempt is known to concern — the
	// server resolved it, not the client claimed it (a desktop ticket's
	// account, the account whose password changed without a session). It
	// is the refusal row's target_id, so the account's erasure reaches it.
	account string
}

// ssoLoginMethod classifies a connector for meta.method: the two built-in
// providers by name, an org's own IdP as org_sso, any other connector as
// oidc.
func ssoLoginMethod(connector string, orgProvider bool) string {
	switch {
	case orgProvider:
		return loginMethodOrgSSO
	case connector == loginMethodGitHub:
		return loginMethodGitHub
	case connector == loginMethodGoogle:
		return loginMethodGoogle
	}
	return loginMethodOIDC
}

// auditLoginSucceeded records a session minted for res. The row is tenant
// scoped on the org the session is active in — org-scoped, never
// team-scoped, so the org purge sweeps it with the org — and attributed to
// the account. An account with no active org (none to read the row) writes
// a platform row instead, so the sign-in is recorded somewhere rather than
// under a tenant nobody can list.
func (s *Server) auditLoginSucceeded(r *http.Request, a loginAttempt, res auth.LoginResult) {
	if s.auditStore == nil {
		return
	}
	meta := a.meta()
	if res.ActiveTeamID != "" {
		meta["team_id"] = res.ActiveTeamID
	}
	scope, tenant := audit.ScopeTenant, res.ActiveOrgID
	if tenant == "" {
		scope = audit.ScopePlatform
	}
	s.auditWriteActor(r, scope, tenant, res.User.ID, actorKindOf(res.User), actionLoginSucceeded, "user", res.User.ID, meta)
}

// auditLoginFailed records a sign-in err refused. The row is platform
// scoped and names no tenant: a refusal resolved no session, so there is no
// org to attribute it to, and the attempt may name no account at all. The
// actor is the request's identity when the route is authenticated (a
// password change proves the current password from a live session) and
// anonymous otherwise; the target is that identity, or else the account
// the server resolved for the attempt (loginAttempt.account), or none. The
// reason is the typed refusal, never the message.
func (s *Server) auditLoginFailed(r *http.Request, a loginAttempt, err error) {
	if s.auditStore == nil {
		return
	}
	meta := a.meta()
	meta["reason"] = loginFailureReason(err)
	if a.subject != "" && s.authSvc != nil {
		if digest, ok := s.authSvc.SubjectDigest(a.subject); ok {
			meta["subject_hash"] = digest
		}
	}
	actorID, actorKind, targetID := "", "anonymous", a.account
	if id, ok := auth.FromContext(r.Context()); ok && id.UserID != "" {
		actorID, targetID = id.UserID, id.UserID
		actorKind = "user"
		if id.IsSuperAdmin {
			actorKind = "super_admin"
		}
	}
	s.auditWriteActor(r, audit.ScopePlatform, "", actorID, actorKind, actionLoginFailed, "user", targetID, meta)
}

// meta is the attempt's description: the method, and the flow and provider
// where set. The subject is not in it.
func (a loginAttempt) meta() map[string]any {
	m := map[string]any{"method": a.method}
	if a.flow != "" {
		m["flow"] = a.flow
	}
	if a.provider != "" {
		m["provider"] = a.provider
	}
	return m
}

// actorKindOf is the audit actor kind of an account.
func actorKindOf(u identity.User) string {
	if u.IsSuperAdmin {
		return "super_admin"
	}
	return "user"
}

// loginFailureReason maps a refused sign-in to the stable token meta.reason
// carries: a committed credential change that opened no session first — it
// wraps whatever cause stopped the session, refusal reasons included, and
// is no refusal of the credential — then the typed reason a login path
// annotated (auth.ReasonOf), then the sentinel. A failure the server owns
// (a store that did not answer) is "error"; a refusal no token names is
// "refused", so a new sentinel is recorded rather than dropped.
func loginFailureReason(err error) string {
	if errors.Is(err, auth.ErrPasswordChangedSignIn) {
		return "password_changed_sign_in"
	}
	if reason, ok := auth.ReasonOf(err); ok {
		return string(reason)
	}
	switch {
	case errors.Is(err, auth.ErrAccountDisabled):
		return "disabled"
	case errors.Is(err, auth.ErrInvalidCredentials):
		return "invalid_credentials"
	case errors.Is(err, auth.ErrPasswordChangeRequired):
		return "password_change_required"
	case errors.Is(err, auth.ErrPasswordWeak):
		return "password_weak"
	case errors.Is(err, auth.ErrSignupClosed):
		return "signup_closed"
	case errors.Is(err, auth.ErrLinkRequiresConsent):
		return "link_required"
	case errors.Is(err, auth.ErrLinkAlreadyOwned):
		return "link_owned"
	case errors.Is(err, auth.ErrSSORestricted):
		return "sso_restricted"
	case errors.Is(err, auth.ErrInvitationNotFound),
		errors.Is(err, auth.ErrInvitationMismatch),
		errors.Is(err, identity.ErrInvitationUsed),
		errors.Is(err, identity.ErrInvitationExpired):
		return "invitation_invalid"
	case errors.Is(err, identity.ErrEmailAlreadyTaken):
		return "email_taken"
	case errors.Is(err, auth.ErrSessionRevoked),
		errors.Is(err, auth.ErrSessionExpired),
		errors.Is(err, auth.ErrSessionNotFound):
		return "session_revoked"
	case errors.Is(err, auth.ErrNotAMember),
		errors.Is(err, auth.ErrNotAnOrgMember),
		errors.Is(err, auth.ErrNoTeamInOrg):
		return "no_membership"
	case errors.Is(err, auth.ErrCredentialChangeBusy):
		return "busy"
	case errors.Is(err, desktopsso.ErrTicketNotFound):
		return "ticket_invalid"
	case errors.Is(err, oidc.ErrEmailMissing):
		return "email_missing"
	case errors.Is(err, oidc.ErrUnknownProvider):
		return "provider_unknown"
	case errors.Is(err, errAgentBindingMismatch):
		return "agent_binding"
	}
	if mapAuthErrorStatus(err) >= http.StatusInternalServerError {
		return "error"
	}
	return "refused"
}
