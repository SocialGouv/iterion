package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/usernotify"
	"github.com/SocialGouv/iterion/pkg/usernotify/webpush"
)

func newNotifTestServer(t *testing.T) *Server {
	t.Helper()
	s := New(Config{
		PushSubscriptions:      webpush.NewMemSubscriptionStore(),
		NotificationPrefs:      usernotify.NewMemPrefsStore(),
		NotificationSent:       usernotify.NewMemSentStore(),
		WebPushVAPIDPublicKey:  "pub",
		WebPushVAPIDPrivateKey: "priv",
	}, iterlog.New(iterlog.LevelError, nil))
	return s
}

func notifCtx(userID, teamID string) context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: userID, TeamID: teamID})
}

func notifReq(ctx context.Context, method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	return r.WithContext(ctx)
}

func TestPushSubscribeUnsubscribe(t *testing.T) {
	s := newNotifTestServer(t)
	ctx := notifCtx("u1", "t1")

	// Missing keys → 400.
	w := httptest.NewRecorder()
	s.handlePushSubscribe(w, notifReq(ctx, "POST", "/api/v1/notifications/push/subscriptions", `{"endpoint":"https://push/x"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing keys: code=%d body=%s", w.Code, w.Body.String())
	}

	// Valid subscribe.
	w = httptest.NewRecorder()
	s.handlePushSubscribe(w, notifReq(ctx, "POST", "/api/v1/notifications/push/subscriptions",
		`{"endpoint":"https://push/x","keys":{"p256dh":"p","auth":"a"}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("subscribe: code=%d body=%s", w.Code, w.Body.String())
	}
	subs, _ := s.cfg.PushSubscriptions.ListForUsers(ctx, "t1", []string{"u1"})
	if len(subs) != 1 || subs[0].Endpoint != "https://push/x" {
		t.Fatalf("stored subs = %+v", subs)
	}

	// Another user cannot delete it.
	w = httptest.NewRecorder()
	s.handlePushUnsubscribe(w, notifReq(notifCtx("intruder", "t1"), "DELETE", "/api/v1/notifications/push/subscriptions", `{"endpoint":"https://push/x"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("unsubscribe (other user): code=%d", w.Code)
	}
	if subs, _ = s.cfg.PushSubscriptions.ListForUsers(ctx, "t1", []string{"u1"}); len(subs) != 1 {
		t.Fatal("cross-user unsubscribe removed the subscription")
	}

	// The owner can.
	w = httptest.NewRecorder()
	s.handlePushUnsubscribe(w, notifReq(ctx, "DELETE", "/api/v1/notifications/push/subscriptions", `{"endpoint":"https://push/x"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("unsubscribe: code=%d body=%s", w.Code, w.Body.String())
	}
	if subs, _ = s.cfg.PushSubscriptions.ListForUsers(ctx, "t1", []string{"u1"}); len(subs) != 0 {
		t.Fatalf("subscription not removed: %+v", subs)
	}
}

func TestNotificationPrefsRoundTrip(t *testing.T) {
	s := newNotifTestServer(t)
	ctx := notifCtx("u1", "t1")

	// Default (no row) = own.
	w := httptest.NewRecorder()
	s.handleGetNotificationPrefs(w, notifReq(ctx, "GET", "/api/v1/notifications/prefs", ""))
	var got struct {
		Scope usernotify.Scope `json:"scope"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.Scope != usernotify.ScopeOwn {
		t.Fatalf("default prefs: code=%d scope=%q", w.Code, got.Scope)
	}

	// Invalid scope → 400.
	w = httptest.NewRecorder()
	s.handlePutNotificationPrefs(w, notifReq(ctx, "PUT", "/api/v1/notifications/prefs", `{"scope":"everything"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid scope: code=%d", w.Code)
	}

	// team opt-in persists and reads back.
	w = httptest.NewRecorder()
	s.handlePutNotificationPrefs(w, notifReq(ctx, "PUT", "/api/v1/notifications/prefs", `{"scope":"team"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("put prefs: code=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleGetNotificationPrefs(w, notifReq(ctx, "GET", "/api/v1/notifications/prefs", ""))
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Scope != usernotify.ScopeTeam {
		t.Fatalf("scope after put = %q", got.Scope)
	}
	// The opt-in is tenant-scoped.
	users, _ := s.cfg.NotificationPrefs.ListTeamWide(ctx, "t1")
	if len(users) != 1 || users[0] != "u1" {
		t.Fatalf("team-wide list = %v", users)
	}
}

func TestNotificationRoutesDisabledWithoutConfig(t *testing.T) {
	s := New(Config{}, iterlog.New(iterlog.LevelError, nil))
	if s.webPushEnabled() {
		t.Fatal("web push must be off without VAPID keys + store")
	}
	w := httptest.NewRecorder()
	s.handlePushSubscribe(w, notifReq(notifCtx("u1", "t1"), "POST", "/api/v1/notifications/push/subscriptions",
		`{"endpoint":"https://push/x","keys":{"p256dh":"p","auth":"a"}}`))
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled subscribe: code=%d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handlePushTest(w, notifReq(notifCtx("u1", "t1"), "POST", "/api/v1/notifications/push/test", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled test: code=%d", w.Code)
	}
}
