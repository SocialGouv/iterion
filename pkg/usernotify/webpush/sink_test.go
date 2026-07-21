package webpush

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/usernotify"
)

// newTestKeys mints a throwaway VAPID pair for tests.
func newTestKeys(t *testing.T) (priv, pub string) {
	t.Helper()
	priv, pub, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	return priv, pub
}

// clientKeys is a real browser-shaped subscription keypair (p256dh must be
// a valid P-256 point for the payload encryption to succeed). Generated
// once from a Chrome subscription fixture.
const (
	testP256dh = "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM="
	testAuth   = "tBHItJI5svbpez7KI4CCXg=="
)

func testNotification() usernotify.Notification {
	return usernotify.Notification{
		Kind:     usernotify.KindHumanInputRequested,
		TenantID: "team-1",
		UserIDs:  []string{"user-1"},
		Title:    "Input needed: Review release",
		Body:     "Please approve.",
		Link:     "https://iterion.example/runs/run-1",
		RunID:    "run-1",
		Tag:      "run-1",
	}
}

func TestSinkDeliverAndPrune(t *testing.T) {
	priv, pub := newTestKeys(t)

	var hits, gones atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid") {
			t.Errorf("missing VAPID authorization header")
		}
		if strings.HasSuffix(r.URL.Path, "/dead") {
			gones.Add(1)
			w.WriteHeader(http.StatusGone)
			return
		}
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	store := NewMemSubscriptionStore()
	ctx := context.Background()
	for _, ep := range []string{srv.URL + "/live", srv.URL + "/dead"} {
		if err := store.Upsert(ctx, &Subscription{
			TenantID: "team-1", UserID: "user-1",
			Endpoint: ep, P256dh: testP256dh, Auth: testAuth,
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	sink := NewSink(store, SinkOptions{
		VAPIDPublicKey:  pub,
		VAPIDPrivateKey: priv,
		Subscriber:      "mailto:ops@example.org",
		HTTPClient:      srv.Client(),
	}, nil)

	if err := sink.Deliver(ctx, testNotification()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if hits.Load() != 1 || gones.Load() != 1 {
		t.Fatalf("hits=%d gones=%d, want 1/1", hits.Load(), gones.Load())
	}

	// The 410 endpoint must be pruned; the live one stays.
	subs, err := store.ListForUser(ctx, "team-1", "user-1")
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(subs) != 1 || subs[0].Endpoint != srv.URL+"/live" {
		t.Fatalf("subscriptions after prune = %+v", subs)
	}
	if subs[0].LastUsedAt.IsZero() {
		t.Fatal("live subscription not touched after delivery")
	}
}

func TestSinkAllAttemptsFailedErrors(t *testing.T) {
	priv, pub := newTestKeys(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := NewMemSubscriptionStore()
	ctx := context.Background()
	if err := store.Upsert(ctx, &Subscription{
		TenantID: "team-1", UserID: "user-1",
		Endpoint: srv.URL + "/x", P256dh: testP256dh, Auth: testAuth,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	sink := NewSink(store, SinkOptions{VAPIDPublicKey: pub, VAPIDPrivateKey: priv, Subscriber: "mailto:ops@example.org", HTTPClient: srv.Client()}, nil)
	if err := sink.Deliver(ctx, testNotification()); err == nil {
		t.Fatal("expected error when every push attempt fails")
	}
	// A 500 is transient — the subscription must NOT be pruned.
	subs, _ := store.ListForUser(ctx, "team-1", "user-1")
	if len(subs) != 1 {
		t.Fatalf("transient failure pruned the subscription: %+v", subs)
	}
}

func TestSinkNoSubscriptionsIsNotAnError(t *testing.T) {
	priv, pub := newTestKeys(t)
	sink := NewSink(NewMemSubscriptionStore(), SinkOptions{VAPIDPublicKey: pub, VAPIDPrivateKey: priv, Subscriber: "mailto:ops@example.org"}, nil)
	if err := sink.Deliver(context.Background(), testNotification()); err != nil {
		t.Fatalf("Deliver with no subscriptions: %v", err)
	}
}
