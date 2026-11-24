package auth

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// Identity is the authenticated principal extracted from the access
// JWT. Middleware injects it into the request ctx; handlers retrieve
// it via FromContext.
type Identity struct {
	UserID string
	Email  string
	// OrgID / OrgRole are the active organization and the principal's
	// role within it. A token minted before the org rollout carries an
	// empty OrgID; gates that need an org derive it from the active
	// team's OrgID and the session self-heals on the next refresh.
	OrgID        string
	OrgRole      identity.OrgRole
	TeamID       string
	Role         identity.Role
	IsSuperAdmin bool
	// Kind distinguishes a real authenticated user from a synthetic,
	// purpose-scoped principal minted by a self-authenticating surface
	// (see IdentityKind). The zero value is a real user.
	Kind IdentityKind
	// JTI is the JWT ID; useful for audit logging and explicit
	// revocation later (we don't revoke access tokens today; we
	// rely on short TTL + refresh rotation).
	JTI string
	// Credential names the credential the identity was read from (an access
	// token, a personal access token, a WS ticket, the dev-mode principal);
	// empty for an identity no credential reader built, which mints nothing
	// (CheckDerivation).
	Credential CredentialKind
	// ExpiresAt is the instant the credential the identity was read from
	// stops authenticating: an access token's expiry, a personal access
	// token's own expiry (zero when it has none), and for a WS ticket the
	// expiry of the credential that minted it. Zero for a synthetic
	// principal.
	ExpiresAt time.Time
	// SessionLineage and SessionGeneration name the refresh session an
	// access token was minted for: the lineage id of its login and the
	// account session generation that lineage was born under (SessionRoot) —
	// carried by a WS ticket minted from it. CheckDerivation checks them
	// against the session's revocations. Empty for a credential that names
	// no session.
	SessionLineage    string
	SessionGeneration int64
	// CredentialID names the row of a revocable credential the identity was
	// read from: a personal access token's id — carried by a WS ticket minted
	// from it. CheckDerivation and CheckStanding read the row through it.
	// Empty for a credential with no row.
	CredentialID string
	// MintedBy is, for a WS ticket, the kind of the credential that minted
	// it (whose session, expiry and row the ticket carries): CheckStanding
	// asks the ticket's standing as that credential's. Empty otherwise.
	MintedBy CredentialKind
	// Via names how this identity's team was resolved when it does NOT
	// come from the credential's own pin: the run-by-id resolution
	// (run_scope.go) re-scopes an identity to the RUN's team and records
	// here which standing carried it ("membership", "org-admin-lineage").
	// Empty means the team is the credential's own (session, pinned PAT,
	// synthetic principal) — the common case, and every pre-existing
	// construction site keeps it empty.
	Via string
	// TeamPin and OrgPin name the team the CREDENTIAL ITSELF is bound to and
	// that team's organization: a personal access token minted with an
	// explicit team pin (pkg/pat Token.TeamID). They are a property of the
	// credential, not of the request — TeamID follows the team a request
	// acts in and the run-by-id re-scope moves it, while the pin does not
	// move at all — so they are carried alongside the credential's kind,
	// expiry and row (withCredentialOf) and read by PinRefusesTeam /
	// PinRefusesOrg at every authorization gate.
	//
	// Empty for every credential that binds no team: an access token (its
	// session switches teams), an unpinned token (it only follows its
	// owner's default team, which the owner may change), a synthetic or
	// machine principal. An OrgPin empty beside a non-empty TeamPin is a
	// pin on a team with no parent org — it reaches no organization at all,
	// which is the fail-closed reading of a pin, not a missing fact.
	TeamPin string
	OrgPin  string
}

// PinRefusesTeam reports whether the credential's own team pin forbids this
// principal from acting on teamID. It is the ONE gate of the pin: every
// team-addressed authorization check in pkg/server asks it (auth_authz.go's
// gates and the run-by-id re-scope), so a pinned token leaks no team of its
// owner through a route whose own guard was never taught about pins.
//
// A super-admin passes: by default the platform authority sees everything
// (D19), and this predicate is the one named place a future team-level
// opt-in shield inserts for it.
func (i Identity) PinRefusesTeam(teamID string) bool {
	if i.TeamPin == "" || i.IsSuperAdmin {
		return false
	}
	return teamID != i.TeamPin
}

// PinRefusesOrg is PinRefusesTeam's twin for an org-addressed check: a
// credential pinned to a team reaches that team's organization and no other.
// A pin on a team with no parent org reaches none (OrgPin is empty and every
// named org differs from it).
func (i Identity) PinRefusesOrg(orgID string) bool {
	if i.TeamPin == "" || i.IsSuperAdmin {
		return false
	}
	return orgID != i.OrgPin
}

// IdentityKind distinguishes a real authenticated user (JWT or PAT — full
// role in the active team) from a principal nobody signed in as. Two
// families carry a non-empty Kind:
//
//   - SYNTHETIC — minted by a self-authenticating surface on an inbound
//     request: a webhook delivery, or a config-share editor link. It carries
//     a TeamID + Role so tenant-scoped STORE reads still work, but it must
//     NEVER pass the operator RBAC gates (canViewTeam / canManageTeam / …) —
//     those authorize humans acting on a team's resources.
//   - MACHINE — minted by a daemon of this process for work no request asked
//     for: the board dispatcher, the schedule tick, the trigger spine, the
//     retry sweeper. It carries its team so the launch gate meters the right
//     org, and no Role at all, because there is no human standing behind it.
//
// The zero value ("") is a real user, so existing code that builds an
// Identity without a Kind keeps full behaviour.
type IdentityKind string

const (
	KindUser     IdentityKind = ""         // JWT or PAT — a real user (zero value)
	KindWebhook  IdentityKind = "webhook"  // inbound-webhook launch trigger
	KindShare    IdentityKind = "share"    // config-share editor link
	KindInternal IdentityKind = "internal" // a daemon of this process launching its own work
)

// IsSynthetic reports whether this principal is a purpose-scoped,
// self-authenticating identity (webhook/share) rather than a real user.
// Operator RBAC gates deny synthetic identities by default; an endpoint that
// intentionally serves one opts in explicitly.
func (i Identity) IsSynthetic() bool {
	return i.Kind == KindWebhook || i.Kind == KindShare
}

// IsMachine reports whether this principal is a daemon of this process acting
// on its own schedule (KindInternal). Kept SEPARATE from IsSynthetic because
// the two classes answer different questions: a synthetic principal is a
// stranger's request that authenticated itself and is therefore refused by
// every operator RBAC gate, while a machine principal is this process and
// carries no Role to judge. A gate that admits one does not thereby admit the
// other — the launch gate names both explicitly.
func (i Identity) IsMachine() bool {
	return i.Kind == KindInternal
}

// HasRole reports whether the principal has at least the requested
// role *in their active team*. Super-admins always pass.
func (i Identity) HasRole(want identity.Role) bool {
	if i.IsSuperAdmin {
		return true
	}
	return i.Role.AtLeast(want)
}

// HasOrgRole reports whether the principal has at least the requested
// role *in their active org*. Super-admins always pass.
func (i Identity) HasOrgRole(want identity.OrgRole) bool {
	if i.IsSuperAdmin {
		return true
	}
	return i.OrgRole.AtLeast(want)
}

type identityCtxKey struct{}

// WithIdentity returns a child ctx carrying the given Identity.
// Used by middleware after JWT validation.
func WithIdentity(parent context.Context, id Identity) context.Context {
	return context.WithValue(parent, identityCtxKey{}, id)
}

// FromContext returns the Identity carried by ctx and a boolean
// reporting whether one was set. Handlers that need authentication
// should check the second return and surface a 401/500 to the caller
// — never panic. Middleware (RequireAuth) is the right place to gate
// admission, not a panic in the handler body.
func FromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	id, ok := ctx.Value(identityCtxKey{}).(Identity)
	return id, ok
}
