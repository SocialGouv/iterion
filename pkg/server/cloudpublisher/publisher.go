// Package cloudpublisher wires runview.LaunchPublisher on top of
// NATS + Mongo so the cloud-mode `iterion server` can hand work off
// to the runner pool instead of executing in-process.
//
// The package lives under pkg/server/ rather than pkg/runview/ to
// keep the runview package free of NATS / Mongo imports — runview
// remains the local-mode entry point even when a cloud build is in
// the binary, and a build-time cycle would prevent that.
//
// Plan §F (T-31, T-32, T-33).
package cloudpublisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"os"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/reviewtopology"
	"github.com/SocialGouv/iterion/pkg/usagecap"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pluginsource"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Config bundles the dependencies of the publisher.
type Config struct {
	NATS  *natsq.Conn
	Store store.RunStore
	// MongoColl is the Mongo collection the publisher counts
	// queued runs against (for queue_position computation). The
	// caller passes it directly so the publisher doesn't have to
	// re-resolve it from the store interface.
	MongoColl *mongo.Collection
	Logger    *iterlog.Logger
	// Metrics, when non-nil, increments iterion_runs_created_total
	// after every successful Launch / Resume publish.
	Metrics *metrics.Registry

	// ApiKeys is the BYOK store. When non-nil, the publisher
	// resolves per-tenant credentials at launch time and seals
	// them into a per-run RunSecrets record. The runner unseals
	// and injects them into the engine ctx.
	ApiKeys secrets.ApiKeyStore
	// GenericSecrets stores workflow/user secrets addressable by name
	// from the DSL `secrets:` block.
	GenericSecrets secrets.GenericSecretStore
	// BotBindings, when non-nil, is consulted during generic-secret
	// resolution so an org-bound secret resolves for the launching bot
	// (user > binding > team priority).
	BotBindings secrets.BotSecretBindingStore
	// RunSecrets persists the sealed bundle keyed by SecretsRef.
	RunSecrets secrets.RunSecretsStore
	// Sealer is the AES-GCM master-key sealer (shared with the
	// REST handlers).
	Sealer secrets.Sealer
	// OAuthForfait is the per-user OAuth credential store. When
	// non-nil and a run's owner has connected an OAuth subscription,
	// the publisher embeds the verbatim credentials.json / auth.json
	// into the run bundle so the runner can materialise it for the
	// CLI subprocess.
	OAuthForfait secrets.OAuthStore
	// ForgeConnections, when non-nil, lets the publisher resolve the
	// github_app connection a run's forge_token came from and thread its
	// bot login to the runner, so an installation token's commits are
	// attributed to the App bot (the runner can't self-resolve it — see
	// RunBundle.ForgeAppBotLogin).
	ForgeConnections forge.ConnectionStore
	// PluginSources, when non-nil, resolves the launching team's git-hosted
	// org-private plugins at launch (pkg/pluginsource). This is the DURABLE
	// counterpart to a plugin installed into the pod's iterion home, which a
	// restart silently loses. Nil keeps local-only resolution.
	PluginSources *pluginsource.Resolver
	// SandboxImage, when non-nil, resolves the deployment's effective
	// `sandbox: auto` fallback image (platform runtime setting over the env
	// default) at publish time so the pinned value rides the RunMessage.
	// Nil → the runner keeps its own env/built-in resolution.
	SandboxImage func(context.Context) string
	// UsageCaps, when non-nil, is the shared record of what the fleet has
	// learned about each subscription's own quota windows (pkg/usagecap,
	// written by every runner). The launch consults it to skip a forfait
	// whose window is CLOSED, which is what lets the run fall through to
	// the next credential tier instead of parking for a reset.
	UsageCaps usagecap.Store
	// CapPolicy, when non-nil, is the operator's usage-cap posture
	// (pkg/usagecap PolicySource). The walk consults it over the SAME
	// readings as the refusal skip: a credential the runner's pre-flight
	// is certain to park (hard cap reached, reset instant known) is
	// passed over like a refused one, so the tiers stay a fallback chain.
	// Nil keeps the walk refusal-evidence-only.
	CapPolicy usagecap.PolicySource
	// CredPool, when non-nil, lets a run with no credential of its own
	// draw on a contributor's lent subscription (pkg/credpool). Nil — the
	// default — simply means no pool tier.
	CredPool *credpool.Broker
	// Identity, when non-nil, lets the publisher resolve a run's team
	// to its parent org so the RunMessage carries the org id the launch
	// gate metered the run on. The runner charges LLM spend to that key
	// — without it (nil, or a pre-backfill org-less team) spend falls
	// back to the team key and an org-level monthly cost cap can never
	// accumulate against the org document.
	Identity TeamResolver
}

// TeamResolver is the slice of the identity store the publisher needs
// for org spend attribution.
type TeamResolver interface {
	GetTeam(ctx context.Context, id string) (identity.Team, error)
}

// Publisher is a runview.LaunchPublisher backed by NATS + Mongo.
type Publisher struct {
	nats       *natsq.Conn
	publishRun func(context.Context, *queue.RunMessage) error
	// publishRetryDelays is nil in production (the bounded default below).
	// Tests replace it with zero delays while exercising the same choke point.
	publishRetryDelays []time.Duration
	cancelRun          func(string) error
	// maxPayload reports the NATS server-negotiated max message size so
	// the offload path can size a RunMessage against it. Nil (the default
	// in unit tests) disables IR offload — the message is published as-is.
	maxPayload     func() int64
	store          store.RunStore
	runs           *mongo.Collection
	logger         *iterlog.Logger
	metrics        *metrics.Registry
	apiKeys        secrets.ApiKeyStore
	genericSecrets secrets.GenericSecretStore
	botBindings    secrets.BotSecretBindingStore
	runSecrets     secrets.RunSecretsStore
	sealer         secrets.Sealer
	oauthForfait   secrets.OAuthStore
	forgeConns     forge.ConnectionStore
	pluginSources  *pluginsource.Resolver
	sandboxImage   func(context.Context) string
	credPool       *credpool.Broker
	usageCaps      usagecap.Store
	capPolicy      usagecap.PolicySource
	identity       TeamResolver

	// orgCache memoizes team → org id so the publish hot path doesn't
	// add a Mongo read per launch (team/org membership changes are
	// rare; a 5-minute staleness only delays which usage doc new spend
	// lands on).
	orgCacheMu sync.Mutex
	orgCache   map[string]orgCacheEntry

	// detached tracks fire-and-forget goroutines (e.g. MarkUsed
	// observability writes) so Drain can wait for them on shutdown
	// rather than letting orphan Mongo writes pile up against a
	// pod that's already past the SIGTERM mark.
	detached sync.WaitGroup
}

type orgCacheEntry struct {
	orgID   string
	expires time.Time
}

const orgCacheTTL = 5 * time.Minute

// orgIDForTeam mirrors the launch gate's orgForTeam fallback: team →
// OrgID, "" on any miss (resolver absent, unknown team, org-less
// pre-backfill team) so the runner charges the tenant key — the same
// key the gate metered such a launch on.
func (p *Publisher) orgIDForTeam(ctx context.Context, teamID string) string {
	if p.identity == nil || teamID == "" {
		return ""
	}
	now := time.Now()
	p.orgCacheMu.Lock()
	if e, ok := p.orgCache[teamID]; ok && now.Before(e.expires) {
		p.orgCacheMu.Unlock()
		return e.orgID
	}
	p.orgCacheMu.Unlock()
	t, err := p.identity.GetTeam(ctx, teamID)
	if err != nil {
		// Not cached: a transient identity-store miss must not pin ""
		// for the TTL window.
		if p.logger != nil {
			p.logger.Warn("cloudpublisher: org resolve for team %s: %v (spend will charge the team key)", teamID, err)
		}
		return ""
	}
	p.orgCacheMu.Lock()
	if p.orgCache == nil {
		p.orgCache = make(map[string]orgCacheEntry)
	}
	p.orgCache[teamID] = orgCacheEntry{orgID: t.OrgID, expires: now.Add(orgCacheTTL)}
	p.orgCacheMu.Unlock()
	return t.OrgID
}

// New builds a Publisher.
func New(cfg Config) (*Publisher, error) {
	if cfg.NATS == nil {
		return nil, fmt.Errorf("cloudpublisher: NATS connection is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("cloudpublisher: Store is required")
	}
	if cfg.MongoColl == nil {
		return nil, fmt.Errorf("cloudpublisher: MongoColl is required for queue_position computation")
	}
	if cfg.Logger == nil {
		cfg.Logger = iterlog.New(iterlog.LevelInfo, nil)
	}
	return &Publisher{
		nats: cfg.NATS,
		publishRun: func(ctx context.Context, msg *queue.RunMessage) error {
			_, err := cfg.NATS.PublishRun(ctx, msg)
			return err
		},
		cancelRun:      cfg.NATS.CancelRun,
		maxPayload:     cfg.NATS.MaxPayload,
		store:          cfg.Store,
		runs:           cfg.MongoColl,
		logger:         cfg.Logger,
		metrics:        cfg.Metrics,
		apiKeys:        cfg.ApiKeys,
		genericSecrets: cfg.GenericSecrets,
		botBindings:    cfg.BotBindings,
		runSecrets:     cfg.RunSecrets,
		sealer:         cfg.Sealer,
		oauthForfait:   cfg.OAuthForfait,
		forgeConns:     cfg.ForgeConnections,
		pluginSources:  cfg.PluginSources,
		sandboxImage:   cfg.SandboxImage,
		credPool:       cfg.CredPool,
		usageCaps:      cfg.UsageCaps,
		capPolicy:      cfg.CapPolicy,
		identity:       cfg.Identity,
	}, nil
}

// allKnownProviders is the static set the publisher tries to resolve
// for every run. Providers without a configured key are simply
// omitted from the bundle; the runner falls back to env or surfaces
// "no credentials" at the LLM call site.
var allKnownProviders = []secrets.Provider{
	secrets.ProviderAnthropic,
	secrets.ProviderOpenAI,
	secrets.ProviderBedrock,
	secrets.ProviderVertex,
	secrets.ProviderAzure,
	secrets.ProviderOpenRouter,
	secrets.ProviderXAI,
	secrets.ProviderZAI,
}

func genericSecretNamesForWorkflow(wf *ir.Workflow) []string {
	if wf == nil || len(wf.Secrets) == 0 {
		return nil
	}
	names := make([]string, 0, len(wf.Secrets))
	for name, s := range wf.Secrets {
		if strings.TrimSpace(s.Value) != "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// requiredSecretNamesForWorkflow returns the declared secret names that MUST
// resolve to a non-empty value for the run to proceed: non-`optional` and with
// no inline literal `value:` (a literal is always "resolved"). These are the
// names the launch-time required-secret gate checks against the resolver's
// output.
func requiredSecretNamesForWorkflow(wf *ir.Workflow) []string {
	if wf == nil || len(wf.Secrets) == 0 {
		return nil
	}
	names := make([]string, 0, len(wf.Secrets))
	for name, s := range wf.Secrets {
		if s == nil || s.Optional || strings.TrimSpace(s.Value) != "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// appBotLoginForForgeToken returns the github_app bot login (e.g.
// "iterion-forge-1234[bot]") whose managed secret backs the run's resolved
// forge push token, or "" when the token is a PAT/OAuth/GitLab token (the
// runner resolves those from the token's own /user) or the store is
// unavailable. Best-effort: any store error yields "".
func (p *Publisher) appBotLoginForForgeToken(ctx context.Context, tenantID string, resolved map[string]secrets.GenericResolution) string {
	if p.forgeConns == nil || tenantID == "" {
		return ""
	}
	var secretID string
	for _, name := range []string{"forge_token", "github_token"} {
		if r, ok := resolved[name]; ok && r.SecretID != "" {
			secretID = r.SecretID
			break
		}
	}
	if secretID == "" {
		return ""
	}
	conns, err := p.forgeConns.ListByTenant(ctx, tenantID)
	if err != nil {
		return ""
	}
	for _, c := range conns {
		if c.Kind == forge.KindGitHubApp && c.ManagedSecretID == secretID && strings.TrimSpace(c.AccountLogin) != "" {
			return c.AccountLogin
		}
	}
	return ""
}

// resolveAndSealCredentials looks up every provider key visible to
// (tenantID, ownerID), pairs it with any OAuth-forfait the owner has
// connected — falling back to a contributor's pooled subscription when the
// tenant has none — seals the resulting bundle, and persists it under a
// fresh secrets ref. An empty ref means no credentials are available; the
// runner then falls back to env.
func (p *Publisher) resolveAndSealCredentials(ctx context.Context, runID, orgID, tenantID, ownerID, botID string, wf *ir.Workflow, keyOverrides, secretOverrides map[string]string, modelOverrides model.ModelOverrides, runFallbacks []model.FallbackEntry) (credResolution, error) {
	if p.runSecrets == nil || p.sealer == nil {
		return credResolution{}, nil
	}
	// Defence in depth: every caller (SubmitLaunch, SubmitResume)
	// already derives tenantID from either auth.FromContext or the
	// prior run document, but a future caller that forgets to thread
	// the identity must not silently produce a bundle keyed under
	// the empty tenant — that would let any team's runner unseal it.
	if tenantID == "" {
		if runID != "" && p.logger != nil {
			p.logger.Warn("cloudpublisher: refusing to seal credentials for run %s without a tenant_id", runID)
		}
		return credResolution{}, nil
	}
	bundle := secrets.RunBundle{
		APIKeys:            map[secrets.Provider]string{},
		GenericSecrets:     map[string]string{},
		GenericSecretHosts: map[string][]string{},
		GenericSecretRefs:  map[string]string{},
		OAuthCredentials:   map[string][]byte{},
		PlatformSourced:    map[string]bool{},
	}

	// Refused-but-only keys, per provider, remembered across the tiers for
	// the restore step — the api-key twin of skippedForfaits below.
	skippedAPIKeys := map[secrets.Provider]skippedAPIKey{}
	// Audit identity of the API key that filled each provider slot, for
	// the run-document stamp (the OAuth side rides bundle.OAuthFingerprints).
	apiKeyFPs := map[secrets.Provider]string{}

	// 1. BYOK API keys.
	if p.apiKeys != nil {
		// Per-webhook key overrides (provider name → api_key id) take
		// precedence over the org/user default inside secrets.Resolve.
		var overrides map[secrets.Provider]string
		if len(keyOverrides) > 0 {
			overrides = make(map[secrets.Provider]string, len(keyOverrides))
			for prov, keyID := range keyOverrides {
				overrides[secrets.Provider(prov)] = keyID
			}
		}
		// Evidence-based skip: a key the provider freshly refused is
		// passed over so the priority walk yields the next key of that
		// provider — the BYOK tier becomes an ordered fallback chain.
		resolved, err := secrets.Resolve(ctx, p.apiKeys, tenantID, ownerID, allKnownProviders, overrides, p.sealer,
			p.apiKeyUsable(ctx, usagecap.TenantScope(tenantID), runID))
		if err != nil {
			return credResolution{}, fmt.Errorf("cloudpublisher: resolve creds: %w", err)
		}
		now := time.Now().UTC()
		usedIDs := make([]string, 0, len(resolved))
		for prov, r := range resolved {
			if len(r.Plaintext) == 0 {
				continue
			}
			bundle.APIKeys[prov] = string(r.Plaintext)
			apiKeyFPs[prov] = r.Fingerprint
			usedIDs = append(usedIDs, r.KeyID)
		}
		// A provider whose EVERY key was refused resolves to nothing under
		// the predicate. Remember what an unfiltered walk would have chosen:
		// if the end of the resolution finds that wire still empty — no
		// second key, no forfait, no pool grant, no platform credential —
		// the refused key is restored, because a run that makes one refused
		// call parks on a durable usage-window retry, while a run published
		// with an empty wire fails on a no-credential auth error nothing
		// retries (or silently spends the runner pod's ambient env).
		if refused := providersWithoutKey(allKnownProviders, bundle.APIKeys); len(refused) > 0 {
			fallback, ferr := secrets.Resolve(ctx, p.apiKeys, tenantID, ownerID, refused, overrides, p.sealer, nil)
			if ferr != nil {
				p.logger.Warn("cloudpublisher: refused-key fallback resolve: %v", ferr)
			}
			for prov, r := range fallback {
				if len(r.Plaintext) == 0 {
					continue
				}
				skippedAPIKeys[prov] = skippedAPIKey{plaintext: string(r.Plaintext), keyID: r.KeyID, fingerprint: r.Fingerprint}
			}
		}
		// Bumping last_used_at is best-effort observability, not on
		// the launch's critical path. Fire it detached with a short
		// timeout so a slow Mongo write doesn't block the NATS
		// publish.
		if len(usedIDs) > 0 {
			ids, t, tenant := usedIDs, now, tenantID
			p.goSafeDetached("apikey-markused", func() {
				// MarkUsed is tenant-filtered; carry the run's tenant onto the
				// detached ctx (matching the generic-secrets path below) or the
				// update silently matches nothing and last_used_at never moves.
				bg, cancel := context.WithTimeout(store.WithTenant(context.Background(), tenant), 5*time.Second)
				defer cancel()
				for _, id := range ids {
					_ = p.apiKeys.MarkUsed(bg, id, t)
				}
			})
		}
	}

	// 2. Workflow/user generic secrets. A declared secret with an empty
	// value means "resolve a stored secret of the same name" for this run.
	if p.genericSecrets != nil && wf != nil && len(wf.Secrets) > 0 {
		names := genericSecretNamesForWorkflow(wf)
		resolved, err := secrets.ResolveGenericWithBindings(ctx, p.genericSecrets, p.botBindings, tenantID, ownerID, botID, names, secretOverrides, p.sealer, p.logger)
		if err != nil {
			return credResolution{}, fmt.Errorf("cloudpublisher: resolve workflow secrets: %w", err)
		}
		// Required-secret launch gate: a non-`optional` declared secret with no
		// inline value MUST resolve to a non-empty value. If it resolves to
		// nothing (store secret deleted, no binding, no override) fail the launch
		// loudly here — never let the runner skip the empty value and run the bot
		// with the credential unset. `optional: true` secrets are excluded and
		// keep the runner's skip behaviour.
		haveValue := make(map[string]bool, len(resolved))
		for name, r := range resolved {
			if len(r.Plaintext) > 0 {
				haveValue[name] = true
			}
		}
		if missing := secrets.UnresolvedRequired(requiredSecretNamesForWorkflow(wf), haveValue); len(missing) > 0 {
			return credResolution{}, secrets.RequiredSecretsError(missing, "this team/bot")
		}
		now := time.Now().UTC()
		usedIDs := make([]string, 0, len(resolved))
		for name, r := range resolved {
			if len(r.Plaintext) == 0 {
				// Resolved to a metadata-only record with no plaintext (e.g. a
				// nil-sealer resolution). Skip it — but trace the drop so an
				// operator debugging a missing credential isn't left grepping
				// for nothing. Required secrets are already gated loudly above.
				if p.logger != nil {
					p.logger.Debug("cloudpublisher: generic secret %q resolved with empty plaintext (scope=%s) — not injected", name, r.SourceScope)
				}
				continue
			}
			bundle.GenericSecrets[name] = string(r.Plaintext)
			// A binding-sourced resolution may carry an egress allowlist
			// that NARROWS where this credential can go. Thread it to the
			// runner, which intersects it with the workflow's declared
			// hosts in the secret guard. Empty = no binding restriction.
			if len(r.AllowedHosts) > 0 {
				bundle.GenericSecretHosts[name] = r.AllowedHosts
			}
			if r.SecretID != "" {
				// ID only (never the value): lets the runner re-read the
				// worker-refreshed store record mid-run — the snapshot above
				// outlives short-TTL credentials (App tokens live 1h).
				bundle.GenericSecretRefs[name] = r.SecretID
			}
			usedIDs = append(usedIDs, r.SecretID)
		}
		if len(usedIDs) > 0 {
			ids, t, tenant := usedIDs, now, tenantID
			p.goSafeDetached("generic-secret-markused", func() {
				bg, cancel := context.WithTimeout(store.WithTenant(context.Background(), tenant), 5*time.Second)
				defer cancel()
				for _, id := range ids {
					_ = p.genericSecrets.MarkUsed(bg, id, t)
				}
			})
		}
		// When the run's forge token came from a github_app connection,
		// thread the App bot login so the runner can seed the App-bot git
		// committer (an installation token can't `GET /user`). Best-effort:
		// a lookup failure just leaves the neutral fallback identity.
		if login := p.appBotLoginForForgeToken(ctx, tenantID, resolved); login != "" {
			bundle.ForgeAppBotLogin = login
		}
	}

	// 3. OAuth-forfait blobs. Resolution is user-primary with an org
	//    fallback: the run owner's personal forfait wins per kind, and
	//    for any kind the owner hasn't connected we fall back to the
	//    team/org credential (stored under OrgOwnerKey(tenantID)). The
	//    org fallback is what covers automated runs (webhook/dispatcher/
	//    cron) whose owner is a synthetic identity with no personal
	//    forfait. The runner falls back to env when neither an API key
	//    nor an OAuth bundle is present.
	skippedForfaits := map[string]skippedForfait{}
	if p.oauthForfait != nil {
		addOAuth := func(ownerKey, label string) {
			if ownerKey == "" {
				return
			}
			records, err := p.oauthForfait.ListByUser(ctx, ownerKey)
			if err != nil {
				p.logger.Warn("cloudpublisher: oauth list for %s: %v", ownerKey, err)
				return
			}
			for _, rec := range records {
				// User record wins; don't let the org fallback overwrite it.
				if _, exists := bundle.OAuthCredentials[string(rec.Kind)]; exists {
					continue
				}
				payload, err := secrets.OpenOAuthPayload(p.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
				if err != nil {
					p.logger.Warn("cloudpublisher: unseal oauth %s/%s: %v", rec.UserID, rec.Kind, err)
					continue
				}
				// A forfait whose provider window is CLOSED is not a
				// usable credential: handing it to the run means one LLM
				// call, a rate-limit refusal, and a park until the window
				// resets — up to a week on the weekly one — while another
				// tier (a second forfait, the pool) could have served it
				// immediately. Skipping it here is what makes the tiers a
				// FALLBACK CHAIN rather than a fixed first choice.
				if until, why := p.forfaitWindowClosed(ctx, tenantID, ownerKey, rec); !until.IsZero() {
					p.logger.Info("cloudpublisher: oauth-forfait(%s) SKIPPED for run=%s kind=%s fp=%s — %s (reopens %s); falling through to the next credential tier",
						label, runID, rec.Kind, rec.Fingerprint, why, until.UTC().Format(time.RFC3339))
					// Remembered: if the end of the resolution finds the
					// wire still empty, this forfait is restored — a
					// parked run with a durable retry beats a stuck one.
					if _, seen := skippedForfaits[string(rec.Kind)]; !seen {
						skippedForfaits[string(rec.Kind)] = skippedForfait{payload: payload, fp: rec.Fingerprint}
					}
					continue
				}
				bundle.OAuthCredentials[string(rec.Kind)] = payload
				setOAuthFingerprint(&bundle, string(rec.Kind), rec.Fingerprint)
				p.logger.Info("cloudpublisher: oauth-forfait(%s) used run=%s owner=%s kind=%s fp=%s", label, runID, ownerKey, rec.Kind, rec.Fingerprint)
			}
		}
		addOAuth(ownerID, "user")
		addOAuth(secrets.OrgOwnerKey(tenantID), "org")
	}

	// 4. Mutualised pool — the LAST resort, and only for a run that has no
	//    credential of its own at all. Spending a contributor's lent
	//    subscription while the tenant holds a usable key of its own would
	//    be taking a donation nobody needed; "the tenant is out of
	//    credentials" is the condition the pool exists for.
	res := credResolution{}
	if len(bundle.APIKeys) == 0 && len(bundle.OAuthCredentials) == 0 {
		if grant := p.acquireFromPool(ctx, runID, orgID, tenantID, ownerID, botID, wf, modelOverrides, runFallbacks); grant != nil {
			// The lent credential goes in the slot its KIND belongs to, so
			// the runner cannot tell a donation from the tenant's own.
			switch grant.Source {
			case credpool.SourceOAuth:
				bundle.OAuthCredentials[grant.Ref] = grant.Payload
				// The donor's own credential identity, so the borrower's
				// meter follows the lent subscription rather than the slot
				// it landed in: a donor who reconnects a fresh one is not
				// parked by the readings of the account it replaced.
				setOAuthFingerprint(&bundle, grant.Ref, grant.Fingerprint)
			case credpool.SourceAPIKey:
				prov := secrets.Provider(grant.Ref)
				bundle.APIKeys[prov] = string(grant.Payload)
				// The lent key's own audit identity — the same hash the
				// BYOK record carries and the runner derives, so the
				// GRANTED line, the run-document stamp and the metering-time
				// last_used_at bump all name the donor's key, not an
				// unstamped slot.
				apiKeyFPs[prov] = secrets.FingerprintSHA256(string(grant.Payload))
			}
			res.grant = grant
		}
	}

	// 5. Platform tier — the deployment's own DB-backed credentials, the
	//    last stop before the runner's env fallback (which stays: an empty
	//    platform store keeps today's behaviour byte-identical). Fills only
	//    the slots tiers 1–4 left empty, per WIRE FAMILY, so a platform key
	//    can never shadow a credential the run already holds in another
	//    shape (delegate precedence ranks an API key above an OAuth dir on
	//    the same wire). Skipped entirely when the pool granted: a granted
	//    run runs on its donor — filling alongside would outrank the lent
	//    credential while still consuming the donor's quota and slot.
	if res.grant == nil {
		p.fillFromPlatform(ctx, runID, &bundle, skippedAPIKeys, apiKeyFPs)
	}

	// A skipped credential is only an improvement when some other tier
	// could actually serve its wire. If nothing did — no second key, no
	// second forfait, no pool grant, no platform credential — restore it:
	// the run then parks on the provider refusal with a DURABLE
	// usage-window retry, instead of failing on a no-credential auth
	// error nothing retries. Keys before forfaits, in allKnownProviders
	// order, matching both the delegate's precedence on a shared wire and
	// the deterministic-winner rule of fillFromPlatform.
	if len(skippedForfaits) > 0 || len(skippedAPIKeys) > 0 {
		taken := map[string]bool{}
		for prov := range bundle.APIKeys {
			taken[secrets.WireFamily(string(prov))] = true
		}
		for kind := range bundle.OAuthCredentials {
			taken[secrets.WireFamily(kind)] = true
		}
		for _, prov := range allKnownProviders {
			sk, ok := skippedAPIKeys[prov]
			if !ok || taken[secrets.WireFamily(string(prov))] {
				continue
			}
			bundle.APIKeys[prov] = sk.plaintext
			apiKeyFPs[prov] = sk.fingerprint
			if sk.platform {
				bundle.PlatformSourced[string(prov)] = true
			}
			taken[secrets.WireFamily(string(prov))] = true
			p.logger.Info("cloudpublisher: refused api-key RESTORED for run=%s provider=%s — no other tier could serve; a parked run with a durable retry beats a stuck one", runID, prov)
		}
		for kind, sf := range skippedForfaits {
			if taken[secrets.WireFamily(kind)] {
				continue
			}
			bundle.OAuthCredentials[kind] = sf.payload
			setOAuthFingerprint(&bundle, kind, sf.fp)
			taken[secrets.WireFamily(kind)] = true
			p.logger.Info("cloudpublisher: window-closed forfait RESTORED for run=%s kind=%s fp=%s — no other tier could serve; a parked run with a durable retry beats a stuck one", runID, kind, sf.fp)
		}
	}

	// Record which review families the resolved credentials back — every
	// tier included (BYOK, oauth-forfait, pool grant, platform). This is
	// what lets SubmitLaunch resolve the credential-derived topology vars
	// (review_mode / plan_review / llm_families) for a queued run, where
	// no host detection report applies. Empty when nothing resolved: the
	// runner's env fallback is unknowable here, and injecting "no family"
	// facts about credentials we cannot see would be asserting a falsehood.
	providers := make([]string, 0, len(bundle.APIKeys))
	for prov := range bundle.APIKeys {
		providers = append(providers, string(prov))
	}
	oauthKinds := make([]string, 0, len(bundle.OAuthCredentials))
	for kind := range bundle.OAuthCredentials {
		oauthKinds = append(oauthKinds, kind)
	}
	res.families = reviewtopology.FamiliesFromCredentialNames(providers, oauthKinds)
	// Harvest every sealed credential's audit identity for the run-doc
	// stamp: API keys from the slot records above, OAuth from the bundle's
	// own fingerprint map (each fill site stamps it there).
	seen := map[string]bool{}
	for _, fp := range apiKeyFPs {
		if fp != "" && !seen[fp] {
			seen[fp] = true
			res.fingerprints = append(res.fingerprints, fp)
		}
	}
	for _, fp := range bundle.OAuthFingerprints {
		if fp != "" && !seen[fp] {
			seen[fp] = true
			res.fingerprints = append(res.fingerprints, fp)
		}
	}
	sort.Strings(res.fingerprints)

	// The moment "no LLM credential" becomes definitive: every tier
	// abstained, and the runner will either spend its pod's ambient env or
	// fail at the first LLM call. ONE Warn here, naming the tiers consulted
	// — the per-tier lines above say which said no and why. A workflow that
	// cannot call a model (tool-only, a nil workflow is unknown) spends
	// nothing and gets no Warn.
	//
	// Generic secrets do not fund an LLM call: a webhook-launched review
	// resolves a forge or tracker token into this same bundle, which is the
	// most common cloud shape, so gating the Warn on the whole bundle would
	// silence it exactly where it matters.
	noLLMCred := len(bundle.APIKeys) == 0 && len(bundle.OAuthCredentials) == 0
	if noLLMCred && (wf == nil || wf.UsesLLM()) {
		p.logger.Warn("cloudpublisher: no credential resolved for run=%s tenant=%s — tiers consulted: byok, oauth-forfait, pool, platform; the runner falls back to its env or fails at the first LLM call",
			runID, tenantID)
	}
	if noLLMCred && len(bundle.GenericSecrets) == 0 {
		return res, nil
	}

	// #659 pt 1: log the granted credential set once per run, symmetric
	// with the per-tier SKIPPED lines above. This is what lets an
	// operator answer "which credential funded that run?" from the
	// server logs, instead of grepping /proc/<pid>/environ inside a pod
	// (measured, 2026-09-03). Only fingerprints (never plaintext) and
	// tier tags — bundle.PlatformSourced already tracks the platform
	// vs tenant/tier split.
	logGrantedCredentials(p.logger, runID, bundle, apiKeyFPs, res.grant)

	sealed, err := secrets.SealRunBundle(p.sealer, runID, bundle)
	if err != nil {
		return res, fmt.Errorf("cloudpublisher: seal bundle: %w", err)
	}
	ref := secrets.NewSecretsRef()
	now := time.Now().UTC()
	rec := secrets.RunSecretsRecord{
		ID:           ref,
		TenantID:     tenantID,
		RunID:        runID,
		SealedBundle: sealed,
		CreatedAt:    now,
		ExpiresAt:    now.Add(secrets.DefaultRunSecretsTTL),
	}
	if err := p.runSecrets.Put(ctx, rec); err != nil {
		return res, fmt.Errorf("cloudpublisher: persist run secrets: %w", err)
	}
	res.secretsRef = ref
	return res, nil
}

// credResolution is what resolving a run's credentials produced: the sealed
// bundle's ref, plus the pool grant when a contributor's subscription was
// what made the run possible. The grant travels back to the caller because
// it carries the donor's remaining allowance, which must become the run's
// own cost ceiling.
type credResolution struct {
	secretsRef string
	grant      *credpool.Grant
	// families is the set of review families the sealed credentials back
	// (reviewtopology), so the launch can resolve the credential-derived
	// topology vars for a queued run. Empty = nothing resolved (env
	// fallback), in which case no injection happens.
	families reviewtopology.FamilySet
	// fingerprints are the audit identities of every credential the
	// bundle sealed — API keys and OAuth records alike, never secrets.
	// The caller stamps them on the run document so the per-key
	// concurrency meter can count alive runs by credential.
	fingerprints []string
}

// setOAuthFingerprint stamps the audit identity of the credential that
// filled an OAuth slot, whichever tier it came from. Every tier must do it:
// the runner's usage-cap meter keys on this, and a slot filled without one
// falls back to the historical slot-shaped meter — where a rotated
// credential inherits the exhausted readings of the account it replaced.
// An empty fingerprint (a record predating stamping) is left absent rather
// than written blank, so the historical key stays reachable.
func setOAuthFingerprint(bundle *secrets.RunBundle, kind, fp string) {
	if fp == "" {
		return
	}
	if bundle.OAuthFingerprints == nil {
		bundle.OAuthFingerprints = map[string]string{}
	}
	bundle.OAuthFingerprints[kind] = fp
}

// fillFromPlatform fills the API-key and OAuth slots still empty after the
// tenant tiers and the pool from the platform stores — the DB-backed form
// of the runner-pod env fallback, so rotating the deployment's credential
// is one CLI call instead of a redeploy. It fills per WIRE FAMILY
// (secrets.WireFamily), never adding a credential to a wire the run
// already holds in another shape — the delegates rank a ctx API key above
// a ctx OAuth dir, so a platform key filled next to a tenant's own forfait
// would silently serve every call. Filled slots are recorded on
// bundle.PlatformSourced so the runner's usage-cap scope check keeps
// metering them on the shared platform key rather than per tenant.
//
// Best-effort like the pool: a degraded store read or unseal failure logs
// and leaves the slot to the env fallback — it must never fail a launch
// that env can still serve.
func (p *Publisher) fillFromPlatform(ctx context.Context, runID string, bundle *secrets.RunBundle, skippedAPIKeys map[secrets.Provider]skippedAPIKey, apiKeyFPs map[secrets.Provider]string) {
	if p.sealer == nil {
		return
	}
	taken := map[string]bool{}
	for prov := range bundle.APIKeys {
		taken[secrets.WireFamily(string(prov))] = true
	}
	for kind := range bundle.OAuthCredentials {
		taken[secrets.WireFamily(kind)] = true
	}
	fillable := func(slot string) bool { return !taken[secrets.WireFamily(slot)] }

	// Platform API keys live under the sentinel tenant; the ctx tenant must
	// match or the store's isolation filter (correctly) returns nothing.
	if p.apiKeys != nil {
		missing := make([]secrets.Provider, 0, len(allKnownProviders))
		for _, prov := range allKnownProviders {
			if fillable(string(prov)) {
				missing = append(missing, prov)
			}
		}
		if len(missing) > 0 {
			pctx := store.WithTenant(ctx, secrets.PlatformTenantID)
			resolved, err := secrets.Resolve(pctx, p.apiKeys, secrets.PlatformTenantID, "", missing, nil, p.sealer,
				p.apiKeyUsable(pctx, usagecap.ScopePlatform, runID))
			if err != nil {
				p.logger.Warn("cloudpublisher: platform api-key resolve: %v", err)
			} else {
				now := time.Now().UTC()
				usedIDs := make([]string, 0, len(resolved))
				// Iterate `missing` (allKnownProviders order), NOT the
				// resolved map: when the platform holds two keys on one wire
				// family (anthropic + zai both map to "anthropic-wire"),
				// fillable() lets only the first through — and map iteration
				// order is randomised, so which key funds a run would flip
				// between launches. The provider slice fixes the winner.
				for _, prov := range missing {
					r, ok := resolved[prov]
					if !ok || len(r.Plaintext) == 0 || !fillable(string(prov)) {
						continue
					}
					bundle.APIKeys[prov] = string(r.Plaintext)
					bundle.PlatformSourced[string(prov)] = true
					apiKeyFPs[prov] = r.Fingerprint
					taken[secrets.WireFamily(string(prov))] = true
					usedIDs = append(usedIDs, r.KeyID)
					p.logger.Info("cloudpublisher: platform credential used run=%s slot=%s", runID, prov)
				}
				if len(usedIDs) > 0 {
					ids, t := usedIDs, now
					p.goSafeDetached("platform-apikey-markused", func() {
						bg, cancel := context.WithTimeout(store.WithTenant(context.Background(), secrets.PlatformTenantID), 5*time.Second)
						defer cancel()
						for _, id := range ids {
							_ = p.apiKeys.MarkUsed(bg, id, t)
						}
					})
				}
			}
			// This tier is the last DB-backed one and the runner's env
			// backstop is invisible from here, so a refused platform key
			// must not vanish outright — the rule the OAuth branch below
			// states verbatim. It is remembered for the restore step
			// instead, behind any tenant key of the same provider, whose
			// restore takes precedence.
			if refused := providersWithoutKey(missing, bundle.APIKeys); len(refused) > 0 {
				fallback, ferr := secrets.Resolve(pctx, p.apiKeys, secrets.PlatformTenantID, "", refused, nil, p.sealer, nil)
				if ferr != nil {
					p.logger.Warn("cloudpublisher: platform refused-key fallback resolve: %v", ferr)
				}
				for prov, r := range fallback {
					if len(r.Plaintext) == 0 {
						continue
					}
					if _, seen := skippedAPIKeys[prov]; seen {
						continue
					}
					skippedAPIKeys[prov] = skippedAPIKey{plaintext: string(r.Plaintext), keyID: r.KeyID, fingerprint: r.Fingerprint, platform: true}
				}
			}
		}
	}

	// Platform OAuth-forfait blobs under the reserved owner key. Skipped —
	// like the api-key read above via its `missing` guard — when no OAuth
	// kind is still fillable, so a run already funded on both wires costs
	// zero extra store reads here.
	if p.oauthForfait != nil &&
		(fillable(string(secrets.OAuthKindClaudeCode)) || fillable(string(secrets.OAuthKindCodex))) {
		records, err := p.oauthForfait.ListByUser(ctx, secrets.PlatformOwnerKey)
		if err != nil {
			p.logger.Warn("cloudpublisher: platform oauth list: %v", err)
			return
		}
		for _, rec := range records {
			if !fillable(string(rec.Kind)) {
				continue
			}
			// Deliberately NO window skip here: the platform tier is the
			// last DB-backed tier, and the runner's env backstop is
			// invisible from the publisher. Skipping a refused platform
			// forfait could only trade a self-healing park (one refused
			// call, durable usage-window retry) for a possibly-stuck run
			// with no credential at all.
			payload, err := secrets.OpenOAuthPayload(p.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
			if err != nil {
				p.logger.Warn("cloudpublisher: unseal platform oauth %s: %v", rec.Kind, err)
				continue
			}
			bundle.OAuthCredentials[string(rec.Kind)] = payload
			bundle.PlatformSourced[string(rec.Kind)] = true
			// The platform forfait is ONE meter for the whole deployment
			// (ScopePlatform), which makes rotating it exactly the lived
			// failure one tier up: without the identity, a super-admin who
			// swaps in a fresh subscription inherits the exhausted
			// readings of the one it replaced, fleet-wide.
			setOAuthFingerprint(bundle, string(rec.Kind), rec.Fingerprint)
			taken[secrets.WireFamily(string(rec.Kind))] = true
			p.logger.Info("cloudpublisher: platform credential used run=%s slot=%s fp=%s", runID, rec.Kind, rec.Fingerprint)
		}
	}
}

// poolWantOrder is the order the pool is asked for a credential when a run
// has none.
//
// Subscriptions first, and claude_code before codex: a lent Claude forfait
// runs natively on the Claude Code CLI, drawing on a plan its lender has
// already paid for. Metered keys come last on purpose — spending one costs
// its lender real money per token, so it is the option of last resort even
// among donations.
var poolWantOrder = func() []credpool.Credential {
	out := []credpool.Credential{
		{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)},
		{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindCodex)},
	}
	for _, prov := range allKnownProviders {
		out = append(out, credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(prov)})
	}
	return out
}()

// skippedForfait remembers a window-closed forfait so the end of the
// resolution can restore it when no other tier served its wire.
type skippedForfait struct {
	payload []byte
	fp      string
}

// skippedAPIKey is a provider's refused-but-only key, held back by the
// evidence predicate and restored when no other tier served its wire.
// platform marks the deployment's own key so the restore keeps its
// PlatformSourced metering scope.
type skippedAPIKey struct {
	plaintext   string
	keyID       string
	fingerprint string
	platform    bool
}

// providersWithoutKey returns the subset of provs that filled no API-key
// slot — the candidates for the refused-key fallback lookup.
func providersWithoutKey(provs []secrets.Provider, apiKeys map[secrets.Provider]string) []secrets.Provider {
	var missing []secrets.Provider
	for _, prov := range provs {
		if apiKeys[prov] == "" {
			missing = append(missing, prov)
		}
	}
	return missing
}

// usageCapLookupTimeout bounds the meter read on the launch path — the
// same ceiling the runner puts on the identical call. The meter is an
// optimisation; a slow store must not hold a launch.
const usageCapLookupTimeout = 5 * time.Second

// forfaitWindowClosed reports when a forfait's provider window is closed
// (and why), or the zero time when it is usable.
//
// Two signals close a window, because two gates can park the run:
//   - the provider's own refusal (a fresh StatusRejected reading) — true
//     without any operator policy, so the skip works on a deployment
//     that never configured a cap;
//   - the operator's HARD usage cap over the same readings (CapPolicy,
//     usagecap.Preflight): the runner's pre-flight enforces it before
//     any node runs, so a credential at a hard cap with a known reset
//     is not a usable credential — granting it parks the run for that
//     reset while a lower tier could have served. Soft caps warn, they
//     never close a window here. (This deliberately supersedes the
//     earlier evidence-only doctrine: it kept the walk from spending
//     another tier to dodge a tenant budget, but the week cap is the
//     operator's machine-wide posture, the fallthrough tiers are the
//     same operator's, and the pre-flight parks either way.)
//
// Everything uncertain means "usable", because a wrong skip spends
// somebody else's quota for a subscription that would have worked:
//   - no store wired, or no fingerprint ⇒ usable (nothing to judge with);
//   - no reading for this credential ⇒ usable ("nothing learned yet"
//     is the store's documented meaning for an unknown key);
//   - a STALE reading ⇒ usable (Fresh already encodes "past its own
//     reset", so a window that reopened stops blocking by itself);
//   - allowed/warning at ANY utilization, reset instant or not ⇒ usable.
func (p *Publisher) forfaitWindowClosed(ctx context.Context, tenantID, ownerKey string, rec secrets.OAuthRecord) (time.Time, string) {
	backend := usageBackendForKind(rec.Kind)
	scope := usagecap.ScopePlatform
	if ownerKey != secrets.PlatformOwnerKey && tenantID != "" {
		scope = usagecap.TenantScope(tenantID)
	}
	return p.refusedUntil(ctx, backend, scope, rec.Fingerprint, string(rec.Kind))
}

// refusedUntil is the ONE evidence reading shared by every credential-skip
// site (the oauth-forfait tiers and the BYOK api-key walk): it reports when
// fresh provider refusals against this credential lapse, or the zero time
// when the credential is usable. label is for the lookup-failure log only.
func (p *Publisher) refusedUntil(ctx context.Context, backend string, scope string, fingerprint, label string) (time.Time, string) {
	if p.usageCaps == nil || fingerprint == "" || backend == "" {
		return time.Time{}, ""
	}
	lctx, cancel := context.WithTimeout(ctx, usageCapLookupTimeout)
	defer cancel()
	readings, err := p.usageCaps.Latest(lctx, usagecap.Key(backend, scope, fingerprint))
	if err != nil {
		// The meter is an optimisation; its failure must not change which
		// credential a run gets.
		p.logger.Warn("cloudpublisher: usage-cap lookup for %s/%s: %v", label, fingerprint, err)
		return time.Time{}, ""
	}
	now := time.Now()
	// When several windows are refused, the one that reopens LAST rules:
	// the credential stays unusable until every refusal has lapsed.
	var until time.Time
	var why string
	for _, r := range readings {
		if r.Status != usagecap.StatusRejected || !r.Fresh(now, usagecap.DefaultMaxAge) {
			continue
		}
		// Windows usagecap itself never blocks on are no evidence here
		// either: a rejected OVERAGE reading is about the pay-as-you-go
		// money channel, and an unknown window must not be folded into
		// a rule that was never meant to govern it (usagecap.FamilyOf's
		// own contract). The store is populated unfiltered, so the
		// filter has to live at the consumer.
		if usagecap.FamilyOf(r.Window) == usagecap.FamilyNone {
			continue
		}
		reopen := r.ResetsAt
		if reopen.IsZero() {
			// A refusal with no reset instant is trusted only for the
			// reading's own staleness bound.
			reopen = r.ObservedAt.Add(usagecap.DefaultMaxAge)
		}
		if reopen.After(until) {
			until = reopen
			// Frequency and auth are refusals, not windows with a fill
			// level — "(0% used)" on them would read as a contradiction.
			switch r.Window {
			case usagecap.WindowAuth:
				why = "provider rejected the credential itself (auth failure)"
			case usagecap.WindowFrequency:
				why = "provider refused the account's request rate (fair-usage)"
			case usagecap.WindowSpend:
				why = "the account's spend ceiling is reached — an admin must raise it (claude.ai/settings/usage)"
			default:
				why = fmt.Sprintf("provider refused the %s window (%.0f%% used)", r.Window, r.Percent())
			}
		}
	}
	if until.IsZero() && p.capPolicy != nil {
		// No provider refusal on record — but the runner's pre-flight
		// enforces the operator's caps over these same readings and parks
		// BEFORE any node runs; otherwise the walk re-grants the same
		// capped credential on every retry while a usable lower tier sits
		// unreached, and the park writes no refusal that would ever break
		// the loop.
		//
		// The gate mirrored is Decision.Blocked, NOT Stop: soft means
		// "never interrupts work in flight", and it still lets no NEW run
		// start (docs/usage-caps.md) — which is precisely what a launch
		// is. Requiring Stop would leave the shipped default posture (5h
		// soft) reproducing the very loop this closes, and would also
		// mask a hard week decision whenever Preflight's latest-reset
		// arbitration picks a soft five-hour one.
		//
		// A blocked window with no reset instant is trusted for the
		// reading's own staleness bound, the same synthesis the refusal
		// branch above applies: bounded, self-healing, and symmetric.
		if d := usagecap.Preflight(readings, p.capPolicy.Effective(ctx), now, usagecap.DefaultMaxAge); d.Blocked {
			reopen := d.ResetsAt
			if reopen.IsZero() {
				reopen = now.Add(usagecap.DefaultMaxAge)
			}
			until = reopen
			why = fmt.Sprintf("the operator's cap on the %s window is reached (%.0f%% ≥ %.0f%%)", d.Window, d.Percent, d.Cap)
		}
	}
	return until, why
}

// usageBackendForKind maps a forfait kind to the meter backend the runner
// records under. "" for a kind whose windows are not metered.
func usageBackendForKind(kind secrets.OAuthKind) string {
	if kind == secrets.OAuthKindClaudeCode {
		return delegate.BackendClaudeCode
	}
	return ""
}

// usageBackendForProvider maps a BYOK api-key provider to the meter backend
// its refusals are recorded under. Anthropic-shaped keys (the real one and
// the z.ai facade) are spent by claude_code sessions, so that is where the
// runner meters them. "" for a provider with no metered evidence — those
// keys are never skipped.
func usageBackendForProvider(prov secrets.Provider) string {
	switch prov {
	case secrets.ProviderAnthropic, secrets.ProviderZAI:
		return delegate.BackendClaudeCode
	}
	return ""
}

// apiKeyUsable builds the Resolve predicate for one resolution: a key with
// fresh provider-refusal evidence under its fingerprint is skipped, and the
// priority walk hands the NEXT key of that provider over — the ordered
// fallback the 2026-09-02 fair-usage freeze was routed around by hand.
// Everything uncertain means "usable", same contract as refusedUntil.
func (p *Publisher) apiKeyUsable(ctx context.Context, scope string, runID string) func(secrets.ApiKey) bool {
	return func(k secrets.ApiKey) bool {
		// Concurrency ceiling first — cheaper than the meter read, and a
		// key at its ceiling must rest whatever its refusal history says.
		// Providers with fair-usage frequency limits publish no numeric
		// bound to adapt to; the operator sets one on the key, and the
		// walk passes a full key over exactly like a refused one. Fails
		// OPEN on a count error: the ceiling is protection against
		// tripping a provider, not a correctness invariant.
		if k.MaxConcurrentRuns > 0 && p.store != nil && k.Fingerprint != "" {
			n, err := p.store.CountAliveRunsWithCredFingerprint(ctx, k.Fingerprint, runID)
			if err != nil {
				p.logger.Warn("cloudpublisher: concurrency count for api-key(%s) %q: %v — ceiling not applied", k.Provider, k.Name, err)
			} else if n >= k.MaxConcurrentRuns {
				p.logger.Info("cloudpublisher: api-key(%s) %q AT ITS CEILING for run=%s (%d/%d alive runs); trying the next key of this provider",
					k.Provider, k.Name, runID, n, k.MaxConcurrentRuns)
				return false
			}
		}
		backend := usageBackendForProvider(k.Provider)
		until, why := p.refusedUntil(ctx, backend, scope, k.Fingerprint, string(k.Provider))
		if until.IsZero() {
			return true
		}
		p.logger.Info("cloudpublisher: api-key(%s) %q SKIPPED for run=%s — %s (reopens %s); trying the next key of this provider",
			k.Provider, k.Name, runID, why, until.UTC().Format(time.RFC3339))
		return false
	}
}

// providerOfWant maps a want back to the LLM provider it authenticates
// against, so a run that pinned its models can be matched to donations it
// could actually use. "" for a want with no single provider.
func providerOfWant(w credpool.Credential) string {
	if w.Source == credpool.SourceAPIKey {
		return w.Ref
	}
	switch secrets.OAuthKind(w.Ref) {
	case secrets.OAuthKindClaudeCode:
		return string(secrets.ProviderAnthropic)
	case secrets.OAuthKindCodex:
		return string(secrets.ProviderOpenAI)
	}
	return ""
}

// buildModelOverrides folds launch entries into the executor's overrides.
// A tiny type-adapter over model.OverridesFrom — the single source of
// truth now lives in pkg/backend/model, next to ModelOverrides itself,
// so the runner's own fold consumes the same helper.
func buildModelOverrides(entries []runview.ModelOverrideEntry) model.ModelOverrides {
	out := make([]model.OverrideEntry, len(entries))
	for i, e := range entries {
		out[i] = model.OverrideEntry{Selector: e.Selector, Backend: e.Backend, Model: e.Model, Provider: e.Provider}
	}
	return model.OverridesFrom(out)
}

// buildModelOverridesFromRun is the resume-path twin (persisted form).
func buildModelOverridesFromRun(entries []store.RunModelOverride) model.ModelOverrides {
	out := make([]model.OverrideEntry, len(entries))
	for i, e := range entries {
		out[i] = model.OverrideEntry{Selector: e.Selector, Backend: e.Backend, Model: e.Model, Provider: e.Provider}
	}
	return model.OverridesFrom(out)
}

// runFallbackEntries adapts the launch's fallback chain onto
// model.FallbackEntry, so the wants derivation sees the operator's
// run-level `--fallback` alongside per-node fallbacks.
func runFallbackEntries(entries []runview.FallbackEntry) []model.FallbackEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]model.FallbackEntry, len(entries))
	for i, e := range entries {
		out[i] = model.FallbackEntry{Backend: e.Backend, Model: e.Model, Provider: e.Provider}
	}
	return out
}

// runFallbackEntriesFromRun is the resume-path twin.
func runFallbackEntriesFromRun(entries []store.RunFallbackEntry) []model.FallbackEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]model.FallbackEntry, len(entries))
	for i, e := range entries {
		out[i] = model.FallbackEntry{Backend: e.Backend, Model: e.Model, Provider: e.Provider}
	}
	return out
}

// knownPoolProviders is the provider vocabulary the wants derivation
// resolves against — derived from allKnownProviders so the two cannot
// drift. A hint outside it names a pin the pool cannot serve by name.
var knownPoolProviders = func() map[string]bool {
	m := make(map[string]bool, len(allKnownProviders))
	for _, p := range allKnownProviders {
		m[strings.ToLower(string(p))] = true
	}
	return m
}()

// wantsFor narrows the pool request to donations a run can actually spend,
// and returns the resolution it narrowed on for the log lines.
//
// A bot that pins `model: "anthropic/…"` has no use for a lent z.ai key:
// granting one would consume a unit of the donor's daily runs and hold a
// concurrency slot for a run that then fails at the first LLM call, and
// every retry would pick the same wrong donation. The narrowing is exact
// per provider because the delegates are: a `zai` hint spends a z.ai key
// and nothing else (no fall-through to the OAuth dir), `anthropic` spends
// the Anthropic key or the claude_code forfait.
//
// And it FAILS OPEN. A run that pins nothing takes the FULL order — claw
// substitutes the first available provider, so any donation serves it —
// and so does any run with ONE route the walk cannot resolve (no pin, an
// explicit "auto", a ${VAR} empty here, a name nobody knows, a
// model-answering node with no LLMFields): that route takes whatever the
// process holds, so narrowing on its pinned peers would drop the very
// donation it needs. A run whose cross-family reviewer pins openai but
// whose implementer pins nothing keeps the claude_code forfait in its
// wants, and a typo in a provider hint costs a Warn, never the pool tier.
func wantsFor(wf *ir.Workflow, overrides model.ModelOverrides, runFallbacks []model.FallbackEntry) ([]credpool.Credential, model.ProviderResolution) {
	res := model.EffectiveProviders(wf, overrides, runFallbacks, knownPoolProviders)
	if !res.NarrowSafe || len(res.Providers) == 0 {
		return poolWantOrder, res
	}
	pinned := make(map[string]bool, len(res.Providers))
	for _, p := range res.Providers {
		pinned[p] = true
	}
	out := make([]credpool.Credential, 0, len(poolWantOrder))
	for _, w := range poolWantOrder {
		if prov := providerOfWant(w); prov != "" && pinned[prov] {
			out = append(out, w)
		}
	}
	return out, res
}

// acquireFromPool asks the credential pool for a donor. Returns nil when no
// pool is wired, none serves this requester, or every donor is currently
// unavailable — all ordinary outcomes that must let the launch proceed
// exactly as it would have without a pool. Every abstention is logged Warn
// at the moment it becomes final: the run is about to spend someone else's
// credential (platform tier) or fail at the LLM call, both worth a line an
// operator can grep instead of half a day of investigation on a path that
// decides who pays (#654).
func (p *Publisher) acquireFromPool(ctx context.Context, runID, orgID, tenantID, ownerID, botID string, wf *ir.Workflow, overrides model.ModelOverrides, runFallbacks []model.FallbackEntry) *credpool.Grant {
	if p.credPool == nil {
		// No pool at all — a static configuration fact. Debug, not Warn:
		// pkg/log forwards Warn to the error tracker's breadcrumbs, and a
		// platform-funded deployment with no pool would emit one per
		// credential-less launch, forever. The definitive "no credential"
		// Warn at the end of resolveAndSealCredentials is the signal.
		p.logger.Debug("cloudpublisher: credential pool NOT CONSULTED for run %s — no pool broker wired (reason=%s)",
			runID, credpool.ReasonPoolDisabled)
		return nil
	}
	// One walk per launch: the wants, the diagnostics and the consulted
	// line all read the same resolution.
	wants, res := wantsFor(wf, overrides, runFallbacks)
	if len(res.Unknown) > 0 {
		// A pin nobody knows never narrows the request to nothing: the
		// walk widened to the full order instead. Name the pin — a typo
		// in `provider:` is worth one line, not a silently skipped tier.
		p.logger.Warn("cloudpublisher: credential pool asked for the FULL order for run %s — provider hint(s) match no known provider: %s (%s)",
			runID, strings.Join(res.Unknown, ","), providersSummary(res.Providers))
	}
	if len(wants) == 0 {
		// Every route resolved to a KNOWN provider the pool never lends.
		// The want order covers every known provider today, so this is
		// the guard for a narrower order tomorrow, not a live path.
		p.logger.Warn("cloudpublisher: credential pool NOT CONSULTED for run %s — bot pins provider(s) no donation matches (%s)",
			runID, providersSummary(res.Providers))
		return nil
	}
	// Trace the derived wants once per run — the pointer the incident
	// this fix serves (#668) asked for so an operator can see what the
	// resolver is asking on their behalf, not only what it got back.
	p.logger.Info("cloudpublisher: credential pool consulted for run %s — wants=%s (%s narrow_safe=%v)",
		runID, wantsSummary(wants), providersSummary(res.Providers), res.NarrowSafe)
	grant, err := p.credPool.Acquire(ctx, credpool.Request{
		RunID:    runID,
		OrgID:    orgID,
		TenantID: tenantID,
		UserID:   ownerID,
		BotID:    botID,
		Wants:    wants,
	})
	if err != nil {
		// A typed abstention names the reason: distinguishing "the audience
		// refused me" from "every donor was cooling" is exactly the class
		// of ambiguity that turned a half-day of investigation into "the
		// pool answered nothing".
		var nd *credpool.NoDonorError
		if errors.As(err, &nd) {
			if nd.Reason == credpool.ReasonNoEnabledPool {
				// Static configuration, like the nil broker above.
				p.logger.Debug("cloudpublisher: credential pool NOT CONSULTED for run %s — no enabled pool (reason=%s)", runID, nd.Reason)
				return nil
			}
			// A pool EXISTS and refused: which pools opened, how many
			// pledges of a wanted kind were walked, and what state held
			// each one out (paused / unhealthy / out_of_hours /
			// bot_filtered / cooling / exhausted / serving).
			p.logger.Warn("cloudpublisher: credential pool declined run %s — reason=%s pools_enabled=%d pools_admitted=%d pledges_considered=%d skips=%s wants=%s",
				runID, nd.Reason, nd.PoolsEnabled, nd.PoolsAdmitted, nd.PledgesConsidered, pledgeSkipSummary(nd.Skips), wantsSummary(wants))
			return nil
		}
		if errors.Is(err, credpool.ErrNoDonor) {
			// A wrapper we did not recognise still counts as an abstention;
			// keep it visible instead of falling silently to the next tier.
			p.logger.Warn("cloudpublisher: credential pool declined run %s — %v", runID, err)
			return nil
		}
		// A store failure must not fail the launch: the pool is a
		// best-effort extra tier, and a run with no credential still
		// surfaces a legible error at the LLM call site.
		p.logger.Warn("cloudpublisher: credential pool lookup for run %s: %v", runID, err)
		return nil
	}
	p.logger.Info("cloudpublisher: run %s runs on a pooled contributor credential (donor=%s %s allowance=$%.2f)",
		runID, grant.DonorID, grant.Credential, grant.RemainingUSD)
	return grant
}

// wantsSummary renders a wants list for the abstention log — the field
// that answers "was this decision even made against the right set?".
func wantsSummary(wants []credpool.Credential) string {
	if len(wants) == 0 {
		return "<none>"
	}
	parts := make([]string, 0, len(wants))
	for _, w := range wants {
		parts = append(parts, string(w.Source)+":"+w.Ref)
	}
	return strings.Join(parts, ",")
}

// providersSummary renders a set of provider names for a log line.
// "<none>" when empty — the log stays readable when a walk found nothing.
func providersSummary(provs []string) string {
	if len(provs) == 0 {
		return "pinned=<none>"
	}
	return "pinned=" + strings.Join(provs, ",")
}

// pledgeSkipSummary renders the per-pledge decline states an abstention
// carries — `<pledge-id>:<status>` per donor, "<none>" when the walk
// considered nobody.
func pledgeSkipSummary(skips []credpool.PledgeSkip) string {
	if len(skips) == 0 {
		return "<none>"
	}
	parts := make([]string, 0, len(skips))
	for _, sk := range skips {
		parts = append(parts, sk.PledgeID+":"+string(sk.Status))
	}
	return strings.Join(parts, ",")
}

// logGrantedCredentials emits ONE INFO line per run listing which
// credential funded which slot at the end of resolveAndSealCredentials.
// Symmetric with the per-tier SKIPPED log lines the file already emits
// (a grant used to be silent — #659 pt 1), and the answer an operator
// needed to `grep run=<id>` for instead of `/proc/<pid>/environ` inside
// a pod (measured, 2026-09-03). The line carries the credential's
// TIER (byok / oauth-forfait / pool / platform), the slot name
// (provider or oauth kind), and the FINGERPRINT — never plaintext.
func logGrantedCredentials(logger *iterlog.Logger, runID string, bundle secrets.RunBundle, apiKeyFPs map[secrets.Provider]string, grant *credpool.Grant) {
	if logger == nil {
		return
	}
	parts := make([]string, 0, len(bundle.APIKeys)+len(bundle.OAuthCredentials))
	// API keys — the PlatformSourced map tells apart platform-tier
	// grants (deployment-wide fallback keys) from tenant BYOK.
	for _, prov := range allKnownProviders {
		if _, ok := bundle.APIKeys[prov]; !ok {
			continue
		}
		tier := "byok"
		switch {
		case grant != nil && grant.Source == credpool.SourceAPIKey && grant.Ref == string(prov):
			tier = "pool"
		case bundle.PlatformSourced[string(prov)]:
			tier = "platform"
		}
		fp := apiKeyFPs[prov]
		if fp == "" {
			fp = "<unstamped>"
		}
		parts = append(parts, fmt.Sprintf("%s(api_key:%s fp=%s)", tier, prov, fp))
	}
	// OAuth-forfait — grant != nil means a donor's subscription came in
	// via the pool tier; the platform sentinel marks the deployment's
	// own fallback forfait; otherwise it is a user-or-org connect.
	for _, kind := range []string{string(secrets.OAuthKindClaudeCode), string(secrets.OAuthKindCodex)} {
		if _, ok := bundle.OAuthCredentials[kind]; !ok {
			continue
		}
		tier := "oauth-forfait"
		if grant != nil && grant.Ref == kind && grant.Source == credpool.SourceOAuth {
			tier = "pool"
		} else if bundle.PlatformSourced[kind] {
			tier = "platform"
		}
		fp := bundle.OAuthFingerprints[kind]
		if fp == "" {
			fp = "<unstamped>"
		}
		parts = append(parts, fmt.Sprintf("%s(oauth:%s fp=%s)", tier, kind, fp))
	}
	if len(parts) == 0 {
		return
	}
	logger.Info("cloudpublisher: credentials GRANTED for run=%s — %s", runID, strings.Join(parts, ", "))
}

// SubmitLaunch persists the run as queued in Mongo, then publishes
// the RunMessage to JetStream. The runner pool drains the queue and
// transitions queued → running on pickup.
//
// Tenant and owner identifiers are pulled from ctx (stamped by the
// server's auth middleware) and propagate to both the persisted Run
// document and the NATS message so the runner can verify isolation.
func (p *Publisher) SubmitLaunch(ctx context.Context, runID string, spec runview.LaunchSpec, wf *ir.Workflow, hash string) (int, error) {
	// 1. Persist the run with status=queued + workflow_hash + file_path
	//    so List endpoints see it instantly and Resume can reload the
	//    workflow. Single SaveRun (upsert) avoids the CreateRun → LoadRun
	//    → SaveRun round-trip the previous shape required.
	now := time.Now().UTC()
	tenantID, _ := store.TenantFromContext(ctx)
	ownerID, _ := store.OwnerFromContext(ctx)
	// One inputs map shared by the run doc and the RunMessage, so the
	// credential-derived topology injection below reaches both carriers.
	inputs := varsAsAny(spec.Vars)
	r := &store.Run{
		FormatVersion:   store.RunFormatVersion,
		ID:              runID,
		WorkflowName:    wf.Name,
		WorkflowHash:    hash,
		FilePath:        spec.FilePath,
		Status:          store.RunStatusQueued,
		Inputs:          inputs,
		CreatedAt:       now,
		UpdatedAt:       now,
		QueuedAt:        &now,
		TenantID:        tenantID,
		OwnerID:         ownerID,
		RepoURL:         spec.RepoURL,
		RepoSHA:         spec.RepoRef,
		ProjectPath:     spec.ProjectPath,
		BotID:           spec.BotID,
		BotSourceTenant: botSourceTenantOf(spec.BotBundle),
		KeyOverrides:    spec.KeyOverrides,
		SecretOverrides: spec.SecretOverrides,
		// Cap. 3 sharding fields — propagate to the persisted Run so
		// studio surfaces can render the parent/child relationship,
		// and onto the published RunMessage below so the runner pod
		// that claims this work knows its place in the shard set.
		ParentRunID:        spec.ParentRunID,
		ShardIndex:         spec.ShardIndex,
		ShardCount:         spec.ShardCount,
		ShardLabel:         spec.ShardLabel,
		CallbackURL:        spec.CallbackURL,
		CallbackToken:      spec.CallbackToken,
		CallbackAnswerNode: spec.CallbackAnswerNode,
		// Same display parity a local launch gets from the engine: the
		// studio Overview reads the pins from the run doc, and the resume
		// path replays them onto its RunMessage from here.
		ModelOverrides: runModelOverrides(spec.ModelOverrides),
		// The launch-frozen outcome contract, same replay-from-the-doc
		// doctrine: consumers read it from the run, never from a
		// mutable setting.
		RoutingPolicy: spec.RoutingPolicy,
		// The run-level fallback chain, same doctrine: stamped raw so the
		// resume path replays it, and mirrored onto the RunMessage below
		// so the claiming runner applies it.
		Fallback: runFallbackOf(spec.Fallback),
		// The raw budget ask, same doctrine as the model pins above: the
		// run doc is the single source the resume path replays from. The
		// clamped/effective figure is NOT what is stamped — the resume
		// re-clamps against its own grant.
		BudgetOverrides: runtime.RunBudgetOverridesOf(spec.Budget),
	}
	// Typed provenance (schedule / dispatcher / trigger spine). The queued
	// doc is the ONLY carrier: the RunMessage has no source field, and the
	// runner's engine only stamps run.Source when it was given one, so a
	// value lost here is lost for good — taking the run's source_kind back
	// to "manual" and blinding the overlap gate, which counts a schedule's
	// live runs through source.schedule_id.
	if spec.SourceRef != nil {
		src := *spec.SourceRef // copy: never share the caller's pointer
		r.Source = &src
	}
	// Same "the queued doc is the only carrier" reasoning as Source above:
	// the RunMessage has no retry field, and the launch site is the only
	// place that could see every layer of the policy. A value lost here
	// leaves the runner falling back to defaults for a run whose owner
	// asked for something else.
	if spec.RetryPolicy != nil {
		rp := *spec.RetryPolicy // copy: never share the caller's pointer
		r.RetryPolicy = &rp
	}
	// 1b. Resolve BYOK credentials and seal them under a fresh
	//     secrets_ref. Empty ref means "no team-scoped credentials
	//     configured" — the runner falls back to env. This runs BEFORE the
	//     run record is persisted so a required-secret launch failure leaves
	//     NO run record behind (never a stray queued/running run for a launch
	//     that could not resolve its mandatory credentials).
	orgID := p.orgIDForTeam(ctx, tenantID)
	creds, err := p.resolveAndSealCredentials(ctx, runID, orgID, tenantID, ownerID, spec.BotID, wf, spec.KeyOverrides, spec.SecretOverrides, buildModelOverrides(spec.ModelOverrides), runFallbackEntries(spec.Fallback))
	// A donor's admission is consumed the moment it is granted. Armed BEFORE
	// the error check: resolveAndSealCredentials can fail AFTER acquiring —
	// sealing the bundle, persisting it — and still returns the grant. Every
	// exit below is a launch that will never happen, so it must be returned,
	// or one Mongo write blip spends a contributor's daily quota and holds
	// their concurrency slot for the lease's whole 12h life, on a run that
	// never ran.
	launched := false
	if creds.grant != nil {
		defer func() {
			if !launched {
				p.credPool.Release(ctx, runID)
			}
		}()
	}
	if err != nil {
		return 0, err
	}
	// The sealed credentials' audit identities ride the run document's
	// single SaveRun below (never secrets): the per-key concurrency meter
	// counts alive runs by them. Assigned on the in-memory doc rather
	// than patched afterwards — at this point the document does NOT
	// exist yet (the launch's one persist comes later), and a patch here
	// would be a warn-and-lose no-op that leaves the ceiling blind to
	// every launched run.
	r.CredFingerprints = creds.fingerprints

	// A run served by the pool may not spend more than what remains of its
	// donor's allowance. This is the enforcement: the engine stops the run
	// on its own cost budget, instead of the overspend being discovered
	// after the fact when the donor is already out of pocket.
	budget := clampBudgetToGrant(spec.Budget, wf, creds.grant, 0, p.logger, runID)

	// Resolve the credential-derived topology vars (review_mode +
	// mono_family, plan_review, llm_families) from what actually sealed
	// into the run — the queued-run counterpart of the host-detection
	// injection every in-process launch surface applies. Only when the
	// bundle resolved at least one participating credential: an empty set
	// means the run rides the runner's env fallback, which is unknowable
	// here, and the bots' own "auto" defaults must survive untouched.
	if len(creds.families) > 0 {
		if inputs == nil {
			inputs = map[string]any{}
			r.Inputs = inputs
		}
		if inj := reviewtopology.InjectAll(wf, inputs, creds.families, spec.ReviewMode); inj.Summary() != "" {
			p.logger.Info("cloudpublisher: run %s %s", runID, inj.Summary())
		}
	}

	if err := p.store.SaveRun(ctx, r); err != nil {
		return 0, fmt.Errorf("cloudpublisher: save run: %w", err)
	}

	// 2. Build the RunMessage. We marshal the AST inline; p.publish then
	//    offloads it out-of-band via an IRRef (T-42) if it would exceed
	//    the NATS max_payload. The runner side re-parses + re-compiles, so
	//    the wire payload is the AST File, not the compiled IR.
	body, err := marshalIRFromSpec(spec.FilePath, spec.Source)
	if err != nil {
		return 0, err
	}
	// Plugin/library skills resolved HERE, from this instance's iterion home:
	// the runner pod's is empty, so an operator-installed plugin's skill would
	// otherwise never reach the workspace (see resolveContributions).
	contributions, err := resolveContributionsFor(ctx, wf, "", tenantID, p.pluginSources, p.logger)
	if err != nil {
		return 0, err
	}
	msg := &queue.RunMessage{
		V:             queue.SchemaVersion,
		Contributions: contributions,
		RunID:         runID,
		WorkflowName:  wf.Name,
		WorkflowHash:  hash,
		IRCompiled:    body,
		Vars:          inputs,
		SecretsRef:    creds.secretsRef,
		// The stored-bundle ref THREADED from the launch surface's own
		// resolution (never re-fetched here — a push racing the launch must
		// not pair this compile's IR with newer resources). The runner
		// rebuilds the bundle from the store and verifies the version.
		BotBundle:       queueBotBundleRef(spec.BotBundle),
		SandboxImage:    p.effectiveSandboxImage(ctx),
		AutoMemory:      spec.AutoMemory,
		LoopBudgetGuard: spec.LoopBudgetGuard,
		Supervisors:     spec.Supervisors,
		BackendConfig:   queue.BackendConfig{Default: queue.BackendClaw},
		PublishedAtRFC:  time.Now().UTC().Format(time.RFC3339Nano),
		TenantID:        tenantID,
		OrgID:           orgID,
		OwnerID:         ownerID,
		// Cap. 3 sharding: when this run is a child shard, the runner
		// pod that picks it up sees its place in the set so the studio
		// can group siblings and so a future event-based aggregator
		// can correlate completions.
		ParentRunID:        spec.ParentRunID,
		ShardIndex:         spec.ShardIndex,
		ShardCount:         spec.ShardCount,
		ShardLabel:         spec.ShardLabel,
		CallbackURL:        spec.CallbackURL,
		CallbackToken:      spec.CallbackToken,
		CallbackAnswerNode: spec.CallbackAnswerNode,
		// Repo to clone before sandboxing (webhook-launched runs have no
		// operator checkout). RepoRef carries a branch or sha. ProjectPath
		// is NOT on the wire — the runner clones from RepoURL and the
		// persisted run doc is the authoritative carrier of the slug.
		RepoURL: spec.RepoURL,
		RepoSHA: spec.RepoRef,
		BotID:   spec.BotID,
		Budget:  budget,
		// The operator's model/backend pins must ride the wire: the runner
		// pod builds its own executor, so a pin only persisted on the run
		// doc is display-only — the studio would show an override the
		// delegates never honoured.
		ModelOverrides: queueModelOverrides(spec.ModelOverrides),
		// The run-level fallback chain rides the wire for the same reason:
		// a chain only persisted on the doc is display-only, and this one
		// exists precisely for unattended cloud runs hitting a provider's
		// usage window.
		Fallback: queueFallbackOf(spec.Fallback),
	}
	if err := p.publish(ctx, msg); err != nil {
		pubErr := fmt.Errorf("cloudpublisher: publish: %w", err)
		// Roll the run doc back to failed so the studio surfaces the
		// queue failure rather than a stuck "queued" row that never
		// moves. Roll back on a cancel-immune ctx: a publish failure
		// caused by request cancellation must not also doom the status
		// rollback.
		if uerr := p.store.UpdateRunStatus(context.WithoutCancel(ctx), runID, store.RunStatusFailed, fmt.Sprintf("queue publish: %v", err)); uerr != nil {
			return 0, errors.Join(pubErr, fmt.Errorf("cloudpublisher: mark run %s failed after publish failure (run may be stuck queued): %w", runID, uerr))
		}
		return 0, pubErr
	}
	launched = true
	if p.metrics != nil {
		p.metrics.RunsCreatedTotal.WithLabelValues(string(store.RunStatusQueued)).Inc()
	}

	// 3. Compute queue position: count of runs with status=queued
	//    and created_at <= ours.
	pos, err := p.queuePosition(ctx, runID)
	if err != nil {
		// Non-fatal: the studio falls back to "Waiting on the queue"
		// generic copy when queue_position is zero.
		p.logger.Warn("cloudpublisher: queue position lookup: %v", err)
	}
	return pos, nil
}

// CancelRun flips the Mongo doc to cancelled. Two effects:
//   - the runner's cooperative-cancel check on next pickup acks the
//     JetStream delivery without executing;
//   - if a runner is currently holding the lease, the cancel subject
//     `iterion.cancel.<run_id>` unwinds engine.Run.
//
// Idempotent: running CancelRun on an already-terminal run is a no-op.
func (p *Publisher) CancelRun(ctx context.Context, runID string) error {
	return p.CancelRunWithReason(ctx, runID, "cancelled by user")
}

// CancelRunWithReason is CancelRun with an explicit reason. The reason lands
// in run.Error — which the run list, the board cards and the merge-gate
// synthetic status all read — so an AUTOMATED cancel must say what actually
// happened instead of masquerading as an operator action: a webhook supersede
// once overwrote a runner validation error with "cancelled by user", and the
// PR's synthetic gate then blamed a human who had touched nothing.
//
// The prior error, when present, is carried forward (same rationale as
// runview.CancelInactiveCtx): run.Error is the only record in run.json of WHY
// the run was in its pre-cancel state, and cancelling must not erase it.
func (p *Publisher) CancelRunWithReason(ctx context.Context, runID, reason string) error {
	// Cancel descends here with a NON-request context (runview.Service.Cancel
	// takes no ctx), so the mongo tenant filter has no tenant and its
	// tenant-scoped queries panic. The caller (handleCancelRun / the WS
	// handleCancel) has ALREADY gated access with a tenant-scoped LoadRunCtx,
	// so bypass the filter explicitly here rather than crashing — exactly the
	// seam store.WithoutTenantFilter exists for. Without this, cancelling any
	// cloud run 502s (panic: "tenant-scoped query without tenant in ctx").
	ctx = store.WithoutTenantFilter(ctx)
	r, err := p.store.LoadRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("cloudpublisher: load run %s: %w", runID, err)
	}
	if !r.Status.CanBeCancelled() {
		return nil // already settled
	}
	// CAS on the cancellable statuses. A SubmitResume that races this
	// call can flip queued → running between our LoadRun and the
	// UpdateRunStatus below; without the conditional update we'd
	// silently overwrite the in-flight resume back to cancelled with
	// no visible warning. The expectedFrom set lists every status we
	// consider cancellable here.
	// The cloud publisher owns the queue AND the doc, so its reach is
	// the full canonical set — including paused_operator (once missing
	// here, which made an operator-paused cloud run un-cancellable) and
	// queued (this surface can retract a queued attempt; the engine's
	// narrower CAS cannot).
	var cancellable []store.RunStatus
	for _, st := range store.AllRunStatuses {
		if st.CanBeCancelled() {
			cancellable = append(cancellable, st)
		}
	}
	if strings.TrimSpace(reason) == "" {
		reason = "cancelled"
	}
	if prior := strings.TrimSpace(r.Error); prior != "" {
		reason += " (was " + string(r.Status) + ": " + prior + ")"
	}
	changed, err := p.store.UpdateRunStatusIfCoded(ctx, runID, store.RunStatusCancelled, reason, store.FailureCancelled, cancellable)
	if err != nil {
		return fmt.Errorf("cloudpublisher: flip status: %w", err)
	}
	if !changed {
		// Status raced from under us — re-read and decide if this is
		// an actual no-op (already terminal) or a transient state we
		// should surface for the operator to retry.
		r2, _ := p.store.LoadRun(ctx, runID)
		if r2 != nil {
			if !r2.Status.CanBeCancelled() {
				return nil
			}
			return fmt.Errorf("cloudpublisher: cancel raced (status now %s) — retry", r2.Status)
		}
		return nil
	}
	if err := p.cancel(runID); err != nil {
		p.logger.Warn("cloudpublisher: nats cancel %s: %v", runID, err)
	}
	return nil
}

// SubmitResume republishes a RunMessage with ResumeSpec set. The
// runner picks it up and dispatches to engine.Resume which threads
// the answers in.
//
// A JetStream redelivery of the ORIGINAL launch message is a different
// resume path (the runner converts it in place, keeping the message's
// launch-time budget — the figure clamped against the launch grant):
// only this explicit republication re-clamps the persisted budget ask
// against the CURRENT grant. Two resumes of the same run can therefore
// carry different caps, each honest about its own grant.
//
// On publish failure the run is reverted to its prior resumable status
// so the studio surfaces an actionable error instead of leaving a
// "queued" row that no runner will ever pick up. Mirrors the rollback
// pattern in SubmitLaunch.
func (p *Publisher) SubmitResume(ctx context.Context, spec runview.ResumeSpec, wf *ir.Workflow, hash string) (retErr error) {
	body, err := marshalIRFromSpec(spec.FilePath, spec.Source)
	if err != nil {
		return err
	}
	// Capture the prior status so we can roll back to the right
	// resumable state if publish fails — the user could be resuming
	// from paused_waiting_human, paused_operator, failed_resumable, or
	// cancelled.
	prior, loadErr := p.store.LoadRun(ctx, spec.RunID)
	if loadErr != nil {
		return fmt.Errorf("cloudpublisher: load prior run %s: %w", spec.RunID, loadErr)
	}
	priorStatus := prior.Status
	// The runview layer validates first, but SubmitResume repeats the
	// boundary check because another request may have changed the row
	// since that read.
	//
	// An automatic resume (the retry sweeper) gates on the NARROWER
	// CanAutoResume(): cancelled is excluded there, so a cancel that landed
	// between the sweeper's claim and this republish is refused before the
	// CAS. Operator resumes keep the wider surface (paused /
	// failed_resumable / cancelled).
	if spec.Automatic {
		if !priorStatus.CanAutoResume() {
			return fmt.Errorf("cloudpublisher: run %s is not auto-resumable from status %s (CanAutoResume() excludes it deliberately)", spec.RunID, priorStatus)
		}
	} else if !priorStatus.CanOperatorResume() {
		return fmt.Errorf("cloudpublisher: run %s is not resumable from status %s", spec.RunID, priorStatus)
	}

	// Claim this resume BEFORE resolving credentials. The status CAS is the
	// serialization point for double-clicks/client retries: only one request
	// may create the next queue attempt, so only that request may acquire a
	// credential-pool lease or publish. Transitioning to queued also refreshes
	// QueuedAt, the durable attempt marker used to reject stale deliveries.
	claimed, claimErr := p.store.UpdateRunStatusIf(ctx, spec.RunID, store.RunStatusQueued, "", []store.RunStatus{priorStatus})
	if claimErr != nil {
		return fmt.Errorf("cloudpublisher: claim resume %s: %w", spec.RunID, claimErr)
	}
	if !claimed {
		current, _ := p.store.LoadRun(ctx, spec.RunID)
		if current != nil {
			return fmt.Errorf("cloudpublisher: resume raced for %s (status now %s)", spec.RunID, current.Status)
		}
		return fmt.Errorf("cloudpublisher: resume raced for %s", spec.RunID)
	}

	// Any failure before a successful publish restores the exact resumable
	// source status. The rollback itself is a queued-only CAS, so a concurrent
	// cancel/runner transition is never overwritten.
	republished := false
	defer func() {
		if republished {
			return
		}
		// Restore the prior failure classification alongside its text —
		// the queued claim cleared both. A publish failure gets its own
		// text but keeps the prior code: the run is back in the state
		// whose cause that code classifies.
		runErr := prior.Error
		if retErr != nil {
			runErr = fmt.Sprintf("queue resume: %v", retErr)
		}
		rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer rollbackCancel()
		if _, rbErr := p.store.UpdateRunStatusIfCoded(rollbackCtx, spec.RunID, priorStatus, runErr, prior.FailureCode,
			[]store.RunStatus{store.RunStatusQueued}); rbErr != nil {
			p.logger.Error("cloudpublisher: rollback %s after resume failure: %v", spec.RunID, rbErr)
		}
	}()

	// Keys may have rotated between launch and resume; using the prior run's
	// secrets ref blindly would inject stale plaintext. Preserve BotID so bot-
	// secret bindings remain durable across pause/failure/TTL republishes.
	secretsCtx := store.WithTenant(ctx, prior.TenantID)
	secretsCtx = store.WithOwner(secretsCtx, prior.OwnerID)
	priorOrgID := p.orgIDForTeam(ctx, prior.TenantID)
	creds, secretsErr := p.resolveAndSealCredentials(secretsCtx, spec.RunID, priorOrgID, prior.TenantID, prior.OwnerID, prior.BotID, wf, prior.KeyOverrides, prior.SecretOverrides, buildModelOverridesFromRun(prior.ModelOverrides), runFallbackEntriesFromRun(prior.Fallback))
	// Armed before the error check — see SubmitLaunch.
	if creds.grant != nil {
		defer func() {
			if !republished {
				p.credPool.Release(ctx, spec.RunID)
			}
		}()
	}
	if secretsErr != nil {
		return secretsErr
	}
	// Re-stamp on resume, UNCONDITIONALLY: re-resolution may have picked
	// different credentials — or none at all (key deleted, every
	// candidate refused with nothing to restore, env fallback) — and a
	// stale stamp would hold a slot on a credential the run demonstrably
	// no longer carries, for its whole remaining alive life. An empty
	// re-resolution therefore CLEARS the stamp. Best-effort: a failed
	// write degrades the ceiling toward uncapped, never the resume.
	if serr := p.store.SetRunCredFingerprints(secretsCtx, spec.RunID, creds.fingerprints); serr != nil {
		p.logger.Warn("cloudpublisher: re-stamp cred fingerprints on run %s: %v", spec.RunID, serr)
	}
	// Re-resolved on resume too: the engine re-mirrors skills on every resume,
	// so a resumed run must carry the same payload a fresh launch would.
	contributions, contribErr := resolveContributionsFor(ctx, wf, "", prior.TenantID, p.pluginSources, p.logger)
	if contribErr != nil {
		return contribErr
	}
	// The budget the resumed attempt executes against, computed ONCE: the
	// launch ask replayed from the run doc, this resume's ask merged over
	// it per field (#652 part 2 — the remote CLI's --max-* flags beat the
	// persisted replay, or the documented "raise the cap + resume"
	// recovery is inert), then clamped to the donor's remaining allowance
	// when a credential-pool grant serves the run. It rides the wire below
	// AND stamps the doc's effective-caps snapshot further down — the same
	// figure, so the studio never advertises a cap the run does not have.
	merged := runtime.MergeResumeBudgetAsk(spec.Budget, prior.BudgetOverrides)
	wire := clampBudgetToGrant(merged, wf, creds.grant, checkpointCostUSD(prior), p.logger, spec.RunID)
	msg := &queue.RunMessage{
		V:             queue.SchemaVersion,
		Contributions: contributions,
		RunID:         spec.RunID,
		WorkflowName:  wf.Name,
		WorkflowHash:  hash,
		IRCompiled:    body,
		Resume: &queue.ResumeSpec{
			Answers: spec.Answers,
			Force:   spec.Force,
		},
		SecretsRef: creds.secretsRef,
		// Re-resolved by the resume surface like credentials are re-sealed:
		// the resumed attempt runs the CURRENT stored bundle, consistently
		// across the compile above and the runner-side materialization.
		BotBundle:       queueBotBundleRef(spec.BotBundle),
		SandboxImage:    p.effectiveSandboxImage(ctx),
		AutoMemory:      spec.AutoMemory,
		LoopBudgetGuard: spec.LoopBudgetGuard,
		Supervisors:     spec.Supervisors,
		// A resume re-acquires from the pool, so it re-inherits the donor's
		// CURRENT remaining allowance as its cost ceiling — a run that was
		// paused for a day must not come back holding yesterday's budget.
		// Offset by what the run already banked: the engine restores that
		// figure into the very tracker this ceiling is checked against.
		// The launch's own budget ask is replayed from the run doc (same
		// doctrine as ModelOverrides below): cloud resumes are often
		// unattended auto-retries, so nothing else can re-state it, and a
		// dropped override silently reverts the run to the workflow's cap.
		// A THIS-RESUME override is also persisted to the run doc below so
		// a subsequent auto-retry keeps the raised cap rather than
		// reverting to the launch ask that already killed the run.
		Budget:         wire,
		BackendConfig:  queue.BackendConfig{Default: queue.BackendClaw},
		PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano),
		// A resumed attempt must honour the SAME pins the launch declared —
		// replayed from the run doc, the single source the launch stamped.
		ModelOverrides: queueOverridesFromRun(prior.ModelOverrides),
		// The fallback chain is replayed from the doc for the same reason:
		// the auto-retry that follows a usage-window park is exactly the
		// publication that must still carry the rescue chain.
		Fallback: queueFallbackFromRun(prior.Fallback),
		// Carry the prior run's tenant onto the resume publication so
		// the runner re-acquires the lease in the right scope. We trust
		// the loaded prior doc rather than ctx: a super-admin resuming
		// from another team's UI must still target that team's tenant —
		// and the resumed attempt's spend must charge that team's org,
		// not the resumer's.
		TenantID: prior.TenantID,
		OrgID:    priorOrgID,
		OwnerID:  prior.OwnerID,
		// Preserve webhook/cloud source metadata so a resumed runner can
		// reconstruct the same workspace as the original launch. ProjectPath
		// is carried by the persisted run doc, not the wire.
		RepoURL: prior.RepoURL,
		RepoSHA: prior.RepoSHA,
		BotID:   prior.BotID,
	}
	if err := p.publish(ctx, msg); err != nil {
		return fmt.Errorf("cloudpublisher: republish: %w", err)
	}
	republished = true
	if p.metrics != nil {
		p.metrics.RunsCreatedTotal.WithLabelValues("resumed").Inc()
	}
	// E4 (#652 review round 1): persist the MERGED budget ask onto the
	// run doc, so a subsequent unattended auto-retry (usage-window
	// sweeper) keeps the raised cap rather than reverting to the
	// launch ask that already killed the run. Only when the operator
	// asked for a change on THIS resume — an ask-less resume leaves the
	// ASK untouched, and the ask is persisted UN-CLAMPED on purpose: the
	// donor allowance is re-derived against the current grant on every
	// resume, so a run capped at $5 today correctly replays its $120 ask
	// once the donor recovers. Granular SetRunBudgetOverrides so the CAS
	// status transition above (queued) stays intact. Best-effort.
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer persistCancel()
	if spec.Budget != nil && !spec.Budget.IsZero() {
		if err := p.store.SetRunBudgetOverrides(persistCtx, spec.RunID, runtime.RunBudgetOverridesOf(merged)); err != nil && p.logger != nil {
			p.logger.Warn("cloudpublisher: persist merged budget ask for %s after resume: %v", spec.RunID, err)
		}
	}
	// The doc's effective-caps snapshot (Run.Budget, the studio
	// Overview's denominator) is stamped on EVERY resume that puts a
	// budget on the wire — ask or no ask — from the figure the wire
	// carries (merged AND clamped to the donor's allowance), never from
	// the ask: the allowance moves between resumes in both directions
	// (an ask-less auto-retry can find a $5 donor after a $500 one, or
	// the reverse), and the engine stamps this field only at launch
	// (post-clamp, so the field means "enforced"). Enforcement is
	// correct on the wire in every case; without this stamp it is the
	// doc that lies — the studio reading $120 while the pod dies at the
	// donor's $5 with CapImposed denying the exit grace, or $5 while the
	// run may spend $120. A nil wire means nothing changed: the runner's
	// post-ceiling launch stamp stands. What this stamp cannot see is the
	// PLATFORM ceiling (ITERION_CLOUD_MAX_*): it lives in the runner's
	// environment, not the publisher's, and a cap raised above it is
	// clamped on the pod and logged there — the doc over-reports by that
	// margin until the engine re-stamps on resume (ticketed).
	if wire != nil {
		if snap := runtime.EffectiveBudgetSnapshot(wf, runtime.BudgetOverridesFromWire(wire)); snap != nil {
			if err := p.store.SetRunBudgetSnapshot(persistCtx, spec.RunID, snap); err != nil && p.logger != nil {
				p.logger.Warn("cloudpublisher: refresh budget snapshot for %s after resume: %v", spec.RunID, err)
			}
		}
	}
	// Disarm any usage-window retry the retry sweeper had armed for this
	// run: an operator (or the auto-retry) is resuming it NOW, and a
	// surviving retry_after re-fires the sweeper days later on a run that
	// finished. Observed live (#669 part 3): a manual resume left the pre-park
	// retry_state in place, and the 09-08 21:07 sweep would have claimed a
	// completed run. Best-effort — a store blip here does not cancel the
	// resume: the sweep's ClaimRunRetry CAS still checks the run status
	// (which is now queued/running), so the worst case is one spurious
	// abandon audit line.
	//
	// E5 (#652 review round 1): bound the detached-context write like
	// every other sibling detached store call (publisher.go:1553's
	// rollback uses 10s, usage_retry follows the same convention) so a
	// wedged store cannot pin the resume goroutine indefinitely.
	if retryStore := store.AsRunRetryStore(p.store); retryStore != nil {
		clearCtx, clearCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if err := retryStore.ClearRunRetry(clearCtx, spec.RunID); err != nil && p.logger != nil {
			p.logger.Warn("cloudpublisher: clear armed retry for %s after manual resume: %v", spec.RunID, err)
		}
		clearCancel()
	}
	return nil
}

func (p *Publisher) publish(ctx context.Context, msg *queue.RunMessage) error {
	if err := p.offloadOversizedIR(ctx, msg); err != nil {
		return err
	}
	return p.publishWithRetry(ctx, msg)
}

const publishRetryWindow = 10 * time.Second

var defaultPublishRetryDelays = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	750 * time.Millisecond,
}

// publishWithRetry is the single retry choke point for cloud launches and
// resumes. RunMessage's Nats-Msg-Id stays stable across attempts, so a publish
// whose acknowledgement was lost is absorbed by JetStream deduplication.
func (p *Publisher) publishWithRetry(ctx context.Context, msg *queue.RunMessage) error {
	retryCtx, cancel := context.WithTimeout(ctx, publishRetryWindow)
	defer cancel()

	delays := p.publishRetryDelays
	if delays == nil {
		delays = defaultPublishRetryDelays
	}
	attempts := len(delays) + 1
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// PublishRun owns its 5s acknowledgement deadline. Do not impose a
		// shorter outer attempt timeout: cancelling only the ack wait after the
		// broker accepted the message can turn a successful publish into retries
		// and finally a false QUEUE_UNAVAILABLE response.
		lastErr = p.publishOnce(retryCtx, msg)
		if lastErr == nil {
			return nil
		}
		// A caller cancellation is not a queue outage and must retain its
		// cancellation semantics. PublishRun's own deadline, on the other
		// hand, is a transient NATS timeout and is retried below.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !natsq.IsTransientPublishError(lastErr) {
			return lastErr
		}
		if attempt == attempts || retryCtx.Err() != nil {
			break
		}
		delay := delays[attempt-1]
		if p.logger != nil {
			p.logger.Warn("cloudpublisher: queue publish attempt %d/%d failed transiently: %v — retrying in %s", attempt, attempts, lastErr, delay)
		}
		if delay <= 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-retryCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return &runview.QueueUnavailableError{Cause: lastErr}
		}
	}
	return &runview.QueueUnavailableError{Cause: lastErr}
}

func (p *Publisher) publishOnce(ctx context.Context, msg *queue.RunMessage) error {
	if p.publishRun != nil {
		return p.publishRun(ctx, msg)
	}
	if p.nats == nil {
		return fmt.Errorf("cloudpublisher: NATS publisher is not configured")
	}
	_, err := p.nats.PublishRun(ctx, msg)
	return err
}

// irEnvelopeReserve is the byte headroom kept for the RunMessage's
// non-IR fields (ids, vars, trace, backend, …) when deciding whether the
// inline IR still fits under max_payload. IRCompiled is embedded verbatim
// (json.RawMessage), so the marshaled envelope is ~len(IRCompiled) plus
// these fields; the reserve lets the common (small-IR) path skip the
// precise marshal below.
const irEnvelopeReserve = 64 * 1024

// offloadOversizedIR swaps the inline compiled IR for a lightweight IRRef
// when the marshaled RunMessage would exceed the NATS max_payload. It
// stashes the IR in the store's out-of-band blob backend (S3, via the
// IRBlobStore seam) keyed by run id and points the message at it. This is
// the T-42 fallback that keeps a workflow whose compiled IR is larger than
// the payload budget (default 1 MiB) dispatchable instead of hard-failing
// at enqueue.
//
// No-op when: NATS is not wired (unit tests stub publishRun), the message
// already carries an IRRef, there is no inline IR, or the IR comfortably
// fits. Fails loudly — rather than silently truncating — when the IR is
// oversized but the store cannot host an IR blob.
func (p *Publisher) offloadOversizedIR(ctx context.Context, msg *queue.RunMessage) error {
	if p.maxPayload == nil || msg.IRRef != nil || len(msg.IRCompiled) == 0 {
		return nil
	}
	maxPayload := p.maxPayload()
	if maxPayload <= 0 {
		return nil
	}
	// Cheap gate: if the IR plus the envelope reserve is under the limit,
	// it fits — skip the precise (re-)marshal on the hot path.
	if int64(len(msg.IRCompiled))+irEnvelopeReserve <= maxPayload {
		return nil
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("cloudpublisher: marshal RunMessage for run %s: %w", msg.RunID, err)
	}
	if int64(len(body)) <= maxPayload {
		return nil
	}

	blobs := store.AsIRBlobStore(p.store)
	if blobs == nil {
		return fmt.Errorf("cloudpublisher: RunMessage for run %s is %d bytes (exceeds NATS max_payload %d) but the store cannot host an out-of-band IR blob (IRRef fallback unavailable)", msg.RunID, len(body), maxPayload)
	}
	backend, err := irBackendForName(blobs.IRBlobBackend())
	if err != nil {
		return err
	}
	key, err := blobs.PutIRBlob(ctx, msg.RunID, msg.IRCompiled)
	if err != nil {
		return fmt.Errorf("cloudpublisher: stash oversized IR for run %s: %w", msg.RunID, err)
	}
	msg.IRCompiled = nil
	msg.IRRef = &queue.IRRef{StorageKey: key, Backend: backend}
	p.logger.Info("cloudpublisher: run %s compiled IR (%d bytes) exceeds NATS max_payload %d — offloaded to %s:%s", msg.RunID, len(body), maxPayload, backend, key)
	return nil
}

// irBackendForName maps an IRBlobStore backend name to the wire enum the
// runner validates, rejecting anything the queue contract doesn't accept.
func irBackendForName(name string) (queue.IRBackend, error) {
	switch queue.IRBackend(name) {
	case queue.IRBackendS3:
		return queue.IRBackendS3, nil
	case queue.IRBackendMongo:
		return queue.IRBackendMongo, nil
	default:
		return "", fmt.Errorf("cloudpublisher: IR blob store reported unsupported backend %q (want s3|mongo)", name)
	}
}

func (p *Publisher) cancel(runID string) error {
	if p.cancelRun != nil {
		return p.cancelRun(runID)
	}
	if p.nats == nil {
		return fmt.Errorf("cloudpublisher: NATS publisher is not configured")
	}
	return p.nats.CancelRun(runID)
}

// queuePosition counts the runs with status=queued and created_at
// less than or equal to ours. The result is 1-based, matching the
// "1st in queue" copy the studio renders.
func (p *Publisher) queuePosition(ctx context.Context, runID string) (int, error) {
	if p.runs == nil {
		return 0, nil
	}
	var doc struct {
		CreatedAt time.Time `bson:"created_at"`
	}
	if err := p.runs.FindOne(ctx, bson.M{"_id": runID}).Decode(&doc); err != nil {
		return 0, err
	}
	count, err := p.runs.CountDocuments(ctx, bson.M{
		"status":     store.RunStatusQueued,
		"created_at": bson.M{"$lte": doc.CreatedAt},
	})
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

// marshalIRFromSpec returns the AST.File bytes for the workflow.
// Resolution order: inline `source` (preferred in cloud mode where
// the studio SPA uploads source verbatim and the server pod has no
// shared filesystem) → `path` on local disk (fallback for tests and
// migration tooling). The runner re-parses + re-compiles, so the
// wire payload is the AST File, not the compiled IR.
func marshalIRFromSpec(path, source string) (json.RawMessage, error) {
	var src string
	parserPath := path
	switch {
	case source != "":
		src = source
		if parserPath == "" {
			parserPath = "<inline>"
		}
	case path != "":
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("cloudpublisher: read %s: %w", path, err)
		}
		src = string(body)
	default:
		return nil, fmt.Errorf("cloudpublisher: launch spec has no source and no file_path; cannot serialise IR")
	}
	pr := parser.Parse(parserPath, src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			return nil, fmt.Errorf("cloudpublisher: parse %s: %s", parserPath, d.Error())
		}
	}
	if pr.File == nil {
		return nil, fmt.Errorf("cloudpublisher: empty AST for %s", parserPath)
	}
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		return nil, fmt.Errorf("cloudpublisher: marshal IR: %w", err)
	}
	return body, nil
}

// Drain waits for any in-flight fire-and-forget goroutines (MarkUsed
// writes, etc.) to complete or the supplied ctx to fire — whichever
// comes first. Returns the ctx error on timeout so the server's
// graceful-shutdown path can log how many writes were lost.
//
// Safe to call multiple times; concurrent calls all observe the same
// WaitGroup.
func (p *Publisher) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.detached.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// goSafeDetached runs fn in a tracked, panic-recovering goroutine: it
// increments the detached WaitGroup so Drain still waits for it, and
// contains any panic in the best-effort body (a MarkUsed write into a
// driver) so it can't crash the process from a goroutine the caller
// can't recover. label identifies the task in the recovery log.
func (p *Publisher) goSafeDetached(label string, fn func()) {
	p.detached.Add(1)
	go func() {
		defer p.detached.Done()
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			// A no-op unless SENTRY_DSN is set; the recovery semantics
			// are unchanged either way.
			errtrack.CapturePanicFields(r, map[string]any{"task": label, "surface": "cloudpublisher.goSafeDetached"})
			if p.logger != nil {
				p.logger.Error("cloudpublisher: detached task %q panicked: %v", label, r)
			}
		}()
		fn()
	}()
}

// checkpointCostUSD is what a run has already banked across its earlier
// attempts, 0 for a run that never checkpointed.
func checkpointCostUSD(r *store.Run) float64 {
	if r == nil || r.Checkpoint == nil {
		return 0
	}
	return r.Checkpoint.BudgetCostUSD
}

// clampBudgetToGrant caps a run's cost budget at what remains of its
// donor's allowance, and returns the wire form.
//
// This is what makes a pledge an actual ceiling rather than a hope: the
// engine enforces max_cost_usd as the run proceeds, so a run that would
// exhaust the donation stops itself. The post-hoc ledger charge is the
// final truth, but it arrives too late to protect anyone.
//
// A grant with RemainingUSD == 0 means the donor set NO spend cap, not
// "nothing left" (an exhausted donor is never selected in the first
// place) — so it imposes no ceiling. Otherwise the tightest of the
// requested override, the workflow's own declared budget, and the
// allowance wins; the clamp only ever lowers.
//
// alreadySpentUSD is what THIS run banked in its earlier attempts, and it
// must be added on a resume: the engine restores the checkpoint's
// cumulative cost into the same tracker it checks max_cost_usd against
// (runtime/checkpoint.go), whereas a grant is what the donor will lend
// NEXT. Handing the marginal figure to a cumulative tracker fails the
// budget check before the first node runs — a $6-of-$10 run coming back
// with a $4 ceiling against $6 already counted, dead on arrival.
func clampBudgetToGrant(o *ir.BudgetOverrides, wf *ir.Workflow, grant *credpool.Grant, alreadySpentUSD float64, logger *iterlog.Logger, runID string) *queue.BudgetOverrides {
	if grant == nil || grant.RemainingUSD <= 0 {
		return budgetForWire(o)
	}
	allowance := grant.RemainingUSD
	if alreadySpentUSD > 0 {
		allowance += alreadySpentUSD
	}
	effective := ir.BudgetOverrides{}
	if o != nil {
		effective = *o
	}
	// What the run would have spent up to, absent the pool: a launch
	// override replaces the workflow's own figure (ApplyBudgetOverrides
	// semantics), and zero means unlimited.
	resolved := ir.Budget{MaxCostUSD: effective.MaxCostUSD}
	if resolved.MaxCostUSD <= 0 && wf != nil && wf.Budget != nil {
		resolved.MaxCostUSD = wf.Budget.MaxCostUSD
	}
	// The donor's allowance is a ceiling like any other, so it goes through
	// the same primitive the platform ceiling uses: lower what exceeds it,
	// impose it on a run that declared nothing, never raise.
	resolved.ClampToCeiling(&ir.Budget{MaxCostUSD: allowance})
	if effective.MaxCostUSD != resolved.MaxCostUSD && logger != nil {
		logger.Info("cloudpublisher: run %s cost budget capped at $%.2f by its donor's remaining allowance", runID, resolved.MaxCostUSD)
	}
	effective.MaxCostUSD = resolved.MaxCostUSD
	// The donor's allowance is an ABSOLUTE promise: the exit grace must
	// not spend past it, so the imposed-cap marker travels with the
	// override (ir.Budget.CapImposed on the runner side).
	effective.CapImposed = effective.CapImposed || resolved.CapImposed
	return budgetForWire(&effective)
}

// budgetForWire converts launch-time budget overrides to their queue wire
// mirror. Nil (or all-zero) overrides publish as nil so old payload diffs
// stay byte-identical and the runner's nil-check stays meaningful.
func budgetForWire(o *ir.BudgetOverrides) *queue.BudgetOverrides {
	if o == nil || o.IsZero() {
		return nil
	}
	return &queue.BudgetOverrides{
		MaxCostUSD:          o.MaxCostUSD,
		MaxTokens:           o.MaxTokens,
		MaxDuration:         o.MaxDuration,
		MaxIterations:       o.MaxIterations,
		MaxParallelBranches: o.MaxParallelBranches,
		CapImposed:          o.CapImposed,
	}
}

// varsAsAny upgrades a string-keyed map to interface{} so the wire
// payload can carry richer types if the launch spec ever evolves.
func varsAsAny(in map[string]string) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// queueModelOverrides converts the launch entries into the wire form the
// claiming runner applies to its executor. Nil in, nil out — an absent
// field keeps older messages byte-identical.
func queueModelOverrides(entries []runview.ModelOverrideEntry) []queue.ModelOverride {
	if len(entries) == 0 {
		return nil
	}
	out := make([]queue.ModelOverride, 0, len(entries))
	for _, e := range entries {
		out = append(out, queue.ModelOverride{Selector: e.Selector, Backend: e.Backend, Model: e.Model, Provider: e.Provider})
	}
	return out
}

// runModelOverrides converts the launch entries into the persisted run-doc
// form (studio display + the resume path's replay source).
func runModelOverrides(entries []runview.ModelOverrideEntry) []store.RunModelOverride {
	if len(entries) == 0 {
		return nil
	}
	out := make([]store.RunModelOverride, 0, len(entries))
	for _, e := range entries {
		out = append(out, store.RunModelOverride{Selector: e.Selector, Backend: e.Backend, Model: e.Model, Provider: e.Provider})
	}
	return out
}

// runFallbackOf converts the launch's run-level fallback chain into the
// persisted run-doc form (the resume path's replay source). Targetless
// stages are omitted so override-less launches stay byte-identical.
func runFallbackOf(entries []runview.FallbackEntry) store.RunFallback {
	var out store.RunFallback
	for _, e := range entries {
		if e.Backend == "" && e.Model == "" && e.Provider == "" {
			continue
		}
		out = append(out, store.RunFallbackEntry{
			Backend: e.Backend, Model: e.Model, Provider: e.Provider,
		})
	}
	return out
}

// queueFallbackOf puts the launch's run-level fallback chain on the wire.
func queueFallbackOf(entries []runview.FallbackEntry) queue.RunFallback {
	var out queue.RunFallback
	for _, e := range entries {
		if e.Backend == "" && e.Model == "" && e.Provider == "" {
			continue
		}
		out = append(out, queue.RunFallbackEntry{
			Backend: e.Backend, Model: e.Model, Provider: e.Provider,
		})
	}
	return out
}

// queueFallbackFromRun replays a run doc's persisted fallback chain onto
// a resume publication.
func queueFallbackFromRun(entries store.RunFallback) queue.RunFallback {
	if len(entries) == 0 {
		return nil
	}
	out := make(queue.RunFallback, 0, len(entries))
	for _, f := range entries {
		out = append(out, queue.RunFallbackEntry{
			Backend: f.Backend, Model: f.Model, Provider: f.Provider,
		})
	}
	return out
}

// queueOverridesFromRun replays a run doc's persisted pins onto a resume
// publication.
func queueOverridesFromRun(entries []store.RunModelOverride) []queue.ModelOverride {
	if len(entries) == 0 {
		return nil
	}
	out := make([]queue.ModelOverride, 0, len(entries))
	for _, e := range entries {
		out = append(out, queue.ModelOverride{Selector: e.Selector, Backend: e.Backend, Model: e.Model, Provider: e.Provider})
	}
	return out
}
