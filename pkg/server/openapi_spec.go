package server

import (
	"fmt"
	"os"
	"strings"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/credusage"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/pat"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/usagecap"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// BuildOpenAPISpec constructs the COMPLETE OpenAPI document offline, with no
// network or database. It wires a Server with in-memory stubs for every
// optional dependency so routes() registers the full cloud surface, then runs
// the generator. Handlers are never invoked — the stubs exist only to satisfy
// the non-nil gates in routes(), which register routes deref-free.
//
// This is the single source of truth for the published spec and the generated
// client: `iterion openapi` prints it and CI regenerates+diffs the committed
// artifacts from it, so the client can never drift from the code.
func BuildOpenAPISpec() (map[string]any, error) {
	tmp, err := os.MkdirTemp("", "iterion-openapi-spec-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	runStore, err := store.New(tmp)
	if err != nil {
		return nil, fmt.Errorf("temp run store: %w", err)
	}

	// A REAL auth service over a memory identity store, not a bare
	// &auth.Service{}: the team-scoped families (team OAuth-forfait, …)
	// register only when authStore() resolves, and a nil store silently
	// left every one of them out of the published spec and the generated
	// client. Nothing here is ever invoked; the store only has to exist.
	signer, err := auth.NewJWTSigner(strings.Repeat("0", 43), 15*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("spec signer: %w", err)
	}
	authSvc, err := auth.NewService(auth.Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("spec auth service: %w", err)
	}

	cfg := Config{
		StoreDir:                tmp,
		WorkDir:                 tmp,
		SkipProjectRegistration: true,
		DisableAuth:             true,

		// Gate fields — non-nil stubs so every register*() fires. None are
		// invoked here (registration only calls s.mux.Handle).
		AuthService:        authSvc,
		Sealer:             specNoopSealer{},
		ApiKeys:            secrets.NewMemoryApiKeyStore(),
		GenericSecrets:     secrets.NewMemoryGenericSecretStore(),
		BotBindings:        secrets.NewMemoryBotSecretBindingStore(),
		OAuthForfait:       secrets.NewMemoryOAuthStore(),
		OAuthPending:       secrets.NewMemoryOAuthPendingStore(),
		WebhookConfigs:     webhooks.NewMemoryConfigStore(),
		WebhookDeliveries:  webhooks.NewMemoryDeliveryStore(),
		WebhookCounter:     webhooks.NewMemoryCounter(),
		ForgeConnections:   forge.NewMemoryConnectionStore(),
		ForgeIntegrations:  forge.NewMemoryRepoIntegrationStore(),
		ProvisionApprovals: forge.NewMemoryProvisionApprovalStore(),
		ForgeOAuthApps:     forge.NewMemoryOAuthAppStore(),
		OrgSSO:             orgsso.NewMemoryStore(),
		OrgDomains:         orgsso.NewMemoryDomainStore(),
		PATs:               pat.NewMemoryStore(),
		TriggerStore:       trigger.NewMemorySubscriptionStore(),
		OrgUsage:           orgusage.NewMemoryCounter(),
		CredUsage:          credusage.NewMemoryCounter(),
		CredPoolPools:      credpool.NewMemoryPoolStore(),
		CredPoolPledges:    credpool.NewMemoryPledgeStore(),
		CredPoolLeases:     credpool.NewMemoryLeaseStore(),
		CredPoolLedger:     credpool.NewMemoryLedger(),
		Audit:              audit.NewMemoryStore(),
		UsageCapSettings:   usagecap.NewMemorySettingsStore(),
		UsageCaps:          usagecap.NewMemStore(),
		BotSources:         botsource.NewMemoryStore(),
		BotRolesSettings:   platformcfg.NewMemoryStore[platformcfg.BotRoles](),
		SandboxSettings:    platformcfg.NewMemoryStore[platformcfg.Sandbox](),
		BotVarsSettings:    platformcfg.NewMemoryStore[platformcfg.BotVars](),
		Store:              runStore,
	}

	s := New(cfg, iterlog.New(iterlog.LevelError, nil))
	doc := s.buildOpenAPI()
	// Pin the version to a stable literal: the committed openapi.json (and the
	// generated client) must be invariant across commits/builds so the CI
	// drift-guard compares only real API changes, not the injected build SHA.
	// The LIVE spec (`iterion remote openapi`) keeps the real build version.
	if info, ok := doc["info"].(map[string]any); ok {
		info["version"] = "current"
	}
	return doc, nil
}

// specNoopSealer is a non-functional secrets.Sealer used only to satisfy the
// routes() gate during offline spec generation. It is never called.
type specNoopSealer struct{}

func (specNoopSealer) Seal(plaintext, aad []byte) ([]byte, error) { return plaintext, nil }
func (specNoopSealer) Open(sealed, aad []byte) ([]byte, error)    { return sealed, nil }
