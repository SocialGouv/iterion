package forge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The approval queue's wire fact: a parked request's routing block is
// serialized to the org admin (json tags on ProvisionApproval). Whatever
// the org admin's UI shows, the payload CARRIES the decision.
func TestRV_ProvisionApproval_WireCarriesRouting(t *testing.T) {
	b, err := json.Marshal(ProvisionApproval{
		ID: "pa-1", OrgID: "o1", TenantID: "t1", ConnectionID: "c1",
		RepoFullName: "org/api", BotIDs: []string{"review-pr"},
		Routing:     routingTestBlock("byo"),
		RoutingSet:  true,
		RequestedBy: "u1",
		CreatedAt:   time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"routing"`) || !strings.Contains(string(b), `"routing_set":true`) {
		t.Fatalf("approval JSON drops the routing block: %s", b)
	}
}
