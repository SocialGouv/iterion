package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/store"
)

// withApiKeyTenantFilter augments a Mongo filter with a tenant_id
// clause derived from the request context. Returns an error when ctx
// carries no tenant so callers can't accidentally bypass isolation.
// The matching error sentinel below maps to 401 in HTTP handlers.
func withApiKeyTenantFilter(ctx context.Context, base bson.M) (bson.M, error) {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return nil, ErrApiKeyTenantMissing
	}
	out := make(bson.M, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["tenant_id"] = tenantID
	return out, nil
}

// ErrApiKeyTenantMissing is returned when an ApiKeyStore call lacks
// the tenant_id needed to scope its query. Callers must propagate it
// up — falling back to an unscoped query would expose every tenant's
// keys.
var ErrApiKeyTenantMissing = errors.New("secrets: ApiKey store called without tenant context")

// platformScope is the single reserved literal both platform scopes derive
// from: one concept — "the deployment itself" — indexed in two namespaces
// (api-key tenant ids and OAuth owner keys). The prefix-with-colon shape
// cannot collide with a real tenant/user id (UUIDs, emails), with an
// OrgOwnerKey (always "org:<team-uuid>"), nor with an org-tier scope
// (always "orgtier:<org-uuid>"). Renames migrate BOTH exported names at
// once by construction.
const platformScope = "platform:"

// orgTierScope is the reserved prefix for a credential owned by an
// identity.Org — the billing/governance tenant that groups teams — and
// lent to the teams its CredentialAudience names. Same trick as
// platformScope: an org key is an ordinary ApiKey row and an org forfait
// an ordinary OAuthRecord, under a reserved scope, so the whole store
// (tenant filter, defaults, rotation, MarkUsed, the refresh worker) is
// reused with zero schema change.
//
// It deliberately is NOT "org:". That prefix is already taken by
// OrgOwnerPrefix, which despite its name keys a TEAM-scoped forfait
// (OrgOwnerKey's argument is a tenant/team id). Reusing it would make an
// org credential and a team credential collide in one owner namespace,
// and the collision would be silent — the publisher would serve whichever
// row the store returned first.
const orgTierScope = "orgtier:"

// OrgTierTenantID is the sentinel tenant under which an ORG's own shared
// provider API keys are stored: ordinary ApiKey rows with
// TenantID = ScopeTeamID = OrgTierTenantID(orgID), written and read under
// store.WithTenant. The cloud publisher consults them after the team's own
// BYOK and forfaits and before the mutualised pool, for the teams the org's
// CredentialAudience admits. The ApiKeyStore counterpart of
// OrgTierOwnerKey.
func OrgTierTenantID(orgID string) string { return orgTierScope + orgID }

// OrgTierOwnerKey is the synthetic OAuth owner key under which an ORG's own
// shared forfait blobs are stored. Shares its literal with
// OrgTierTenantID (see orgTierScope) — one concept, two index namespaces,
// exactly as PlatformOwnerKey shares platformScope with PlatformTenantID.
func OrgTierOwnerKey(orgID string) string { return orgTierScope + orgID }

// IsOrgTierScope reports whether a tenant id or owner key addresses the
// org tier. Callers that must never treat a reserved scope as a real
// tenant (roster reads, quota metering, the studio's team pickers) use it
// rather than re-deriving the prefix.
func IsOrgTierScope(s string) bool { return strings.HasPrefix(s, orgTierScope) }

// OrgIDFromTierScope returns the org id an org-tier scope addresses, and
// false when s is not one.
func OrgIDFromTierScope(s string) (string, bool) {
	if !IsOrgTierScope(s) {
		return "", false
	}
	return strings.TrimPrefix(s, orgTierScope), true
}

// PlatformTenantID is the sentinel tenant under which the DEPLOYMENT's own
// provider API keys are stored — the DB-backed form of the platform env
// fallback (ANTHROPIC_API_KEY et al. on the runner pod). Platform keys are
// ordinary ApiKey rows with TenantID = ScopeTeamID = PlatformTenantID,
// written and read under store.WithTenant(ctx, PlatformTenantID), so the
// whole store (tenant filter, defaults, rotation, MarkUsed) is reused with
// zero schema change — the ApiKeyStore counterpart of PlatformOwnerKey.
// The cloud publisher consults these LAST (after tenant BYOK,
// OAuth-forfait, and the mutualised pool) for the providers a run still
// lacks; managed by super-admins via /api/admin/llm/api-keys.
const PlatformTenantID = platformScope

// Provider enumerates the supported LLM credential providers. The
// string values are stable wire identifiers; do not rename without a
// migration. Naming mirrors what the model registry consumes.
type Provider string

const (
	ProviderAnthropic  Provider = "anthropic"
	ProviderOpenAI     Provider = "openai"
	ProviderBedrock    Provider = "bedrock"
	ProviderVertex     Provider = "vertex"
	ProviderAzure      Provider = "azure"
	ProviderOpenRouter Provider = "openrouter"
	ProviderXAI        Provider = "xai"
	// ProviderZAI is z.ai's Coding-Plan token. The provider exposes an
	// Anthropic-compatible HTTP surface, so credentials flow through the
	// existing Anthropic codepath with ANTHROPIC_BASE_URL pointed at
	// z.ai's endpoint and the ZAI token used as ANTHROPIC_AUTH_TOKEN.
	// Kept as a distinct Provider so per-tenant BYOK can pick "z.ai"
	// vs "anthropic" explicitly.
	ProviderZAI Provider = "zai"
	// ProviderMoonshot is Moonshot's account key, which reaches the Kimi
	// family. Like z.ai the provider exposes an Anthropic-compatible HTTP
	// surface, so credentials flow through the existing Anthropic codepath
	// with the base URL pointed at Moonshot's endpoint and the token used
	// as ANTHROPIC_AUTH_TOKEN. Distinct from ProviderZAI because the
	// account, the endpoint and the bill are all different: a campaign
	// whose only Anthropic-wire providers are anthropic and zai freezes
	// the moment both are walled at once.
	//
	// NOT the same thing as `backend: "kimi"` (ADR-065), which drives
	// Moonshot's own CLI and resolves its credentials from the host
	// environment, outside this store entirely.
	ProviderMoonshot Provider = "moonshot"
	// ProviderOpenAICompatible is a tenant-owned OpenAI-compatible gateway
	// (a LiteLLM endpoint in front of a regional model host, a vLLM
	// server …): the record carries the gateway's base URL beside the key,
	// and the model id after the `openai_compatible/` prefix is sent to it
	// verbatim. Until the walk vocabulary learns it (#2005 PR-C'), such a
	// key is storable but never sealed into a run.
	ProviderOpenAICompatible Provider = "openai_compatible"
)

// ZAIDefaultBaseURL is the Anthropic-compatible endpoint z.ai's Coding
// Plan publishes. Centralised here so the delegate and the model
// registry agree without re-deriving the URL.
const ZAIDefaultBaseURL = "https://api.z.ai/api/anthropic"

// MoonshotDefaultBaseURL is the Anthropic-compatible endpoint Moonshot
// publishes for the Kimi family. The api.moonshot.cn gateway answers on the
// same wire; MOONSHOT_BASE_URL overrides. Centralised here so the delegate
// and the model registry agree without re-deriving the URL.
const MoonshotDefaultBaseURL = "https://api.moonshot.ai/anthropic"

// XAIDefaultBaseURL is the host claw's OpenAI-compatible client targets
// for xAI Grok. The openai provider appends `/v1/chat/completions`, so
// this value must NOT include a trailing `/v1` (unlike the public
// OpenAI-SDK convention of `base_url=https://api.x.ai/v1`). Override
// with `XAI_BASE_URL` when pointing at a proxy or regional endpoint.
const XAIDefaultBaseURL = "https://api.x.ai"

// Valid reports whether p is one of the known providers.
func (p Provider) Valid() bool {
	switch p {
	case ProviderAnthropic, ProviderOpenAI, ProviderBedrock,
		ProviderVertex, ProviderAzure, ProviderOpenRouter, ProviderXAI,
		ProviderZAI, ProviderMoonshot, ProviderOpenAICompatible:
		return true
	}
	return false
}

// CredentialIsJSON reports whether this provider's BYOK value is a JSON
// credential document rather than a bearer token: Bedrock takes an
// AWS-style credential object, Vertex a service-account file. The shape
// gate at ingestion reads this so the two are not refused as "a terminal
// transcript" for containing newlines and spaces.
func (p Provider) CredentialIsJSON() bool {
	switch p {
	case ProviderBedrock, ProviderVertex:
		return true
	}
	return false
}

// ApiKey is a BYOK record: a single API key (or AWS-style credential
// blob, JSON-encoded inside SealedSecret) attached to a team and
// optionally scoped to a single user. The plaintext secret is never
// persisted — only the AES-GCM-sealed blob.
//
// Scope semantics:
//   - ScopeUserID == "": team-wide. Any member of the team picks it up
//     when their per-user keys do not provide the requested provider.
//   - ScopeUserID != "": user-only. Visible only to that user even when
//     listed by other team members (the API list endpoint hides them).
//
// Default flag: per (team, user, provider) tuple at most ONE entry is
// flagged is_default. Resolution prefers it over non-default keys.
type ApiKey struct {
	ID           string     `bson:"_id" json:"id"`
	TenantID     string     `bson:"tenant_id" json:"tenant_id"`
	ScopeTeamID  string     `bson:"scope_team" json:"scope_team_id"`
	ScopeUserID  string     `bson:"scope_user,omitempty" json:"scope_user_id,omitempty"`
	Provider     Provider   `bson:"provider" json:"provider"`
	Name         string     `bson:"name" json:"name"`
	Last4        string     `bson:"last4,omitempty" json:"last4,omitempty"`
	SealedSecret []byte     `bson:"sealed_secret" json:"-"`
	IsDefault    bool       `bson:"is_default,omitempty" json:"is_default,omitempty"`
	CreatedBy    string     `bson:"created_by" json:"created_by"`
	CreatedAt    time.Time  `bson:"created_at" json:"created_at"`
	LastUsedAt   *time.Time `bson:"last_used_at,omitempty" json:"last_used_at,omitempty"`
	ExpiresAt    *time.Time `bson:"expires_at,omitempty" json:"expires_at,omitempty"`
	Fingerprint  string     `bson:"fingerprint,omitempty" json:"fingerprint,omitempty"`
	// MaxConcurrentRuns caps how many ALIVE runs (queued or running) may
	// hold this key at once; 0 means uncapped. Providers that enforce
	// fair-usage frequency limits publish NO numeric bound to adapt to —
	// the operator sets one here, and the resolver's usable-predicate
	// walks past a key at its ceiling exactly like a refused one, so the
	// next key or tier serves instead of tripping the provider. A SOFT
	// cap by design: the refused-key restore still hands the key out when
	// no other tier could serve its wire — progress beats the ceiling
	// when the alternative is a run with no credential at all.
	MaxConcurrentRuns int `bson:"max_concurrent_runs,omitempty" json:"max_concurrent_runs,omitempty"`
	// BaseURL is the gateway root of an openai_compatible key (no trailing
	// slash, no `/v1`; https). Required for that provider, refused on every
	// other: a vendor key has a compiled endpoint. It is not secret, so it
	// rides the row in clear — and the seal binds it, so a repointed row
	// fails to open rather than sending the key to another host.
	BaseURL string `bson:"base_url,omitempty" json:"base_url,omitempty"`
	// Bots is the key's WORKLOAD audience: the bot ids whose runs may draw
	// on it. Empty admits every bot, so a key written before this field
	// existed keeps funding exactly what it funded yesterday — naming one
	// bot is what turns enforcement on, never a migration.
	//
	// Entries are CANONICAL bot ids (botregistry.NormalizeName: lower-case,
	// trimmed, `_` and spaces folded to `-`). This package cannot call that
	// helper — botregistry depends on it — so the canonical form is imposed
	// by the two edges instead: the write routes normalise what they store,
	// and the publisher normalises the run's bot id once before the walk.
	// The comparison below is therefore EXACT on purpose. Matching leniently
	// here would be a second spelling rule competing with botregistry's, and
	// a prefix or case-insensitive match would silently widen the audience —
	// `sec` would open a key scoped to `sec-audit-source`.
	//
	// Unlike MaxConcurrentRuns this is a GATE, not a ceiling: it is checked
	// inside Resolve rather than through the usable-predicate, so neither an
	// explicit pin nor the refused-key restore can hand the key to a bot its
	// operator did not name. A scope says who may SEE a key; this says what
	// it is FOR, and the two are independent — a team-wide key can fund one
	// bot, and a personal key can fund all of them.
	Bots []string `bson:"bots,omitempty" json:"bots,omitempty"`
	// Repos restricts which repositories a launch may spend this key for,
	// named as the forge's full path (`owner/repo`). Empty admits every
	// repository; a launch that names no repository cannot spend a
	// restricted key. Checked in Resolve before the pin exemption, like Bots.
	Repos []string `bson:"repos,omitempty" json:"repos,omitempty"`
}

// ServesBotForLaunch reports whether this key's audience admits botID.
//
// An empty allow-list admits everything. A non-empty one refuses a run that
// names NO bot: an operator who wrote "this key funds the audit" did not mean
// "and also anything launched without naming a bot", and an inline `.bot` a
// requester uploaded is exactly that shape. Same rule, and same reason, as
// credpool.Pledge.AvailableForLaunch.
//
// What this is NOT: an authorisation boundary. It expresses OPERATOR INTENT
// over which of a deployment's workloads a key funds, on the bot id the
// launch path resolved — a value the server derived from its own tiers, or
// no id at all when the request carried inline source (pkg/server/
// runs_launch.go). A caller naming another team's bot therefore reaches
// nothing of theirs: audience matching sits inside a tenant's own key set.
func (k ApiKey) ServesBotForLaunch(botID string) bool {
	if len(k.Bots) == 0 {
		return true
	}
	return botID != "" && k.listsBot(botID)
}

type launchRepoCtxKey struct{}

// WithLaunchRepo stamps the repository a launch serves (the forge's full
// path, "owner/repo") on the context the credential walk runs under, the
// way the tenant travels: Resolve reads it to apply a key's Repos audience.
// A launch that serves no repository stamps the empty string, and that
// stamp overrides whatever a parent context carried: a repo-less launch
// must never inherit a repository and spend a key restricted to it.
func WithLaunchRepo(ctx context.Context, repo string) context.Context {
	return context.WithValue(ctx, launchRepoCtxKey{}, repo)
}

// LaunchRepoFromContext returns the repository stamped by WithLaunchRepo,
// or "" when the launch serves none.
func LaunchRepoFromContext(ctx context.Context) string {
	repo, _ := ctx.Value(launchRepoCtxKey{}).(string)
	return repo
}

// ServesRepoForLaunch reports whether a launch for repo (the forge's full
// path, "owner/repo") may spend this key. An empty Repos admits every
// repository; a restricted key refuses a launch that names no repository
// at all — a key scoped to one repo must never be spent from a surface
// that cannot say which repo it serves.
func (k ApiKey) ServesRepoForLaunch(repo string) bool {
	if len(k.Repos) == 0 {
		return true
	}
	if repo == "" {
		return false
	}
	for _, r := range k.Repos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

func (k ApiKey) listsBot(botID string) bool {
	for _, b := range k.Bots {
		if b == botID {
			return true
		}
	}
	return false
}

// ApiKeyStore is the persistence interface for BYOK records.
type ApiKeyStore interface {
	Create(ctx context.Context, k ApiKey) error
	Get(ctx context.Context, id string) (ApiKey, error)
	// GetOwned reads a PERSONAL key by (id, owner) with no tenant filter.
	//
	// Get is tenant-scoped, which is right for a run spending its own
	// team's keys and wrong for the credential pool: a lent key is read
	// from the BORROWER's context, in another tenant entirely, so Get
	// finds nothing and the caller cannot tell "not yours" from "gone".
	// Ownership replaces tenancy as the boundary here — the key must be
	// user-scoped to ownerUserID, which is strictly narrower than an id
	// lookup, and is the only thing the pool ever lends.
	GetOwned(ctx context.Context, id, ownerUserID string) (ApiKey, error)
	// Patch writes ONLY the fields the patch names and returns the row as
	// stored afterwards. There is deliberately no whole-document writer:
	// a replace carries back whatever a stale read holds (an is_default a
	// concurrent ClearDefault just lowered, a last_used_at the runner just
	// bumped) and drops every column this build does not know — the
	// field a newer server wrote on the same row during a rolling update
	// or a rollback. A patch that names nothing is a read.
	Patch(ctx context.Context, id string, p ApiKeyPatch) (ApiKey, error)
	Delete(ctx context.Context, id string) error
	// ListByTeam returns every key visible from teamID — i.e. team-
	// scoped keys plus the requesting user's user-scoped keys. The
	// requestingUserID filter MUST be applied; passing "" returns
	// only team-wide keys (admin path).
	ListByTeam(ctx context.Context, teamID, requestingUserID string) ([]ApiKey, error)
	// ListByUser returns the requesting user's user-scoped keys
	// inside a given team.
	ListByUser(ctx context.Context, teamID, userID string) ([]ApiKey, error)
	// MarkUsed updates last_used_at without altering anything else.
	MarkUsed(ctx context.Context, id string, at time.Time) error
	// MarkFingerprintUsed bumps last_used_at on every key whose stable
	// audit identity matches the given fingerprint. Called by the runner
	// at the start and end of every attempt so an operator can tell an
	// idle key from one that is actively serving. Match by fingerprint on
	// purpose: the runner knows what the bundle sealed, not the row ids
	// under it. The tenant scope comes from the CONTEXT: with a tenant on
	// it, only that tenant's rows move (a tenant's own key — another
	// tenant holding the byte-identical secret must not read as "in
	// use"); without one, every row carrying the fingerprint moves (a
	// pool-lent or platform-tier key, whose row lives in another tenant
	// and serves every tenant). The runner picks per slot from the
	// bundle's tier markers. A missing fingerprint is a no-op, not an
	// error.
	MarkFingerprintUsed(ctx context.Context, fingerprint string, at time.Time) error
	// ClearDefault removes the is_default flag from any other key in
	// the same (team, user, provider) tuple. Used when a new key is
	// created with is_default=true or an existing one is promoted.
	ClearDefault(ctx context.Context, teamID, userID string, provider Provider, exceptID string) error
}

// Sentinel errors raised by Store implementations.
var (
	ErrApiKeyNotFound = errors.New("secrets: api key not found")
)

// Resolution describes a single resolved key the publisher needs to
// inject for one provider on a given run.
type Resolution struct {
	Provider Provider
	KeyID    string
	// Plaintext is filled by the resolver only if the caller passes
	// a Sealer; without one the resolver returns the sealed blob
	// untouched (handlers that do not need plaintext yet).
	Plaintext   []byte
	SealedBlob  []byte
	SourceScope string // "user" or "team" — for audit logging
	// Fingerprint is the chosen key's stable audit identity, carried so
	// the publisher can stamp the run document with the credentials it
	// actually sealed (the concurrency meter counts alive runs by it).
	Fingerprint string
	// BaseURL is the gateway root of an openai_compatible key, as the
	// record stores it (canonical); "" for every other provider, whatever
	// its row carries. It rides with the plaintext into the bundle
	// (RunBundle.APIBaseURLs): the key is one credential with its URL.
	BaseURL string
}

// Resolve returns at most one ApiKey for each requested provider,
// applying the priority chain documented in the cloud admin plan:
//
//  1. KeyOverrides[provider] — caller-pinned key id (validated to
//     belong to the team and to be visible to userID).
//  2. (team, userID, provider, default=true)
//  3. (team, userID, provider) — first match
//  4. (team, "", provider, default=true)
//  5. (team, "", provider) — first match
//
// Providers without a hit are simply omitted. Callers consult the
// returned map and either inject what's there or fall back to env.
//
// When sealer is non-nil, every Resolution.Plaintext is decrypted; on
// decrypt failure the resolution is skipped and an error is logged
// to logErr. Pass nil sealer to get sealed blobs only.
// A nil usable predicate accepts every key. A non-nil one is consulted in
// the priority walk (pass 2) ONLY: a key it refuses is skipped and the walk
// takes the NEXT visible key of that provider, which is what turns several
// keys of one provider into an ordered fallback chain. An explicit
// keyOverrides pin is deliberately NOT filtered — the operator named that
// key, and honouring the pin over the optimisation is what keeps the
// predicate an optimisation.
func Resolve(
	ctx context.Context,
	store ApiKeyStore,
	teamID, userID string,
	botID string,
	providers []Provider,
	keyOverrides map[Provider]string,
	sealer Sealer,
	usable func(ApiKey) bool,
	onWithheld func(ApiKey, AudienceRefusal),
) (map[Provider]Resolution, error) {
	if teamID == "" {
		return nil, fmt.Errorf("secrets: team id required for resolve")
	}
	if len(providers) == 0 {
		return map[Provider]Resolution{}, nil
	}
	visible, err := store.ListByTeam(ctx, teamID, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[Provider]Resolution, len(providers))
	repo := LaunchRepoFromContext(ctx)
	for _, candidate := range OrderedAPIKeyCandidates(visible, userID, providers, keyOverrides) {
		k := candidate.Key
		if _, already := out[k.Provider]; already {
			continue
		}
		// The workload audience is checked BEFORE the pin exemption, and
		// inside Resolve rather than through the usable-predicate, because
		// it is a gate and not an optimisation: the refused-key restore
		// re-resolves with a nil predicate, so an audience expressed there
		// would hand the key out on exactly the path it exists to close.
		//
		// onWithheld carries the fact out to whoever can say it: a key
		// dropped here leaves NO other trace — the walk simply yields the
		// next key, or nothing, and a caller reading an empty result cannot
		// tell "no key" from "a key I refused". The withheld key travels
		// with the refusal rather than being reconstructed from its absence.
		// The repository audience is the same kind of gate: checked before
		// the pin exemption, from the launch's own repository (WithLaunchRepo),
		// so a key scoped to one repository is never spent for another nor
		// from a surface that names none. Which audience refused travels
		// with the key: a caller told "withheld" and left to guess the cause
		// blames the bot for a repository mismatch.
		why := AudienceRefusal{Bot: !k.ServesBotForLaunch(botID), Repo: !k.ServesRepoForLaunch(repo)}
		if why.Bot || why.Repo {
			if onWithheld != nil {
				onWithheld(k, why)
			}
			continue
		}
		if !candidate.Pinned && usable != nil && !usable(k) {
			continue
		}
		if r, ok := buildResolution(k, sealer, userID); ok {
			out[k.Provider] = r
		}
	}
	return out, nil
}

// AudienceRefusal says which of a key's audiences refused a launch. Resolve
// hands it to onWithheld beside the key so the fact travels with the
// refusal rather than being reconstructed by whoever reports it; both
// flags may be true at once, and a report states every true one.
type AudienceRefusal struct {
	// Bot: the key's Bots list does not name the launch's bot.
	Bot bool
	// Repo: the key's Repos list does not name the launch's repository, or
	// the launch named none.
	Repo bool
}

// APIKeyCandidate is an ordered attempt, before any secret is opened.
// A pin is tried first and deliberately bypasses the availability predicate.
type APIKeyCandidate struct {
	Key    ApiKey
	Pinned bool
}

// OrderedAPIKeyCandidates is the common selection order for live resolution
// and its read-only preview. It copies its input and never decrypts a key.
// Pinned keys also occur in the ordinary walk: if opening a pin fails, Resolve
// retains its historical fallback behavior, including that ordinary attempt.
func OrderedAPIKeyCandidates(visible []ApiKey, userID string, providers []Provider, overrides map[Provider]string) []APIKeyCandidate {
	keys := append([]ApiKey(nil), visible...)
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := keyRank(keys[i], userID), keyRank(keys[j], userID)
		if a != b {
			return a < b
		}
		return keys[i].CreatedAt.Before(keys[j].CreatedAt)
	})
	wanted := make(map[Provider]bool, len(providers))
	out := make([]APIKeyCandidate, 0, len(keys)+len(overrides))
	for _, provider := range providers {
		wanted[provider] = true
		if pin := overrides[provider]; pin != "" {
			for _, key := range keys {
				if key.ID == pin && key.Provider == provider && keyRank(key, userID) != 99 {
					out = append(out, APIKeyCandidate{Key: key, Pinned: true})
					break
				}
			}
		}
	}
	for _, key := range keys {
		if wanted[key.Provider] && keyRank(key, userID) != 99 {
			out = append(out, APIKeyCandidate{Key: key})
		}
	}
	return out
}

// keyRank assigns a sort key. Lower rank = higher priority.
func keyRank(k ApiKey, currentUserID string) int {
	userMatch := currentUserID != "" && k.ScopeUserID == currentUserID
	switch {
	case userMatch && k.IsDefault:
		return 0
	case userMatch:
		return 1
	case k.ScopeUserID == "" && k.IsDefault:
		return 2
	case k.ScopeUserID == "":
		return 3
	}
	// Other users' user-scoped keys never apply.
	return 99
}

// buildResolution decrypts (when sealer != nil) and packages the key
// for the publisher. AAD binds the sealed blob to its api_key id so
// a sealed payload moved between records cannot be opened.
func buildResolution(k ApiKey, sealer Sealer, currentUserID string) (Resolution, bool) {
	scope := "team"
	if k.ScopeUserID == currentUserID && currentUserID != "" {
		scope = "user"
	}
	r := Resolution{
		Provider:    k.Provider,
		KeyID:       k.ID,
		SealedBlob:  k.SealedSecret,
		SourceScope: scope,
		Fingerprint: k.Fingerprint,
	}
	// Only a gateway key's URL travels: a vendor row's base_url column
	// (refused by the routes, writable by hand in the store) is bound by
	// no AAD and would reach the run unauthenticated.
	if k.Provider == ProviderOpenAICompatible {
		r.BaseURL = k.BaseURL
	}
	if sealer == nil {
		return r, true
	}
	pt, err := OpenApiKey(sealer, k)
	if err != nil {
		return Resolution{}, false
	}
	r.Plaintext = pt
	return r, true
}

// OpenApiKey decrypts a stored key. The AAD binds the ciphertext to the
// key's id, so a sealed secret copied onto another record cannot be
// opened. Exported because resolution is no longer the only consumer: the
// credential pool serves a key its donor lends by id, outside the
// team-priority chain Resolve implements.
func OpenApiKey(sealer Sealer, k ApiKey) ([]byte, error) {
	if sealer == nil {
		return nil, fmt.Errorf("secrets: nil sealer for OpenApiKey")
	}
	if err := checkApiKeyAADID(k.ID); err != nil {
		return nil, err
	}
	return sealer.Open(k.SealedSecret, apiKeyAAD(k))
}

// apiKeyAAD is the single definition of a key's additional authenticated
// data — sealing and opening must never disagree on it. Every key binds
// its id; a gateway key also binds the base URL it was sealed with, so a
// row repointed at another host IN THE STORE opens nowhere. (The API's
// own base_url change is a rotation — the caller re-sends the secret —
// so the binding is what makes a store-level repoint fail closed, not
// what keeps a mutator from redirecting a secret they hold.) The NUL
// between id and URL is the delimiter checkApiKeyAADID keeps out of ids.
func apiKeyAAD(k ApiKey) []byte {
	aad := "api_key:" + k.ID
	if k.Provider == ProviderOpenAICompatible {
		aad += "\x00" + k.BaseURL
	}
	return []byte(aad)
}

// checkApiKeyAADID refuses an id carrying the AAD delimiter: with a NUL in
// the id, a vendor row's AAD "api_key:<id>" could spell a gateway row's
// "api_key:<id>\x00<url>" and open a blob sealed for another provider.
// Ids are uuids in production; this keeps the AAD unambiguous for every
// row a test or a hand-written document may construct.
func checkApiKeyAADID(id string) error {
	if strings.ContainsRune(id, 0) {
		return fmt.Errorf("secrets: api key id must not contain NUL (the AAD delimiter)")
	}
	return nil
}

// APIKeyFingerprint is the stable audit identity of a key: the ceiling,
// alive_runs, last_used_at and the spend ledger all key on it. A gateway
// key folds its base URL in — the same token on two gateways is two
// identities — every other provider keeps sha256(secret). The runner
// stamps what the server stamped: the server's fingerprint travels in the
// bundle beside a gateway key (RunBundle.APIKeyFingerprints), and a key
// met without one is recomputed here from the URL the bundle pairs it
// with (pkg/runner loop_credentials.go). The pool and fallback-door
// fallbacks still hash the plaintext alone (cloudpublisher) and move onto
// this helper with #2005 PR-C' — harmless until then: they refuse a
// gateway key by name instead of sealing one — the vocabulary is pinned by
// cloudpublisher's provider-coverage test, the pool lane's refusal by
// TestPoolGatewayRefusal_Drift.
func APIKeyFingerprint(p Provider, secret, baseURL string) string {
	if p == ProviderOpenAICompatible {
		return FingerprintSHA256(string(p) + "\x00" + baseURL + "\x00" + secret)
	}
	return FingerprintSHA256(secret)
}

// SealAPIKey produces the sealed blob for storage. Pass the caller's
// shared Sealer (e.g. ITERION_SECRETS_KEY-driven AESGCMSealer) and
// the freshly-generated key ID; the AAD ties the ciphertext to that
// record so it cannot be moved.
func SealAPIKey(sealer Sealer, keyID string, plaintext []byte) ([]byte, error) {
	return SealAPIKeyFor(sealer, ApiKey{ID: keyID}, plaintext)
}

// SealAPIKeyFor seals plaintext for the record k: its id, and for a gateway
// key its base URL, are bound into the AAD (apiKeyAAD). A gateway key must
// be sealed through this form — SealAPIKey knows no URL and would produce
// a blob OpenApiKey refuses.
func SealAPIKeyFor(sealer Sealer, k ApiKey, plaintext []byte) ([]byte, error) {
	if sealer == nil {
		return nil, errNilSealer
	}
	if err := checkApiKeyAADID(k.ID); err != nil {
		return nil, err
	}
	if k.Provider == ProviderOpenAICompatible && k.BaseURL == "" {
		return nil, fmt.Errorf("secrets: a gateway key is sealed with its base URL; none given for %s", k.ID)
	}
	return sealer.Seal(plaintext, apiKeyAAD(k))
}

// MemoryApiKeyStore is the in-process store used by tests of the
// resolution chain.
type MemoryApiKeyStore struct {
	mu   sync.Mutex
	keys map[string]ApiKey
}

func NewMemoryApiKeyStore() *MemoryApiKeyStore {
	return &MemoryApiKeyStore{keys: make(map[string]ApiKey)}
}

// Create stores the row. Like the Mongo twin it stamps TenantID from the
// context when one is present (the Mongo twin REQUIRES one; the memory
// twin stays usable from bare-context tests), so the tenant-scoped
// MarkFingerprintUsed reads the same field on both.
func (m *MemoryApiKeyStore) Create(ctx context.Context, k ApiKey) error {
	if tenantID, ok := store.TenantFromContext(ctx); ok && tenantID != "" {
		k.TenantID = tenantID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[k.ID] = k
	return nil
}

// Create already stamps the ctx tenant; the reads MUST filter on it too,
// or the double cannot express the failure the Mongo store produces (a row
// written under one tenant, invisible under another) and a tenant-scoping
// regression passes its tests. See stampTenant / visibleToTenant (#997).
func (m *MemoryApiKeyStore) Get(ctx context.Context, id string) (ApiKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok || !visibleToTenant(ctx, k.TenantID) {
		return ApiKey{}, ErrApiKeyNotFound
	}
	return k, nil
}

func (m *MemoryApiKeyStore) GetOwned(_ context.Context, id, ownerUserID string) (ApiKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	// The ownership term is enforced here even though this store ignores
	// tenancy: it is the whole guarantee the method offers, and a memory
	// store that waved it through would let a pool test pass against a
	// key its "donor" does not own.
	if !ok || ownerUserID == "" || k.ScopeUserID != ownerUserID {
		return ApiKey{}, ErrApiKeyNotFound
	}
	return k, nil
}

// ApiKeyPatch names the fields an update may change. A nil field is left
// alone; a non-nil field is written as given, zero values included — so
// `Bots: &[]string{}` clears the audience and `MaxConcurrentRuns: &zero`
// uncaps the key, the distinction a struct marshalled with omitempty
// tags cannot make.
type ApiKeyPatch struct {
	Name              *string
	IsDefault         *bool
	MaxConcurrentRuns *int
	Bots              *[]string
	Repos             *[]string
	// BaseURL is the gateway root the Secret beside it is sealed under. It
	// never travels alone (validate refuses it): the AAD binds the URL, so
	// a row whose URL moved without its blob opens nowhere.
	BaseURL *string
	// Secret rotates the sealed value. Its three derived fields travel
	// together: a sealed blob with a stale last4 or fingerprint would
	// name a credential that no longer exists.
	Secret *ApiKeySecretPatch
	// ExpectSealed, when set, makes the write conditional on the row still
	// holding this sealed value (ErrApiKeyChanged otherwise): the guard of
	// a rotation prepared from a row another writer may have rotated or
	// moved since — without it the stale rotation lands and silently
	// undoes the other writer's change.
	ExpectSealed []byte
}

// ApiKeySecretPatch is the sealed secret with its derived identity.
type ApiKeySecretPatch struct {
	Sealed      []byte
	Last4       string
	Fingerprint string
}

// ErrApiKeyPatchInvalid refuses a patch that cannot be written as one
// consistent row: a rotation missing one of its three fields would leave
// a sealed blob that no fingerprint names, or a fingerprint over a blob
// that cannot be opened.
var ErrApiKeyPatchInvalid = errors.New("secrets: api key patch invalid")

// ErrApiKeyChanged is a compare-and-swap miss: the row's sealed secret is
// no longer the one the caller read. A rotation prepared from that read
// would otherwise land as the last writer and silently undo what landed in
// between — a rotation, or a move away from a gateway the operator no
// longer trusts, reverted to the old host with a fresh secret.
var ErrApiKeyChanged = errors.New("secrets: api key changed since it was read")

// IsEmpty reports whether the patch names no field at all. It reads the
// $set body rather than a second hand-kept list of the fields.
func (p ApiKeyPatch) IsEmpty() bool {
	return len(p.setDoc()) == 0
}

// validate refuses a rotation that is not whole. Both twins call it before
// writing, so an incomplete triplet never reaches either store.
func (p ApiKeyPatch) validate() error {
	if p.Secret != nil && (len(p.Secret.Sealed) == 0 || p.Secret.Last4 == "" || p.Secret.Fingerprint == "") {
		return fmt.Errorf("%w: a rotation carries the sealed value, its last4 and its fingerprint together", ErrApiKeyPatchInvalid)
	}
	if p.BaseURL != nil && p.Secret == nil {
		return fmt.Errorf("%w: a base URL travels with the secret sealed under it — the AAD binds the URL, so a row whose URL moved without its blob opens nowhere", ErrApiKeyPatchInvalid)
	}
	return nil
}

// applyTo writes the named fields onto k for the memory twin. applyTo and
// setDoc enumerate the same fields: TestApiKeyPatch_EveryFieldReachesBothWriters
// walks the struct and refuses a field one of them does not write.
func (p ApiKeyPatch) applyTo(k *ApiKey) {
	if p.Name != nil {
		k.Name = *p.Name
	}
	if p.IsDefault != nil {
		k.IsDefault = *p.IsDefault
	}
	if p.MaxConcurrentRuns != nil {
		k.MaxConcurrentRuns = *p.MaxConcurrentRuns
	}
	if p.Bots != nil {
		k.Bots = append([]string(nil), (*p.Bots)...)
	}
	if p.Repos != nil {
		k.Repos = append([]string(nil), (*p.Repos)...)
	}
	if p.BaseURL != nil {
		k.BaseURL = *p.BaseURL
	}
	if p.Secret != nil {
		k.SealedSecret = append([]byte(nil), p.Secret.Sealed...)
		k.Last4 = p.Secret.Last4
		k.Fingerprint = p.Secret.Fingerprint
	}
}

// setDoc is the Mongo $set body for the named fields. bots is written as
// an array even when empty, so a cleared audience reads back as "every
// bot" rather than as the absent field a nil would leave behind.
func (p ApiKeyPatch) setDoc() bson.M {
	set := bson.M{}
	if p.Name != nil {
		set["name"] = *p.Name
	}
	if p.IsDefault != nil {
		set["is_default"] = *p.IsDefault
	}
	if p.MaxConcurrentRuns != nil {
		set["max_concurrent_runs"] = *p.MaxConcurrentRuns
	}
	if p.Bots != nil {
		bots := *p.Bots
		if bots == nil {
			bots = []string{}
		}
		set["bots"] = bots
	}
	if p.Repos != nil {
		repos := *p.Repos
		if repos == nil {
			repos = []string{}
		}
		set["repos"] = repos
	}
	if p.BaseURL != nil {
		set["base_url"] = *p.BaseURL
	}
	if p.Secret != nil {
		set["sealed_secret"] = p.Secret.Sealed
		set["last4"] = p.Secret.Last4
		set["fingerprint"] = p.Secret.Fingerprint
	}
	return set
}

func (m *MemoryApiKeyStore) Patch(ctx context.Context, id string, p ApiKeyPatch) (ApiKey, error) {
	if err := p.validate(); err != nil {
		return ApiKey{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.keys[id]
	if !ok || !visibleToTenant(ctx, cur.TenantID) {
		return ApiKey{}, ErrApiKeyNotFound
	}
	// The Mongo twin's Patch refuses a context that carries no tenant. A
	// tenant-stamped row must be as unreachable here from a bare context,
	// or a handler that drops the scoped context passes every memory test
	// and fails in production. Rows with no tenant stay writable from any
	// context: that is what the bare-context fixtures need.
	if _, scoped := store.TenantFromContext(ctx); !scoped && cur.TenantID != "" {
		return ApiKey{}, ErrApiKeyTenantMissing
	}
	if p.ExpectSealed != nil && !bytes.Equal(cur.SealedSecret, p.ExpectSealed) {
		return ApiKey{}, ErrApiKeyChanged
	}
	p.applyTo(&cur)
	m.keys[id] = cur
	return cur, nil
}

// Replace overwrites a row wholesale. It exists on the memory twin ONLY,
// for test fixtures that need to move a field no patch names (a key's
// scope, provider or expiry changing behind a pledge's back); the Mongo
// store has no such writer, by design — see ApiKeyStore.Patch.
func (m *MemoryApiKeyStore) Replace(ctx context.Context, k ApiKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.keys[k.ID]
	if !ok || !visibleToTenant(ctx, cur.TenantID) {
		return ErrApiKeyNotFound
	}
	k.TenantID = stampTenant(ctx, cur.TenantID)
	m.keys[k.ID] = k
	return nil
}

func (m *MemoryApiKeyStore) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok || !visibleToTenant(ctx, k.TenantID) {
		return ErrApiKeyNotFound
	}
	delete(m.keys, id)
	return nil
}

func (m *MemoryApiKeyStore) ListByTeam(_ context.Context, teamID, userID string) ([]ApiKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ApiKey
	for _, k := range m.keys {
		if k.ScopeTeamID != teamID {
			continue
		}
		if k.ScopeUserID != "" && k.ScopeUserID != userID {
			continue
		}
		out = append(out, k)
	}
	return out, nil
}

func (m *MemoryApiKeyStore) ListByUser(_ context.Context, teamID, userID string) ([]ApiKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ApiKey
	for _, k := range m.keys {
		if k.ScopeTeamID == teamID && k.ScopeUserID == userID {
			out = append(out, k)
		}
	}
	return out, nil
}

func (m *MemoryApiKeyStore) MarkUsed(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok {
		return ErrApiKeyNotFound
	}
	t := at
	k.LastUsedAt = &t
	m.keys[id] = k
	return nil
}

// MarkFingerprintUsed bumps last_used_at on every key that carries this
// fingerprint — within the context's tenant when it carries one (matched
// on the row's TenantID, the field the Mongo twin filters on), across
// tenants otherwise. Empty fingerprint is a no-op (nothing to look up),
// never an error — a runner metering a key that predates fingerprint
// stamping just leaves the observation on the floor rather than failing
// the report.
func (m *MemoryApiKeyStore) MarkFingerprintUsed(ctx context.Context, fingerprint string, at time.Time) error {
	if fingerprint == "" {
		return nil
	}
	tenantID, scoped := store.TenantFromContext(ctx)
	scoped = scoped && tenantID != ""
	m.mu.Lock()
	defer m.mu.Unlock()
	t := at
	for id, k := range m.keys {
		if k.Fingerprint != fingerprint {
			continue
		}
		if scoped && k.TenantID != tenantID {
			continue
		}
		k.LastUsedAt = &t
		m.keys[id] = k
	}
	return nil
}

func (m *MemoryApiKeyStore) ClearDefault(_ context.Context, teamID, userID string, provider Provider, exceptID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, k := range m.keys {
		if id == exceptID {
			continue
		}
		if k.ScopeTeamID != teamID {
			continue
		}
		if k.ScopeUserID != userID {
			continue
		}
		if k.Provider != provider {
			continue
		}
		if k.IsDefault {
			k.IsDefault = false
			m.keys[id] = k
		}
	}
	return nil
}

// MongoApiKeyStore implements ApiKeyStore on Mongo.
type MongoApiKeyStore struct {
	coll *mongo.Collection
}

const ApiKeysCollectionName = "api_keys"

func NewMongoApiKeyStore(db *mongo.Database) *MongoApiKeyStore {
	return &MongoApiKeyStore{coll: db.Collection(ApiKeysCollectionName)}
}

// EnsureSchema creates the indexes used by the store.
func (s *MongoApiKeyStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "scope_team", Value: 1}, {Key: "scope_user", Value: 1}, {Key: "provider", Value: 1}}, Options: options.Index().SetName("team_user_provider")},
		{Keys: bson.D{{Key: "scope_team", Value: 1}, {Key: "provider", Value: 1}, {Key: "is_default", Value: 1}}, Options: options.Index().SetName("team_provider_default")},
		// MarkFingerprintUsed's predicate: every attempt start and end
		// bumps by fingerprint, with no tenant filter (a lent or platform
		// key moves on its own row). Unindexed it was a collection scan
		// per attempt. Sparse: rows that predate stamping carry none.
		{Keys: bson.D{{Key: "fingerprint", Value: 1}}, Options: options.Index().SetName("fingerprint").SetSparse(true)},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("secrets: ensure api_keys indexes: %w", err)
	}
	return nil
}

func (s *MongoApiKeyStore) Create(ctx context.Context, k ApiKey) error {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return ErrApiKeyTenantMissing
	}
	// Stamp tenant_id from ctx so the caller can't smuggle a different
	// tenant onto a writeable row. The struct field is the source of
	// truth on the wire; we overwrite it here unconditionally.
	k.TenantID = tenantID
	if _, err := s.coll.InsertOne(ctx, k); err != nil {
		return fmt.Errorf("secrets: insert api key: %w", err)
	}
	return nil
}

func (s *MongoApiKeyStore) Get(ctx context.Context, id string) (ApiKey, error) {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return ApiKey{}, err
	}
	return mongoutil.FindOne[ApiKey](ctx, s.coll, filter, ErrApiKeyNotFound, "secrets: get api key")
}

func (s *MongoApiKeyStore) GetOwned(ctx context.Context, id, ownerUserID string) (ApiKey, error) {
	if ownerUserID == "" {
		return ApiKey{}, ErrApiKeyNotFound
	}
	// No tenant term, by design — see the interface doc. scope_user is the
	// boundary: only the key's own owner can be served this way.
	return mongoutil.FindOne[ApiKey](ctx, s.coll,
		bson.M{"_id": id, "scope_user": ownerUserID},
		ErrApiKeyNotFound, "secrets: get owned api key")
}

// Patch is one $set of the named fields, matched by both _id AND
// tenant_id so a tenant can never rewrite another tenant's row by
// guessing an id. Never a replace: the row may carry fields this build
// does not know (written by a newer server beside it in a rolling update,
// or left behind by a rollback), and the caller's copy may be stale on
// the fields it does not name — both must survive untouched.
func (s *MongoApiKeyStore) Patch(ctx context.Context, id string, p ApiKeyPatch) (ApiKey, error) {
	if err := p.validate(); err != nil {
		return ApiKey{}, err
	}
	filter, err := withApiKeyTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return ApiKey{}, err
	}
	if p.IsEmpty() {
		// Nothing to write; the compare-and-swap still answers, as it does
		// on the memory twin: a caller who guarded an empty patch is asking
		// whether the row still holds the value it read.
		cur, err := mongoutil.FindOne[ApiKey](ctx, s.coll, filter, ErrApiKeyNotFound, "secrets: patch api key")
		if err != nil {
			return ApiKey{}, err
		}
		if p.ExpectSealed != nil && !bytes.Equal(cur.SealedSecret, p.ExpectSealed) {
			return ApiKey{}, ErrApiKeyChanged
		}
		return cur, nil
	}
	guarded := filter
	if p.ExpectSealed != nil {
		guarded = make(bson.M, len(filter)+1)
		for k, v := range filter {
			guarded[k] = v
		}
		guarded["sealed_secret"] = p.ExpectSealed
	}
	var out ApiKey
	err = s.coll.FindOneAndUpdate(ctx, guarded, bson.M{"$set": p.setDoc()},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		if p.ExpectSealed == nil {
			return ApiKey{}, ErrApiKeyNotFound
		}
		// Distinguish "gone" from "changed under us": the row exists but
		// its sealed value moved. A read that fails for any other reason
		// is reported as that failure — "not found" would send the caller
		// to a 404 for a store blip.
		_, gerr := mongoutil.FindOne[ApiKey](ctx, s.coll, filter, ErrApiKeyNotFound, "secrets: patch api key")
		switch {
		case gerr == nil:
			return ApiKey{}, ErrApiKeyChanged
		case errors.Is(gerr, ErrApiKeyNotFound):
			return ApiKey{}, ErrApiKeyNotFound
		default:
			return ApiKey{}, fmt.Errorf("secrets: patch api key: compare-and-swap miss, re-read failed: %w", gerr)
		}
	}
	if err != nil {
		return ApiKey{}, fmt.Errorf("secrets: patch api key: %w", err)
	}
	return out, nil
}

func (s *MongoApiKeyStore) Delete(ctx context.Context, id string) error {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	return mongoutil.DeleteOneChecked(ctx, s.coll, filter, ErrApiKeyNotFound, "secrets: delete api key")
}

func (s *MongoApiKeyStore) ListByTeam(ctx context.Context, teamID, userID string) ([]ApiKey, error) {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{
		"scope_team": teamID,
		"$or": []bson.M{
			{"scope_user": bson.M{"$exists": false}},
			{"scope_user": ""},
			{"scope_user": userID},
		},
	})
	if err != nil {
		return nil, err
	}
	cur, err := s.coll.Find(ctx, filter, options.Find().SetSort(bson.M{"created_at": 1}))
	if err != nil {
		return nil, fmt.Errorf("secrets: list api keys: %w", err)
	}
	defer cur.Close(ctx)
	var out []ApiKey
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("secrets: decode api keys: %w", err)
	}
	return out, nil
}

func (s *MongoApiKeyStore) ListByUser(ctx context.Context, teamID, userID string) ([]ApiKey, error) {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{"scope_team": teamID, "scope_user": userID})
	if err != nil {
		return nil, err
	}
	cur, err := s.coll.Find(ctx, filter, options.Find().SetSort(bson.M{"created_at": 1}))
	if err != nil {
		return nil, fmt.Errorf("secrets: list user api keys: %w", err)
	}
	defer cur.Close(ctx)
	var out []ApiKey
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("secrets: decode user api keys: %w", err)
	}
	return out, nil
}

func (s *MongoApiKeyStore) MarkUsed(ctx context.Context, id string, at time.Time) error {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	if _, err := s.coll.UpdateOne(ctx, filter, bson.M{"$set": bson.M{"last_used_at": at}}); err != nil {
		return fmt.Errorf("secrets: mark used: %w", err)
	}
	return nil
}

// MarkFingerprintUsed updates last_used_at on every row matching the
// fingerprint — within the context's tenant when it carries one, across
// tenants otherwise (see ApiKeyStore). A missing fingerprint (empty
// string) is a no-op — the metering path calls this per stamped fp on the
// run doc, and a run predating fingerprint stamping simply carries none.
// UpdateMany matches every matching row in scope: two rows sharing a
// fingerprint inside one tenant (an operator saved the same secret twice)
// still get both bumped.
func (s *MongoApiKeyStore) MarkFingerprintUsed(ctx context.Context, fingerprint string, at time.Time) error {
	if fingerprint == "" {
		return nil
	}
	filter := bson.M{"fingerprint": fingerprint}
	if tenantID, ok := store.TenantFromContext(ctx); ok && tenantID != "" {
		filter["tenant_id"] = tenantID
	}
	if _, err := s.coll.UpdateMany(ctx, filter, bson.M{"$set": bson.M{"last_used_at": at}}); err != nil {
		return fmt.Errorf("secrets: mark fingerprint used: %w", err)
	}
	return nil
}

func (s *MongoApiKeyStore) ClearDefault(ctx context.Context, teamID, userID string, provider Provider, exceptID string) error {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{
		"scope_team": teamID,
		"provider":   provider,
		"is_default": true,
	})
	if err != nil {
		return err
	}
	if userID == "" {
		// Team-wide rows are persisted with scope_user omitted (the
		// field has `bson:"...,omitempty"`), so a literal "" filter
		// never matches them and the "at most one default per tuple"
		// invariant silently fractures. Match both absent and empty
		// forms here. See pattern in ListByTeam.
		filter["$or"] = []bson.M{
			{"scope_user": bson.M{"$exists": false}},
			{"scope_user": ""},
		}
	} else {
		filter["scope_user"] = userID
	}
	if exceptID != "" {
		filter["_id"] = bson.M{"$ne": exceptID}
	}
	if _, err := s.coll.UpdateMany(ctx, filter, bson.M{"$set": bson.M{"is_default": false}}); err != nil {
		return fmt.Errorf("secrets: clear default: %w", err)
	}
	return nil
}

// NewApiKeyID returns a fresh UUID-string id for an ApiKey record.
// Centralised so the routes layer doesn't reach for uuid directly.
func NewApiKeyID() string {
	return uuid.NewString()
}

// ParseProvider returns Provider when s matches one of the known
// names (case-insensitive) or an error otherwise.
func ParseProvider(s string) (Provider, error) {
	p := Provider(strings.ToLower(strings.TrimSpace(s)))
	if !p.Valid() {
		return "", fmt.Errorf("unknown provider %q", s)
	}
	return p, nil
}
