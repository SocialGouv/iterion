package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// #2644, the third deleter. The forge orchestrator deletes the managed
// secrets it minted straight through the store — never through
// handleDeleteGenericSecret and its sweep — and those secrets are
// team-scoped with no user scope, so validateSecretOverrides admits them as
// a pin on any other webhook or subscription of the tenant. Without the
// store hook, a deprovision leaves that pin dead and the lane seals the
// team default in silence. These witnesses drive the REAL disable route
// through the REAL wiring (newForgeTestServer hands the orchestrator the
// same store New does) against real memory stores.

// enableProjectBotRepo registers the platform app, connects a project-bot
// GitLab connection and enables review-pr on group/api. It returns the
// provision result and the minted bot's secret id, the one the provisioned
// config pins under forge_token.
func enableProjectBotRepo(t *testing.T, s *Server, base string) (forge.ProvisionResult, string) {
	t.Helper()
	ctx := superAdminCtx()
	w := httptest.NewRecorder()
	s.handleAdminRegisterForgeOAuthApp(w, forgeReq(ctx, "POST", "/api/admin/forge/oauth-apps",
		`{"provider":"gitlab","forge_base_url":"`+base+`","client_id":"cid","client_secret":"csecret","webhook_base_url":"https://iterion.fabrique.example/"}`, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("register platform app: code=%d body=%s", w.Code, w.Body.String())
	}
	conn := oauthConnectProjectBot(t, s, base)
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+conn.ID+`","repo":"group/api","bot_ids":["review-pr"]}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	secID := cfg.SecretOverrides["forge_token"]
	if secID == "" {
		t.Fatalf("the provisioned config pins no managed secret: %v", cfg.SecretOverrides)
	}
	return res, secID
}

// pinSecretElsewhere pins secID the way an operator pins a different forge
// identity: on a second webhook config and on a trigger subscription of the
// tenant — through the write-time validator first, which is the proof the
// managed secret IS admissible there.
func pinSecretElsewhere(t *testing.T, s *Server, secID string) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), "t1")
	pins := map[string]string{"forge_token": secID}
	if err := s.validateSecretOverrides(ctx, "t1", pins); err != nil {
		t.Fatalf("the managed secret is refused as an operator pin, the class does not exist: %v", err)
	}
	if err := s.webhookConfigs.Create(ctx, webhooks.Config{
		ID: "wh-other", TenantID: "t1", TokenHash: "hash-other", Enabled: true,
		SecretOverrides: map[string]string{"forge_token": secID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.TriggerStore.Create(ctx, trigger.Subscription{
		ID: "sub-other", TenantID: "t1", BotID: "review-pr", Enabled: true,
		SecretOverrides: map[string]string{"forge_token": secID},
	}); err != nil {
		t.Fatal(err)
	}
}

func disableRepo(t *testing.T, s *Server, integrationID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := forgeReq(superAdminCtx(), "DELETE", "/api/teams/t1/forge/repo-bots/"+integrationID, "", "t1")
	r.SetPathValue("integration_id", integrationID)
	s.handleDisableForgeRepoBots(w, r)
	return w
}

func secretPinStands(t *testing.T, s *Server, webhookID, subID string) (webhook, sub bool) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), "t1")
	cfg, err := s.webhookConfigs.Get(ctx, webhookID)
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.cfg.TriggerStore.Get(ctx, subID)
	if err != nil {
		t.Fatal(err)
	}
	_, webhook = cfg.SecretOverrides["forge_token"]
	_, sub = row.SecretOverrides["forge_token"]
	return webhook, sub
}

func TestDisableForgeRepoBots_SweepsThePinsNamingTheProjectBotSecret(t *testing.T) {
	gl := &projectBotGitLab{}
	srv := gl.server()
	defer srv.Close()
	s := newProjectBotTestServer(t)
	s.cfg.TriggerStore = trigger.NewMemorySubscriptionStore()
	res, secID := enableProjectBotRepo(t, s, srv.URL)
	pinSecretElsewhere(t, s, secID)

	if w := disableRepo(t, s, res.IntegrationID); w.Code != http.StatusNoContent {
		t.Fatalf("disable: code=%d body=%s", w.Code, w.Body.String())
	}
	ctx := store.WithTenant(context.Background(), "t1")
	if _, err := s.genericSecrets.Get(ctx, secID); !errors.Is(err, secrets.ErrGenericSecretNotFound) {
		t.Fatalf("the bot's secret survived the disable: %v", err)
	}
	if webhook, sub := secretPinStands(t, s, "wh-other", "sub-other"); webhook || sub {
		t.Fatalf("pins naming the deleted bot secret stand (webhook=%v subscription=%v) — the forge deletion bypassed the sweep and the lane seals the team default in silence", webhook, sub)
	}
	own, err := s.webhookConfigs.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if _, stands := own.SecretOverrides["forge_token"]; stands {
		t.Fatalf("the provisioned config still pins the deleted secret: %v", own.SecretOverrides)
	}
}

// failOncePinClear refuses the first clear of a subscription's pins and
// serves every later one — the interrupted sweep a retry must finish.
type failOncePinClear struct {
	trigger.SubscriptionStore
	refused int
}

func (f *failOncePinClear) ClearCredentialPins(ctx context.Context, id, recordID string) (bool, error) {
	if f.refused == 0 {
		f.refused++
		return false, errors.New("trigger store down")
	}
	return f.SubscriptionStore.ClearCredentialPins(ctx, id, recordID)
}

// Revocation is the irreversible half and happens first, so a refused sweep
// must not hide behind a clean disable: the route answers 5xx naming the
// pins, the integration stays retired-but-uncleaned, and the retry re-enters
// the hook with the secret already gone and finishes the sweep.
func TestDisableForgeRepoBots_ARefusedSweepFailsTheDisableAndTheRetryFinishesIt(t *testing.T) {
	gl := &projectBotGitLab{}
	srv := gl.server()
	defer srv.Close()
	s := newProjectBotTestServer(t)
	subs := &failOncePinClear{SubscriptionStore: trigger.NewMemorySubscriptionStore()}
	s.cfg.TriggerStore = subs
	res, secID := enableProjectBotRepo(t, s, srv.URL)
	pinSecretElsewhere(t, s, secID)

	// The body stays opaque by the 5xx convention; the logged cause names
	// both halves (the deletion that happened, the pins still standing).
	w := disableRepo(t, s, res.IntegrationID)
	if w.Code < http.StatusInternalServerError {
		t.Fatalf("a disable whose sweep failed answered %d %s — the dead pin stands behind a clean receipt", w.Code, w.Body.String())
	}
	ctx := store.WithTenant(context.Background(), "t1")
	if _, err := s.genericSecrets.Get(ctx, secID); !errors.Is(err, secrets.ErrGenericSecretNotFound) {
		t.Fatalf("revocation is the first half, yet the secret stands: %v", err)
	}
	if _, sub := secretPinStands(t, s, "wh-other", "sub-other"); !sub {
		t.Fatal("the fixture's refused clear swept the subscription pin anyway")
	}
	// A retired row is a lifecycle read, hidden from Get.
	ri, err := s.forgeIntegrations.GetProvision(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !ri.Retired || ri.RetireCleanupComplete {
		t.Fatalf("after the failed disable: retired=%v cleanup_complete=%v, want retired and NOT complete so the retry reconciles", ri.Retired, ri.RetireCleanupComplete)
	}

	if w := disableRepo(t, s, res.IntegrationID); w.Code != http.StatusNoContent {
		t.Fatalf("retry: code=%d body=%s", w.Code, w.Body.String())
	}
	if webhook, sub := secretPinStands(t, s, "wh-other", "sub-other"); webhook || sub {
		t.Fatalf("the retry left pins standing (webhook=%v subscription=%v)", webhook, sub)
	}
	if ri, err = s.forgeIntegrations.GetProvision(ctx, res.IntegrationID); err != nil || !ri.RetireCleanupComplete {
		t.Fatalf("after the retry: cleanup_complete=%v err=%v", ri.RetireCleanupComplete, err)
	}
	if subs.refused != 1 {
		t.Fatalf("the fixture refused %d clears, want exactly the first", subs.refused)
	}
}

// The production wiring: New hands the orchestrator the sweeping store, not
// the bare one the routes use — the hook is only as real as this literal.
func TestNew_HandsTheForgeOrchestratorThePinSweepingSecretStore(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		ForgeConnections:  connection.NewMemoryForgeStore(),
		ForgeIntegrations: forge.NewMemoryRepoIntegrationStore(),
		WebhookConfigs:    webhooks.NewMemoryConfigStore(),
		GenericSecrets:    secrets.NewMemoryGenericSecretStore(),
		Sealer:            sealer,
	}, iterlog.New(iterlog.LevelError, nil))
	if s.forgeOrchestrator == nil {
		t.Fatal("the forge orchestrator is not built with its full dependency set")
	}
	if _, ok := s.forgeOrchestrator.Secrets.(pinSweepingSecrets); !ok {
		t.Fatalf("the orchestrator deletes through a %T — a bare store leaves the pins naming a managed secret dead", s.forgeOrchestrator.Secrets)
	}
}

// The hook's own contract, at the seam the orchestrator holds: revoke, then
// sweep under the read scope's tenant and detached from the caller's
// context; a refused sweep is an error that leaves the record gone, and the
// retry re-enters with ErrGenericSecretNotFound and sweeps what is left.
func TestPinSweepingSecrets_RevokesThenSweepsAndARetryFinishesAnInterruptedSweep(t *testing.T) {
	wh := webhooks.NewMemoryConfigStore()
	subs := trigger.NewMemorySubscriptionStore()
	gen := secrets.NewMemoryGenericSecretStore()
	s := New(Config{DisableAuth: true, WebhookConfigs: wh, GenericSecrets: gen, TriggerStore: subs}, iterlog.New(iterlog.LevelError, nil))
	ctx := store.WithTenant(context.Background(), "t1")
	if err := gen.Create(ctx, secrets.GenericSecret{ID: "sec-1", TenantID: "t1", ScopeTeamID: "t1", Name: "forge_token", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := wh.Create(ctx, webhooks.Config{ID: "wh-1", TenantID: "t1", TokenHash: "h1", SecretOverrides: map[string]string{"forge_token": "sec-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := subs.Create(ctx, trigger.Subscription{ID: "sub-1", TenantID: "t1", BotID: "review-pr", Enabled: true, SecretOverrides: map[string]string{"forge_token": "sec-1"}}); err != nil {
		t.Fatal(err)
	}
	hooked := s.pinSweepingSecrets()

	// The webhook home refuses its clear; the trigger home is swept, the
	// record is gone, and the error names the pins.
	s.webhookConfigs = failingPinSweep{ConfigStore: wh}
	err := hooked.Delete(ctx, "sec-1")
	if err == nil || !strings.Contains(err.Error(), "credential pins") {
		t.Fatalf("a delete whose sweep failed = %v, want an error naming the pins", err)
	}
	if _, gerr := gen.Get(ctx, "sec-1"); !errors.Is(gerr, secrets.ErrGenericSecretNotFound) {
		t.Fatalf("revocation is the first half, yet the secret stands: %v", gerr)
	}
	if webhook, sub := secretPinStands(t, s, "wh-1", "sub-1"); !webhook || sub {
		t.Fatalf("after the refused webhook clear: webhook=%v subscription=%v, want the webhook pin standing and the subscription pin swept", webhook, sub)
	}

	// The retry: the inner delete answers not-found, the sweep runs anyway
	// and that verdict is what the caller gets back once the pins are gone.
	// A context already cancelled must not cancel the clear: the sweep is
	// detached, as on the routes.
	s.webhookConfigs = wh
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := hooked.Delete(cancelled, "sec-1"); !errors.Is(err, secrets.ErrGenericSecretNotFound) {
		t.Fatalf("the retry = %v, want ErrGenericSecretNotFound once the sweep finished", err)
	}
	if webhook, sub := secretPinStands(t, s, "wh-1", "sub-1"); webhook || sub {
		t.Fatalf("the retry left pins standing (webhook=%v subscription=%v)", webhook, sub)
	}
}
