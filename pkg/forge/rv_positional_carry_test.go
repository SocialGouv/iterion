package forge

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
)

// The carry queue under production ids: o.id() is a random uuid, so the
// prior rows of ONE batch share CreatedAt and no id ordering recovers
// which row served which invocation - only the row's own cadence does (a
// row carries the cron its invocation resolved to when syncSchedules
// built it). Two schedule invocations with different default_vars; the
// operator tunes a key neither default names on the 3am row. After a
// routing-only re-provision each row must still carry its OWN
// invocation's defaults, on however many attempts the store's listing
// order happens to shuffle them: the memory store iterates a map, so a
// rebuild that pairs positionally flips the rows on roughly half the
// attempts, and the corruption sticks (the wrong lane's default reads
// back as operator tuning forever after).
func TestRV_PositionalCarry_MergesEachRowsOwnDefaults(t *testing.T) {
	ctx := context.Background()
	for attempt := 0; attempt < 40; attempt++ {
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
		// Tune the 3am row with a key NEITHER default names.
		for _, r := range rows {
			if r.Cron == "0 3 * * *" {
				if _, err := sched.Update(ctx, r.ID, cloudsched.SchedulePatch{
					Vars: rvPtr(map[string]string{"open_mr": "yes"}),
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		// A routing-only write re-provisions (the no-op branch re-syncs).
		o.Now = func() time.Time { return t0.Add(time.Hour) }
		if _, err := o.Provision(ctx, ProvisionRequest{
			TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
			BotIDs: []string{"review-pr"}, ActorID: "u1",
			Routing: routingTestBlock("claw"), RoutingSet: true,
		}); err != nil {
			t.Fatalf("re-provision: %v", err)
		}
		rows2, err := sched.ListByIntegration(ctx, "t1", res.IntegrationID)
		if err != nil || len(rows2) != 2 {
			t.Fatalf("rows after = %v (err %v), want 2", rows2, err)
		}
		for _, r := range rows2 {
			want := "collect"
			if r.Cron == "0 4 * * *" {
				want = "digest"
			}
			if r.Vars["mode"] != want {
				t.Fatalf("attempt %d: the %q row carries mode %q, want %q (vars %v) — the carry merged it with the OTHER invocation's defaults", attempt, r.Cron, r.Vars["mode"], want, r.Vars)
			}
		}
	}
}

// Two invocations sharing one cadence leave the store no witness of which
// row served which - the first pairing of such a batch is a choice the
// store cannot inform (no invocation identity travels on a row). What the
// carry can guarantee is CONVERGENCE: once a pairing is established, a
// re-provision reproduces it instead of re-flipping it on every routing
// write (the rows' ids persist; a listing order that changes per call
// would not). The defaults are disjoint keys, so a re-flip pollutes both
// rows' vars and is visible.
func TestRV_PositionalCarry_SameCronRows_KeepTheirTuningAcrossResyncs(t *testing.T) {
	ctx := context.Background()
	for attempt := 0; attempt < 4; attempt++ {
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
						SuggestedCron: "0 3 * * *", DefaultVars: map[string]string{"lane": "nightly"},
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
		pairing := map[string]map[string]string{}
		capture := func(rows []cloudsched.ScheduledBot) {
			for _, r := range rows {
				pairing[r.ID] = r.Vars
			}
		}
		compare := func(rows []cloudsched.ScheduledBot, resync int) {
			for _, r := range rows {
				if !maps.Equal(pairing[r.ID], r.Vars) {
					t.Fatalf("attempt %d resync %d: row %s flipped vars %v -> %v - the carry re-paired rows it had already paired", attempt, resync, r.ID, pairing[r.ID], r.Vars)
				}
			}
		}
		for resync := 0; resync < 25; resync++ {
			o.Now = func() time.Time { return t0.Add(time.Duration(resync+1) * time.Hour) }
			// Alternate the spoken block: an identical repeated write is a
			// recognized no-op that never reaches syncSchedules, and this
			// witness is about the resyncs that DO.
			block := "claw"
			if resync%2 == 1 {
				block = "byo"
			}
			if _, err := o.Provision(ctx, ProvisionRequest{
				TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
				BotIDs: []string{"review-pr"}, ActorID: "u1",
				Routing: routingTestBlock(block), RoutingSet: true,
			}); err != nil {
				t.Fatalf("resync %d: %v", resync, err)
			}
			rows2, err := sched.ListByIntegration(ctx, "t1", res.IntegrationID)
			if err != nil || len(rows2) != 2 {
				t.Fatalf("resync %d: rows = %v (err %v), want 2", resync, rows2, err)
			}
			if resync == 0 {
				capture(rows2) // the first pairing of a same-cadence batch is the store's choice
			} else {
				compare(rows2, resync)
			}
		}
	}
}
