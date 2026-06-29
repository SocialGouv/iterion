// Package orgsweep nightly-purges organizations that were soft-deleted
// (Status == pending_deletion) once their grace window (Org.PurgeAfter) has
// elapsed. "Purge" means HARD-deleting every trace: all team-scoped data across
// the cloud Mongo collections, then the identity records (teams, memberships,
// invitations, the org) via the auth-service cascade.
//
// Safety: every collection delete matches on the team/org's unique ID, so a
// missing or renamed collection (or a wrong tenant-field name) can only
// UNDER-purge — it can never touch another tenant's data. New tenant-scoped
// collections should be appended to the tables below.
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
	// event triggers (pkg/trigger)
	"trigger_subscriptions",
	// inbound webhooks (pkg/webhooks)
	"webhook_configs", "webhook_deliveries", "webhook_quotas",
	// native board (pkg/dispatcher/boardmongo)
	"board_issues", "board_config", "board_events",
}

// orgScopedCollections are partitioned by the org id (org_id or tenant_id ==
// the org's ID).
var orgScopedCollections = []string{
	"audit_events", // pkg/audit (control-plane log; tenant == org)
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
	orgFilter := bson.M{"$or": bson.A{bson.M{"org_id": orgID}, bson.M{"tenant_id": orgID}}}
	for _, coll := range orgScopedCollections {
		deleted += p.deleteMany(ctx, coll, orgFilter)
	}
	// org_usage docs use a composite _id "org|<orgID>|<month>".
	deleted += p.deleteMany(ctx, "org_usage",
		bson.M{"_id": bson.M{"$regex": "^org\\|" + regexp.QuoteMeta(orgID) + "\\|"}})
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
