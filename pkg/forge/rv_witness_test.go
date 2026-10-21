package forge

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

func rvPtr[T any](v T) *T { return &v }

// The no-op provision branch now re-syncs schedules on ANY operator-settings
// change (changed || len(req.ScheduleCrons) > 0). The carry must therefore
// re-establish EVERY operator-owned row field, or a routing/vars write
// becomes a silent drop. This witness pins NextFireAt and StaleAfter —
// both operator-owned through the schedule PATCH (SchedulePatch.NextFireAt,
// SchedulePatch.StaleAfter), both absent from the carry list.
func TestRV_RoutingOnlyProvision_PreservesOperatorRowNextFireAndStaleAfter(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	sched := cloudsched.NewMemoryStore()
	o.Schedules = sched
	o.Invocations = func(_ context.Context, _, botID string) ([]bundle.Invocation, error) {
		if botID == "review-pr" {
			return []bundle.Invocation{{
				Kind:     bundle.InvocationKindSchedule,
				Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 3 * * *"},
			}}, nil
		}
		return nil, nil
	}

	provision := func(routing *llmroute.Policy, routingSet bool) ProvisionResult {
		t.Helper()
		res, err := o.Provision(ctx, ProvisionRequest{
			TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
			BotIDs: []string{"review-pr"}, ActorID: "u1",
			Routing: routing, RoutingSet: routingSet,
		})
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		return res
	}

	t0 := time.Unix(1700000000, 0).UTC() // 2023-11-14 22:13:20 UTC
	o.Now = func() time.Time { return t0 }
	res := provision(nil, false)

	rows, err := sched.ListByIntegration(ctx, "t1", res.IntegrationID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows after provision = %v (err %v), want 1", rows, err)
	}
	row := rows[0]

	// The row is pinned out-of-band through the store's Update (which takes
	// a next-fire verbatim — the ticker's ClaimTick writes the same shape
	// between fires) plus a keepalive silence cutoff: the carried state the
	// rebuild must respect.
	pinned := t0.Add(96 * time.Hour)
	if _, err := sched.Update(ctx, row.ID, cloudsched.SchedulePatch{
		NextFireAt: rvPtr(pinned),
		StaleAfter: rvPtr("2h"),
	}); err != nil {
		t.Fatalf("operator patch: %v", err)
	}

	// An hour later, a routing-only write (the binding pin) re-provisions.
	o.Now = func() time.Time { return t0.Add(time.Hour) }
	provision(routingTestBlock("claw"), true)

	rows2, err := sched.ListByIntegration(ctx, "t1", res.IntegrationID)
	if err != nil || len(rows2) != 1 {
		t.Fatalf("rows after routing write = %v (err %v), want 1", rows2, err)
	}
	after := rows2[0]
	if !after.NextFireAt.Equal(pinned) {
		t.Errorf("next fire = %v, want the operator's pinned %v — a routing write must not postpone it", after.NextFireAt, pinned)
	}
	if after.StaleAfter != "2h" {
		t.Errorf("stale_after = %q, want the operator's \"2h\" — a routing write must not reset it", after.StaleAfter)
	}
}

// The same drop class, the other two operator-owned row fields: a row the
// operator converted to keepalive (interval_seconds in, cron out — the
// schedule PATCH's exclusive cadence switch) and a pinned checkout ref
// survive the rebuild as a unit. A rebuild that came out cron-driven, or
// dropped the ref, would silently change what the row launches.
func TestRV_ProvisionRebuild_KeepsAKeepaliveRowKeepalive(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	sched := cloudsched.NewMemoryStore()
	o.Schedules = sched
	o.Invocations = func(_ context.Context, _, botID string) ([]bundle.Invocation, error) {
		if botID == "review-pr" {
			return []bundle.Invocation{{
				Kind:     bundle.InvocationKindSchedule,
				Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 3 * * *"},
			}}, nil
		}
		return nil, nil
	}

	t0 := time.Unix(1700000000, 0).UTC() // 2023-11-14 22:13:20 UTC
	o.Now = func() time.Time { return t0 }
	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	rows, err := sched.ListByIntegration(ctx, "t1", res.IntegrationID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows after provision = %v (err %v), want 1", rows, err)
	}
	row := rows[0]

	// The operator converts the row to keepalive through the schedule
	// PATCH's exclusive cadence switch and pins a checkout ref.
	iv := 30
	next := t0.Add(30 * time.Second)
	empty := ""
	if _, err := sched.Update(ctx, row.ID, cloudsched.SchedulePatch{
		IntervalSeconds: &iv,
		Cron:            &empty,
		NextFireAt:      &next,
		RepoRef:         rvPtr("refs/heads/release"),
	}); err != nil {
		t.Fatalf("operator patch: %v", err)
	}

	// An hour later, a routing-only write re-provisions.
	o.Now = func() time.Time { return t0.Add(time.Hour) }
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
		Routing: routingTestBlock("claw"), RoutingSet: true,
	}); err != nil {
		t.Fatalf("re-provision: %v", err)
	}

	rows2, err := sched.ListByIntegration(ctx, "t1", res.IntegrationID)
	if err != nil || len(rows2) != 1 {
		t.Fatalf("rows after rebuild = %v (err %v), want 1", rows2, err)
	}
	after := rows2[0]
	if after.IntervalSeconds != 30 || after.Cron != "" {
		t.Errorf("cadence after rebuild = cron %q interval %d, want the operator's keepalive (interval 30, no cron)", after.Cron, after.IntervalSeconds)
	}
	if after.RepoRef != "refs/heads/release" {
		t.Errorf("repo_ref = %q, want the operator's pinned ref", after.RepoRef)
	}
	if !after.NextFireAt.Equal(next) {
		t.Errorf("next fire = %v, want the keepalive row's stored %v", after.NextFireAt, next)
	}
}

// The other edge of the cadence rule: an explicit enable-dialog cron spoken
// OVER a keepalive row replaces the cadence wholesale — the row comes out
// cron-driven (interval zeroed, next fire computed from the spoken cron),
// never cron AND interval.
func TestRV_EnableDialogCron_ReplacesAKeepaliveRowCadence(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	sched := cloudsched.NewMemoryStore()
	o.Schedules = sched
	o.Invocations = func(_ context.Context, _, botID string) ([]bundle.Invocation, error) {
		if botID == "review-pr" {
			return []bundle.Invocation{{
				Kind:     bundle.InvocationKindSchedule,
				Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 3 * * *"},
			}}, nil
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
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows after provision = %v (err %v), want 1", rows, err)
	}

	// Operator converts the row to keepalive.
	iv := 30
	next := t0.Add(30 * time.Second)
	empty := ""
	if _, err := sched.Update(ctx, rows[0].ID, cloudsched.SchedulePatch{
		IntervalSeconds: &iv, Cron: &empty, NextFireAt: &next,
	}); err != nil {
		t.Fatalf("operator patch: %v", err)
	}

	// Re-provision with an explicit cron override for the bot.
	t1 := t0.Add(time.Hour)
	o.Now = func() time.Time { return t1 }
	spoken := "*/5 * * * *"
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
		ScheduleCrons: map[string]string{"review-pr": spoken},
	}); err != nil {
		t.Fatalf("re-provision: %v", err)
	}

	rows2, err := sched.ListByIntegration(ctx, "t1", res.IntegrationID)
	if err != nil || len(rows2) != 1 {
		t.Fatalf("rows after override = %v (err %v), want 1", rows2, err)
	}
	after := rows2[0]
	if after.Cron != spoken || after.IntervalSeconds != 0 {
		t.Errorf("cadence after spoken override = cron %q interval %d, want cron-only %q", after.Cron, after.IntervalSeconds, spoken)
	}
	wantNext, err := cloudsched.NextFire(spoken, t1)
	if err != nil {
		t.Fatal(err)
	}
	if !after.NextFireAt.Equal(wantNext) {
		t.Errorf("next fire = %v, want the spoken cron's %v", after.NextFireAt, wantNext)
	}
}
