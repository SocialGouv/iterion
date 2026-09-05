package secrets

import (
	"context"
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
	AccountLabel string    `bson:"account_label" json:"account_label,omitempty"`
	CreatedAt    time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt    time.Time `bson:"updated_at" json:"updated_at"`
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
