package forge

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The integration's routing block (ADR-129): the binding level for every
// launch the integration makes, carried on the record beside LaunchVars
// because Provision rebuilds the whole webhook config. Four projections
// must see it: the webhook copy, the scheduled rows, verifyOperatorSettings
// and the org-approval park/apply (the last one tested at the HTTP edge in
// pkg/server).

func routingTestBlock(backend string) *llmroute.Policy {
	return &llmroute.Policy{
		Overrides: []llmroute.Override{{Selector: "*", Backend: backend}},
	}
}

// TestProvision_RoutingChanges_Clears_AndSurvives walks the three
// directions of the three-way rule on ONE repo: a CHANGED block reaches
// both stores; an UNSPECIFIED block survives a re-provision; a CLEARED
// (null) block clears both stores and nothing resurrects it.
func TestProvision_RoutingChanges_Clears_AndSurvives(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	ctx := context.Background()

	provision := func(req ProvisionRequest) (ProvisionResult, error) {
		req.TenantID, req.ConnectionID, req.RepoFullName, req.BotIDs, req.ActorID =
			"t1", conn.ID, "group/api", []string{"review-pr"}, "tester"
		return o.Provision(ctx, req)
	}
	readBoth := func(phase string) (RepoIntegration, webhooks.Config) {
		t.Helper()
		// Read by conn/repo: the integration id is stable, the webhook is
		// reused on updates and re-created after a clear (the token re-mints
		// but the webhookID carries) — the conn/repo lookup cannot go stale.
		ri, err := o.Integrations.GetByConnRepo(ctx, "t1", conn.ID, "group/api")
		if err != nil {
			t.Fatalf("%s: read integration: %v", phase, err)
		}
		cfg, err := o.Webhooks.Get(ctx, ri.WebhookID)
		if err != nil {
			t.Fatalf("%s: read webhook: %v", phase, err)
		}
		return ri, cfg
	}
	assertRouting := func(phase string, want *llmroute.Policy) {
		t.Helper()
		ri, cfg := readBoth(phase)
		if !reflect.DeepEqual(ri.Routing, want) {
			t.Errorf("%s: integration routing = %+v, want %+v", phase, ri.Routing, want)
		}
		if !reflect.DeepEqual(cfg.Routing, want) {
			t.Errorf("%s: webhook routing = %+v, want %+v — the config is the ENFORCEMENT half", phase, cfg.Routing, want)
		}
	}

	// CHANGED: the request's block replaces what the stores held (nothing)
	// and reaches the webhook config — the enforcement half carries what
	// the record reports.
	claw := routingTestBlock("claw")
	if _, err := provision(ProvisionRequest{Routing: claw, RoutingSet: true}); err != nil {
		t.Fatalf("provision with a routing block: %v", err)
	}
	assertRouting("changed: set from nothing", claw)

	// UNSPECIFIED: a re-provision that says nothing about routing keeps the
	// stored block — enabling one more bot must not drop the repo's pin.
	if _, err := provision(ProvisionRequest{}); err != nil {
		t.Fatalf("silent re-provision: %v", err)
	}
	assertRouting("unspecified: preserved", claw)

	// UNSPECIFIED riding a REBUILD: adding a bot in the same request takes
	// the full-rebuild path, where the resolution adopts the stored block
	// into the literal — the carry must have nothing to fill.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr", "revi-converse"}, ActorID: "tester",
	}); err != nil {
		t.Fatalf("silent rebuild: %v", err)
	}
	assertRouting("unspecified: survives the rebuild that carried it", claw)

	// CLEARED: the explicit null (RoutingSet + nil policy) clears BOTH
	// stores — and the carry must not resurrect it on the next write.
	if _, err := provision(ProvisionRequest{RoutingSet: true}); err != nil {
		t.Fatalf("clear provision: %v", err)
	}
	assertRouting("cleared: both stores", nil)

	// A silent re-provision after the clear stays clear: the carry reads a
	// nil block from BOTH stores, so it has nothing to resurrect.
	if _, err := provision(ProvisionRequest{}); err != nil {
		t.Fatalf("silent re-provision after clear: %v", err)
	}
	assertRouting("cleared: survives the next silent provision", nil)

	// And the block can be set again — through the idempotent path AND the
	// rebuild path (write-once would fail here: the rebuild's carry would
	// overwrite the request's new block with prev's).
	if _, err := provision(ProvisionRequest{Routing: routingTestBlock("claude_code"), RoutingSet: true}); err != nil {
		t.Fatalf("re-set after clear: %v", err)
	}
	assertRouting("changed: set again after a clear", routingTestBlock("claude_code"))
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr", "revi-converse", "dep-guard"}, ActorID: "tester",
		Routing: routingTestBlock("codex"), RoutingSet: true,
	}); err != nil {
		t.Fatalf("changed block on a rebuild: %v", err)
	}
	assertRouting("changed: the rebuild's new block wins over prev", routingTestBlock("codex"))

	// THE clear that rides a REBUILD — the write-once shape itself: the pin
	// is SET and one request speaks null AND changes the bot set (the
	// Replace unbind, the shape the approval apply replays), so the
	// full-rebuild path runs with prev.Routing still holding the old block.
	// The clear must land on BOTH stores: a carry that fills the literal's
	// nil from prev resurrects the pin on the enforcement half while the
	// record reports it cleared (and the next silent re-provision then
	// refuses on the divergence its own bug created).
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr", "revi-converse"}, ActorID: "tester",
		Replace:    true,
		RoutingSet: true,
	}); err != nil {
		t.Fatalf("clear riding a rebuild: %v", err)
	}
	assertRouting("cleared: the rebuild that carried the clear", nil)
}

// TestSyncSchedules_CarriesRoutingAndRetry: a schedule row's own routing
// block (the schedule PATCH's voice) and its retry tuning survive the next
// `bots enable` — syncSchedules rebuilds every row from a literal, so
// anything the carry list omits is erased in silence.
func TestSyncSchedules_CarriesRoutingAndRetry(t *testing.T) {
	mem := cloudsched.NewMemoryStore()
	now := time.Unix(1700000000, 0).UTC()
	o := &Orchestrator{
		Schedules: mem,
		Now:       func() time.Time { return now },
		NewID:     func() string { return "s1" },
	}
	inv := map[string][]bundle.Invocation{
		"docs-refresh": {{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 3 * * *"}}},
	}
	ctx := context.Background()
	if err := o.syncSchedules(ctx, "t1", "ri-1", "https://github.com/org/app.git", inv, nil, "u1", nil); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	rows, _ := mem.ListByIntegration(ctx, "t1", "ri-1")
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}

	// The operator tunes the row through the schedule PATCH (its own
	// routing block + retry policy) — then re-enables the repo's bots.
	own := &llmroute.Policy{Overrides: []llmroute.Override{{Selector: "judge", Backend: "claw"}}}
	maxAttempts := 4
	cron := rows[0].Cron
	patch := cloudsched.SchedulePatch{
		UpdatedAt:        now.Add(time.Minute),
		RetryMaxAttempts: &maxAttempts,
	}
	patch.Routing = own
	if _, err := mem.Update(ctx, rows[0].ID, patch); err != nil {
		t.Fatalf("patch the row: %v", err)
	}

	if err := o.syncSchedules(ctx, "t1", "ri-1", "https://github.com/org/app.git", inv, nil, "u1", nil); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	rows2, _ := mem.ListByIntegration(ctx, "t1", "ri-1")
	if len(rows2) != 1 {
		t.Fatalf("re-sync: want 1 row, got %d", len(rows2))
	}
	got := rows2[0]
	if !reflect.DeepEqual(got.Routing, own) {
		t.Errorf("the row's own routing block was erased by the re-provision: got %+v, want %+v", got.Routing, own)
	}
	if got.RetryMaxAttempts != maxAttempts {
		t.Errorf("retry_max_attempts = %d, want %d — the same carry class, erased in silence too", got.RetryMaxAttempts, maxAttempts)
	}
	if got.Cron != cron {
		t.Errorf("cron changed on re-sync: %q → %q", cron, got.Cron)
	}
}

// TestProvision_ProjectsIntegrationRoutingOntoSchedules: a pinned
// integration pins its SCHEDULED bots too — the scheduled row is the only
// layer a scheduled launch resolves, so the integration's block must reach
// the ScheduledBot literal when the row carries none of its own.
func TestProvision_ProjectsIntegrationRoutingOntoSchedules(t *testing.T) {
	mem := cloudsched.NewMemoryStore()
	var idc int
	o, _, _ := newTestOrch(t)
	o.Schedules = mem
	o.Invocations = func(_ context.Context, _, botID string) ([]bundle.Invocation, error) {
		return []bundle.Invocation{
			{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 5 * * *"}},
			{Kind: bundle.InvocationKindCommand, Mode: bundle.ExecutionMode("board"), ArgsVar: "prompt", Command: &bundle.InvocationCommand{Name: botID + "-cmd"}},
		}, nil
	}
	o.NewID = func() string { idc++; return "id-" + string(rune('a'+idc)) }
	conn := seedConn(t, o, o.Sealer)

	block := &llmroute.Policy{Overrides: []llmroute.Override{{Selector: "*", Backend: "claw"}}}
	res, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "tester",
		Routing: block, RoutingSet: true,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	rows, _ := mem.ListByIntegration(context.Background(), "t1", res.IntegrationID)
	if len(rows) != 1 {
		t.Fatalf("want 1 schedule row, got %d", len(rows))
	}
	if !reflect.DeepEqual(rows[0].Routing, block) {
		t.Errorf("the integration's routing block never reached the scheduled row: got %+v, want %+v — a pinned integration leaves its scheduled bots unpinned", rows[0].Routing, block)
	}
}

// TestProvision_RoutingOnlyPatch_ReachesTheScheduledRows: a routing-ONLY
// update (same bots, no crons spoken) is a real mutation for the scheduled
// lane — the scheduled row is the only layer a scheduled launch resolves,
// so the no-op branch's operator-settings write must re-project the rows
// with the block it just landed, in BOTH directions (replace and clear).
func TestProvision_RoutingOnlyPatch_ReachesTheScheduledRows(t *testing.T) {
	mem := cloudsched.NewMemoryStore()
	o, _, _ := newTestOrch(t)
	o.Schedules = mem
	o.Invocations = func(_ context.Context, _, botID string) ([]bundle.Invocation, error) {
		return []bundle.Invocation{
			{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 5 * * *"}},
		}, nil
	}
	conn := seedConn(t, o, o.Sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "tester",
		Routing: routingTestBlock("claw"), RoutingSet: true,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	rowRouting := func(phase string) *llmroute.Policy {
		t.Helper()
		rows, lerr := mem.ListByIntegration(ctx, "t1", res.IntegrationID)
		if lerr != nil || len(rows) != 1 {
			t.Fatalf("%s: list rows: %v (%d rows)", phase, lerr, len(rows))
		}
		return rows[0].Routing
	}
	if got := rowRouting("after create"); !reflect.DeepEqual(got, routingTestBlock("claw")) {
		t.Fatalf("after create: row routing = %+v, want the claw projection", got)
	}

	// Replace through a routing-only PATCH: the webhook and the record move
	// to codex, the nightly must move with them.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "tester",
		Routing: routingTestBlock("codex"), RoutingSet: true,
	}); err != nil {
		t.Fatalf("routing-only replace: %v", err)
	}
	if got := rowRouting("after replace"); !reflect.DeepEqual(got, routingTestBlock("codex")) {
		t.Fatalf("after replace: row routing = %+v, want codex — the nightly keeps enforcing the pin the record just replaced", got)
	}

	// Clear through a routing-only PATCH: the nightly must stop pinning too.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "tester",
		RoutingSet: true,
	}); err != nil {
		t.Fatalf("routing-only clear: %v", err)
	}
	if got := rowRouting("after clear"); got != nil {
		t.Fatalf("after clear: row routing = %+v, want nil — the nightly keeps enforcing the pin the record just cleared", got)
	}
}

// TestSyncSchedules_ProjectedRowsFollowTheIntegration: a row whose block the
// PROJECTION wrote must follow every later integration write — clear and
// replace alike — while a row the operator spoke through the schedule PATCH
// keeps theirs. Without provenance the first projection is a snapshot no
// later provision can repair.
func TestSyncSchedules_ProjectedRowsFollowTheIntegration(t *testing.T) {
	mem := cloudsched.NewMemoryStore()
	now := time.Unix(1700000000, 0).UTC()
	o := &Orchestrator{
		Schedules: mem,
		Now:       func() time.Time { return now },
		NewID:     func() string { return "s1" },
	}
	inv := map[string][]bundle.Invocation{
		"docs-refresh": {{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 3 * * *"}}},
	}
	ctx := context.Background()
	sync := func(integRouting *llmroute.Policy) {
		t.Helper()
		if err := o.syncSchedules(ctx, "t1", "ri-1", "https://github.com/org/app.git", inv, nil, "u1", integRouting); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	rowRouting := func(phase string) *llmroute.Policy {
		t.Helper()
		rows, err := mem.ListByIntegration(ctx, "t1", "ri-1")
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: list rows: %v (%d rows)", phase, err, len(rows))
		}
		return rows[0].Routing
	}

	sync(routingTestBlock("claw"))
	if got := rowRouting("projected"); !reflect.DeepEqual(got, routingTestBlock("claw")) {
		t.Fatalf("projected: row routing = %+v, want claw", got)
	}

	// The integration CLEARS: the projected copy must clear with it.
	sync(nil)
	if got := rowRouting("after integration clear"); got != nil {
		t.Fatalf("after integration clear: row routing = %+v, want nil — the projection's own copy shadows the operator's clear", got)
	}

	// The integration REPLACES: the projected copy must replace with it.
	sync(routingTestBlock("codex"))
	if got := rowRouting("after integration replace"); !reflect.DeepEqual(got, routingTestBlock("codex")) {
		t.Fatalf("after integration replace: row routing = %+v, want codex", got)
	}

	// A row the OPERATOR spoke through the schedule PATCH keeps its block —
	// the row is the more specific voice, and the integration must not
	// overwrite it in either direction.
	own := &llmroute.Policy{Overrides: []llmroute.Override{{Selector: "judge", Backend: "claw"}}}
	rows, _ := mem.ListByIntegration(ctx, "t1", "ri-1")
	patch := cloudsched.SchedulePatch{UpdatedAt: now.Add(time.Minute)}
	patch.Routing = own
	if _, err := mem.Update(ctx, rows[0].ID, patch); err != nil {
		t.Fatalf("operator row patch: %v", err)
	}
	sync(routingTestBlock("dep-guard"))
	if got := rowRouting("operator block survives a replace"); !reflect.DeepEqual(got, own) {
		t.Fatalf("operator block survives a replace: row routing = %+v, want the operator's own block", got)
	}
	sync(nil)
	if got := rowRouting("operator block survives a clear"); !reflect.DeepEqual(got, own) {
		t.Fatalf("operator block survives a clear: row routing = %+v, want the operator's own block", got)
	}
}

// TestVerifyOperatorSettings_RefusesARoutingDivergence: the verify pass
// certifies both stores field by field; a routing block the record does not
// read back is the same ErrProvisionDiverged class as any other dropped
// operator field.
func TestVerifyOperatorSettings_RefusesARoutingDivergence(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "tester",
		Routing: routingTestBlock("claw"), RoutingSet: true,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	integ, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	// The WANT side is what the provision wrote; copies, because the tamper
	// below mutates the STORED rows, not these.
	wantInteg, wantCfg := integ, cfg

	// Tamper the record behind the verify pass's back: the report no longer
	// says what was written.
	tampered := routingTestBlock("claude_code")
	integ.Routing = tampered
	if err := o.Integrations.Update(ctx, integ); err != nil {
		t.Fatal(err)
	}
	err = o.verifyOperatorSettings(ctx, wantInteg, wantCfg)
	if !errors.Is(err, ErrProvisionDiverged) {
		t.Fatalf("verifyOperatorSettings = %v, want ErrProvisionDiverged — a routing block the record does not read back must fail the provision, not certify it", err)
	}

	// Same on the webhook half: the want side is the written value (the
	// claw block the provision stamped), the stored side the tampered one.
	fresh, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Routing = routingTestBlock("codex")
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	err = o.verifyOperatorSettings(ctx, fresh, wantCfg)
	if !errors.Is(err, ErrProvisionDiverged) {
		t.Fatalf("verifyOperatorSettings (webhook half) = %v, want ErrProvisionDiverged — the wrong block was certified as written", err)
	}
}
