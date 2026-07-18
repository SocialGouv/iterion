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

const shareTestConfig = `{"categories":{
  "a11y":{"digest_title":"A11y","feeds":["https://a.example/rss"],"editorial":"old","sinks":[{"webhook":"prod"}]},
  "cyber":{"digest_title":"Cyber","feeds":["https://c.example/rss"],"editorial":"sec"}
},"notes":"internal"}`

type fakeShareFC struct {
	content []byte
	sha     string
	put     []byte
	puts    int
}

func (f *fakeShareFC) GetFile(_ context.Context, _, p, ref string) (forge.FileRef, error) {
	return forge.FileRef{Path: p, Content: f.content, SHA: f.sha, Ref: ref}, nil
}
func (f *fakeShareFC) PutFile(_ context.Context, _ string, in forge.PutFile) (forge.FileRef, error) {
	f.puts++
	f.put = in.Content
	return forge.FileRef{Path: in.Path, SHA: "sha-2"}, nil
}

func shareReq(method, path, id, token, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.SetPathValue("id", id)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestConfigShare_EndToEnd(t *testing.T) {
	s := newOrgTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{ID: "op", Email: "op@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	fc := &fakeShareFC{content: []byte(shareTestConfig), sha: "sha-1"}
	s.configShareFC = func(context.Context, *configshare.Share) (forge.FileClient, error) { return fc, nil }

	// ---- Operator mints a share (admin identity, real user) ----
	adminCtx := auth.WithIdentity(ctx, auth.Identity{UserID: "op", TeamID: "t1", Role: identity.RoleAdmin})
	body := `{"bot_id":"feed-watch","label":"a11y","repo_url":"https://github.com/o/r","repo_ref":"main",
	  "config_path":"feed-watch.json","category":"a11y",
	  "allowed_paths":["categories.a11y.feeds","categories.a11y.editorial"],
	  "visible_paths":["categories.a11y.feeds","categories.a11y.editorial","categories.a11y.digest_title"]}`
	cr := httptest.NewRequest("POST", "/api/teams/t1/config-shares", strings.NewReader(body)).WithContext(adminCtx)
	cr.SetPathValue("id", "t1")
	cw := httptest.NewRecorder()
	s.handleCreateConfigShare(cw, cr)
	if cw.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", cw.Code, cw.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(cw.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	shareID, token := created["id"].(string), created["token"].(string)
	if shareID == "" || !strings.HasPrefix(token, "iws_") {
		t.Fatalf("bad create response: %v", created)
	}

	get := func(id, tok string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.configShareAuth(http.HandlerFunc(s.handleConfigShareGet)).ServeHTTP(w, shareReq("GET", "/api/config-share/"+id+"/config", id, tok, ""))
		return w
	}

	// ---- GET projects to the scoped fields only ----
	w := get(shareID, token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET config = %d: %s", w.Code, w.Body.String())
	}
	gs := w.Body.String()
	for _, want := range []string{"a11y", "editorial", "digest_title", "sha-1"} {
		if !strings.Contains(gs, want) {
			t.Fatalf("GET missing %q: %s", want, gs)
		}
	}
	for _, no := range []string{"cyber", "sinks", "prod", "notes", "internal"} {
		if strings.Contains(gs, no) {
			t.Fatalf("GET LEAKED %q: %s", no, gs)
		}
	}

	// ---- bad / missing token → uniform 401 ----
	if c := get(shareID, "iws_wrong").Code; c != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", c)
	}
	if c := get(shareID, "").Code; c != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", c)
	}
	if c := get("nope", token).Code; c != http.StatusUnauthorized {
		t.Fatalf("unknown id = %d, want 401", c)
	}

	patch := func(payload string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.configShareAuth(http.HandlerFunc(s.handleConfigSharePatch)).ServeHTTP(w, shareReq("PATCH", "/api/config-share/"+shareID+"/config", shareID, token, payload))
		return w
	}

	// ---- PATCH an allowed field → merged, write lands ----
	pw := patch(`{"patch":{"categories":{"a11y":{"editorial":"fresh"}}},"sha":"sha-1"}`)
	if pw.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", pw.Code, pw.Body.String())
	}
	if fc.puts != 1 || !strings.Contains(string(fc.put), `"fresh"`) || !strings.Contains(string(fc.put), `"sinks"`) {
		t.Fatalf("merged write wrong (puts=%d): %s", fc.puts, fc.put)
	}

	// ---- PATCH an off-list field → 400, no write ----
	fc.puts = 0
	if c := patch(`{"patch":{"categories":{"a11y":{"sinks":[{"webhook":"x"}]}}},"sha":"sha-1"}`).Code; c != http.StatusBadRequest {
		t.Fatalf("off-list PATCH = %d, want 400", c)
	}
	if fc.puts != 0 {
		t.Fatalf("off-list PATCH wrote to forge (puts=%d)", fc.puts)
	}

	// ---- PATCH with an empty sha → 400, no blind write ----
	fc.puts = 0
	if c := patch(`{"patch":{"categories":{"a11y":{"editorial":"x"}}},"sha":""}`).Code; c != http.StatusBadRequest {
		t.Fatalf("empty-sha PATCH = %d, want 400", c)
	}
	if fc.puts != 0 {
		t.Fatalf("empty-sha PATCH wrote to forge (puts=%d)", fc.puts)
	}

	// ---- a delivery audit row landed for the successful write ----
	// (best-effort/detached; the memory store records synchronously enough)
	rows, _ := s.configShares.ListDeliveries(ctx, shareID, 10)
	if len(rows) == 0 {
		t.Fatal("no delivery audit row recorded")
	}
}

// TestConfigShare_AdminGatesRejectSynthetic confirms a share (synthetic)
// identity cannot reach the operator CRUD even with a matching team.
func TestConfigShare_AdminGatesRejectSynthetic(t *testing.T) {
	s := newOrgTestServer(t)
	seedTeam(t, s, "t1", "acme")
	synCtx := auth.WithIdentity(context.Background(), auth.Identity{
		UserID: "share:x", TeamID: "t1", Role: identity.RoleAdmin, Kind: auth.KindShare,
	})
	r := httptest.NewRequest("GET", "/api/teams/t1/config-shares", nil).WithContext(synCtx)
	r.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	s.handleListConfigShares(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("synthetic identity reached operator list = %d, want 403", w.Code)
	}
}
