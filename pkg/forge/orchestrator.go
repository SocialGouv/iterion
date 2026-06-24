package forge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// BotForgeLookup returns a bot's declared forge requirements (its manifest
// forge: block). A nil result with a nil error means the bot exists but
// declares no forge: block — it cannot be auto-provisioned. A non-nil error
// means the bot could not be resolved. The server wires this to
// botregistry; tests pass a closure.
type BotForgeLookup func(botID string) (*bundle.ForgeRequirements, error)

// BotInvocationsLookup returns a bot's manifest invocations (the typed
// routing contract — bundle.EffectiveInvocations). Used by Provision to build
// the webhook CommandMap. An empty slice (or a nil lookup) leaves the command
// index empty. The server wires this to botregistry; tests pass a closure.
type BotInvocationsLookup func(botID string) ([]bundle.Invocation, error)

// Orchestrator turns "enable bot(s) X on repo Y of connection C" into the
// concrete trio — an iterion webhooks.Config, a forge-side hook, and a
// per-webhook secret override pinning the connection's managed forge token
// — recorded as one RepoIntegration. Idempotent and reversible.
type Orchestrator struct {
	Connections  ConnectionStore
	Integrations RepoIntegrationStore
	Webhooks     webhooks.ConfigStore
	Secrets      secrets.GenericSecretStore
	Sealer       secrets.Sealer
	Bots         BotForgeLookup
	// Invocations returns a bot's manifest invocations so Provision can build
	// the webhook CommandMap. Optional: nil leaves CommandMap empty (the
	// GitLab /revi special-case still works; other commands just aren't
	// provisioned). Wired to botregistry by the server; closures in tests.
	Invocations BotInvocationsLookup

	// Schedules, when set (cloud mode), persists a ScheduledBot row per
	// schedule invocation of the enabled bots so the cloud scheduler fires
	// them. nil = no scheduling (self-hosted uses `iterion schedule`).
	Schedules cloudsched.Store
	// AdminFor builds the outbound client for a connection (opens its sealed
	// token). Injected so the orchestrator stays provider-agnostic and
	// testable with a fake admin.
	AdminFor  func(ctx context.Context, conn Connection) (Admin, error)
	PublicURL string

	// Optional injection points for tests (default to time.Now / uuid).
	Now   func() time.Time
	NewID func() string
}

func (o *Orchestrator) clock() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}

func (o *Orchestrator) id() string {
	if o.NewID != nil {
		return o.NewID()
	}
	return uuid.NewString()
}

// ProvisionRequest enables a set of bots on one repo of one connection.
type ProvisionRequest struct {
	TenantID     string
	ConnectionID string
	RepoFullName string // "owner/repo" (GitLab: namespace/project path)
	BotIDs       []string
	ActorID      string // operator who triggered it (audit / created_by)
	// ScheduleCrons overrides a bot's schedule cron (botID → 5-field cron) for
	// the operator's chosen cadence; falls back to the manifest suggested_cron.
	ScheduleCrons map[string]string
}

// ProvisionResult reports what the orchestrator created or reused.
type ProvisionResult struct {
	IntegrationID   string   `json:"integration_id"`
	WebhookID       string   `json:"webhook_id"`
	HookID          string   `json:"hook_id"`
	ManagedSecretID string   `json:"managed_secret_id"`
	BotIDs          []string `json:"bot_ids"`
	// Created is false when the call was a fully idempotent no-op (the repo
	// already had exactly these bots + events enabled).
	Created bool `json:"created"`
}

// Provision is the one-action enable flow. See the package doc for the
// separation of concerns. Requires the ctx to carry the tenant for the
// Mongo secret store (the provisioning route wraps it); the Memory store
// used in tests does not need it.
func (o *Orchestrator) Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
	if req.TenantID == "" || req.ConnectionID == "" || strings.TrimSpace(req.RepoFullName) == "" {
		return ProvisionResult{}, fmt.Errorf("forge: provision requires tenant, connection and repo")
	}
	if len(req.BotIDs) == 0 {
		return ProvisionResult{}, fmt.Errorf("forge: provision requires at least one bot")
	}

	conn, err := o.Connections.Get(ctx, req.ConnectionID)
	if err != nil {
		return ProvisionResult{}, err
	}
	if conn.TenantID != req.TenantID {
		return ProvisionResult{}, ErrConnectionNotFound // cross-tenant — do not leak existence
	}

	existing, exErr := o.Integrations.GetByConnRepo(ctx, req.TenantID, conn.ID, req.RepoFullName)
	hasExisting := exErr == nil
	if exErr != nil && !errors.Is(exErr, ErrIntegrationNotFound) {
		return ProvisionResult{}, exErr
	}

	desiredBots := dedupSorted(req.BotIDs)
	if hasExisting {
		desiredBots = dedupSorted(append(append([]string{}, existing.BotIDs...), req.BotIDs...))
	}

	// Resolve every bot's forge requirements (its optional forge: block for
	// credentials/scopes) AND its invocations (the typed routing contract).
	// A bot is auto-provisionable when it has a forge: block OR a
	// forge/command invocation — the events to subscribe come from BOTH
	// sources, so a command-only bot (no forge: block) is enable-able and
	// its webhook subscribes to the comment event.
	frByBot := make(map[string]*bundle.ForgeRequirements, len(desiredBots))
	invByBot := make(map[string][]bundle.Invocation, len(desiredBots))
	for _, b := range desiredBots {
		fr, err := o.Bots(b)
		if err != nil {
			return ProvisionResult{}, fmt.Errorf("forge: resolve bot %q: %w", b, err)
		}
		var invs []bundle.Invocation
		if o.Invocations != nil {
			if invs, err = o.Invocations(b); err != nil {
				return ProvisionResult{}, fmt.Errorf("forge: resolve invocations for %q: %w", b, err)
			}
		}
		if fr == nil && !hasForgeReachableInvocation(invs) {
			return ProvisionResult{}, fmt.Errorf("forge: bot %q declares neither a forge: block nor a forge/command invocation; cannot auto-provision", b)
		}
		frByBot[b] = fr
		invByBot[b] = invs
	}
	eventsNormalized := unionAllEvents(desiredBots, frByBot, invByBot)
	nativeEvents := ToNativeEvents(conn.Provider, eventsNormalized)
	if len(nativeEvents) == 0 {
		return ProvisionResult{}, fmt.Errorf("forge: bots %v declare no forge events to subscribe to", desiredBots)
	}

	// Idempotent no-op: same bots + same events already provisioned.
	if hasExisting && equalStringSet(existing.BotIDs, desiredBots) && equalStringSet(existing.EventsNormalized, eventsNormalized) {
		return ProvisionResult{
			IntegrationID:   existing.ID,
			WebhookID:       existing.WebhookID,
			HookID:          existing.HookID,
			ManagedSecretID: existing.ManagedSecretID,
			BotIDs:          existing.BotIDs,
			Created:         false,
		}, nil
	}

	managedSecretID, err := o.ensureManagedSecret(ctx, &conn, req.ActorID)
	if err != nil {
		return ProvisionResult{}, err
	}

	// Per-webhook secret override pinning the connection's managed forge
	// token under each bot's declared workflow-secret name (Tier-0 in
	// ResolveGenericWithBindings — wins over any org binding, and avoids the
	// (tenant,bot,name) binding unique-constraint when the same bot runs on
	// several connections).
	secretOverrides := map[string]string{}
	launchVars := map[string]string{}
	minRole := ""
	authorSet := map[string]string{} // lower-key → canonical entry (dedup, order-stable below)
	var authorAllowlist []string
	authorOpen := false // a forge-webhook bot left AuthorAllowlist empty → allow all
	for _, b := range desiredBots {
		fr := frByBot[b]
		// A command-only bot (no forge: block) still binds the connection's
		// managed token under the default forge_token name so it can post
		// back if it wants to.
		secretName := bundle.DefaultForgeSecretName
		if fr != nil {
			secretName = fr.SecretName()
			if fr.Webhook != nil {
				for k, v := range fr.Webhook.LaunchVars {
					launchVars[k] = v
				}
				if fr.Webhook.MinReplierRole != "" && webhookRoleRank(fr.Webhook.MinReplierRole) > webhookRoleRank(minRole) {
					minRole = fr.Webhook.MinReplierRole
				}
				// Union the per-bot author allowlists (dedup case-insensitively,
				// first-seen order). Restricting authors is a per-bot opt-in:
				// if ANY co-enabled forge-webhook bot leaves it empty, the
				// shared webhook must stay open to all authors (else that bot's
				// human PRs would be silently dropped) — see authorOpen below.
				if len(fr.Webhook.AuthorAllowlist) == 0 {
					authorOpen = true
				}
				for _, a := range fr.Webhook.AuthorAllowlist {
					if key := strings.ToLower(strings.TrimSpace(a)); key != "" {
						if _, seen := authorSet[key]; !seen {
							authorSet[key] = a
							authorAllowlist = append(authorAllowlist, a)
						}
					}
				}
			}
		}
		secretOverrides[secretName] = managedSecretID
	}

	if authorOpen {
		authorAllowlist = nil // a co-enabled bot reviews all authors → don't filter
	}

	// Build the command→bot route index from the co-enabled bots' command
	// invocations. Rejects an un-disambiguated cross-bot command collision.
	commandMap, err := o.buildCommandMap(desiredBots)
	if err != nil {
		return ProvisionResult{}, err
	}

	// Mint a fresh iwh_ on every mutating provision (create OR event-widen):
	// it keeps the forge hook secret and the iterion config hash in lockstep
	// without ever needing the prior plaintext. The operator never sees it —
	// iterion holds both ends.
	plaintext, hash, last4, fingerprint, err := webhooks.MintToken()
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("forge: mint webhook token: %w", err)
	}

	webhookID := o.id()
	if hasExisting && existing.WebhookID != "" {
		webhookID = existing.WebhookID
	}

	now := o.clock()
	cfg := webhooks.Config{
		ID:               webhookID,
		TenantID:         req.TenantID,
		Name:             provisionedWebhookName(conn.Provider, req.RepoFullName),
		Provider:         webhooks.Provider(conn.Provider),
		SignMode:         signModeFor(conn.Provider),
		Enabled:          true,
		TokenHash:        hash,
		TokenLast4:       last4,
		Fingerprint:      fingerprint,
		BotIDs:           desiredBots,
		DefaultBotID:     singleBotDefault(desiredBots),
		ProjectAllowlist: []string{req.RepoFullName},
		EventAllowlist:   nativeEvents,
		AuthorAllowlist:  authorAllowlist,
		ForgeBaseURL:     conn.BaseURL(),
		RateLimit:        webhooks.Rate{Rate: 1, Burst: 10},
		LaunchVars:       nilIfEmpty(launchVars),
		SecretOverrides:  secretOverrides,
		MinReplierRole:   minRole,
		CommandMap:       commandMap,
		ProvisionedBy:    "forge:" + conn.ID,
		CreatedBy:        req.ActorID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if cfg.SignMode == webhooks.SignModeHMAC {
		sealed, err := webhooks.SealHMACSecret(o.Sealer, cfg.ID, plaintext)
		if err != nil {
			return ProvisionResult{}, fmt.Errorf("forge: seal webhook hmac secret: %w", err)
		}
		cfg.HMACSecretSealed = sealed
	}

	createdConfig := false
	if hasExisting && existing.WebhookID != "" {
		if old, gerr := o.Webhooks.Get(ctx, webhookID); gerr == nil {
			cfg.CreatedAt = old.CreatedAt
			cfg.CreatedBy = old.CreatedBy
		}
		cfg.RotatedAt = &now
		if err := o.Webhooks.Update(ctx, cfg); err != nil {
			return ProvisionResult{}, fmt.Errorf("forge: update webhook config: %w", err)
		}
	} else {
		if err := o.Webhooks.Create(ctx, cfg); err != nil {
			return ProvisionResult{}, fmt.Errorf("forge: create webhook config: %w", err)
		}
		createdConfig = true
	}

	// Register / update the forge-side hook. On any failure during a fresh
	// provision, roll the just-created config back so we don't strand an
	// orphan inbound endpoint.
	admin, err := o.AdminFor(ctx, conn)
	if err != nil {
		o.rollbackConfig(ctx, createdConfig, webhookID)
		return ProvisionResult{}, fmt.Errorf("forge: build admin client: %w", err)
	}
	hookURL := o.inboundURL(conn.Provider, webhookID)
	spec := HookSpec{URL: hookURL, Secret: plaintext, Events: nativeEvents, Active: true}

	// existing is the zero RepoIntegration when !hasExisting, so its HookID
	// is "" — upsertHook treats that as "no prior hook".
	hookID, err := o.upsertHook(ctx, admin, req.RepoFullName, existing.HookID, hookURL, spec)
	if err != nil {
		o.rollbackConfig(ctx, createdConfig, webhookID)
		return ProvisionResult{}, err
	}

	ri := RepoIntegration{
		TenantID:         req.TenantID,
		ConnectionID:     conn.ID,
		Provider:         conn.Provider,
		RepoFullName:     req.RepoFullName,
		BotIDs:           desiredBots,
		EventsNormalized: eventsNormalized,
		WebhookID:        webhookID,
		HookID:           hookID,
		HookURL:          hookURL,
		ManagedSecretID:  managedSecretID,
		CreatedBy:        req.ActorID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if hasExisting {
		ri.ID = existing.ID
		ri.CreatedAt = existing.CreatedAt
		ri.CreatedBy = existing.CreatedBy
		if err := o.Integrations.Update(ctx, ri); err != nil {
			return ProvisionResult{}, fmt.Errorf("forge: update integration: %w", err)
		}
	} else {
		ri.ID = o.id()
		if err := o.Integrations.Create(ctx, ri); err != nil {
			o.rollbackConfig(ctx, createdConfig, webhookID)
			return ProvisionResult{}, fmt.Errorf("forge: record integration: %w", err)
		}
	}

	// Materialise cloud schedules for the enabled bots' schedule invocations.
	if err := o.syncSchedules(ctx, req.TenantID, ri.ID, invByBot, req.ScheduleCrons, req.ActorID); err != nil {
		return ProvisionResult{}, err
	}

	return ProvisionResult{
		IntegrationID:   ri.ID,
		WebhookID:       webhookID,
		HookID:          hookID,
		ManagedSecretID: managedSecretID,
		BotIDs:          desiredBots,
		Created:         !hasExisting,
	}, nil
}

// upsertHook reuses an existing iterion hook (by stored id or by probing
// the delivery URL) or creates a fresh one, always pushing the current
// spec (events + secret).
func (o *Orchestrator) upsertHook(ctx context.Context, admin Admin, repo, priorID, hookURL string, spec HookSpec) (string, error) {
	if priorID != "" {
		h, err := admin.UpdateHook(ctx, repo, priorID, spec)
		if err == nil {
			return h.ID, nil
		}
		if !errors.Is(err, ErrHookNotFound) {
			return "", fmt.Errorf("forge: update hook: %w", err)
		}
		// fall through: the stored hook is gone on the forge — recreate it.
	}
	if found, err := admin.GetHook(ctx, repo, hookURL); err == nil && found != nil {
		h, err := admin.UpdateHook(ctx, repo, found.ID, spec)
		if err != nil {
			return "", fmt.Errorf("forge: update hook: %w", err)
		}
		return h.ID, nil
	}
	h, err := admin.CreateHook(ctx, repo, spec)
	if err != nil {
		return "", fmt.Errorf("forge: create hook: %w", err)
	}
	return h.ID, nil
}

func (o *Orchestrator) rollbackConfig(ctx context.Context, created bool, webhookID string) {
	if created && webhookID != "" {
		_ = o.Webhooks.Delete(ctx, webhookID)
	}
}

// ensureManagedSecret creates (once per connection) the team-scoped generic
// secret holding the connection's admin token as the bot-runtime forge
// token, stamping its id onto the connection. Reused across every repo/bot
// of the connection; the refresh worker rewrites its plaintext on rotation.
func (o *Orchestrator) ensureManagedSecret(ctx context.Context, conn *Connection, actor string) (string, error) {
	if conn.ManagedSecretID != "" {
		return conn.ManagedSecretID, nil
	}
	sec, err := openConnectionSecret(o.Sealer, conn.ID, conn.SealedPayload)
	if err != nil {
		return "", err
	}
	token := sec.AdminToken()
	if token == "" {
		return "", fmt.Errorf("forge: connection %s holds no usable token", conn.ID)
	}
	secID := secrets.NewGenericSecretID()
	sealed, err := secrets.SealGenericSecret(o.Sealer, secID, []byte(token))
	if err != nil {
		return "", fmt.Errorf("forge: seal managed secret: %w", err)
	}
	now := o.clock()
	gs := secrets.GenericSecret{
		ID:           secID,
		TenantID:     conn.TenantID,
		ScopeTeamID:  conn.TenantID,
		Name:         managedSecretName(conn),
		Last4:        secrets.Last4(token),
		Fingerprint:  secrets.FingerprintSHA256(token),
		SealedSecret: sealed,
		CreatedBy:    actor,
		CreatedAt:    now,
	}
	if err := o.Secrets.Create(ctx, gs); err != nil {
		return "", fmt.Errorf("forge: create managed secret: %w", err)
	}
	conn.ManagedSecretID = secID
	conn.UpdatedAt = now
	if err := o.Connections.Update(ctx, *conn); err != nil {
		return "", fmt.Errorf("forge: stamp managed secret on connection: %w", err)
	}
	return secID, nil
}

// Deprovision removes one repo integration: the forge hook (best-effort —
// a 404 is success), the iterion webhook config, and the join row. The
// connection's managed secret survives (it is connection-level, shared
// across that connection's other repos).
func (o *Orchestrator) Deprovision(ctx context.Context, tenantID, integrationID string) error {
	ri, err := o.Integrations.Get(ctx, integrationID)
	if err != nil {
		return err
	}
	if ri.TenantID != tenantID {
		return ErrIntegrationNotFound
	}
	if ri.HookID != "" {
		if conn, cerr := o.Connections.Get(ctx, ri.ConnectionID); cerr == nil {
			if admin, aerr := o.AdminFor(ctx, conn); aerr == nil {
				if derr := admin.DeleteHook(ctx, ri.RepoFullName, ri.HookID); derr != nil && !errors.Is(derr, ErrHookNotFound) {
					return fmt.Errorf("forge: delete forge hook: %w", derr)
				}
			}
		}
	}
	if ri.WebhookID != "" {
		if derr := o.Webhooks.Delete(ctx, ri.WebhookID); derr != nil && !errors.Is(derr, webhooks.ErrNotFound) {
			return fmt.Errorf("forge: delete webhook config: %w", derr)
		}
	}
	if o.Schedules != nil {
		if err := o.Schedules.DeleteByIntegration(ctx, ri.TenantID, ri.ID); err != nil {
			return fmt.Errorf("forge: delete schedules: %w", err)
		}
	}
	return o.Integrations.Delete(ctx, ri.ID)
}

// syncSchedules replaces the integration's ScheduledBot rows with one per
// schedule invocation (with a suggested cron) of the enabled bots, so the
// cloud scheduler fires them. Clean-slate (delete-then-create) keeps it
// idempotent across re-provisions. No-op when no schedule store is wired.
func (o *Orchestrator) syncSchedules(ctx context.Context, tenantID, integrationID string, invByBot map[string][]bundle.Invocation, crons map[string]string, actor string) error {
	if o.Schedules == nil {
		return nil
	}
	if err := o.Schedules.DeleteByIntegration(ctx, tenantID, integrationID); err != nil {
		return fmt.Errorf("forge: clear schedules: %w", err)
	}
	now := o.clock()
	for _, bot := range dedupSorted(keysOfInvByBot(invByBot)) {
		for _, inv := range invByBot[bot] {
			if inv.Kind != bundle.InvocationKindSchedule || inv.Schedule == nil {
				continue
			}
			// Operator's chosen cron overrides the manifest suggested_cron.
			cron := strings.TrimSpace(inv.Schedule.SuggestedCron)
			if ov := strings.TrimSpace(crons[bot]); ov != "" {
				cron = ov
			}
			if cron == "" {
				continue // no cron (no suggestion, no override) → can't fire
			}
			next, err := cloudsched.NextFire(cron, now)
			if err != nil {
				return fmt.Errorf("forge: schedule for %q: %w", bot, err)
			}
			if err := o.Schedules.Create(ctx, cloudsched.ScheduledBot{
				ID:                o.id(),
				TenantID:          tenantID,
				RepoIntegrationID: integrationID,
				BotID:             bot,
				Cron:              cron,
				Vars:              inv.Schedule.DefaultVars,
				NextFireAt:        next,
				CreatedBy:         actor,
				CreatedAt:         now,
				UpdatedAt:         now,
			}); err != nil {
				return fmt.Errorf("forge: create schedule for %q: %w", bot, err)
			}
		}
	}
	return nil
}

func keysOfInvByBot(m map[string][]bundle.Invocation) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// DeprovisionConnection tears down every integration for a connection, then
// deletes the connection's managed secret and the connection itself.
func (o *Orchestrator) DeprovisionConnection(ctx context.Context, tenantID, connID string) error {
	conn, err := o.Connections.Get(ctx, connID)
	if err != nil {
		return err
	}
	if conn.TenantID != tenantID {
		return ErrConnectionNotFound
	}
	items, err := o.Integrations.ListByConnection(ctx, tenantID, connID)
	if err != nil {
		return err
	}
	for _, ri := range items {
		if derr := o.Deprovision(ctx, tenantID, ri.ID); derr != nil {
			return derr
		}
	}
	if conn.ManagedSecretID != "" {
		if derr := o.Secrets.Delete(ctx, conn.ManagedSecretID); derr != nil && !errors.Is(derr, secrets.ErrGenericSecretNotFound) {
			return fmt.Errorf("forge: delete managed secret: %w", derr)
		}
	}
	return o.Connections.Delete(ctx, connID)
}

func (o *Orchestrator) inboundURL(p Provider, webhookID string) string {
	return strings.TrimRight(o.PublicURL, "/") + "/api/webhooks/" + string(p) + "/" + webhookID
}

// ---- small helpers ----

func signModeFor(p Provider) webhooks.SignatureMode {
	switch p {
	case ProviderGitHub, ProviderForgejo:
		return webhooks.SignModeHMAC
	default: // gitlab uses the secret-token header
		return webhooks.SignModeToken
	}
}

func provisionedWebhookName(p Provider, repo string) string {
	return string(p) + ":" + repo
}

func managedSecretName(conn *Connection) string {
	short := strings.ReplaceAll(conn.ID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return "forge_" + string(conn.Provider) + "_" + short
}

func singleBotDefault(bots []string) string {
	if len(bots) == 1 {
		return bots[0]
	}
	return ""
}

// hasForgeReachableInvocation reports whether a bot's invocations include a
// forge-webhook-reachable trigger (a forge event or a slash-command), i.e.
// something that needs an inbound webhook subscription. board-only and
// schedule-only invocations do not make a bot auto-provisionable onto a repo
// webhook by themselves.
func hasForgeReachableInvocation(invs []bundle.Invocation) bool {
	for _, inv := range invs {
		if inv.Kind == bundle.InvocationKindForge || inv.Kind == bundle.InvocationKindCommand {
			return true
		}
	}
	return false
}

// unionAllEvents computes the normalized forge events the webhook must
// subscribe to across all enabled bots, unioning each bot's forge: block
// events with its invocation-implied events (a forge invocation contributes
// its event; a command invocation contributes the comment event, since
// commands arrive in PR/issue comments). Sorted + deduped for a stable hash.
func unionAllEvents(bots []string, frByBot map[string]*bundle.ForgeRequirements, invByBot map[string][]bundle.Invocation) []string {
	set := map[string]bool{}
	for _, b := range bots {
		if fr := frByBot[b]; fr != nil {
			for _, e := range fr.Events {
				set[e] = true
			}
		}
		for _, inv := range invByBot[b] {
			switch inv.Kind {
			case bundle.InvocationKindForge:
				if inv.Forge != nil {
					set[inv.Forge.Event] = true
				}
			case bundle.InvocationKindCommand:
				set[bundle.ForgeEventPullRequestComment] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// buildCommandMap flattens the co-enabled bots' command invocations into the
// webhook CommandMap (command name + each alias → routes). Two different bots
// may share a command name only when they disambiguate by complementary args
// states (the review-pr vs revi-converse pattern); any other collision is a
// provision error. Returns nil when no bot declares a command invocation (or
// the Invocations lookup isn't wired), leaving Config.CommandMap unset.
func (o *Orchestrator) buildCommandMap(bots []string) (map[string][]webhooks.CommandRoute, error) {
	if o.Invocations == nil {
		return nil, nil
	}
	out := map[string][]webhooks.CommandRoute{}
	for _, b := range bots {
		invs, err := o.Invocations(b)
		if err != nil {
			return nil, fmt.Errorf("forge: resolve invocations for %q: %w", b, err)
		}
		for _, inv := range invs {
			if inv.Kind != bundle.InvocationKindCommand || inv.Command == nil {
				continue
			}
			route := webhooks.CommandRoute{
				BotID:          b,
				Mode:           string(inv.EffectiveMode()),
				ArgsVar:        inv.ArgsVar,
				ContextVars:    inv.ContextVars,
				Scope:          inv.Command.Scope,
				MinReplierRole: inv.Command.MinReplierRole,
				Disambiguator:  inv.Command.Disambiguator,
				OpensMR:        inv.Command.OpensMR,
			}
			for _, name := range append([]string{inv.Command.Name}, inv.Command.Aliases...) {
				if err := addCommandRoute(out, strings.ToLower(strings.TrimSpace(name)), route); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// addCommandRoute appends a route under key, enforcing the collision policy:
// the same bot re-declaring an alias is a no-op; a different bot on the same
// key is allowed only when it and the incumbent disambiguate by complementary
// args states.
func addCommandRoute(m map[string][]webhooks.CommandRoute, key string, route webhooks.CommandRoute) error {
	if key == "" {
		return nil
	}
	for _, e := range m[key] {
		if e.BotID == route.BotID {
			return nil // same bot, alias overlap — keep the first
		}
		if !complementaryArgs(e.Disambiguator, route.Disambiguator) {
			return fmt.Errorf("forge: bots %q and %q both claim command /%s without args disambiguation", e.BotID, route.BotID, key)
		}
	}
	m[key] = append(m[key], route)
	return nil
}

// complementaryArgs reports whether two command disambiguators split the
// command cleanly by args presence (one when_args_empty, one when_args_present).
func complementaryArgs(a, b string) bool {
	return (a == "when_args_empty" && b == "when_args_present") ||
		(a == "when_args_present" && b == "when_args_empty")
}

// webhookRoleRank mirrors gitlab role precedence so UnionScopes-style merges
// keep the MOST restrictive declared min-replier-role.
func webhookRoleRank(role string) int {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "owner":
		return 5
	case "maintainer":
		return 4
	case "developer":
		return 3
	case "reporter":
		return 2
	case "guest":
		return 1
	}
	return 0
}

func dedupSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func equalStringSet(a, b []string) bool {
	as, bs := dedupSorted(a), dedupSorted(b)
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}
