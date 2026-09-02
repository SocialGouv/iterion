package forge

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
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
	// Bindings, when set, gets a bot-secret binding per enabled bot pinning
	// the managed forge token under the bot's workflow-secret name (forge_token).
	// The webhook secret override (Tier-0) only reaches the DIRECT webhook launch
	// path; a run launched from a promoted board card goes through the board
	// coordinator's runs.Launch, which resolves generic secrets by (tenant, bot)
	// binding — so without this binding an issue-labeled → Featurly → PR run
	// has no forge_token and can't open its PR. nil (older wiring / self-hosted)
	// = no binding, direct-webhook path unaffected.
	Bindings secrets.BotSecretBindingStore
	Bots     BotForgeLookup
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
	AdminFor func(ctx context.Context, conn Connection) (Admin, error)
	// GitHubAppMinter, when set, mints a fresh least-privilege installation
	// token (scoped to the connection's provisioned repos + minimal
	// permissions) for a github_app connection's managed forge token. Called
	// after each repo provision to narrow the runtime token to the exact repo
	// set. Best-effort: on error the previously-stored (already minimal-
	// permission) token is kept, so a mint failure never blocks a provision.
	// nil (oauth/pat, or no github app configured) → no-op. Injected by the
	// server so the orchestrator stays free of the github package + App key.
	GitHubAppMinter func(ctx context.Context, conn Connection) (string, error)
	// LogWarn, when set, reports a non-blocking anomaly (the only current one:
	// a security-read withdrawal that could not run while disconnecting).
	// Injected like the other seams so the package stays logger-free; nil
	// silences it, which is why callers that CAN act on the error get it
	// returned instead.
	LogWarn   func(format string, args ...any)
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

// ensureBotBinding upserts a (tenant, bot, secretName) → secretID binding so
// board-coordinator launches resolve the managed forge token by bot. Idempotent
// on the (tenant, bot, name) triple: an existing binding for the same name is
// re-pointed at secretID (a re-provision may rotate the managed secret), a new
// one is created. No-op when the store isn't wired.
func (o *Orchestrator) ensureBotBinding(ctx context.Context, tenantID, botID, secretName, secretID string) error {
	if o.Bindings == nil {
		return nil
	}
	existing, err := o.Bindings.ListByTenantBot(ctx, tenantID, botID)
	if err != nil {
		return err
	}
	for _, b := range existing {
		if b.SecretNameForWorkflow == secretName {
			if b.SecretID == secretID {
				return nil
			}
			b.SecretID = secretID
			b.UpdatedAt = o.clock()
			return o.Bindings.Update(ctx, b)
		}
	}
	now := o.clock()
	return o.Bindings.Create(ctx, secrets.BotSecretBinding{
		ID:                    o.id(),
		TenantID:              tenantID,
		BotID:                 botID,
		SecretID:              secretID,
		SecretNameForWorkflow: secretName,
		CreatedAt:             now,
		UpdatedAt:             now,
	})
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
	// LaunchVars are operator overrides stamped onto every run this repo's
	// bots launch, layered after their manifest vars. Persisted on the
	// integration and re-applied on every Provision (see
	// RepoIntegration.LaunchVars). Nil leaves the stored ones untouched.
	LaunchVars map[string]string
	// HoldLabels is the operator's per-repo automation pause. Nil means "keep
	// what the repo already has" — the same rule as LaunchVars.
	HoldLabels []string
	// LabelAllowlist narrows which freshly-applied issue label dispatches the
	// implementer. Nil means "keep what the repo already has"; a non-nil empty
	// slice widens it back to any label — the same rule as HoldLabels.
	LabelAllowlist []string

	// AutoFix opts the repo into the zero-touch lane (a red merge gate launches
	// the repo's fixer). A nil pointer means "leave the repo's current choice
	// alone" — enabling one more bot must not silently switch automation on or
	// off, which a bare bool could not express.
	AutoFix *bool

	// Overlap is the operator's concurrency policy for this repo's webhook
	// (pkg/schedgate vocabulary). Empty leaves the stored one untouched.
	Overlap string
	// Replace makes BotIDs the EXACT desired set instead of a union with the
	// existing integration's bots — the per-bot unbind path. The webhook
	// events, command map and schedules reconcile down accordingly. Removing
	// the last bot is not expressible here (BotIDs must stay non-empty);
	// full teardown is Deprovision.
	Replace bool
}

// ProvisionResult reports what the orchestrator created or reused.
type ProvisionResult struct {
	IntegrationID   string   `json:"integration_id"`
	WebhookID       string   `json:"webhook_id"`
	HookID          string   `json:"hook_id"`
	ManagedSecretID string   `json:"-"`
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
	if hasExisting && !req.Replace {
		desiredBots = dedupSorted(append(append([]string{}, existing.BotIDs...), req.BotIDs...))
	}

	// The provisioned config is where these settings are ENFORCED, and it can
	// legitimately hold a value the integration does not: hold labels and the
	// label allowlist were webhook-config PATCHes before they lived on the
	// integration, and that is still what the webhook API accepts. Read it once
	// — the adoption backfills below and the idempotent path's divergence check
	// both need it.
	var prevCfg webhooks.Config
	hasPrevCfg := false
	if hasExisting && existing.WebhookID != "" {
		prev, gerr := o.Webhooks.Get(ctx, existing.WebhookID)
		switch {
		case gerr == nil:
			prevCfg, hasPrevCfg = prev, true
		case errors.Is(gerr, webhooks.ErrNotFound):
			// The integration points at a config that is gone; the rebuild
			// below recreates it, and there is nothing to adopt.
		default:
			// Any other failure is transient or unknown, and carrying on would
			// rebuild the config from the manifests WITHOUT the operator
			// settings it still carries — losing the repo's narrowing silently
			// and fail-OPEN, which is the very defect this path exists to
			// prevent. Refuse rather than guess.
			return ProvisionResult{}, fmt.Errorf("forge: read webhook %s (the repo's operator settings live there): %w", existing.WebhookID, gerr)
		}
	}

	// Operator overrides survive a re-provision: Provision rewrites the whole
	// webhook config from the manifests, so anything PATCHed onto the webhook
	// is lost at the next enable. A nil request map means "leave them alone",
	// not "clear them" — enabling one more bot must not silently drop the
	// repo's own settings.
	// Each of the four resolves the same way — request, then the integration,
	// then the config that still enforces the repo's choice. The last step is
	// what keeps a setting made the documented way (a webhook PATCH) from
	// being rebuilt away, and it is required of EVERY field the write block
	// below stamps: one that skipped it would be re-written from an empty
	// integration and dropped the moment any other field changes.
	operatorVars := req.LaunchVars
	if operatorVars == nil && hasExisting {
		operatorVars = existing.LaunchVars
		if len(operatorVars) == 0 && hasPrevCfg {
			operatorVars = prevCfg.OperatorLaunchVars
		}
	}
	operatorOverlap := req.Overlap
	if operatorOverlap == "" && hasExisting {
		operatorOverlap = existing.Overlap
		if operatorOverlap == "" && hasPrevCfg {
			operatorOverlap = prevCfg.Overlap
		}
	}
	operatorHold := req.HoldLabels
	if operatorHold == nil && hasExisting {
		operatorHold = existing.HoldLabels
		// Backfill: hold labels were settable only on the webhook config before
		// they lived here, and that is still what the webhook API PATCHes. A
		// provision that read only the integration would wipe a pause an
		// operator had set the documented way — so adopt it instead.
		if len(operatorHold) == 0 && hasPrevCfg {
			operatorHold = prevCfg.HoldLabels
		}
	}
	operatorLabels := req.LabelAllowlist
	if operatorLabels == nil && hasExisting {
		operatorLabels = existing.LabelAllowlist
		// Same backfill as the pause above: narrowing the issue lane was a
		// webhook-config PATCH before it lived here, and that is still the
		// documented gesture. Dropping it on re-provision fails OPEN — the repo
		// silently returns to "any label dispatches the implementer".
		if len(operatorLabels) == 0 && hasPrevCfg {
			operatorLabels = prevCfg.LabelAllowlist
		}
	}
	operatorAutoFix := hasExisting && existing.AutoFixOnGateFailure
	if req.AutoFix != nil {
		operatorAutoFix = *req.AutoFix
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

	// Idempotent no-op: same bots + same events already provisioned. Still
	// reconcile the per-bot token bindings before returning — an integration
	// provisioned before the binding fix landed has none, so the board-launch
	// path can't authenticate until a re-provision backfills them. Cheap and
	// idempotent (ensureBotBinding no-ops when the binding already matches).
	if hasExisting && equalStringSet(existing.BotIDs, desiredBots) && equalStringSet(existing.EventsNormalized, eventsNormalized) {
		for _, b := range desiredBots {
			if err := o.ensureBotBinding(ctx, req.TenantID, b, frByBot[b].SecretName(), existing.ManagedSecretID); err != nil {
				return ProvisionResult{}, fmt.Errorf("forge: bind %s for bot %s: %w", frByBot[b].SecretName(), b, err)
			}
		}
		// A changed operator override is a real mutation even when the bot set
		// is not — without this the short-circuit would silently ignore it.
		// Compared against BOTH stores, which is only sound because every field
		// resolved above ends at the config when the integration says nothing:
		// a silent request therefore already equals the config value and
		// signals no difference to erase. What the config half catches is the
		// EXPLICIT request that happens to match an empty integration while the
		// config still enforces the old value — `label_allowlist: []` sent to
		// widen a lane narrowed the documented way. Without it the call returns
		// 200 having written nothing, and the operator reads an open lane back
		// from an API whose enforcement surface still filters.
		changed := !maps.Equal(operatorVars, existing.LaunchVars) || operatorOverlap != existing.Overlap ||
			operatorAutoFix != existing.AutoFixOnGateFailure || !slices.Equal(operatorHold, existing.HoldLabels) ||
			!slices.Equal(operatorLabels, existing.LabelAllowlist)
		if hasPrevCfg {
			changed = changed || !slices.Equal(operatorLabels, prevCfg.LabelAllowlist) ||
				!slices.Equal(operatorHold, prevCfg.HoldLabels) ||
				operatorOverlap != prevCfg.Overlap ||
				!maps.Equal(operatorVars, prevCfg.OperatorLaunchVars)
		}
		if changed {
			// The config is the enforcement half, so it cannot be skipped:
			// writing only the integration would leave the repo running the
			// previous settings while the API reports the new ones.
			if !hasPrevCfg {
				return ProvisionResult{}, fmt.Errorf("forge: integration %s claims webhook %q, which does not exist — cannot apply the repo's operator settings", existing.ID, existing.WebhookID)
			}
			existing.LaunchVars = operatorVars
			existing.Overlap = operatorOverlap
			existing.HoldLabels = operatorHold
			existing.LabelAllowlist = operatorLabels
			existing.AutoFixOnGateFailure = operatorAutoFix
			existing.UpdatedAt = o.clock()
			if uerr := o.Integrations.Update(ctx, existing); uerr != nil {
				return ProvisionResult{}, fmt.Errorf("forge: update integration launch vars: %w", uerr)
			}
			cfg := prevCfg
			cfg.LaunchVars = nilIfEmpty(manifestLaunchVars(desiredBots, frByBot))
			cfg.OperatorLaunchVars = nilIfEmpty(maps.Clone(operatorVars))
			cfg.Overlap = operatorOverlap
			cfg.HoldLabels = operatorHold
			cfg.LabelAllowlist = operatorLabels
			cfg.UpdatedAt = o.clock()
			if uerr := o.Webhooks.Update(ctx, cfg); uerr != nil {
				return ProvisionResult{}, fmt.Errorf("forge: update webhook operator settings: %w", uerr)
			}
		}
		// Backfill the per-bot routing table onto a config provisioned before
		// BotRules existed. Without this the short-circuit is exactly what
		// keeps an already-provisioned repo on the legacy single-bot path
		// forever: re-enabling the same bots is a no-op, so the rules would
		// never appear in production even though every test passes. Config
		// write only — no token mint, no forge hook call.
		if err := o.backfillBotRules(ctx, existing.WebhookID, desiredBots, frByBot, invByBot); err != nil {
			return ProvisionResult{}, err
		}
		// An explicit cron override is a schedule mutation even when the
		// bot/event set is unchanged — without this, re-enabling with a new
		// cadence would be silently ignored by the idempotent short-circuit.
		if len(req.ScheduleCrons) > 0 {
			if err := o.syncSchedules(ctx, req.TenantID, existing.ID, CloneURLFor(conn.BaseURL(), req.RepoFullName), invByBot, req.ScheduleCrons, req.ActorID); err != nil {
				return ProvisionResult{}, err
			}
		}
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

		// Also bind the managed token under the bot's workflow-secret name so a
		// board-coordinator launch (issue-labeled → Featurly/Billy) — which
		// resolves by (tenant, bot) binding, not the webhook override — can
		// authenticate its push/PR. A binding failure is surfaced (not masked):
		// without it those runs silently can't open their PR, exactly the
		// failure this closes.
		if err := o.ensureBotBinding(ctx, req.TenantID, b, secretName, managedSecretID); err != nil {
			return ProvisionResult{}, fmt.Errorf("forge: bind %s for bot %s: %w", secretName, b, err)
		}
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

	botRules := resolveBotRules(desiredBots, frByBot, invByBot)

	// The derivation only owns UNPINNED syncs: an operator's explicit
	// review_on_sync set (ReviewOnSyncPinned, stamped by the webhook API) is
	// never silently replaced — in either direction (Rf2f99f).
	reviewOnSync := anyBotGatesMerges(desiredBots, frByBot) && !operatorGateDisabled(operatorVars)
	reviewOnSyncPinned := hasPrevCfg && prevCfg.ReviewOnSyncPinned
	if reviewOnSyncPinned {
		reviewOnSync = prevCfg.ReviewOnSync
	}
	// Same release-visibility rule as the backfill: a full provision that
	// rebuilds the config without the sync a previous one carried is the
	// definitive moment of a repo-wide posture change.
	if hasPrevCfg && prevCfg.ReviewOnSync && !reviewOnSync && o.LogWarn != nil {
		o.LogWarn("forge: webhook %s (%s): rebuilt without review_on_sync — pushes no longer auto-review; re-review is on-demand", prevCfg.ID, req.RepoFullName)
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
		ID:                 webhookID,
		TenantID:           req.TenantID,
		Name:               provisionedWebhookName(conn.Provider, req.RepoFullName),
		Provider:           webhooks.Provider(conn.Provider),
		SignMode:           signModeFor(conn.Provider),
		Enabled:            true,
		TokenHash:          hash,
		TokenLast4:         last4,
		Fingerprint:        fingerprint,
		BotIDs:             desiredBots,
		DefaultBotID:       singleBotDefault(desiredBots),
		BotRules:           botRules,
		ProjectAllowlist:   []string{req.RepoFullName},
		EventAllowlist:     nativeEvents,
		AuthorAllowlist:    authorAllowlist,
		LabelAllowlist:     operatorLabels,
		ReviewOnSync:       reviewOnSync,
		ReviewOnSyncPinned: reviewOnSyncPinned,
		ForgeBaseURL:       conn.BaseURL(),
		// The burst must absorb a full review fan-out: one submitted review
		// fires one pull_request_review_comment delivery PER inline comment,
		// near-simultaneously, and the bucket is charged BEFORE the handler
		// can filter the echoes — overflow answers 429, which GitHub never
		// redelivers and counts toward auto-disabling the hook.
		RateLimit:          webhooks.Rate{Rate: 2, Burst: 60},
		LaunchVars:         nilIfEmpty(launchVars),
		OperatorLaunchVars: nilIfEmpty(maps.Clone(operatorVars)),
		Overlap:            operatorOverlap,
		HoldLabels:         operatorHold,
		SecretOverrides:    secretOverrides,
		MinReplierRole:     minRole,
		CommandMap:         commandMap,
		ProvisionedBy:      "forge:" + conn.ID,
		CreatedBy:          req.ActorID,
		CreatedAt:          now,
		UpdatedAt:          now,
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
		if hasPrevCfg {
			cfg.CreatedAt = prevCfg.CreatedAt
			cfg.CreatedBy = prevCfg.CreatedBy
			carryOperatorWebhookSettings(&cfg, prevCfg)
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
		TenantID:             req.TenantID,
		ConnectionID:         conn.ID,
		Provider:             conn.Provider,
		RepoFullName:         req.RepoFullName,
		BotIDs:               desiredBots,
		EventsNormalized:     eventsNormalized,
		WebhookID:            webhookID,
		HookID:               hookID,
		HookURL:              hookURL,
		ManagedSecretID:      managedSecretID,
		LaunchVars:           nilIfEmpty(maps.Clone(operatorVars)),
		Overlap:              operatorOverlap,
		HoldLabels:           operatorHold,
		LabelAllowlist:       operatorLabels,
		AutoFixOnGateFailure: operatorAutoFix,
		CreatedBy:            req.ActorID,
		CreatedAt:            now,
		UpdatedAt:            now,
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

	// github_app: narrow the runtime forge token to the now-current provisioned
	// repo set (least-privilege). Best-effort — see narrowGitHubAppSecret; runs
	// after Integrations.Create so the just-provisioned repo is in the set.
	o.narrowGitHubAppSecret(ctx, &conn)

	// Materialise cloud schedules for the enabled bots' schedule invocations.
	// The repo clone URL rides along so a stateful scheduled bot gets a git
	// workspace (#219) — without it the run lands in the pod's bare WorkDir.
	if err := o.syncSchedules(ctx, req.TenantID, ri.ID, CloneURLFor(conn.BaseURL(), req.RepoFullName), invByBot, req.ScheduleCrons, req.ActorID); err != nil {
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

// forgeTokenEgressHosts returns the egress host allowlist a connection's
// managed forge token is pinned to — the connection's forge host. The
// secret guard's parent-domain match (host.go: "github.com" also permits
// "api.github.com"/"codeload.github.com") means this single host covers the
// API + git + upload subdomains a forge token legitimately needs, while
// blocking exfiltration to any off-forge host. Without it a prompt-injected
// bot holding the token could ship it to an arbitrary host. Empty (→ nil,
// unrestricted) only if the connection resolves no host, which should not
// happen.
func forgeTokenEgressHosts(conn *Connection) []string {
	h := hostOf(conn.BaseURL())
	if h == "" {
		return nil
	}
	return []string{h}
}

// narrowGitHubAppSecret best-effort re-mints a github_app connection's managed
// forge token scoped to its now-current provisioned repo set + minimal
// permissions (least-privilege), rewriting the managed secret's plaintext in
// place (AllowedHosts and every other field preserved). It runs after each
// provision so the runtime token tracks exactly the repos iterion operates on,
// rather than the whole installation until the refresh worker next rotates it.
// A nil minter (oauth/pat, or no github app configured) or any error is a
// no-op: the previously-stored token — already pinned to minimal permissions —
// stays, so narrowing never blocks a provision.
func (o *Orchestrator) narrowGitHubAppSecret(ctx context.Context, conn *Connection) {
	if conn.Kind != KindGitHubApp || o.GitHubAppMinter == nil || conn.ManagedSecretID == "" {
		return
	}
	token, err := o.GitHubAppMinter(ctx, *conn)
	if err != nil || token == "" {
		return
	}
	gs, err := o.Secrets.Get(ctx, conn.ManagedSecretID)
	if err != nil {
		return
	}
	sealed, err := secrets.SealGenericSecret(o.Sealer, gs.ID, []byte(token))
	if err != nil {
		return
	}
	gs.SealedSecret = sealed
	gs.Last4 = secrets.Last4(token)
	gs.Fingerprint = secrets.FingerprintSHA256(token)
	_ = o.Secrets.Update(ctx, gs)
}

// EnsureManagedSecret exposes ensureManagedSecret to launch-time callers
// (the repo-targeted launch pins the connection's managed token as the
// run's forge secret — same Tier-0 pinning the webhook path uses).
//
// For a github_app connection the managed secret's plaintext is a ONE-HOUR
// installation token, so a stored value is dead for any launch that isn't
// right after a provision or a worker rotation (observed live: every
// repo-targeted launch on a quiet connection failing its clone with
// "Invalid username or token"). Re-mint at the point of use — one API
// call, best-effort: a mint failure keeps the stored token, which may
// still be live within its hour.
func (o *Orchestrator) EnsureManagedSecret(ctx context.Context, conn *Connection, actor string) (string, error) {
	secID, err := o.ensureManagedSecret(ctx, conn, actor)
	if err != nil {
		return "", err
	}
	o.narrowGitHubAppSecret(ctx, conn)
	return secID, nil
}

// ensureManagedSecret creates (once per connection) the team-scoped generic
// secret holding the connection's admin token as the bot-runtime forge
// token, stamping its id onto the connection. Reused across every repo/bot
// of the connection; the refresh worker rewrites its plaintext on rotation.
func (o *Orchestrator) ensureManagedSecret(ctx context.Context, conn *Connection, actor string) (string, error) {
	// A watch-only connection has no runtime token to hand a bot: its App
	// holds neither contents nor hooks. Refusing HERE is what makes the guard
	// exhaustive — this is the single chokepoint every runtime path funnels
	// through (Provision, and the repo-targeted launch via
	// EnsureManagedSecret), and it fires BEFORE Provision writes the bot
	// bindings. That ordering matters more than the message: a binding is
	// keyed (tenant, bot, secret_name) and therefore TEAM-GLOBAL, it is
	// written before the first forge call, and nothing rolls it back — so a
	// provision that fails later would leave every repo of that bot pointing
	// at a token that cannot push.
	if conn.IsSecurityReadOnly() {
		return "", fmt.Errorf("forge: connection %s is watch-only (Dependabot alerts): it holds no contents/hooks grant and has no runtime token to hand a bot — use the team's runtime connection", conn.ID)
	}
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
		AllowedHosts: forgeTokenEgressHosts(conn), // egress lock, see forgeTokenEgressHosts
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
// idempotent across re-provisions; operator tuning on surviving bots'
// rows (pause state, overlap/guard policy, a customised cron) is carried
// over by bot id so re-provisioning one bot doesn't reset the others.
// No-op when no schedule store is wired.
func (o *Orchestrator) syncSchedules(ctx context.Context, tenantID, integrationID, repoURL string, invByBot map[string][]bundle.Invocation, crons map[string]string, actor string) error {
	if o.Schedules == nil {
		return nil
	}
	// Snapshot the rows we're about to replace, queued per bot in creation
	// order so a bot with several schedule invocations keeps each row's
	// tuning positionally.
	priorByBot := map[string][]cloudsched.ScheduledBot{}
	if prior, err := o.Schedules.ListByIntegration(ctx, tenantID, integrationID); err == nil {
		for _, row := range prior {
			priorByBot[row.BotID] = append(priorByBot[row.BotID], row)
		}
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
			sb := cloudsched.ScheduledBot{
				ID:                o.id(),
				TenantID:          tenantID,
				RepoIntegrationID: integrationID,
				BotID:             bot,
				Cron:              cron,
				Vars:              inv.Schedule.DefaultVars,
				RepoURL:           repoURL,
				CreatedBy:         actor,
				CreatedAt:         now,
				UpdatedAt:         now,
			}
			if q := priorByBot[bot]; len(q) > 0 {
				prev := q[0]
				priorByBot[bot] = q[1:]
				// Explicit enable-dialog cron wins; otherwise the row the
				// operator may have edited does.
				if strings.TrimSpace(crons[bot]) == "" && strings.TrimSpace(prev.Cron) != "" {
					sb.Cron = prev.Cron
				}
				sb.Disabled = prev.Disabled
				sb.Overlap = prev.Overlap
				sb.MaxConcurrent = prev.MaxConcurrent
				sb.Guard = prev.Guard
				sb.GuardTimeout = prev.GuardTimeout
				sb.GuardVar = prev.GuardVar
				sb.CreatedBy = prev.CreatedBy
				sb.CreatedAt = prev.CreatedAt
			}
			if sb.Cron == "" {
				continue // no cron (no suggestion, no override) → can't fire
			}
			next, err := cloudsched.NextFire(sb.Cron, now)
			if err != nil {
				return fmt.Errorf("forge: schedule for %q: %w", bot, err)
			}
			sb.NextFireAt = next
			if err := o.Schedules.Create(ctx, sb); err != nil {
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
	// The security-read map is SHARED across connections, so it survives this
	// one — withdraw just this connection's org entry. Disconnecting is the
	// operator's most explicit cut: leaving a live org-wide alerts token
	// readable by every bot of the team (and then unreachable, since the
	// connection is gone) is the opposite of what they asked for.
	if conn.SecurityReadEnabled {
		if derr := RemoveSecurityReadToken(ctx, o.Secrets, o.Sealer, &conn); derr != nil {
			// A MALFORMED map must not block the operator's most explicit
			// security action (the documented hand-set path makes "a bare
			// PAT instead of a JSON map" the obvious mistake), and there is
			// nothing to withdraw from it anyway. Anything else — a store
			// error, a seal failure — would leave a LIVE org-wide token
			// behind, so it propagates and the disconnect is retried.
			if errors.Is(derr, ErrSecurityReadMalformed) {
				if o.LogWarn != nil {
					o.LogWarn("forge: %s holds an unparseable %s secret; disconnecting %s without withdrawing it: %v",
						conn.TenantID, SecurityReadSecretName, conn.ID, derr)
				}
			} else {
				return fmt.Errorf("forge: withdraw security-read token: %w", derr)
			}
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

// carryOperatorWebhookSettings preserves the operator-settable webhook fields
// this provision does NOT stamp. Re-provisioning rebuilds the whole Config
// literal, so a field that is neither stamped from the integration nor carried
// here is silently reset the next time any bot is enabled or disabled on the
// repo — and the webhook PATCH endpoint that sets these has no ProvisionedBy
// guard, so they are settable precisely on the managed configs this rebuilds.
//
// It is the complement of the operator-settings resolution above (LaunchVars /
// Overlap / HoldLabels / LabelAllowlist), which threads through the
// RepoIntegration because the provisioning API can also set those. These have
// no integration half: preserving the previous config IS their storage.
//
// Deliberately NOT carried: the bot scope (BotIDs / WildcardBots /
// DefaultBotID / BotRules / CommandMap), the allowlists and the routing table —
// those are what a provision exists to recompute from the enabled bots.
// Name is provision-owned (provisionedWebhookName) and LaunchVars threads
// through the RepoIntegration (the operator-settings resolution above), so
// neither belongs here.
//
// INVARIANT: a field listed here must have NO ProvisionRequest input. The
// carry is an unconditional overwrite placed after the literal, so the day one
// of these becomes settable through the provisioning API, it has to move to
// the operator-settings resolution above (the LaunchVars / HoldLabels shape) or
// the carry would silently override the caller. Two fields are CONDITIONAL
// exceptions, each commented in place: RateLimit (zero means never set) and
// MinReplierRole (a provision-derived floor merged stricter-of).
func carryOperatorWebhookSettings(cfg *webhooks.Config, prev webhooks.Config) {
	// Enabled is the operator's per-repo kill switch (PATCH {"enabled":false}
	// → every inbound delivery answers 410) — a re-provision must not
	// silently re-arm the lanes it paused. Safe to carry unconditionally:
	// this function only runs when a previous config exists, so the
	// provision literal's Enabled:true still governs first creation, and
	// re-enabling goes through the same PATCH that paused it.
	cfg.Enabled = prev.Enabled
	cfg.ReviewRequestLogins = prev.ReviewRequestLogins
	cfg.AuthorizedRepliers = prev.AuthorizedRepliers
	cfg.KeyOverrides = prev.KeyOverrides
	cfg.MonthlyCallLimit = prev.MonthlyCallLimit
	cfg.AutoImplementOnOpen = prev.AutoImplementOnOpen
	cfg.BranchImproveAsPR = prev.BranchImproveAsPR
	cfg.BlockForkPRs = prev.BlockForkPRs
	cfg.RetryUsageWindow = prev.RetryUsageWindow
	cfg.RetryMaxAttempts = prev.RetryMaxAttempts
	cfg.RetryMaxWait = prev.RetryMaxWait
	cfg.RetryJitter = prev.RetryJitter
	// The liveness stamp is written by MarkUsed with a $set; the rebuild
	// REPLACES the whole document, so without this every re-provision erases
	// "when did this webhook last receive anything" — the one field an
	// operator reads to tell a silent hook from an idle repo.
	cfg.LastUsedAt = prev.LastUsedAt

	// Rate limit: enforced by the inbound middleware, so losing an operator's
	// raise means deliveries silently 429 and reviews never launch — but an
	// UNPINNED value is the provisioner's own former default, and carrying it
	// would freeze every existing webhook on the burst it was born with,
	// making a default bump unreachable by re-provision (the review-comment
	// fan-out needs the raised burst precisely on already-provisioned repos).
	// Only an API-set value (RateLimitPinned, same rule as ReviewOnSyncPinned)
	// survives the rebuild.
	if prev.RateLimitPinned && prev.RateLimit != (webhooks.Rate{}) {
		cfg.RateLimit = prev.RateLimit
	}
	cfg.RateLimitPinned = prev.RateLimitPinned

	// MinReplierRole: a conditional merge, never an overwrite — the provision
	// stamps a manifest-derived FLOOR (the max requirement over the enabled
	// bots), which a blind carry would override. But the field is also
	// PATCH-settable with no ProvisionedBy guard, and an operator's RAISE is
	// a security control every replier gate reads (`/command`, `/revi`, the
	// re-request button); resetting it to the derived value on the next bot
	// toggle silently lowers the floor. Keep the stricter of the two — with
	// an unset prev deferring to the derivation: the gates read "" as
	// developer, but the DERIVATION ranks "" as zero (webhookRoleRank) so a
	// manifest may legitimately land a sub-developer floor, and a never-set
	// prev must not discard it (R948c68).
	if prev.MinReplierRole != "" &&
		webhooks.ReplierRoleRank(prev.MinReplierRole) > webhooks.ReplierRoleRank(cfg.MinReplierRole) {
		cfg.MinReplierRole = prev.MinReplierRole
	}

	// Secret overrides MERGE rather than replace: the provision owns the keys
	// it derives from the enabled bots (rewriting them is how a rotation
	// lands), while an operator's own pin — a bot posting under a different
	// forge identity — has nowhere else to live.
	for k, v := range prev.SecretOverrides {
		if _, stamped := cfg.SecretOverrides[k]; !stamped {
			if cfg.SecretOverrides == nil {
				cfg.SecretOverrides = map[string]string{}
			}
			cfg.SecretOverrides[k] = v
		}
	}
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

// buildBotRules materialises one webhooks.BotRule per co-enabled bot: the
// events it claims, its own author filter, and its own manifest launch vars.
// This is what lets one repo webhook serve several bots that react to
// DIFFERENT PRs — the flattened Config fields can only express a single
// winner, so without rules a dependency guard and a reviewer sharing a repo
// collapse into whichever one the legacy selection picks.
//
// The event set is derived in this order, and the order matters:
//  1. the bot's kind=forge invocations (the typed routing contract);
//  2. else its forge: block events MINUS the comment event — a manifest with
//     a forge: block and no invocations must keep launching (third-party
//     bots), but a comment must never auto-launch a bot: comments route
//     through CommandMap;
//  3. else no events — a command/board-only bot, reachable but never
//     auto-launched.
func buildBotRules(bots []string, frByBot map[string]*bundle.ForgeRequirements, invByBot map[string][]bundle.Invocation) []webhooks.BotRule {
	rules := make([]webhooks.BotRule, 0, len(bots))
	for _, b := range bots {
		fr := frByBot[b]
		rule := webhooks.BotRule{BotID: b}
		if fr != nil && fr.Webhook != nil {
			rule.AuthorAllowlist = append([]string(nil), fr.Webhook.AuthorAllowlist...)
			rule.LaunchVars = nilIfEmpty(maps.Clone(fr.Webhook.LaunchVars))
		}

		eventSet := map[string]bool{}
		actionSet := map[string]bool{}
		for _, inv := range invByBot[b] {
			if inv.Kind != bundle.InvocationKindForge || inv.Forge == nil {
				continue
			}
			eventSet[inv.Forge.Event] = true
			for _, a := range inv.Forge.Actions {
				actionSet[a] = true
			}
			if rule.Mode == "" {
				rule.Mode = string(inv.EffectiveMode())
			}
			if inv.Board != nil {
				rule.LabelAllowlist = dedupSorted(append(rule.LabelAllowlist, inv.Board.AllLabels...))
			}
		}
		if len(eventSet) == 0 && fr != nil {
			for _, e := range fr.Events {
				if e != bundle.ForgeEventPullRequestComment {
					eventSet[e] = true
				}
			}
		}
		rule.Events = sortedKeys(eventSet)
		rule.Actions = sortedKeys(actionSet)
		rules = append(rules, rule)
	}
	return rules
}

// backfillBotRules reconciles what a webhook config derives from its bots'
// manifests, when the bot/event set is otherwise unchanged: the per-bot
// routing table and whether a push must re-review. It rewrites the config
// ONLY when something actually differs, so the common re-enable stays a true
// no-op.
//
// Both reconciliations exist for the same reason — the ALREADY-provisioned
// repo is the production case, and it reaches the short-circuit, not the full
// path. A derivation that only runs on a fresh provision fixes nothing that
// is already deployed.
func (o *Orchestrator) backfillBotRules(ctx context.Context, webhookID string, bots []string, frByBot map[string]*bundle.ForgeRequirements, invByBot map[string][]bundle.Invocation) error {
	if webhookID == "" {
		return nil
	}
	want := resolveBotRules(bots, frByBot, invByBot)
	cfg, err := o.Webhooks.Get(ctx, webhookID)
	if err != nil {
		// A missing config is reported by the surrounding provision paths; a
		// backfill is best-effort reconciliation, not the authority on it.
		return nil //nolint:nilerr
	}
	// An operator pin of gate_enabled=false is an explicit per-repo decision
	// to run without a merge gate — the "gate needs re-review-on-sync to
	// survive" derivation no longer applies, in EITHER direction: don't force
	// sync on, and release a sync the derivation itself had forced. A sync
	// the operator set EXPLICITLY through the webhook API
	// (cfg.ReviewOnSyncPinned) is not the derivation's to touch at all — an
	// explicit choice is never silently replaced (Rf2f99f); an unpinned
	// value is presumed derivation-owned.
	gateOff := operatorGateDisabled(cfg.OperatorLaunchVars)
	wantSync := anyBotGatesMerges(bots, frByBot) && !gateOff && !cfg.ReviewOnSyncPinned
	dropSync := gateOff && cfg.ReviewOnSync && !cfg.ReviewOnSyncPinned
	if reflect.DeepEqual(cfg.BotRules, want) && (!wantSync || cfg.ReviewOnSync) && !dropSync {
		return nil
	}
	cfg.BotRules = want
	// With no gate pin this only ever turns ON: an operator who deliberately
	// disabled re-review on a repo whose bots gate must not have it silently
	// re-enabled under them.
	if wantSync {
		cfg.ReviewOnSync = true
	}
	if dropSync {
		cfg.ReviewOnSync = false
		// The moment the release becomes definitive is the moment to say so:
		// this is a repo-wide posture change (every co-enabled gating bot
		// stops re-reviewing pushes), and it is reachable from an ordinary
		// launch-vars update.
		if o.LogWarn != nil {
			o.LogWarn("forge: webhook %s: operator pinned gate_enabled off — releasing forced review_on_sync (pushes no longer auto-review; re-review is on-demand)", webhookID)
		}
	}
	cfg.UpdatedAt = o.clock()
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		return fmt.Errorf("forge: backfill bot rules on webhook %s: %w", webhookID, err)
	}
	return nil
}

// GateValueDisables classifies one EXPLICIT `gate_enabled` pin: true when
// the value leaves the gating bot silent. It deliberately mirrors the
// gating bots' own truthy test (`'1','true','yes','on'` — bots/review-pr
// publish step): the classification must track "will the bot post a
// status?" EXACTLY. Any value that does not affirmatively enable the gate
// counts as disabling — including the empty string (the runtime coerces ""
// to false) and unparsable values (passed through raw, failing the bot's
// truthy test). Shared with pkg/server's gate arms (claim, reconciler,
// auto-fix), so "the bot won't answer" and "the machinery must not arm"
// can never diverge.
func GateValueDisables(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return false
	}
	return true
}

// operatorGateDisabled reports whether the repo's operator launch vars pin
// the merge gate off. The pin is what turns the review bot advisory-only —
// the publish step skips the commit status and the server-side gate
// machinery never arms — so the anyBotGatesMerges derivation must not force
// re-review-on-sync for such a repo: the forced sync exists solely to keep
// a REQUIRED check alive across pushes, and a silent bot with a
// still-forced re-review is the deadlock-at-full-cost shape (every push
// reviewed, the required check never answered). An ABSENT key keeps the
// gate derivation untouched.
func operatorGateDisabled(vars map[string]string) bool {
	v, ok := vars["gate_enabled"]
	return ok && GateValueDisables(v)
}

// anyBotGatesMerges reports whether any of these bots declares the `statuses`
// scope — i.e. posts a commit status, i.e. can be a REQUIRED check.
//
// A required check lives on ONE head SHA: if the bot does not re-run when the
// author pushes a fix, the status is simply absent from the new head —
// indistinguishable from "never reviewed" — and the PR is blocked with no way
// forward but an admin bypass (observed live on SocialGouv/iterion#300). So
// re-review on sync is not an operator preference on such a repo, it is what
// makes the gate survivable. Derived from the DECLARED capability, never from
// a bot id.
func anyBotGatesMerges(bots []string, frByBot map[string]*bundle.ForgeRequirements) bool {
	for _, b := range bots {
		if fr := frByBot[b]; fr != nil && fr.TokenScopes[bundle.ForgeScopeStatuses] != "" {
			return true
		}
	}
	return false
}

// resolveBotRules derives the complete per-bot routing table — events, author
// filters and the exclusive claims resolved against each other — from the
// bots' manifests. Pure: same inputs always yield the same table, so callers
// can compare it against a stored one to decide whether a config needs a
// backfill.
func resolveBotRules(bots []string, frByBot map[string]*bundle.ForgeRequirements, invByBot map[string][]bundle.Invocation) []webhooks.BotRule {
	exclusive := map[string][]string{}
	for _, b := range bots {
		if fr := frByBot[b]; fr != nil && fr.Webhook.IsExclusiveAuthors() {
			exclusive[b] = append([]string(nil), fr.Webhook.AuthorAllowlist...)
		}
	}
	return applyExclusiveAuthors(buildBotRules(bots, frByBot, invByBot), exclusive)
}

// applyExclusiveAuthors materialises each bot's exclusive author claim into
// every OTHER rule's denylist, so a general reviewer stops double-reviewing
// the PRs a dependency guard owns WITHOUT the reviewer's manifest naming that
// guard. Storing the denylist (rather than deriving it per request) keeps the
// suppression auditable in the config the studio renders.
//
// Idempotent and order-independent; a bot never denies itself.
func applyExclusiveAuthors(rules []webhooks.BotRule, exclusiveByBot map[string][]string) []webhooks.BotRule {
	if len(exclusiveByBot) == 0 {
		return rules
	}
	for i := range rules {
		var deny []string
		for bot, authors := range exclusiveByBot {
			if bot == rules[i].BotID {
				continue
			}
			deny = append(deny, authors...)
		}
		rules[i].AuthorDenylist = dedupSorted(deny)
	}
	return rules
}

// manifestLaunchVars re-derives the union of the bots' own manifest launch
// vars — the layer the operator's overrides sit on top of.
func manifestLaunchVars(bots []string, frByBot map[string]*bundle.ForgeRequirements) map[string]string {
	out := map[string]string{}
	for _, b := range bots {
		if fr := frByBot[b]; fr != nil && fr.Webhook != nil {
			maps.Copy(out, fr.Webhook.LaunchVars)
		}
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
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
			// Same bot re-listed in the provision set: manifest validation
			// already rejects intra-bot duplicate command names, so the
			// incumbent route is identical — keep-first is lossless dedup.
			return nil
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
