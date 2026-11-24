package server

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// promCloudStore is a boardmongo.TriggerStore over the real filesystem
// native store, so the promotion's Update passes through the same
// BotArgsForWrite twin production writes through. Only the cursor/label
// edges the spine needs are stubbed — Promote touches Get/Update alone.
type promCloudStore struct {
	trigger.EffectOutbox
	b *native.Store
}

func (s *promCloudStore) TriggerCursor() (int64, error)                   { return 0, nil }
func (s *promCloudStore) EventsAfter(int64, int) ([]native.Event, error)  { return nil, nil }
func (s *promCloudStore) AdvanceTriggerCursor(int64, int64) (bool, error) { return true, nil }
func (s *promCloudStore) Get(id string) (*native.Issue, error)            { return s.b.Get(id) }
func (s *promCloudStore) Update(id string, p native.Patch) (*native.Issue, error) {
	return s.b.Update(id, p)
}
func (s *promCloudStore) ConsumeLabels(string, []string) (bool, error) { return false, nil }
func (s *promCloudStore) ConsumeLabelsForEffect(string, []string, string) (bool, error) {
	return false, nil
}

type promCloudCoord struct {
	st     boardmongo.TriggerStore
	tenant string
}

func (c promCloudCoord) TriggerStoreFor(string) boardmongo.TriggerStore { return c.st }
func (c promCloudCoord) DistinctEffectTenants(context.Context) ([]string, error) {
	return []string{c.tenant}, nil
}

// A trigger promotion is a wholesale bot-args replace on a card that may be
// webhook-carried: the subscription's own vars land, the card's SERVER-
// stamped launch context survives the swap — the repo identity, BYOK
// overrides and integration id the dispatcher meters, bounds and keys the
// run by. A promotion wiping them launches the card's run unbounded and
// silently degrades its BYOK resolution.
func TestCloudBoardPromoteKeepsTheStoredLaunchContext(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	stored, err := board.Create(native.Issue{
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
		t.Fatalf("board.Create: %v", err)
	}
	eff := &cloudBoardEffect{coord: promCloudCoord{st: &promCloudStore{b: board}, tenant: "t1"}}
	plan := trigger.LaunchPlan{
		BotID:    "botB",
		TenantID: "t1",
		Vars: map[string]string{
			"team_var":               "x",
			"__iterion_project_path": "harmless/other",
			"__iterion_webhook_id":   "wh-x",
		},
		Event: trigger.Event{TenantID: "t1", Subject: trigger.Subject{Type: "card", ID: stored.ID}},
	}
	if _, err := eff.Promote(context.Background(), plan); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	after, err := board.Get(stored.ID)
	if err != nil {
		t.Fatalf("board.Get: %v", err)
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
	// The plan's own vars never write the reserved class over the stamp.
	if after.BotArgs["__iterion_project_path"] == "harmless/other" {
		t.Errorf("the promotion's vars forged the project path over the server's stamp")
	}
}
