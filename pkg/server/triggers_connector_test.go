package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

const chatBotMain = "schema out:\n  ok: bool\n\nvars:\n  prompt: string\n  tone: string = \"calm\"\n\ntool work:\n  command: `printf '{\"ok\":true}'`\n  output: out\n\nworkflow chatbot:\n  worktree: none\n  entry: work\n  work -> done\n"

const chatBotManifest = `name: chatbot
version: 1.0.0
invocations:
  - kind: connector
    connector:
      trigger: chat.message.posted
      filters:
        room: [ops]
      vars:
        prompt: event.prompt
`

type connectorTriggerWorld struct {
	s     *Server
	conns *connection.MemoryStore
}

func newConnectorTriggerWorld(t *testing.T) *connectorTriggerWorld {
	t.Helper()
	pkg := &spec.Package{
		Connector: spec.Connector{ID: "chat"},
		Ingress: &spec.Ingress{
			Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "body", Name: "token", Secret: "vendor"},
			Headers:    []string{"X-Chat-Listed"},
		},
		Triggers: []spec.Trigger{{ID: "chat.message.posted", Event: "deploy",
			Filters:    []spec.TriggerFilter{{ID: "room", From: "payload.room", Type: "enum", Options: []string{"ops", "dev", "bots"}}},
			Outputs:    []spec.TriggerOutput{{Name: "prompt", From: "payload.text"}},
			Ignore:     "payload.room == 'bots'",
			SampleBody: []byte(`{"headers":{"X-Chat-Listed":"sampled"},"payload":{"room":"ops","text":"from the sample","token":"CANARY-SAMPLE"}}`),
		}},
	}
	f := newIngressFixture(t, pkg)
	f.s.connectorCatalog = connection.NewMemoryCatalog(pkg)
	other := ingConnection("mail")
	if err := f.conns.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	ungranted := ingConnection("chat")
	ungranted.ID, ungranted.Alias, ungranted.Capabilities = "conn-chat-actions", "chat-actions", []connection.Capability{connection.CapAction}
	if err := f.conns.Create(context.Background(), ungranted); err != nil {
		t.Fatal(err)
	}
	f.s.cfg.TriggerStore = trigger.NewMemorySubscriptionStore()
	botsDir := t.TempDir()
	dir := filepath.Join(botsDir, "chatbot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"main.bot": chatBotMain, "manifest.yaml": chatBotManifest} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.s.cfg.Bots.Paths = []string{botsDir}
	return &connectorTriggerWorld{s: f.s, conns: f.conns}
}

func teamT1(r *http.Request) *http.Request {
	return r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "admin", TeamID: "t1", IsSuperAdmin: true}))
}

func (w *connectorTriggerWorld) create(body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	w.s.handleCreateTrigger(rec, teamT1(httptest.NewRequest(http.MethodPost, "/api/v1/triggers", bytes.NewBufferString(body))))
	return rec
}

const validConnectorSub = `"bot_id":"chatbot","invocation":"connector","connection_id":"conn-chat","trigger_id":"chat.message.posted","match":{}`

// A connector subscription is judged at save against its connection, its
// package's trigger and its bot, and stored with the server's match.
func TestAConnectorSubscriptionIsJudgedAtSave(t *testing.T) {
	w := newConnectorTriggerWorld(t)
	rec := w.create(`{` + validConnectorSub + `,"filters":{"room":["ops"]},"match":{"sources":["board"]}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a valid connector subscription = %d %s", rec.Code, rec.Body.String())
	}
	var got trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Match.Sources) != 1 || got.Match.Sources[0] != trigger.SourceConnector || got.ConnectorKey != "conn-chat/chat.message.posted" {
		t.Fatalf("stored match %+v key %q, want the server's connector match and key", got.Match, got.ConnectorKey)
	}

	for name, tc := range map[string]struct{ body, want string }{
		"unknown connection":   {`"bot_id":"chatbot","invocation":"connector","connection_id":"conn-x","trigger_id":"chat.message.posted","match":{}`, "not found"},
		"another connector":    {`"bot_id":"chatbot","invocation":"connector","connection_id":"conn-mail","trigger_id":"chat.message.posted","match":{}`, "belongs to connector"},
		"no trigger grant":     {`"bot_id":"chatbot","invocation":"connector","connection_id":"conn-chat-actions","trigger_id":"chat.message.posted","match":{}`, "trigger"},
		"undeclared trigger":   {`"bot_id":"chatbot","invocation":"connector","connection_id":"conn-chat","trigger_id":"chat.message.edited","match":{}`, "declares no trigger"},
		"unknown bot":          {`"bot_id":"ghost","invocation":"connector","connection_id":"conn-chat","trigger_id":"chat.message.posted","match":{}`, "not found"},
		"filter option":        {validConnectorSub + `,"filters":{"room":["lobby"]}`, "options"},
		"required var dropped": {validConnectorSub + `,"var_map":{"tone":"event.prompt"}`, "nothing fills it"},
		"connection elsewhere": {`"bot_id":"chatbot","invocation":"forge","connection_id":"conn-chat","match":{}`, "belong to a connector subscription"},
	} {
		rec := w.create(`{` + tc.body + `}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %d %s, want 400 naming %q", name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

// An edit that changes a connector binding is judged again; a pause is not,
// so a subscription whose connection is gone stays pausable.
func TestAConnectorEditIsJudgedOnlyWhenItRebinds(t *testing.T) {
	w := newConnectorTriggerWorld(t)
	rec := w.create(`{` + validConnectorSub + `}`)
	var sub trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	put := func(body string) *httptest.ResponseRecorder {
		req := teamT1(httptest.NewRequest(http.MethodPut, "/api/v1/triggers/"+sub.ID, bytes.NewBufferString(body)))
		req.SetPathValue("id", sub.ID)
		rec := httptest.NewRecorder()
		w.s.handleUpdateTrigger(rec, req)
		return rec
	}
	if rec := put(`{` + validConnectorSub + `,"filters":{"room":["lobby"]}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a re-filtering edit with a bad option = %d, want 400", rec.Code)
	}
	if err := w.conns.Delete(context.Background(), "t1", "conn-chat"); err != nil {
		t.Fatal(err)
	}
	if rec := put(`{` + validConnectorSub + `,"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("pausing a subscription whose connection is gone = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// A bot's connector invocation is enabled onto a connection, under the id
// its invocation and that connection derive; again is a 409.
func TestAConnectorInvocationIsEnabledOntoAConnection(t *testing.T) {
	w := newConnectorTriggerWorld(t)
	post := func(body string) *httptest.ResponseRecorder {
		req := teamT1(httptest.NewRequest(http.MethodPost, "/api/v1/bots/chatbot/triggers/from-invocation", bytes.NewBufferString(body)))
		req.SetPathValue("name", "chatbot")
		rec := httptest.NewRecorder()
		w.s.handleTriggerFromInvocation(rec, req)
		return rec
	}
	if rec := post(`{"index":0}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "connection_id") {
		t.Fatalf("no connection = %d %s, want 400 asking for connection_id", rec.Code, rec.Body.String())
	}
	rec := post(`{"index":0,"connection_id":"conn-chat"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enable = %d %s", rec.Code, rec.Body.String())
	}
	var sub trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	want := trigger.DerivedSubscriptionID("t1", "chatbot", bundle.InvocationKindConnector, "chat.message.posted", "conn-chat")
	if sub.ID != want || sub.VarMap["prompt"] != "event.prompt" || sub.Filters["room"][0] != "ops" || sub.Match.Sources[0] != trigger.SourceConnector {
		t.Fatalf("enabled = %+v, want id %s with the manifest's filters and vars", sub, want)
	}
	if rec := post(`{"index":0,"connection_id":"conn-chat"}`); rec.Code != http.StatusConflict {
		t.Fatalf("enable twice = %d, want 409", rec.Code)
	}
	if rec := post(`{"index":0,"connection_id":"conn-mail"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("enable onto another connector's connection = %d, want 400", rec.Code)
	}
}

func (w *connectorTriggerWorld) preview(body string) (*httptest.ResponseRecorder, triggerPreview) {
	rec := httptest.NewRecorder()
	w.s.handleTriggerPreview(rec, teamT1(httptest.NewRequest(http.MethodPost, "/api/v1/triggers/preview", bytes.NewBufferString(body))))
	var out triggerPreview
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// The preview reads a delivery as the lane, the matcher and the launch
// would — the credential never in what it answers — and writes nothing.
func TestThePreviewReadsADeliveryAndWritesNothing(t *testing.T) {
	w := newConnectorTriggerWorld(t)
	base := `"bot_id":"chatbot","connection_id":"conn-chat","trigger_id":"chat.message.posted","filters":{"room":["ops"]}`
	rec, out := w.preview(`{` + base + `,"payload":{"room":"ops","text":"ship it","token":"CANARY-PASTED"},"headers":{"X-Chat-Listed":"yes","X-Other":"CANARY-HEADER"}}`)
	if rec.Code != http.StatusOK || !out.Matched || out.Vars["prompt"] != "ship it" {
		t.Fatalf("a matching delivery = %d %s, want matched with its vars", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(out.Event), `"X-Chat-Listed":"yes"`) {
		t.Fatalf("the event = %s, want the pasted listed header", out.Event)
	}
	for _, canary := range []string{"CANARY-PASTED", "CANARY-HEADER"} {
		if strings.Contains(rec.Body.String(), canary) {
			t.Fatalf("the preview answered %s: %s", canary, rec.Body.String())
		}
	}
	if _, out := w.preview(`{` + base + `,"payload":{"room":"dev","text":"x"}}`); out.Matched || out.Reason == "" {
		t.Fatalf("a delivery for another room = %+v, want not matched with its reason", out)
	}
	if _, out := w.preview(`{` + base + `,"payload":{"room":"bots","text":"x"}}`); out.Filtered == "" {
		t.Fatalf("a delivery the loop guard drops = %+v, want filtered", out)
	}
	rec, out = w.preview(`{` + base + `}`)
	if rec.Code != http.StatusOK || !out.Sample || !out.Matched || out.Vars["prompt"] != "from the sample" || strings.Contains(rec.Body.String(), "CANARY-SAMPLE") {
		t.Fatalf("no payload = %d %s, want the sample read and judged, its credential gone", rec.Code, rec.Body.String())
	}
	if rec, _ := w.preview(`{"bot_id":"chatbot","connection_id":"conn-chat","trigger_id":"chat.message.posted","var_map":{"tone":"event.prompt"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a subscription the save would refuse = %d, want 400", rec.Code)
	}
	if rows, _ := w.s.cfg.TriggerStore.ListByTenant(context.Background(), "t1"); len(rows) != 0 {
		t.Fatalf("the preview stored %d subscription(s)", len(rows))
	}
}

// TestTriggerCreateRefusesADuplicateBinding: one binding, one row — the
// same bot listening to the same declared trigger on the same connection
// twice would fire the bot twice per delivery. The refusal is the
// SERVER's (409 naming the row), so every client of the route is
// protected, not just the wizard.
func TestTriggerCreateRefusesADuplicateBinding(t *testing.T) {
	w := newConnectorTriggerWorld(t)
	if rec := w.create(`{` + validConnectorSub + `}`); rec.Code != http.StatusCreated {
		t.Fatalf("first binding: %d %s", rec.Code, rec.Body.String())
	}
	rec := w.create(`{` + validConnectorSub + `}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate binding: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already binds") {
		t.Fatalf("the refusal should name the row: %s", rec.Body.String())
	}
}

// A subscription nothing can feed is refused at save: a webhook trigger of a
// package that declares no ingress (no delivery reaches it), and a polled
// trigger (no poller runs yet). The catalogue offers neither, and the save
// agrees with it.
func TestASubscriptionNothingCanFeedIsRefusedAtSave(t *testing.T) {
	w := newConnectorTriggerWorld(t)
	pkg, _, err := w.s.connectorCatalog.Package("chat")
	if err != nil {
		t.Fatal(err)
	}
	noIngress := *pkg
	noIngress.Ingress = nil
	w.s.connectorCatalog = connection.NewMemoryCatalog(&noIngress)
	if rec := w.create(`{` + validConnectorSub + `}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "declares no ingress") {
		t.Fatalf("a trigger of a package with no ingress: %d %s, want 400 naming it", rec.Code, rec.Body.String())
	}
	polled := *pkg
	polled.Triggers = []spec.Trigger{{ID: "chat.message.posted", Delivery: spec.DeliveryPoll}}
	w.s.connectorCatalog = connection.NewMemoryCatalog(&polled)
	rec := w.create(`{` + validConnectorSub + `}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "runs no pollers yet") {
		t.Fatalf("a polled trigger: %d %s, want 400 naming why", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "L4") || strings.Contains(rec.Body.String(), "#3274") {
		t.Fatalf("the refusal speaks of the project's lots, not the operator's deployment: %s", rec.Body.String())
	}
}

// triggerEventNames is what a filtered delivery's answer tells the operator
// the package DOES believe: the events a delivery can carry, never a polled
// trigger's empty name.
func TestTriggerEventNamesListsWhatADeliveryCanCarry(t *testing.T) {
	polled := spec.Trigger{ID: "chat.message.listed", Delivery: spec.DeliveryPoll}
	for _, tc := range []struct {
		triggers []spec.Trigger
		want     string
	}{
		{nil, "no events at all"},
		{[]spec.Trigger{polled}, "no event a delivery can carry"},
		{[]spec.Trigger{polled, {ID: "chat.message.posted", Event: "deploy"}}, "deploy"},
	} {
		if got := triggerEventNames(tc.triggers); got != tc.want {
			t.Errorf("triggerEventNames(%d triggers) = %q, want %q", len(tc.triggers), got, tc.want)
		}
	}
}
