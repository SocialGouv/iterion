package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// mintShareReq builds an operator (admin) create-share request.
func mintShareReq(t *testing.T, adminCtx context.Context, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/teams/t1/config-shares", strings.NewReader(body)).WithContext(adminCtx)
	r.SetPathValue("id", "t1")
	return r
}

func jsonStrs(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestConfigShare_MintDerivesFromBotManifest proves the generalization: an
// operator mints a share for feed-watch with ONLY bot_id + category — no
// hand-typed JSON paths — and the mint DERIVES the editable/visible paths +
// config file from the bot's manifest config_share: block (expanding
// {category}). This is the "a bot declares its shareable surface, a share can
// never exceed it" contract, exercised against the REAL feed-watch manifest.
func TestConfigShare_MintDerivesFromBotManifest(t *testing.T) {
	s := newOrgTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	s.cfg.Bots.Paths = []string{botsDirAbs(t)} // real bots/, incl. feed-watch's config_share block
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{ID: "op", Email: "op@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	adminCtx := auth.WithIdentity(ctx, auth.Identity{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin})

	// Only bot_id + category — the surface is derived, not supplied.
	body := `{"bot_id":"feed-watch","label":"a11y","repo_url":"https://github.com/o/r","repo_ref":"main","category":"a11y"}`
	cw := httptest.NewRecorder()
	s.handleCreateConfigShare(cw, mintShareReq(t, adminCtx, body))
	if cw.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", cw.Code, cw.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(cw.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if got := created["config_path"]; got != "feed-watch.json" {
		t.Errorf("config_path = %v, want feed-watch.json (derived from manifest)", got)
	}
	allowed := jsonStrs(created["allowed_paths"])
	wantAllowed := []string{"categories.a11y.feeds", "categories.a11y.editorial"}
	if !reflect.DeepEqual(allowed, wantAllowed) {
		t.Errorf("allowed_paths = %v, want %v ({category}→a11y)", allowed, wantAllowed)
	}
	visible := jsonStrs(created["visible_paths"])
	for _, want := range []string{"categories.a11y.feeds", "categories.a11y.editorial", "categories.a11y.digest_title"} {
		found := false
		for _, v := range visible {
			if v == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("visible_paths %v missing %q (feeds+editorial+digest_title expected)", visible, want)
		}
	}
}

// TestConfigShare_MintSubsetOfEditableFields proves per-share least privilege:
// an operator mints a feeds-only share of feed-watch (editable_fields:["feeds"]),
// and the derived grant excludes editorial from BOTH the writable and visible
// sets — a feeds curator can't touch or even read the editorial prompt.
func TestConfigShare_MintSubsetOfEditableFields(t *testing.T) {
	s := newOrgTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{ID: "op", Email: "op@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	adminCtx := auth.WithIdentity(ctx, auth.Identity{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin})

	body := `{"bot_id":"feed-watch","label":"feeds-only","repo_url":"https://github.com/o/r","repo_ref":"main","category":"a11y","editable_fields":["feeds"]}`
	cw := httptest.NewRecorder()
	s.handleCreateConfigShare(cw, mintShareReq(t, adminCtx, body))
	if cw.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", cw.Code, cw.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(cw.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if allowed := jsonStrs(created["allowed_paths"]); !reflect.DeepEqual(allowed, []string{"categories.a11y.feeds"}) {
		t.Errorf("allowed_paths = %v, want [categories.a11y.feeds] only", allowed)
	}
	for _, p := range jsonStrs(created["visible_paths"]) {
		if p == "categories.a11y.editorial" {
			t.Errorf("editorial leaked into a feeds-only share's visible_paths: %v", created["visible_paths"])
		}
	}
}

// TestConfigShare_MintRejectsUndeclaredEditableField proves the subset can't
// escape the declared surface: selecting a field the bot never declared
// editable (here `sinks`, the digest routing) is a 400, not a silent widen.
func TestConfigShare_MintRejectsUndeclaredEditableField(t *testing.T) {
	s := newOrgTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{ID: "op", Email: "op@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	adminCtx := auth.WithIdentity(ctx, auth.Identity{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin})

	body := `{"bot_id":"feed-watch","label":"x","repo_url":"https://github.com/o/r","repo_ref":"main","category":"a11y","editable_fields":["sinks"]}`
	cw := httptest.NewRecorder()
	s.handleCreateConfigShare(cw, mintShareReq(t, adminCtx, body))
	if cw.Code != http.StatusBadRequest {
		t.Fatalf("select undeclared field = %d, want 400 (%s)", cw.Code, cw.Body.String())
	}
}

// TestConfigShare_MintNeverExpires proves the opt-out from the default TTL: a
// never_expires mint stores a zero ExpiresAt (Share.Active never times out) and
// the view omits expires_at so the UI renders "never".
func TestConfigShare_MintNeverExpires(t *testing.T) {
	s := newOrgTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{ID: "op", Email: "op@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	adminCtx := auth.WithIdentity(ctx, auth.Identity{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin})

	body := `{"bot_id":"feed-watch","label":"durable","repo_url":"https://github.com/o/r","repo_ref":"main","category":"a11y","never_expires":true}`
	cw := httptest.NewRecorder()
	s.handleCreateConfigShare(cw, mintShareReq(t, adminCtx, body))
	if cw.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", cw.Code, cw.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(cw.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if v, ok := created["expires_at"]; ok {
		t.Errorf("never-expiring share must omit expires_at, got %v", v)
	}
}

// TestConfigShare_MintRejectsMissingCategoryForCategorySurface proves the
// derived surface enforces its own contract: feed-watch's paths carry
// {category}, so minting without one fails closed (400) rather than pinning a
// path with a literal "{category}" segment.
func TestConfigShare_MintRejectsMissingCategoryForCategorySurface(t *testing.T) {
	s := newOrgTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{ID: "op", Email: "op@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	adminCtx := auth.WithIdentity(ctx, auth.Identity{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin})

	body := `{"bot_id":"feed-watch","label":"x","repo_url":"https://github.com/o/r","repo_ref":"main"}`
	cw := httptest.NewRecorder()
	s.handleCreateConfigShare(cw, mintShareReq(t, adminCtx, body))
	if cw.Code != http.StatusBadRequest {
		t.Fatalf("mint without category = %d, want 400 (%s)", cw.Code, cw.Body.String())
	}
}
