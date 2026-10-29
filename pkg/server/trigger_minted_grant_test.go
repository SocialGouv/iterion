package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// mintedGrantSpineSentinel stands in for the bearer a server mints at launch:
// a value no real grant can take, so finding it on the spine proves a leak.
const mintedGrantSpineSentinel = "sentinel-spine-grant-2033"

// recordingBus keeps every event the spine was handed. The handler publishes
// from the request's goroutine and the assertion reads from the test's, so
// the slice is taken under the mutex on both sides.
type recordingBus struct {
	mu     sync.Mutex
	events []trigger.Event
}

func (b *recordingBus) Publish(_ context.Context, ev trigger.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, ev)
	return nil
}

func (b *recordingBus) Subscribe(_ string, _ trigger.Matcher, _ eventbus.Handler) (func(context.Context), error) {
	return func(context.Context) {}, nil
}

func (b *recordingBus) published() []trigger.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]trigger.Event(nil), b.events...)
}

func payloadVars(t *testing.T, ev trigger.Event) map[string]any {
	t.Helper()
	raw, ok := ev.Payload[trigger.PayloadVars]
	if !ok {
		return nil
	}
	vars, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("payload vars = %T, want map[string]any", raw)
	}
	return vars
}

// The webhook lane mirrors its launch onto the trigger spine, which fans the
// event out to every matching subscription — in-process locally, NATS in
// cloud. The grant the server minted for THAT run does not travel with it.
func TestForgeTriggerPayloadDropsTheMintedGrant(t *testing.T) {
	payload := forgeTriggerPayload("run-42", "review-pr", "github", "https://github.com/o/r", map[string]string{
		store.ForgePublishTokenVar: mintedGrantSpineSentinel,
		store.ForgePublishURLVar:   "https://iterion.example/publish",
		"pr_number":                "7",
	})
	vars, _ := payload[trigger.PayloadVars].(map[string]any)
	if vars == nil {
		t.Fatal("the event carries no vars at all: the subscription would lose the webhook's own payload")
	}
	for name := range map[string]bool{store.ForgePublishTokenVar: true, store.ForgePublishURLVar: true} {
		if v, ok := vars[name]; ok {
			t.Errorf("the spine event carries the minted var %s (%v)", name, v)
		}
	}
	if vars["pr_number"] != "7" {
		t.Errorf("vars = %v, want the webhook's own pr_number", vars)
	}
	if payload[trigger.PayloadLaunchedRunID] != "run-42" {
		t.Errorf("payload = %v, want the launched-run marker the evaluator reads", payload)
	}
}

// A run whose only vars were minted publishes an event with no vars key —
// not an empty map that reads as "the webhook sent nothing it should keep".
func TestForgeTriggerPayloadOmitsVarsWhenOnlyTheGrantWasThere(t *testing.T) {
	payload := forgeTriggerPayload("run-42", "review-pr", "github", "https://github.com/o/r", map[string]string{
		store.ForgePublishTokenVar: mintedGrantSpineSentinel,
	})
	if _, ok := payload[trigger.PayloadVars]; ok {
		t.Errorf("payload = %v, want no vars key", payload)
	}
}

// The custom ingress is client-driven: whatever a client sends under a
// server-minted var's name is not that var, and it never reaches an event
// every matching subscription reads.
func TestEmitTriggerDropsAClientSentMintedGrant(t *testing.T) {
	srv := newEmitTestServer(t)
	bus := &recordingBus{}
	srv.cfg.EventsBus = bus

	rec := httptest.NewRecorder()
	srv.handleEmitTrigger(rec, httptest.NewRequest(http.MethodPost, "/api/v1/triggers/emit",
		bytes.NewBufferString(`{"kind":"ci.done","vars":{"`+store.ForgePublishTokenVar+`":"`+mintedGrantSpineSentinel+`","build":"42"}}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("emit status = %d; body=%s", rec.Code, rec.Body.String())
	}
	events := bus.published()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	vars := payloadVars(t, events[0])
	if v, ok := vars[store.ForgePublishTokenVar]; ok {
		t.Errorf("the emitted event carries the client's %s (%v)", store.ForgePublishTokenVar, v)
	}
	if vars["build"] != "42" {
		t.Errorf("vars = %v, want the client's own build var kept", vars)
	}
}
