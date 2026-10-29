package secrets

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Binding kinds say what a binding makes resolvable for its bot. The
// discriminator is absent on every binding written before kinds existed, so
// an empty Kind reads as BindingKindSecret — see EffectiveKind and ADR-147.
const (
	BindingKindSecret  = "secret"
	BindingKindContext = "context"
)

// ContextSource is the `context` kind's payload: reference material an
// OPERATOR controls, materialised into the bot's runs without republishing
// the bundle. It is pinned on purpose, the way pluginsource.PluginSource is:
// a moving ref makes the material change under the operator's feet, which is
// the silent-drift failure that record exists to prevent (ADR-147 D3).
type ContextSource struct {
	// GitURL is the clone URL holding the material (https or ssh, per the
	// credential kind).
	GitURL string `bson:"git_url" json:"git_url"`
	// Ref is what to resolve. PIN A TAG OR SHA: a branch makes every launch
	// a network round-trip and the content mutable behind the operator.
	Ref string `bson:"ref" json:"ref"`
	// Path is the file inside the source, relative to its root.
	Path string `bson:"path" json:"path"`
	// SecretID references the GenericSecret holding the READ credential (PAT
	// or deploy key) — empty for a public repository. It is not the material:
	// a context binding exposes no secret to the workflow, which is why the
	// record's own SecretID stays empty for this kind.
	SecretID string `bson:"secret_id,omitempty" json:"secret_id,omitempty"`
}

// BotSecretBinding is a policy wrapper over an existing generic secret:
// it makes a stored org/user secret resolvable for a specific bot under
// the name the bot's workflow declares in its `secrets:` block. It does
// NOT store secret material — only a reference (SecretID) plus optional
// tightening (AllowedHosts narrows the egress policy).
type BotSecretBinding struct {
	ID       string `bson:"_id" json:"id"`
	TenantID string `bson:"tenant_id" json:"tenant_id"`
	BotID    string `bson:"bot_id" json:"bot_id"`

	// Kind discriminates the payload below. Absent on every binding stored
	// before kinds existed, and absent from a client that does not send it,
	// so it reads as BindingKindSecret — read it through EffectiveKind, never
	// raw.
	Kind string `bson:"kind,omitempty" json:"kind,omitempty"`

	SecretID string `bson:"secret_id" json:"secret_id"` // -> GenericSecret._id (secret kind only)
	// SecretNameForWorkflow is the name the bot's workflow declares, and the
	// name this binding resolves under: a `secrets:` name for the secret
	// kind, an `attachments:` name for the context kind (ADR-147 D6). It is
	// the third key of the unique index, so one name on one bot carries one
	// meaning whatever the kind — see bindingUniqueKey.
	SecretNameForWorkflow string `bson:"secret_name_for_workflow" json:"secret_name_for_workflow"`

	// Context is the `context` kind's payload, nil for every other kind.
	// New kinds take a payload of their own rather than more flat fields
	// conditional on Kind: the flat fields above are what the secret kind
	// was stored as before kinds existed, not a pattern to repeat.
	Context *ContextSource `bson:"context,omitempty" json:"context,omitempty"`

	// AllowedHosts, when non-empty, intersects (never broadens) the
	// workflow secret's declared egress hosts. This is an ENFORCED egress
	// control: the resolver carries it on GenericResolution.AllowedHosts,
	// the publisher threads it onto RunBundle.GenericSecretHosts, the
	// runner puts it on Credentials.GenericHosts, and the secret guard
	// intersects it with the workflow's `secrets.<name>.hosts`
	// (model.effectiveSecretHosts). Empty = no binding-level restriction.
	AllowedHosts []string `bson:"allowed_hosts,omitempty" json:"allowed_hosts,omitempty"`

	CreatedBy string    `bson:"created_by" json:"created_by"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// BotSecretBindingStore persists bot-secret bindings.
type BotSecretBindingStore interface {
	Create(ctx context.Context, b BotSecretBinding) error
	Get(ctx context.Context, id string) (BotSecretBinding, error)
	Update(ctx context.Context, b BotSecretBinding) error
	Delete(ctx context.Context, id string) error
	ListByTenantBot(ctx context.Context, tenantID, botID string) ([]BotSecretBinding, error)
	ListByTenant(ctx context.Context, tenantID string) ([]BotSecretBinding, error)
}

var (
	ErrBindingNotFound      = errors.New("secrets: bot secret binding not found")
	ErrBindingTenantMissing = errors.New("secrets: bot secret binding store called without tenant context")
	// ErrBindingNameConflict is what both stores return when a second
	// binding claims a name already bound on that bot. The Mongo store has
	// enforced it by unique index since the collection existed; the memory
	// store used to accept the duplicate, which made every in-memory test of
	// a conflict pass while production answered a driver error.
	ErrBindingNameConflict = errors.New("secrets: a binding for this workflow name already exists on this bot")
	// ErrBindingKindUnknown names a kind the server does not implement.
	ErrBindingKindUnknown = errors.New("secrets: unknown bot binding kind")
)

// EffectiveKind is the binding's kind, reading an absent discriminator as the
// only kind that existed before kinds did.
func (b BotSecretBinding) EffectiveKind() string {
	if b.Kind == "" {
		return BindingKindSecret
	}
	return b.Kind
}

// DeclaredName is the workflow-declared name the binding resolves under,
// whatever its kind.
func (b BotSecretBinding) DeclaredName() string { return b.SecretNameForWorkflow }

// bindingUniqueKey is the triple the Mongo unique index `tenant_bot_name_unique`
// enforces (tenant_id, bot_id, secret_name_for_workflow). It is written once,
// here, because the memory store has to refuse exactly what the index refuses
// — two keys drifting apart is how a fixture becomes gentler than production.
func bindingUniqueKey(b BotSecretBinding) [3]string {
	return [3]string{b.TenantID, b.BotID, b.DeclaredName()}
}

// Validate refuses a binding whose payload contradicts its kind. It is the
// one place that knows what each kind requires, so a route, a store or a
// future importer cannot each decide differently.
func (b BotSecretBinding) Validate() error {
	if strings.TrimSpace(b.DeclaredName()) == "" {
		return fmt.Errorf("secrets: bot binding needs the workflow name it resolves under")
	}
	switch kind := b.EffectiveKind(); kind {
	case BindingKindSecret:
		if strings.TrimSpace(b.SecretID) == "" {
			return fmt.Errorf("secrets: a %s binding needs the secret it exposes", BindingKindSecret)
		}
		if b.Context != nil {
			return fmt.Errorf("secrets: a %s binding carries no context payload", BindingKindSecret)
		}
	case BindingKindContext:
		if b.Context == nil {
			return fmt.Errorf("secrets: a %s binding needs its pinned source", BindingKindContext)
		}
		if strings.TrimSpace(b.SecretID) != "" {
			return fmt.Errorf("secrets: a %s binding exposes no secret to the workflow — put the read credential in context.secret_id", BindingKindContext)
		}
		if strings.TrimSpace(b.Context.GitURL) == "" || strings.TrimSpace(b.Context.Ref) == "" || strings.TrimSpace(b.Context.Path) == "" {
			return fmt.Errorf("secrets: a %s binding needs context.git_url, context.ref and context.path", BindingKindContext)
		}
	default:
		return fmt.Errorf("%w %q (known: %s, %s)", ErrBindingKindUnknown, kind, BindingKindSecret, BindingKindContext)
	}
	return nil
}

// IntersectHosts returns the stricter of two egress host policies. An
// empty list means "no restriction"; the result never broadens either
// input:
//   - both empty            -> empty (unrestricted)
//   - one empty, one set    -> the set (the restriction wins)
//   - both set              -> their intersection
func IntersectHosts(a, b []string) []string {
	if len(a) == 0 {
		return append([]string(nil), b...)
	}
	if len(b) == 0 {
		return append([]string(nil), a...)
	}
	inB := make(map[string]bool, len(b))
	for _, h := range b {
		inB[h] = true
	}
	var out []string
	seen := make(map[string]bool, len(a))
	for _, h := range a {
		if inB[h] && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// Bot bindings may only dereference team-scoped generic secrets from the
// binding's own team. Personal (/api/me/secrets) credentials are deliberately
// excluded: a binding is shared org automation policy and synthetic/webhook
// runs do not carry the original user's consent/ownership context.
func bindableGenericSecretForBotBinding(sec GenericSecret, teamID string) bool {
	return sec.ScopeTeamID == teamID && sec.ScopeUserID == ""
}

// ResolveGenericWithBindings resolves each requested workflow-secret
// name with the priority user-scoped > bot-binding > team-scoped:
//   - a developer's personal secret of that name still wins (interactive
//     opt-in);
//   - else a bot binding maps the name to a stored team-scoped secret —
//     the canonical route for unattended (webhook) runs whose synthetic
//     actor owns no user secrets;
//   - else a team-scoped secret of that name (the existing fallback).
//
// Binding-sourced resolutions carry the binding's AllowedHosts so the
// caller can intersect the egress policy.
func ResolveGenericWithBindings(
	ctx context.Context,
	secretStore GenericSecretStore,
	bindingStore BotSecretBindingStore,
	teamID, userID, botID string,
	names []string,
	secretOverrides map[string]string,
	sealer Sealer,
	logger *iterlog.Logger,
) (map[string]GenericResolution, error) {
	if secretStore == nil {
		return map[string]GenericResolution{}, nil
	}
	if teamID == "" {
		return nil, fmt.Errorf("secrets: team id required for generic secret resolve")
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	if len(want) == 0 {
		return map[string]GenericResolution{}, nil
	}

	// Tier 1+3 source: everything visible to (team, user).
	visible, err := secretStore.ListByTeam(ctx, teamID, userID)
	if err != nil {
		return nil, err
	}
	userByName := make(map[string]GenericSecret)
	teamByName := make(map[string]GenericSecret)
	for _, s := range visible {
		if !want[s.Name] {
			continue
		}
		if userID != "" && s.ScopeUserID == userID {
			if _, ok := userByName[s.Name]; !ok {
				userByName[s.Name] = s
			}
		} else if s.ScopeUserID == "" {
			if _, ok := teamByName[s.Name]; !ok {
				teamByName[s.Name] = s
			}
		}
	}

	// Tier 2 source: bot bindings (name -> secret + AllowedHosts).
	type boundSecret struct {
		sec   GenericSecret
		hosts []string
	}
	bindingByName := make(map[string]boundSecret)
	if bindingStore != nil && botID != "" {
		bindings, err := bindingStore.ListByTenantBot(ctx, teamID, botID)
		if err != nil {
			return nil, err
		}
		for _, b := range bindings {
			if !want[b.SecretNameForWorkflow] {
				continue
			}
			if _, ok := bindingByName[b.SecretNameForWorkflow]; ok {
				continue
			}
			sec, err := secretStore.Get(ctx, b.SecretID)
			if err != nil {
				// dangling binding (secret deleted) — skip, don't fail the run.
				// Warn: a configured binding pointing at a missing secret is a
				// misconfiguration the operator should see (name + reason, never
				// the value).
				logger.Warn("secrets: bot binding for %q (bot=%s, secret_id=%s) dangles — credential dropped: %v", b.SecretNameForWorkflow, botID, b.SecretID, err)
				continue
			}
			if !bindableGenericSecretForBotBinding(sec, teamID) {
				// Defensive authorization check: older/compromised bindings may
				// point at a personal secret or a secret moved out of this team.
				// Treat those like dangling bindings rather than injecting them
				// into synthetic bot/webhook runs.
				logger.Warn("secrets: bot binding for %q (bot=%s, secret_id=%s) points at a non-bindable (cross-tenant or personal) secret — credential dropped", b.SecretNameForWorkflow, botID, b.SecretID)
				continue
			}
			bindingByName[b.SecretNameForWorkflow] = boundSecret{sec: sec, hosts: b.AllowedHosts}
		}
	}

	// Tier 0 source: per-webhook secret overrides (name -> secret id), the
	// highest precedence. Lets a webhook pin a specific stored secret — e.g. a
	// distinct forge_token / bot identity — over the org binding, mirroring the
	// BYOK keyOverrides path. See docs/byok.md. The override carries no
	// binding-level AllowedHosts of its own, but the resolved secret's OWN
	// egress lock (GenericSecret.AllowedHosts, seeded in buildGenericResolution)
	// still applies — so a managed forge token pinned to its forge host stays
	// egress-locked even when bound via a Tier-0 override.
	overrideByName := make(map[string]GenericSecret)
	for name, secretID := range secretOverrides {
		if !want[name] || strings.TrimSpace(secretID) == "" {
			continue
		}
		sec, err := secretStore.Get(ctx, secretID)
		if err != nil {
			// dangling override -> fall through to the lower tiers. Warn: an
			// explicitly-pinned secret that can't be read is a misconfiguration.
			logger.Warn("secrets: webhook override for %q (secret_id=%s) dangles — falling through to lower tiers: %v", name, secretID, err)
			continue
		}
		if !bindableGenericSecretForBotBinding(sec, teamID) {
			// cross-tenant / personal secret -> ignore (fall through).
			logger.Warn("secrets: webhook override for %q (secret_id=%s) points at a non-bindable (cross-tenant or personal) secret — ignored", name, secretID)
			continue
		}
		overrideByName[name] = sec
	}

	out := make(map[string]GenericResolution, len(want))
	for name := range want {
		if s, ok := overrideByName[name]; ok {
			if r, ok := buildGenericResolution(s, sealer, userID, logger); ok {
				r.SourceScope = "webhook-override"
				out[name] = r
				continue
			}
		}
		if s, ok := userByName[name]; ok {
			if r, ok := buildGenericResolution(s, sealer, userID, logger); ok {
				r.SourceScope = "user"
				out[name] = r
				continue
			}
		}
		if b, ok := bindingByName[name]; ok {
			if r, ok := buildGenericResolution(b.sec, sealer, userID, logger); ok {
				r.SourceScope = "binding"
				// Intersect the binding's egress hosts with the secret's own
				// lock (buildGenericResolution seeded r.AllowedHosts from the
				// secret) so a binding narrows, never broadens.
				r.AllowedHosts = IntersectHosts(r.AllowedHosts, b.hosts)
				out[name] = r
				continue
			}
		}
		if s, ok := teamByName[name]; ok {
			if r, ok := buildGenericResolution(s, sealer, userID, logger); ok {
				r.SourceScope = "team"
				out[name] = r
				continue
			}
		}
		// Expected fall-through: no tier produced a value for this name and no
		// candidate dropped on unseal (those are already warned above). Debug —
		// a run may legitimately declare an optional secret the team never
		// configured / bound.
		if _, hadCandidate := overrideByName[name]; hadCandidate {
			continue
		}
		if _, hadCandidate := userByName[name]; hadCandidate {
			continue
		}
		if _, hadCandidate := bindingByName[name]; hadCandidate {
			continue
		}
		if _, hadCandidate := teamByName[name]; hadCandidate {
			continue
		}
		logger.Debug("secrets: no generic secret named %q resolvable for team=%s bot=%s (no user/binding/team/override match) — leaving credential unset", name, teamID, botID)
	}
	return out, nil
}

// ---- in-memory store (tests / local) ----

type MemoryBotSecretBindingStore struct {
	mu       sync.RWMutex
	bindings map[string]BotSecretBinding
}

func NewMemoryBotSecretBindingStore() *MemoryBotSecretBindingStore {
	return &MemoryBotSecretBindingStore{bindings: make(map[string]BotSecretBinding)}
}

// The memory store mirrors the Mongo one's tenant semantics (stamp on
// write, filter on read) — see stampTenant / visibleToTenant in generic.go
// for why a permissive double hides a whole class of bugs (#997).
func (m *MemoryBotSecretBindingStore) Create(ctx context.Context, b BotSecretBinding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b.TenantID = stampTenant(ctx, b.TenantID)
	if err := m.refuseNameConflict(b); err != nil {
		return err
	}
	m.bindings[b.ID] = b
	return nil
}

// refuseNameConflict is the memory twin of the `tenant_bot_name_unique`
// index: the same triple, refused for the same reason, so a conflict a test
// exercises here is the conflict production answers.
func (m *MemoryBotSecretBindingStore) refuseNameConflict(candidate BotSecretBinding) error {
	key := bindingUniqueKey(candidate)
	for id, existing := range m.bindings {
		if id != candidate.ID && bindingUniqueKey(existing) == key {
			return ErrBindingNameConflict
		}
	}
	return nil
}

func (m *MemoryBotSecretBindingStore) Get(ctx context.Context, id string) (BotSecretBinding, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.bindings[id]
	if !ok || !visibleToTenant(ctx, b.TenantID) {
		return BotSecretBinding{}, ErrBindingNotFound
	}
	return b, nil
}

func (m *MemoryBotSecretBindingStore) Update(ctx context.Context, b BotSecretBinding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.bindings[b.ID]
	if !ok || !visibleToTenant(ctx, cur.TenantID) {
		return ErrBindingNotFound
	}
	b.TenantID = stampTenant(ctx, cur.TenantID)
	if err := m.refuseNameConflict(b); err != nil {
		return err
	}
	m.bindings[b.ID] = b
	return nil
}

func (m *MemoryBotSecretBindingStore) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bindings[id]
	if !ok || !visibleToTenant(ctx, b.TenantID) {
		return ErrBindingNotFound
	}
	delete(m.bindings, id)
	return nil
}

func (m *MemoryBotSecretBindingStore) ListByTenantBot(_ context.Context, tenantID, botID string) ([]BotSecretBinding, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []BotSecretBinding
	for _, b := range m.bindings {
		if b.TenantID == tenantID && b.BotID == botID {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryBotSecretBindingStore) ListByTenant(_ context.Context, tenantID string) ([]BotSecretBinding, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []BotSecretBinding
	for _, b := range m.bindings {
		if b.TenantID == tenantID {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ---- Mongo store ----

const BotSecretBindingsCollectionName = "bot_secret_bindings"

type MongoBotSecretBindingStore struct {
	coll *mongo.Collection
}

func NewMongoBotSecretBindingStore(db *mongo.Database) *MongoBotSecretBindingStore {
	return &MongoBotSecretBindingStore{coll: db.Collection(BotSecretBindingsCollectionName)}
}

func (s *MongoBotSecretBindingStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// Keys in lock-step with bindingUniqueKey, which is what the memory
		// twin refuses on: one workflow name per bot, whatever the kind.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "bot_id", Value: 1}, {Key: "secret_name_for_workflow", Value: 1}}, Options: options.Index().SetUnique(true).SetName("tenant_bot_name_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "secret_id", Value: 1}}, Options: options.Index().SetName("tenant_secret")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("secrets: ensure bot_secret_bindings indexes: %w", err)
	}
	return nil
}

func withBindingTenantFilter(ctx context.Context, base bson.M) (bson.M, error) {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return nil, ErrBindingTenantMissing
	}
	out := make(bson.M, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["tenant_id"] = tenantID
	return out, nil
}

func (s *MongoBotSecretBindingStore) Create(ctx context.Context, b BotSecretBinding) error {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return ErrBindingTenantMissing
	}
	b.TenantID = tenantID
	if _, err := s.coll.InsertOne(ctx, b); err != nil {
		// The index is the enforcement; without this translation the caller
		// reads a driver error and answers 500 to what is a 409.
		if mongoutil.IsDuplicateKey(err) {
			return ErrBindingNameConflict
		}
		return fmt.Errorf("secrets: insert bot binding: %w", err)
	}
	return nil
}

func (s *MongoBotSecretBindingStore) Get(ctx context.Context, id string) (BotSecretBinding, error) {
	filter, err := withBindingTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return BotSecretBinding{}, err
	}
	return mongoutil.FindOne[BotSecretBinding](ctx, s.coll, filter, ErrBindingNotFound, "secrets: get bot binding")
}

func (s *MongoBotSecretBindingStore) Update(ctx context.Context, b BotSecretBinding) error {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return ErrBindingTenantMissing
	}
	b.TenantID = tenantID
	return mongoutil.ReplaceOneChecked(ctx, s.coll, bson.M{"_id": b.ID, "tenant_id": tenantID}, b, ErrBindingNameConflict, ErrBindingNotFound, "secrets: update bot binding")
}

func (s *MongoBotSecretBindingStore) Delete(ctx context.Context, id string) error {
	filter, err := withBindingTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	return mongoutil.DeleteOneChecked(ctx, s.coll, filter, ErrBindingNotFound, "secrets: delete bot binding")
}

func (s *MongoBotSecretBindingStore) ListByTenantBot(ctx context.Context, tenantID, botID string) ([]BotSecretBinding, error) {
	filter, err := withBindingTenantFilter(ctx, bson.M{"bot_id": botID})
	if err != nil {
		return nil, err
	}
	return s.list(ctx, filter)
}

func (s *MongoBotSecretBindingStore) ListByTenant(ctx context.Context, tenantID string) ([]BotSecretBinding, error) {
	filter, err := withBindingTenantFilter(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	return s.list(ctx, filter)
}

func (s *MongoBotSecretBindingStore) list(ctx context.Context, filter bson.M) ([]BotSecretBinding, error) {
	return mongoutil.FindAllSorted[BotSecretBinding](ctx, s.coll, filter, "created_at",
		"secrets: list bot bindings", "secrets: decode bot bindings")
}
