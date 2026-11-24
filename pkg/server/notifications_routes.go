package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/deeplink"
	"github.com/SocialGouv/iterion/pkg/usernotify"
	"github.com/SocialGouv/iterion/pkg/usernotify/webpush"
)

// webPushEnabled reports whether the web-push notification stack can run:
// a subscription store plus the shared VAPID keypair.
func (s *Server) webPushEnabled() bool {
	return s.cfg.PushSubscriptions != nil &&
		s.cfg.WebPushVAPIDPublicKey != "" &&
		s.cfg.WebPushVAPIDPrivateKey != ""
}

// registerNotificationRoutes wires the browser push-subscription CRUD, the
// per-user notification prefs, and the settings-page test push.
func (s *Server) registerNotificationRoutes() {
	s.mux.Handle("POST /api/v1/notifications/push/subscriptions", s.requireAuth(http.HandlerFunc(s.handlePushSubscribe)))
	s.mux.Handle("DELETE /api/v1/notifications/push/subscriptions", s.requireAuth(http.HandlerFunc(s.handlePushUnsubscribe)))
	s.mux.Handle("GET /api/v1/notifications/prefs", s.requireAuth(http.HandlerFunc(s.handleGetNotificationPrefs)))
	s.mux.Handle("PUT /api/v1/notifications/prefs", s.requireAuth(http.HandlerFunc(s.handlePutNotificationPrefs)))
	s.mux.Handle("POST /api/v1/notifications/push/test", s.requireAuth(http.HandlerFunc(s.handlePushTest)))
}

// pushSubscriptionReq mirrors the browser PushSubscription.toJSON() shape.
type pushSubscriptionReq struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.webPushEnabled() {
		s.httpErrorFor(w, r, http.StatusNotFound, "web push is not configured on this server")
		return
	}
	id, _ := auth.FromContext(r.Context())
	var req pushSubscriptionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid subscription payload: %v", err)
		return
	}
	if req.Endpoint == "" || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "subscription requires endpoint, keys.p256dh and keys.auth")
		return
	}
	sub := &webpush.Subscription{
		TenantID:  id.TeamID,
		UserID:    id.UserID,
		Endpoint:  req.Endpoint,
		P256dh:    req.Keys.P256dh,
		Auth:      req.Keys.Auth,
		UserAgent: r.UserAgent(),
	}
	// An access token outlives its account's deactivation or erasure by up
	// to its TTL: the subscription — endpoint, keys, user agent — is read
	// against the account's standing before the write, and again after it,
	// so one that lands behind an erasure's deletion of the subscriptions is
	// taken back rather than kept under the pseudonym.
	if refused, err := s.accountRefusesSignIn(r.Context(), id.UserID); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "read the account's standing: %v", err)
		return
	} else if refused {
		s.httpErrorFor(w, r, http.StatusForbidden, "this account is deactivated or erased")
		return
	}
	if err := s.cfg.PushSubscriptions.Upsert(r.Context(), sub); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "store subscription: %v", err)
		return
	}
	if refused, err := s.accountRefusesSignIn(r.Context(), id.UserID); err != nil || refused {
		if derr := s.cfg.PushSubscriptions.DeleteByEndpoint(r.Context(), id.UserID, sub.Endpoint); derr != nil {
			err = errors.Join(err, fmt.Errorf("take back the subscription just written: %w", derr))
		}
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "re-read the account's standing after the write: %v", err)
			return
		}
		s.httpErrorFor(w, r, http.StatusForbidden, "this account is deactivated or erased")
		return
	}
	s.writeJSONFor(w, r, map[string]any{"ok": true})
}

// accountRefusesSignIn reports whether userID's account refuses sign-ins
// (deactivated or erased). No identity store (local mode) holds no account
// to refuse.
func (s *Server) accountRefusesSignIn(ctx context.Context, userID string) (bool, error) {
	st := s.authStore()
	if st == nil || userID == "" {
		return false, nil
	}
	u, err := st.GetUser(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("read account %s: %w", userID, err)
	}
	return u.Status.SignInRefused(), nil
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.webPushEnabled() {
		s.httpErrorFor(w, r, http.StatusNotFound, "web push is not configured on this server")
		return
	}
	id, _ := auth.FromContext(r.Context())
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Endpoint == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "endpoint required")
		return
	}
	if err := s.cfg.PushSubscriptions.DeleteByEndpoint(r.Context(), id.UserID, req.Endpoint); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "delete subscription: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"ok": true})
}

func (s *Server) handleGetNotificationPrefs(w http.ResponseWriter, r *http.Request) {
	if s.cfg.NotificationPrefs == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "notification prefs are not available on this server")
		return
	}
	id, _ := auth.FromContext(r.Context())
	p, err := s.cfg.NotificationPrefs.Get(r.Context(), id.TeamID, id.UserID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load prefs: %v", err)
		return
	}
	scope := usernotify.ScopeOwn
	if p != nil {
		scope = p.Scope
	}
	s.writeJSONFor(w, r, map[string]any{"scope": scope})
}

func (s *Server) handlePutNotificationPrefs(w http.ResponseWriter, r *http.Request) {
	if s.cfg.NotificationPrefs == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "notification prefs are not available on this server")
		return
	}
	id, _ := auth.FromContext(r.Context())
	var req struct {
		Scope usernotify.Scope `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid prefs payload: %v", err)
		return
	}
	if !usernotify.ValidScope(req.Scope) {
		s.httpErrorFor(w, r, http.StatusBadRequest, "scope must be %q or %q", usernotify.ScopeOwn, usernotify.ScopeTeam)
		return
	}
	if err := s.cfg.NotificationPrefs.Set(r.Context(), &usernotify.Prefs{
		TenantID: id.TeamID,
		UserID:   id.UserID,
		Scope:    req.Scope,
	}); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "save prefs: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"scope": req.Scope})
}

// handlePushTest sends a canned notification to the CURRENT user only — the
// settings page's "does it reach this browser?" smoke test.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	if s.pushSink == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "web push is not configured on this server")
		return
	}
	id, _ := auth.FromContext(r.Context())
	n := usernotify.Notification{
		Kind:     usernotify.KindHumanInputRequested,
		TenantID: id.TeamID,
		UserIDs:  []string{id.UserID},
		Title:    "Test notification",
		Body:     "Push notifications are working — a run waiting for your input will look like this.",
		Link:     deeplink.Runs(s.cfg.PublicURL),
		Tag:      "iterion-test",
	}
	if err := s.pushSink.Deliver(r.Context(), n); err != nil {
		s.httpErrorFor(w, r, http.StatusBadGateway, "test push failed: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"ok": true})
}
