package trigger

import (
	"context"
	"testing"
	"time"
)

// TestSubscriptionStoreQueries locks the two query primitives the studio's
// repo-scoped and bot-scoped Automations views are built on — ListByRepo and
// ListByBot — plus the tenant boundary every store method owes.
//
// Their contracts are asymmetric on purpose and that asymmetry is the part a
// refactor silently breaks: ListByRepo also returns the WORKSPACE-WIDE
// subscriptions (Repo == ""), because a subscription with no repo fires for
// every repo of the tenant; ListByBot does not — a bot-scoped view asks for
// exactly that bot. Dropping either filter, or dropping the tenant scope,
// leaks another tenant's automations into the answer.
func TestSubscriptionStoreQueries(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySubscriptionStore()

	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	seed := []Subscription{
		{ID: "s1", TenantID: "t1", Repo: "acme/api", BotID: "revi", CreatedAt: base},
		{ID: "s2", TenantID: "t1", Repo: "acme/api", BotID: "billy", CreatedAt: base.Add(time.Minute)},
		{ID: "s3", TenantID: "t1", Repo: "acme/web", BotID: "revi", CreatedAt: base.Add(2 * time.Minute)},
		// Workspace-wide: no repo, so it applies to every repo of t1.
		{ID: "s4", TenantID: "t1", Repo: "", BotID: "nexie", CreatedAt: base.Add(3 * time.Minute)},
		// Another tenant, same repo and bot names.
		{ID: "s5", TenantID: "t2", Repo: "acme/api", BotID: "revi", CreatedAt: base.Add(4 * time.Minute)},
	}
	for _, s := range seed {
		if err := store.Create(ctx, s); err != nil {
			t.Fatalf("create %s: %v", s.ID, err)
		}
	}

	ids := func(subs []Subscription) []string {
		out := make([]string, 0, len(subs))
		for _, s := range subs {
			out = append(out, s.ID)
		}
		return out
	}
	equal := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	t.Run("ListByRepo returns the repo's own plus the workspace-wide ones", func(t *testing.T) {
		got, err := store.ListByRepo(ctx, "t1", "acme/api")
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		// Ordered by CreatedAt; s4 is workspace-wide so it applies here too,
		// and s5 belongs to another tenant.
		if want := []string{"s1", "s2", "s4"}; !equal(ids(got), want) {
			t.Fatalf("ListByRepo(t1, acme/api) = %v, want %v", ids(got), want)
		}
	})

	t.Run("ListByRepo excludes another repo's subscriptions", func(t *testing.T) {
		got, err := store.ListByRepo(ctx, "t1", "acme/web")
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		if want := []string{"s3", "s4"}; !equal(ids(got), want) {
			t.Fatalf("ListByRepo(t1, acme/web) = %v, want %v", ids(got), want)
		}
	})

	t.Run("ListByBot returns only that bot, never the workspace-wide ones", func(t *testing.T) {
		got, err := store.ListByBot(ctx, "t1", "revi")
		if err != nil {
			t.Fatalf("ListByBot: %v", err)
		}
		if want := []string{"s1", "s3"}; !equal(ids(got), want) {
			t.Fatalf("ListByBot(t1, revi) = %v, want %v", ids(got), want)
		}
	})

	t.Run("both queries are tenant-scoped", func(t *testing.T) {
		byRepo, err := store.ListByRepo(ctx, "t2", "acme/api")
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		if want := []string{"s5"}; !equal(ids(byRepo), want) {
			t.Fatalf("ListByRepo(t2, acme/api) = %v, want %v — another tenant's rows leaked", ids(byRepo), want)
		}
		byBot, err := store.ListByBot(ctx, "t2", "revi")
		if err != nil {
			t.Fatalf("ListByBot: %v", err)
		}
		if want := []string{"s5"}; !equal(ids(byBot), want) {
			t.Fatalf("ListByBot(t2, revi) = %v, want %v — another tenant's rows leaked", ids(byBot), want)
		}
	})

	t.Run("an unknown repo or bot answers empty, not everything", func(t *testing.T) {
		if got, _ := store.ListByRepo(ctx, "t1", "acme/nope"); len(got) != 1 || got[0].ID != "s4" {
			t.Fatalf("ListByRepo(t1, acme/nope) = %v, want just the workspace-wide s4", ids(got))
		}
		if got, _ := store.ListByBot(ctx, "t1", "ghost"); len(got) != 0 {
			t.Fatalf("ListByBot(t1, ghost) = %v, want none", ids(got))
		}
	})

	t.Run("a deleted subscription leaves both queries", func(t *testing.T) {
		if err := store.Delete(ctx, "s1"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if got, _ := store.ListByRepo(ctx, "t1", "acme/api"); !equal(ids(got), []string{"s2", "s4"}) {
			t.Fatalf("after delete ListByRepo = %v, want [s2 s4]", ids(got))
		}
		if got, _ := store.ListByBot(ctx, "t1", "revi"); !equal(ids(got), []string{"s3"}) {
			t.Fatalf("after delete ListByBot = %v, want [s3]", ids(got))
		}
	})
}
