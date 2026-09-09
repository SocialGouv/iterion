package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// OAuthKind enumerates the third-party CLIs whose OAuth subscription
// (forfait) iterion can drive on behalf of an authenticated user. The
// names match the delegate.Backend slug so cloudpublisher and the
// runner can resolve them mechanically.
type OAuthKind string

const (
	OAuthKindClaudeCode OAuthKind = "claude_code"
	OAuthKindCodex      OAuthKind = "codex"
)

func (k OAuthKind) Valid() bool {
	switch k {
	case OAuthKindClaudeCode, OAuthKindCodex:
		return true
	}
	return false
}

// OrgOwnerPrefix marks an OAuthRecord whose owner is a team/org rather
// than an individual user. An org-scoped forfait is stored as an
// ordinary OAuthRecord whose UserID is OrgOwnerKey(tenantID) — this
// reuses the whole store/seal/refresh machinery (AAD, Mongo id,
// ExpiringBefore) without a schema change. The cloud publisher uses
// these as a FALLBACK when the run's owner has no personal record,
// covering automated runs (webhook/dispatcher/cron) whose owner is a
// synthetic identity. See OrgOwnerKey.
const OrgOwnerPrefix = "org:"

// OrgOwnerKey returns the synthetic owner key under which a team/org's
// shared forfait credential is stored.
func OrgOwnerKey(tenantID string) string { return OrgOwnerPrefix + tenantID }

// PlatformOwnerKey is the synthetic owner key under which the DEPLOYMENT's
// own forfait credential is stored — the DB-backed form of the platform
// fallback that historically lived only in runner-pod env
// (CLAUDE_CODE_OAUTH_TOKEN et al.). Same OrgOwnerKey trick: an ordinary
// OAuthRecord under a reserved owner reuses the whole store/seal/refresh
// machinery without a schema change. The cloud publisher consults it LAST
// (after user, org, and the mutualised pool) for the OAuth kinds a run
// still lacks; managed by super-admins via /api/admin/llm/oauth. Shares
// its literal with PlatformTenantID (see platformScope in byok.go) — one
// concept, two index namespaces.
const PlatformOwnerKey = platformScope

// OAuthRecord is the per-(user, kind) sealed credential bundle.
//
// SealedPayload is opaque to iterion — it holds the verbatim
// credentials.json (Anthropic) or auth.json (OpenAI Codex) blob the
// user uploaded, sealed with the master key bound to the record id.
// We never decrypt for display; the only consumer is the runner,
// which materialises the file in a tmpdir and points the CLI at it.
//
// AccessTokenExpiresAt is captured separately from the sealed blob
// so the refresh worker can identify expiring records without
// decrypting. Best-effort: providers without an access-token expiry
// (or when the user pasted only the refresh token) leave it zero
// and the worker skips them.
type OAuthRecord struct {
	ID                   string     `bson:"_id" json:"id"`
	UserID               string     `bson:"user_id" json:"user_id"`
	Kind                 OAuthKind  `bson:"kind" json:"kind"`
	SealedPayload        []byte     `bson:"sealed_payload" json:"-"`
	Scopes               []string   `bson:"scopes,omitempty" json:"scopes,omitempty"`
	AccessTokenExpiresAt *time.Time `bson:"access_token_expires_at,omitempty" json:"access_token_expires_at,omitempty"`
	LastRefreshedAt      *time.Time `bson:"last_refreshed_at,omitempty" json:"last_refreshed_at,omitempty"`
	// NotRefreshable marks a payload that carries no refresh token: the
	// refresh worker and manual refresh must skip it — only a re-connect
	// can renew it. Inverted polarity so legacy records (field absent =
	// false) keep being attempted; the first ErrNotRefreshable outcome
	// self-heals them by setting this flag.
	NotRefreshable bool `bson:"not_refreshable,omitempty" json:"not_refreshable,omitempty"`
	// Fingerprint is the audit identity of the SUBSCRIPTION behind this
	// record: stamped when a human connects/pastes credentials, PRESERVED
	// by the automatic refresh worker (whose rewrites are the same
	// account), self-healed on legacy records at their first refresh. It
	// is what downstream metering keys on — re-posting credentials is the
	// act that says "different subscription", so it re-stamps.
	Fingerprint string `bson:"fingerprint,omitempty" json:"fingerprint,omitempty"`
	// AccountLabel names the ACCOUNT this credential belongs to, in the
	// operator's own words ("jothedev", "SocialGouv Revi"). Nothing else
	// in the record identifies it: the payload is sealed, and the runtime
	// logs print only the fingerprint — so answering "whose subscription
	// is this run spending?" meant grepping server logs and correlating
	// hex by hand. Purely descriptive; no resolution path reads it.
	//
	// No bson omitempty: the Mongo store writes records through $set, and
	// an omitted key leaves the OLD value in place — so clearing the label
	// would report success and keep the stale name.
	AccountLabel string `bson:"account_label" json:"account_label,omitempty"`
	// RefreshClaimOwner / RefreshNotBefore fence the ONE refresh exchange a
	// record may have in flight, and hold the sweep off a record it must
	// not retry yet. They are the record's scheduling state, never a
	// credential fact — nothing outside the refresh paths reads them, which
	// is why both are `json:"-"`.
	//
	// The claim exists because a refresh is NOT an atomic write: the caller
	// reads the record, spends a round trip at the provider, then persists.
	// OpenAI rotates the refresh token on that round trip, so two holders
	// exchanging concurrently retire each other's token and the credential
	// dies until a human re-connects it (measured:
	// docs/bot-runs/feed-watch.md). Every server replica runs its own
	// OAuthRefreshWorker with no leader election, so "one refresher" is a
	// property that has to be enforced per record, not assumed.
	//
	// RefreshClaimOwner is a fencing token minted per attempt, never a
	// replica identity: the commit (UpdateTokens with ClaimOwner set) is
	// conditional on it, so a refresher whose claim expired — or was
	// superseded by a re-connect, which clears both fields — discards its
	// exchange instead of overwriting the credential that replaced it.
	//
	// RefreshNotBefore doubles as a cool-down when no owner holds it: a
	// refresh that succeeded but yielded no readable expiry leaves the
	// record inside ExpiringBefore's window forever, and without a
	// cool-down every sweep would re-run the exchange (and rotate the
	// refresh token) every 10 minutes for good.
	//
	// No bson omitempty on either: the Mongo store writes through $set, so
	// an omitted key would leave a stale claim in place — the trap already
	// documented on AccountLabel. Clearing has to travel on the wire.
	RefreshClaimOwner string     `bson:"refresh_claim_owner" json:"-"`
	RefreshNotBefore  *time.Time `bson:"refresh_not_before" json:"-"`
	CreatedAt         time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt         time.Time  `bson:"updated_at" json:"updated_at"`
}

// OAuthStore is the persistence interface for sealed OAuth records.
type OAuthStore interface {
	Upsert(ctx context.Context, rec OAuthRecord) error
	Get(ctx context.Context, userID string, kind OAuthKind) (OAuthRecord, error)
	ListByUser(ctx context.Context, userID string) ([]OAuthRecord, error)
	Delete(ctx context.Context, userID string, kind OAuthKind) error
	// ExpiringBefore returns records whose access token is set and
	// expires before t — used by the background refresh worker.
	ExpiringBefore(ctx context.Context, t time.Time) ([]OAuthRecord, error)
	// SetAccountLabel writes ONLY the label (and updated_at) of an existing
	// record; "" clears it. A rename must not travel through Upsert: that
	// rewrites the whole record, sealed payload included, so a refresh
	// committed between the caller's Get and its Upsert would be reverted
	// to a token the provider may already have rotated out. Missing record
	// → ErrOAuthNotFound.
	SetAccountLabel(ctx context.Context, userID string, kind OAuthKind, label string) error
	// UpdateTokens writes ONLY the keys a token refresh owns — the mirror
	// of SetAccountLabel on the other side of the same race. A refresh
	// reads a record, spends a round trip at the provider, then persists;
	// through Upsert that persist carries the WHOLE record it read back,
	// reverting a rename committed in the meantime to the label it happened
	// to hold. Missing record → ErrOAuthNotFound. Upsert is left to the
	// connect paths, which legitimately replace the record.
	//
	// When upd.ClaimOwner is set the write is CONDITIONAL on still holding
	// that claim: ErrRefreshClaimLost and no write otherwise — and that
	// sentinel then covers a record that VANISHED under the holder too,
	// since a claim is all a fenced write can ask about (the Mongo twin
	// reads one MatchedCount for both). ErrOAuthNotFound stays the answer
	// for an unclaimed write, which is the shape the self-heal uses.
	UpdateTokens(ctx context.Context, userID string, kind OAuthKind, upd OAuthTokenUpdate) error
	// ClaimRefresh elects the ONE holder allowed to exchange this record's
	// refresh token, by compare-and-swap: it succeeds only while nobody
	// holds a live claim (RefreshNotBefore absent or already past), and
	// stamps owner + until when it does. Returns false — not an error —
	// when someone else holds it; that caller must not touch the provider.
	// A crashed holder's claim is re-claimable as soon as `until` passes,
	// so nothing has to release it. Missing record → false, no error.
	ClaimRefresh(ctx context.Context, userID string, kind OAuthKind, owner string, now, until time.Time) (bool, error)
	// ReleaseRefreshClaim hands the claim back without writing tokens — the
	// path a FAILED exchange takes, so the next sweep may retry at once
	// instead of waiting the lease out. Conditional on still owning the
	// claim (ErrRefreshClaimLost otherwise), so a slow holder can never
	// free its successor's. notBefore sets the cool-down the sweep must
	// respect afterwards; nil clears it.
	ReleaseRefreshClaim(ctx context.Context, userID string, kind OAuthKind, owner string, notBefore *time.Time) error
}

// ErrRefreshClaimLost is the outcome of a refresh whose claim no longer
// holds at commit time — the lease expired under a slow exchange, or a
// re-connect replaced the credential while the provider round trip was in
// flight. It is not a failure of the exchange but a verdict on it: the
// tokens just obtained belong to a session that is no longer the stored
// one, so they are DISCARDED rather than written over what replaced them.
var ErrRefreshClaimLost = errors.New("secrets: oauth refresh claim lost")

// OAuthTokenUpdate is the set of fields a refresh (or its self-heal) may
// rewrite. Everything absent from it belongs to another writer — the
// account label to the rename endpoint, created_at to the connect path —
// and is left exactly as stored.
//
// A nil/empty field means "leave it alone", never "clear it": a refresh
// only ever learns MORE about a record. The two shapes that rely on it are
// the self-heal (flips NotRefreshable on a record it never re-sealed) and
// a provider response that carries no expiry or no scopes, which
// RefreshRecord already treats as "keep what we had".
type OAuthTokenUpdate struct {
	// SealedPayload replaces the sealed blob; nil leaves it in place.
	SealedPayload []byte
	// AccessTokenExpiresAt / LastRefreshedAt replace their fields; nil
	// leaves them in place.
	AccessTokenExpiresAt *time.Time
	LastRefreshedAt      *time.Time
	// Scopes replaces the scope list; empty leaves it in place.
	Scopes []string
	// Fingerprint stamps the subscription identity on a legacy record;
	// "" leaves whatever is stored (a refresh is the same subscription,
	// so it never re-stamps a record that already carries one).
	Fingerprint string
	// NotRefreshable is always written: a successful refresh proves the
	// record IS refreshable, and the self-heal path exists to set it.
	NotRefreshable bool
	// ClaimOwner is a PRECONDITION, not a field write: when set, the store
	// commits only while the record still carries that claim owner
	// (ErrRefreshClaimLost otherwise) and releases the claim as part of the
	// same write. Empty means "no claim was taken" and leaves the claim
	// fields exactly as stored — the shape the self-heal partial writes use.
	ClaimOwner string
	// RefreshNotBefore is the cool-down to leave behind when releasing the
	// claim, and — like NotRefreshable — it is always written on a claimed
	// commit, nil meaning "cleared". Leaving a stale cool-down in place
	// would hold the sweep off a record the refresh just made schedulable
	// again. Ignored when ClaimOwner is empty.
	RefreshNotBefore *time.Time
}

// OAuthTokenUpdateFrom projects the refresh-owned fields out of a record
// RefreshRecord has just mutated in place. One projection shared by every
// refresh site, so a field added to RefreshRecord's output cannot reach
// only some of them.
func OAuthTokenUpdateFrom(rec OAuthRecord) OAuthTokenUpdate {
	return OAuthTokenUpdate{
		SealedPayload:        rec.SealedPayload,
		AccessTokenExpiresAt: rec.AccessTokenExpiresAt,
		LastRefreshedAt:      rec.LastRefreshedAt,
		Scopes:               rec.Scopes,
		Fingerprint:          rec.Fingerprint,
		NotRefreshable:       rec.NotRefreshable,
		// RefreshNotBefore travels too: RefreshRecord sets it when it
		// refreshed a token it could not date, and that cool-down is the
		// only thing keeping the record out of the next sweep.
		RefreshNotBefore: rec.RefreshNotBefore,
	}
}

// WithClaim returns the update fenced by a refresh claim: the store then
// commits only while that claim still holds, and releases it in the same
// write. The claim owner is never taken from the record — a record read
// before the claim was taken carries the previous owner, and fencing on
// that would defeat the mechanism.
func (u OAuthTokenUpdate) WithClaim(owner string) OAuthTokenUpdate {
	u.ClaimOwner = owner
	return u
}

// ErrOAuthNotFound is the sentinel for missing records.
var ErrOAuthNotFound = errors.New("secrets: oauth record not found")

// SealOAuthPayload encrypts the raw credentials JSON. AAD binds the
// ciphertext to (userID, kind) so a sealed payload moved between
// users or kinds cannot be opened.
func SealOAuthPayload(sealer Sealer, userID string, kind OAuthKind, payload []byte) ([]byte, error) {
	if sealer == nil {
		return nil, errors.New("secrets: nil sealer for SealOAuthPayload")
	}
	return sealer.Seal(payload, oauthAAD(userID, kind))
}

// OpenOAuthPayload is the inverse: returns the raw JSON blob.
func OpenOAuthPayload(sealer Sealer, userID string, kind OAuthKind, sealed []byte) ([]byte, error) {
	if sealer == nil {
		return nil, errors.New("secrets: nil sealer for OpenOAuthPayload")
	}
	return sealer.Open(sealed, oauthAAD(userID, kind))
}

func oauthAAD(userID string, kind OAuthKind) []byte {
	return []byte("oauth:" + userID + ":" + string(kind))
}

// AnthropicCredentialsView is the minimal shape we extract from a
// Claude Code credentials.json blob to drive expiry tracking + refresh.
// We do NOT replace the user-supplied JSON with this struct on store —
// extra fields the CLI cares about round-trip via the sealed payload.
type AnthropicCredentialsView struct {
	ClaudeAIOauth struct {
		AccessToken  string   `json:"accessToken"`
		RefreshToken string   `json:"refreshToken"`
		ExpiresAt    int64    `json:"expiresAt"` // ms epoch
		Scopes       []string `json:"scopes,omitempty"`
	} `json:"claudeAiOauth"`
}

// CodexCredentialsView is the analogous shape for the Codex CLI's
// auth.json. Field names mirror the Codex SDK.
//
// AuthMode is "apikey" or "chatgpt" — Codex CLI sets it based on how the
// user signed in. Tokens.AccountID is only populated in "chatgpt" mode and
// is required by the ChatGPT-Codex backend (sent verbatim in the
// `ChatGPT-Account-ID` request header).
type CodexCredentialsView struct {
	AuthMode string `json:"auth_mode,omitempty"`
	Tokens   struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token,omitempty"`
		ExpiresIn    int64  `json:"expires_in,omitempty"`
		AccountID    string `json:"account_id,omitempty"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh,omitempty"`
}

// OAuthClientID returns the OAuth client the credential was minted for,
// read from the credential itself rather than configured.
//
// Codex tokens name their own client: the access token carries a
// `client_id` claim and the id token an `aud` matching it. That makes the
// blob self-describing, which matters because the value is NOT one
// well-known constant — a credential minted by the CLI and one minted by
// another first-party client carry different ids, so a hardcoded default
// would refresh some operators' forfaits and silently fail others'.
//
// The claims are read WITHOUT verifying the signature, which is correct
// here and would be wrong elsewhere: the token is not being trusted as
// proof of anything. It is our own stored credential, and the only thing
// taken from it is the address to send its refresh to — a request that
// simply fails if the value is wrong.
//
// Returns "" when neither token carries the claim; callers then fall back
// to an explicitly configured id.
func (v CodexCredentialsView) OAuthClientID() string {
	if id := jwtStringClaim(v.Tokens.AccessToken, "client_id"); id != "" {
		return id
	}
	return jwtFirstAudience(v.Tokens.IDToken)
}

// AccessTokenExpiry returns when the credential's access token stops being
// accepted, read from the token's own `exp` claim.
//
// The blob has no absolute expiry field of its own: `expires_in` is
// relative to a refresh that may have happened days ago, and `last_refresh`
// dates the write, not the token. The claim is the only self-contained
// answer. Returns the zero time when the token is absent or carries no
// numeric `exp`.
func (v CodexCredentialsView) AccessTokenExpiry() time.Time {
	exp, ok := jwtClaims(v.Tokens.AccessToken)["exp"].(float64)
	if !ok || exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(exp), 0).UTC()
}

// jwtClaims decodes a JWT payload segment without verifying the signature.
// Returns nil for anything that is not a three-segment token with a JSON
// payload.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

func jwtStringClaim(token, name string) string {
	s, _ := jwtClaims(token)[name].(string)
	return strings.TrimSpace(s)
}

// jwtFirstAudience reads `aud`, which OIDC allows to be either a string or
// an array of strings.
func jwtFirstAudience(token string) string {
	switch aud := jwtClaims(token)["aud"].(type) {
	case string:
		return strings.TrimSpace(aud)
	case []any:
		for _, a := range aud {
			if s, ok := a.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// IsChatGPTMode reports whether the auth blob authorises ChatGPT-Codex
// backend access (forfait), with the access token + account id required
// to actually issue requests.
func (v CodexCredentialsView) IsChatGPTMode() bool {
	return v.AuthMode == "chatgpt" && v.Tokens.AccessToken != "" && v.Tokens.AccountID != ""
}

// ParseAnthropicView extracts the lightweight metadata view from a
// raw credentials.json blob. Returns the parsed view; errors when the
// JSON is malformed but never inspects scopes / expiry validity.
func ParseAnthropicView(payload []byte) (AnthropicCredentialsView, error) {
	var v AnthropicCredentialsView
	if err := json.Unmarshal(payload, &v); err != nil {
		return v, fmt.Errorf("secrets: parse credentials.json: %w", err)
	}
	return v, nil
}

// SetupTokenAssumedLifetime is how long iterion treats a bare
// `claude setup-token` credential as valid.
//
// The token carries no expiry of its own — it is an opaque string — so a
// value has to be chosen, and the choice is asymmetric. A record with NO
// expiry is read by the Claude CLI as "Not logged in" and never serves a
// run; an expiry that is too SHORT retires a live credential in silence;
// one that is too long only means the eventual refusal arrives from the
// provider, loudly, at the call. So it errs long.
const SetupTokenAssumedLifetime = 365 * 24 * time.Hour

// setupTokenScope is the one scope a wrapped setup token declares. At
// least one is required (an empty list is the other half of what the CLI
// reads as "Not logged in"), and inference is what the token is for.
const setupTokenScope = "user:inference"

// AnthropicBlob is an Anthropic credential in the shape the rest of the
// system expects, plus the bytes that identify the SUBSCRIPTION behind it.
//
// The two are not the same thing, and conflating them is a live hazard:
// wrapping a setup token stamps a computed expiry into the payload, so
// hashing the payload would hand the same token a different fingerprint on
// every upload — a fresh usage meter and a dropped account label each time.
type AnthropicBlob struct {
	// Payload is what to seal and hand to a run: always credentials.json.
	Payload []byte
	// Identity is what to fingerprint. Stable across re-uploads of the
	// same credential.
	Identity []byte
	// Wrapped reports that the input was a bare setup token rather than a
	// credentials.json — the caller says so, since the record's expiry is
	// then iterion's assumption and not a provider statement.
	Wrapped bool
}

// NormalizeAnthropicBlob accepts either of the two shapes an operator
// actually holds — the `credentials.json` of a logged-in Claude Code, or
// the bare `sk-ant-oat…` token `claude setup-token` prints — and returns
// the credentials.json shape for both.
//
// The bare token is the common case for provisioning a team or the
// platform tier (nobody logs a shared account into a local CLI just to
// export its file), and refusing it only moved the wrapping into every
// operator's shell, where the expiry convention had to be re-guessed.
//
// A blob that is neither is returned untouched, so it reaches the JSON
// parser and earns the existing typed refusal rather than a vaguer one.
func NormalizeAnthropicBlob(blob []byte, now time.Time) (AnthropicBlob, error) {
	token := strings.TrimSpace(string(blob))
	if !strings.HasPrefix(token, anthropicOAuthTokenPrefix) {
		return AnthropicBlob{Payload: blob, Identity: blob}, nil
	}
	if err := ValidateTokenShape("setup token", token); err != nil {
		return AnthropicBlob{}, err
	}
	var v AnthropicCredentialsView
	v.ClaudeAIOauth.AccessToken = token
	v.ClaudeAIOauth.ExpiresAt = now.Add(SetupTokenAssumedLifetime).UnixMilli()
	v.ClaudeAIOauth.Scopes = []string{setupTokenScope}
	payload, err := json.Marshal(v)
	if err != nil {
		return AnthropicBlob{}, fmt.Errorf("secrets: wrap setup token as credentials.json: %w", err)
	}
	// The TOKEN identifies the subscription, not the wrapper built around
	// it — and it identifies it better than a credentials.json does, since
	// two exports of one logged-in account differ byte for byte.
	return AnthropicBlob{Payload: payload, Identity: []byte(token), Wrapped: true}, nil
}

// ParseCodexView extracts the analogous view from auth.json.
func ParseCodexView(payload []byte) (CodexCredentialsView, error) {
	var v CodexCredentialsView
	if err := json.Unmarshal(payload, &v); err != nil {
		return v, fmt.Errorf("secrets: parse auth.json: %w", err)
	}
	return v, nil
}

// SubscriptionFingerprint returns the audit identity of the SUBSCRIPTION
// behind an OAuth credentials payload — what the usage-cap meter keys on,
// so runs that spend one account share one ledger.
//
// It prefers a stable account identifier the payload names, because the
// blob itself is not one: the same subscription connected twice (org-level
// and personally, or re-pasted after a token looked broken) serialises
// differently every time, and hashing the blob would open a second meter
// that starts empty and admits a run the first meter would have parked.
// Codex's auth.json carries `tokens.account_id`, which is exactly that
// identifier; the hash is namespaced so an account-derived identity can
// never be confused with a blob-derived one.
//
// KNOWN GAP — Anthropic: a Claude Code credentials.json carries no account
// or subscription id (see AnthropicCredentialsView, and the token exchange
// in oauth_authcode.go, which returns only tokens/scopes/expiry), so it
// falls back to the whole-blob hash and re-connecting the SAME Claude
// subscription still opens a fresh meter. That fails OPEN — the next run
// proceeds and republishes the provider's own reading at its first call —
// and the mid-run guard remains the backstop. Closing it needs an identity
// from outside the payload (an Anthropic profile lookup at connect time),
// not a different hash of it.
func SubscriptionFingerprint(kind OAuthKind, payload []byte) string {
	if kind == OAuthKindCodex {
		if v, err := ParseCodexView(payload); err == nil && v.Tokens.AccountID != "" {
			return fingerprintHex("oauth-account:" + string(kind) + ":" + v.Tokens.AccountID)
		}
	}
	return fingerprintHex(string(payload))
}

// CodexAuthJSONPath returns the on-disk location of Codex CLI's auth.json,
// honouring the `CODEX_HOME` env var (Codex's documented override) and
// falling back to `~/.codex/auth.json`. Returns an empty string when no
// home directory is resolvable, leaving callers to treat it as "no auth".
func CodexAuthJSONPath() string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

// LoadCodexCredentialsFrom reads and parses Codex CLI's auth.json from an
// EXPLICIT CODEX_HOME-shaped directory (`<dir>/auth.json`), rather than the
// process's default location. This is the cloud path: the runner materialises
// a tenant's resolved codex OAuth-forfait into a per-run temp dir
// (Credentials.OAuthDir("codex")), and the in-process claw model factory reads
// it from there instead of the pod's (empty) ~/.codex. Empty dir → error.
func LoadCodexCredentialsFrom(dir string) (CodexCredentialsView, error) {
	if strings.TrimSpace(dir) == "" {
		return CodexCredentialsView{}, fmt.Errorf("secrets: empty codex credentials dir")
	}
	path := filepath.Join(dir, "auth.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return CodexCredentialsView{}, fmt.Errorf("secrets: read %s: %w", path, err)
	}
	return ParseCodexView(data)
}

// LoadAnthropicCredentialsFrom reads and parses a Claude Code
// .credentials.json from an EXPLICIT CLAUDE_CONFIG_DIR-shaped directory, the
// anthropic twin of LoadCodexCredentialsFrom. This is the cloud path: the
// runner materialises the tenant's resolved Claude forfait into a per-run temp
// dir (Credentials.OAuthDir("claude_code")), where the in-process claw factory
// reads it instead of the pod's (empty) ~/.claude. Empty dir → error.
func LoadAnthropicCredentialsFrom(dir string) (AnthropicCredentialsView, error) {
	if strings.TrimSpace(dir) == "" {
		return AnthropicCredentialsView{}, fmt.Errorf("secrets: empty claude_code credentials dir")
	}
	path := filepath.Join(dir, ClaudeCodeCredentialsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return AnthropicCredentialsView{}, fmt.Errorf("secrets: read %s: %w", path, err)
	}
	return ParseAnthropicView(data)
}

// AnthropicForfaitAccessToken returns the Claude Code OAuth access token
// materialised in a CLAUDE_CONFIG_DIR-shaped dir, or "" for every failure
// mode (no dir, absent file, malformed JSON, blank token) — a missing file is
// the ordinary "not a forfait host" case, not an error to propagate.
//
// It exists because three readers were extracting the same field from the same
// file independently (the CLI env builder, the forfait usage prober, the claw
// ctx factory); the fourth would have drifted. The token is never logged.
// ErrAnthropicForfaitExpired names the one failure mode that is not "there is
// no credential here": a forfait WAS provisioned into this dir and its access
// token has lapsed. Callers that can still fall back keep treating it as
// absent; callers whose fall-back is an unauthenticated client must surface it,
// because a lapsed blob is a refresh that did not happen, not a host without a
// subscription.
var ErrAnthropicForfaitExpired = errors.New("secrets: the materialised Claude Code forfait has expired")

// AnthropicForfaitToken returns the usable access token in dir, or an error
// naming why there is none — ErrAnthropicForfaitExpired when the blob lapsed,
// a read/parse error otherwise, and (\"\", nil) when the dir simply holds no
// token. AnthropicForfaitAccessToken is the "usable or not" shorthand over it.
func AnthropicForfaitToken(dir string) (string, error) {
	view, err := LoadAnthropicCredentialsFrom(dir)
	if err != nil {
		return "", err
	}
	if exp := view.ClaudeAIOauth.ExpiresAt; exp > 0 && time.UnixMilli(exp).Before(time.Now()) {
		return "", ErrAnthropicForfaitExpired
	}
	return strings.TrimSpace(view.ClaudeAIOauth.AccessToken), nil
}

func AnthropicForfaitAccessToken(dir string) string {
	view, err := LoadAnthropicCredentialsFrom(dir)
	if err != nil {
		return ""
	}
	// An expired blob is worse than no blob. Baked into a process-lifetime
	// client cache it answers 401 with nothing naming expiry as the cause, and
	// the CLI path degrades better without it — claude re-reads and refreshes
	// the file itself. `expiresAt == 0` means the payload states no expiry, not
	// that it expired.
	if exp := view.ClaudeAIOauth.ExpiresAt; exp > 0 && time.UnixMilli(exp).Before(time.Now()) {
		return ""
	}
	return strings.TrimSpace(view.ClaudeAIOauth.AccessToken)
}

// AnthropicForfaitWireOK reports whether a Claude subscription bearer may be
// sent to baseURL. Only the real Anthropic API qualifies: an empty value (the
// SDK default) or the exact host api.anthropic.com.
//
// Everything else is a destination the OPERATOR chose — a z.ai/bigmodel facade
// that wants the token as an x-api-key-style key, a corporate gateway, an
// interception proxy — and a forfait bearer is not a scoped key: it carries the
// whole Claude account. On a cloud deployment the base URL is set by the
// platform while the credential belongs to the tenant, so the consent gap is
// wider there than on a laptop, never narrower.
//
// It is deliberately ONE predicate: the desktop factory, the per-run ctx
// factory and pkg/supervise's funding check all decide this same question, and
// a supervisor that calls anthropic funded for a wire the registry then
// declines is the disagreement this package keeps paying for.
func AnthropicForfaitWireOK(baseURL string) bool {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "api.anthropic.com")
}

// ClaudeCodeConfigDir returns the on-disk CLAUDE_CONFIG_DIR the Claude Code
// CLI stores its forfait credentials in, honouring the env override and
// falling back to `~/.claude`. Returns "" when no home directory resolves,
// which callers treat as "no forfait on this host".
func ClaudeCodeConfigDir() string {
	if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// AnthropicForfaitAccessTokenFromDisk is the desktop twin of
// LoadCodexCredentialsFromDisk: the Claude Code forfait token from this host's
// own config dir, or "" when there is none. Used by the in-process claw
// anthropic factory so a laptop with a Claude subscription authenticates the
// same way one with a ChatGPT subscription already did.
func AnthropicForfaitAccessTokenFromDisk() string {
	return AnthropicForfaitAccessToken(ClaudeCodeConfigDir())
}

// LoadCodexCredentialsFromDisk reads and parses Codex CLI's auth.json from
// its standard location. Returns the parsed view on success; on missing or
// malformed file it returns the zero view plus a non-nil error. Callers
// gating on availability should use `errors.Is(err, fs.ErrNotExist)` to
// distinguish "no auth installed" from "auth file is corrupted".
//
// The reader does not validate token expiry — refresh is delegated to
// Codex CLI's background process; iterion just reads whatever access_token
// is currently materialised on disk.
func LoadCodexCredentialsFromDisk() (CodexCredentialsView, error) {
	path := CodexAuthJSONPath()
	if path == "" {
		return CodexCredentialsView{}, fmt.Errorf("secrets: no codex auth.json path resolvable (set CODEX_HOME or HOME)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return CodexCredentialsView{}, fmt.Errorf("secrets: read %s: %w", path, err)
	}
	return ParseCodexView(data)
}

// MemoryOAuthStore — for tests.
type MemoryOAuthStore struct {
	mu sync.Mutex
	m  map[string]OAuthRecord
}

func NewMemoryOAuthStore() *MemoryOAuthStore {
	return &MemoryOAuthStore{m: make(map[string]OAuthRecord)}
}

func mkOAuthKey(userID string, kind OAuthKind) string {
	return userID + "|" + string(kind)
}

func (s *MemoryOAuthStore) Upsert(_ context.Context, rec OAuthRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.ID == "" {
		rec.ID = mkOAuthKey(rec.UserID, rec.Kind)
	}
	s.m[mkOAuthKey(rec.UserID, rec.Kind)] = rec
	return nil
}

func (s *MemoryOAuthStore) Get(_ context.Context, userID string, kind OAuthKind) (OAuthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[mkOAuthKey(userID, kind)]
	if !ok {
		return OAuthRecord{}, ErrOAuthNotFound
	}
	return r, nil
}

func (s *MemoryOAuthStore) ListByUser(_ context.Context, userID string) ([]OAuthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []OAuthRecord
	for _, r := range s.m {
		if r.UserID == userID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *MemoryOAuthStore) Delete(_ context.Context, userID string, kind OAuthKind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Report ErrOAuthNotFound for a missing key, matching MongoOAuthStore
	// (DeleteOneChecked). Without parity, a caller that keys behaviour on
	// the outcome — e.g. auditing a delete only when something was actually
	// removed — is correct against Mongo but silently wrong under the memory
	// store used by tests and local mode.
	key := mkOAuthKey(userID, kind)
	if _, ok := s.m[key]; !ok {
		return ErrOAuthNotFound
	}
	delete(s.m, key)
	return nil
}

func (s *MemoryOAuthStore) ExpiringBefore(_ context.Context, t time.Time) ([]OAuthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []OAuthRecord
	for _, r := range s.m {
		if r.AccessTokenExpiresAt != nil && r.AccessTokenExpiresAt.Before(t) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *MemoryOAuthStore) SetAccountLabel(_ context.Context, userID string, kind OAuthKind, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := mkOAuthKey(userID, kind)
	r, ok := s.m[key]
	if !ok {
		return ErrOAuthNotFound
	}
	r.AccountLabel = label
	r.UpdatedAt = time.Now().UTC()
	s.m[key] = r
	return nil
}

func (s *MemoryOAuthStore) ClaimRefresh(_ context.Context, userID string, kind OAuthKind, owner string, now, until time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := mkOAuthKey(userID, kind)
	r, ok := s.m[key]
	if !ok {
		return false, nil
	}
	if r.RefreshNotBefore != nil && r.RefreshNotBefore.After(now) {
		return false, nil
	}
	u := until.UTC()
	r.RefreshClaimOwner = owner
	r.RefreshNotBefore = &u
	s.m[key] = r
	return true, nil
}

func (s *MemoryOAuthStore) ReleaseRefreshClaim(_ context.Context, userID string, kind OAuthKind, owner string, notBefore *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := mkOAuthKey(userID, kind)
	r, ok := s.m[key]
	// A record that vanished under the holder is the claim-lost outcome,
	// not a lookup failure — same verdict the Mongo twin's MatchedCount
	// gives, and the caller acts on it identically.
	if !ok || r.RefreshClaimOwner != owner {
		return ErrRefreshClaimLost
	}
	r.RefreshClaimOwner = ""
	r.RefreshNotBefore = copyTimePtr(notBefore)
	s.m[key] = r
	return nil
}

func copyTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := t.UTC()
	return &c
}

func (s *MemoryOAuthStore) UpdateTokens(_ context.Context, userID string, kind OAuthKind, upd OAuthTokenUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := mkOAuthKey(userID, kind)
	r, ok := s.m[key]
	if upd.ClaimOwner != "" {
		// A fenced commit reads its verdict off the fence, exactly like
		// ReleaseRefreshClaim: a record that vanished under the holder is
		// the claim-lost outcome, not a lookup failure — and the Mongo
		// twin's filtered UpdateOne cannot tell the two apart either.
		if !ok || r.RefreshClaimOwner != upd.ClaimOwner {
			return ErrRefreshClaimLost
		}
		r.RefreshClaimOwner = ""
		r.RefreshNotBefore = copyTimePtr(upd.RefreshNotBefore)
	} else if !ok {
		return ErrOAuthNotFound
	}
	if upd.SealedPayload != nil {
		r.SealedPayload = upd.SealedPayload
	}
	if upd.AccessTokenExpiresAt != nil {
		r.AccessTokenExpiresAt = upd.AccessTokenExpiresAt
	}
	if upd.LastRefreshedAt != nil {
		r.LastRefreshedAt = upd.LastRefreshedAt
	}
	if len(upd.Scopes) > 0 {
		r.Scopes = upd.Scopes
	}
	if upd.Fingerprint != "" {
		r.Fingerprint = upd.Fingerprint
	}
	r.NotRefreshable = upd.NotRefreshable
	r.UpdatedAt = time.Now().UTC()
	s.m[key] = r
	return nil
}

// MongoOAuthStore — production impl.
type MongoOAuthStore struct {
	coll *mongo.Collection
}

const OAuthCollectionName = "oauth_credentials"

func NewMongoOAuthStore(db *mongo.Database) *MongoOAuthStore {
	return &MongoOAuthStore{coll: db.Collection(OAuthCollectionName)}
}

func (s *MongoOAuthStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "kind", Value: 1}}, Options: options.Index().SetUnique(true).SetName("user_kind_unique")},
		{Keys: bson.D{{Key: "access_token_expires_at", Value: 1}}, Options: options.Index().SetName("access_expiry_partial").SetPartialFilterExpression(bson.M{"access_token_expires_at": bson.M{"$exists": true}})},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("secrets: ensure oauth indexes: %w", err)
	}
	return nil
}

func (s *MongoOAuthStore) Upsert(ctx context.Context, rec OAuthRecord) error {
	if rec.ID == "" {
		rec.ID = mkOAuthKey(rec.UserID, rec.Kind)
	}
	rec.UpdatedAt = time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = rec.UpdatedAt
	}
	// _id lives only in $setOnInsert: Mongo rejects an update that touches
	// it on a subsequent upsert.
	setBody, err := mongoutil.SetBodyWithoutID(rec, "secrets: oauth")
	if err != nil {
		return err
	}

	_, err = s.coll.UpdateOne(
		ctx,
		bson.M{"user_id": rec.UserID, "kind": rec.Kind},
		bson.M{
			"$set":         setBody,
			"$setOnInsert": bson.M{"_id": rec.ID},
		},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("secrets: upsert oauth: %w", err)
	}
	return nil
}

func (s *MongoOAuthStore) Get(ctx context.Context, userID string, kind OAuthKind) (OAuthRecord, error) {
	return mongoutil.FindOne[OAuthRecord](ctx, s.coll, bson.M{"user_id": userID, "kind": kind}, ErrOAuthNotFound, "secrets: get oauth")
}

func (s *MongoOAuthStore) ListByUser(ctx context.Context, userID string) ([]OAuthRecord, error) {
	return mongoutil.FindAllSorted[OAuthRecord](ctx, s.coll, bson.M{"user_id": userID}, "kind",
		"secrets: list oauth", "secrets: decode oauth")
}

func (s *MongoOAuthStore) Delete(ctx context.Context, userID string, kind OAuthKind) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"user_id": userID, "kind": kind}, ErrOAuthNotFound, "secrets: delete oauth")
}

func (s *MongoOAuthStore) SetAccountLabel(ctx context.Context, userID string, kind OAuthKind, label string) error {
	// A literal $set of the two keys, never the struct: the sealed payload
	// and the fingerprint stay whatever the last connect/refresh wrote.
	res, err := s.coll.UpdateOne(ctx,
		bson.M{"user_id": userID, "kind": kind},
		bson.M{"$set": bson.M{"account_label": label, "updated_at": time.Now().UTC()}},
	)
	if err != nil {
		return fmt.Errorf("secrets: set oauth account label: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrOAuthNotFound
	}
	return nil
}

// ClaimRefresh is the CAS: the filter matches only while no live claim
// stands (refresh_not_before absent, null, or already past — `nil` matches
// the first two in Mongo), so the first replica to stamp its owner wins and
// the rest get (false, nil). One refresher per record, no leader.
func (s *MongoOAuthStore) ClaimRefresh(ctx context.Context, userID string, kind OAuthKind, owner string, now, until time.Time) (bool, error) {
	res, err := s.coll.UpdateOne(ctx,
		bson.M{"user_id": userID, "kind": kind, "$or": []bson.M{
			{"refresh_not_before": nil},
			{"refresh_not_before": bson.M{"$lte": now.UTC()}},
		}},
		bson.M{"$set": bson.M{
			"refresh_claim_owner": owner,
			"refresh_not_before":  until.UTC(),
		}},
	)
	if err != nil {
		return false, fmt.Errorf("secrets: claim oauth refresh: %w", err)
	}
	return res.MatchedCount > 0, nil
}

func (s *MongoOAuthStore) ReleaseRefreshClaim(ctx context.Context, userID string, kind OAuthKind, owner string, notBefore *time.Time) error {
	// updated_at is deliberately NOT touched here, nor in ClaimRefresh:
	// taking or dropping the lock changes no credential, and the field is
	// rendered to operators as when this connection last changed. A refresh
	// that actually rotates tokens moves it through UpdateTokens.
	set := bson.M{
		"refresh_claim_owner": "",
		"refresh_not_before":  notBeforeValue(notBefore),
	}
	res, err := s.coll.UpdateOne(ctx,
		bson.M{"user_id": userID, "kind": kind, "refresh_claim_owner": owner},
		bson.M{"$set": set},
	)
	if err != nil {
		return fmt.Errorf("secrets: release oauth refresh claim: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrRefreshClaimLost
	}
	return nil
}

// notBeforeValue renders the cool-down for a $set: a nil pointer must reach
// the wire as an explicit null (clearing it), never be omitted — an omitted
// key leaves the OLD instant in place and holds the sweep off a record that
// is due.
func notBeforeValue(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// oauthTokenUpdateWrite builds the (filter, update) pair the Mongo
// UpdateTokens commits. It is a separate pure function for two reasons,
// both learned from the fence being built and then not sent — a filter is
// the one half of a Mongo write nothing observable can betray, since an
// unfenced UpdateOne matches every time and leaves a record identical to a
// legitimate commit's:
//
//   - the fence becomes assertable with no live Mongo, which is where the
//     only suite that could have caught it skips
//     (TestOAuthTokenUpdateWrite_FencesOnTheClaim);
//   - the filter arrives as a RETURNED value, so a call site that passes a
//     literal instead leaves it unused and does not compile. Built in place,
//     it was "used" by its own map-index assignments and compiled fine.
func oauthTokenUpdateWrite(userID string, kind OAuthKind, upd OAuthTokenUpdate, now time.Time) (bson.M, bson.M) {
	// A literal $set of the refresh-owned keys, never the struct: the
	// account label — and anything else a future writer owns — stays
	// whatever its own endpoint last wrote.
	set := bson.M{
		"not_refreshable": upd.NotRefreshable,
		"updated_at":      now.UTC(),
	}
	filter := bson.M{"user_id": userID, "kind": kind}
	if upd.ClaimOwner != "" {
		// Fenced commit: the tokens land only while this holder still owns
		// the claim, and the claim is released by the same write. A lost
		// claim means a re-connect (which clears the owner) or an expired
		// lease superseded us, and the exchange result belongs to a
		// session that is no longer stored.
		filter["refresh_claim_owner"] = upd.ClaimOwner
		set["refresh_claim_owner"] = ""
		set["refresh_not_before"] = notBeforeValue(upd.RefreshNotBefore)
	}
	if upd.SealedPayload != nil {
		set["sealed_payload"] = upd.SealedPayload
	}
	if upd.AccessTokenExpiresAt != nil {
		set["access_token_expires_at"] = *upd.AccessTokenExpiresAt
	}
	if upd.LastRefreshedAt != nil {
		set["last_refreshed_at"] = *upd.LastRefreshedAt
	}
	if len(upd.Scopes) > 0 {
		set["scopes"] = upd.Scopes
	}
	if upd.Fingerprint != "" {
		set["fingerprint"] = upd.Fingerprint
	}
	return filter, bson.M{"$set": set}
}

func (s *MongoOAuthStore) UpdateTokens(ctx context.Context, userID string, kind OAuthKind, upd OAuthTokenUpdate) error {
	filter, update := oauthTokenUpdateWrite(userID, kind, upd, time.Now())
	res, err := s.coll.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("secrets: update oauth tokens: %w", err)
	}
	if res.MatchedCount == 0 {
		if upd.ClaimOwner != "" {
			// The fence is what failed to match — the same verdict
			// ReleaseRefreshClaim reads off MatchedCount, and for the same
			// reason: a record whose claim moved on and one that vanished
			// under the holder both mean "these tokens belong to a session
			// that is no longer stored". Callers branch on the sentinel to
			// DISCARD the exchange rather than count a persist failure.
			return ErrRefreshClaimLost
		}
		return ErrOAuthNotFound
	}
	return nil
}

func (s *MongoOAuthStore) ExpiringBefore(ctx context.Context, t time.Time) ([]OAuthRecord, error) {
	cur, err := s.coll.Find(ctx, bson.M{
		"access_token_expires_at": bson.M{"$lt": t, "$exists": true},
	})
	if err != nil {
		return nil, fmt.Errorf("secrets: list expiring oauth: %w", err)
	}
	defer cur.Close(ctx)
	var out []OAuthRecord
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("secrets: decode expiring oauth: %w", err)
	}
	return out, nil
}
