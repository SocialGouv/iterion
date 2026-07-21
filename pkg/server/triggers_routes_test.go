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

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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
	coord := StartTriggerCoordinator(ns, subs, nil, &recordingLauncher{}, nil, nil, iterlog.New(iterlog.LevelError, nil))
	if coord == nil {
		t.Skip("trigger coordinator unavailable (fsnotify)")
	}
	t.Cleanup(coord.Close)
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
	coord := StartTriggerCoordinator(ns, subs, nil, rl, nil, nil, iterlog.New(iterlog.LevelError, nil))
	if coord == nil {
		t.Skip("trigger coordinator unavailable (fsnotify)")
	}
	t.Cleanup(coord.Close)

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
