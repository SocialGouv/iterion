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
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/binding"
	"github.com/SocialGouv/iterion/pkg/connector/poller"
	"github.com/SocialGouv/iterion/pkg/connector/polling"
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

// A webhook trigger of a package that declares no ingress is refused at
// save: no delivery reaches it, and the catalogue does not offer it either.
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
}

// polledChat turns the world's chat trigger into a polled one with a
// required query, and gives the world a poller store.
func polledChat(t *testing.T, w *connectorTriggerWorld) {
	t.Helper()
	pkg, _, err := w.s.connectorCatalog.Package("chat")
	if err != nil {
		t.Fatal(err)
	}
	polled := *pkg
	trig := pkg.Triggers[0]
	trig.Event, trig.Delivery = "", spec.DeliveryPoll
	trig.Poll = &spec.TriggerPoll{Operation: "chat.message.search", Query: []spec.PollQueryField{{ID: "room", Required: true}},
		Interval: spec.PollInterval{Default: "5m", Min: "1m"}}
	trig.SampleBody = nil
	polled.Triggers = []spec.Trigger{trig}
	w.s.connectorCatalog = connection.NewMemoryCatalog(&polled)
	w.s.connectorPollers = poller.NewMemoryStore()
	// Polling spends the connection's credential: the action grant.
	conn, err := w.conns.Get(context.Background(), "t1", "conn-chat")
	if err != nil {
		t.Fatal(err)
	}
	conn.Capabilities = []connection.Capability{connection.CapTrigger, connection.CapAction}
	if err := w.conns.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
}

// A subscription to a polled trigger names its query; the server derives
// the poller, writes it before the subscription, and records the sequence
// the subscription was activated at — at each activation, never on an edit
// that keeps it listening.
func TestAPolledSubscriptionActivatesOnItsPoller(t *testing.T) {
	w := newConnectorTriggerWorld(t)
	polledChat(t, w)
	ctx := context.Background()
	query := map[string]string{"room": "ops"}
	pid := poller.ID(connection.LocalTenant, "conn-chat", "chat.message.posted", query)
	if pid == poller.ID("t1", "conn-chat", "chat.message.posted", query) {
		t.Fatal("the fixture cannot tell the tenants apart")
	}

	// A poller that already accepted items: a new subscription starts after
	// them.
	tenant := binding.ConnectionTenant(w.s.triggerTenant(teamT1(httptest.NewRequest(http.MethodGet, "/", nil))))
	p, _, err := w.s.connectorPollers.Ensure(ctx, poller.Poller{TenantID: tenant, ConnectionID: "conn-chat", TriggerID: "chat.message.posted", Query: query, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	p, _ = w.s.connectorPollers.Acquire(ctx, p.ID, 0, "w", time.Now())
	if _, err := w.s.connectorPollers.Reserve(ctx, p.ID, p.Generation, 4); err != nil {
		t.Fatal(err)
	}

	rec := w.create(`{` + validConnectorSub + `,"poll_query":{"room":"ops"},"backfill":2}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a polled subscription = %d %s", rec.Code, rec.Body.String())
	}
	var got trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Poller != p.ID || got.PollerSince != 4 || got.Backfill != 2 || got.BackfillDone {
		t.Fatalf("stored = poller %q since %d backfill %d done %v, want %s since 4 with its backfill owed", got.Poller, got.PollerSince, got.Backfill, got.BackfillDone, p.ID)
	}

	// Paused, then resumed after more items: the resume is an activation.
	got.BackfillDone = true
	if err := w.s.cfg.TriggerStore.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	update := func(body string) trigger.Subscription {
		t.Helper()
		rec := httptest.NewRecorder()
		req := teamT1(httptest.NewRequest(http.MethodPut, "/api/v1/triggers/"+got.ID, bytes.NewBufferString(body)))
		req.SetPathValue("id", got.ID)
		w.s.handleUpdateTrigger(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("update %s = %d %s", body, rec.Code, rec.Body.String())
		}
		var out trigger.Subscription
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	paused := update(`{` + validConnectorSub + `,"poll_query":{"room":"ops"},"backfill":2,"enabled":false}`)
	if paused.PollerSince != 4 || !paused.BackfillDone {
		t.Fatalf("a pause = since %d done %v, want both kept", paused.PollerSince, paused.BackfillDone)
	}
	if _, err := w.s.connectorPollers.Reserve(ctx, p.ID, p.Generation, 3); err != nil {
		t.Fatal(err)
	}
	resumed := update(`{` + validConnectorSub + `,"poll_query":{"room":"ops"},"backfill":2,"enabled":true}`)
	if resumed.PollerSince != 7 || resumed.BackfillDone {
		t.Fatalf("a resume = since %d done %v, want 7 and the backfill owed again", resumed.PollerSince, resumed.BackfillDone)
	}
	kept := update(`{` + validConnectorSub + `,"poll_query":{"room":"ops"},"backfill":2,"enabled":true,"when":"event.prompt != ''"}`)
	if kept.PollerSince != 7 {
		t.Fatalf("an edit that keeps it listening moved since to %d", kept.PollerSince)
	}
	// Another query is another poller, written before the subscription.
	moved := update(`{` + validConnectorSub + `,"poll_query":{"room":"dev"},"enabled":true}`)
	other := poller.ID(tenant, "conn-chat", "chat.message.posted", map[string]string{"room": "dev"})
	if moved.Poller != other || moved.PollerSince != 0 {
		t.Fatalf("a new query = poller %q since %d, want %s since 0", moved.Poller, moved.PollerSince, other)
	}
	if _, err := w.s.connectorPollers.Get(ctx, tenant, other); err != nil {
		t.Fatalf("the new query's poller was not written: %v", err)
	}
}

func TestAPolledSubscriptionIsRefusedWhatItCannotListenTo(t *testing.T) {
	w := newConnectorTriggerWorld(t)
	polledChat(t, w)
	for name, tc := range map[string]struct{ body, want string }{
		"a client-named poller": {validConnectorSub + `,"poll_query":{"room":"ops"},"poller":"pl-mine"`, "derived from it"},
		"its query unset":       {validConnectorSub, "requires it"},
	} {
		if rec := w.create(`{` + tc.body + `}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %d %s, want 400 naming %q", name, rec.Code, rec.Body.String(), tc.want)
		}
	}
	w.s.connectorPollers = nil
	// The server's, not the request's: a 503, like a server with no
	// connection plane.
	if rec := w.create(`{` + validConnectorSub + `,"poll_query":{"room":"ops"}}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a server with no poller store: %d %s, want 503", rec.Code, rec.Body.String())
	}
	if n, _ := w.s.cfg.TriggerStore.ListByBot(context.Background(), w.s.triggerTenant(teamT1(httptest.NewRequest(http.MethodGet, "/", nil))), "chatbot"); len(n) != 0 {
		t.Errorf("a subscription no poller can feed was stored: %+v", n)
	}
}

// A polled trigger's preview reads an item as its poller would: the item
// is the event's payload.
func TestThePreviewReadsAPolledItem(t *testing.T) {
	w := newConnectorTriggerWorld(t)
	polledChat(t, w)
	rec, got := w.preview(`{"bot_id":"chatbot","connection_id":"conn-chat","trigger_id":"chat.message.posted","poll_query":{"room":"ops"},"payload":{"room":"ops","text":"polled one"}}`)
	if rec.Code != http.StatusOK || !got.Matched || got.Vars["prompt"] != "polled one" {
		t.Fatalf("preview = %d %+v, want the item matched and its output in the vars", rec.Code, got)
	}
	if _, got := w.preview(`{"bot_id":"chatbot","connection_id":"conn-chat","trigger_id":"chat.message.posted","poll_query":{"room":"ops"},"payload":{"room":"bots","text":"loop"}}`); got.Filtered == "" {
		t.Fatalf("the loop guard did not drop a polled item: %+v", got)
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

// The refusals the save of a polled subscription owes: a connection without
// the action grant polling spends, dials that keep the pollers off, a poller
// other than the derived one — while sending back the one a read answered is
// an ordinary edit.
func TestAPolledSaveRefusesWhatWouldNeverFire(t *testing.T) {
	w := newConnectorTriggerWorld(t)
	polledChat(t, w)
	ctx := context.Background()
	body := `{` + validConnectorSub + `,"poll_query":{"room":"ops"}}`

	t.Setenv(polling.EnvInterval, "5")
	if rec := w.create(body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a save while a dial keeps the pollers off = %d %s, want 503", rec.Code, rec.Body.String())
	}
	t.Setenv(polling.EnvInterval, "")

	conn, _ := w.conns.Get(ctx, "t1", "conn-chat")
	conn.Capabilities = []connection.Capability{connection.CapTrigger}
	if err := w.conns.Update(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if rec := w.create(body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "needs the action grant") {
		t.Fatalf("a save on a connection without action = %d %s, want 400 naming it", rec.Code, rec.Body.String())
	}
	conn, _ = w.conns.Get(ctx, "t1", "conn-chat")
	conn.Capabilities = []connection.Capability{connection.CapTrigger, connection.CapAction}
	if err := w.conns.Update(ctx, conn); err != nil {
		t.Fatal(err)
	}

	rec := w.create(body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	var got trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := teamT1(httptest.NewRequest(http.MethodPut, "/api/v1/triggers/"+got.ID, bytes.NewBufferString(body)))
		req.SetPathValue("id", got.ID)
		w.s.handleUpdateTrigger(rec, req)
		return rec
	}
	if rec := put(`{` + validConnectorSub + `,"poll_query":{"room":"ops"},"poller":"` + got.Poller + `","enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("a pause sending back the read row's poller = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := put(`{` + validConnectorSub + `,"poll_query":{"room":"ops"},"poller":"pl-other"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "derived") {
		t.Fatalf("an edit naming another poller = %d %s, want 400", rec.Code, rec.Body.String())
	}
}
