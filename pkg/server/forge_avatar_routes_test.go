package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/brand"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// mockGitLabAvatar is a GitLab whose /user reports a bot (or a person) and
// whose PUT /user/avatar records what it received, answering the configured
// status (0 = 200 with an avatar_url).
type mockGitLabAvatar struct {
	mu       sync.Mutex
	bot      bool
	status   int
	uploads  int
	gotBytes []byte
}

func (m *mockGitLabAvatar) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 575, "username": "group_1026_bot_a7c08cc4", "bot": m.bot})
	})
	mux.HandleFunc("PUT /api/v4/user/avatar", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.uploads++
		if mr, err := r.MultipartReader(); err == nil {
			if part, err := mr.NextPart(); err == nil {
				m.gotBytes, _ = io.ReadAll(part)
			}
		}
		if m.status != 0 {
			w.WriteHeader(m.status)
			_, _ = w.Write([]byte(`{"message":{"avatar":["is too big (should be at most 200 KB)"]}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"avatar_url": "https://gl/uploads/avatar.png"})
	})
	return httptest.NewServer(mux)
}

func (m *mockGitLabAvatar) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.uploads
}

func connectGitLabPAT(t *testing.T, s *Server, baseURL string) forge.Connection {
	t.Helper()
	body := `{"provider":"gitlab","mode":"pat","forge_base_url":"` + baseURL + `","pat":"glpat-token"}`
	w := httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(superAdminCtx(), "POST", "/api/teams/t1/forge/connections", body, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp forgeConnectResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Connection == nil {
		t.Fatalf("no connection in %s", w.Body.String())
	}
	return *resp.Connection
}

// A group/project access token authenticates as a bot user GitLab flags as
// such: the connect wires it AND gives it the iterion-bot face, recorded on
// the connection and persisted.
func TestForgeConnect_BotIdentityGetsTheAvatar(t *testing.T) {
	gl := &mockGitLabAvatar{bot: true}
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)

	conn := connectGitLabPAT(t, s, srv.URL)
	if conn.AccountKind != forge.AccountKindBot {
		t.Errorf("account_kind = %q, want bot", conn.AccountKind)
	}
	if conn.AvatarAppliedAt == nil || conn.AvatarError != "" {
		t.Fatalf("avatar not applied: applied_at=%v error=%q", conn.AvatarAppliedAt, conn.AvatarError)
	}
	if gl.count() != 1 {
		t.Errorf("uploads = %d, want 1", gl.count())
	}
	if !bytes.Equal(gl.gotBytes, brand.BotAvatar(brand.VariantPlain)) {
		t.Error("the connect uploaded something other than the plain iterion-bot avatar")
	}
	stored, err := s.forgeConnections.Get(context.Background(), conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AvatarAppliedAt == nil || stored.AccountKind != forge.AccountKindBot {
		t.Errorf("persisted connection lost the avatar state: %+v", stored)
	}
}

// A person's PAT is never rebranded on connect: the flag is the forge's, not
// the operator's guess.
func TestForgeConnect_PersonIsNotRebranded(t *testing.T) {
	gl := &mockGitLabAvatar{bot: false}
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)

	conn := connectGitLabPAT(t, s, srv.URL)
	if conn.AccountKind != forge.AccountKindUser {
		t.Errorf("account_kind = %q, want user", conn.AccountKind)
	}
	if conn.AvatarAppliedAt != nil || gl.count() != 0 {
		t.Errorf("a person's account was rebranded: applied_at=%v uploads=%d", conn.AvatarAppliedAt, gl.count())
	}
}

// ITERION_FORGE_BRAND_AVATAR=off is the deployment's escape hatch: the bot is
// still recorded as one (the explicit action stays available), nothing is
// uploaded.
func TestForgeConnect_BrandAvatarSwitchedOff(t *testing.T) {
	gl := &mockGitLabAvatar{bot: true}
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)
	s.cfg.DisableForgeBrandAvatar = true

	conn := connectGitLabPAT(t, s, srv.URL)
	if conn.AccountKind != forge.AccountKindBot {
		t.Errorf("account_kind = %q, want bot", conn.AccountKind)
	}
	if conn.AvatarAppliedAt != nil || gl.count() != 0 {
		t.Errorf("switch off but uploaded: applied_at=%v uploads=%d", conn.AvatarAppliedAt, gl.count())
	}
}

// A refusal by the forge never fails the connect — the connection is created
// and the reason is kept on it, for the studio to name and retry.
func TestForgeConnect_AvatarFailureIsRecordedNotFatal(t *testing.T) {
	gl := &mockGitLabAvatar{bot: true, status: http.StatusBadRequest}
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)

	conn := connectGitLabPAT(t, s, srv.URL)
	if conn.AvatarAppliedAt != nil {
		t.Error("applied_at set although GitLab refused")
	}
	if !strings.Contains(conn.AvatarError, "is too big") {
		t.Errorf("avatar_error = %q, want GitLab's reason quoted", conn.AvatarError)
	}
	stored, _ := s.forgeConnections.Get(context.Background(), conn.ID)
	if stored.AvatarError != conn.AvatarError {
		t.Errorf("persisted avatar_error = %q", stored.AvatarError)
	}
}

func seedAvatarConn(t *testing.T, s *Server, c forge.Connection) forge.Connection {
	t.Helper()
	if c.Kind == forge.KindPAT {
		sealed, err := forge.SealPAT(s.sealer, c.ID, "tok")
		if err != nil {
			t.Fatal(err)
		}
		c.SealedPayload = sealed
	}
	c.TenantID = "t1"
	c.Status = forge.StatusActive
	if err := s.forgeConnections.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func avatarReq(s *Server, connID, body string) *httptest.ResponseRecorder {
	r := forgeReq(superAdminCtx(), "POST", "/api/teams/t1/forge/connections/"+connID+"/avatar", body, "t1")
	r.SetPathValue("conn_id", connID)
	w := httptest.NewRecorder()
	s.handleForgeConnectionAvatar(w, r)
	return w
}

// The explicit action's policy, kind by kind. The refusals are the contract:
// each names what to do instead and never touches the forge.
func TestForgeConnectionAvatar_Policy(t *testing.T) {
	gl := &mockGitLabAvatar{bot: false}
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)
	s.forgeOAuthApps = forge.NewMemoryOAuthAppStore()
	if err := s.forgeOAuthApps.Create(context.Background(), forge.ForgeOAuthApp{
		ID: "app1", TenantID: "t1", Provider: forge.ProviderGitHub, ForgeBaseURL: "https://github.com",
		AppSlug: "iterion-forge-1234", AppManageURL: "https://github.com/organizations/acme/settings/apps/iterion-forge-1234/advanced",
	}); err != nil {
		t.Fatal(err)
	}

	oauth := seedAvatarConn(t, s, forge.Connection{ID: "c-oauth", Provider: forge.ProviderGitLab, Kind: forge.KindOAuthApp, AccountLogin: "alice", AccountKind: forge.AccountKindUser, ForgeBaseURL: srv.URL})
	ghPAT := seedAvatarConn(t, s, forge.Connection{ID: "c-gh-pat", Provider: forge.ProviderGitHub, Kind: forge.KindPAT, AccountLogin: "iterion-bot", AccountKind: forge.AccountKindUser})
	ghApp := seedAvatarConn(t, s, forge.Connection{ID: "c-gh-app", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp, AccountLogin: "iterion-forge-1234[bot]", AccountKind: forge.AccountKindInstallation, OAuthAppID: "app1", InstallationID: 42})
	glUser := seedAvatarConn(t, s, forge.Connection{ID: "c-gl-user", Provider: forge.ProviderGitLab, Kind: forge.KindPAT, AccountLogin: "iterion-bot", AccountKind: forge.AccountKindUser, ForgeBaseURL: srv.URL})
	glBot := seedAvatarConn(t, s, forge.Connection{ID: "c-gl-bot", Provider: forge.ProviderGitLab, Kind: forge.KindPAT, AccountLogin: "group_1_bot_x", AccountKind: forge.AccountKindBot, ForgeBaseURL: srv.URL, AvatarError: "earlier refusal"})

	decode := func(w *httptest.ResponseRecorder) map[string]any {
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		return m
	}

	t.Run("oauth is a person — refused, no override", func(t *testing.T) {
		w := avatarReq(s, oauth.ID, `{"force":true}`)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "personal account") {
			t.Errorf("code=%d body=%s", w.Code, w.Body.String())
		}
	})
	t.Run("github PAT — no API, points at the logo", func(t *testing.T) {
		w := avatarReq(s, ghPAT.ID, "")
		m := decode(w)
		if w.Code != http.StatusUnprocessableEntity || m["logo_url"] != "/brand/iterion-bot.png" {
			t.Errorf("code=%d body=%s", w.Code, w.Body.String())
		}
	})
	t.Run("github App — no API, deep-links the App's settings page", func(t *testing.T) {
		w := avatarReq(s, ghApp.ID, "")
		m := decode(w)
		if w.Code != http.StatusUnprocessableEntity || m["manage_url"] != "https://github.com/organizations/acme/settings/apps/iterion-forge-1234" {
			t.Errorf("code=%d body=%s", w.Code, w.Body.String())
		}
	})
	t.Run("gitlab PAT of an unflagged account — needs force", func(t *testing.T) {
		w := avatarReq(s, glUser.ID, "")
		m := decode(w)
		if w.Code != http.StatusConflict || m["needs_force"] != true || m["account_login"] != "iterion-bot" {
			t.Errorf("code=%d body=%s", w.Code, w.Body.String())
		}
		if gl.count() != 0 {
			t.Error("a refusal must not touch the forge")
		}
	})
	t.Run("gitlab PAT forced — applied", func(t *testing.T) {
		w := avatarReq(s, glUser.ID, `{"force":true,"variant":"circle"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var resp forgeAvatarResp
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.AvatarURL != "https://gl/uploads/avatar.png" || resp.Connection == nil || resp.Connection.AvatarAppliedAt == nil {
			t.Errorf("resp = %s", w.Body.String())
		}
		if !bytes.Equal(gl.gotBytes, brand.BotAvatar(brand.VariantCircle)) {
			t.Error("variant circle was asked, the plain bytes were uploaded")
		}
	})
	t.Run("gitlab bot — applied, earlier error cleared", func(t *testing.T) {
		w := avatarReq(s, glBot.ID, "")
		if w.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		stored, _ := s.forgeConnections.Get(context.Background(), glBot.ID)
		if stored.AvatarError != "" || stored.AvatarAppliedAt == nil || time.Since(*stored.AvatarAppliedAt) > time.Minute {
			t.Errorf("persisted = error %q applied_at %v", stored.AvatarError, stored.AvatarAppliedAt)
		}
		if stored.SealedPayload == nil {
			t.Error("the update dropped the sealed token")
		}
	})
	t.Run("unknown variant is a 400", func(t *testing.T) {
		if w := avatarReq(s, glBot.ID, `{"variant":"square"}`); w.Code != http.StatusBadRequest {
			t.Errorf("code=%d body=%s", w.Code, w.Body.String())
		}
	})
	t.Run("forge refusal is a 502 and kept on the connection", func(t *testing.T) {
		gl.mu.Lock()
		gl.status = http.StatusBadRequest
		gl.mu.Unlock()
		w := avatarReq(s, glBot.ID, "")
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "is too big") {
			t.Errorf("code=%d body=%s", w.Code, w.Body.String())
		}
		stored, _ := s.forgeConnections.Get(context.Background(), glBot.ID)
		if !strings.Contains(stored.AvatarError, "is too big") {
			t.Errorf("persisted avatar_error = %q", stored.AvatarError)
		}
	})
}

func TestForgeConnectionAvatar_RequiresManager(t *testing.T) {
	s := newForgeTestServer(t)
	seedAvatarConn(t, s, forge.Connection{ID: "c1", Provider: forge.ProviderGitLab, Kind: forge.KindPAT, AccountKind: forge.AccountKindBot})
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{UserID: "u2", TeamID: "t1", Role: identity.RoleMember}); err != nil {
		t.Fatal(err)
	}
	memberCtx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u2", TeamID: "t1", Role: identity.RoleMember})
	r := forgeReq(memberCtx, "POST", "/api/teams/t1/forge/connections/c1/avatar", "", "t1")
	r.SetPathValue("conn_id", "c1")
	w := httptest.NewRecorder()
	s.handleForgeConnectionAvatar(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("a plain member could rebrand: code=%d", w.Code)
	}
}

// The upload is a forge round-trip; a provision stamping ManagedSecretID on
// the same document during it must survive the apply's write.
func TestForgeConnectionAvatar_ConcurrentProvisionWriteSurvives(t *testing.T) {
	s := newForgeTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v4/user/avatar", func(w http.ResponseWriter, r *http.Request) {
		// The concurrent writer, replayed verbatim from the orchestrator: a
		// full-document update carrying the managed secret id.
		c, err := s.forgeConnections.Get(context.Background(), "c-race")
		if err != nil {
			t.Errorf("get during upload: %v", err)
		}
		c.ManagedSecretID = "sec-managed-forge-token"
		if err := s.forgeConnections.Update(context.Background(), c); err != nil {
			t.Errorf("concurrent update: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"avatar_url": "https://gl/uploads/avatar.png"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	seedAvatarConn(t, s, forge.Connection{ID: "c-race", Provider: forge.ProviderGitLab, Kind: forge.KindPAT, AccountLogin: "group_1_bot_x", AccountKind: forge.AccountKindBot, ForgeBaseURL: srv.URL})

	if w := avatarReq(s, "c-race", ""); w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	stored, _ := s.forgeConnections.Get(context.Background(), "c-race")
	if stored.ManagedSecretID != "sec-managed-forge-token" {
		t.Fatalf("the avatar apply lost a concurrent write: managed_secret_id=%q", stored.ManagedSecretID)
	}
	if stored.AvatarAppliedAt == nil {
		t.Error("the apply itself was not recorded")
	}
}

// cancelAwareStore is the memory store with the one behaviour Mongo has and
// the memory store lacks: a write on a dead context fails.
type cancelAwareStore struct {
	forge.ConnectionStore
	updates int
}

func (c *cancelAwareStore) Update(ctx context.Context, conn forge.Connection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.updates++
	return c.ConnectionStore.Update(ctx, conn)
}

// The failure worth recording is a slow forge — exactly when the caller has
// already gone away. The record must not ride the request context.
func TestForgeConnectionAvatar_RecordsOnACancelledRequest(t *testing.T) {
	s := newForgeTestServer(t)
	cs := &cancelAwareStore{ConnectionStore: s.forgeConnections}
	s.forgeConnections = cs
	gl := &mockGitLabAvatar{bot: true}
	srv := gl.server()
	defer srv.Close()
	seedAvatarConn(t, s, forge.Connection{ID: "c-cancel", Provider: forge.ProviderGitLab, Kind: forge.KindPAT, AccountLogin: "group_1_bot_x", AccountKind: forge.AccountKindBot, ForgeBaseURL: srv.URL})

	ctx, cancel := context.WithCancel(superAdminCtx())
	cancel() // the client is gone before the upload starts
	r := forgeReq(ctx, "POST", "/api/teams/t1/forge/connections/c-cancel/avatar", "", "t1")
	r.SetPathValue("conn_id", "c-cancel")
	w := httptest.NewRecorder()
	s.handleForgeConnectionAvatar(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	stored, _ := s.forgeConnections.Get(context.Background(), "c-cancel")
	if !strings.Contains(stored.AvatarError, "context canceled") || cs.updates != 1 {
		t.Fatalf("the failure was not recorded: avatar_error=%q updates=%d", stored.AvatarError, cs.updates)
	}
}

func TestForgeConnectionAvatar_RevokedConnectionIsRefused(t *testing.T) {
	s := newForgeTestServer(t)
	gl := &mockGitLabAvatar{bot: true}
	srv := gl.server()
	defer srv.Close()
	c := seedAvatarConn(t, s, forge.Connection{ID: "c-revoked", Provider: forge.ProviderGitLab, Kind: forge.KindPAT, AccountLogin: "group_1_bot_x", AccountKind: forge.AccountKindBot, ForgeBaseURL: srv.URL})
	c.Status = forge.StatusRevoked
	if err := s.forgeConnections.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	w := avatarReq(s, "c-revoked", `{"force":true}`)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "reconnect") || gl.count() != 0 {
		t.Errorf("code=%d body=%s uploads=%d", w.Code, w.Body.String(), gl.count())
	}
}

// The rebrand nobody asked for is the one that must leave a trace.
func TestForgeConnect_AutoApplyIsAudited(t *testing.T) {
	gl := &mockGitLabAvatar{bot: true}
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)
	store := audit.NewMemoryStore()
	s.auditStore = store

	conn := connectGitLabPAT(t, s, srv.URL)
	// Audit writes are detached (goSafe); poll briefly for the row to land.
	var events []audit.Event
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		events, _ = store.ListByTenant(context.Background(), "t1", audit.Page{Action: "forge.connection.avatar_applied"})
		if len(events) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(events) != 1 || events[0].TargetID != conn.ID || events[0].Meta["automatic"] != true {
		t.Fatalf("audit trail = %+v", events)
	}
}
