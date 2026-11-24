package server

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The integration OPTS OUT of the pool (enabled:false) over a team that
// attaches it, and the operator then DISABLES the integration (or the
// card names a bot the integration does not run). The pipeline lane keeps
// applying the integration's routing and var layers, but cardPoolLayers
// drops its pool block — the opt-out included — so the carded run of THIS
// repo draws the team's pool.
func TestPoolReview2_cardLaneDropsAnOptOutWhenTheIntegrationIsDisabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		bots    []string
	}{
		{"integration disabled", false, []string{"probe"}},
		{"card bot not the integration's", true, []string{"other-bot"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &tierPublisher{}
			s, _ := newTeamForkServer(t, pub)
			wirePoolFold(t, s)
			rvaSeedTeamAttachment(t, s)
			board, err := native.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg := webhooks.Config{
				ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, Enabled: tc.enabled, BotIDs: tc.bots,
				Pool: &credpool.Binding{PoolID: "pool-bound", Enabled: false},
			}
			seedIntegrationConfig(t, s, cfg)
			iss, err := board.Create(native.Issue{Title: "ticket", State: native.StateReady, Bot: "probe",
				BotArgs: map[string]string{boardWebhookIDKey: "wh1"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.launchTicketNow(boundedCtx(t), "t1", systemLaunch(pipelineAdmissionActor), s.runs, board, iss); err != nil {
				t.Fatalf("launchTicketNow = %v", err)
			}
			got := pub.only(t).PoolAttachment
			t.Logf("%s: opt-out block, team attaches pool-bound -> carded run: %s", tc.name, rvaDescribe(got))
			if got != nil && got.PoolID != "" {
				t.Errorf("the integration's opt-out was dropped: the carded run draws %s", rvaDescribe(got))
			}
		})
	}
}
