package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// CredentialKind names the credential an Identity was read from. Every reader
// of a presented credential sets it: JWTSigner.Verify, the personal-access-
// token resolution, the WS-ticket redemption and the dev-mode principal. An
// Identity built anywhere else carries none, and derives nothing
// (CheckDerivation).
type CredentialKind string

const (
	// CredentialAccessToken is an access JWT a login, a refresh or a switch
	// minted.
	CredentialAccessToken CredentialKind = "access_token"
	// CredentialPAT is a personal access token (pkg/pat).
	CredentialPAT CredentialKind = "personal_access_token"
	// CredentialWSTicket is a single-use WebSocket ticket: what a redeemed
	// ticket authenticates as once its WebSocket opens.
	CredentialWSTicket CredentialKind = "ws_ticket"
	// CredentialDevMode is the principal a server running without
	// authentication (DisableAuth) synthesizes for every request.
	CredentialDevMode CredentialKind = "dev_mode"
)

// Mint names what a route does with the credential presented to it: mint a
// credential from it, change or remove a credential of its account, or
// exercise platform authority. CheckDerivation answers each the same way for
// the presented credential's standing, and decides per kind what that
// credential may do at all.
type Mint string

const (
	// MintAccessToken is the team and org switches: an access token
	// re-scoped from the presented one.
	MintAccessToken Mint = "an access token"
	// MintPAT is POST /api/me/tokens.
	MintPAT Mint = "a personal access token"
	// MintWSTicket is POST /api/ws/ticket, and the redemption of the ticket
	// it minted, which re-asks the question for the credential that minted
	// it.
	MintWSTicket Mint = "a WebSocket ticket"
	// MintSSOLink is the start of an SSO link from the account settings,
	// and its callback, which links an IdP identity to the account — before
	// and after it writes the link.
	MintSSOLink Mint = "an SSO link"
	// MintPassword is the admin password reset of the presented
	// credential's own account (POST /api/admin/users/{id}/reset-password):
	// a temporary password its holder signs in with. The reset of another
	// account is ExercisePlatformAuthority.
	MintPassword Mint = "a password"
	// MintTeamToken is the mint of a team-scoped self-authenticating token
	// that outlives the credential it is minted from and is bound to
	// nothing of it: a webhook token (POST /api/teams/{id}/webhooks and its
	// rotate; POST /api/teams/{id}/forge/repo-bots and the PATCH that
	// re-provisions it, where the orchestrator mints the repo's hook token;
	// POST /api/orgs/{id}/provision-approvals/{approval_id}/approve, which
	// replays that enable), a config share-link token (POST
	// /api/teams/{id}/config-shares and its rotate), a team invitation token
	// (POST /api/teams/{id}/invitations, POST /api/orgs/{id}/invitations).
	// None of them authenticates AS the account, but each opens a door that
	// stays open after the minter's session is gone — a delivery channel that
	// launches runs, a public read of the team's configuration, a
	// membership whoever holds the token joins with — so the mint asks the
	// chokepoint, and a personal access token (pinned to its team and its
	// own expiry) does not make one.
	//
	// The set is defined by the VERB, not by the route's name: webhooks.
	// MintToken, configshare.MintToken and Service.CreateInvitation demand
	// the verdict in the context they are called with
	// (RequireMintVerdict — a mint without one refuses), the verdict is
	// stamped only where the chokepoint granted (pkg/server's
	// derivation_gate.go), and pkg/server's
	// TestEveryTeamTokenMintPrimitiveDemandsTheVerdict reads the demand in
	// each primitive's own body while
	// TestEveryMintPrimitiveCallerLivesInTheGuardsGraph fails on a caller
	// the guards do not read.
	MintTeamToken Mint = "a team token (webhook, share link or invitation)"
	// ChangeCredential is a route that changes or removes a credential of
	// the presented credential's account: POST /api/me/password, POST
	// /api/me/sessions/revoke-all, DELETE /api/me/tokens/{id}, DELETE
	// /api/me/sso/links/{provider}/{subject}.
	ChangeCredential Mint = "a change of the account's credentials"
	// ChangeSignIn is a route that changes how an org's accounts sign in:
	// its SSO providers and its verified email domains (create, update,
	// delete, verify) — an auto-linking provider signs in as the accounts of
	// a verified domain.
	ChangeSignIn Mint = "a change of how an org's accounts sign in"
	// ExercisePlatformAuthority is every platform-admin route
	// (requireSuperAdmin): the super-admin's authority over every account —
	// granting super-admin, disabling, re-enabling, resetting a password —
	// and over the platform. Derivation.User says whether the account is
	// still a super-admin.
	ExercisePlatformAuthority Mint = "platform authority"
)

// ErrNotDerivable refuses a mint the presented credential may never make: a
// personal access token asked for a credential of its own account (an access
// token, another personal access token, an SSO link, a password), an access
// token that names no session asked for an SSO link, a WS ticket asked for
// anything, a synthetic principal. The HTTP layer answers 403.
var ErrNotDerivable = errors.New("auth: this credential cannot mint that credential")

// notDerivable is an ErrNotDerivable whose message tells the caller what to
// do instead.
type notDerivable struct{ msg string }

func (e notDerivable) Error() string        { return e.msg }
func (e notDerivable) Is(target error) bool { return target == ErrNotDerivable }

// Derivation is what a route may mint from a presented credential.
type Derivation struct {
	// User is the account as the store reads it now — dev mode excepted,
	// where it is the synthesized principal.
	User identity.User
	// NotAfter is the instant the presented credential stops
	// authenticating; zero when it names none (a personal access token
	// without an expiry, dev mode). A minted credential that is derived from
	// the presented one for the same use — a switched access token, a WS
	// ticket — expires no later than it.
	NotAfter time.Time
}

// PATState answers whether the personal access token id still authenticates
// at now: its row exists, is not revoked and has not expired. An error is a
// read that did not complete.
type PATState func(ctx context.Context, id string, now time.Time) (usable bool, err error)

// SetPATState registers the reader CheckDerivation and CheckStanding ask about
// a personal access token — presented, or the minter of a WS ticket. The
// server registers its own (its pat.Store) when it is built on this service;
// a service with none refuses every personal access token it is asked about.
func (s *Service) SetPATState(fn PATState) { s.patState = fn }

// CheckDerivation is the one check every route that mints a credential from
// the credential presented to it, changes or removes a credential of its
// account, or exercises platform authority runs before it acts (Mint lists
// them), and that the redemption of a WS ticket and the SSO link callback run
// again. It refuses what a refresh refuses — an account disabled or pending a
// password change (sessionRefusal), a session a revocation covers: its
// lineage signed out or reuse-revoked, its account signed out everywhere or
// its password changed (SessionStore.SessionRevoked) — a personal access
// token whose row is revoked, expired or gone, and a presented credential
// past its own expiry; and it refuses, whatever the account's state, what the
// presented kind may never do (ErrNotDerivable): a personal access token
// mints no credential of its own account but a WS ticket — it is pinned to its
// team and to its own expiry, and a switch, a child token, an SSO link or a
// password would escape both — a WS ticket mints nothing, and an access token
// that names no session links no SSO identity.
//
// An access token that names no session (minted before access tokens named
// their session) is otherwise checked for its account alone, and bounded by
// its own expiry, which Verify already enforces.
//
// It answers the account as the store reads it now and the expiry a credential
// derived for the same use must not outlive (Derivation.NotAfter). A read
// that fails is returned: the route acts on nothing.
func (s *Service) CheckDerivation(ctx context.Context, presented Identity, mint Mint) (Derivation, error) {
	switch presented.Credential {
	case CredentialDevMode:
		return devModeDerivation(presented), nil
	case CredentialAccessToken:
		if mint == MintSSOLink && presented.SessionLineage == "" {
			return Derivation{}, notDerivable{msg: "an access token that names no session cannot link an SSO identity: sign in again to link one"}
		}
	case CredentialPAT:
		switch mint {
		case MintWSTicket, ChangeCredential, ChangeSignIn, ExercisePlatformAuthority:
		default:
			return Derivation{}, notDerivable{msg: "a personal access token cannot mint another credential: sign in to mint one"}
		}
	case CredentialWSTicket:
		return Derivation{}, notDerivable{msg: "a WebSocket ticket opens one WebSocket and mints nothing"}
	default:
		if presented.IsSynthetic() || presented.IsMachine() {
			return Derivation{}, notDerivable{msg: "this principal mints no credential"}
		}
		return Derivation{}, fmt.Errorf("auth: %s from a credential of unknown kind %q", mint, presented.Credential)
	}
	u, err := s.standing(ctx, presented)
	if err != nil {
		return Derivation{}, err
	}
	return Derivation{User: u, NotAfter: presented.ExpiresAt}, nil
}

// CheckStanding asks whether the credential an identity was read from still
// stands — what CheckDerivation asks of it, without the question of what its
// kind may do: its account active, its session not revoked, a personal access
// token's row usable, the credential not past its expiry. A WS ticket is
// asked as the credential that minted it (Identity.MintedBy). An open
// WebSocket asks it again with the expiry left out (StandingOfOpenSocket).
//
// It answers the account as the store reads it now. A read that fails is
// returned: the caller acts on nothing.
func (s *Service) CheckStanding(ctx context.Context, presented Identity) (identity.User, error) {
	if presented.Credential == CredentialWSTicket {
		presented.Credential = presented.MintedBy
		presented.MintedBy = ""
	}
	switch presented.Credential {
	case CredentialDevMode:
		return devModeDerivation(presented).User, nil
	case CredentialAccessToken, CredentialPAT:
		return s.standing(ctx, presented)
	case CredentialWSTicket:
		return identity.User{}, fmt.Errorf("auth: a WS ticket minted by a WS ticket: %w", ErrTokenInvalid)
	default:
		return identity.User{}, fmt.Errorf("auth: the standing of a credential of unknown kind %q", presented.Credential)
	}
}

// StandingOfOpenSocket is the identity an open WebSocket asks CheckStanding
// about: the credential it was opened with, its expiry left out. A WebSocket
// outlives the access token it was opened with by design (the client renews
// its tokens over HTTP); its session, its account and a personal access
// token's row are what still decide.
func StandingOfOpenSocket(id Identity) Identity {
	id.ExpiresAt = time.Time{}
	return id
}

// devModeDerivation is the derivation of the principal a server without
// authentication synthesizes.
func devModeDerivation(presented Identity) Derivation {
	return Derivation{User: identity.User{
		ID:           presented.UserID,
		Email:        presented.Email,
		IsSuperAdmin: presented.IsSuperAdmin,
		Status:       identity.UserStatusActive,
	}}
}

// standing reads what decides whether an access token or a personal access
// token still stands: its expiry, its account, its session's revocations (an
// access token naming one) or its row (a personal access token).
func (s *Service) standing(ctx context.Context, presented Identity) (identity.User, error) {
	if presented.UserID == "" {
		return identity.User{}, errors.New("auth: a presented credential that names no account")
	}
	now := s.now()
	if !presented.ExpiresAt.IsZero() && !now.Before(presented.ExpiresAt) {
		return identity.User{}, ErrTokenExpired
	}
	u, err := s.store.GetUser(ctx, presented.UserID)
	if errors.Is(err, identity.ErrNotFound) {
		return identity.User{}, fmt.Errorf("%w: its account no longer exists", ErrSessionNotFound)
	}
	if err != nil {
		return identity.User{}, fmt.Errorf("auth: read the account of a presented credential: %w", err)
	}
	if refusal := sessionRefusal(u); refusal != nil {
		return identity.User{}, refusal
	}
	switch presented.Credential {
	case CredentialAccessToken:
		if presented.SessionLineage == "" {
			return u, nil
		}
		cctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
		defer cancel()
		revoked, err := s.sessions.SessionRevoked(cctx, Session{
			ID:         presented.SessionLineage,
			UserID:     u.ID,
			LineageID:  presented.SessionLineage,
			Generation: presented.SessionGeneration,
		})
		if err != nil {
			return identity.User{}, fmt.Errorf("auth: check the session of an access token against its revocations: %w", err)
		}
		if revoked {
			return identity.User{}, ErrSessionRevoked
		}
	case CredentialPAT:
		if presented.CredentialID == "" {
			return identity.User{}, fmt.Errorf("auth: a personal access token credential that names no token: %w", ErrTokenInvalid)
		}
		if s.patState == nil {
			return identity.User{}, errors.New("auth: no reader of personal access tokens is registered")
		}
		cctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
		defer cancel()
		usable, err := s.patState(cctx, presented.CredentialID, now.UTC())
		if err != nil {
			return identity.User{}, fmt.Errorf("auth: read the personal access token %s: %w", presented.CredentialID, err)
		}
		if !usable {
			return identity.User{}, fmt.Errorf("%w: the personal access token is revoked or expired", ErrTokenRevoked)
		}
	}
	return u, nil
}
