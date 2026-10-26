package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// mintFailingAppGitHub is a GitHub (Enterprise path layout) serving an App's
// installation-token mint and one repository's hooks, recording each hook
// write and the secret it carries.
type mintFailingAppGitHub struct {
	mu        sync.Mutex
	mintCode  int // 0 = mint succeeds
	mints     int
	hookWrite int
	secret    string
	hookID    int
}

func (g *mintFailingAppGitHub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			g.mints++
			if g.mintCode != 0 {
				w.WriteHeader(g.mintCode)
				_, _ = w.Write([]byte(`{"message":"Server Error"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_mgmt", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		case r.URL.Path == "/api/v3/repos/o/r/hooks" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case strings.HasPrefix(r.URL.Path, "/api/v3/repos/o/r/hooks") && (r.Method == http.MethodPost || r.Method == http.MethodPatch):
			var body struct {
				Config map[string]any `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.hookWrite++
			g.secret, _ = body.Config["secret"].(string)
			if r.Method == http.MethodPost {
				g.hookID++
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": g.hookID, "active": true, "events": []string{"pull_request"}, "config": map[string]any{"url": body.Config["url"]}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
}

// TestForgeEnable_AppTokenMintFailureKeepsTheWorkingRepo: on a GitHub App
// connection a cold client (another replica, a restart, an expired token) mints
// its installation token inside the hook write. A mint that fails sent no hook
// request, so the re-provision puts the previous webhook config back and the
// repo keeps verifying the deliveries its untouched hook signs.
func TestForgeEnable_AppTokenMintFailureKeepsTheWorkingRepo(t *testing.T) {
	gh := &mintFailingAppGitHub{}
	srv := gh.server()
	defer srv.Close()

	s := newForgeTestServer(t)
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t), AppSlug: "iterion-forge-x"}
	s.forgeOrchestrator.Bots = gateBotLookup
	logs := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelWarn, logs)
	sealed, err := forge.SealPAT(s.sealer, "c-app", "ghs_install")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "c-app", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: srv.URL, Purpose: forge.PurposeRuntime,
		InstallationID: 42, AppSlug: "iterion-forge-x", AccountLogin: "iterion[bot]",
		SealedPayload: sealed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	enable := func(bots ...string) *errorRecorder {
		body, _ := json.Marshal(map[string]any{"connection_id": "c-app", "repo": "o/r", "bot_ids": bots})
		w := newErrorRecorder()
		s.handleEnableForgeRepoBots(w, forgeReq(superAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", string(body), "t1"))
		return w
	}

	first := enable("plain-bot")
	if first.Code != http.StatusOK {
		t.Fatalf("premise (a working repo on a github_app connection): code=%d body=%s", first.Code, first.Body.String())
	}
	var res struct {
		WebhookID string `json:"webhook_id"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &res)
	gh.mu.Lock()
	live, writes0 := gh.secret, gh.hookWrite
	gh.mu.Unlock()
	cfg0, _ := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if !webhooks.VerifyToken(live, cfg0.TokenHash) {
		t.Fatal("premise: the provisioned repo does not verify")
	}

	// A client that has not minted this connection's token yet, and a mint that
	// answers 502.
	s.forgetForgeAppClient("c-app")
	gh.mu.Lock()
	gh.mintCode = http.StatusBadGateway
	gh.mu.Unlock()

	w := enable("gate-bot")
	gh.mu.Lock()
	writes, nowLive := gh.hookWrite-writes0, gh.secret
	gh.mu.Unlock()
	cfg, _ := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if w.Code == http.StatusOK {
		t.Fatalf("the re-provision reported success although its token mint failed: %s", w.Body.String())
	}
	if writes != 0 || nowLive != live {
		t.Fatalf("the hook write reached GitHub (%d) — not the case under test", writes)
	}
	if !webhooks.VerifyToken(live, cfg.TokenHash) {
		t.Errorf("no hook write was sent (the token mint before it failed), yet the stored config keeps this "+
			"attempt's secret — every delivery to this working repo fails verification: %s", w.Body.String())
	}
}

// lostAnswerAppGitHub is a GitHub (Enterprise path layout) that APPLIES a hook
// write and, on demand, loses the answer to a PATCH: it records the secret the
// hook now signs with, then answers 502.
type lostAnswerAppGitHub struct {
	mu        sync.Mutex
	patch502  bool
	secret    string
	hookID    int
	hookWrite int
}

func (g *lostAnswerAppGitHub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_mgmt", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		case r.URL.Path == "/api/v3/repos/o/r/hooks" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case strings.HasPrefix(r.URL.Path, "/api/v3/repos/o/r/hooks") && (r.Method == http.MethodPost || r.Method == http.MethodPatch):
			var body struct {
				Config map[string]any `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.hookWrite++
			g.secret, _ = body.Config["secret"].(string) // APPLIED
			if r.Method == http.MethodPost {
				g.hookID++
			}
			if r.Method == http.MethodPatch && g.patch502 {
				w.WriteHeader(http.StatusBadGateway) // …and the answer is lost
				_, _ = w.Write([]byte(`{"message":"Bad Gateway"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": g.hookID, "active": true, "events": []string{"pull_request"}, "config": map[string]any{"url": body.Config["url"]}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
}

// TestForgeEnable_AppliedHookWriteWithALostAnswerKeepsTheFreshConfig is the
// other direction of the mint test: the token mint succeeds, the hook PATCH
// reaches GitHub and is applied, and its answer is lost (502). The forge now
// signs with the new secret, so the stored config keeps it and the operator is
// told to re-run — a restore there is what would break the repo.
func TestForgeEnable_AppliedHookWriteWithALostAnswerKeepsTheFreshConfig(t *testing.T) {
	gh := &lostAnswerAppGitHub{}
	srv := gh.server()
	defer srv.Close()

	s := newForgeTestServer(t)
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t), AppSlug: "iterion-forge-x"}
	s.forgeOrchestrator.Bots = gateBotLookup
	sealed, err := forge.SealPAT(s.sealer, "c-app", "ghs_install")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "c-app", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: srv.URL, Purpose: forge.PurposeRuntime,
		InstallationID: 42, AppSlug: "iterion-forge-x", AccountLogin: "iterion[bot]",
		SealedPayload: sealed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	enable := func(bots ...string) *errorRecorder {
		body, _ := json.Marshal(map[string]any{"connection_id": "c-app", "repo": "o/r", "bot_ids": bots})
		w := newErrorRecorder()
		s.handleEnableForgeRepoBots(w, forgeReq(superAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", string(body), "t1"))
		return w
	}
	first := enable("plain-bot")
	if first.Code != http.StatusOK {
		t.Fatalf("premise: %d %s", first.Code, first.Diagnostic(t))
	}
	var res struct {
		WebhookID string `json:"webhook_id"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &res)

	gh.mu.Lock()
	gh.patch502 = true
	gh.mu.Unlock()
	w := enable("gate-bot")
	gh.mu.Lock()
	forgeSecret := gh.secret
	gh.mu.Unlock()
	cfg, _ := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if w.Code == http.StatusOK {
		t.Fatalf("the re-provision reported success although the hook answer was lost: %s", w.Diagnostic(t))
	}
	if !strings.Contains(w.Diagnostic(t), "re-run the same request") {
		t.Errorf("the answer does not tell the operator to re-run an unsettled hook write: %s", w.Diagnostic(t))
	}
	if !webhooks.VerifyToken(forgeSecret, cfg.TokenHash) {
		t.Errorf("the forge signs with the applied secret but the stored config was restored to the old one — every delivery to this repo fails verification: %s", w.Diagnostic(t))
	}
}
