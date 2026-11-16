package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The /api/teams/{id}/… family is scoped by its path's team: a viewer of
// that team reads and writes nothing, a member writes under it whatever
// team their session has active, and an outsider reaches none of it.
func TestTheTeamTriggerFamilyIsScopedByItsPath(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(auth.Config{Store: identity.NewMemoryStore(), Sessions: auth.NewMemorySessionStore(),
		Signer: signer, SignupMode: auth.SignupOpen, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	subs := trigger.NewMemorySubscriptionStore()
	s := New(Config{WorkDir: t.TempDir(), SkipProjectRegistration: true, AuthService: svc, AuthSigner: signer, TriggerStore: subs},
		iterlog.New(iterlog.LevelError, nil))
	ctx := context.Background()
	seedTeam(t, s, "t1", "acme")
	seedTeam(t, s, "t2", "other")
	viewer := seedTeamMember(t, s, ctx, "vi", identity.RoleViewer)
	member := seedTeamMember(t, s, ctx, "me", identity.RoleMember)
	member.TeamID = "t2" // the session's active team is not the path's
	if _, err := s.authStore().CreateUser(ctx, identity.User{ID: "out", Email: "out@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	outsider := auth.Identity{UserID: "out", TeamID: "t2"}
	token := func(id auth.Identity) string {
		tok, _, err := signer.IssueAccess(id)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	do := func(id auth.Identity, method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("Authorization", "Bearer "+token(id))
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, r)
		return rec
	}

	rec := do(member, http.MethodPost, "/api/teams/t1/triggers", `{"bot_id":"b","invocation":"forge","match":{}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a member's create = %d %s", rec.Code, rec.Body.String())
	}
	var created trigger.Subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.TenantID != "t1" {
		t.Fatalf("created %+v (%v), want it under the path's team t1", created, err)
	}
	if rec := do(viewer, http.MethodGet, "/api/teams/t1/triggers/"+created.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("a viewer's read of the row = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(viewer, http.MethodPost, "/api/teams/t1/triggers", `{"bot_id":"b","invocation":"forge","match":{}}`); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer's create = %d, want 403", rec.Code)
	}
	if rec := do(viewer, http.MethodDelete, "/api/teams/t1/triggers/"+created.ID, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer's delete = %d, want 403", rec.Code)
	}
	if rec := do(outsider, http.MethodGet, "/api/teams/t1/triggers", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("an outsider's read = %d, want 403", rec.Code)
	}
	if rows, _ := subs.ListByTenant(ctx, "t1"); len(rows) != 1 {
		t.Fatalf("t1 holds %d rows, want the member's one", len(rows))
	}
}
