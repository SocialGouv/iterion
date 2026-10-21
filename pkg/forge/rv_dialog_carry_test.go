package forge

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
)

// The enable-dialog cron (schedule_crons on the enable/update request) is
// applied to EVERY schedule invocation of the bot, so the moment it is
// spoken the invocation's RESOLVED cron stops distinguishing the lanes —
// and when the dialog cron equals one lane's stored cron, a carry keyed on
// the resolved cron hands that lane's row to the FIRST invocation: the row
// is the only one matching the scan, deterministically, not a listing-order
// coin toss. The pairing must key on the invocation's SUGGESTED cron first
// — the manifest's lane identity, which the dialog override cannot rewrite.
// Production ids: the rows' ids travel through the carry, so a wrong
// pairing sticks instead of flipping back on the next resync.
func TestRV_DialogCron_KeepsEachLanesTuning(t *testing.T) {
	ctx := context.Background()
	for attempt := 0; attempt < 10; attempt++ {
		o, _, sealer := newTestOrch(t)
		o.NewID = nil // the production id(): uuid.NewString()
		seedConn(t, o, sealer)
		sched := cloudsched.NewMemoryStore()
		o.Schedules = sched
		o.Invocations = func(_ context.Context, _, botID string) ([]bundle.Invocation, error) {
			if botID == "review-pr" {
				return []bundle.Invocation{
					{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{
						SuggestedCron: "0 3 * * *", DefaultVars: map[string]string{"mode": "collect"},
					}},
					{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{
						SuggestedCron: "0 4 * * *", DefaultVars: map[string]string{"mode": "digest"},
					}},
				}, nil
			}
			return nil, nil
		}
		t0 := time.Unix(1700000000, 0).UTC()
		o.Now = func() time.Time { return t0 }
		res, err := o.Provision(ctx, ProvisionRequest{
			TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
			BotIDs: []string{"review-pr"}, ActorID: "u1",
		})
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		rows, err := sched.ListByIntegration(ctx, "t1", res.IntegrationID)
		if err != nil || len(rows) != 2 {
			t.Fatalf("rows = %v (err %v), want 2", rows, err)
		}
		var digestLaneID string
		for _, r := range rows {
			if r.Cron == "0 4 * * *" {
				digestLaneID = r.ID
			}
		}
		if digestLaneID == "" {
			t.Fatalf("no 4am row among %v", rows)
		}
		// The operator tunes the digest lane's row through the schedule
		// PATCH — exactly the state the carry exists to preserve.
		if _, err := sched.Update(ctx, digestLaneID, cloudsched.SchedulePatch{
			Vars: rvPtr(map[string]string{"open_mr": "yes"}),
		}); err != nil {
			t.Fatal(err)
		}
		// The enable dialog re-submits with a cron for the bot — here the
		// digest lane's own stored cron, a shape the studio's enable flow
		// can produce (it sends schedule_crons, and a parked request
		// replays them on approval).
		o.Now = func() time.Time { return t0.Add(time.Hour) }
		if _, err := o.Provision(ctx, ProvisionRequest{
			TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
			BotIDs: []string{"review-pr"}, ActorID: "u1",
			ScheduleCrons: map[string]string{"review-pr": "0 4 * * *"},
		}); err != nil {
			t.Fatalf("re-provision: %v", err)
		}
		rows2, err := sched.ListByIntegration(ctx, "t1", res.IntegrationID)
		if err != nil || len(rows2) != 2 {
			t.Fatalf("rows after = %v (err %v), want 2", rows2, err)
		}
		var digestRow, collectRow *cloudsched.ScheduledBot
		for i := range rows2 {
			switch rows2[i].ID {
			case digestLaneID:
				digestRow = &rows2[i]
			default:
				if rows2[i].Cron == "0 4 * * *" {
					collectRow = &rows2[i]
				}
			}
		}
		if digestRow == nil {
			t.Fatalf("the digest lane's row id did not survive the rebuild: %v", rows2)
		}
		if digestRow.Cron != "0 4 * * *" {
			t.Fatalf("attempt %d: the dialog cron was not applied (row cron %q)", attempt, digestRow.Cron)
		}
		if digestRow.Vars["mode"] != "digest" || digestRow.Vars["open_mr"] != "yes" {
			t.Fatalf("attempt %d: the 4am lane's row carries vars %v, want its own default digest merged with the operator's patch ({mode:digest open_mr:yes}) — the dialog cron handed its row to the other invocation", attempt, digestRow.Vars)
		}
		if collectRow == nil {
			t.Fatalf("the collect lane's row did not survive the rebuild: %v", rows2)
		}
		if len(collectRow.Vars) != 1 || collectRow.Vars["mode"] != "collect" {
			t.Fatalf("attempt %d: the 3am lane's row carries vars %v, want only its own default {mode:collect}", attempt, collectRow.Vars)
		}
	}
}
