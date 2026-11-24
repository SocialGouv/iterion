package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// #2644, the write-side half of the sweep. A pin is judged against its
// record at validation time, so a write whose validation beat the record's
// revocation can land after the delete's sweep already listed — the sweep
// never sees the row, no client can re-delete a gone record, and a pin
// naming the dead record would stand forever, sealing the team's default
// on a cron subscription. These witnesses interpose the revocation between
// the route's validation and the row's write (the midFlight stores below)
// and read the STORES after the answer: a stored pin naming a dead id is
// the defect; its absence is the fix. One witness per pin-landing write —
// a site without its witness is a site where the window stays open.

// midFlightSubStore runs once immediately before the wrapped subscription
// store's first Create or Update lands — the seam that revokes the record
// after the route's validation judged it and before the row's write lands.
type midFlightSubStore struct {
	trigger.SubscriptionStore
	once func()
}

func (s *midFlightSubStore) fire() {
	if s.once != nil {
		fn := s.once
		s.once = nil
		fn()
	}
}

func (s *midFlightSubStore) Create(ctx context.Context, sub trigger.Subscription) error {
	s.fire()
	return s.SubscriptionStore.Create(ctx, sub)
}

func (s *midFlightSubStore) Update(ctx context.Context, sub trigger.Subscription) error {
	s.fire()
	return s.SubscriptionStore.Update(ctx, sub)
}

// midFlightConfigStore is the webhook-config twin of midFlightSubStore.
type midFlightConfigStore struct {
	webhooks.ConfigStore
	once func()
}

func (s *midFlightConfigStore) fire() {
	if s.once != nil {
		fn := s.once
		s.once = nil
		fn()
	}
}

func (s *midFlightConfigStore) Create(ctx context.Context, cfg webhooks.Config) error {
	s.fire()
	return s.ConfigStore.Create(ctx, cfg)
}

func (s *midFlightConfigStore) Update(ctx context.Context, cfg webhooks.Config) error {
	s.fire()
	return s.ConfigStore.Update(ctx, cfg)
}

// getThenFailKeyStore passes its first `pass` Gets through and refuses the
// rest with a store error that is not a not-found — the shape that must
// reach the operator as an explicit 5xx, never as a successful answer over
// an unreconciled pin. The route's flow is deterministic in-process: the
// validator's Get is first, the reconcile's re-check second.
type getThenFailKeyStore struct {
	secrets.ApiKeyStore
	pass int
}

func (s *getThenFailKeyStore) Get(ctx context.Context, id string) (secrets.ApiKey, error) {
	if s.pass <= 0 {
		return secrets.ApiKey{}, errors.New("getThenFailKeyStore: store unavailable")
	}
	s.pass--
	return s.ApiKeyStore.Get(ctx, id)
}

func seedSweepSecret(t *testing.T, srv *Server, tenant, id string) secrets.GenericSecret {
	t.Helper()
	sec := secrets.GenericSecret{ID: id, TenantID: tenant, ScopeTeamID: tenant, Name: "forge_token", CreatedAt: time.Now().UTC()}
	if err := srv.genericSecrets.Create(store.WithTenant(t.Context(), tenant), sec); err != nil {
		t.Fatal(err)
	}
	return sec
}

// TestCreateTrigger_SweepsAPinWhoseRecordDiedMidFlight: the subscription's
// secret pin validated against a live record, the record's delete ran
// while the create was in flight (its sweep listed before the row landed),
// and the create landed anyway. The route's own reconciliation is what
// sweeps the dead pin — the delete's listing never knew the row.
func TestCreateTrigger_SweepsAPinWhoseRecordDiedMidFlight(t *testing.T) {
	srv, org, sec := triggerPinServer(t)
	var logs bytes.Buffer
	srv.logger = iterlog.New(iterlog.LevelWarn, &logs)

	srv.cfg.TriggerStore = &midFlightSubStore{
		SubscriptionStore: trigger.NewMemorySubscriptionStore(),
		once: func() {
			if err := srv.genericSecrets.Delete(store.WithTenant(t.Context(), "t1"), sec.ID); err != nil {
				t.Fatalf("seed secret delete: %v", err)
			}
		},
	}

	body := `{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},
		"key_overrides":{"anthropic":"` + org.ID + `"},
		"secret_overrides":{"forge_token":"` + sec.ID + `"}}`
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	// The answer must not resell the swept pin.
	if _, kept := created.SecretOverrides["forge_token"]; kept {
		t.Fatalf("the create answer still carries the swept pin: %v", created.SecretOverrides)
	}
	got, err := srv.cfg.TriggerStore.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SecretOverrides) != 0 {
		t.Fatalf("stored secret_overrides = %v, want the clear — the record died mid-flight and nothing else can ever revisit this row; a cron subscription would fire under the unfilled secret forever", got.SecretOverrides)
	}
	// The sibling pin naming a record that stayed alive survives the
	// reconcile — a dead-record sweep is not a pin clear.
	if got.KeyOverrides["anthropic"] != org.ID {
		t.Fatalf("stored key_overrides = %v, want the live pin kept", got.KeyOverrides)
	}
	if !strings.Contains(logs.String(), created.ID) || !strings.Contains(logs.String(), "swept") {
		t.Fatalf("the reconcile's receipt is missing from the log: %q", logs.String())
	}
}

// TestUpdateTrigger_SweepsAPinWhoseRecordDiedMidFlight: the same window on
// the edit route — the new pin validated while its record lived and landed
// after the record's delete sweep listed.
func TestUpdateTrigger_SweepsAPinWhoseRecordDiedMidFlight(t *testing.T) {
	srv, org, _ := triggerPinServer(t)
	dying := seedVendorKey(t, srv.apiKeys, "t1", "", "org-dying", false)

	body := `{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},
		"key_overrides":{"anthropic":"` + org.ID + `"}}`
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d; body=%s", rec.Code, rec.Body.String())
	}
	var created trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	srv.cfg.TriggerStore = &midFlightSubStore{
		SubscriptionStore: srv.cfg.TriggerStore,
		once: func() {
			if err := srv.apiKeys.Delete(store.WithTenant(t.Context(), "t1"), dying.ID); err != nil {
				t.Fatalf("seed key delete: %v", err)
			}
		},
	}
	repl := httptest.NewRecorder()
	srv.handleUpdateTrigger(repl, teamTriggerReq(t, http.MethodPut, "/api/v1/triggers/"+created.ID, created.ID,
		`{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},
		"key_overrides":{"anthropic":"`+dying.ID+`"}}`))
	if repl.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200; body=%s", repl.Code, repl.Body.String())
	}
	got, err := srv.cfg.TriggerStore.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.KeyOverrides) != 0 {
		t.Fatalf("stored key_overrides = %v, want the swept clear — the edit's pin named a key that died mid-flight and the delete's listing never saw this edit", got.KeyOverrides)
	}
}

// TestCreateWebhook_SweepsAPinWhoseRecordDiedMidFlight: the webhook lane's
// create site, and the key half of the re-check.
func TestCreateWebhook_SweepsAPinWhoseRecordDiedMidFlight(t *testing.T) {
	srv, spy, _, auditStore := newPinSweepServer(t)
	dying := seedSweepKey(t, spy, "team-a", "key-dying", secrets.ProviderAnthropic)
	alive := seedSweepSecret(t, srv, "team-a", "sec-live")

	srv.webhookConfigs = &midFlightConfigStore{
		ConfigStore: srv.webhookConfigs,
		once: func() {
			if err := spy.Delete(store.WithTenant(t.Context(), "team-a"), dying.ID); err != nil {
				t.Fatalf("seed key delete: %v", err)
			}
		},
	}

	body := `{"name":"wh","provider":"gitlab","wildcard_bots":true,
		"key_overrides":{"anthropic":"` + dying.ID + `"},
		"secret_overrides":{"forge_token":"` + alive.ID + `"}}`
	r := httptest.NewRequest(http.MethodPost, "/api/teams/team-a/webhooks", strings.NewReader(body))
	r.SetPathValue("id", "team-a")
	// Past the mux, so past the derivation gate: the mint handlers refuse a
	// context without the verdict; stamp the same one the gate does.
	ctx := testMintVerdictContext(store.WithTenant(auth.WithIdentity(r.Context(),
		auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.handleCreateWebhook(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	// The create answers webhookWithToken: the config travels under
	// "config", not at the top level.
	var created struct {
		Config webhooks.Config `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	// The answer must not resell the swept pin.
	if _, kept := created.Config.KeyOverrides["anthropic"]; kept {
		t.Fatalf("the create answer still carries the swept pin: %v", created.Config.KeyOverrides)
	}
	got, err := srv.webhookConfigs.Get(context.Background(), created.Config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.KeyOverrides) != 0 {
		t.Fatalf("stored key_overrides = %v, want the swept clear — a stored pin naming a deleted key seals the team's default in silence", got.KeyOverrides)
	}
	if got.SecretOverrides["forge_token"] != alive.ID {
		t.Fatalf("stored secret_overrides = %v, want the live pin kept", got.SecretOverrides)
	}
	// The delete route's receipt convention carries here: the audit line
	// names the surface that lost a pin.
	cleared := sweptSurfaces(t, auditStore, "team-a", "webhook.created")
	if len(cleared) != 1 || cleared[0] != "webhook "+created.Config.ID {
		t.Fatalf("audit pins_cleared = %v, want [webhook %s]", cleared, created.Config.ID)
	}
}

// TestUpdateWebhook_SweepsAPinWhoseRecordDiedMidFlight: the webhook lane's
// edit site, and the secret half of the re-check.
func TestUpdateWebhook_SweepsAPinWhoseRecordDiedMidFlight(t *testing.T) {
	srv, _, _, _ := newPinSweepServer(t)
	dying := seedSweepSecret(t, srv, "team-a", "sec-dying")
	seedSweepWebhook(t, srv.webhookConfigs, webhooks.Config{ID: "wh-1", TenantID: "team-a"})

	srv.webhookConfigs = &midFlightConfigStore{
		ConfigStore: srv.webhookConfigs,
		once: func() {
			if err := srv.genericSecrets.Delete(store.WithTenant(t.Context(), "team-a"), dying.ID); err != nil {
				t.Fatalf("seed secret delete: %v", err)
			}
		},
	}

	body := `{"secret_overrides":{"forge_token":"` + dying.ID + `"}}`
	r := httptest.NewRequest(http.MethodPut, "/api/teams/team-a/webhooks/wh-1", strings.NewReader(body))
	r.SetPathValue("id", "team-a")
	r.SetPathValue("webhook_id", "wh-1")
	ctx := testMintVerdictContext(store.WithTenant(auth.WithIdentity(r.Context(),
		auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.handleUpdateWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got, err := srv.webhookConfigs.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SecretOverrides) != 0 {
		t.Fatalf("stored secret_overrides = %v, want the swept clear — the edit's pin named a secret that died mid-flight", got.SecretOverrides)
	}
}

// TestCreateTrigger_KeepsAPinWhoseRecordSurvives: the reconcile is a
// dead-record sweep, not a pin clear — pins naming live records stand and
// write nothing.
func TestCreateTrigger_KeepsAPinWhoseRecordSurvives(t *testing.T) {
	srv, org, sec := triggerPinServer(t)

	body := `{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},
		"key_overrides":{"anthropic":"` + org.ID + `"},
		"secret_overrides":{"forge_token":"` + sec.ID + `"}}`
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d; body=%s", rec.Code, rec.Body.String())
	}
	var created trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	got, err := srv.cfg.TriggerStore.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyOverrides["anthropic"] != org.ID || got.SecretOverrides["forge_token"] != sec.ID {
		t.Fatalf("stored pins = %v / %v, want both kept — the reconcile must not sweep a live record's pin", got.KeyOverrides, got.SecretOverrides)
	}
}

// A failed re-check after a CREATE is a receipt on the 201, never a 5xx:
// the row is already stored under a fresh id, a client retrying a 5xx
// mints a second subscription or a second webhook, the 5xx body is opaque
// by convention so the id could not even travel in it, and a webhook's
// token exists only in that one answer. The witnesses below read the
// answer for the row and the `pins_unreconciled` receipt, the store for
// the single row, and the log for the cause under the row id. The EDIT
// routes keep the explicit 5xx: nothing one-time is lost there, and the
// retry's own re-check, over every pin the row holds, is the mend.

// TestCreateTrigger_AFailedReCheckAnswersTheRowWithAReceipt: the key store
// blips on the re-check (the validator's Get passed). Answering 500 (the
// mutant) loses the id behind an opaque body and sends the retry minting a
// duplicate cron subscription.
func TestCreateTrigger_AFailedReCheckAnswersTheRowWithAReceipt(t *testing.T) {
	srv, org, _ := triggerPinServer(t)
	var logs bytes.Buffer
	srv.logger = iterlog.New(iterlog.LevelWarn, &logs)
	srv.apiKeys = &getThenFailKeyStore{ApiKeyStore: srv.apiKeys, pass: 1}

	body := `{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},
		"key_overrides":{"anthropic":"` + org.ID + `"}}`
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 — the row is stored, a 5xx would hide its id and duplicate it on retry; body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		trigger.Subscription
		PinsUnreconciled []string `json:"pins_unreconciled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatalf("the answer carries no id: %s", rec.Body.String())
	}
	if len(created.PinsUnreconciled) != 1 || created.PinsUnreconciled[0] != org.ID {
		t.Fatalf("pins_unreconciled = %v, want [%s] — the client must learn which record it has to re-check", created.PinsUnreconciled, org.ID)
	}
	// The pin the route could not vouch for stays in the answer as stored:
	// a receipt is not a clear.
	if created.KeyOverrides["anthropic"] != org.ID {
		t.Fatalf("the answer dropped an unreconciled pin: %v", created.KeyOverrides)
	}
	rows, err := srv.cfg.TriggerStore.ListByTenant(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != created.ID {
		t.Fatalf("stored rows = %+v, want exactly the answered one", rows)
	}
	if !strings.Contains(logs.String(), created.ID) || !strings.Contains(logs.String(), "store unavailable") {
		t.Fatalf("the log must carry the cause under the row id: %q", logs.String())
	}
	// The receipt names ids only; the store's error text stays in the log.
	if strings.Contains(rec.Body.String(), "store unavailable") {
		t.Fatalf("the answer leaks the store error: %s", rec.Body.String())
	}
}

// TestCreateWebhook_AFailedReCheckStillDeliversTheToken: the webhook lane's
// create, where the 5xx (the mutant) would withhold the only copy of the
// token for a row that exists — unknowable, unrecoverable short of a rotate
// the client cannot address without the id.
func TestCreateWebhook_AFailedReCheckStillDeliversTheToken(t *testing.T) {
	srv, _, logs, auditStore := newPinSweepServer(t)
	key := seedSweepKey(t, srv.apiKeys, "team-a", "key-live", secrets.ProviderAnthropic)
	srv.apiKeys = &getThenFailKeyStore{ApiKeyStore: srv.apiKeys, pass: 1}

	body := `{"name":"wh","provider":"gitlab","wildcard_bots":true,
		"key_overrides":{"anthropic":"` + key.ID + `"}}`
	r := httptest.NewRequest(http.MethodPost, "/api/teams/team-a/webhooks", strings.NewReader(body))
	r.SetPathValue("id", "team-a")
	ctx := testMintVerdictContext(store.WithTenant(auth.WithIdentity(r.Context(),
		auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.handleCreateWebhook(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 — the row is stored and the token has no second copy; body=%s", w.Code, w.Body.String())
	}
	var created webhookWithToken
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Token == "" || created.Config.ID == "" {
		t.Fatalf("the answer must carry the token and the id: %s", w.Body.String())
	}
	if len(created.PinsUnreconciled) != 1 || created.PinsUnreconciled[0] != key.ID {
		t.Fatalf("pins_unreconciled = %v, want [%s]", created.PinsUnreconciled, key.ID)
	}
	if created.Config.KeyOverrides["anthropic"] != key.ID {
		t.Fatalf("the answer dropped an unreconciled pin: %v", created.Config.KeyOverrides)
	}
	rows, err := srv.webhookConfigs.ListByTenant(context.Background(), "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != created.Config.ID {
		t.Fatalf("stored rows = %+v, want exactly the answered one", rows)
	}
	// The audit line carries the receipt beside the create, and the log the
	// cause under the row id.
	if got := auditPinReceipt(t, auditStore, "team-a", "webhook.created", "pins_unreconciled"); len(got) != 1 || got[0] != key.ID {
		t.Fatalf("audit pins_unreconciled = %v, want [%s]", got, key.ID)
	}
	if !strings.Contains(logs.String(), created.Config.ID) || !strings.Contains(logs.String(), "store unavailable") {
		t.Fatalf("the log must carry the cause under the row id: %q", logs.String())
	}
}

// TestUpdateTrigger_AFailedReCheckAnswersAnExplicitFailure: the edit lane
// keeps the 5xx — the edit is stored, nothing one-time is lost, and the
// retry's own re-check is the mend
// (TestUpdateTrigger_TheRetryOfAFailedReCheckSweepsTheStoredPin). A 200
// over an unvouched pin (the mutant) would be the silent-sealed-default the
// re-check exists to close.
func TestUpdateTrigger_AFailedReCheckAnswersAnExplicitFailure(t *testing.T) {
	srv, org, _ := triggerPinServer(t)
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "",
		`{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]}}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d; body=%s", rec.Code, rec.Body.String())
	}
	var created trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	srv.apiKeys = &getThenFailKeyStore{ApiKeyStore: srv.apiKeys, pass: 1}

	repl := httptest.NewRecorder()
	srv.handleUpdateTrigger(repl, teamTriggerReq(t, http.MethodPut, "/api/v1/triggers/"+created.ID, created.ID,
		`{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},
		"key_overrides":{"anthropic":"`+org.ID+`"}}`))
	if repl.Code != http.StatusInternalServerError {
		t.Fatalf("update = %d, want 500; body=%s", repl.Code, repl.Body.String())
	}
	got, err := srv.cfg.TriggerStore.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyOverrides["anthropic"] != org.ID {
		t.Fatalf("stored key_overrides = %v, want the edit — the 5xx reports the re-check, it does not undo the write", got.KeyOverrides)
	}
}

// TestUpdateWebhook_AFailedReCheckAnswersAnExplicitFailure: the webhook
// lane's edit, same posture.
func TestUpdateWebhook_AFailedReCheckAnswersAnExplicitFailure(t *testing.T) {
	srv, _, _, _ := newPinSweepServer(t)
	key := seedSweepKey(t, srv.apiKeys, "team-a", "key-live", secrets.ProviderAnthropic)
	seedSweepWebhook(t, srv.webhookConfigs, webhooks.Config{ID: "wh-1", TenantID: "team-a"})
	srv.apiKeys = &getThenFailKeyStore{ApiKeyStore: srv.apiKeys, pass: 1}

	body := `{"key_overrides":{"anthropic":"` + key.ID + `"}}`
	r := httptest.NewRequest(http.MethodPut, "/api/teams/team-a/webhooks/wh-1", strings.NewReader(body))
	r.SetPathValue("id", "team-a")
	r.SetPathValue("webhook_id", "wh-1")
	ctx := testMintVerdictContext(store.WithTenant(auth.WithIdentity(r.Context(),
		auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.handleUpdateWebhook(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("update = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	got, err := srv.webhookConfigs.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyOverrides["anthropic"] != key.ID {
		t.Fatalf("stored key_overrides = %v, want the edit — the 5xx reports the re-check, it does not undo the write", got.KeyOverrides)
	}
}

// TestUpdateTrigger_TheRetryOfAFailedReCheckSweepsTheStoredPin: the mend
// behind the edit's 5xx. The pinned key dies with no sweep behind it, and
// the next edit — a pause, as the studio's toggle sends it, with no pin map,
// so nothing is validated — meets a key store that cannot answer its
// re-check: 5xx, the pause stored, the dead pin standing. The retry of the
// same request lands on the same row, and its own re-check, which reads
// every pin the row holds and not only the maps the request names, sweeps
// the dead pin. A re-check bounded to the request's maps (the mutant)
// re-checks nothing on either pass: the first answers 200 over the dead
// pin.
func TestUpdateTrigger_TheRetryOfAFailedReCheckSweepsTheStoredPin(t *testing.T) {
	srv, org, _ := triggerPinServer(t)
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "",
		`{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},
		"key_overrides":{"anthropic":"`+org.ID+`"}}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d; body=%s", rec.Code, rec.Body.String())
	}
	var created trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	keys := srv.apiKeys
	if err := keys.Delete(store.WithTenant(t.Context(), "t1"), org.ID); err != nil {
		t.Fatal(err)
	}

	pause := `{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},"enabled":false}`
	srv.apiKeys = &getThenFailKeyStore{ApiKeyStore: keys}
	first := httptest.NewRecorder()
	srv.handleUpdateTrigger(first, teamTriggerReq(t, http.MethodPut, "/api/v1/triggers/"+created.ID, created.ID, pause))
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("a pause whose re-check meets the outage = %d, want 500 — the stored pin is the row's, and the re-check owes it a read; body=%s", first.Code, first.Body.String())
	}
	if got := getSub(t, srv.cfg.TriggerStore, created.ID); got.Enabled || got.KeyOverrides["anthropic"] != org.ID {
		t.Fatalf("after the failed re-check: enabled=%v keys=%v, want the pause stored and the dead pin standing — the state the retry mends", got.Enabled, got.KeyOverrides)
	}

	srv.apiKeys = keys
	retry := httptest.NewRecorder()
	srv.handleUpdateTrigger(retry, teamTriggerReq(t, http.MethodPut, "/api/v1/triggers/"+created.ID, created.ID, pause))
	if retry.Code != http.StatusOK {
		t.Fatalf("the retry = %d, want 200; body=%s", retry.Code, retry.Body.String())
	}
	if got := getSub(t, srv.cfg.TriggerStore, created.ID); len(got.KeyOverrides) != 0 {
		t.Fatalf("stored key_overrides after the retry = %v, want the dead pin swept — the retry validated nothing, so its re-check is the whole mend", got.KeyOverrides)
	}
	var answered trigger.Subscription
	if err := json.Unmarshal(retry.Body.Bytes(), &answered); err != nil {
		t.Fatal(err)
	}
	if _, kept := answered.KeyOverrides["anthropic"]; kept {
		t.Fatalf("the retry's answer resells the swept pin: %v", answered.KeyOverrides)
	}
}

// TestUpdateWebhook_TheRetryOfAFailedReCheckSweepsTheStoredPin is the
// webhook lane's twin: a rename sends no pin map, so neither it nor its
// retry validates the stored pin, and the retry's re-check over the row's
// pins is what sweeps the dead one.
func TestUpdateWebhook_TheRetryOfAFailedReCheckSweepsTheStoredPin(t *testing.T) {
	srv, spy, _, _ := newPinSweepServer(t)
	dead := seedSweepKey(t, spy, "team-a", "key-dead", secrets.ProviderAnthropic)
	seedSweepWebhook(t, srv.webhookConfigs, webhooks.Config{ID: "wh-1", TenantID: "team-a",
		KeyOverrides: map[string]string{"anthropic": dead.ID}})
	if err := spy.Delete(store.WithTenant(t.Context(), "team-a"), dead.ID); err != nil {
		t.Fatal(err)
	}
	rename := func(keys secrets.ApiKeyStore) *httptest.ResponseRecorder {
		srv.apiKeys = keys
		r := httptest.NewRequest(http.MethodPut, "/api/teams/team-a/webhooks/wh-1", strings.NewReader(`{"name":"renamed"}`))
		r.SetPathValue("id", "team-a")
		r.SetPathValue("webhook_id", "wh-1")
		r = r.WithContext(testMintVerdictContext(store.WithTenant(auth.WithIdentity(r.Context(),
			auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a")))
		w := httptest.NewRecorder()
		srv.handleUpdateWebhook(w, r)
		return w
	}

	if w := rename(&getThenFailKeyStore{ApiKeyStore: spy}); w.Code != http.StatusInternalServerError {
		t.Fatalf("a rename whose re-check meets the outage = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	got, err := srv.webhookConfigs.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.KeyOverrides["anthropic"] != dead.ID {
		t.Fatalf("after the failed re-check: name=%q keys=%v, want the rename stored and the dead pin standing — the state the retry mends", got.Name, got.KeyOverrides)
	}

	w := rename(spy)
	if w.Code != http.StatusOK {
		t.Fatalf("the retry = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got, err = srv.webhookConfigs.Get(context.Background(), "wh-1"); err != nil {
		t.Fatal(err)
	}
	if len(got.KeyOverrides) != 0 {
		t.Fatalf("stored key_overrides after the retry = %v, want the dead pin swept — the retry validated nothing, so its re-check is the whole mend", got.KeyOverrides)
	}
	var answered webhooks.Config
	if err := json.Unmarshal(w.Body.Bytes(), &answered); err != nil {
		t.Fatal(err)
	}
	if _, kept := answered.KeyOverrides["anthropic"]; kept {
		t.Fatalf("the retry's answer resells the swept pin: %v", answered.KeyOverrides)
	}
}
