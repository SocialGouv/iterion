package cloudpublisher

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The run's price-feed fold reads the org the resolution's own fresh team
// read names. The org a launch or a resume passes in comes from a cached
// resolve that answers "" after a failed identity read as it does for an
// org-less team; folded under it, the org level's declarations — an
// org's accepted efforts — would vanish from the run's table. A team the
// fresh read places in no org folds without one, and a deployment with no
// identity seam folds under the org the caller names.
func TestPriceFeedFoldReadsTheOrgTheFreshTeamReadNames(t *testing.T) {
	for _, tc := range []struct {
		name      string
		identity  *fakeTeamResolver
		callerOrg string
		want      string
	}{
		{"the cached resolve failed", &fakeTeamResolver{orgs: map[string]string{"team1": "org1"}}, "", "org1"},
		{"an org-less team", &fakeTeamResolver{orgs: map[string]string{"team1": ""}}, "", ""},
		{"no identity seam", nil, "org1", "org1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, buf := gatewayKeyPublisher(t)
			type call struct {
				org, team string
				named     []string
			}
			var mu sync.Mutex
			var calls []call
			cfg := Config{
				ApiKeys: base.apiKeys, OAuthForfait: base.oauthForfait, UsageCaps: usagecap.NewMemStore(),
				RunSecrets: base.runSecrets, Sealer: base.sealer, Logger: base.logger,
				PriceFeedEffective: func(_ context.Context, orgID, teamID string, named []string) (json.RawMessage, error) {
					mu.Lock()
					defer mu.Unlock()
					calls = append(calls, call{orgID, teamID, append([]string(nil), named...)})
					return json.RawMessage(`{"entries":{}}`), nil
				},
			}
			if tc.identity != nil {
				cfg.Identity = tc.identity
			}
			p := newPublisher(cfg)
			seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
			creds := resolveBoundaryRun(t, p, "run-org", tc.callerOrg, gatewayOnlyWorkflow(), store.RunTrustDefault, nil)
			if b := openGatewayBundle(t, p, "run-org", creds); string(b.GatewayPriceFeed) != `{"entries":{}}` {
				t.Fatalf("the bundle does not carry the fold's table: %s (log:\n%s)", b.GatewayPriceFeed, buf.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if want := []call{{tc.want, "team1", []string{"team/m"}}}; !reflect.DeepEqual(calls, want) {
				t.Errorf("fold calls %+v, want %+v", calls, want)
			}
		})
	}
}
