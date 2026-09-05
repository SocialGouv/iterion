package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// A GitHub App connection's client is LAZY: forgeAdminFor returns an
// AppClient that mints its installation token on the first call, so a mint
// that fails — an installation whose grant lags the permission set iterion
// requests (422), a rotated App key (401), a suspended installation — never
// surfaces at construction. The webhook lanes prefer the covering
// connection and keep the webhook's forge_token binding as the fallback; a
// fallback keyed on construction alone is therefore dead on every App
// connection, and the lane 502s on a world where a working token sits one
// branch away — the 5xx class GitHub answers by disabling the hook.
//
// These probes wire the real App path end to end: the platform App identity
// signs a JWT with a test key, the fake forge serves the mint, and every
// endpoint records the bearer that reached it, so a test proves WHICH
// credential served — the minted ghs_ token or the hand-owned binding.

func testAppKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// appConnWorld is a webhook on a repo covered by a github_app team
// connection (installation 42 on the fake forge), authenticated through the
// platform App identity. mintFail makes the installation-token mint answer
// GitHub's permissions-not-granted 422; withToken binds a hand-owned
// forge_token on the webhook.
func appConnWorld(t *testing.T, mintFail, withToken bool) (*Server, *fakeGitHubForge, webhooks.Config, string) {
	t.Helper()
	s := newWebhookTestServer(t)
	f := newFakeGitHubForge(t)
	f.mintFail = mintFail
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t), AppSlug: "iterion-forge-x"}
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{
		ID: "c-app", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime,
		InstallationID: 42, AccountLogin: "iterion-forge-x[bot]", AppSlug: "iterion-forge-x",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	cfg, pt := ghConfig(t, s)
	cfg.ForgeBaseURL = f.srv.URL
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	if withToken {
		seedForgeToken(t, s, &cfg, "ghp_hand_owned")
	}
	return s, f, cfg, pt
}

// bearerIsToken reports whether every bearer an endpoint received is the
// hand-owned binding (never the minted installation token).
func bearerIsToken(bearers []string) bool {
	if len(bearers) == 0 {
		return false
	}
	for _, b := range bearers {
		if b != "Bearer ghp_hand_owned" {
			return false
		}
	}
	return true
}

// The approve lane on a mint-failing App connection with a forge_token
// binding: the commenter's role is read and the status written through the
// token, and the approve lands — not a 502 on the authz check.
func TestPRForgeAppFallback_ApproveServesThroughTheTokenWhenTheMintFails(t *testing.T) {
	s, f, cfg, pt := appConnWorld(t, true, true)
	f.perms["maintainer-jane"] = "maintain"
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("maintainer-jane"), prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("a mint failure with a bound token must not 5xx (GitHub disables the hook), got %d body=%s mints=%d", w.Code, w.Body.String(), f.mintCount())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "revi-approved" {
		t.Fatalf("approve must land through the token, got %v (mints=%d)", resp, f.mintCount())
	}
	statuses, _ := f.snapshot()
	if len(statuses) != 1 || statuses[0]["state"] != "success" {
		t.Fatalf("want one success status written through the token, got %v", statuses)
	}
	if !bearerIsToken(f.bearersFor("permission")) || !bearerIsToken(f.bearersFor("status")) {
		t.Fatalf("the role read and the status write must ride the forge_token binding, got permission=%v status=%v", f.bearersFor("permission"), f.bearersFor("status"))
	}
	if f.mintCount() == 0 {
		t.Fatal("the connection must have been tried first (no mint attempted)")
	}
}

// The command lane (/billy on a PR) on the same world: the role read and the
// PR-head resolution both serve through the token, and the bot launches.
func TestPRForgeAppFallback_CommandLaunchesThroughTheTokenWhenTheMintFails(t *testing.T) {
	s, f, cfg, pt := appConnWorld(t, true, true)
	f.perms["dev-dan"] = "write"
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{"billy": {{BotID: "branch-improve-loop"}}}
	launched := 0
	var gotRef string
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, repoRef, _ string, _, _ map[string]string) (string, error) {
		launched++
		gotRef = repoRef
		return "run-billy", nil
	}
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","user":{"login":"alice"},"pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":557,"body":"/billy","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-557"},"sender":{"login":"dev-dan"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("/billy must launch through the token, got %d body=%s mints=%d", w.Code, w.Body.String(), f.mintCount())
	}
	if launched != 1 || gotRef != "feat/x" {
		t.Fatalf("want one launch on the PR head branch, got launched=%d ref=%q", launched, gotRef)
	}
	if !bearerIsToken(f.bearersFor("permission")) || !bearerIsToken(f.bearersFor("pull")) {
		t.Fatalf("the role read and the PR resolution must ride the forge_token binding, got permission=%v pull=%v", f.bearersFor("permission"), f.bearersFor("pull"))
	}
}

// The zero-touch author-trust gate on the same world: the live
// CollaboratorPermission fallback reads through the token, so a
// write-permission author is trusted instead of parked.
func TestPRForgeAppFallback_AuthorTrustReadsThroughTheTokenWhenTheMintFails(t *testing.T) {
	s, f, cfg, _ := appConnWorld(t, true, true)
	f.perms["dev-dan"] = "write"
	p := prforge.ParsedIssue{
		ProjectPath: "acme/widgets", IssueNumber: 5, Action: "opened",
		IssueURL: f.srv.URL + "/acme/widgets/issues/5", SenderLogin: "dev-dan", IssueAuthorLogin: "dev-dan",
	}
	if !s.issueAuthorTrusted(context.Background(), cfg, webhooks.ProviderGitHub, "feature-dev", p) {
		t.Fatalf("a write-permission author must be trusted through the token fallback (mints=%d permission bearers=%v)", f.mintCount(), f.bearersFor("permission"))
	}
	if !bearerIsToken(f.bearersFor("permission")) {
		t.Fatalf("the role read must ride the forge_token binding, got %v", f.bearersFor("permission"))
	}
}

// The App-only setup (no token binding) with a working mint keeps landing
// through the installation token: the preflight must never displace a
// connection that can serve.
func TestPRForgeAppFallback_AppOnlyStillLandsThroughTheMint(t *testing.T) {
	s, f, cfg, pt := appConnWorld(t, false, false)
	f.perms["maintainer-jane"] = "maintain"
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("maintainer-jane"), prforge.EventHeaderIssueComment, pt))
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp["status"] != "revi-approved" {
		t.Fatalf("App-only approve must land through the mint, got %d %v", w.Code, resp)
	}
	statuses, _ := f.snapshot()
	if len(statuses) != 1 {
		t.Fatalf("want one status, got %v", statuses)
	}
	for _, ep := range []string{"permission", "status"} {
		for _, b := range f.bearersFor(ep) {
			if b != "Bearer ghs_inst" {
				t.Fatalf("%s must ride the minted installation token, got %q", ep, b)
			}
		}
	}
	if len(f.bearersFor("permission")) == 0 || len(f.bearersFor("status")) == 0 {
		t.Fatal("the connection's client must have served both the role read and the status write")
	}
}

// An installation that CAN mint but withholds statuses:write — created
// before the merge gate, or one that declined the permission — is served
// by the App client's baseline re-mint, so its reads work and the token
// looks healthy; only the status write 403s. The approve lane exists to
// write that status: with a forge_token binding on the webhook, the write
// must go through the binding, and the withheld token must never be sent
// a status write.
func TestPRForgeAppFallback_StatusesWithheldWritesThroughTheToken(t *testing.T) {
	s, f, cfg, pt := appConnWorld(t, false, true)
	f.mintDenyStatuses = true
	f.perms["maintainer-jane"] = "maintain"
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("maintainer-jane"), prforge.EventHeaderIssueComment, pt))
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp["status"] != "revi-approved" {
		t.Fatalf("approve must land through the binding when the installation withholds statuses, got %d %v (status bearers=%v)", w.Code, resp, f.bearersFor("status"))
	}
	if got := f.bearersFor("status"); len(got) != 1 || got[0] != "Bearer ghp_hand_owned" {
		t.Fatalf("the status write must ride the forge_token binding and nothing else, got %v", got)
	}
	if f.mintsBaseline == 0 {
		t.Fatal("the connection must have been tried first (no baseline re-mint recorded)")
	}
}

// The same installation with NO binding: a refusal that names the withheld
// grant — the thing the operator must approve on the App — not a bare
// "insufficient scope" from a status write that was never going to land.
func TestPRForgeAppFallback_StatusesWithheldWithoutTokenIsANamedRefusal(t *testing.T) {
	s, f, cfg, pt := appConnWorld(t, false, false)
	f.mintDenyStatuses = true
	f.perms["maintainer-jane"] = "maintain"
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("maintainer-jane"), prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("a withheld grant must never 5xx the hook, got %d body=%s", w.Code, w.Body.String())
	}
	rows := approveDeliveries(t, s, cfg)
	if len(rows) != 1 || rows[0].Status != webhooks.StatusFiltered {
		t.Fatalf("a withheld grant is a configuration refusal: want one filtered row, got %+v", rows)
	}
	if !strings.Contains(rows[0].Error, "statuses") || !strings.Contains(rows[0].Error, "c-app") || strings.Contains(rows[0].Error, "insufficient scope") {
		t.Fatalf("the refusal must name the withheld statuses grant on the connection, never a bare insufficient scope, got %q", rows[0].Error)
	}
	if got := f.bearersFor("status"); len(got) != 0 {
		t.Fatalf("no status write may be attempted with a token known to lack the grant, got %v", got)
	}
	_, comments := f.snapshot()
	if len(comments) != 1 || !strings.Contains(comments[0], "statuses") {
		t.Fatalf("the maintainer must be told which grant is missing, got %v", comments)
	}
	// What to approve, on which App — never the connection id or GitHub's
	// error text, which stay on the audit row.
	if strings.Contains(comments[0], "c-app") || strings.Contains(comments[0], "not granted") {
		t.Fatalf("the reply pasted internal detail onto the PR:\n%s", comments[0])
	}
}

// A mint-failing App connection and NO token binding is a refusal, not a
// 5xx: the delivery is audited with a reason naming BOTH the unusable
// connection and the missing binding, so the operator knows which of the
// two to fix.
func TestPRForgeAppFallback_MintFailureWithoutTokenIsANamedRefusal(t *testing.T) {
	s, f, cfg, pt := appConnWorld(t, true, false)
	f.perms["maintainer-jane"] = "maintain"
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("maintainer-jane"), prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("a mint failure must never 5xx the hook, got %d body=%s", w.Code, w.Body.String())
	}
	rows := approveDeliveries(t, s, cfg)
	if len(rows) != 1 || rows[0].Status != webhooks.StatusFiltered {
		t.Fatalf("want one filtered row, got %+v", rows)
	}
	if !strings.Contains(rows[0].Error, "c-app") || !strings.Contains(rows[0].Error, "not granted") || !strings.Contains(rows[0].Error, "forge_token") {
		t.Fatalf("the refusal must name the unusable connection, GitHub's reason and the missing binding, got %q", rows[0].Error)
	}
	if statuses, _ := f.snapshot(); len(statuses) != 0 {
		t.Fatalf("nothing may be written, got %v", statuses)
	}
}
