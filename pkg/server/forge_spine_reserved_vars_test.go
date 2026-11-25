package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// plantedLaunchContext is what a manifest (or a hand-written row) would plant
// under the server's reserved launch-context keys: another repository's
// secret, a key of its own, a ref of its choosing, the own-vars marker.
var plantedLaunchContext = map[string]string{
	"__iterion_secret_overrides": `{"forge_token":"sec-of-another-repo"}`,
	"__iterion_key_overrides":    `{"anthropic":"key-planted"}`,
	"__iterion_repo_ref":         "attacker-branch",
	"__iterion_repo_url":         "https://evil.example/fork.git",
	"__iterion_own_vars":         "planted",
}

// plantSpineRowVars writes the planted launch context into every row bound
// to the fixture's integration, the way an older planner or a hand edit of
// the store would: the router's own guard is then the only one between the
// row and the launch.
func plantSpineRowVars(t *testing.T, f *spineIssueFixture) {
	t.Helper()
	ctx := context.Background()
	rows, err := f.s.cfg.TriggerStore.ListByForgeBinding(ctx, f.cfg.TenantID, trigger.ForgeBindingForIntegration(spineIntegrationID))
	if err != nil || len(rows) == 0 {
		t.Fatalf("list the fixture's rows: %v (%d rows)", err, len(rows))
	}
	for _, row := range rows {
		if row.Vars == nil {
			row.Vars = map[string]string{}
		}
		for k, v := range plantedLaunchContext {
			row.Vars[k] = v
		}
		if err := f.s.cfg.TriggerStore.Update(ctx, row); err != nil {
			t.Fatalf("plant the vars of row %s: %v", row.ID, err)
		}
	}
}

// wantNoPlantedLaunchContext fails on any planted value that reached args.
func wantNoPlantedLaunchContext(t *testing.T, what string, args map[string]string) {
	t.Helper()
	for k, v := range plantedLaunchContext {
		if got, ok := args[k]; ok && got == v {
			t.Errorf("%s carries the planted %s=%q: a manifest var wrote the server's reserved launch context", what, k, got)
		}
	}
}

// R-F1, end to end: a manifest's context_vars named after the reserved
// launch-context keys never reach the spine card nor the launch it drives —
// the projection drops them (and reports it), the card builder drops them.
//
// Mutation: routeLaunchVars returns all instead of the filtered vars AND
// buildBoardCard skips its DropServerMintedLaunchVars → red on "the launch
// resolves the planted overrides".
func TestSpineIssueLabel_ManifestCannotPlantReservedLaunchContext(t *testing.T) {
	f := newSpineGitHubFixture(t,
		spineBotDecl{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur"},
			context: map[string]string{
				"__iterion_secret_overrides": `{\"forge_token\":\"sec-of-another-repo\"}`,
				"__iterion_key_overrides":    `{\"anthropic\":\"key-planted\"}`,
				"__iterion_repo_ref":         "attacker-branch",
				"keep":                       "kept",
			}},
	)
	w := f.deliver(f.body("saboteur"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202 carded", w.Code, w.Body.String())
	}
	cards := laneCards(t, f.board)
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(cards))
	}
	if cards[0].BotArgs["keep"] != "kept" {
		t.Fatalf("card BotArgs = %v, want the ordinary context var keep=kept carried", cards[0].BotArgs)
	}
	spec := driveSpineCard(t, "saboteur", cards[0].BotArgs, webhooks.Config{ID: cards[0].BotArgs[boardWebhookIDKey], TenantID: "t1"})
	if spec.SecretOverrides["forge_token"] == "sec-of-another-repo" || spec.KeyOverrides["anthropic"] == "key-planted" || spec.RepoRef == "attacker-branch" {
		t.Fatalf("the launch resolves the planted overrides (keys=%v secrets=%v ref=%q): a manifest var wrote the card's launch context",
			spec.KeyOverrides, spec.SecretOverrides, spec.RepoRef)
	}
}

// R-F1, the card builder's own guard: rows that carry the reserved keys (an
// older planner, a hand edit) still card a run whose launch context is the
// server's stamp alone, own-vars marker included.
//
// Mutation: buildBoardCard skips its DropServerMintedLaunchVars → red on
// "card BotArgs carries the planted".
func TestSpineIssueLabel_PlantedRowVarsNeverReachTheCard(t *testing.T) {
	f := newSpineGitHubFixture(t,
		spineBotDecl{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur"}},
	)
	plantSpineRowVars(t, f)
	w := f.deliver(f.body("saboteur"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202 carded", w.Code, w.Body.String())
	}
	cards := laneCards(t, f.board)
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(cards))
	}
	wantNoPlantedLaunchContext(t, "card BotArgs", cards[0].BotArgs)
	if cards[0].BotArgs[boardOwnVarsKey] != "1" {
		t.Fatalf("card BotArgs[%s] = %q, want the server's own-vars stamp 1", boardOwnVarsKey, cards[0].BotArgs[boardOwnVarsKey])
	}
}

// R-F1, the direct lane: the run vars of a spine direct launch never carry
// the reserved launch context a row holds.
//
// Mutation: spineIssueVars skips its DropServerMintedLaunchVars → red on
// "run vars carries the planted".
func TestSpineIssueLabel_PlantedRowVarsNeverReachTheDirectRun(t *testing.T) {
	f := newSpineGitHubFixture(t,
		spineBotDecl{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
	)
	plantSpineRowVars(t, f)
	w := f.deliver(f.body("nestor"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202 accepted", w.Code, w.Body.String())
	}
	got := f.launchesByBot(t, "nestor")
	if len(got) != 1 {
		t.Fatalf("nestor launches = %d, want 1", len(got))
	}
	wantNoPlantedLaunchContext(t, "run vars", got[0].vars)
	for _, k := range native.ReservedLaunchArgs {
		if _, ok := got[0].vars[k]; ok {
			t.Errorf("run vars carry the reserved key %s", k)
		}
	}
}
