package forge

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
)

func invLookup(m map[string][]bundle.Invocation) BotInvocationsLookup {
	return func(botID string) ([]bundle.Invocation, error) { return m[botID], nil }
}

func cmdInv(name, mode, argsVar, disamb string, aliases ...string) bundle.Invocation {
	return bundle.Invocation{
		Kind:    bundle.InvocationKindCommand,
		Mode:    bundle.ExecutionMode(mode),
		ArgsVar: argsVar,
		Command: &bundle.InvocationCommand{Name: name, Aliases: aliases, Disambiguator: disamb},
	}
}

func TestBuildCommandMap_SingleBotAndAliases(t *testing.T) {
	o := &Orchestrator{Invocations: invLookup(map[string][]bundle.Invocation{
		"feature-dev": {cmdInv("featurly", "board", "feature_prompt", "", "feature-dev")},
	})}
	m, err := o.buildCommandMap([]string{"feature-dev"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, key := range []string{"featurly", "feature-dev"} {
		routes, ok := m[key]
		if !ok || len(routes) != 1 {
			t.Fatalf("key %q: want 1 route, got %v", key, routes)
		}
		if routes[0].BotID != "feature-dev" || routes[0].Mode != "board" || routes[0].ArgsVar != "feature_prompt" {
			t.Errorf("key %q route: %+v", key, routes[0])
		}
	}
}

func TestBuildCommandMap_ArgsDisambiguation(t *testing.T) {
	o := &Orchestrator{Invocations: invLookup(map[string][]bundle.Invocation{
		"review-pr":     {cmdInv("revi", "direct", "", "when_args_empty")},
		"revi-converse": {cmdInv("revi", "direct", "converse_question", "when_args_present")},
	})}
	m, err := o.buildCommandMap([]string{"review-pr", "revi-converse"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(m["revi"]); got != 2 {
		t.Fatalf("revi: want 2 disambiguated routes, got %d", got)
	}
}

func TestBuildCommandMap_CollisionRejected(t *testing.T) {
	o := &Orchestrator{Invocations: invLookup(map[string][]bundle.Invocation{
		"bot-a": {cmdInv("dup", "direct", "", "")},
		"bot-b": {cmdInv("dup", "direct", "", "")},
	})}
	_, err := o.buildCommandMap([]string{"bot-a", "bot-b"})
	if err == nil {
		t.Fatal("expected collision error for two bots claiming /dup without disambiguation")
	}
}

func TestBuildCommandMap_NilWhenNoCommands(t *testing.T) {
	// Lookup wired but the bot declares only a forge invocation.
	o := &Orchestrator{Invocations: invLookup(map[string][]bundle.Invocation{
		"review-pr": {{Kind: bundle.InvocationKindForge, Forge: &bundle.InvocationForge{Event: bundle.ForgeEventPullRequest}}},
	})}
	m, err := o.buildCommandMap([]string{"review-pr"})
	if err != nil || m != nil {
		t.Errorf("want nil map, got %v (err=%v)", m, err)
	}

	// No lookup wired at all → nil.
	if m, _ := (&Orchestrator{}).buildCommandMap([]string{"x"}); m != nil {
		t.Errorf("nil Invocations should yield nil map, got %v", m)
	}
}

// TestProvision_CommandOnlyBot proves a bot with NO forge: block but a command
// invocation is auto-provisionable: the webhook subscribes to the comment
// event (derived from the invocation) and the CommandMap routes the command.
func TestProvision_CommandOnlyBot(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	// "no-forge-bot" returns nil from testBotLookup (no forge: block); give it
	// a command invocation so it becomes forge-reachable.
	o.Invocations = func(botID string) ([]bundle.Invocation, error) {
		if botID == "no-forge-bot" {
			return []bundle.Invocation{
				{Kind: bundle.InvocationKindCommand, Mode: bundle.ExecutionBoard, ArgsVar: "feature_prompt",
					Command: &bundle.InvocationCommand{Name: "featurly", Scope: "any"}},
				{Kind: bundle.InvocationKindBoard},
			}, nil
		}
		return nil, nil
	}
	conn := seedConn(t, o, sealer)
	res, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "owner/repo",
		BotIDs: []string{"no-forge-bot"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("command-only bot should provision (forge: optional): %v", err)
	}
	cfg, err := o.Webhooks.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := ToNativeEvents(ProviderGitLab, []string{bundle.ForgeEventPullRequestComment})
	if !sameSet(cfg.EventAllowlist, wantEvents) {
		t.Errorf("event allowlist: want %v (derived from command invocation), got %v", wantEvents, cfg.EventAllowlist)
	}
	routes := cfg.CommandMap["featurly"]
	if len(routes) != 1 || routes[0].BotID != "no-forge-bot" || routes[0].Mode != "board" || routes[0].ArgsVar != "feature_prompt" {
		t.Errorf("command map for /featurly: %+v", cfg.CommandMap)
	}
}

// TestSyncSchedules_CreatesAndIsIdempotent: schedule invocations with a cron
// become ScheduledBot rows; a re-sync replaces (not duplicates) them; a
// schedule with no cron + non-schedule invocations are ignored.
func TestSyncSchedules_CreatesAndIsIdempotent(t *testing.T) {
	mem := cloudsched.NewMemoryStore()
	var idc int
	o := &Orchestrator{
		Schedules: mem,
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
		NewID:     func() string { idc++; return "sched-" + string(rune('a'+idc)) },
	}
	invByBot := map[string][]bundle.Invocation{
		"sec-audit-source": {
			{Kind: bundle.InvocationKindCommand, Command: &bundle.InvocationCommand{Name: "seki"}},
			{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 2 * * 1"}},
			{Kind: bundle.InvocationKindBoard},
		},
		"feature-dev": {
			{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{}}, // no cron → skipped
		},
	}
	ctx := context.Background()
	if err := o.syncSchedules(ctx, "t1", "ri-1", "https://github.com/org/app.git", invByBot, nil, "u1"); err != nil {
		t.Fatalf("syncSchedules: %v", err)
	}
	rows, _ := mem.ListByIntegration(ctx, "t1", "ri-1")
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 schedule row (only the cron'd one), got %d: %+v", len(rows), rows)
	}
	if rows[0].RepoURL != "https://github.com/org/app.git" {
		t.Fatalf("schedule must carry the repo clone URL (stateful repo-bound bots), got %q", rows[0].RepoURL)
	}
	if rows[0].BotID != "sec-audit-source" || rows[0].Cron != "0 2 * * 1" || rows[0].NextFireAt.IsZero() {
		t.Errorf("schedule row: %+v", rows[0])
	}

	// Re-sync replaces, doesn't duplicate.
	if err := o.syncSchedules(ctx, "t1", "ri-1", "https://github.com/org/app.git", invByBot, nil, "u1"); err != nil {
		t.Fatal(err)
	}
	rows2, _ := mem.ListByIntegration(ctx, "t1", "ri-1")
	if len(rows2) != 1 {
		t.Errorf("re-sync must not duplicate, got %d", len(rows2))
	}
}

// TestSyncSchedules_CronOverride: the operator's chosen cron wins over the
// manifest suggested_cron.
func TestSyncSchedules_CronOverride(t *testing.T) {
	mem := cloudsched.NewMemoryStore()
	o := &Orchestrator{
		Schedules: mem,
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
		NewID:     func() string { return "s1" },
	}
	invByBot := map[string][]bundle.Invocation{
		"sec-audit-source": {{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 2 * * 1"}}},
	}
	ctx := context.Background()
	if err := o.syncSchedules(ctx, "t1", "ri-1", "https://github.com/org/app.git", invByBot, map[string]string{"sec-audit-source": "30 9 * * *"}, "u1"); err != nil {
		t.Fatal(err)
	}
	rows, _ := mem.ListByIntegration(ctx, "t1", "ri-1")
	if len(rows) != 1 || rows[0].Cron != "30 9 * * *" {
		t.Errorf("operator cron override should win, got %+v", rows)
	}
}
