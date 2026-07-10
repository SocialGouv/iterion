package runner

import (
	"context"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// TestRecordOrgSpendKey pins the usage key the runner charges spend to:
// the message's OrgID (the key the launch gate metered on) with a
// TenantID fallback for pre-orgid messages and org-less teams. Charging
// the team key when an org exists left the org's cost-cap document at
// zero forever — the multi-team cost-cap bug.
func TestRecordOrgSpendKey(t *testing.T) {
	cases := []struct {
		name    string
		orgID   string
		wantKey string
	}{
		{"org message charges the org key", "org-1", "org-1"},
		{"pre-orgid message falls back to the tenant key", "", "team-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counter := orgusage.NewMemoryCounter()
			r := &Runner{cfg: Config{OrgUsage: counter, Logger: iterlog.New(iterlog.LevelError, nil)}}
			usage := newMetricsEmitter(nil, nil)
			usage.mu.Lock()
			usage.runCostUSD = 1.5
			usage.runInputTokens = 100
			usage.runOutputTokens = 50
			usage.mu.Unlock()

			r.recordOrgSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-a", OrgID: tc.orgID}, usage)

			now := time.Now().UTC()
			got, err := counter.Usage(context.Background(), tc.wantKey, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.CostUSD != 1.5 || got.InputTokens != 100 || got.OutputTokens != 50 {
				t.Fatalf("usage on %q = %+v, want cost 1.5 / 100 / 50", tc.wantKey, got)
			}
			// Nothing lands on the other key.
			other := "org-1"
			if tc.wantKey == "org-1" {
				other = "team-a"
			}
			if u, _ := counter.Usage(context.Background(), other, now); u.CostUSD != 0 {
				t.Fatalf("spend leaked onto %q: %+v", other, u)
			}
		})
	}
}
