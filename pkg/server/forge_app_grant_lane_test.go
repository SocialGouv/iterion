package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The publish and gate write paths reach GitHub through forgeAdminFor, whose
// App client minted its management token from the constant baseline plus
// statuses. An installation whose owner granted LESS than the baseline 422'd
// on every mint — the retry without statuses 422'd again — so the connection
// could never serve, and a connection-only integration lost every write lane
// at once. The mint narrows to the grant the connection recorded, so the
// approve lands through the connection with the token the grant allows.
func TestReviewApproveServesAConnectionGrantedLessThanTheBaseline(t *testing.T) {
	subset := map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read", "statuses": "write"} // no issues, no repository_hooks
	s := newWebhookTestServer(t)
	f := newFakeGitHubForge(t)
	f.granted = subset
	f.perms["maintainer-jane"] = "maintain"
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t), AppSlug: "iterion-forge-x"}
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{
		ID: "c-app", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime,
		InstallationID: 42, AccountLogin: "iterion-forge-x[bot]", AppSlug: "iterion-forge-x",
		GrantedPermissions: subset, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	cfg, pt := ghConfig(t, s)
	cfg.ForgeBaseURL = f.srv.URL
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("maintainer-jane"), prforge.EventHeaderIssueComment, pt))
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp["status"] != "revi-approved" {
		t.Fatalf("an installation granted a strict subset of the baseline must still serve the approve through its connection: code=%d body=%s mints=%d", w.Code, w.Body.String(), f.mintCount())
	}
	statuses, _ := f.snapshot()
	if len(statuses) != 1 || statuses[0]["state"] != "success" {
		t.Fatalf("want one success status, got %v", statuses)
	}
	for _, b := range f.bearersFor("status") {
		if !strings.HasPrefix(b, "Bearer ghs_") {
			t.Fatalf("the status must be written with the connection's minted token, got %v", f.bearersFor("status"))
		}
	}
}
