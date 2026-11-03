// Package orgsweep nightly-purges organizations that were soft-deleted
// (Status == pending_deletion) once their grace window (Org.PurgeAfter) has
// elapsed. "Purge" means HARD-deleting every trace: every run of every team
// through the run store's DeleteRun (every S3 object of the six key spaces a
// run owns, the IR bundle snapshots included, then its Mongo side tables),
// all team-scoped data across the cloud Mongo
// collections, the org-scoped rows, then the identity records (teams,
// memberships, invitations, the org) via the auth-service cascade. What a
// purge deliberately leaves is named in notPurged with its reason — the user
// rows first: a user outlives their orgs.
//
// Safety: every collection delete matches on the team/org's unique ID (or on
// a reserved scope derived from it), so a missing or renamed collection (or a
// wrong tenant-field name) can only UNDER-purge — it can never touch another
// tenant's data. New tenant-scoped collections should be appended to the
// tables below; rows owned by a RESERVED scope rather than a plain tenant id
// cannot live in those tables and are purged explicitly in PurgeOrg.
//
// Order: the runs' blobs go first and nothing in Mongo is swept while a blob
// delete has failed, so an interrupted or failed purge leaves the org
// pending — fully re-purgeable by the next sweep, on any replica — and never
// a run row gone with its objects still in the bucket, where no key would
// name the tenant any more. A run that is not terminal is not deleted
// (store.RunDeletable): it withholds the sweep like a failed delete. The
// org's eligibility is re-read before every run delete and before the first
// collection sweep, so a restore accepted mid-purge stops the purge there.
//
// Escape hatch: ForceSweepEnv names the orgs whose sweep goes on past the
// runs that could not be deleted — their objects are abandoned in the
// bucket, each named in a warning.
package orgsweep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
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
	// control-plane audit (pkg/audit) — written under BOTH scopes: a
	// team-scoped row carries tenant_id == the team id (auditTenant: the
	// member.added with the member's email, team.status_changed, …), an
	// org-scoped or platform row carries the org id. It is listed in both
	// tables so each loop reaches its half; absent from this one, every
	// team-keyed row would survive the purge of its org.
	"audit_events",
}

// orgScopedCollections are partitioned by the org id (org_id or tenant_id ==
// the org's ID).
var orgScopedCollections = []string{
	"audit_events", // pkg/audit (control-plane log; the org-keyed half, see teamScopedCollections)
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
// referential order. Every row of this table is witnessed against the real
// cascade (cascade_truth_test.go): a name listed here that the cascade
// leaves behind fails that test.
var purgedByCascade = map[string]string{
	"orgs":            "the org row itself",
	"teams":           "every team of the org",
	"memberships":     "team memberships",
	"org_memberships": "org memberships",
	"invitations":     "pending invitations",
}

// notPurged name the collections an org purge deliberately LEAVES, each with
// the reason. They are the answer to "this collection is tenant-shaped, why
// does the sweep ignore it" — a question that otherwise gets re-asked by
// every reader of this file, and silently answered wrong by the next one.
var notPurged = map[string]string{
	// The USER, who outlives the org (they may hold others, and may be
	// invited into a new one). Erasing an account is an explicit request,
	// never a side effect of an org purge.
	"users":      "the user rows: a user outlives the org and may be re-invited; account erasure is its own explicit gesture",
	"oidc_links": "a user's SSO identity links, kept with the user row they belong to",
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

// RunStore is the slice of the run store the purge needs: enumerate the live
// runs of the tenant the context carries, load one to judge it deletable
// (store.RunDeletable), and delete one with every blob it owns (the six S3
// key spaces keyed by run id — artifacts, attachments, tools, IR and its
// bundle snapshots, sessions, run files — then its Mongo side tables).
// Satisfied by the cloud store (pkg/store/mongo); every call is made under
// store.WithTenant.
type RunStore interface {
	ListRunIDsForPurge(ctx context.Context) ([]string, error)
	LoadRun(ctx context.Context, id string) (*store.Run, error)
	DeleteRun(ctx context.Context, id string) error
}

// RunBatchEnv names the operator dial behind Purger.RunBatch.
const RunBatchEnv = "ITERION_ORG_PURGE_RUN_BATCH"

// ForceSweepEnv names the operator escape hatch behind Purger.ForceSweep: a
// comma-separated list of org ids whose purge sweeps Mongo even though some
// of their runs could not be deleted.
const ForceSweepEnv = "ITERION_ORG_PURGE_FORCE_SWEEP"

// ErrNoLongerEligible is returned by PurgeOrg when the org stops being
// eligible (restored, re-scheduled, or gone) while its purge is under way:
// the purge stops before its next irreversible step. The runs already
// deleted stay deleted; nothing else of the org is swept.
var ErrNoLongerEligible = errors.New("orgsweep: the org is no longer eligible for purge")

// ErrPurgeDeferred is returned by PurgeOrg when the run batch (Purger.RunBatch)
// is exhausted before the org's runs are all gone: the pass stopped before
// any Mongo sweep, the org stays pending_deletion, and the next sweep resumes
// where this one stopped. Sweep reports it as progress, not as a failure.
var ErrPurgeDeferred = errors.New("orgsweep: run batch exhausted, the purge resumes at the next sweep")

// runProgressEvery is the number of deleted runs between two progress log
// lines of one org purge.
const runProgressEvery = 100

// Purger hard-purges one org's data across the cloud Mongo database and the
// blob store.
type Purger struct {
	DB      *mongo.Database
	Store   identity.Store
	Cascade Cascade
	Logger  *log.Logger
	// Runs deletes each run of the org with its blobs before the Mongo
	// collections are swept. Required where the cloud run store is wired —
	// a nil Runs refuses to purge any org that has a team (an explicit
	// error, never a DeleteMany that orphans the objects).
	Runs RunStore
	// RunBatch bounds the run deletes one PurgeOrg call ATTEMPTS — failed
	// ones included, so a bucket outage cannot stretch a pass over every run
	// of the org — before it defers the rest to the next sweep
	// (ErrPurgeDeferred). A run delete is paginated S3 work, so an org of
	// many thousands of runs is purged over several nights rather than in
	// one pass with no bound on its length. Each pass re-lists the live runs
	// of every team of the org, so a small batch multiplies the listings by
	// the number of nights the org takes to drain. 0 (the default) is no
	// bound: the escape hatch when the operator wants the org gone in one
	// pass. Set from RunBatchEnv by the server.
	RunBatch int
	// ForceSweep names the orgs (by id) whose Mongo sweep goes on even
	// though some of their runs could not be deleted — a run whose delete
	// keeps failing, or one that is not terminal. Each abandoned run is named
	// in a warning; its objects stay in the bucket, unattributed. A forced
	// org is purged in one pass: RunBatch does not apply to it, so a run that
	// keeps failing cannot hold the batch on every pass. The operator's
	// escape hatch for an org a run withholds for good. Set from
	// ForceSweepEnv by the server.
	ForceSweep map[string]bool
}

// RunBatchFromEnv reads RunBatchEnv: unset or empty is 0 (no bound); a value
// that is not a non-negative integer is refused rather than read as 0.
func RunBatchFromEnv() (int, error) {
	raw, ok := os.LookupEnv(RunBatchEnv)
	if !ok || raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("orgsweep: %s=%q: want a non-negative integer (0 = no bound)", RunBatchEnv, raw)
	}
	return n, nil
}

// ForceSweepFromEnv reads ForceSweepEnv: a comma-separated list of org ids,
// blanks ignored. Unset or empty forces nothing.
func ForceSweepFromEnv() map[string]bool {
	out := map[string]bool{}
	for _, id := range strings.Split(os.Getenv(ForceSweepEnv), ",") {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}

// purgeEligible is the one predicate every irreversible step of a purge
// crosses: the org is soft-deleted and its grace has elapsed at now.
func purgeEligible(org identity.Org, now time.Time) bool {
	return org.PendingDeletion() && org.PurgeAfter != nil && !org.PurgeAfter.After(now)
}

// stillEligible re-reads the org and returns nil only while it is still
// eligible. A restore, a re-scheduled deletion or an org already gone comes
// back as ErrNoLongerEligible; a failed read is returned as is — either way
// the purge stops before its next irreversible step.
func (p *Purger) stillEligible(ctx context.Context, orgID string) error {
	org, err := p.Store.GetOrg(ctx, orgID)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return fmt.Errorf("%w: org %s is gone", ErrNoLongerEligible, orgID)
		}
		return fmt.Errorf("orgsweep: re-read org %s before the next purge step: %w", orgID, err)
	}
	if !purgeEligible(org, time.Now().UTC()) {
		return fmt.Errorf("%w: org %s is %s", ErrNoLongerEligible, orgID, org.EffectiveStatus())
	}
	return nil
}

// PurgeOrg removes every trace of one org: every run of each of its teams
// with its blobs, all team-scoped collections for each team, all org-scoped
// collections, then the identity cascade. Safe to call more than once
// (DeleteRun and DeleteMany are idempotent; a no-op once the org is gone),
// and safe to interrupt: no Mongo row is swept before every blob is gone.
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
	// The same predicate is re-read before every run delete and before the
	// first collection sweep (stillEligible), so a restore accepted while the
	// runs are being deleted stops the purge at its next step.
	if !purgeEligible(org, time.Now().UTC()) {
		if p.Logger != nil {
			p.Logger.Info("orgsweep: skip org %s (%s) — no longer eligible (status=%s)", org.ID, org.Name, org.EffectiveStatus())
		}
		return 0, nil
	}
	teams, err := p.Store.ListTeamsByOrg(ctx, orgID)
	if err != nil {
		return 0, err
	}
	// The runs first, each through DeleteRun: the blobs are keyed by run id
	// alone, so once the run row is gone nothing names the tenant of an
	// object left in the bucket. A failed run delete stops the purge here,
	// before any DeleteMany, and the org stays pending for the next sweep.
	deleted, err := p.purgeRuns(ctx, org, teams)
	if err != nil {
		return deleted, err
	}
	if err := p.stillEligible(ctx, orgID); err != nil {
		return deleted, err
	}
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

// purgeRuns deletes every run of every team of the org through the run
// store, under each team's tenant scope, and returns how many it deleted.
// Every run is attempted, so one failing run does not shadow the others;
// the joined failures come back after the pass and leave the org pending. A
// run that is not terminal (store.RunDeletable) is not deleted — the engine
// may still be writing its objects — and counts as a failure: the org stays
// pending until it ends. The org's eligibility is re-read before each run
// delete, so a restore stops the pass (ErrNoLongerEligible). The batch
// bound, once reached by the attempts, returns ErrPurgeDeferred — a run
// already deleted is tombstoned and not listed again, so the next pass
// resumes with the remaining ones.
func (p *Purger) purgeRuns(ctx context.Context, org identity.Org, teams []identity.Team) (int64, error) {
	var deleted int64
	var failures []error
	forced := p.ForceSweep[org.ID]
	for _, t := range teams {
		tctx := store.WithTenant(ctx, t.ID)
		if p.Runs == nil {
			return deleted, fmt.Errorf("orgsweep: purge org %s: no run store wired, refusing to sweep team %s while its runs may own blobs", org.ID, t.ID)
		}
		ids, err := p.Runs.ListRunIDsForPurge(tctx)
		if err != nil {
			return deleted, fmt.Errorf("orgsweep: list runs of team %s: %w", t.ID, err)
		}
		for i, id := range ids {
			if err := ctx.Err(); err != nil {
				return deleted, err
			}
			if !forced && p.RunBatch > 0 && deleted+int64(len(failures)) >= int64(p.RunBatch) {
				if p.Logger != nil {
					p.Logger.Info("orgsweep: org %s (%s): run batch of %d attempt(s) reached, %d run(s) of team %s still pending — resuming at the next sweep", org.ID, org.Name, p.RunBatch, len(ids)-i, t.ID)
				}
				// A pass that already lost runs is a failed pass, not
				// progress: the failures are the verdict, the batch only
				// says where it stopped.
				if len(failures) > 0 {
					return deleted, withheldSweep(org, failures)
				}
				return deleted, ErrPurgeDeferred
			}
			if err := p.stillEligible(ctx, org.ID); err != nil {
				return deleted, err
			}
			gone, err := p.deleteRun(tctx, id)
			if err != nil {
				if p.Logger != nil {
					p.Logger.Warn("orgsweep: delete run %s of team %s: %v", id, t.ID, err)
				}
				failures = append(failures, fmt.Errorf("run %s of team %s: %w", id, t.ID, err))
				continue
			}
			if !gone {
				continue
			}
			deleted++
			if p.Logger != nil && deleted%runProgressEvery == 0 {
				p.Logger.Info("orgsweep: org %s (%s): %d run(s) deleted with their blobs so far", org.ID, org.Name, deleted)
			}
		}
	}
	if len(failures) == 0 {
		return deleted, nil
	}
	if forced {
		if p.Logger != nil {
			p.Logger.Warn("orgsweep: org %s (%s): %s forces the sweep past %d run(s) that could not be deleted — their objects stay in the bucket, no longer attributable to the org: %v", org.ID, org.Name, ForceSweepEnv, len(failures), errors.Join(failures...))
		}
		return deleted, nil
	}
	return deleted, withheldSweep(org, failures)
}

// deleteRun deletes one run of the tenant tctx carries, through the one
// lifecycle guard every delete authority crosses (store.RunDeletable): a run
// that is not terminal is refused with ErrRunNotDeletable. A run gone since
// the listing is no failure: it reports gone=false, nil.
func (p *Purger) deleteRun(tctx context.Context, id string) (gone bool, err error) {
	r, err := p.Runs.LoadRun(tctx, id)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("load: %w", err)
	}
	if err := store.RunDeletable(r); err != nil {
		return false, err
	}
	if err := p.Runs.DeleteRun(tctx, id); err != nil {
		return false, err
	}
	return true, nil
}

// withheldFailuresShown is how many failed runs the withheld-sweep error
// names in full; the rest are counted. Every failure already has its own
// Warn line from purgeRuns, so the one error the sweep logs stays one line
// even when an org of thousands of runs fails against a bucket outage.
const withheldFailuresShown = 5

// withheldSweep is the verdict of a pass that could not delete every run it
// reached: the Mongo sweep is withheld and the org stays pending. The
// returned error wraps the first withheldFailuresShown failures, so
// errors.Is finds the cause of each.
func withheldSweep(org identity.Org, failures []error) error {
	shown := failures
	more := ""
	if len(failures) > withheldFailuresShown {
		shown = failures[:withheldFailuresShown]
		more = fmt.Sprintf(" (the first %d shown, %d more in the per-run warnings)", withheldFailuresShown, len(failures)-withheldFailuresShown)
	}
	return fmt.Errorf("orgsweep: purge org %s: %d run(s) could not be deleted, the Mongo sweep is withheld until they are%s: %w", org.ID, len(failures), more, errors.Join(shown...))
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
// HourUTC (plus one immediate catch-up when Run starts). The sweep is
// idempotent — PurgeOrg no-ops once the org is gone — but its cost (S3
// listings and deletes per run) is not, so the server runs it on the one
// replica holding its lease (pkg/server, leaseOrgPurgeSweeper).
type Sweeper struct {
	Purger  *Purger
	HourUTC int // hour of day (UTC) to run; 0..23, default 2
	Logger  *log.Logger
}

// Run loops the nightly sweep until ctx is cancelled. The server calls it
// for each lease term it holds; a term that ends cancels ctx, which stops a
// purge under way at its next run. After each sweep the pass's verdict goes
// to health (lease.Health.Observe), and Run returns when it says to hand the
// term over — a replica whose bucket egress is broken gives the lease to a
// peer. A nil health never ends the loop on the work failing.
func (s *Sweeper) Run(ctx context.Context, health *lease.Health) {
	for {
		var pass lease.Pass
		if _, err := s.sweep(ctx, &pass); err != nil && s.Logger != nil {
			s.Logger.Warn("orgsweep: sweep: %v", err)
		}
		if health.Observe(pass.Verdict()) {
			return
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

// Sweep purges every org whose grace has elapsed, once. Exposed for tests.
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	return s.sweep(ctx, nil)
}

// sweep is Sweep accounting into pass: each org whose purge ended, deferred
// or stopped on a restore is one success, each failed purge one failure, a
// failed listing one failure; an interrupted context is not counted (it is
// the term ending, not the work failing). A nil pass accounts into nothing.
func (s *Sweeper) sweep(ctx context.Context, pass *lease.Pass) (int, error) {
	orgs, err := s.Purger.Store.ListOrgsPendingPurge(ctx, time.Now().UTC())
	if err != nil {
		if ctx.Err() == nil {
			pass.Failed(err)
		}
		return 0, err
	}
	purged := 0
	for _, o := range orgs {
		n, err := s.Purger.PurgeOrg(ctx, o.ID)
		switch {
		case ctx.Err() != nil:
		case err == nil, errors.Is(err, ErrPurgeDeferred), errors.Is(err, ErrNoLongerEligible):
			pass.Succeeded()
		default:
			pass.Failed(err)
		}
		if errors.Is(err, ErrPurgeDeferred) {
			if s.Logger != nil {
				s.Logger.Info("orgsweep: org %s (%s): %d run(s) deleted this pass, the purge resumes at the next sweep", o.ID, o.Name, n)
			}
			continue
		}
		if errors.Is(err, ErrNoLongerEligible) {
			// The operator restored the org (or re-scheduled its deletion)
			// while its runs were being deleted: what the pass deleted is
			// gone for good, and the restored org comes back without it.
			if s.Logger != nil {
				s.Logger.Warn("orgsweep: org %s (%s) left purge eligibility mid-purge (%v) — the purge stopped; %d run(s) already deleted with their blobs stay deleted, nothing else of the org was swept", o.ID, o.Name, err, n)
			}
			continue
		}
		if err != nil {
			if s.Logger != nil {
				s.Logger.Warn("orgsweep: purge org %s (%s): %v", o.ID, o.Name, err)
			}
			continue
		}
		purged++
		if s.Logger != nil {
			s.Logger.Info("orgsweep: purged org %s (%s) — %d run(s) and doc(s)", o.ID, o.Name, n)
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
