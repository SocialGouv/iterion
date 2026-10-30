package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/mail"
)

// Sentinel errors raised by Service. Handlers map them to HTTP
// statuses (Unauthorized, Forbidden, NotFound, Conflict).
var (
	ErrInvalidCredentials     = errors.New("auth: invalid credentials")
	ErrAccountDisabled        = errors.New("auth: account disabled")
	ErrPasswordChangeRequired = errors.New("auth: password change required")
	ErrPasswordWeak           = errors.New("auth: password too weak")
	ErrSignupClosed           = errors.New("auth: signup is invite-only")
	ErrLinkRequiresConsent    = errors.New("auth: SSO login matched an existing account by email — link explicitly from settings")
	ErrSSORestricted          = errors.New("auth: SSO login is restricted to allow-listed GitHub teams")
	ErrLinkAlreadyOwned       = errors.New("auth: this SSO identity is already linked to a different account")
	ErrInvitationNotFound     = errors.New("auth: invitation not found")
	ErrInvitationMismatch     = errors.New("auth: invitation does not match user")
	ErrTeamNotFound           = errors.New("auth: team not found")
	ErrNotAMember             = errors.New("auth: user is not a member of the team")
	ErrOrgNotFound            = errors.New("auth: org not found")
	ErrNotAnOrgMember         = errors.New("auth: user is not a member of the org")
	ErrNoTeamInOrg            = errors.New("auth: user has no team in the org")
)

// LockoutThreshold is the number of consecutive failed password
// attempts that triggers a temporary account lockout. The counter
// resets to zero on any successful Login. Tuned conservatively: high
// enough that a typo cluster doesn't lock a legitimate user, low
// enough that an attacker can't credential-stuff at full rate.
const LockoutThreshold = 5

// LockoutDuration is the wall-clock window a lockout lasts. While
// active, the gate at the top of Login short-circuits with
// ErrInvalidCredentials regardless of the password supplied — the
// constant-time dummy-hash check still runs so timing doesn't leak
// the lockout state.
const LockoutDuration = 15 * time.Minute

// passwordProofLocked answers whether the account's brute-force lockout
// refuses a current-password proof right now — the same gate Login runs,
// shared with every route whose proof IS the current password.
func passwordProofLocked(now time.Time, u identity.User) bool {
	return u.LockedUntil != nil && now.Before(*u.LockedUntil)
}

// countPasswordProofFailure records a failed current-password proof into the
// account's brute-force counter and locks it at LockoutThreshold for
// LockoutDuration. Login and ChangePassword feed the same counter, so the
// current-password proof is no cheaper to brute-force than the sign-in it
// stands for. The write is best-effort (the same trade the login counter
// makes): the caller's answer does not change.
func (s *Service) countPasswordProofFailure(ctx context.Context, u identity.User) {
	lockout := identity.UserLockout{FailedLogins: u.FailedLogins + 1}
	if lockout.FailedLogins >= LockoutThreshold {
		lockUntil := s.now().UTC().Add(LockoutDuration)
		lockout.LockedUntil = &lockUntil
	}
	if _, persistErr := s.store.PatchUser(ctx, u.ID, identity.UserPatch{Lockout: &lockout}); persistErr != nil && s.logger != nil {
		s.logger.Error("auth: persist failed-login counter for %s: %v", u.ID, persistErr)
	}
}

// SignupMode controls who may register without an invitation.
type SignupMode string

const (
	SignupOpen       SignupMode = "open"
	SignupInviteOnly SignupMode = "invite_only"
)

// GitHubUngrantedPolicy controls what happens to a GitHub SSO login that
// reaches the deployment while team-gating is active but matches no
// allow-listed team (orgsso.GitHubTeamGrant). It decouples GitHub admission
// from the global SignupMode.
type GitHubUngrantedPolicy string

const (
	// GitHubUngrantedRefuse rejects the login (ErrSSORestricted). Default —
	// preserves the historical behaviour.
	GitHubUngrantedRefuse GitHubUngrantedPolicy = "refuse"
	// GitHubUngrantedSubmitter admits the user as a teamless account (no
	// personal team), so they can authenticate + submit to the marketplace
	// but have no other rights until an admin adds them to an allow-listed
	// team. This is the public "sign up with GitHub to submit a bot" tier.
	GitHubUngrantedSubmitter GitHubUngrantedPolicy = "submitter"
)

// MinPasswordLen is the floor enforced at registration. Set
// intentionally low to avoid frustrating users with a password
// manager; argon2id covers the brute-force surface.
const MinPasswordLen = 8

// InvitationTTL is the default lifetime of an invitation. Keep
// generous (7 days) so users on email-light schedules still arrive
// in time.
const InvitationTTL = 7 * 24 * time.Hour

// Service is the high-level entry point for authentication and
// identity-mutation flows. Handlers in pkg/server depend on this
// type, not directly on the Mongo stores, so tests can swap in
// memory-backed implementations.
type Service struct {
	store    identity.Store
	sessions SessionStore
	// patState reads a personal access token's row for CheckDerivation and
	// CheckStanding (SetPATState).
	patState   PATState
	signer     *JWTSigner
	signupMode SignupMode
	// githubUngrantedPolicy decides whether a GitHub login matching no
	// allow-listed team is refused or admitted teamless (see B2 in
	// LoginWithExternal). Defaults to GitHubUngrantedRefuse.
	githubUngrantedPolicy GitHubUngrantedPolicy
	now                   func() time.Time
	refreshTTL            time.Duration
	// refreshReuseGrace is the reuse grace window of Refresh: how long
	// after a rotation the token it retired still receives its successor.
	// Zero disables the window.
	refreshReuseGrace time.Duration
	// trustedAutoLinkProviders names the OIDC providers whose verified
	// email may be used to link a fresh external identity onto an
	// existing password-account user without an explicit "link this
	// connection" UI step. Empty = no auto-link (the default).
	// Operators only enroll providers here when they fully trust the
	// IdP's email verification (e.g. an in-house SSO they control).
	trustedAutoLinkProviders map[string]struct{}
	// orgSSO holds per-tenant SSO provider rows. nil disables every per-org
	// branch (LoginWithExternalForOrg → ErrUnknownProvider) and the GitHub
	// team-grant reverse lookup in LoginWithExternal becomes a no-op.
	orgSSO orgsso.Store
	// domains holds per-tenant verified email-domain claims, gating per-org
	// auto-link. nil → auto-link is never permitted (existing-email logins fall
	// back to ErrLinkRequiresConsent).
	domains orgsso.DomainStore
	// logger, when non-nil, receives audit-grade events like
	// "stored hash unparseable for user X" that we don't want to
	// silently swallow into a generic ErrInvalidCredentials response.
	logger *iterlog.Logger
	// resets + mailer + publicURL power the self-service password
	// reset (password_reset.go) and invitation emails. All optional:
	// nil resets/mailer disable the reset flow (request becomes a
	// logged no-op).
	resets    PasswordResetStore
	mailer    mail.Mailer
	publicURL string
	// teamGrantSweep takes the pending invitations kept on a team grant a
	// write of this service revokes (SetTeamGrantSweep). The selection and
	// its audit line live in pkg/server, which owns the invitation rules and
	// the audit store, so this package holds the registered implementation
	// rather than a second one.
	teamGrantSweep TeamGrantSweep
	// teamRankReconcile re-measures, over the pending invitations of userID
	// in teamID, the mint cap that would bind their creator NOW, after a
	// write of this service LOWERED their team authority (SetTeamRankReconcile).
	// Same split as teamGrantSweep: the selection and its audit line live in
	// pkg/server.
	teamRankReconcile TeamRankReconcile
	// passes tracks the detached passes of recorded revocations (runPass).
	passes sync.WaitGroup
}

// TeamGrantSweep takes the pending invitations kept on userID's grant in
// teamID — the rows a mint's own confirmation keeps on that grant, which
// nothing else measures again (pkg/server standingAfterMintTakesInvitation)
// — under reason as the audit cause of each row taken. It is called by the
// writes of this service that REVOKE such a grant, right after the grant is
// gone: whoever withdraws a premise takes what rests on it, and what stands
// otherwise is a re-entry ticket into the team the grant was taken from.
//
// It returns an error when a read it needs or a revocation failed, which its
// caller answers for: the rows may stand.
type TeamGrantSweep func(ctx context.Context, userID, teamID, reason string) error

// SetTeamGrantSweep registers the sweep the grant-revoking writes of this
// service owe (TeamGrantSweep). The server registers its own
// (revokeInvitationsKeptOnTeamGrant) when it is built on this service, so one
// implementation decides for every write that owes it; a service with none
// refuses to revoke a grant rather than leave the rows kept on it standing
// (reconcileGitHubGrants).
func (s *Service) SetTeamGrantSweep(sweep TeamGrantSweep) { s.teamGrantSweep = sweep }

// TeamRankReconcile re-measures, over the pending invitations of userID in
// teamID, the mint cap that would bind their creator NOW, after a write of
// this service LOWERED their team authority to grant — the role the write
// left on their row, or "" where it took the grant away. It is the minted
// half of the same rule the kept-on-grant sweep is one half of: whoever
// lowers a team authority takes the rows its target minted above what that
// leaves them (pkg/server takeTeamRankInvitations).
//
// It returns an error when a read it needs or a revocation failed, which its
// caller answers for: the rows may stand.
type TeamRankReconcile func(ctx context.Context, userID, teamID string, grant identity.Role) error

// SetTeamRankReconcile registers the reconcile the authority-lowering writes
// of this service owe (TeamRankReconcile). The server registers its own
// (takeTeamRankInvitations) when it is built on this service, so one
// implementation decides for every write that owes it; a service with none
// refuses a write that would lower an authority rather than leave the rows
// minted above what that leaves standing.
func (s *Service) SetTeamRankReconcile(fn TeamRankReconcile) { s.teamRankReconcile = fn }

// Config wires the Service.
type Config struct {
	Store      identity.Store
	Sessions   SessionStore
	Signer     *JWTSigner
	SignupMode SignupMode
	// GitHubUngrantedPolicy controls a gated GitHub login that matches no
	// allow-listed team: "refuse" (default) or "submitter" (teamless admit).
	GitHubUngrantedPolicy GitHubUngrantedPolicy
	RefreshTTL            time.Duration
	// RefreshReuseGrace is how long after a rotation the refresh token it
	// retired still receives its successor instead of reading as reuse
	// (Service.Refresh). nil takes DefaultRefreshReuseGrace; zero disables
	// the window, so every presentation of a retired token is reuse;
	// a negative value is refused.
	RefreshReuseGrace *time.Duration
	// TrustedAutoLinkProviders is the operator-configured allowlist of
	// OIDC providers whose verified email is safe to auto-link onto a
	// pre-existing password-account user. Empty = no auto-link; a fresh
	// SSO login that finds an existing user by email returns
	// ErrLinkRequiresConsent so the UI can prompt the user to link
	// manually from their settings.
	TrustedAutoLinkProviders []string
	// OrgSSO is the per-tenant SSO provider store (per-org Keycloak rows +
	// GitHub team-gating). Optional; nil disables per-org SSO.
	OrgSSO orgsso.Store
	// Domains is the per-tenant verified email-domain store gating per-org
	// auto-link. Optional; nil disables auto-link.
	Domains orgsso.DomainStore
	Logger  *iterlog.Logger
	// Resets + Mailer + PublicURL enable the password-reset flow and
	// invitation emails (all optional — see Service fields).
	Resets    PasswordResetStore
	Mailer    mail.Mailer
	PublicURL string
}

// NewService validates the config and returns a wired Service.
func NewService(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("auth: nil identity store")
	}
	if cfg.Sessions == nil {
		return nil, errors.New("auth: nil session store")
	}
	if cfg.Signer == nil {
		return nil, errors.New("auth: nil signer")
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 30 * 24 * time.Hour
	}
	reuseGrace := DefaultRefreshReuseGrace
	if cfg.RefreshReuseGrace != nil {
		reuseGrace = *cfg.RefreshReuseGrace
	}
	if err := checkRefreshReuseGrace(reuseGrace, cfg.RefreshTTL); err != nil {
		return nil, err
	}
	if reuseGrace > LongRefreshReuseGrace && cfg.Logger != nil {
		cfg.Logger.Warn("auth: refresh reuse grace %s is above %s: a refresh token a rotation retired keeps receiving its successor that long, while the retries the window exists for land within seconds (check the unit of ITERION_REFRESH_REUSE_GRACE)", reuseGrace, LongRefreshReuseGrace)
	}
	if cfg.SignupMode == "" {
		cfg.SignupMode = SignupInviteOnly
	}
	switch cfg.SignupMode {
	case SignupOpen, SignupInviteOnly:
	default:
		return nil, fmt.Errorf("auth: invalid signup mode %q", cfg.SignupMode)
	}
	if cfg.GitHubUngrantedPolicy == "" {
		cfg.GitHubUngrantedPolicy = GitHubUngrantedRefuse
	}
	switch cfg.GitHubUngrantedPolicy {
	case GitHubUngrantedRefuse, GitHubUngrantedSubmitter:
	default:
		return nil, fmt.Errorf("auth: invalid github ungranted policy %q", cfg.GitHubUngrantedPolicy)
	}
	trusted := make(map[string]struct{}, len(cfg.TrustedAutoLinkProviders))
	for _, p := range cfg.TrustedAutoLinkProviders {
		if p != "" {
			trusted[p] = struct{}{}
		}
	}
	return &Service{
		store:                    cfg.Store,
		sessions:                 cfg.Sessions,
		signer:                   cfg.Signer,
		signupMode:               cfg.SignupMode,
		githubUngrantedPolicy:    cfg.GitHubUngrantedPolicy,
		refreshTTL:               cfg.RefreshTTL,
		refreshReuseGrace:        reuseGrace,
		now:                      time.Now,
		trustedAutoLinkProviders: trusted,
		orgSSO:                   cfg.OrgSSO,
		domains:                  cfg.Domains,
		logger:                   cfg.Logger,
		resets:                   cfg.Resets,
		mailer:                   cfg.Mailer,
		publicURL:                strings.TrimRight(cfg.PublicURL, "/"),
	}, nil
}

// LongRefreshReuseGrace is the reuse grace window above which NewService
// warns: far above the retries the window exists for (seconds), and more
// often a unit typed wrong than a choice.
const LongRefreshReuseGrace = 5 * time.Minute

// checkRefreshReuseGrace refuses a reuse grace window that is negative, or
// not below the refresh TTL: a window as long as a session's lifetime
// outlives the successor it would redeliver.
func checkRefreshReuseGrace(grace, refreshTTL time.Duration) error {
	if grace < 0 {
		return fmt.Errorf("auth: invalid refresh reuse grace %s (want >= 0)", grace)
	}
	if grace > 0 && grace >= refreshTTL {
		return fmt.Errorf("auth: invalid refresh reuse grace %s (want below the refresh TTL %s)", grace, refreshTTL)
	}
	return nil
}

// EmailEnabled reports whether a real mailer is wired (drives the
// SPA's forgot-password entry point via server_info).
func (s *Service) EmailEnabled() bool { return s.mailer != nil && s.mailer.Enabled() }

// Mailer exposes the wired mailer (nil-safe for callers that send
// optional notifications like invitation emails).
func (s *Service) Mailer() mail.Mailer { return s.mailer }

// PublicURL is the externally-reachable base URL used in email links.
func (s *Service) PublicURL() string { return s.publicURL }

// LoginResult bundles the artifacts returned to the caller after a
// successful login or refresh. The HTTP layer translates these into
// cookies / JSON.
type LoginResult struct {
	User           identity.User
	ActiveOrgID    string
	ActiveOrgRole  identity.OrgRole
	ActiveTeamID   string
	ActiveRole     identity.Role
	AccessToken    string
	AccessExpires  time.Time
	RefreshToken   string
	RefreshExpires time.Time
	Memberships    []identity.Membership
	OrgMemberships []identity.OrgMembership
}

// DesktopLogin is what a desktop SSO login defers from the IdP callback to
// the ticket's redemption (#2635): the account, the lineage its session will
// be born under and the landing team the login resolved — and NO token. The
// callback mints nothing: no session row is written and no refresh token
// exists while the login waits, so an unredeemed ticket leaves no live
// session (there is nothing to revoke at its expiry) and the ticket store
// never rests a credential. The session is minted by RedeemDesktopLogin,
// when the desktop redeems the ticket.
//
// The bson tags are the wire shape of a desktop_sso_tickets document's
// payload (pkg/auth/desktopsso).
type DesktopLogin struct {
	UserID string `bson:"user_id" json:"user_id"`
	// Root names the lineage the session will be born under, resolved at the
	// callback: a revoke-all landing between the callback and the redemption
	// raises the account's generation above the one carried here, and the
	// redemption's standing check refuses it (SessionRevoked).
	Root SessionRoot `bson:"root" json:"root"`
	// PreferredTeamID is the active team the login resolved at the callback
	// (the per-org SSO landing, or the account's default): the redemption
	// prefers it again when the account is still a member of it.
	PreferredTeamID string `bson:"preferred_team,omitempty" json:"preferred_team,omitempty"`
}

// Login authenticates with email + password. On success, issues an
// access JWT bound to the user's default team (or first available)
// and a refresh token.
func (s *Service) Login(ctx context.Context, email, password, userAgent, ip string) (LoginResult, error) {
	u, err := s.store.GetUserByEmail(ctx, email)
	if errors.Is(err, identity.ErrNotFound) {
		// Spend the same time hashing a dummy password so we don't
		// leak account existence via response timing.
		_, _ = VerifyPassword(password, dummyHash)
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, err
	}
	if u.Status == identity.UserStatusDisabled {
		return LoginResult{}, ErrAccountDisabled
	}
	if u.LockedUntil != nil && s.now().Before(*u.LockedUntil) {
		// Spend the same argon2id cycles a real attempt would, so a
		// network observer can't distinguish "locked" from "wrong
		// password" by response timing. The LockoutDuration docstring
		// promises this; the gate must honour it.
		_, _ = VerifyPassword(password, dummyHash)
		return LoginResult{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil {
		// A non-nil error from VerifyPassword is structural (corrupt
		// stored hash, unknown algorithm prefix) — distinct from a
		// plain mismatch. Swallowing it as ErrInvalidCredentials
		// leaves the account permanently un-loginable with no signal
		// for the operator. Log the failure under the user id so it
		// surfaces in audit aggregation without leaking the hash.
		if s.logger != nil {
			s.logger.Error("auth: stored password hash for user %s is unparseable: %v", u.ID, err)
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	if !ok {
		// Brute-force lockout: increment the failed counter and lock
		// the account when it reaches LockoutThreshold. The gate at
		// the top of Login then short-circuits subsequent attempts
		// during LockoutDuration. The counter persistence is
		// best-effort — a write failure logs at error but doesn't
		// surface to the caller, since the response shape must remain
		// indistinguishable from a non-locked invalid-credentials
		// case (per the comment on dummyHash above).
		// The counter and the deadline it produced are one fact, and the only
		// fact this path owns: a whole-row write would also restore the
		// status, the privilege flag and the password hash it read.
		s.countPasswordProofFailure(ctx, u)
		return LoginResult{}, ErrInvalidCredentials
	}

	// Password verified — but a pending_password_change status must
	// not yield a fully-privileged session. Returning the dedicated
	// sentinel lets the HTTP layer / SPA route to a change-password
	// flow without first minting access + refresh tokens.
	if u.Status == identity.UserStatusPendingPasswordChange {
		return LoginResult{}, ErrPasswordChangeRequired
	}

	// The session's generation is read BEFORE the stamp commits the login to
	// the password it verified: a credential change replaces the hash, then
	// raises the generation again (ChangePassword), so a login whose stamp
	// lands before the new hash was read under a generation that raise
	// covers, and one whose stamp lands after it is refused by the stamp.
	root, err := NewSessionRoot(ctx, s.sessions, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	// A verified password clears the brute-force lockout; the stamp carries
	// it, so the counter and the login time land in one write. The
	// precondition names BOTH statuses the gates above refused — an admin
	// password reset commits pending_password_change together with a hash of
	// its own, so a session minted after it would be a session on a
	// credential the admin had already replaced — and the hash the password
	// was verified against.
	verified := u.PasswordHash
	u, err = s.stampLogin(ctx, u, s.now().UTC(), identity.UserPatch{
		Lockout: &identity.UserLockout{},
		IfNotStatus: []identity.UserStatus{
			identity.UserStatusDisabled,
			identity.UserStatusPendingPasswordChange,
		},
		IfPasswordHash: &verified,
	})
	if err != nil {
		return LoginResult{}, err
	}
	lc, err := s.prepareLogin(ctx, u, "")
	if err != nil {
		return LoginResult{}, err
	}
	if lc, err = s.signLogin(lc, root); err != nil {
		return LoginResult{}, err
	}
	return s.commitLogin(ctx, lc, u, userAgent, ip)
}

// stampLogin records a successful login on u — the last-login time, plus
// whatever else the calling path owns in `owned` — and returns the stored
// row. LastLoginAt is stampLogin's own and is overwritten; the precondition
// is the CALLER's, and stampLogin only adds `disabled` to it — the one status
// no login path may ever stamp over — keeping every status the caller named.
// A path that refused more at read time (Login also refuses
// pending_password_change) therefore keeps that refusal at write time.
//
// The precondition is where a login path commits to the status it checked:
// the patch refuses a row whose status moved into what the caller would not
// act on, and stampLogin answers ErrAccountDisabled, so no session is minted
// on a snapshot that stopped being true while the credential was being
// verified. That single sentinel answers whichever status arm refused: a
// login that loses this race says no more than that it will not proceed. A
// password login also names the hash it verified (IfPasswordHash): a hash
// replaced since answers ErrInvalidCredentials. Other store errors are
// returned as they are, with u unchanged: each caller decides whether a
// cosmetic stamp failing fails the login.
func (s *Service) stampLogin(ctx context.Context, u identity.User, at time.Time, owned identity.UserPatch) (identity.User, error) {
	owned.LastLoginAt = &at
	owned.IfNotStatus = alsoRefusing(owned.IfNotStatus, identity.UserStatusDisabled)
	patched, err := s.store.PatchUser(ctx, u.ID, owned)
	if errors.Is(err, identity.ErrUserStatusChanged) {
		return identity.User{}, ErrAccountDisabled
	}
	if errors.Is(err, identity.ErrUserPasswordChanged) {
		// The password the caller verified was replaced while it was being
		// checked: it no longer signs the account in.
		return identity.User{}, ErrInvalidCredentials
	}
	if err != nil {
		return u, err
	}
	return patched, nil
}

// alsoRefusing returns refused with want added unless it is already there, so
// adding a mandatory arm to a precondition cannot silently drop the arms its
// caller put in. The result is a fresh slice: the caller's array is never
// appended into.
func alsoRefusing(refused []identity.UserStatus, want identity.UserStatus) []identity.UserStatus {
	for _, st := range refused {
		if st == want {
			return refused
		}
	}
	out := make([]identity.UserStatus, 0, len(refused)+1)
	out = append(out, refused...)
	return append(out, want)
}

// ChangePasswordPending completes the forced-rotation flow for a user in
// pending_password_change status (e.g. the bootstrapped super-admin): it
// verifies the temporary password, sets the chosen new one, marks the
// account active, and issues a normal session. It applies ONLY to
// pending_password_change accounts — rotating an already-active account's
// password is a separate, authenticated concern — and returns the opaque
// ErrInvalidCredentials for a missing user, wrong status, or bad temp
// password so the endpoint cannot enumerate accounts or their state. A
// lookup that FAILS is returned as the failure it is, as Login returns it:
// it fails whether or not the address exists, so it tells nothing about the
// account. Like every credential change it holds the account's
// credential-change claim, revokes before and after its commit, and answers
// a failure once the new password is committed as ErrPasswordChangedSignIn.
func (s *Service) ChangePasswordPending(ctx context.Context, email, currentPassword, newPassword, userAgent, ip string) (LoginResult, error) {
	if len(newPassword) < MinPasswordLen {
		return LoginResult{}, ErrPasswordWeak
	}
	u, err := s.store.GetUserByEmail(ctx, email)
	if errors.Is(err, identity.ErrNotFound) {
		// Spend the same argon2id cycles a real attempt would so a
		// network observer can't distinguish "no such account" from
		// "wrong temp password" by response timing (matches Login).
		_, _ = VerifyPassword(currentPassword, dummyHash)
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: look up the account whose temporary password is changed: %w", err)
	}
	if u.Status != identity.UserStatusPendingPasswordChange {
		// Same timing-equalisation for the wrong-status branch.
		_, _ = VerifyPassword(currentPassword, dummyHash)
		return LoginResult{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(currentPassword, u.PasswordHash)
	if err != nil || !ok {
		return LoginResult{}, ErrInvalidCredentials
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return LoginResult{}, err
	}
	// One credential change of the account at a time: a twin of this request
	// waits here, then finds the temporary password replaced.
	ctx, release, err := s.claimCredentialChange(ctx, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	defer release()
	verified := u.PasswordHash
	if u, err = s.store.GetUser(ctx, u.ID); err != nil {
		return LoginResult{}, fmt.Errorf("auth: read the account whose temporary password is changed: %w", err)
	}
	if u.Status != identity.UserStatusPendingPasswordChange || u.PasswordHash != verified {
		return LoginResult{}, ErrInvalidCredentials
	}
	// The session's reads run before the rotation commits: a read that fails
	// then leaves the temporary password the holder's to use again.
	lc, err := s.prepareLogin(ctx, u, "")
	if err != nil {
		return LoginResult{}, err
	}
	// Every session of the account ends before its credential is replaced:
	// none may outlive the temporary password it was minted under, and a
	// revocation that fails leaves the temporary password the holder's to use
	// again.
	if _, err := s.revokeAccount(ctx, u.ID); err != nil {
		return LoginResult{}, err
	}
	now := s.now().UTC()
	active := identity.UserStatusActive
	// The rotation owns the credential, the status it unblocks and the
	// lockout — under the precondition that REPRODUCES the checks above: the
	// row must still be pending and still hold the temporary password this
	// request verified. An account an admin closed must not be reactivated by
	// its own holder's rotation, and an account an admin activated must not
	// have the credential it was given replaced by the holder of a superseded
	// temporary password. The refusal stays opaque, like the wrong-status
	// branch above, and leaves the credential that landed under it in place.
	u, err = s.store.PatchUser(ctx, u.ID, identity.UserPatch{
		PasswordHash: &hash,
		Status:       &active,
		Lockout:      &identity.UserLockout{},
		LastLoginAt:  &now,
		IfNotStatus: []identity.UserStatus{
			identity.UserStatusDisabled,
			identity.UserStatusActive,
		},
		IfPasswordHash: &verified,
	})
	if errors.Is(err, identity.ErrUserStatusChanged) || errors.Is(err, identity.ErrUserPasswordChanged) {
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, s.commitUnconfirmed(ctx, lc.user.ID, err)
	}
	return s.openSessionAfterChange(ctx, lc, u, userAgent, ip)
}

// changeRevocationAttempts bounds the revocation a credential change runs
// once its new password is committed (openSessionAfterChange), or may be
// (commitUnconfirmed).
const changeRevocationAttempts = 3

// commitUnconfirmed answers a credential change whose commit was sent and
// failed with anything but its precondition's refusal — a cancelled request,
// a dropped connection: the write may have landed. The revocation that
// follows a commit runs anyway (if the commit did not land, it ends only the
// sessions born since the revocation before it), and the failure is returned
// for the caller to answer; a reset link it used is not given back.
func (s *Service) commitUnconfirmed(ctx context.Context, userID string, cause error) error {
	var err error
	for attempt := 0; attempt < changeRevocationAttempts; attempt++ {
		if _, err = s.revokeAccount(ctx, userID); err == nil {
			break
		}
	}
	if err != nil && s.logger != nil {
		s.logger.Error("auth: the password change of user %s may have been saved (its commit answered: %v), and the revocation after it failed %d times (last: %v): a sign-in with the old password during the change may survive it — sign the account out everywhere", userID, cause, changeRevocationAttempts, err)
	}
	return fmt.Errorf("auth: the password change of user %s may have been saved: %w", userID, cause)
}

// openSessionAfterChange completes a credential change whose new password is
// committed: it raises the account's generation again — a sign-in with the
// old password whose stamp landed before the commit read its generation
// before that stamp (Login), so this raise covers it — then stamps the
// account under the precondition a sign-in stamps with, and opens the
// caller's session under the generation it raised. A disable whose status
// landed after the commit and whose record landed before the raise recorded
// a generation BELOW the session's: the stamp refuses the account it
// disabled, and a disable landing after the stamp records a raise above the
// session's generation, which covers it. Every failure from here on is
// ErrPasswordChangedSignIn: the password IS changed, and the caller signs in
// with it.
func (s *Service) openSessionAfterChange(ctx context.Context, lc loginContext, u identity.User, userAgent, ip string) (LoginResult, error) {
	var generation int64
	var err error
	for attempt := 0; attempt < changeRevocationAttempts; attempt++ {
		if generation, err = s.revokeAccount(ctx, u.ID); err == nil {
			break
		}
	}
	if err != nil {
		if s.logger != nil {
			s.logger.Error("auth: the password of user %s is changed, but the revocation after the change failed %d times (last: %v): a sign-in with the old password during the change may survive it — sign the account out everywhere", u.ID, changeRevocationAttempts, err)
		}
		return LoginResult{}, passwordChangedSignIn(err)
	}
	if u, err = s.stampLogin(ctx, u, s.now().UTC(), identity.UserPatch{
		IfNotStatus: []identity.UserStatus{identity.UserStatusPendingPasswordChange},
	}); err != nil {
		return LoginResult{}, passwordChangedSignIn(err)
	}
	if lc, err = s.signLogin(lc, RevokedAccountRoot(generation)); err != nil {
		return LoginResult{}, passwordChangedSignIn(err)
	}
	res, err := s.commitLogin(ctx, lc, u, userAgent, ip)
	if err != nil {
		return LoginResult{}, passwordChangedSignIn(err)
	}
	return res, nil
}

// commitLogin completes a login whose context was prepared and signed
// before the operation committed: it writes the refresh session of u — the
// row the operation wrote — under the root the context names, which is the
// one step after the commit; nothing is read after it.
func (s *Service) commitLogin(ctx context.Context, lc loginContext, u identity.User, userAgent, ip string) (LoginResult, error) {
	refresh, sess, err := IssueSession(ctx, s.sessions, lc.root, u.ID, userAgent, ip, s.refreshTTL)
	if err != nil {
		return LoginResult{}, err
	}
	lc.user = u
	return lc.result(refresh, sess.ExpiresAt), nil
}

// issueLoginInTeam is the shared post-authentication path of an EXISTING
// account: every caller that has just authenticated one ends here. It picks
// the active team — preferredTeamID when it names one of the user's
// memberships (and its role), the DefaultTeamID/first-membership pick
// otherwise; the per-org SSO flow uses it so a user who logs in via an
// org's own Keycloak lands in that org rather than their stored default —
// mints tokens, and records the refresh session.
func (s *Service) issueLoginInTeam(ctx context.Context, u identity.User, preferredTeamID, userAgent, ip string) (LoginResult, error) {
	lc, err := s.prepareLogin(ctx, u, preferredTeamID)
	if err != nil {
		return LoginResult{}, err
	}
	root, err := NewSessionRoot(ctx, s.sessions, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	if lc, err = s.signLogin(lc, root); err != nil {
		return LoginResult{}, err
	}
	return s.commitLogin(ctx, lc, u, userAgent, ip)
}

// issueNewAccountLogin is issueLoginInTeam for an account the calling flow
// created a moment ago: its root is born under generation zero
// (NewAccountRoot), so no session-store row of the account is read.
func (s *Service) issueNewAccountLogin(ctx context.Context, u identity.User, preferredTeamID, userAgent, ip string) (LoginResult, error) {
	lc, err := s.prepareLogin(ctx, u, preferredTeamID)
	if err != nil {
		return LoginResult{}, err
	}
	if lc, err = s.signLogin(lc, NewAccountRoot()); err != nil {
		return LoginResult{}, err
	}
	return s.commitLogin(ctx, lc, u, userAgent, ip)
}

// finishExternalLogin ends an SSO login of an existing account — the tails
// of LoginWithExternal and LoginWithExternalForOrg share it. With desktop nil
// it mints and delivers the session at once (issueLoginInTeam). With one, it
// DEFERS the mint to the desktop ticket's redemption: what redemption needs
// is captured (the account, the lineage its session will be born under — read
// now so a revoke-all landing meanwhile covers it — and the landing team the
// login resolved), and nothing is signed and no session is written (#2635).
func (s *Service) finishExternalLogin(ctx context.Context, u identity.User, preferredTeamID, userAgent, ip string, desktop *DesktopLogin) (LoginResult, error) {
	if desktop == nil {
		return s.issueLoginInTeam(ctx, u, preferredTeamID, userAgent, ip)
	}
	lc, err := s.prepareLogin(ctx, u, preferredTeamID)
	if err != nil {
		return LoginResult{}, err
	}
	root, err := NewSessionRoot(ctx, s.sessions, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	*desktop = DesktopLogin{UserID: u.ID, Root: root, PreferredTeamID: lc.teamID}
	return LoginResult{}, nil
}

// finishNewAccountLogin is finishExternalLogin for an account the calling
// flow created a moment ago: its deferred root is born under generation zero
// (NewAccountRoot) — no session-store row of the account is read, and a
// revoke-all landing once the account exists raises the generation above
// zero, which the redemption's standing check refuses.
func (s *Service) finishNewAccountLogin(ctx context.Context, u identity.User, preferredTeamID, userAgent, ip string, desktop *DesktopLogin) (LoginResult, error) {
	if desktop == nil {
		return s.issueNewAccountLogin(ctx, u, preferredTeamID, userAgent, ip)
	}
	lc, err := s.prepareLogin(ctx, u, preferredTeamID)
	if err != nil {
		return LoginResult{}, err
	}
	*desktop = DesktopLogin{UserID: u.ID, Root: NewAccountRoot(), PreferredTeamID: lc.teamID}
	return LoginResult{}, nil
}

// RedeemDesktopLogin completes a desktop SSO login whose ticket the desktop
// redeemed: the account is asked about its standing as the login's lineage
// and generation name it — a disable, a pending-password status, a sign-out
// or a "sign out everywhere" landing since the IdP callback refuses (the
// generation the root carries is the one the callback read) — and ONLY then
// is the session minted. The refresh token exists from this call on and is
// handed to the caller; before it, nothing of this login rested in any
// store (#2635).
func (s *Service) RedeemDesktopLogin(ctx context.Context, d DesktopLogin, userAgent, ip string) (LoginResult, error) {
	if d.UserID == "" || d.Root.ID == "" {
		return LoginResult{}, errors.New("auth: a desktop login that names no account or no lineage")
	}
	u, err := s.standing(ctx, Identity{
		UserID:            d.UserID,
		Credential:        CredentialAccessToken,
		SessionLineage:    d.Root.ID,
		SessionGeneration: d.Root.Generation,
	})
	if err != nil {
		return LoginResult{}, err
	}
	lc, err := s.prepareLogin(ctx, u, d.PreferredTeamID)
	if err != nil {
		return LoginResult{}, err
	}
	if lc, err = s.signLogin(lc, d.Root); err != nil {
		return LoginResult{}, err
	}
	return s.commitLogin(ctx, lc, u, userAgent, ip)
}

// loginContext is everything a login answers with except its refresh
// token: the account, the team and org it lands in, its memberships, the
// lineage its session belongs to and, once signed (signLogin), the access
// token naming it.
type loginContext struct {
	user           identity.User
	orgID          string
	orgRole        identity.OrgRole
	teamID         string
	role           identity.Role
	memberships    []identity.Membership
	orgMemberships []identity.OrgMembership
	root           SessionRoot
	access         string
	accessExpires  time.Time
}

// prepareLogin builds a login's context from reads alone, so a caller can
// build it before it commits anything: a read that fails then fails the
// login with nothing written. A read that fails is returned, never taken for
// "no membership" or "no org". The context is signed by signLogin.
func (s *Service) prepareLogin(ctx context.Context, u identity.User, preferredTeamID string) (loginContext, error) {
	memberships, err := s.store.ListMembershipsByUser(ctx, u.ID)
	if err != nil {
		return loginContext{}, fmt.Errorf("auth: list team memberships for user %s: %w", u.ID, err)
	}
	teamID, role := s.pickActiveTeam(u, memberships)
	if preferredTeamID != "" {
		for _, m := range memberships {
			if m.TeamID == preferredTeamID {
				teamID, role = m.TeamID, m.Role
				break
			}
		}
	}
	orgID, orgRole, err := s.resolveOrgForTeam(ctx, u, teamID)
	if err != nil {
		return loginContext{}, err
	}
	orgMems, err := s.store.ListOrgMembershipsByUser(ctx, u.ID)
	if err != nil {
		return loginContext{}, fmt.Errorf("auth: list org memberships for user %s: %w", u.ID, err)
	}
	return loginContext{
		user:           u,
		orgID:          orgID,
		orgRole:        orgRole,
		teamID:         teamID,
		role:           role,
		memberships:    memberships,
		orgMemberships: orgMems,
	}, nil
}

// loginContextFromRows is prepareLogin for an account a registration just
// created, built from the rows the registration wrote — the account's only
// ones — with no read: memberships and orgMems are those rows, teamOrgID the
// parent org of the team the account lands in (the team row the caller
// read). A registration that committed then has nothing left to read.
func (s *Service) loginContextFromRows(u identity.User, memberships []identity.Membership, teamOrgID string, orgMems []identity.OrgMembership) loginContext {
	teamID, role := s.pickActiveTeam(u, memberships)
	var orgID string
	var orgRole identity.OrgRole
	if teamID != "" && teamOrgID != "" {
		orgID = teamOrgID
		for _, om := range orgMems {
			if om.OrgID == teamOrgID {
				orgRole = om.Role
			}
		}
	}
	return loginContext{
		user:           u,
		orgID:          orgID,
		orgRole:        orgRole,
		teamID:         teamID,
		role:           role,
		memberships:    memberships,
		orgMemberships: orgMems,
	}
}

// signLogin signs the access token of a resolved login context, naming the
// lineage its session belongs to — the root a login writes, or the lineage a
// refresh continues — so a switch can check the token against the
// revocations of that session.
func (s *Service) signLogin(lc loginContext, root SessionRoot) (loginContext, error) {
	access, exp, err := s.signer.IssueAccess(Identity{
		UserID:            lc.user.ID,
		Email:             lc.user.Email,
		OrgID:             lc.orgID,
		OrgRole:           lc.orgRole,
		TeamID:            lc.teamID,
		Role:              lc.role,
		IsSuperAdmin:      lc.user.IsSuperAdmin,
		SessionLineage:    root.ID,
		SessionGeneration: root.Generation,
	})
	if err != nil {
		return loginContext{}, err
	}
	lc.root = root
	lc.access, lc.accessExpires = access, exp
	return lc, nil
}

// result completes the context with the login's refresh session.
func (lc loginContext) result(refresh string, refreshExpires time.Time) LoginResult {
	return LoginResult{
		User:           lc.user,
		ActiveOrgID:    lc.orgID,
		ActiveOrgRole:  lc.orgRole,
		ActiveTeamID:   lc.teamID,
		ActiveRole:     lc.role,
		AccessToken:    lc.access,
		AccessExpires:  lc.accessExpires,
		RefreshToken:   refresh,
		RefreshExpires: refreshExpires,
		Memberships:    lc.memberships,
		OrgMemberships: lc.orgMemberships,
	}
}

// resolveOrgForTeam derives the active org context for a chosen team:
// the team's parent OrgID and the user's role in that org. A super-admin
// with no org-membership row is treated as an org admin (mirrors the
// team-level super-admin step-in). Returns empty strings when teamID is
// empty, when the store reports the team gone, or when the team has no
// parent org yet (pre-backfill row). A read that fails is returned: a
// session minted without its org would be a session the store never
// described.
func (s *Service) resolveOrgForTeam(ctx context.Context, u identity.User, teamID string) (orgID string, orgRole identity.OrgRole, err error) {
	if teamID == "" {
		return "", "", nil
	}
	t, err := s.store.GetTeam(ctx, teamID)
	if errors.Is(err, identity.ErrNotFound) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("auth: read team %s: %w", teamID, err)
	}
	if t.OrgID == "" {
		return "", "", nil
	}
	om, err := s.store.GetOrgMembership(ctx, u.ID, t.OrgID)
	switch {
	case err == nil:
		return t.OrgID, om.Role, nil
	case !errors.Is(err, identity.ErrNotFound):
		return "", "", fmt.Errorf("auth: read org membership of user %s in org %s: %w", u.ID, t.OrgID, err)
	case u.IsSuperAdmin:
		return t.OrgID, identity.OrgRoleAdmin, nil
	}
	return t.OrgID, "", nil
}

// orgAdminOf reports whether the user administers an org without needing a
// team grant inside it. A read that fails is returned, never taken for "not
// an admin".
func (s *Service) orgAdminOf(ctx context.Context, u identity.User, orgID string) (bool, error) {
	if orgID == "" {
		return false, nil
	}
	om, err := s.store.GetOrgMembership(ctx, u.ID, orgID)
	if errors.Is(err, identity.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: read org membership of user %s in org %s: %w", u.ID, orgID, err)
	}
	return om.Role.AtLeast(identity.OrgRoleAdmin), nil
}

// effectiveMembership resolves the team grant a principal acts under. A real
// membership row wins; without one, a super-admin steps into any team and an
// org admin/owner into any team of THEIR org.
//
// The org-admin arm is not a widening: canManageTeam/orgAdminOfTeam already
// let an org admin write to every team of their org, and buildOrgTree already
// offers those teams in the switcher with a synthesized admin role. What was
// missing was only the ACTIVE CONTEXT on the same team, so the switcher listed
// teams the switch then refused — a click that did nothing, with no message.
//
// Non-existence is never disclosed to a principal that has no claim on the
// team: an unauthorized caller gets ErrNotAMember whether or not the team
// exists, and only a super-admin can tell the two apart. A read that fails
// is returned as the failure it is, never taken for "not a member".
func (s *Service) effectiveMembership(ctx context.Context, u identity.User, teamID string) (identity.Membership, error) {
	mb, err := s.store.GetMembership(ctx, u.ID, teamID)
	if err == nil {
		return mb, nil
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return identity.Membership{}, err
	}
	team, err := s.store.GetTeam(ctx, teamID)
	if errors.Is(err, identity.ErrNotFound) {
		if u.IsSuperAdmin {
			return identity.Membership{}, ErrTeamNotFound
		}
		return identity.Membership{}, ErrNotAMember
	}
	if err != nil {
		return identity.Membership{}, fmt.Errorf("auth: read team %s: %w", teamID, err)
	}
	if !u.IsSuperAdmin {
		admin, err := s.orgAdminOf(ctx, u, team.OrgID)
		if err != nil {
			return identity.Membership{}, err
		}
		if !admin {
			return identity.Membership{}, ErrNotAMember
		}
	}
	return identity.Membership{UserID: u.ID, TeamID: team.ID, Role: identity.RoleAdmin}, nil
}

// pickActiveTeam picks the JWT-stamped team based on (1) the user's
// stored DefaultTeamID, (2) the first team they're a member of, or
// (3) empty (super-admin without memberships — UI lands them on
// /admin/teams to pick one).
func (s *Service) pickActiveTeam(u identity.User, ms []identity.Membership) (string, identity.Role) {
	if u.DefaultTeamID != "" {
		for _, m := range ms {
			if m.TeamID == u.DefaultTeamID {
				return m.TeamID, m.Role
			}
		}
	}
	if len(ms) > 0 {
		return ms[0].TeamID, ms[0].Role
	}
	return "", ""
}

// Refresh rotates a presented refresh token: it answers with a fresh access
// token and the token's successor, and retires the presented token.
//
// Every read the answer needs runs BEFORE the rotation, so a failed read
// leaves the presented token live and the client can present it again. The
// steps after the rotation are the successor's write, its check against the
// revocations that may cover it, and its claim (writeSuccessor,
// deliverSuccessor); a failure there leaves the successor unwritten or
// unclaimed — handed to nobody — which the next presentation of the token
// completes (refreshRetired). The refresh as a whole is bounded by
// RefreshTimeout, each of its session-store steps by sessionOpTimeout.
//
// A token a rotation retired, presented again, is answered from the record
// that rotation left on it — the successor derived again from the presented
// token (refreshSuccessor), never stored:
//
//   - its successor was handed out (claimed): the presentation receives it
//     while it is live and was never rotated itself, and only within the
//     reuse grace window (refreshReuseGrace) of the rotation — two tabs
//     refreshing at once, a client retrying an answer it lost, a refresh that
//     lost the rotation's compare-and-set. The window is measured on the
//     session store's clock (SessionStore.Now), the clock that stamped the
//     rotation.
//   - its successor was handed to nobody — never written, or written and
//     never claimed: the retired token is still the only credential of its
//     lineage, and its presentation writes or claims the successor and
//     receives it — within the completion horizon of the rotation
//     (completionHorizon, or the window when that is longer), on the store's
//     clock, and never past the token's own expiry. A store that failed a
//     step after the rotation, or a client that came back after the window,
//     then costs nothing; a token presented past the horizon is one its
//     holder abandoned, and its presentation is reuse.
//
// Everything else is reuse, and the token's lineage is revoked — every
// session rotated from the same login, whoever holds it — while the
// account's other lineages, its other devices, keep their sessions
// (reuseDetected): a handed-out successor's predecessor presented past the
// window, one handed to nobody past the completion horizon, a successor no
// longer live, and a session revoked by anything other than a rotation — a
// sign-out, "sign out everywhere", a password change — since those clear the
// rotation record and with it every successor the record could name. A zero
// window disables all of it: every presentation of a retired token is reuse.
//
// The window's price: a stolen refresh token replayed within the window of
// its rotation receives the same successor the legitimate holder received.
// Both then hold the same lineage, and the first presentation of a retired
// token of it outside its window, by either holder, revokes the lineage.
func (s *Service) Refresh(ctx context.Context, presented, userAgent, ip string) (LoginResult, error) {
	ctx, cancel := context.WithTimeout(ctx, RefreshTimeout)
	defer cancel()
	hash := HashRefreshToken(presented)
	prev, err := s.sessionByHash(ctx, hash)
	if err != nil {
		return LoginResult{}, err
	}
	now := s.now().UTC()
	if prev.RevokedAt != nil {
		return s.refreshRetired(ctx, prev, presented, now)
	}
	if !prev.ExpiresAt.IsZero() && now.After(prev.ExpiresAt) {
		return LoginResult{}, ErrSessionExpired
	}
	u, err := s.refreshAccount(ctx, prev.UserID)
	if err != nil {
		return LoginResult{}, err
	}
	if refusal := sessionRefusal(u); refusal != nil {
		s.retireRefusedSession(ctx, prev.ID, now)
		return LoginResult{}, refusal
	}
	lc, err := s.prepareLogin(ctx, u, "")
	if err != nil {
		return LoginResult{}, err
	}
	if lc, err = s.signLogin(lc, prev.root()); err != nil {
		return LoginResult{}, err
	}
	rot, successor, err := s.newRotation(presented, userAgent, ip)
	if err != nil {
		return LoginResult{}, err
	}
	// Rotate. The compare-and-set retires the session once: a refresh that
	// loses it finds the session retired and answers like any other
	// presentation of a retired token.
	octx, ocancel := context.WithTimeout(ctx, sessionOpTimeout)
	rotatedAt, rotated, err := s.sessions.RotateSession(octx, prev.ID, rot)
	ocancel()
	if err != nil {
		return LoginResult{}, err
	}
	if !rotated {
		cur, err := s.sessionByHash(ctx, hash)
		if err != nil {
			return LoginResult{}, err
		}
		if cur.RevokedAt == nil {
			return LoginResult{}, fmt.Errorf("auth: session %s: its rotation was refused, yet it reads as live", prev.ID)
		}
		return s.refreshRetired(ctx, cur, presented, now)
	}
	next, err := s.writeSuccessor(ctx, prev, successor, rotatedAt, rot)
	if err != nil {
		return LoginResult{}, err
	}
	return lc.result(successor, next.ExpiresAt), nil
}

// sessionByHash reads the session of a token hash, bounded by
// sessionOpTimeout.
func (s *Service) sessionByHash(ctx context.Context, hash string) (Session, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	defer cancel()
	return s.sessions.GetSessionByTokenHash(ctx, hash)
}

// completionHorizon bounds how long after its rotation a retired token whose
// successor was handed to nobody still completes it (refreshRetired). It
// covers the retries of every shipped client with margin — the desktop
// presents again at once after a refresh it abandoned at its 10 s deadline,
// then every 15 s; the studio after 2, 5 and 10 s. A token presented past it
// was abandoned by its holder, and its presentation is reuse. A reuse grace
// window longer than it extends it (Service.completionWithin).
const completionHorizon = 5 * time.Minute

// completionWithin is the horizon refreshRetired completes a successor
// handed to nobody within: completionHorizon, or the reuse grace window when
// that is longer.
func (s *Service) completionWithin() time.Duration {
	if s.refreshReuseGrace > completionHorizon {
		return s.refreshReuseGrace
	}
	return completionHorizon
}

// refreshRetired answers the presentation of a token whose session is
// already revoked (see Refresh): the successor of the rotation that retired
// it, or reuse.
func (s *Service) refreshRetired(ctx context.Context, prev Session, presented string, now time.Time) (LoginResult, error) {
	if s.refreshReuseGrace <= 0 {
		return LoginResult{}, s.reuseDetected(ctx, prev)
	}
	successor, ok := s.recordedSuccessor(prev, presented)
	if !ok {
		return LoginResult{}, s.reuseDetected(ctx, prev)
	}
	next, err := s.sessionByHash(ctx, HashRefreshToken(successor))
	written := err == nil
	if err != nil && !errors.Is(err, ErrSessionNotFound) {
		return LoginResult{}, err
	}
	if written && (next.UserID != prev.UserID || next.RotatedFromID != prev.ID) {
		return LoginResult{}, fmt.Errorf("auth: session %s: the successor its rotation derives is not its own", prev.ID)
	}
	switch {
	case written && next.DeliveredAt != nil:
		// Handed out: redelivered within the window only.
		within, err := s.retiredWithin(ctx, prev, s.refreshReuseGrace)
		if err != nil {
			return LoginResult{}, err
		}
		if !within || next.RevokedAt != nil || (!next.ExpiresAt.IsZero() && now.After(next.ExpiresAt)) {
			return LoginResult{}, s.reuseDetected(ctx, prev)
		}
	case written && next.RevokedAt != nil:
		// Handed to nobody, and revoked since it was written: its lineage
		// or its account was signed out.
		return LoginResult{}, s.reuseDetected(ctx, prev)
	default:
		// Handed to nobody — never written, or written and never claimed:
		// the retired token is still its lineage's only credential.
		if !prev.ExpiresAt.IsZero() && now.After(prev.ExpiresAt) {
			// The successor is answered as the token would be, and the
			// token is past its own lifetime.
			return LoginResult{}, ErrSessionExpired
		}
		within, err := s.retiredWithin(ctx, prev, s.completionWithin())
		if err != nil {
			return LoginResult{}, err
		}
		if !within {
			return LoginResult{}, s.reuseDetected(ctx, prev)
		}
		if !written {
			// A revocation recorded since the rotation covers the successor
			// it never wrote: refused before it is written.
			if err := s.refuseCovered(ctx, prev, now); err != nil {
				return LoginResult{}, err
			}
		}
	}
	u, err := s.refreshAccount(ctx, prev.UserID)
	if err != nil {
		return LoginResult{}, err
	}
	if refusal := sessionRefusal(u); refusal != nil {
		if written {
			s.retireRefusedSession(ctx, next.ID, now)
		} else {
			// Clearing the record takes the successor that was never
			// written: the account's return must not write it.
			s.retireRefusedSession(ctx, prev.ID, now)
		}
		return LoginResult{}, refusal
	}
	lc, err := s.prepareLogin(ctx, u, "")
	if err != nil {
		return LoginResult{}, err
	}
	if lc, err = s.signLogin(lc, prev.root()); err != nil {
		return LoginResult{}, err
	}
	if written {
		next, err = s.deliverSuccessor(ctx, next)
	} else {
		// The rotation landed but its successor's write did not: this
		// presentation completes it, as the client the rotation was for.
		next, err = s.writeSuccessor(ctx, prev, successor, *prev.RevokedAt, prev.rotation())
	}
	if err != nil {
		return LoginResult{}, err
	}
	return lc.result(successor, next.ExpiresAt), nil
}

// retiredWithin reports whether prev was retired at most d ago, measured on
// the session store's clock: the clock RotateSession stamped prev.RevokedAt
// with.
func (s *Service) retiredWithin(ctx context.Context, prev Session, d time.Duration) (bool, error) {
	if d <= 0 || prev.RevokedAt == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	defer cancel()
	storeNow, err := s.sessions.Now(ctx)
	if err != nil {
		return false, err
	}
	return !storeNow.After(prev.RevokedAt.Add(d)), nil
}

// recordedSuccessor derives again, from the presented token, the successor
// the rotation that retired prev recorded — and only that one: ok is false
// unless the derivation's hash is the successor hash the rotation wrote. A
// session revoked by anything other than a rotation (a sign-out, a
// revoke-all) carries no record; a rotation whose key is gone from the key
// set, or no longer the same secret, cannot be named again. Either way the
// presentation is what it would be without the window.
func (s *Service) recordedSuccessor(prev Session, presented string) (string, bool) {
	successor, ok := s.refreshSuccessor(presented, prev.rotation())
	if !ok || !hmac.Equal([]byte(HashRefreshToken(successor)), []byte(prev.SuccessorHash)) {
		return "", false
	}
	return successor, true
}

// revocationTimeout bounds one attempt of the detached pass of a revocation
// (runPass) over the sessions its record covers.
const revocationTimeout = 2 * sessionOpTimeout

// reuseDetected is the answer to a retired refresh token presented outside
// its window: possible theft, so the token's lineage is revoked — every
// session rotated from the same login, the thief's and the legitimate
// holder's alike, since either may hold it — and nothing else: the
// account's other lineages keep their sessions. A row last written before
// lineages were recorded names no family — no rotation of that version
// linked a session to its successor — so its reuse revokes the account, as
// reuse detection did before lineages.
//
// The revocation's record runs whether or not the presenter is still
// waiting, and the presentation is refused (ErrSessionRevoked) once it
// landed; its pass runs detached (runPass), and the record alone already
// leaves no session of the lineage that a refresh delivers
// (confirmSuccessor). A record that failed is answered as the failure it is:
// the presenter keeps the token and presents it again, which is reuse again
// and retries the revocation. The detection is a security event, logged with
// its outcome.
func (s *Service) reuseDetected(ctx context.Context, prev Session) error {
	if prev.LineageID == "" {
		if _, err := s.revokeAccount(ctx, prev.UserID); err != nil {
			if s.logger != nil {
				s.logger.Error("auth: refresh-token reuse: session %s of user %s, written before lineages, presented outside its window; revoking every session of the account failed: %v", prev.ID, prev.UserID, err)
			}
			return fmt.Errorf("auth: revoke every session of user %s after a refresh-token reuse: %w", prev.UserID, err)
		}
		if s.logger != nil {
			s.logger.Warn("auth: refresh-token reuse: session %s of user %s, written before lineages, presented outside its window; every session of the account revoked", prev.ID, prev.UserID)
		}
		s.removeReusedSSOLinks(ctx, prev.UserID, "")
		return ErrSessionRevoked
	}
	if err := s.revokeLineage(ctx, prev.UserID, prev.LineageID); err != nil {
		if s.logger != nil {
			s.logger.Error("auth: refresh-token reuse: session %s of user %s presented outside its window; revoking lineage %s failed: %v", prev.ID, prev.UserID, prev.LineageID, err)
		}
		return fmt.Errorf("auth: revoke lineage %s after a refresh-token reuse: %w", prev.LineageID, err)
	}
	if s.logger != nil {
		s.logger.Warn("auth: refresh-token reuse: session %s of user %s presented outside its window; lineage %s revoked", prev.ID, prev.UserID, prev.LineageID)
	}
	s.removeReusedSSOLinks(ctx, prev.UserID, prev.LineageID)
	return ErrSessionRevoked
}

// removeReusedSSOLinks removes, after a reuse revocation landed, the SSO links
// the revoked lineage wrote from the account settings — every link a session
// wrote when the reused row predates lineages, which revokes the account. It
// runs whether or not the presenter still waits, bounded by sessionOpTimeout;
// a removal that fails is logged as the error it is: the links stand until
// "sign out everywhere" removes them.
func (s *Service) removeReusedSSOLinks(ctx context.Context, userID, lineageID string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionOpTimeout)
	defer cancel()
	n, err := s.store.DeleteSessionOIDCLinks(rctx, userID, lineageID)
	if s.logger == nil {
		return
	}
	scope := "lineage " + lineageID
	if lineageID == "" {
		scope = "every session"
	}
	if err != nil {
		s.logger.Error("auth: refresh-token reuse: removing the SSO links %s of user %s wrote failed: %v (they stand until sign out everywhere removes them)", scope, userID, err)
		return
	}
	if n > 0 {
		s.logger.Warn("auth: refresh-token reuse: %d SSO link(s) %s of user %s wrote removed", n, scope, userID)
	}
}

// refuseCovered refuses a session a recorded revocation covers
// (SessionStore.SessionRevoked) and signs it out the way a sign-out would —
// its rotation record cleared, so the successor it names is never written:
// ErrSessionRevoked. A read that fails is returned.
func (s *Service) refuseCovered(ctx context.Context, prev Session, now time.Time) error {
	cctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	covered, err := s.sessions.SessionRevoked(cctx, prev)
	cancel()
	if err != nil {
		return fmt.Errorf("auth: check session %s against its revocations: %w", prev.ID, err)
	}
	if !covered {
		return nil
	}
	s.retireRefusedSession(ctx, prev.ID, now)
	return ErrSessionRevoked
}

// refreshAccount reads the account a refresh session belongs to. A session
// whose account no longer exists is a session the client can no longer
// use: ErrSessionNotFound, the answer a client reads as "sign in again".
func (s *Service) refreshAccount(ctx context.Context, userID string) (identity.User, error) {
	u, err := s.store.GetUser(ctx, userID)
	if errors.Is(err, identity.ErrNotFound) {
		return identity.User{}, fmt.Errorf("%w: its account no longer exists", ErrSessionNotFound)
	}
	if err != nil {
		return identity.User{}, fmt.Errorf("auth: read user %s: %w", userID, err)
	}
	return u, nil
}

// errPasswordChangePending refuses a refresh or a switch of an account an
// administrator reset or set pending a password change: its sessions end
// there, and its next sign-in goes through the forced change.
var errPasswordChangePending = fmt.Errorf("%w: the account must change its password", ErrSessionRevoked)

// sessionRefusal is the answer of a refresh and of a switch for an account no
// session may continue on — disabled, or pending a password change — and nil
// for an active one.
func sessionRefusal(u identity.User) error {
	switch u.Status {
	case identity.UserStatusDisabled:
		return ErrAccountDisabled
	case identity.UserStatusPendingPasswordChange:
		return errPasswordChangePending
	}
	return nil
}

// retireRefusedSession signs out one session of an account sessionRefusal
// refused, so the slot does not outlive the refusal — re-enabling the
// account, or the password change it waits for, must not resurrect it:
// revoked_at where it is absent, and the rotation record cleared, so a
// successor it names is never written. It runs whether or not the client
// still waits, bounded by sessionOpTimeout. A failed revoke is logged: the
// slot stays refused for as long as the account is.
func (s *Service) retireRefusedSession(ctx context.Context, sessionID string, now time.Time) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionOpTimeout)
	defer cancel()
	if _, err := s.sessions.RevokeSession(rctx, sessionID, now); err != nil && !errors.Is(err, ErrSessionNotFound) && s.logger != nil {
		s.logger.Error("auth: revoke session %s of a refused account: %v", sessionID, err)
	}
}

// refreshSuccessorLabel separates the successor-derivation key from every
// other use of the signing keys.
const refreshSuccessorLabel = "iterion/auth/refresh-successor/v1"

// newRotation draws a rotation record for the presented token and the
// successor it names: a fresh nonce, under the active signing key, for the
// client presenting it.
func (s *Service) newRotation(presented, userAgent, ip string) (Rotation, string, error) {
	nonce, _, err := GenerateRandomToken(16)
	if err != nil {
		return Rotation{}, "", fmt.Errorf("auth: gen rotation nonce: %w", err)
	}
	rot := Rotation{Nonce: nonce, KeyID: s.signer.ActiveKID(), UserAgent: userAgent, IP: ip}
	successor, ok := s.refreshSuccessor(presented, rot)
	if !ok {
		return Rotation{}, "", fmt.Errorf("auth: the signer's active key %q is not in its key set", rot.KeyID)
	}
	rot.SuccessorHash = HashRefreshToken(successor)
	return rot, successor, nil
}

// refreshSuccessor derives the successor a rotation hands out for the token
// it retires: HMAC-SHA512 over the rotation's nonce and the presented token,
// under a key derived from the server's signing key, truncated to a refresh
// token's length. It needs the presented token itself, which the store
// never holds (only its hash), and the server key, which the store never
// holds either: the rotation record alone names no token. ok is false when
// the rotation's key is no longer in the signer's key set.
func (s *Service) refreshSuccessor(presented string, rot Rotation) (string, bool) {
	key, ok := s.signer.derivedKey(rot.KeyID, refreshSuccessorLabel)
	if !ok {
		return "", false
	}
	mac := hmac.New(sha512.New, key)
	mac.Write([]byte(rot.Nonce))
	mac.Write([]byte{0})
	mac.Write([]byte(presented))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:refreshTokenBytes]), true
}

// writeSuccessor records the successor session of prev, the session the
// rotation rot retired at rotatedAt — the client rot was for, prev's lineage
// and generation, and a lifetime that starts at the rotation, wherever the
// write lands — then hands it out (deliverSuccessor). Another refresh of the
// same token derives the same successor and may have written it first; that
// row is then the one handed out.
//
// A write whose answer was lost may have landed: its row then stays, never
// claimed, and the next presentation of the token completes it as it would a
// successor never written. No refresh deletes a session it wrote, so no
// successor another presentation was handed can disappear under it.
func (s *Service) writeSuccessor(ctx context.Context, prev Session, successor string, rotatedAt time.Time, rot Rotation) (Session, error) {
	next := Session{
		ID:            uuid.NewString(),
		UserID:        prev.UserID,
		TokenHash:     HashRefreshToken(successor),
		UserAgent:     rot.UserAgent,
		IP:            rot.IP,
		IssuedAt:      rotatedAt,
		ExpiresAt:     rotatedAt.Add(s.refreshTTL),
		RotatedFromID: prev.ID,
		LineageID:     prev.lineage(),
		Generation:    prev.Generation,
	}
	wctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	err := s.sessions.CreateSession(wctx, next)
	cancel()
	if errors.Is(err, ErrSessionExists) {
		stored, gerr := s.sessionByHash(ctx, next.TokenHash)
		if gerr != nil {
			return Session{}, gerr
		}
		if stored.UserID != prev.UserID || stored.RotatedFromID != prev.ID {
			return Session{}, fmt.Errorf("auth: session %s: the successor its rotation derives is not its own", prev.ID)
		}
		if stored.RevokedAt != nil {
			return Session{}, ErrSessionRevoked
		}
		return s.deliverSuccessor(ctx, stored)
	}
	if err != nil {
		return Session{}, fmt.Errorf("auth: create session: %w", err)
	}
	return s.deliverSuccessor(ctx, next)
}

// deliverSuccessor hands a written successor out: it checks it against the
// revocations that may cover it (confirmSuccessor), claims it
// (SessionStore.ClaimSession), then checks it again — the last step before
// the refresh answers. A revocation recorded between the first check and the
// claim is caught by the second: its pass runs detached, so the claim alone
// cannot tell. A revocation recorded after the second check is ordered after
// this delivery, and its pass revokes the successor it handed out. A claim
// matches nothing once a pass revoked the successor: the presentation is
// refused. A step that fails leaves the successor as it was — unclaimed, or
// claimed — and the refresh answers the failure.
func (s *Service) deliverSuccessor(ctx context.Context, next Session) (Session, error) {
	if err := s.confirmSuccessor(ctx, next); err != nil {
		return Session{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	claimed, err := s.sessions.ClaimSession(cctx, next.ID)
	cancel()
	if err != nil {
		return Session{}, fmt.Errorf("auth: claim the successor %s: %w", next.ID, err)
	}
	if !claimed {
		return Session{}, ErrSessionRevoked
	}
	if err := s.confirmSuccessor(ctx, next); err != nil {
		return Session{}, err
	}
	return next, nil
}

// confirmSuccessor is the check every successor passes before it is
// delivered, once its row exists: a revocation of its lineage or of its
// account covers it, even one whose pass over the sessions ran before the
// row was written and so never saw it (SessionStore.SessionRevoked). A
// covered successor is retired here and the presentation refused. A
// revocation records itself before its pass, so a successor whose check
// finds no record was written before the record, and the pass sees it:
// no interleaving delivers a session of a revoked family that stays live.
func (s *Service) confirmSuccessor(ctx context.Context, next Session) error {
	cctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	revoked, err := s.sessions.SessionRevoked(cctx, next)
	cancel()
	if err != nil {
		return fmt.Errorf("auth: check session %s against its revocations: %w", next.ID, err)
	}
	if !revoked {
		return nil
	}
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), sessionOpTimeout)
	defer rcancel()
	if _, err := s.sessions.RevokeSessionIfNotRevoked(rctx, next.ID, s.now().UTC()); err != nil && s.logger != nil {
		s.logger.Error("auth: retire the session %s a revocation covers: %v", next.ID, err)
	}
	return ErrSessionRevoked
}

// RevokeUserSessions invalidates every refresh session of the user, on
// every device: it raises the account's session generation — the
// revocation's commit point, the one write it waits for — so every session
// born before it is refused at its next refresh (confirmSuccessor) and every
// access token minted before it at its next derivation (CheckDerivation),
// and a session written while it runs is covered too. The pass that marks
// the live rows revoked runs detached (runPass). Used by "sign out
// everywhere", a credential change and the admin disable. An access token of
// the account keeps the access it grants until its own expiry (at most the
// access TTL); it mints nothing from then on. A record that fails is
// returned, for the caller to answer as the failure it is: nothing was
// revoked.
func (s *Service) RevokeUserSessions(ctx context.Context, userID string) error {
	_, err := s.revokeAccount(ctx, userID)
	return err
}

// Logout signs out the lineage of the presented refresh token (LogoutOf, for
// any lineage).
func (s *Service) Logout(ctx context.Context, presented string) error {
	_, err := s.LogoutOf(ctx, presented, "")
	return err
}

// LogoutOf signs out the lineage of the presented refresh token: every
// session rotated from the same login — the token, its predecessors and
// successors, and the successor a rotation of it would still write — and
// nothing else: other devices (other logins of the same user) keep their
// sessions. The lineage's record is the sign-out's commit point, the one
// write it waits for; the pass over its live rows runs detached (runPass). A
// sign-out never moves the instant a session was retired, so a retired token
// presented after it is reuse as before it, and finds its lineage revoked.
//
// lineage, when not empty, names the lineage the caller means to sign out: a
// token of another lineage signs nothing out, and signedOut is false — the
// client holds a session opened since the one it meant. A token the store
// does not know signs nothing out either, and signedOut is true: there is no
// session of it to end. A read or a record that fails is returned: the
// lineage may still be live, and the sign-out is not done.
func (s *Service) LogoutOf(ctx context.Context, presented, lineage string) (signedOut bool, err error) {
	sess, err := s.sessionByHash(ctx, HashRefreshToken(presented))
	if errors.Is(err, ErrSessionNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: sign out: read the session of the token: %w", err)
	}
	if lineage != "" && sess.lineage() != lineage {
		return false, nil
	}
	if err := s.revokeLineage(ctx, sess.UserID, sess.lineage()); err != nil {
		return false, fmt.Errorf("auth: sign out the lineage of session %s: %w", sess.ID, err)
	}
	return true, nil
}

// SwitchResult is a switch's answer: the identity the new access token
// carries, the token, and the account it was minted for — the row the switch
// wrote its defaults to, or the one it read when that write failed.
type SwitchResult struct {
	Identity      Identity
	User          identity.User
	AccessToken   string
	AccessExpires time.Time
}

// SwitchTeam re-issues the presented access token — the identity requireAuth
// read from it — bound to teamID. Validates that the user may act as the
// team — a membership row, or the step-in of a super-admin / an admin of the
// team's parent org (see effectiveMembership). A team grant implies an org
// membership, so the team check is sufficient.
//
// A switch extends nothing: the token it mints names the presented token's
// session and expires when the presented token does (IssueAccessUntil), and
// it refuses what CheckDerivation refuses — a revoked session, a disabled or
// pending account, and any credential but an access token (a personal access
// token is pinned to its team: 403).
//
// Every read runs before the switch persists the choice as the user's
// default, and nothing is read after it: a failed read changes nothing, and
// a switch that persisted its default answers with it.
func (s *Service) SwitchTeam(ctx context.Context, presented Identity, teamID string) (SwitchResult, error) {
	d, err := s.CheckDerivation(ctx, presented, MintAccessToken)
	if err != nil {
		return SwitchResult{}, err
	}
	u := d.User
	mb, err := s.effectiveMembership(ctx, u, teamID)
	if err != nil {
		return SwitchResult{}, err
	}
	orgID, orgRole, err := s.resolveOrgForTeam(ctx, u, mb.TeamID)
	if err != nil {
		return SwitchResult{}, err
	}
	id := Identity{
		UserID:            u.ID,
		Email:             u.Email,
		OrgID:             orgID,
		OrgRole:           orgRole,
		TeamID:            mb.TeamID,
		Role:              mb.Role,
		IsSuperAdmin:      u.IsSuperAdmin,
		SessionLineage:    presented.SessionLineage,
		SessionGeneration: presented.SessionGeneration,
	}
	tok, exp, err := s.signer.IssueAccessUntil(id, d.NotAfter)
	if err != nil {
		return SwitchResult{}, err
	}
	// Persist the choice as the user's new default (team + org) — and only
	// that: a whole-row write would carry the IsSuperAdmin and PasswordHash
	// this read predates, re-granting a privilege an admin revoked in
	// between. The JWT is already issued and carries the chosen context — a
	// persistence failure here means the next session won't auto-resume, but
	// the current login is still good. Log so an operator can investigate.
	patch := identity.UserPatch{DefaultTeamID: &teamID}
	if orgID != "" {
		patch.DefaultOrgID = &orgID
	}
	return SwitchResult{Identity: id, User: s.persistDefaults(ctx, u, patch, "team"), AccessToken: tok, AccessExpires: exp}, nil
}

// persistDefaults writes a switch's chosen defaults and returns the stored
// row, or u unchanged when the write failed (logged — see SwitchTeam).
func (s *Service) persistDefaults(ctx context.Context, u identity.User, patch identity.UserPatch, what string) identity.User {
	patched, err := s.store.PatchUser(ctx, u.ID, patch)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("auth: persist default %s for user %s failed: %v", what, u.ID, err)
		}
		return u
	}
	return patched
}

// SwitchOrg re-issues the presented access token bound to a different org.
// It picks an active team within that org (the user's default team if it
// belongs to the org, else the first team they're granted there) so the
// session always lands on a concrete workspace. A user with no team in the
// org still switches — they land org-scoped with an empty active team (the
// UI offers to create one). Validates org membership (super-admins step
// into any org). Same rules as SwitchTeam: it extends nothing and refuses
// what CheckDerivation refuses, and every read runs before the default is
// persisted, none after.
func (s *Service) SwitchOrg(ctx context.Context, presented Identity, orgID string) (SwitchResult, error) {
	d, err := s.CheckDerivation(ctx, presented, MintAccessToken)
	if err != nil {
		return SwitchResult{}, err
	}
	u := d.User
	om, err := s.store.GetOrgMembership(ctx, u.ID, orgID)
	orgRole := om.Role
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			if !u.IsSuperAdmin {
				return SwitchResult{}, ErrNotAnOrgMember
			}
			if _, oerr := s.store.GetOrg(ctx, orgID); errors.Is(oerr, identity.ErrNotFound) {
				return SwitchResult{}, ErrOrgNotFound
			} else if oerr != nil {
				return SwitchResult{}, fmt.Errorf("auth: read org %s: %w", orgID, oerr)
			}
			orgRole = identity.OrgRoleAdmin
		} else {
			return SwitchResult{}, err
		}
	}
	// Pick a team in the org the user is granted (or any team, for a
	// principal that administers them all), preferring their stored default.
	administers := u.IsSuperAdmin || orgRole.AtLeast(identity.OrgRoleAdmin)
	teamID, teamRole, err := s.pickActiveTeamInOrg(ctx, u, orgID, administers)
	if err != nil {
		return SwitchResult{}, err
	}
	id := Identity{
		UserID:            u.ID,
		Email:             u.Email,
		OrgID:             orgID,
		OrgRole:           orgRole,
		TeamID:            teamID,
		Role:              teamRole,
		IsSuperAdmin:      u.IsSuperAdmin,
		SessionLineage:    presented.SessionLineage,
		SessionGeneration: presented.SessionGeneration,
	}
	tok, exp, err := s.signer.IssueAccessUntil(id, d.NotAfter)
	if err != nil {
		return SwitchResult{}, err
	}
	// The chosen defaults, and nothing else — see SwitchTeam.
	patch := identity.UserPatch{DefaultOrgID: &orgID}
	if teamID != "" {
		patch.DefaultTeamID = &teamID
	}
	return SwitchResult{Identity: id, User: s.persistDefaults(ctx, u, patch, "org"), AccessToken: tok, AccessExpires: exp}, nil
}

// pickActiveTeamInOrg selects a team for the user within one org: their
// stored DefaultTeamID if it belongs to the org and they're a member,
// else the first team they're granted in the org. Returns ("","") when
// the user holds no team in the org — except for a principal that
// administers them all (administers: a super-admin, or an admin/owner of
// THIS org, as the caller read it), which falls back to the org's first
// team so it always lands somewhere. A read that fails is returned, never
// taken for "no team here": landing the session on an empty workspace
// would state a fact the store did not.
func (s *Service) pickActiveTeamInOrg(ctx context.Context, u identity.User, orgID string, administers bool) (string, identity.Role, error) {
	memberships, err := s.store.ListMembershipsByUser(ctx, u.ID)
	if err != nil {
		return "", "", fmt.Errorf("auth: list team memberships for user %s: %w", u.ID, err)
	}
	// One read for the org's teams, then intersect with the user's grants —
	// instead of a GetTeam per membership.
	teams, err := s.store.ListTeamsByOrg(ctx, orgID)
	if err != nil {
		return "", "", fmt.Errorf("auth: list teams of org %s: %w", orgID, err)
	}
	inOrg := make(map[string]bool, len(teams))
	for _, t := range teams {
		inOrg[t.ID] = true
	}
	var first *identity.Membership
	for i := range memberships {
		if !inOrg[memberships[i].TeamID] {
			continue
		}
		if memberships[i].TeamID == u.DefaultTeamID {
			return memberships[i].TeamID, memberships[i].Role, nil
		}
		if first == nil {
			first = &memberships[i]
		}
	}
	if first != nil {
		return first.TeamID, first.Role, nil
	}
	// Same rule as effectiveMembership, applied when no team was named: a
	// principal that administers every team of the org must land on one of
	// them rather than on an empty workspace it cannot leave.
	if len(teams) > 0 && administers {
		return teams[0].ID, identity.RoleAdmin, nil
	}
	return "", "", nil
}

// Register creates a new user. When SignupMode is invite_only, an
// invitation token must be supplied and matching the email.
func (s *Service) Register(ctx context.Context, email, password, name, invitationToken, userAgent, ip string) (LoginResult, error) {
	email = identity.NormalizeEmail(email)
	if email == "" {
		return LoginResult{}, ErrInvalidCredentials
	}
	if len(password) < MinPasswordLen {
		return LoginResult{}, ErrPasswordWeak
	}
	switch s.signupMode {
	case SignupOpen:
		return s.registerOpen(ctx, email, password, name, userAgent, ip)
	case SignupInviteOnly:
		if invitationToken == "" {
			return LoginResult{}, ErrSignupClosed
		}
		return s.registerWithInvitation(ctx, email, password, name, invitationToken, userAgent, ip)
	}
	return LoginResult{}, ErrSignupClosed
}

func (s *Service) registerOpen(ctx context.Context, email, password, name, userAgent, ip string) (LoginResult, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return LoginResult{}, err
	}
	now := s.now().UTC()
	u := identity.User{
		ID:           uuid.NewString(),
		Email:        email,
		Name:         name,
		PasswordHash: hash,
		Status:       identity.UserStatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if u, err = s.store.CreateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	// Auto-create a personal org + team so the user has somewhere to land.
	space, err := s.createPersonalTeam(ctx, u)
	if err != nil {
		return LoginResult{}, err
	}
	if u, err = s.store.PatchUser(ctx, u.ID, identity.UserPatch{
		DefaultOrgID: &space.orgID, DefaultTeamID: &space.teamID,
	}); err != nil {
		return LoginResult{}, err
	}
	lc, err := s.signLogin(s.loginContextFromRows(u, []identity.Membership{space.membership}, space.orgID, []identity.OrgMembership{space.orgMembership}), NewAccountRoot())
	if err != nil {
		return LoginResult{}, err
	}
	return s.commitLogin(ctx, lc, u, userAgent, ip)
}

func (s *Service) registerWithInvitation(ctx context.Context, email, password, name, token, userAgent, ip string) (LoginResult, error) {
	inv, err := s.store.GetInvitationByTokenHash(ctx, hashInvitationToken(token))
	if errors.Is(err, identity.ErrNotFound) {
		return LoginResult{}, ErrInvitationNotFound
	}
	if err != nil {
		return LoginResult{}, err
	}
	if inv.AcceptedAt != nil {
		return LoginResult{}, identity.ErrInvitationUsed
	}
	if !inv.ExpiresAt.IsZero() && s.now().After(inv.ExpiresAt) {
		return LoginResult{}, identity.ErrInvitationExpired
	}
	if !inv.AddressedTo(email) {
		return LoginResult{}, ErrInvitationMismatch
	}
	hash, err := HashPassword(password)
	if err != nil {
		return LoginResult{}, err
	}
	now := s.now().UTC()
	u := identity.User{
		ID:           uuid.NewString(),
		Email:        email,
		Name:         name,
		PasswordHash: hash,
		Status:       identity.UserStatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	// Every read the registration needs runs before the account is written:
	// a read that fails leaves no account behind and the invitation unspent.
	grant, err := s.planOrgGrantForTeam(ctx, u.ID, inv.TeamID, inv.Role, now)
	if err != nil {
		return LoginResult{}, err
	}
	if u, err = s.store.CreateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	// The invitation is CLAIMED before either access row: it is this
	// accept's authority and the one write a concurrent off-boarding can
	// refuse (claimInvitation).
	if err := s.claimInvitation(ctx, inv, u.ID, now); err != nil {
		return LoginResult{}, err
	}
	// The org membership the team grant is mirrored up to is written FIRST,
	// as the SSO path does (grantMembership): the two writes share no
	// transaction, and a grant left standing by a failure between them is
	// live access to a team of an org the holder is not a member of — the
	// orphan GET /api/admin/users/{id} reports and the server's own
	// compensations exist to prevent. The other order leaves an org member
	// with no team: org-scoped READ — the roster, the org's settings, its
	// usage and its team and SSO-provider lists — and no team access, which
	// is the lesser of the two states and why this order is the chosen one.
	if err := s.applyOrgGrant(ctx, grant); err != nil {
		return LoginResult{}, err
	}
	mb := identity.Membership{
		UserID:    u.ID,
		TeamID:    inv.TeamID,
		Role:      inv.Role,
		InvitedBy: inv.InvitedBy,
		JoinedAt:  now,
	}
	if err := s.store.UpsertMembership(ctx, mb); err != nil {
		return LoginResult{}, err
	}
	patch := identity.UserPatch{DefaultTeamID: &inv.TeamID}
	if grant.orgID != "" {
		patch.DefaultOrgID = &grant.orgID
	}
	if u, err = s.store.PatchUser(ctx, u.ID, patch); err != nil {
		return LoginResult{}, err
	}
	var orgMems []identity.OrgMembership
	if grant.write != nil {
		orgMems = append(orgMems, *grant.write)
	}
	lc, err := s.signLogin(s.loginContextFromRows(u, []identity.Membership{mb}, grant.orgID, orgMems), NewAccountRoot())
	if err != nil {
		return LoginResult{}, err
	}
	return s.commitLogin(ctx, lc, u, userAgent, ip)
}

// claimInvitation marks inv accepted by userID and is the first ACCESS
// write of both accept paths — registerWithInvitation creates the account
// row before it, which is not access and cannot be refused by an
// off-boarding. UpdateInvitation is conditional on the row in both stores —
// ErrNotFound once it is gone — so a token whose invitation an org
// off-boarding has already revoked (pkg/server revokeOrgTeamAccess) writes
// no access at all. Claiming LAST instead let the two access writes of an
// accept that had merely READ the row land behind the sweep that was
// revoking it, walking a removed account back into the org and the team
// under a 204, with the accept itself answering that it had failed.
//
// The order has a price, and it is the fail-closed one: a store error after
// the claim burns the token, since the row IS the claim. The invitee asks
// for another invitation, where claiming last answered a retryable error
// and risked the access instead. Un-claiming would be worse than the price:
// UpdateInvitation is an unconditional replace, so re-opening a row an
// off-boarding sweep has already skipped as accepted hands a removed
// account a live re-entry ticket.
//
// The state that price leaves is worth naming, because both sides are told
// something true and neither is told this: the row says "accepted by this
// account", the admin's invitation list shows it accepted, and the account
// holds no grant at all — the writes after the claim are what it was
// accepted FOR. The same link then answers identity.ErrInvitationUsed
// ("invitation already accepted", 409), so the invitee cannot retry their
// way in; a NEW invitation is the recovery. Witness:
// TestAcceptInvitationForExistingUser_AFailedReadAfterTheClaimBurnsTheLink.
//
// On the registration path that price includes the account row already
// written: once the claim has landed, the retry answers
// identity.ErrInvitationUsed, and only a registration that failed BEFORE
// the claim answers identity.ErrEmailAlreadyTaken. Either way the invitee
// recovers by logging in with the password they just set and accepting an
// invitation as an existing user, not by registering again.
func (s *Service) claimInvitation(ctx context.Context, inv identity.Invitation, userID string, at time.Time) error {
	acceptedAt := at
	inv.AcceptedAt = &acceptedAt
	inv.AcceptedBy = userID
	if err := s.store.UpdateInvitation(ctx, inv); err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return fmt.Errorf("%w: it was revoked while it was being accepted", ErrInvitationNotFound)
		}
		return err
	}
	return nil
}

// grantOrgMembershipForTeam ensures the user holds an org membership in
// the parent org of teamID, with at least the org-role implied by their
// team role (never downgrades an existing higher org-role). Returns the
// resolved orgID (empty if the team has no parent org yet).
func (s *Service) grantOrgMembershipForTeam(ctx context.Context, userID, teamID string, teamRole identity.Role, now time.Time) (string, error) {
	grant, err := s.planOrgGrantForTeam(ctx, userID, teamID, teamRole, now)
	if err != nil {
		return "", err
	}
	if err := s.applyOrgGrant(ctx, grant); err != nil {
		return "", err
	}
	return grant.orgID, nil
}

// orgGrant is the org membership a team grant is mirrored up to, decided
// from reads before anything is written: orgID is the team's parent org
// (empty when it has none), write the membership row to write there — nil
// when there is nothing to write.
type orgGrant struct {
	orgID string
	write *identity.OrgMembership
}

// planOrgGrantForTeam reads what mirroring a team grant in teamID up to its
// parent org takes. An existing org role at least the one the team role
// implies is kept, never downgraded — so a read of it that FAILS is returned,
// never taken for "no membership": writing over a role the read could not
// see would demote it.
func (s *Service) planOrgGrantForTeam(ctx context.Context, userID, teamID string, teamRole identity.Role, now time.Time) (orgGrant, error) {
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
		// membership — never mirror it up (an org-member row would pass
		// canViewOrg → org roster/usage read). Login still resolves the org
		// context with an empty OrgRole, which the org gates reject. ADR-078.
		return orgGrant{orgID: t.OrgID}, nil
	}
	existing, err := s.store.GetOrgMembership(ctx, userID, t.OrgID)
	switch {
	case err == nil && existing.Role.AtLeast(want):
		return orgGrant{orgID: t.OrgID}, nil
	case err != nil && !errors.Is(err, identity.ErrNotFound):
		return orgGrant{}, fmt.Errorf("auth: read org membership of user %s in org %s: %w", userID, t.OrgID, err)
	}
	return orgGrant{orgID: t.OrgID, write: &identity.OrgMembership{
		UserID:   userID,
		OrgID:    t.OrgID,
		Role:     want,
		JoinedAt: now,
	}}, nil
}

// applyOrgGrant writes the org membership planOrgGrantForTeam decided.
func (s *Service) applyOrgGrant(ctx context.Context, grant orgGrant) error {
	if grant.write == nil {
		return nil
	}
	return s.store.UpsertOrgMembership(ctx, *grant.write)
}

// AcceptInvitationForExistingUser is the path used when an invited
// email already corresponds to a registered user — they accept by
// adding a membership.
//
// It reads the org boundary TWICE: once before the claim, so a read that
// fails leaves the invitation unspent and every row as it was, and once
// after it, so an off-boarding landing in between finds the org membership
// written back with the grant rather than a grant left outside its org. The
// second read is therefore past the point of no return: a failure there
// answers an error with the invitation SPENT and nothing granted — the row
// reads "accepted by this account" to an admin while the account holds
// nothing, and only a new invitation gets the user in (the price
// claimInvitation documents, witnessed by
// TestAcceptInvitationForExistingUser_AFailedReadAfterTheClaimBurnsTheLink).
func (s *Service) AcceptInvitationForExistingUser(ctx context.Context, userID, token string) (identity.Membership, error) {
	inv, err := s.store.GetInvitationByTokenHash(ctx, hashInvitationToken(token))
	if errors.Is(err, identity.ErrNotFound) {
		return identity.Membership{}, ErrInvitationNotFound
	}
	if err != nil {
		return identity.Membership{}, err
	}
	if inv.AcceptedAt != nil {
		return identity.Membership{}, identity.ErrInvitationUsed
	}
	if !inv.ExpiresAt.IsZero() && s.now().After(inv.ExpiresAt) {
		return identity.Membership{}, identity.ErrInvitationExpired
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return identity.Membership{}, err
	}
	if !inv.AddressedTo(u.Email) {
		return identity.Membership{}, ErrInvitationMismatch
	}
	now := s.now().UTC()
	mb := identity.Membership{
		UserID:    u.ID,
		TeamID:    inv.TeamID,
		Role:      inv.Role,
		InvitedBy: inv.InvitedBy,
		JoinedAt:  now,
	}
	// A lowering write over the acceptee's own authority: an existing grant
	// in this team at a role ABOVE the one the invitation carries is demoted
	// by this accept, so the rows they minted above what it leaves them are
	// re-measured through the seam the server registers, as every lowering
	// write does. The read that detects it comes before the claim and the
	// upsert; the reconcile runs after the grant is written, on the value it
	// left. A raise landing between that read and the write is a lowering it
	// does not see — the read-once window the roster writes state. A service
	// with no reconcile registered refuses a lowering accept
	// before any write, as reconcileGitHubGrants refuses a revoke with no
	// sweep: a lowering nobody re-measures is the state this exists to
	// prevent.
	lowering := false
	existing, err := s.store.GetMembership(ctx, u.ID, inv.TeamID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		// A first grant — the accept creates it, it lowers nothing.
	case err != nil:
		return identity.Membership{}, err
	case existing.Role != "" && existing.Role != identity.RoleConfigEditor && !inv.Role.AtLeast(existing.Role):
		// The acceptee holds a LADDER grant and the role written does not
		// cover it — the rank falls, off the ladder (config_editor) or to a
		// lower rung. The sibling cap's own exemptions carry over: a
		// config_editor grant is off the ladder (ADR-078) and is no rank to
		// fall FROM, whatever is written over it.
		lowering = true
	}
	if lowering && s.teamRankReconcile == nil {
		return identity.Membership{}, fmt.Errorf("auth: no team-rank reconcile is registered, so accepting this invitation would lower the %s grant of %s in %s and leave the invitations they minted above it standing (SetTeamRankReconcile)",
			existing.Role, u.ID, inv.TeamID)
	}
	// The grant is planned once BEFORE the first write — a read that fails
	// here leaves the invitation unspent and every row as it was — and once
	// again after the claim, below, which is where the boundary is decided
	// from (see this function's own doc for the price of that second read).
	if _, err := s.planOrgGrantForTeam(ctx, u.ID, inv.TeamID, inv.Role, now); err != nil {
		return identity.Membership{}, err
	}
	// The invitation is CLAIMED before either access row: it is this
	// accept's authority and the one write a concurrent off-boarding can
	// refuse (claimInvitation).
	if err := s.claimInvitation(ctx, inv, u.ID, now); err != nil {
		return identity.Membership{}, err
	}
	// The boundary before the grant, as registerWithInvitation and the SSO
	// path (grantMembership) do — decided from a read AFTER the claim, so an
	// off-boarding that removed the membership in between finds it written
	// back with the grant, never a grant left outside its org. A read that
	// fails here fails the accept with the invitation spent, the price
	// claimInvitation names.
	grant, err := s.planOrgGrantForTeam(ctx, u.ID, inv.TeamID, inv.Role, now)
	if err != nil {
		return identity.Membership{}, err
	}
	if err := s.applyOrgGrant(ctx, grant); err != nil {
		return identity.Membership{}, err
	}
	if err := s.store.UpsertMembership(ctx, mb); err != nil {
		return identity.Membership{}, err
	}
	if lowering {
		// The demotion is the accept's own act, so its reconcile measures the
		// value it LEFT (inv.Role) — the same posture as the HTTP writes,
		// whose sweeps decide from the rows they just wrote — and a reconcile
		// that fails fails the accept: the rows it could not take are the
		// invitations the lower grant no longer covers, reported rather than
		// left behind an answer that looks clean.
		if err := s.teamRankReconcile(ctx, u.ID, inv.TeamID, inv.Role); err != nil {
			return identity.Membership{}, fmt.Errorf("auth: accepting this invitation lowered the team role of %s in %s to %s, but taking the invitations they minted above it failed: %w",
				u.ID, inv.TeamID, inv.Role, err)
		}
	}
	return mb, nil
}

// Store returns the underlying identity store. Server handlers
// reach for it to perform read-only joins (e.g. resolving team
// names from membership rows) without proxying every accessor
// through the Service.
func (s *Service) Store() identity.Store { return s.store }

// SetStoreForTest swaps the identity store. It exists for ONE thing a test
// cannot get otherwise: wrapping the store to open a read-modify-write
// window deterministically (a concurrent mutation landing between a
// handler's read and its write). Production wiring passes the store at
// construction and never calls this.
func (s *Service) SetStoreForTest(st identity.Store) { s.store = st }

// provisionUndoTimeout bounds a compensation whose request context may
// already be done — the provisioning often failed BECAUSE the caller went
// away — while keeping it from running unbounded (the shape revokeAccount
// uses for its detached record).
const provisionUndoTimeout = sessionOpTimeout

// provisionUndo holds the undo of every write a provisioning COMMITTED, in
// the order it committed them. compensate runs them newest-first, so a
// provisioning that cannot finish leaves the state it started from: the slug
// free, no half-provisioned row for the retry to collide with. A step is
// pushed only once its write answered.
type provisionUndo struct {
	steps []provisionUndoStep
}

type provisionUndoStep struct {
	what string
	undo func(context.Context) error
}

func (p *provisionUndo) push(what string, undo func(context.Context) error) {
	p.steps = append(p.steps, provisionUndoStep{what: what, undo: undo})
}

// compensate removes the committed writes and answers cause — the failure
// that stopped the provisioning, which is what the caller asked about —
// naming any row the undo could not remove: the operator needs both facts,
// because that row is what a retry will collide with. Detached from the
// caller's cancellation and bounded.
func (p *provisionUndo) compensate(ctx context.Context, cause error) error {
	if len(p.steps) == 0 {
		return cause
	}
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), provisionUndoTimeout)
	defer cancel()
	var left []string
	for i := len(p.steps) - 1; i >= 0; i-- {
		// A row already gone is the state the undo wanted.
		if err := p.steps[i].undo(uctx); err != nil && !errors.Is(err, identity.ErrNotFound) {
			left = append(left, fmt.Sprintf("%s (%v)", p.steps[i].what, err))
		}
	}
	if len(left) == 0 {
		return cause
	}
	return fmt.Errorf("%w — and the compensation could not remove %s, so a retry will answer a duplicate",
		cause, strings.Join(left, ", "))
}

// CreateTeamFor provisions a non-personal team inside org `orgID`,
// owned by user `userID`. The user must be a member of the org (super-
// admins may create in any org). Returns the new team. If slug is empty
// it is derived from name and uniquified with a numeric suffix on
// collision.
func (s *Service) CreateTeamFor(ctx context.Context, userID, orgID, name, slug string) (identity.Team, error) {
	if name == "" {
		return identity.Team{}, fmt.Errorf("auth: team name required")
	}
	if orgID == "" {
		return identity.Team{}, fmt.Errorf("auth: org id required")
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return identity.Team{}, err
	}
	if _, err := s.store.GetOrg(ctx, orgID); errors.Is(err, identity.ErrNotFound) {
		return identity.Team{}, ErrOrgNotFound
	} else if err != nil {
		return identity.Team{}, fmt.Errorf("auth: read org %s: %w", orgID, err)
	}
	if _, err := s.store.GetOrgMembership(ctx, userID, orgID); err != nil {
		if errors.Is(err, identity.ErrNotFound) && !u.IsSuperAdmin {
			return identity.Team{}, ErrNotAnOrgMember
		}
		if !errors.Is(err, identity.ErrNotFound) {
			return identity.Team{}, err
		}
	}
	undo := &provisionUndo{}
	t, err := s.createTeamIn(ctx, u, orgID, name, slug, s.now().UTC(), undo)
	if err != nil {
		return identity.Team{}, undo.compensate(ctx, err)
	}
	return t, nil
}

// createTeamIn is CreateTeamFor's WRITE phase: it reads nothing, so a caller
// that has already read (or just written) the rows the authorization needs
// runs no read after its own first irreversible write. It pushes the undo of
// each write it commits onto undo and leaves compensating to the caller —
// one decision, at the call that owns the whole unit.
func (s *Service) createTeamIn(ctx context.Context, u identity.User, orgID, name, slug string, now time.Time, undo *provisionUndo) (identity.Team, error) {
	base := slug
	if base == "" {
		base = identity.SlugifyTeamName(name)
		if base == "" {
			base = "team"
		}
	}
	for attempt := 0; attempt < 10; attempt++ {
		try := base
		if attempt > 0 {
			try = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		t := identity.Team{
			ID:        uuid.NewString(),
			OrgID:     orgID,
			Name:      name,
			Slug:      try,
			CreatedBy: u.ID,
			CreatedAt: now,
			UpdatedAt: now,
		}
		_, err := s.store.CreateTeam(ctx, t)
		if errors.Is(err, identity.ErrSlugAlreadyTaken) {
			if slug != "" {
				return identity.Team{}, identity.ErrSlugAlreadyTaken
			}
			continue
		}
		if err != nil {
			return identity.Team{}, err
		}
		undo.push(fmt.Sprintf("the team %s (slug %s)", t.ID, t.Slug), func(c context.Context) error {
			return s.store.DeleteTeam(c, t.ID)
		})
		if err := s.store.UpsertMembership(ctx, identity.Membership{
			UserID:   u.ID,
			TeamID:   t.ID,
			Role:     identity.RoleOwner,
			JoinedAt: now,
		}); err != nil {
			return identity.Team{}, err
		}
		undo.push(fmt.Sprintf("the owner grant of %s on team %s", u.ID, t.ID), func(c context.Context) error {
			return s.store.DeleteMembership(c, u.ID, t.ID)
		})
		return t, nil
	}
	return identity.Team{}, errors.New("auth: could not allocate slug for team")
}

// CreateOrgFor provisions a new (non-personal) org owned by user
// `userID`, with an owner org-membership and a default team inside it.
// Returns the new org. Used by the super-admin org console and org
// self-service "create organization" flows. If slug is empty it is
// derived from name and uniquified with a numeric suffix on collision.
func (s *Service) CreateOrgFor(ctx context.Context, userID, name, slug string) (identity.Org, error) {
	if name == "" {
		return identity.Org{}, fmt.Errorf("auth: org name required")
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return identity.Org{}, err
	}
	base := slug
	if base == "" {
		base = identity.SlugifyTeamName(name)
		if base == "" {
			base = "org"
		}
	}
	now := s.now().UTC()
	for attempt := 0; attempt < 10; attempt++ {
		try := base
		if attempt > 0 {
			try = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		o := identity.Org{
			ID:        uuid.NewString(),
			Name:      name,
			Slug:      try,
			CreatedBy: u.ID,
			CreatedAt: now,
			UpdatedAt: now,
		}
		_, err := s.store.CreateOrg(ctx, o)
		if errors.Is(err, identity.ErrOrgSlugAlreadyTaken) {
			if slug != "" {
				return identity.Org{}, identity.ErrOrgSlugAlreadyTaken
			}
			continue
		}
		if err != nil {
			return identity.Org{}, err
		}
		// The org row is the first irreversible write, so everything after
		// it is compensated: a failure in between used to leave a TEAM-LESS
		// org holding the slug, and the retry — the only remedy, super-admin
		// only — answered acme-corp-2.
		undo := &provisionUndo{}
		undo.push(fmt.Sprintf("the org %s (slug %s)", o.ID, o.Slug), func(c context.Context) error {
			return s.store.DeleteOrg(c, o.ID)
		})
		if err := s.store.UpsertOrgMembership(ctx, identity.OrgMembership{
			UserID:   u.ID,
			OrgID:    o.ID,
			Role:     identity.OrgRoleOwner,
			JoinedAt: now,
		}); err != nil {
			return identity.Org{}, undo.compensate(ctx, err)
		}
		undo.push(fmt.Sprintf("the owner membership of %s in org %s", u.ID, o.ID), func(c context.Context) error {
			return s.store.DeleteOrgMembership(c, u.ID, o.ID)
		})
		// Seed a default team so the org is immediately usable. The write
		// phase directly: the account was read above, and the org and the
		// owner membership CreateTeamFor would read are the two rows THIS
		// call just wrote — reading them back is the read-after-write
		// #2655 names, and a failure of it left exactly the team-less org.
		if _, err := s.createTeamIn(ctx, u, o.ID, name, "", now, undo); err != nil {
			return identity.Org{}, undo.compensate(ctx, err)
		}
		return o, nil
	}
	return identity.Org{}, errors.New("auth: could not allocate slug for org")
}

// DeleteOrgCascade removes an org and all its identity-scoped children:
// every team in the org (each team's memberships + pending invitations,
// then the team record), then all org memberships, then the org itself.
//
// It deliberately does NOT purge team-scoped resources living in OTHER
// stores (runs, board, forge connections, SSO providers, secrets) — those
// become orphaned and unreachable rather than deleted. Intended for
// super-admin cleanup of empty / migrated orgs; the caller (admin route)
// gates on super-admin and refuses to delete the caller's active org.
func (s *Service) DeleteOrgCascade(ctx context.Context, orgID string) error {
	if orgID == "" {
		return fmt.Errorf("auth: org id required")
	}
	if _, err := s.store.GetOrg(ctx, orgID); err != nil {
		return err
	}
	teams, err := s.store.ListTeamsByOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("auth: list teams for org %s: %w", orgID, err)
	}
	for _, t := range teams {
		mems, err := s.store.ListMembershipsByTeam(ctx, t.ID)
		if err != nil {
			return fmt.Errorf("auth: list memberships for team %s: %w", t.ID, err)
		}
		for _, m := range mems {
			if err := s.store.DeleteMembership(ctx, m.UserID, t.ID); err != nil {
				return fmt.Errorf("auth: delete membership %s/%s: %w", m.UserID, t.ID, err)
			}
		}
		// Best-effort: drop the team's pending invitations.
		if invs, err := s.store.ListInvitationsByTeam(ctx, t.ID); err == nil {
			for _, inv := range invs {
				_ = s.store.DeleteInvitation(ctx, inv.ID)
			}
		}
		if err := s.store.DeleteTeam(ctx, t.ID); err != nil {
			return fmt.Errorf("auth: delete team %s: %w", t.ID, err)
		}
	}
	oms, err := s.store.ListOrgMembershipsByOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("auth: list org memberships for org %s: %w", orgID, err)
	}
	for _, m := range oms {
		if err := s.store.DeleteOrgMembership(ctx, m.UserID, orgID); err != nil {
			return fmt.Errorf("auth: delete org membership %s/%s: %w", m.UserID, orgID, err)
		}
	}
	if err := s.store.DeleteOrg(ctx, orgID); err != nil {
		return fmt.Errorf("auth: delete org %s: %w", orgID, err)
	}
	return nil
}

// MarkOrgForDeletion soft-deletes an org: Status=pending_deletion and
// PurgeAfter=now+grace. The org is blocked immediately (Suspended()==true);
// the nightly sweeper hard-purges it once PurgeAfter passes. Restorable until
// then via RestoreOrg.
func (s *Service) MarkOrgForDeletion(ctx context.Context, orgID string, grace time.Duration) (identity.Org, error) {
	if orgID == "" {
		return identity.Org{}, fmt.Errorf("auth: org id required")
	}
	o, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return identity.Org{}, err
	}
	now := time.Now().UTC()
	purge := now.Add(grace)
	o.Status = identity.TeamStatusPendingDeletion
	o.PurgeAfter = &purge
	o.UpdatedAt = now
	if err := s.store.UpdateOrg(ctx, o); err != nil {
		return identity.Org{}, fmt.Errorf("auth: mark org for deletion: %w", err)
	}
	return o, nil
}

// RestoreOrg cancels a pending deletion, returning the org to active. It is a
// no-op (returns the org unchanged) when the org isn't pending deletion.
func (s *Service) RestoreOrg(ctx context.Context, orgID string) (identity.Org, error) {
	o, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return identity.Org{}, err
	}
	if o.EffectiveStatus() != identity.TeamStatusPendingDeletion {
		return o, nil
	}
	o.Status = identity.TeamStatusActive
	o.PurgeAfter = nil
	o.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateOrg(ctx, o); err != nil {
		return identity.Org{}, fmt.Errorf("auth: restore org: %w", err)
	}
	return o, nil
}

// CreateInvitation issues a fresh invitation. The plaintext token is
// returned (caller emails it) and only its hash is persisted.
//
// An invitation token is a team token (MintTeamToken): it outlives the
// credential it is minted from, so the context must carry the derivation
// chokepoint's verdict for it — the gate stamped it after its ask granted —
// and a call whose context carries none is refused, whatever calls this.
func (s *Service) CreateInvitation(ctx context.Context, teamID, email string, role identity.Role, invitedBy string) (token string, inv identity.Invitation, err error) {
	if _, err := RequireMintVerdict(ctx, MintTeamToken, "auth.Service.CreateInvitation"); err != nil {
		return "", identity.Invitation{}, err
	}
	if !role.Valid() {
		return "", identity.Invitation{}, identity.ErrInvalidRole
	}
	if _, err := s.store.GetTeam(ctx, teamID); errors.Is(err, identity.ErrNotFound) {
		return "", identity.Invitation{}, ErrTeamNotFound
	} else if err != nil {
		return "", identity.Invitation{}, fmt.Errorf("auth: read team %s: %w", teamID, err)
	}
	tok, _, err := GenerateRandomToken(32)
	if err != nil {
		return "", identity.Invitation{}, err
	}
	now := s.now().UTC()
	inv = identity.Invitation{
		ID:        uuid.NewString(),
		TeamID:    teamID,
		Email:     identity.NormalizeEmail(email),
		Role:      role,
		TokenHash: hashInvitationToken(tok),
		InvitedBy: invitedBy,
		CreatedAt: now,
		ExpiresAt: now.Add(InvitationTTL),
	}
	if err := s.store.CreateInvitation(ctx, inv); err != nil {
		return "", identity.Invitation{}, err
	}
	return tok, inv, nil
}

// CreateUserAndPersonalTeam is used by the bootstrap path to provision
// the very first super-admin. Idempotent: returns the existing user
// if email is already taken.
func (s *Service) CreateUserAndPersonalTeam(ctx context.Context, email, name, password string, isSuperAdmin bool, status identity.UserStatus) (identity.User, identity.Team, error) {
	email = identity.NormalizeEmail(email)
	if existing, err := s.store.GetUserByEmail(ctx, email); err == nil {
		// Find their personal team if any, else return zero team.
		ms, _ := s.store.ListMembershipsByUser(ctx, existing.ID)
		for _, mb := range ms {
			t, terr := s.store.GetTeam(ctx, mb.TeamID)
			if terr == nil && t.Personal {
				return existing, t, nil
			}
		}
		return existing, identity.Team{}, nil
	}
	hash, err := HashPassword(password)
	if err != nil {
		return identity.User{}, identity.Team{}, err
	}
	if status == "" {
		status = identity.UserStatusActive
	}
	now := s.now().UTC()
	u := identity.User{
		ID:           uuid.NewString(),
		Email:        email,
		Name:         name,
		PasswordHash: hash,
		Status:       status,
		IsSuperAdmin: isSuperAdmin,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	u, err = s.store.CreateUser(ctx, u)
	if err != nil {
		return identity.User{}, identity.Team{}, err
	}
	space, err := s.createPersonalTeam(ctx, u)
	if err != nil {
		return identity.User{}, identity.Team{}, err
	}
	if u, err = s.store.PatchUser(ctx, u.ID, identity.UserPatch{
		DefaultOrgID: &space.orgID, DefaultTeamID: &space.teamID,
	}); err != nil {
		return identity.User{}, identity.Team{}, err
	}
	t, _ := s.store.GetTeam(ctx, space.teamID)
	return u, t, nil
}

// personalSpace is what createPersonalTeam wrote: the personal org and team,
// and the user's owner rows in both.
type personalSpace struct {
	orgID, teamID string
	orgMembership identity.OrgMembership
	membership    identity.Membership
}

// createPersonalTeam provisions a personal Org wrapping a personal Team
// for the given user, adds owner memberships at both levels, and returns
// what it wrote. The personal org preserves the single-user
// UX: one org, one team, the user owns both. Slugs derive from the email
// local-part with a numeric suffix on collision (org and team share the
// base; the team slug is suffixed "-team" to keep the namespaces clean).
func (s *Service) createPersonalTeam(ctx context.Context, u identity.User) (personalSpace, error) {
	now := s.now().UTC()
	org, om, err := s.createPersonalOrg(ctx, u, now)
	if err != nil {
		return personalSpace{}, err
	}
	base := identity.SlugifyTeamName(u.Email)
	if base == "" {
		base = "team"
	}
	for attempt := 0; attempt < 10; attempt++ {
		slug := base
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		t := identity.Team{
			ID:        uuid.NewString(),
			OrgID:     org.ID,
			Name:      defaultPersonalTeamName(u),
			Slug:      slug,
			CreatedBy: u.ID,
			CreatedAt: now,
			UpdatedAt: now,
			Personal:  true,
		}
		_, err := s.store.CreateTeam(ctx, t)
		if errors.Is(err, identity.ErrSlugAlreadyTaken) {
			continue
		}
		if err != nil {
			return personalSpace{}, err
		}
		mb := identity.Membership{
			UserID:   u.ID,
			TeamID:   t.ID,
			Role:     identity.RoleOwner,
			JoinedAt: now,
		}
		if err := s.store.UpsertMembership(ctx, mb); err != nil {
			return personalSpace{}, err
		}
		return personalSpace{orgID: org.ID, teamID: t.ID, orgMembership: om, membership: mb}, nil
	}
	return personalSpace{}, errors.New("auth: could not allocate slug for personal team")
}

// createPersonalOrg provisions an Org with Personal=true and an owner
// org-membership, and returns both. Slug derives from the email local-part
// with a numeric suffix on collision.
func (s *Service) createPersonalOrg(ctx context.Context, u identity.User, now time.Time) (identity.Org, identity.OrgMembership, error) {
	base := identity.SlugifyTeamName(u.Email)
	if base == "" {
		base = "org"
	}
	for attempt := 0; attempt < 10; attempt++ {
		slug := base
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		o := identity.Org{
			ID:        uuid.NewString(),
			Name:      defaultPersonalTeamName(u),
			Slug:      slug,
			CreatedBy: u.ID,
			CreatedAt: now,
			UpdatedAt: now,
			Personal:  true,
		}
		_, err := s.store.CreateOrg(ctx, o)
		if errors.Is(err, identity.ErrOrgSlugAlreadyTaken) {
			continue
		}
		if err != nil {
			return identity.Org{}, identity.OrgMembership{}, err
		}
		om := identity.OrgMembership{
			UserID:   u.ID,
			OrgID:    o.ID,
			Role:     identity.OrgRoleOwner,
			JoinedAt: now,
		}
		if err := s.store.UpsertOrgMembership(ctx, om); err != nil {
			return identity.Org{}, identity.OrgMembership{}, err
		}
		return o, om, nil
	}
	return identity.Org{}, identity.OrgMembership{}, errors.New("auth: could not allocate slug for personal org")
}

func defaultPersonalTeamName(u identity.User) string {
	if u.Name != "" {
		return u.Name + "'s team"
	}
	return u.Email
}

// hashInvitationToken is a thin wrapper used only to keep this
// package self-contained without exposing the hashing scheme to
// callers.
func hashInvitationToken(token string) string {
	return HashRefreshToken(token)
}

// dummyHash is a pre-computed argon2id hash of a fixed string used
// to spend equivalent CPU on a not-found lookup so an attacker
// cannot enumerate accounts via timing. The plaintext is unknown to
// VerifyPassword, so it always returns (false, nil).
const dummyHash = "$argon2id$v=19$m=65536,t=2,p=1$YWFhYWFhYWFhYWFhYWFhYQ$" +
	"sBzKvHEqs+sN1iUqXQU8/lLrUgnExwa3JG6lFfCh9HE"
