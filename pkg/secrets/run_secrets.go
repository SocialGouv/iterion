package secrets

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
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

// RunBundle is the per-run sealed payload the runner needs in order
// to execute. It carries every API-key + OAuth credential the
// publisher pre-resolved, keyed by provider/kind.
//
// The structure is JSON-marshalled and then sealed once with the
// run-scoped AAD ("run_secrets:<run_id>"). Runners decrypt with
// the shared master key.
type RunBundle struct {
	APIKeys map[Provider]string `json:"api_keys,omitempty"`
	// PinnedAPIKeys carries keys a shared tier (org, platform) filled ONLY
	// because a route of the run NAMES that provider, on a wire family
	// another slot already fills. They are deliberately NOT in APIKeys:
	// every default-precedence reader walks that map, and the anthropic
	// wire ranks the facade slots FIRST — a moonshot key added there for
	// one pinned node would silently reroute every UNPINNED node of the
	// run onto Kimi, and past a tenant's own forfait, which is the
	// cross-vendor substitution the one-key-per-family rule exists to
	// prevent. Kept in their own map, they are invisible to those readers
	// by construction rather than by a guard each would have to repeat.
	//
	// The rule for reading one: a pinned key serves ONLY a route that
	// names its provider — the claude_code delegate under an explicit
	// `provider:` hint, a claw node whose model spec carries the
	// `<provider>/` prefix. Metering still stamps its fingerprint, so its
	// own usage windows are attributed to it like any other credential.
	PinnedAPIKeys map[Provider]string `json:"pinned_api_keys,omitempty"`
	// GenericSecrets maps workflow secret names to plaintext payloads
	// resolved from the tenant/user secret store at publish time.
	GenericSecrets map[string]string `json:"generic_secrets,omitempty"`
	// GenericSecretHosts maps a workflow secret name to the egress host
	// allowlist a bot-secret binding imposes on it (empty/absent = no
	// binding-level restriction). The runner intersects this with the
	// workflow's own declared `secrets.<name>.hosts` so a binding can
	// only NARROW egress, never broaden it. This is what makes a
	// binding's AllowedHosts an enforced control rather than metadata.
	GenericSecretHosts map[string][]string `json:"generic_secret_hosts,omitempty"`
	// GenericSecretRefs maps a workflow secret name to the ID of the
	// generic-secret store record it was resolved from (IDs only, never
	// values). A short-lived credential (a GitHub App installation token
	// lives 1h) can expire while the run executes; the server-side
	// refresh worker keeps the STORE record fresh, so these refs let the
	// runner re-read the current value mid-run and rewrite the secret's
	// materialised file — the bundle snapshot alone would go stale.
	GenericSecretRefs map[string]string `json:"generic_secret_refs,omitempty"`
	// OAuthCredentials maps "claude_code" / "codex" → opaque blob
	// that the runner materialises as a credentials.json /
	// auth.json before spawning the CLI subprocess.
	OAuthCredentials map[string][]byte `json:"oauth_credentials,omitempty"`
	// OAuthFingerprints maps the same kinds to the SUBSCRIPTION's audit
	// fingerprint (OAuthRecord.Fingerprint) — stable across automatic
	// token refreshes, re-stamped when a human posts new credentials.
	// Metering keys on it; empty for records that predate stamping.
	OAuthFingerprints map[string]string `json:"oauth_fingerprints,omitempty"`
	// OAuthRecordRefs maps the same kinds to the ID of the OAuthStore record
	// the payload was read from. The server's refresh worker is the ONE
	// refresher of a record: a runner follows the record by this id to pick
	// up each rotation, instead of exchanging the refresh token itself and
	// revoking the token every other holder still uses. A lent subscription
	// names the DONOR's record. Absent from bundles sealed by an older
	// server.
	OAuthRecordRefs map[string]string `json:"oauth_record_refs,omitempty"`
	// OAuthRecordConnectedAt maps the same kinds to the connect time
	// (OAuthRecord.CreatedAt) of the record a ref names. Only a connect
	// rewrites it; the refresh worker's rotations never do, even those that
	// re-stamp the fingerprint. A lent slot is held to it: the borrower
	// stops following the donor's record only when the donor re-connected
	// the slot with another subscription. Absent from bundles sealed by an
	// older server.
	OAuthRecordConnectedAt map[string]time.Time `json:"oauth_record_connected_at,omitempty"`
	// ForgeAppBotLogin is the GitHub-App bot login (e.g.
	// "iterion-forge-1234[bot]") when the run's forge_token was resolved
	// from a github_app connection. An installation token can't `GET /user`
	// (403), so the runner can't self-resolve the committer identity from
	// the token alone — this login lets it look up the bot's numeric id via
	// `GET /users/<login>` (which an installation token CAN read) and seed
	// the canonical `<id>+<login>@users.noreply.github.com` committer, so a
	// bot's commits are attributed to the App bot, not the neutral fallback.
	// Empty for PAT/OAuth connections (the token's own /user resolves them).
	ForgeAppBotLogin string `json:"forge_app_bot_login,omitempty"`
	// PlatformSourced marks the credential slots the PLATFORM tier filled —
	// provider names for APIKeys entries ("anthropic", …) and OAuth kinds
	// for OAuthCredentials entries ("claude_code", "codex"); the two
	// namespaces never overlap. The runner's usage-cap scope check needs
	// it: a platform credential riding the bundle must still be metered on
	// the shared platform key, not fragmented per tenant as if the tenant
	// had brought its own.
	PlatformSourced map[string]bool `json:"platform_sourced,omitempty"`
	// PoolSourced marks the credential slots the mutualised credential POOL
	// filled with a contributor's lent credential (same slot namespaces as
	// PlatformSourced). The runner's metering bump needs it: a lent key's
	// row lives in the donor's tenant, so its last_used_at must be bumped
	// without the run's tenant filter — while a tenant's own key is bumped
	// only under its tenant, so another tenant holding the byte-identical
	// secret never sees its own key read as "in use".
	PoolSourced map[string]bool `json:"pool_sourced,omitempty"`
	// OrgSourced marks the credential slots the ORG tier filled — the
	// org's own shared key, lent to the teams its CredentialAudience
	// admits (same slot namespaces as PlatformSourced). Like the two
	// above it rides the bundle as an ordinary credential and is not the
	// team's: one subscription serves several teams, so metering it per
	// team would open one ledger per team of the SAME account and no
	// reading would ever accumulate. Its row also lives under a reserved
	// scope, not the run's tenant, so a last_used_at bump must escape the
	// run's tenant filter exactly as a lent key's does.
	OrgSourced map[string]bool `json:"org_sourced,omitempty"`
}

// RunSecretsRecord is the persisted form of a sealed bundle. _id is
// the SecretsRef the publisher writes into the queue.RunMessage; the
// runner uses that ref to fetch + decrypt right before executing the
// run.
type RunSecretsRecord struct {
	ID       string `bson:"_id" json:"id"`
	TenantID string `bson:"tenant_id" json:"tenant_id"`
	RunID    string `bson:"run_id" json:"run_id"`
	// KeyID names the ring key that sealed SealedBundle. Empty means
	// the record predates key ids: it sealed under the deployment's
	// single shared key with the legacy run-only AAD, and the opener
	// falls back to exactly that.
	KeyID        string    `bson:"key_id,omitempty" json:"key_id,omitempty"`
	SealedBundle []byte    `bson:"sealed_bundle" json:"-"`
	CreatedAt    time.Time `bson:"created_at" json:"created_at"`
	// ExpiresAt drives the Mongo TTL — the runner deletes the
	// record on success, but a TTL guard ensures abandoned bundles
	// never linger past 24h.
	ExpiresAt time.Time `bson:"expires_at" json:"expires_at"`
}

// RunSecretsStore persists sealed RunBundle records keyed by an
// opaque ref carried in the NATS message.
type RunSecretsStore interface {
	Put(ctx context.Context, rec RunSecretsRecord) error
	Get(ctx context.Context, id string) (RunSecretsRecord, error)
	Delete(ctx context.Context, id string) error
}

// ErrRunSecretsNotFound is returned by Get when the ref is unknown
// (already consumed or never published).
var ErrRunSecretsNotFound = errors.New("secrets: run secrets not found")

// ErrBundleDEKMissing is the typed refusal for a "dek"-stamped record
// whose message carries no per-run key: corrupt publish or a message
// stripped in transit — never executed.
var ErrBundleDEKMissing = errors.New("dek-stamped bundle but the message carries no BundleDEK")

// DEKKeyID is the key-id scheme of ADR-123: the bundle sealed under
// the message's per-run DEK — the sealer is built at claim time from
// RunMessage.BundleDEK, and no platform key material ever reaches the
// pod. The two older schemes remain readable: an empty id (pre-ring,
// run-only AAD) and a ring id (P4a, extended AAD).
const DEKKeyID = "dek"

// NewRunBundleDEK generates the per-run key ADR-123 seals a bundle
// under.
func NewRunBundleDEK() ([]byte, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("secrets: dek: %w", err)
	}
	return dek, nil
}

// SealRunBundle marshals + seals a RunBundle for a run, bound to its
// full identity — tenant, pool and run. The AAD makes a bundle from
// one identity undecryptable under another's context, even if a
// confused record store served the ref: the binding is cryptographic,
// not a field comparison. The sealer is the run's per-run DEK sealer
// (ADR-123); the returned key id is the constant DEK scheme. The pool
// grammar and identity tenant ids carry no "/", so the three
// components are unambiguous.
func SealRunBundle(sealer Sealer, tenantID, pool, runID string, b RunBundle) ([]byte, string, error) {
	if sealer == nil {
		return nil, "", errors.New("secrets: nil sealer for SealRunBundle")
	}
	// The AAD's unambiguity rests on neither component carrying "/":
	// enforce it here, at the chokepoint both seal callers traverse, not
	// only at the launch site that validates its inputs.
	if strings.Contains(tenantID, "/") {
		return nil, "", fmt.Errorf("secrets: tenant id %q carries the AAD separator", tenantID)
	}
	if pool != "" && !validPoolName(pool) {
		return nil, "", fmt.Errorf("secrets: pool %q is not a valid runner pool name", pool)
	}
	body, err := json.Marshal(b)
	if err != nil {
		return nil, "", fmt.Errorf("secrets: marshal bundle: %w", err)
	}
	sealed, err := sealer.Seal(body, RunBundleAAD(tenantID, pool, runID))
	if err != nil {
		return nil, "", err
	}
	return sealed, DEKKeyID, nil
}

// OpenRunBundle is the inverse: decrypt + unmarshal, under the same
// identity binding the sealer used. A record with a key id opens under
// THAT key; a record without one predates the ring and opens under
// whichever ring key authenticates, with the legacy run-only AAD.
func OpenRunBundle(sealer Sealer, tenantID, pool, runID, keyID string, sealed []byte) (RunBundle, error) {
	var b RunBundle
	if sealer == nil {
		return b, errors.New("secrets: nil sealer for OpenRunBundle")
	}
	var (
		pt  []byte
		err error
	)
	switch {
	case keyID == DEKKeyID:
		pt, err = sealer.Open(sealed, RunBundleAAD(tenantID, pool, runID))
	case keyID == "":
		pt, err = openLegacyBundle(sealer, runID, sealed)
	default:
		keyed, ok := sealer.(KeyedSealer)
		if !ok {
			return b, errors.New("secrets: opening a keyed run bundle needs a keyed sealer (a key ring)")
		}
		pt, err = keyed.OpenWith(keyID, sealed, RunBundleAAD(tenantID, pool, runID))
	}
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(pt, &b); err != nil {
		return b, fmt.Errorf("secrets: unmarshal bundle: %w", err)
	}
	return b, nil
}

// openLegacyBundle opens a pre-ring record: run-only AAD, under
// whichever ring key authenticates (the record's sealing key left the
// designated-current slot at most one rotation ago; the 24h TTL
// retires the cohort).
func openLegacyBundle(sealer Sealer, runID string, sealed []byte) ([]byte, error) {
	if keyed, ok := sealer.(KeyedSealer); ok {
		if ring, ok := keyed.(*KeyRingSealer); ok {
			return ring.OpenAny(sealed, legacyRunBundleAAD(runID))
		}
		return keyed.Open(sealed, legacyRunBundleAAD(runID))
	}
	return sealer.Open(sealed, legacyRunBundleAAD(runID))
}

// RunBundleAAD binds a sealed bundle to its run identity: tenant, pool
// and run. The pool segment is empty for shared-fleet runs — the
// grammar still disambiguates, since neither pool names (1–31 chars
// [a-z0-9-]) nor tenant ids contain "/".
func RunBundleAAD(tenantID, pool, runID string) []byte {
	return []byte("run_secrets:" + tenantID + "/" + pool + "/" + runID)
}

func legacyRunBundleAAD(runID string) []byte {
	return []byte("run_secrets:" + runID)
}

// validPoolName mirrors queue.ValidPoolName (1–31 chars [a-z0-9-],
// starting alphanumeric). A local copy keeps the NATS client out of
// every secrets importer; run_secrets_test.go pins the two to agree.
func validPoolName(s string) bool {
	if len(s) == 0 || len(s) > 31 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// NewSecretsRef returns a fresh opaque ref for a RunSecretsRecord.
// Random UUID rather than the run id so an attacker who can guess
// run ids cannot enumerate sealed bundles.
func NewSecretsRef() string {
	return uuid.NewString()
}

// MongoRunSecretsStore implements RunSecretsStore on Mongo with a
// 24h TTL guard.
type MongoRunSecretsStore struct {
	coll *mongo.Collection
}

const RunSecretsCollectionName = "run_secrets"

// DefaultRunSecretsTTL bounds how long a sealed bundle can live
// untouched. Resume paths re-publish so the runner can always re-
// fetch even after a TTL eviction (the publisher will re-resolve).
const DefaultRunSecretsTTL = 24 * time.Hour

func NewMongoRunSecretsStore(db *mongo.Database) *MongoRunSecretsStore {
	return &MongoRunSecretsStore{coll: db.Collection(RunSecretsCollectionName)}
}

func (s *MongoRunSecretsStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "run_id", Value: 1}}, Options: options.Index().SetName("tenant_run")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("run_secrets_ttl").SetExpireAfterSeconds(0)},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("secrets: ensure run_secrets indexes: %w", err)
	}
	return nil
}

func (s *MongoRunSecretsStore) Put(ctx context.Context, rec RunSecretsRecord) error {
	_, err := s.coll.InsertOne(ctx, rec)
	if err != nil {
		return fmt.Errorf("secrets: put run secrets: %w", err)
	}
	return nil
}

func (s *MongoRunSecretsStore) Get(ctx context.Context, id string) (RunSecretsRecord, error) {
	return mongoutil.FindOne[RunSecretsRecord](ctx, s.coll, withRunSecretsTenantFilter(ctx, bson.M{"_id": id}), ErrRunSecretsNotFound, "secrets: get run secrets")
}

func (s *MongoRunSecretsStore) Delete(ctx context.Context, id string) error {
	_, err := s.coll.DeleteOne(ctx, withRunSecretsTenantFilter(ctx, bson.M{"_id": id}))
	if err != nil {
		return fmt.Errorf("secrets: delete run secrets: %w", err)
	}
	return nil
}

// withRunSecretsTenantFilter mirrors the mongo package's withTenantFilter:
// when ctx carries a tenant, scope by it; otherwise pass through for
// privileged callers (cluster admin, bootstrap, migration tooling).
func withRunSecretsTenantFilter(ctx context.Context, base bson.M) bson.M {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok {
		return base
	}
	out := make(bson.M, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["tenant_id"] = tenantID
	return out
}

// MemoryRunSecretsStore is the test variant.
type MemoryRunSecretsStore struct {
	mu sync.Mutex
	m  map[string]RunSecretsRecord
}

func NewMemoryRunSecretsStore() *MemoryRunSecretsStore {
	return &MemoryRunSecretsStore{m: make(map[string]RunSecretsRecord)}
}

func (s *MemoryRunSecretsStore) Put(_ context.Context, rec RunSecretsRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[rec.ID] = rec
	return nil
}

func (s *MemoryRunSecretsStore) Get(ctx context.Context, id string) (RunSecretsRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.m[id]
	if !ok {
		return RunSecretsRecord{}, ErrRunSecretsNotFound
	}
	if tenantID, has := store.TenantFromContext(ctx); has && rec.TenantID != "" && rec.TenantID != tenantID {
		return RunSecretsRecord{}, ErrRunSecretsNotFound
	}
	return rec, nil
}

func (s *MemoryRunSecretsStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.m[id]
	if !ok {
		return nil
	}
	if tenantID, has := store.TenantFromContext(ctx); has && rec.TenantID != "" && rec.TenantID != tenantID {
		// Don't reveal cross-tenant ID existence — treat as not-found.
		return nil
	}
	delete(s.m, id)
	return nil
}
