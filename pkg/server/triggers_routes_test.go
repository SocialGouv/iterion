package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

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

func TestTriggers_CreateRequiresBot(t *testing.T) {
	srv := newTriggerTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, httptest.NewRequest(http.MethodPost, "/api/v1/triggers", bytes.NewBufferString(`{"match":{}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create without bot_id status = %d; want 400", rec.Code)
	}
}
