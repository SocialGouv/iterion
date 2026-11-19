package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

type deadLetterWorld struct {
	s       *Server
	intakes *intake.MemoryStore
	in      intake.Intake
}

func newDeadLetterWorld(t *testing.T) *deadLetterWorld {
	t.Helper()
	ctx := context.Background()
	s := newWebhookTestServer(t)
	store := intake.NewMemoryStore()
	s.connectorIntakes = store
	s.connectorCatalog = connection.NewMemoryCatalog(submitChatPackage())
	subs := trigger.NewMemorySubscriptionStore()
	s.cfg.TriggerStore = subs
	if err := subs.Create(ctx, trigger.Subscription{ID: "s-match", TenantID: "t1", BotID: "chatbot", Enabled: true,
		Invocation: bundle.InvocationKindConnector, ConnectionID: "conn-chat", TriggerID: "chat.message.posted",
		Filters: map[string][]string{"room": {"ops"}}}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	in, _, err := store.Accept(ctx, intake.Intake{TenantID: "t1", ConnectionID: "conn-chat", IngressID: "ing-1",
		TriggerID: "chat.message.posted", DedupKey: "d1", ReceivedAt: at,
		Event: []byte(`{"outputs":{"prompt":"ship it"},"payload":{"room":"ops"},"headers":{}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Accept(ctx, intake.Intake{TenantID: "t2", ConnectionID: "conn-x", IngressID: "ing-2",
		TriggerID: "chat.message.posted", DedupKey: "d2", ReceivedAt: at, Event: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	refused := intake.PairFor(in, "s-match", at)
	refused.State, refused.Reason = intake.PairRefused, "expression past its budget"
	gone := intake.PairFor(in, "s-gone", at)
	gone.State, gone.Reason = refused.State, refused.Reason
	live := intake.PairFor(in, "s-dead", at)
	if err := store.WritePairs(ctx, []intake.Pair{refused, gone, live}); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPairs(ctx, "w", at, time.Minute, 5)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %d, %v", len(claimed), err)
	}
	if err := store.BuryPair(ctx, claimed[0].ID, claimed[0].Generation, 3, "spent"); err != nil {
		t.Fatal(err)
	}
	return &deadLetterWorld{s: s, intakes: store, in: in}
}

func (w *deadLetterWorld) call(handler http.HandlerFunc, method, path string, values map[string]string) *httptest.ResponseRecorder {
	req := teamT1(httptest.NewRequest(method, path, nil))
	for k, v := range values {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestTheDeadLetterViewIsTheTeamsOwn(t *testing.T) {
	w := newDeadLetterWorld(t)
	rec := w.call(w.s.handleListIntakes, http.MethodGet, "/api/v1/intakes", nil)
	var list struct{ Intakes []intake.Intake }
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || len(list.Intakes) != 1 || list.Intakes[0].ID != w.in.ID {
		t.Fatalf("list = %d %s, want t1's one intake", rec.Code, rec.Body.String())
	}
	if rec := w.call(w.s.handleGetIntake, http.MethodGet, "/api/v1/intakes/x", map[string]string{"id": w.in.ID}); rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	other, _ := w.intakes.ListIntakes(context.Background(), "t2", "", 10)
	if rec := w.call(w.s.handleGetIntake, http.MethodGet, "/api/v1/intakes/x", map[string]string{"id": other[0].ID}); rec.Code != http.StatusNotFound {
		t.Fatalf("another team's intake = %d, want 404", rec.Code)
	}
	if rec := w.call(w.s.handleListPairs, http.MethodGet, "/api/v1/pairs?limit=0", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("a zero limit = %d, want 400", rec.Code)
	}
	rec = w.call(w.s.handleListPairs, http.MethodGet, "/api/v1/pairs?state=dead", nil)
	var pairs struct{ Pairs []intake.Pair }
	if err := json.Unmarshal(rec.Body.Bytes(), &pairs); err != nil || len(pairs.Pairs) != 1 || pairs.Pairs[0].SubscriptionID != "s-dead" {
		t.Fatalf("dead pairs = %s, want the buried one", rec.Body.String())
	}
}

// A dead pair replays once; a pair matching refused is re-judged against
// its subscription as it stands — never replayed into a launch.
func TestAReplayTakesThePairsOwnPath(t *testing.T) {
	w := newDeadLetterWorld(t)
	replay := func(sub string) (*httptest.ResponseRecorder, intake.Pair) {
		id := intake.PairID(w.in.ID, sub)
		rec := w.call(w.s.handleReplayPair, http.MethodPost, "/api/v1/pairs/x/replay", map[string]string{"id": id})
		var p intake.Pair
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		return rec, p
	}
	if rec, p := replay("s-dead"); rec.Code != http.StatusOK || p.State != intake.PairPending {
		t.Fatalf("replay of a dead pair = %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := replay("s-dead"); rec.Code != http.StatusConflict {
		t.Fatalf("a second replay = %d, want 409", rec.Code)
	}
	if rec, p := replay("s-match"); rec.Code != http.StatusOK || p.State != intake.PairPending || p.MatchRefusal {
		t.Fatalf("re-judging a match-refused pair whose subscription matches = %d %s, want pending", rec.Code, rec.Body.String())
	}
	if rec, p := replay("s-gone"); rec.Code != http.StatusOK || p.State != intake.PairRefused || p.Reason != "its subscription was deleted" {
		t.Fatalf("re-judging a pair whose subscription is gone = %d %s, want refused naming it", rec.Code, rec.Body.String())
	}
	if rec := w.call(w.s.handleReplayPair, http.MethodPost, "/api/v1/pairs/x/replay", map[string]string{"id": "pr-nope"}); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown pair = %d, want 404", rec.Code)
	}
}
