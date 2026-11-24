package boardmongo_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	launchstore "github.com/SocialGouv/iterion/pkg/store"
)

// conformanceGrantSentinel is the bearer a writer tries to put on a card.
const conformanceGrantSentinel = "sentinel-forge-grant-2036-conformance"

// runCardGrantSuite answers, on BOTH twins, whether a writer can persist the
// publish grant a server mints for one run (launchstore.ServerMintedLaunchVars)
// in a card's bot args. The four writers — REST, the board MCP tools, `iterion
// issue`, trigger promote — all land in Create or Update, so the twins' own
// write boundary is the single place that answers; a twin that answered
// differently would hand a live bearer to whoever reads that board.
//
// Named class-wide rather than token-wide on purpose: the endpoints are minted
// WITH the bearer and say where it is spent, so a card that kept them would aim
// the launch's own fresh grant at an address of the writer's choosing.
func runCardGrantSuite(t *testing.T, store native.BoardStore) {
	t.Helper()

	sent := map[string]string{"loop_cap": "5"}
	for _, name := range launchstore.ServerMintedLaunchVars {
		sent[name] = conformanceGrantSentinel
	}
	created, err := store.Create(native.Issue{Title: "a writer sends the grant", State: native.StateReady, BotArgs: sent})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stored, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, where := range []struct {
		what string
		iss  *native.Issue
	}{{"the record Create returned", created}, {"the persisted card", stored}} {
		for _, name := range launchstore.ServerMintedLaunchVars {
			if got, ok := where.iss.BotArgs[name]; ok {
				t.Errorf("%s carries the minted var %s = %q", where.what, name, got)
			}
		}
		if where.iss.BotArgs["loop_cap"] != "5" {
			t.Errorf("%s lost the writer's own arg: %v", where.what, where.iss.BotArgs)
		}
	}

	// Update is the other half of the boundary: the studio's editor and the
	// board tools send the whole map back.
	next := map[string]string{"loop_cap": "7", launchstore.ForgePublishTokenVar: conformanceGrantSentinel}
	updated, err := store.Update(created.ID, native.Patch{BotArgs: &next})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, ok := updated.BotArgs[launchstore.ForgePublishTokenVar]; ok {
		t.Errorf("Update persisted the minted var: %q", got)
	}
	if updated.BotArgs["loop_cap"] != "7" {
		t.Errorf("Update lost the writer's own arg: %v", updated.BotArgs)
	}
	after, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if got, ok := after.BotArgs[launchstore.ForgePublishTokenVar]; ok {
		t.Errorf("the updated card carries the minted var: %q", got)
	}

	// An EMPTY value under a minted name withdraws a grant instead of
	// granting one: a card saying "this bot does not publish" survives, on
	// both twins, or the launch would hand it a fresh grant instead.
	withdrawal := map[string]string{launchstore.ForgePublishTokenVar: ""}
	withdrawn, err := store.Update(created.ID, native.Patch{BotArgs: &withdrawal})
	if err != nil {
		t.Fatalf("Update withdrawal: %v", err)
	}
	got, ok := withdrawn.BotArgs[launchstore.ForgePublishTokenVar]
	if !ok || got != "" {
		t.Errorf("the withdrawal was dropped: got %q, present=%v", got, ok)
	}
}
