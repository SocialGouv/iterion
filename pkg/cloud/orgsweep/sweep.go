// Package orgsweep nightly-purges organizations that were soft-deleted
// (Status == pending_deletion) once their grace window (Org.PurgeAfter) has
// elapsed. "Purge" means HARD-deleting every trace: all team-scoped data across
// the cloud Mongo collections, then the identity records (teams, memberships,
// invitations, the org) via the auth-service cascade.
//
// Safety: every collection delete matches on the team/org's unique ID (or on
// a reserved scope derived from it), so a missing or renamed collection (or a
// wrong tenant-field name) can only UNDER-purge — it can never touch another
// tenant's data. New tenant-scoped collections should be appended to the
// tables below; rows owned by a RESERVED scope rather than a plain tenant id
// cannot live in those tables and are purged explicitly in PurgeOrg.
package orgsweep

import (
	"context"
	"errors"
	"regexp"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// teamScopedCollections are partitioned by team (tenant_id or team_id == the
// team's ID). Grouped by their owning package.
var teamScopedCollections = []string{
	// run store (pkg/store/mongo)
	"runs", "run_seq", "events", "interactions", "user_messages",
	// workspace memory (pkg/store/mongo)
	"memory_docs", "memory_spaces", "memory_tenant_usage",
	// forge (pkg/forge)
	"forge_connections", "forge_oauth_apps", "repo_integrations",
	// secrets (pkg/secrets)
	"api_keys", "generic_secrets", "run_secrets", "bot_secret_bindings",
	// org SSO (pkg/auth/orgsso) — keyed on the org's primary team
	"org_sso_providers", "org_verified_domains",
	// run store, the per-run side tables (pkg/store/mongo)
	"run_gitmeta", "run_plans", "run_notes", "run_turns", "run_logs",
	"run_tags", "run_route_decisions", "retry_circuits",
	// event triggers (pkg/trigger)
	"trigger_subscriptions",
	// inbound webhooks (pkg/webhooks)
	"webhook_configs", "webhook_deliveries", "webhook_quotas",
	"webhook_deferred_launches",
	// native board (pkg/dispatcher/boardmongo)
	"board_issues", "board_config", "board_events", "trigger_effects",
	// cloud schedules (pkg/cloudsched)
	"scheduled_bots",
	// config shares (pkg/configshare)
	"config_shares", "config_share_deliveries",
	// user notifications (pkg/usernotify, pkg/usernotify/webpush)
	"notification_prefs", "push_subscriptions",
	// plugin sources (pkg/pluginsource)
	"plugin_sources",
	// bot sources (pkg/botsource)
	"bot_sources", "bot_source_versions",
	// assistant dock (pkg/assistantmission, pkg/runwatch)
	"assistant_missions", "assistant_run_watches", "assistant_run_watch_episodes",
	// model preferences (pkg/modelprefs)
	"model_prefs",
	// mutualised credential pool (pkg/credpool)
	"cred_pools", "cred_pledges", "cred_leases", "cred_pool_usage",
	// per-credential usage views (pkg/credusage)
	"credential_usage",
	// forge, the records keyed on the team rather than the connection
	"forge_board_bindings", "forge_provision_approvals",
	// personal access tokens (pkg/pat) — a token is minted FOR a team
	"personal_access_tokens",
	// connector connections (pkg/connection) — a credential is scoped to one
	// team, every store read filters on it
	"connections",
}

// orgScopedCollections are partitioned by the org id (org_id or tenant_id ==
// the org's ID).
var orgScopedCollections = []string{
	"audit_events", // pkg/audit (control-plane log; tenant == org)
	"marketplace",  // pkg/marketplace (a listing is published by an org)
}

// purgedByBespokeFilter name the collections PurgeOrg deletes from with a
// filter of their own, because the tenant is not a plain top-level field: it
// rides a composite _id, a reserved owner key, or a nested document. Reading
// the reason here is how the next reader knows the loops above are not where
// to look.
var purgedByBespokeFilter = map[string]string{
	"oauth_credentials":       "keyed by an owner in its own namespace (secrets.OrgOwnerKey / OrgTierOwnerKey), not by a tenant id",
	"llm_routing_policies":    "composite _id (\"team:<id>\" / \"org:<id>\"), ADR-121 delivery 2",
	"org_usage":               "composite _id (\"org|<id>|<month>\", \"forkauthor|<id>|…\"), purged by the id-prefix alternation",
	"schedule_launch_intents": "the tenant rides the embedded schedule document (schedule.tenant_id), not a top-level field",
}

// purgedByCascade name the identity records the auth-service cascade removes
// (Cascade, called last): deleting them here would race the cascade's own
// referential order.
var purgedByCascade = map[string]string{
	"orgs":            "the org row itself",
	"teams":           "every team of the org",
	"memberships":     "team memberships",
	"org_memberships": "org memberships",
	"invitations":     "pending invitations",
	"oidc_links":      "SSO identity links of the deleted users",
	"users":           "the user rows the cascade owns",
}

// notPurged name the collections an org purge deliberately LEAVES, each with
// the reason. They are the answer to "this collection is tenant-shaped, why
// does the sweep ignore it" — a question that otherwise gets re-asked by
// every reader of this file, and silently answered wrong by the next one.
var notPurged = map[string]string{
	// Credentials of a USER, who outlives the org (they may hold others).
	"sessions":            "a user's refresh sessions: the user outlives the org",
	"session_revocations": "revocation markers of a user's sessions, TTL-reaped",
	"password_resets":     "a user's reset tokens, TTL-reaped",
	"oidc_states":         "in-flight OIDC states, TTL-reaped, carry no tenant",
	"ws_tickets":          "single-use WebSocket tickets, TTL-reaped",
	"desktop_sso_tickets": "single-use desktop SSO tickets, TTL-reaped",
	"oauth_pending":       "in-flight OAuth authorizations, TTL-reaped; the sealed result lands in oauth_credentials, which IS purged",
	// Deployment-wide records with no tenant dimension.
	"leases":             "deployment-wide coordination leases keyed by resource name, expiry-reaped",
	"usage_windows":      "per-credential provider readings keyed by the credential, not the tenant; the credential itself is purged",
	"platform_settings":  "the deployment's own settings record",
	"sent_notifications": "idempotency claims keyed by notification key, TTL-reaped, carry no tenant",
	// Uploads a launch form staged and no run claimed yet: the staging sweep
	// of every replica removes the bytes THEN the record within the TTL. A
	// tenant-filtered purge of the records would orphan the bytes under
	// uploads/<id>/ for good — the leak the sweep's order exists to prevent.
	"staged_uploads": "uploads no run claimed yet, reaped within the hour by the staging sweep (bytes, then record); a record-only purge would orphan the bytes",
}

// Cascade hard-deletes an org's identity records (teams, memberships,
// invitations, the org). Satisfied by auth.Service.DeleteOrgCascade.
type Cascade func(ctx context.Context, orgID string) error

// Purger hard-purges one org's data across the cloud Mongo database.
type Purger struct {
	DB      *mongo.Database
	Store   identity.Store
	Cascade Cascade
	Logger  *log.Logger
}

// PurgeOrg removes every trace of one org: all team-scoped collections for each
// of its teams, all org-scoped collections, then the identity cascade. Safe to
// call more than once (DeleteMany is idempotent; a no-op once the org is gone).
func (p *Purger) PurgeOrg(ctx context.Context, orgID string) (int64, error) {
	org, err := p.Store.GetOrg(ctx, orgID)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return 0, nil // already purged (e.g. by another replica)
		}
		return 0, err
	}
	// Defense-in-depth for an IRREVERSIBLE delete: re-assert eligibility here,
	// not only in the caller's pending-list query. This guards against a
	// direct/buggy call with an active org's ID AND the restore-during-sweep
	// race — the operator cancels deletion after Sweep took the pending list
	// but before this runs. A non-eligible org is a safe no-op, never a purge.
	if !org.PendingDeletion() || org.PurgeAfter == nil || org.PurgeAfter.After(time.Now().UTC()) {
		if p.Logger != nil {
			p.Logger.Info("orgsweep: skip org %s (%s) — no longer eligible (status=%s)", org.ID, org.Name, org.EffectiveStatus())
		}
		return 0, nil
	}
	teams, err := p.Store.ListTeamsByOrg(ctx, orgID)
	if err != nil {
		return 0, err
	}
	var deleted int64
	for _, t := range teams {
		filter := bson.M{"$or": bson.A{bson.M{"tenant_id": t.ID}, bson.M{"team_id": t.ID}}}
		for _, coll := range teamScopedCollections {
			deleted += p.deleteMany(ctx, coll, filter)
		}
	}
	// Sealed credentials owned by a RESERVED scope rather than by a plain
	// tenant id. Neither loop above can reach them: an org-tier row carries
	// tenant_id "orgtier:<org>" (not the org id, not any team id), and an
	// OAuth record is keyed by an owner in its own namespace — which is why
	// oauth_credentials is absent from teamScopedCollections entirely, and
	// why a team forfait survived every purge before this.
	//
	// A PERSONAL forfait is deliberately NOT purged here: it belongs to a
	// user, and a user outlives the org (they may hold others). Only the
	// two owner keys this org actually owns are removed.
	for _, t := range teams {
		deleted += p.deleteMany(ctx, secrets.OAuthCollectionName,
			bson.M{"user_id": secrets.OrgOwnerKey(t.ID)})
	}
	// The tenant routing policies (ADR-121 delivery 2) are keyed by
	// COMPOSITE _id ("team:<id>" / "org:<id>"), not by a tenant_id field —
	// the org_usage id-prefix shape, reached the same way. The team records
	// are enumerated while the team rows are still listed.
	deleted += p.deleteMany(ctx, platformcfg.ColRoutingPolicies,
		bson.M{"_id": platformcfg.OrgRoutingPolicyID(orgID)})
	for _, tm := range teams {
		deleted += p.deleteMany(ctx, platformcfg.ColRoutingPolicies,
			bson.M{"_id": platformcfg.TeamRoutingPolicyID(tm.ID)})
	}
	// A launch intent carries its tenant INSIDE the schedule it embeds
	// (cloudsched.LaunchIntent.Schedule), so neither loop above reaches it —
	// and an intent is a pending LAUNCH: left behind, it names a schedule of
	// a team that no longer exists.
	for _, t := range teams {
		deleted += p.deleteMany(ctx, "schedule_launch_intents",
			bson.M{"schedule.tenant_id": t.ID})
	}
	orgTier := secrets.OrgTierTenantID(orgID)
	deleted += p.deleteMany(ctx, "api_keys",
		bson.M{"$or": bson.A{bson.M{"tenant_id": orgTier}, bson.M{"scope_team": orgTier}}})
	deleted += p.deleteMany(ctx, secrets.OAuthCollectionName,
		bson.M{"user_id": secrets.OrgTierOwnerKey(orgID)})

	orgFilter := bson.M{"$or": bson.A{bson.M{"org_id": orgID}, bson.M{"tenant_id": orgID}}}
	for _, coll := range orgScopedCollections {
		deleted += p.deleteMany(ctx, coll, orgFilter)
	}
	// org_usage holds TWO composite _id kinds, both second-segmented on the
	// metering id: "org|<orgID>|<month>" (the org's own budget) and
	// "forkauthor|<orgID>|<provider>|<authorID>|<month>" (one outside
	// contributor's bound on the fork review lane — orgusage.ForkAuthorSubject).
	// Both KINDS are purged for an org id. What is NOT reached either way is
	// a row whose second segment is a TEAM id — gateMonthlyCaps falls back to
	// the team when the org cannot be resolved, PurgeOrg is only ever called
	// with an org id, and org_usage is not in teamScopedCollections. That
	// residue predates this filter and is bounded by the collection's 400-day
	// TTL; it is named here rather than left for the next reader to discover.
	// A new kind added to this collection has to be added to the alternation.
	deleted += p.deleteMany(ctx, "org_usage",
		bson.M{"_id": bson.M{"$regex": "^(org|forkauthor)\\|" + regexp.QuoteMeta(orgID) + "\\|"}})
	// Identity records last — this removes the teams + the org row itself.
	if err := p.Cascade(ctx, orgID); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func (p *Purger) deleteMany(ctx context.Context, coll string, filter bson.M) int64 {
	res, err := p.DB.Collection(coll).DeleteMany(ctx, filter)
	if err != nil {
		if p.Logger != nil {
			p.Logger.Warn("orgsweep: delete from %s: %v", coll, err)
		}
		return 0
	}
	return res.DeletedCount
}

// Sweeper runs PurgeOrg for every org whose grace has elapsed, nightly at
// HourUTC (plus one immediate catch-up at boot). Idempotent across replicas:
// each runs the same sweep, and PurgeOrg no-ops once the org is gone.
type Sweeper struct {
	Purger  *Purger
	HourUTC int // hour of day (UTC) to run; 0..23, default 2
	Logger  *log.Logger
}

// Run loops the nightly sweep until ctx is cancelled. Start one per replica.
func (s *Sweeper) Run(ctx context.Context) {
	for {
		if _, err := s.Sweep(ctx); err != nil && s.Logger != nil {
			s.Logger.Warn("orgsweep: sweep: %v", err)
		}
		timer := time.NewTimer(time.Until(nextRun(time.Now().UTC(), s.HourUTC)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Sweep purges every org whose grace has elapsed, once. Exposed for tests and
// the boot catch-up.
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	orgs, err := s.Purger.Store.ListOrgsPendingPurge(ctx, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	purged := 0
	for _, o := range orgs {
		n, err := s.Purger.PurgeOrg(ctx, o.ID)
		if err != nil {
			if s.Logger != nil {
				s.Logger.Warn("orgsweep: purge org %s (%s): %v", o.ID, o.Name, err)
			}
			continue
		}
		purged++
		if s.Logger != nil {
			s.Logger.Info("orgsweep: purged org %s (%s) — %d docs", o.ID, o.Name, n)
		}
	}
	return purged, nil
}

// nextRun is the next HourUTC:00 strictly after now.
func nextRun(now time.Time, hourUTC int) time.Time {
	if hourUTC < 0 || hourUTC > 23 {
		hourUTC = 2
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), hourUTC, 0, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}
