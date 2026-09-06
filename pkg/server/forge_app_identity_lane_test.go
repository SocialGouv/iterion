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
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// appConnWorldNoSlug is appConnWorld with NEITHER the platform App config
// nor the connection record carrying the App slug — the shape of a
// connection created before the slug was recorded, or of a platform App run
// without ITERION_FORGE_GITHUB_APP_SLUG. No forge_token binding: the
// connection is the only credential.
func appConnWorldNoSlug(t *testing.T) (*Server, *fakeGitHubForge, webhooks.Config, string) {
	t.Helper()
	s := newWebhookTestServer(t)
	f := newFakeGitHubForge(t)
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t)}
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{
		ID: "c-app", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime,
		InstallationID: 42, AccountLogin: "[bot]", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	cfg, pt := ghConfig(t, s)
	cfg.ForgeBaseURL = f.srv.URL
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	return s, f, cfg, pt
}

// The bot's own /command echoes back as an issue_comment by "<slug>[bot]".
// The command gate's loop guard compares the commenter against WhoAmI; with
// no slug configured anywhere that answered "github-app[bot]", so the App's
// own comment cleared the guard and — the installation having write on its
// own repositories — launched a run: the bot answering itself. The slug is
// one GET /app away, and once learnt it is recorded on the connection so the
// identity set every other guard reads (iterionBotLogins) names the App too.
func TestPRForgeCommandGateLoopGuardSeesTheAppSlugItResolves(t *testing.T) {
	s, f, cfg, pt := appConnWorldNoSlug(t)
	f.perms["iterion-forge-x[bot]"] = "write"
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{"billy": {{BotID: "branch-improve-loop"}}}
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-loop", nil
	}
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","user":{"login":"alice"},"pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":558,"body":"/billy","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-558"},"sender":{"login":"iterion-forge-x[bot]"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if launched != 0 {
		t.Fatalf("the App's own comment launched a run (code=%d body=%s): the loop guard compared against a login that never posts", w.Code, w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("a self comment is a benign refusal (200/filtered), got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("status = %v, want filtered (loop-guard)", resp)
	}
	conn, err := s.forgeConnections.Get(context.Background(), "c-app")
	if err != nil {
		t.Fatal(err)
	}
	if conn.AppSlug != "iterion-forge-x" {
		t.Errorf("connection AppSlug = %q, want the slug the client resolved persisted on the record", conn.AppSlug)
	}
	if conn.AccountLogin != "iterion-forge-x[bot]" {
		t.Errorf("connection AccountLogin = %q, want the bare [bot] repaired from the slug", conn.AccountLogin)
	}
	if got := f.appLookupCount(); got != 1 {
		t.Fatalf("GET /app probes after the first delivery = %d, want 1", got)
	}
	// The slug is learnt once per connection: the cached client memoises it
	// and the record now carries it, so a second self comment on the same
	// connection is refused without another probe.
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), strings.Replace(body, `"id":558`, `"id":559`, 1), prforge.EventHeaderIssueComment, pt))
	if launched != 0 {
		t.Fatalf("the App's second self comment launched a run (code=%d body=%s)", w.Code, w.Body.String())
	}
	if got := f.appLookupCount(); got != 1 {
		t.Fatalf("GET /app probes after the second delivery = %d, want still 1 — the identity was resolved again instead of read from the cached client", got)
	}
}

// One connection, one client: a second delivery on the same connection
// mints nothing — the management token and the get profile minted for the
// first delivery are reused until they near expiry. Before the per-connection
// client, every lane built its own AppClient and every delivery paid the
// full set of mints again.
func TestAppConnectionMintsOncePerProfileAcrossDeliveries(t *testing.T) {
	s, f, cfg, pt := appConnWorld(t, false, false)
	f.perms["maintainer-jane"] = "maintain"
	approve := func(id int) {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFromID("maintainer-jane", id), prforge.EventHeaderIssueComment, pt))
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if w.Code != http.StatusOK || resp["status"] != "revi-approved" {
			t.Fatalf("approve #%d: code=%d body=%s mints=%d", id, w.Code, w.Body.String(), f.mintCount())
		}
	}
	approve(556)
	afterFirst := f.mintCount()
	if afterFirst == 0 {
		t.Fatal("the connection must have minted for the first delivery")
	}
	approve(557)
	if got := f.mintCount(); got != afterFirst {
		t.Fatalf("mints after the second delivery = %d, want %d — every lane rebuilt the client and minted its tokens again", got, afterFirst)
	}
}
