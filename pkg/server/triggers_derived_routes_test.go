package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// Enabling an invocation stores the row under the invocation's derived id;
// enabling it again names that row (409); deleting it leaves a tombstone no
// reconciliation crosses; enabling it again is the gesture that lifts it.
func TestTriggerFromInvocation_DerivedRowLifecycle(t *testing.T) {
	srv := newFromInvocationServer(t)
	ctx := context.Background()
	id := trigger.DerivedSubscriptionID("", "trig-bot", bundle.InvocationKindSchedule, "0 7 * * 1", "")

	rec := postFromInvocation(t, srv, "trig-bot", `{"index":0}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enable = %d %s", rec.Code, rec.Body.String())
	}
	var created trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID != id {
		t.Fatalf("enabled row id = %q (%v), want the derived %q", created.ID, err, id)
	}

	again := postFromInvocation(t, srv, "trig-bot", `{"index":0}`)
	var conflict struct {
		SubscriptionID string `json:"subscription_id"`
	}
	_ = json.Unmarshal(again.Body.Bytes(), &conflict)
	if again.Code != http.StatusConflict || conflict.SubscriptionID != id {
		t.Fatalf("enable twice = %d %s, want 409 naming %s", again.Code, again.Body.String(), id)
	}

	del := httptest.NewRequest(http.MethodDelete, "/api/v1/triggers/"+id, nil)
	del.SetPathValue("id", id)
	drec := httptest.NewRecorder()
	srv.handleDeleteTrigger(drec, del)
	if drec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", drec.Code, drec.Body.String())
	}
	sub := trigger.Subscription{ID: id, BotID: "trig-bot", Invocation: bundle.InvocationKindSchedule, CreatedAt: time.Now()}
	if made, err := srv.cfg.TriggerStore.EnsureDerived(ctx, sub); err != nil || made {
		t.Fatalf("after an operator delete, EnsureDerived = %v, %v; want the tombstone to hold", made, err)
	}

	if rec := postFromInvocation(t, srv, "trig-bot", `{"index":0}`); rec.Code != http.StatusCreated {
		t.Fatalf("re-enable after a delete = %d %s, want 201", rec.Code, rec.Body.String())
	}
}

// A row enabled before ids were derived still answers for its kind.
func TestTriggerFromInvocation_ARandomIDRowStillAnswers(t *testing.T) {
	srv := newFromInvocationServer(t)
	legacy := trigger.Subscription{ID: "8f14e45f-ceea-4f2b-9b1e-1c3e5a2e7d10", BotID: "trig-bot", Origin: botHomeTriggerOrigin,
		Invocation: bundle.InvocationKindSchedule, Enabled: true}
	if err := srv.cfg.TriggerStore.Create(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	rec := postFromInvocation(t, srv, "trig-bot", `{"index":0}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("enable beside a random-id row = %d %s, want 409", rec.Code, rec.Body.String())
	}
}

// Deleting a row the operator authored writes no tombstone.
func TestTriggers_DeletingAnOperatorRowLeavesNoTombstone(t *testing.T) {
	srv := newTriggerTestServer(t)
	row := trigger.Subscription{ID: "op-1", BotID: "b", Enabled: true}
	if err := srv.cfg.TriggerStore.Create(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	del := httptest.NewRequest(http.MethodDelete, "/api/v1/triggers/op-1", nil)
	del.SetPathValue("id", "op-1")
	rec := httptest.NewRecorder()
	srv.handleDeleteTrigger(rec, del)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
}

// Two invocations of one kind are two rows: enabling the second is not
// answered by the first.
func TestTriggerFromInvocation_TwoInvocationsOfOneKindAreTwoRows(t *testing.T) {
	srv := newFromInvocationServer(t)
	if rec := postFromInvocation(t, srv, "trig-bot", `{"index":0}`); rec.Code != http.StatusCreated {
		t.Fatalf("enable the first schedule = %d %s", rec.Code, rec.Body.String())
	}
	rec := postFromInvocation(t, srv, "trig-bot", `{"index":4}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enable the second schedule = %d %s, want 201", rec.Code, rec.Body.String())
	}
	var second trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if want := trigger.DerivedSubscriptionID("", "trig-bot", bundle.InvocationKindSchedule, "0 9 * * 5", ""); second.ID != want {
		t.Fatalf("the second schedule's id = %q, want %q", second.ID, want)
	}
}
