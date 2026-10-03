package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The schedule's binding-level routing block: validated on create, and on
// update the ABSENT field leaves the stored block untouched (an edit dialog
// that mirrors the row back must not wipe what it does not know about),
// `null` clears it, an object replaces it wholesale.
func TestSchedule_RoutingBlockLifecycle(t *testing.T) {
	s := newScheduleTestServer(t)
	ctx := superAdminCtx()
	create := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleCreateSchedule(w, scheduleReq(ctx, "POST", "/api/teams/t1/schedules", body, "t1", ""))
		return w
	}
	update := func(sid, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleUpdateSchedule(w, scheduleReq(ctx, "PATCH", "/api/teams/t1/schedules/"+sid, body, "t1", sid))
		return w
	}
	get := func(sid string) cloudsched.ScheduledBot {
		sb, err := s.cfg.ScheduledBots.Get(context.Background(), sid)
		if err != nil {
			t.Fatalf("stored: %v", err)
		}
		return sb
	}

	if w := create(`{"bot_id":"feed-watch","cron":"*/5 * * * *","routing":{"pair_order":["claw+anthropic_key"],"refused_pinned_key":"park"}}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	rows, _ := s.cfg.ScheduledBots.ListByTenant(context.Background(), "t1")
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	created := rows[0]
	if created.Routing == nil || len(created.Routing.PairOrder) != 1 || created.Routing.RefusedPinnedKey != llmroute.RefusedPinnedPark {
		t.Fatalf("created routing = %+v, want the block stored", created.Routing)
	}
	sid := created.ID

	// An update that mirrors the row back WITHOUT routing keeps the block.
	if w := update(sid, `{"disabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("absent-routing update = %d: %s", w.Code, w.Body.String())
	}
	if got := get(sid).Routing; got == nil || len(got.PairOrder) != 1 {
		t.Fatalf("routing wiped by an unaware edit: %+v", got)
	}

	// An object replaces the block wholesale.
	if w := update(sid, `{"routing":{"triggers":["auth"]}}`); w.Code != http.StatusOK {
		t.Fatalf("replace = %d: %s", w.Code, w.Body.String())
	}
	got := get(sid).Routing
	if got == nil || got.PairOrder != nil || len(got.Triggers) != 1 || got.RefusedPinnedKey != "" {
		t.Fatalf("replace left = %+v, want a wholesale replacement", got)
	}

	// `null` clears it.
	if w := update(sid, `{"routing":null}`); w.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", w.Code, w.Body.String())
	}
	if got := get(sid).Routing; got != nil {
		t.Fatalf("routing = %+v, want cleared", got)
	}

	// An unreadable block is refused in both directions, and nothing lands.
	if w := create(`{"bot_id":"w","cron":"*/5 * * * *","routing":{"triggers":["budget"]}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("create invalid = %d: %s", w.Code, w.Body.String())
	}
	if w := update(sid, `{"routing":{"pair_order":["nope+less"]}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("update invalid = %d: %s", w.Code, w.Body.String())
	}
	if got := get(sid).Routing; got != nil {
		t.Fatalf("a refused write must not land: %+v", got)
	}

	// A camelCase typo is REFUSED, not silently stored as a no-op block
	// audited routing_set:true — the create path decodes strictly like the
	// update path and the platform PUT.
	if w := create(`{"bot_id":"w2","cron":"*/5 * * * *","routing":{"pairOrder":["claw+anthropic_key"]}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("camelCase create = %d: %s", w.Code, w.Body.String())
	}
	if w := update(sid, `{"routing":{"pairOrder":["claw+anthropic_key"]}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("camelCase update = %d: %s", w.Code, w.Body.String())
	}

	// The binding level does not hold the lock primitive: a schedule lock
	// would launder an unset past the author. 422, nothing lands.
	if w := create(`{"bot_id":"w3","cron":"*/5 * * * *","routing":{"strict":false,"locks":["strict"]}}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("locks create = %d: %s", w.Code, w.Body.String())
	}
	if w := update(sid, `{"routing":{"locks":["pair_order"]}}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("locks update = %d: %s", w.Code, w.Body.String())
	}
	if got := get(sid).Routing; got != nil {
		t.Fatalf("a refused lock write must not land: %+v", got)
	}
}
