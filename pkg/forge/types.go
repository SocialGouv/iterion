// Package forge is iterion's OUTBOUND forge-integration layer: it
// connects a team to a GitLab / GitHub / Forgejo account (OAuth or PAT),
// lists that account's repos, and AUTO-PROVISIONS the forge-side webhook
// + the bot-secret binding when an operator enables a bot on a repo —
// replacing the manual PAT→secret→binding→webhook→forge-hook chain.
//
// It is the complement of pkg/webhooks (iterion's INBOUND receiver):
// pkg/webhooks authenticates deliveries the forge sends TO iterion;
// pkg/forge holds the admin credential that lets iterion call OUT to the
// forge to register that delivery in the first place.
//
// Separation of concerns (load-bearing):
//   - The connection's admin token (OAuth user token / GitHub-App
//     installation token / PAT) lives sealed on a connection.ForgeConnection
//     and is used only to manage iterion's footprint on the forge (create
//     hooks, list repos). It is NEVER the token a bot posts with.
//   - The bot-runtime forge token (what review-pr posts comments with)
//     stays a secrets.GenericSecret + secrets.BotSecretBinding. The
//     orchestrator DERIVES a managed generic secret from the connection,
//     so the entire downstream (ResolveGenericWithBindings → RunBundle →
//     /run/iterion/secrets/forge_token → glab/gh) is unchanged.
//
// The connection MODEL — the ForgeConnection record, the ForgeStore
// persistence contract and the credential sealer — is pkg/connection's
// since #2760 (the forges are connectors #1-3, ADR-098); this package keeps
// the behaviour and reads the model through the aliases in
// connection_contract.go.
package forge

import (
	"errors"
	"fmt"
)

// Sentinel errors. Callers compare with errors.Is.
//
// Two families live here and the difference is invisible in the text: a
// sentinel is IN the ErrNotFound class iff it wraps ErrNotFound. Membership
// is what pkg/server's forgeUpstreamStatus reads, so wrapping one of the
// store misses below into the class would turn every "could not be recorded"
// %w-wrap in the forge layer into a 404 — the messages unchanged, only what
// errors.Is answers. Each sentinel states its family; the table and the guard
// that pins it live next to forgeUpstreamStatus.
var (
	// ErrIntegrationNotFound and ErrOAuthAppNotFound report an iterion STORE
	// miss, and are deliberately OUTSIDE the ErrNotFound class: they are
	// iterion's own state, never an answer the forge gave, and every handler
	// that %w-wraps a store failure relies on them not classifying as a forge
	// 404. (The third store-miss sentinel, ErrConnectionNotFound, moved to
	// pkg/connection with the store under the same rule — see
	// connection_contract.go.)
	ErrIntegrationNotFound = errors.New("forge: repo integration not found")
	ErrOAuthAppNotFound    = errors.New("forge: oauth app not found")
	ErrOAuthAppExists      = errors.New("forge: oauth app already exists")
	// ErrNotFound is the class every 404 belongs to. A caller that only needs
	// "the forge has no such thing" matches on it; one that acts on a
	// specific absence matches the resource sentinel (ErrHookNotFound) or
	// reads the *NotFoundError the operation carries.
	//
	// Membership is by wrapping, and it is the whole meaning: forgeUpstreamStatus
	// answers a member 404. Only what the forge itself answered belongs.
	ErrNotFound = errors.New("forge: not found")
	// ErrHookNotFound is IN the ErrNotFound class — a forge-answered 404 the
	// orchestrator additionally reads as "the hook is already gone", so
	// deprovision treats it as done.
	ErrHookNotFound = fmt.Errorf("%w: hook", ErrNotFound)
	// ErrForbidden is returned by an admin client when the credential lacks
	// the scope to perform an operation (e.g. create a webhook). The
	// orchestrator surfaces it as a structured "insufficient_scope" error so
	// the studio can prompt for re-auth with broader scope or a PAT.
	ErrForbidden = errors.New("forge: insufficient scope")
	// ErrUnauthorized is returned when the credential is rejected outright
	// (revoked / expired). The refresh worker marks a refreshable connection
	// StatusNeedsReauth on it; an avatar apply meeting it on a PAT — which
	// nothing else probes — marks the connection StatusRevoked.
	ErrUnauthorized = errors.New("forge: credential rejected")
	// ErrPermissionsNotGranted is returned when minting a GitHub-App
	// installation token fails with a PERMANENT permission mismatch — the
	// installation was approved with a narrower permission set than iterion
	// now requests (HTTP 422 "The permissions requested are not granted to
	// this installation"). It is terminal: retrying every refresh cycle can
	// never self-heal, so the worker flips the connection to StatusDegraded
	// (an org admin must re-approve the install with the updated permissions,
	// or the connection should be removed) instead of re-minting each tick.
	ErrPermissionsNotGranted = errors.New("forge: installation permissions not granted")
	// ErrAvatarUnsupported is returned by an AvatarSetter when the forge
	// instance has no avatar endpoint at all (GitLab before 17.0, Gitea before
	// 1.20): the operator's only path is the forge's own profile page.
	ErrAvatarUnsupported = errors.New("forge: this instance has no avatar API")
	// ErrLocalPreflight marks a failure that happened while PREPARING a forge
	// call, before any byte reached the network: a stored App key that is not
	// parseable PEM, a payload that will not marshal, a base URL that will not
	// parse. The forge never saw the request and cannot be blamed for it.
	//
	// It exists because a client method is not the same thing as a round
	// trip. AppClient.rest and every security-read mint sign an App JWT from
	// a stored key first, so a key iterion cannot read fails inside what
	// reads like a pure remote call — and the handlers that default to 502
	// then report a third party as broken. Callers in pkg/server ask
	// isIterionFault, which reads this sentinel and answers 500.
	ErrLocalPreflight = errors.New("forge: the request never left iterion")
	// ErrProjectBotNotPermitted is a forge's refusal to MINT a per-repository
	// bot credential for the connected account: on GitLab a 400 on
	// POST /projects/:id/access_tokens, whose message names a cause the
	// status does not distinguish — the account is below Maintainer on the
	// project, or project access tokens are switched off on the group or the
	// instance. The route answers it with both remedies.
	ErrProjectBotNotPermitted = errors.New("forge: the connected account cannot mint a project bot here")
	// ErrProjectBotUnsupported is a connection that asks for a per-repository
	// bot on a forge whose client cannot mint one: a configuration
	// contradiction, not a forge answer.
	ErrProjectBotUnsupported = errors.New("forge: this forge client cannot mint a project bot")
	// ErrRepoNotEnabled is a repo-targeted launch on a connection that posts
	// as per-repository bots, for a repository that has none: the bot is
	// minted when the repository is enabled, and nothing else can stand in
	// for it.
	ErrRepoNotEnabled = errors.New("forge: repository not enabled on this connection")
)
