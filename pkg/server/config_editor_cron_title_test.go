package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestConfigEditorSchedule_ScopedCronEdit proves a config_editor reads and
// edits ONLY the cron of the schedule bound to its share's (bot, category),
// never another category's schedule — the cadence stays in iterion's schedule
// store (visible), the editor just tunes its own category's frequency.
func TestConfigEditorSchedule_ScopedCronEdit(t *testing.T) {
	s := newScheduleTestServer(t)
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	editor := seedTeamMember(t, s, ctx, "ed", identity.RoleConfigEditor)

	sh := &configshare.Share{
		ID: "sh1", TenantID: "t1", BotID: "feed-watch", Label: "a11y", Category: "a11y",
		RepoURL: "https://github.com/o/r", RepoRef: "main", ConfigPath: "feed-watch.json",
		AllowedPaths: []string{"categories.a11y.feeds"}, Enabled: true,
	}
	if err := s.configShares.Create(ctx, sh); err != nil {
		t.Fatal(err)
	}
	mk := func(id, cat, cron string) {
		if err := s.cfg.ScheduledBots.Create(ctx, cloudsched.ScheduledBot{
			ID: id, TenantID: "t1", BotID: "feed-watch", Cron: cron,
			Vars:       map[string]string{"category": cat, "mode": "digest"},
			NextFireAt: s.scheduleNow().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("sc-a11y", "a11y", "0 8 * * 3")
	mk("sc-cyber", "cyber", "0 8 * * *")

	edCtx := auth.WithIdentity(ctx, editor)
	req := func(method, body string) *http.Request {
		return scheduleReq(edCtx, method, "/api/teams/t1/config-editor/shares/sh1/schedule", body, "t1", "sh1")
	}

	// GET → the a11y schedule, never cyber.
	gw := httptest.NewRecorder()
	s.handleConfigEditorGetSchedule(gw, req("GET", ""))
	if gw.Code != http.StatusOK {
		t.Fatalf("get schedule = %d: %s", gw.Code, gw.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(gw.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["exists"] != true || got["cron"] != "0 8 * * 3" || got["schedule_id"] != "sc-a11y" {
		t.Fatalf("get schedule payload = %v", got)
	}

	// PATCH cron → only the a11y schedule changes.
	pw := httptest.NewRecorder()
	s.handleConfigEditorPatchSchedule(pw, req("PATCH", `{"cron":"30 9 * * 1"}`))
	if pw.Code != http.StatusOK {
		t.Fatalf("patch schedule = %d: %s", pw.Code, pw.Body.String())
	}
	if a, _ := s.cfg.ScheduledBots.Get(ctx, "sc-a11y"); a.Cron != "30 9 * * 1" {
		t.Errorf("a11y cron not updated: %q", a.Cron)
	}
	if c, _ := s.cfg.ScheduledBots.Get(ctx, "sc-cyber"); c.Cron != "0 8 * * *" {
		t.Errorf("cyber schedule was modified through the a11y share: %q", c.Cron)
	}

	// A bad cron is rejected.
	bw := httptest.NewRecorder()
	s.handleConfigEditorPatchSchedule(bw, req("PATCH", `{"cron":"not a cron"}`))
	if bw.Code != http.StatusBadRequest {
		t.Errorf("bad cron = %d, want 400", bw.Code)
	}

	// A plain viewer is 403 on the cadence endpoints (loadEditableShare gate).
	viewer := seedTeamMember(t, s, ctx, "vi", identity.RoleViewer)
	vw := httptest.NewRecorder()
	vr := scheduleReq(auth.WithIdentity(ctx, viewer), "GET", "/x", "", "t1", "sh1")
	s.handleConfigEditorGetSchedule(vw, vr)
	if vw.Code != http.StatusForbidden {
		t.Errorf("viewer on schedule = %d, want 403", vw.Code)
	}
}

// TestConfigEditorSchedule_NoScheduleForCategory: a category with no schedule
// reports exists:false and PATCH returns 404 — creating a schedule (and its
// delivery sinks) stays an operator action, mirroring category creation.
func TestConfigEditorSchedule_NoScheduleForCategory(t *testing.T) {
	s := newScheduleTestServer(t)
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	editor := seedTeamMember(t, s, ctx, "ed", identity.RoleConfigEditor)
	sh := &configshare.Share{
		ID: "sh1", TenantID: "t1", BotID: "feed-watch", Category: "design-systems",
		RepoURL: "https://github.com/o/r", RepoRef: "main", ConfigPath: "feed-watch.json",
		AllowedPaths: []string{"categories.design-systems.feeds"}, Enabled: true,
	}
	if err := s.configShares.Create(ctx, sh); err != nil {
		t.Fatal(err)
	}
	edCtx := auth.WithIdentity(ctx, editor)

	gw := httptest.NewRecorder()
	s.handleConfigEditorGetSchedule(gw, scheduleReq(edCtx, "GET", "/x", "", "t1", "sh1"))
	if gw.Code != http.StatusOK {
		t.Fatalf("get = %d", gw.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(gw.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["exists"] != false {
		t.Errorf("exists = %v, want false", got["exists"])
	}

	pw := httptest.NewRecorder()
	s.handleConfigEditorPatchSchedule(pw, scheduleReq(edCtx, "PATCH", "/x", `{"cron":"0 8 * * 1"}`, "t1", "sh1"))
	if pw.Code != http.StatusNotFound {
		t.Errorf("patch with no schedule = %d, want 404", pw.Code)
	}
}

// TestConfigShare_MintRejectsAbsentCategory proves the mint guard-rail: minting
// a category share for a category that isn't in the config file (a common
// typo — "design" vs the real "design-systems") is rejected with a clear error
// instead of producing a share whose editor sees nothing.
func TestConfigShare_MintRejectsAbsentCategory(t *testing.T) {
	s := newOrgTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	s.cfg.Bots.Paths = []string{botsDirAbs(t)} // real feed-watch manifest (config_share block)
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{ID: "op", Email: "op@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	adminCtx := auth.WithIdentity(ctx, auth.Identity{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin})

	// A FC serving a config with a11y + cyber (no "design").
	s.configShareFC = func(context.Context, *configshare.Share) (forge.FileClient, error) {
		return &fakeShareFC{content: []byte(shareTestConfig), sha: "sha-1"}, nil
	}

	// Present category → mint succeeds.
	okw := httptest.NewRecorder()
	s.handleCreateConfigShare(okw, mintShareReq(t, adminCtx,
		`{"bot_id":"feed-watch","repo_url":"https://github.com/o/r","repo_ref":"main","category":"a11y"}`))
	if okw.Code != http.StatusCreated {
		t.Fatalf("mint present category = %d: %s", okw.Code, okw.Body.String())
	}

	// Absent category → rejected with a clear message.
	badw := httptest.NewRecorder()
	s.handleCreateConfigShare(badw, mintShareReq(t, adminCtx,
		`{"bot_id":"feed-watch","repo_url":"https://github.com/o/r","repo_ref":"main","category":"design"}`))
	if badw.Code != http.StatusBadRequest {
		t.Fatalf("mint absent category = %d, want 400: %s", badw.Code, badw.Body.String())
	}
	if !strings.Contains(badw.Body.String(), "no editable fields") {
		t.Errorf("expected a clear 'no editable fields' error, got: %s", badw.Body.String())
	}
}

// TestConfigEditorList_EditorTitle proves the bot-declared editor branding
// (manifest config_share.editor_title) is surfaced to the config-editor list so
// the shell can show "Éditeur de veilles" instead of the generic heading.
func TestConfigEditorList_EditorTitle(t *testing.T) {
	s := newOrgTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	editor := seedTeamMember(t, s, ctx, "ed", identity.RoleConfigEditor)
	sh := &configshare.Share{
		ID: "sh1", TenantID: "t1", BotID: "feed-watch", Category: "a11y",
		ConfigPath: "feed-watch.json", Enabled: true,
	}
	if err := s.configShares.Create(ctx, sh); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleConfigEditorList(w, scheduleReq(auth.WithIdentity(ctx, editor), "GET", "/api/teams/t1/config-editor/shares", "", "t1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Éditeur de veilles") {
		t.Errorf("editor_title not surfaced from manifest: %s", w.Body.String())
	}
}
