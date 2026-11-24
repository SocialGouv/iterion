package trigger

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// A promotion is a wholesale bot-args replace on a card that may be
// webhook-carried: the subscription's own vars land, the card's SERVER-
// stamped launch context survives the swap. A promotion wiping it launches
// the card's run without the identity its quota reads, and silently degrades
// the webhook's BYOK resolution — the local effect and the cloud twin
// (pkg/server trigger_cloud.go) share the semantics, each pinned.
func TestNativeBoardPromoteKeepsTheStoredLaunchContext(t *testing.T) {
	ns, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	stored, err := ns.Create(native.Issue{
		Title: "webhook card", State: native.StateReady, Bot: "botA",
		BotArgs: map[string]string{
			"member_var":              "v1",
			"__iterion_repo_url":      "https://github.com/acme/widgets.git",
			"__iterion_project_path":  "acme/widgets",
			"__iterion_webhook_id":    "wh-t1",
			"__iterion_key_overrides": `{"https://github.com": {"token": "k1"}}`,
		},
	})
	if err != nil {
		t.Fatalf("create card: %v", err)
	}
	plan := LaunchPlan{
		BotID:    "botB",
		TenantID: "t1",
		Vars: map[string]string{
			"team_var":               "x",
			"__iterion_project_path": "harmless/other",
			"__iterion_webhook_id":   "wh-x",
		},
		Event: Event{TenantID: "t1", Subject: Subject{Type: "card", ID: stored.ID}},
	}
	if _, err := NewNativeBoardEffect(ns, nil, nil).Promote(context.Background(), plan); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	after, err := ns.Get(stored.ID)
	if err != nil {
		t.Fatalf("get card: %v", err)
	}
	if after.Bot != "botB" {
		t.Fatalf("the promotion did not pin its bot (%q) — this test only means something while the stamp lands", after.Bot)
	}
	if after.BotArgs["team_var"] != "x" {
		t.Errorf("the subscription's own var did not land: %v", after.BotArgs)
	}
	if _, carried := after.BotArgs["member_var"]; carried {
		t.Errorf("a promotion must replace the card's own member vars: %v", after.BotArgs)
	}
	for k, want := range map[string]string{
		"__iterion_repo_url":      "https://github.com/acme/widgets.git",
		"__iterion_project_path":  "acme/widgets",
		"__iterion_webhook_id":    "wh-t1",
		"__iterion_key_overrides": `{"https://github.com": {"token": "k1"}}`,
	} {
		if got := after.BotArgs[k]; got != want {
			t.Errorf("the promotion wiped the server-stamped %s to %q, want %q — the card's run loses its meter, bound and BYOK audience", k, got, want)
		}
	}
}
