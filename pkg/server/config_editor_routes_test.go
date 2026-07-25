package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// seedTeamMember creates a user + team membership and returns the matching
// authenticated identity.
func seedTeamMember(t *testing.T, s *Server, ctx context.Context, uid string, role identity.Role) auth.Identity {
	t.Helper()
	if _, err := s.authStore().CreateUser(ctx, identity.User{ID: uid, Email: uid + "@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{UserID: uid, TeamID: "t1", Role: role}); err != nil {
		t.Fatal(err)
	}
	return auth.Identity{UserID: uid, TeamID: "t1", Role: role}
}

// TestConfigEditorGates_OrthogonalCapability is the golden RBAC test for
// ADR-078: config_editor is rejected by every standard team gate and admitted
// only by canEditConfigShares; a plain viewer is the inverse (sees the team,
// can't edit config); an admin passes both.
func TestConfigEditorGates_OrthogonalCapability(t *testing.T) {
	s := newOrgTestServer(t)
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	editor := seedTeamMember(t, s, ctx, "ed", identity.RoleConfigEditor)
	viewer := seedTeamMember(t, s, ctx, "vi", identity.RoleViewer)
	admin := seedTeamMember(t, s, ctx, "ad", identity.RoleAdmin)

	// config_editor: out of the ladder — no team view/manage, yes config edit.
	if s.canViewTeam(ctx, editor, "t1") {
		t.Error("config_editor must NOT pass canViewTeam")
	}
	if s.canManageTeam(ctx, editor, "t1") {
		t.Error("config_editor must NOT pass canManageTeam")
	}
	if !s.canEditConfigShares(ctx, editor, "t1") {
		t.Error("config_editor must pass canEditConfigShares")
	}

	// viewer: the inverse — sees the team, not a config editor.
	if !s.canViewTeam(ctx, viewer, "t1") {
		t.Error("viewer must pass canViewTeam")
	}
	if s.canEditConfigShares(ctx, viewer, "t1") {
		t.Error("viewer must NOT pass canEditConfigShares")
	}

	// admin: manages the team, so also edits config.
	if !s.canManageTeam(ctx, admin, "t1") {
		t.Error("admin must pass canManageTeam")
	}
	if !s.canEditConfigShares(ctx, admin, "t1") {
		t.Error("admin must pass canEditConfigShares")
	}
}

// TestConfigEditor_EndToEnd proves the authenticated editor surface: a
// config_editor session lists the team's shares, reads a projected config, and
// patches it (recording a "user:" delivery); a viewer is 403 on every editor
// endpoint.
func TestConfigEditor_EndToEnd(t *testing.T) {
	s := newOrgTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	editor := seedTeamMember(t, s, ctx, "ed", identity.RoleConfigEditor)
	viewer := seedTeamMember(t, s, ctx, "vi", identity.RoleViewer)

	fc := &fakeShareFC{content: []byte(shareTestConfig), sha: "sha-1"}
	s.configShareFC = func(context.Context, *configshare.Share) (forge.FileClient, error) { return fc, nil }

	sh := &configshare.Share{
		ID: "sh1", TenantID: "t1", BotID: "feed-watch", Label: "a11y", Category: "a11y",
		RepoURL: "https://github.com/o/r", RepoRef: "main", ConfigPath: "feed-watch.json",
		AllowedPaths: []string{"categories.a11y.feeds", "categories.a11y.editorial"},
		VisiblePaths: []string{"categories.a11y.feeds", "categories.a11y.editorial", "categories.a11y.digest_title"},
		Enabled:      true,
	}
	if err := s.configShares.Create(ctx, sh); err != nil {
		t.Fatal(err)
	}

	edCtx := auth.WithIdentity(ctx, editor)
	req := func(method, path string, body string) *http.Request {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		}
		r = r.WithContext(edCtx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("sid", "sh1")
		return r
	}

	// ---- list: config_editor sees the share (reduced view, no token metadata) ----
	lw := httptest.NewRecorder()
	s.handleConfigEditorList(lw, req("GET", "/api/teams/t1/config-editor/shares", ""))
	if lw.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", lw.Code, lw.Body.String())
	}
	if strings.Contains(lw.Body.String(), "token_last4") || strings.Contains(lw.Body.String(), "fingerprint") {
		t.Errorf("config-editor list must NOT leak token metadata: %s", lw.Body.String())
	}
	if !strings.Contains(lw.Body.String(), `"sh1"`) {
		t.Errorf("config-editor list missing the share: %s", lw.Body.String())
	}

	// ---- get: projected config scoped to a11y ----
	gw := httptest.NewRecorder()
	s.handleConfigEditorGet(gw, req("GET", "/api/teams/t1/config-editor/shares/sh1/config", ""))
	if gw.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", gw.Code, gw.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(gw.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["sha"] != "sha-1" {
		t.Errorf("get sha = %v", got["sha"])
	}
	// No leak of the other category / sinks in the projection.
	if strings.Contains(gw.Body.String(), "cyber") || strings.Contains(gw.Body.String(), "sinks") {
		t.Errorf("projection leaked out-of-scope data: %s", gw.Body.String())
	}

	// ---- patch: applies + records a user: delivery ----
	pw := httptest.NewRecorder()
	s.handleConfigEditorPatch(pw, req("PATCH", "/api/teams/t1/config-editor/shares/sh1/config",
		`{"sha":"sha-1","patch":{"categories":{"a11y":{"editorial":"new prompt"}}}}`))
	if pw.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", pw.Code, pw.Body.String())
	}
	if fc.puts != 1 {
		t.Errorf("expected 1 PutFile, got %d", fc.puts)
	}
	dels, err := s.configShares.ListDeliveries(ctx, "sh1", 10)
	if err != nil || len(dels) == 0 {
		t.Fatalf("expected a delivery row: %v", err)
	}
	if dels[0].Actor != "user:ed" {
		t.Errorf("delivery actor = %q, want user:ed", dels[0].Actor)
	}

	// ---- a viewer is 403 on every editor endpoint ----
	viCtx := auth.WithIdentity(ctx, viewer)
	for _, ep := range []struct{ method, path string }{
		{"GET", "/api/teams/t1/config-editor/shares"},
		{"GET", "/api/teams/t1/config-editor/shares/sh1/config"},
	} {
		vr := httptest.NewRequest(ep.method, ep.path, nil).WithContext(viCtx)
		vr.SetPathValue("id", "t1")
		vr.SetPathValue("sid", "sh1")
		vw := httptest.NewRecorder()
		if ep.method == "GET" && strings.HasSuffix(ep.path, "/config") {
			s.handleConfigEditorGet(vw, vr)
		} else {
			s.handleConfigEditorList(vw, vr)
		}
		if vw.Code != http.StatusForbidden {
			t.Errorf("viewer on %s %s = %d, want 403", ep.method, ep.path, vw.Code)
		}
	}
}
