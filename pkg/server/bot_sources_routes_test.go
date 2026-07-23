package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A minimal tool-only bot that compiles (mirrors examples/composition/child.bot).
const testBotMain = "tool work:\n" +
	"  command: `printf ok`\n" +
	"  output: out\n" +
	"\n" +
	"schema out:\n" +
	"  ok: bool\n" +
	"\n" +
	"workflow main:\n" +
	"  entry: work\n" +
	"  work -> done\n"

func newBotSourceTestServer(t *testing.T) (*Server, auth.Identity, auth.Identity) {
	t.Helper()
	s := newOrgTestServer(t)
	s.botSources = botsource.NewMemoryStore()
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	editor := seedTeamMember(t, s, ctx, "ed", identity.RoleConfigEditor)
	viewer := seedTeamMember(t, s, ctx, "vi", identity.RoleViewer)
	return s, editor, viewer
}

func TestBotSource_EndToEnd(t *testing.T) {
	s, editor, viewer := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)

	put := func(slug, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+slug, strings.NewReader(body)).WithContext(edCtx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", slug)
		w := httptest.NewRecorder()
		s.handlePutBotSource(w, r)
		return w
	}

	// ---- create a valid bot ----
	body, _ := json.Marshal(botSourcePutReq{Files: map[string]string{botsource.MainBotFile: testBotMain}})
	w := put("reviewer", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var created botsource.BotSource
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 || created.Origin != "tenant" || created.CreatedBy != "ed" {
		t.Fatalf("create stamps wrong: %+v", created)
	}

	// ---- get returns the files ----
	gr := httptest.NewRequest("GET", "/api/teams/t1/bot-sources/reviewer", nil).WithContext(edCtx)
	gr.SetPathValue("id", "t1")
	gr.SetPathValue("slug", "reviewer")
	gw := httptest.NewRecorder()
	s.handleGetBotSource(gw, gr)
	if gw.Code != http.StatusOK || !strings.Contains(gw.Body.String(), "printf ok") {
		t.Fatalf("get = %d: %s", gw.Code, gw.Body.String())
	}

	// ---- update (same slug) bumps version ----
	body2, _ := json.Marshal(botSourcePutReq{Files: map[string]string{
		botsource.MainBotFile: testBotMain,
		"skills/help.md":      "# help\n",
	}, Version: 1})
	w2 := put("reviewer", string(body2))
	if w2.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", w2.Code, w2.Body.String())
	}
	var updated botsource.BotSource
	_ = json.Unmarshal(w2.Body.Bytes(), &updated)
	if updated.Version != 2 {
		t.Fatalf("want version 2, got %d", updated.Version)
	}

	// ---- stale if-match version → 409 ----
	if w3 := put("reviewer", string(body2)); w3.Code != http.StatusConflict {
		t.Fatalf("stale version want 409, got %d: %s", w3.Code, w3.Body.String())
	}

	// ---- a non-compiling bot → 400 ----
	bad, _ := json.Marshal(botSourcePutReq{Files: map[string]string{botsource.MainBotFile: "this is not a bot"}})
	if wb := put("broken", string(bad)); wb.Code != http.StatusBadRequest {
		t.Fatalf("bad bot want 400, got %d: %s", wb.Code, wb.Body.String())
	}

	// ---- a viewer is 403 ----
	viCtx := auth.WithIdentity(context.Background(), viewer)
	vr := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/reviewer", strings.NewReader(string(body))).WithContext(viCtx)
	vr.SetPathValue("id", "t1")
	vr.SetPathValue("slug", "reviewer")
	vw := httptest.NewRecorder()
	s.handlePutBotSource(vw, vr)
	if vw.Code != http.StatusForbidden {
		t.Fatalf("viewer want 403, got %d", vw.Code)
	}
}

func TestBotSource_Fork(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)

	// Bake a catalog bot on the pod filesystem.
	dir := t.TempDir()
	botDir := filepath.Join(dir, "bots", "seed-bot")
	if err := os.MkdirAll(filepath.Join(botDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "main.bot"), []byte(testBotMain), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "manifest.yaml"), []byte("name: seed-bot\ndisplay_name: Seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "skills", "x.md"), []byte("# x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots = BotsConfig{Paths: []string{filepath.Join(dir, "bots")}}

	edCtx := auth.WithIdentity(context.Background(), editor)
	r := httptest.NewRequest("POST", "/api/teams/t1/bot-sources/my-fork/fork", strings.NewReader(`{"from":"seed-bot"}`)).WithContext(edCtx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", "my-fork")
	w := httptest.NewRecorder()
	s.handleForkBotSource(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("fork = %d: %s", w.Code, w.Body.String())
	}
	var forked botsource.BotSource
	if err := json.Unmarshal(w.Body.Bytes(), &forked); err != nil {
		t.Fatal(err)
	}
	if forked.Origin != "forked:seed-bot" {
		t.Errorf("origin = %q", forked.Origin)
	}
	for _, want := range []string{"main.bot", "manifest.yaml", "skills/x.md"} {
		if _, ok := forked.Files[want]; !ok {
			t.Errorf("fork missing file %q (have %v)", want, keysOf(forked.Files))
		}
	}
}

// TestBotSource_GalleryMergeAndLaunchResolve proves the two read paths that make
// a team bot usable: it appears in /api/v1/bots marked editable, and the launch
// source resolver returns its main.bot inline.
func TestBotSource_GalleryMergeAndLaunchResolve(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	edCtx := auth.WithIdentity(context.Background(), editor)

	if _, err := s.botSources.Create(store.WithTenant(edCtx, "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "reviewer",
		Files: map[string]string{botsource.MainBotFile: testBotMain, "skills/x.md": "# x\n"},
	}); err != nil {
		t.Fatal(err)
	}

	// ---- gallery merge: the tenant bot shows up editable ----
	lr := httptest.NewRequest("GET", "/api/v1/bots", nil).WithContext(edCtx)
	lw := httptest.NewRecorder()
	s.handleBotsList(lw, lr)
	if lw.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", lw.Code, lw.Body.String())
	}
	var listResp struct {
		Bots []struct {
			Name     string `json:"name"`
			Editable bool   `json:"editable"`
			Origin   string `json:"origin"`
		} `json:"bots"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, b := range listResp.Bots {
		if b.Name == "reviewer" {
			found = true
			if !b.Editable || b.Origin != "tenant" {
				t.Errorf("tenant bot not marked editable/tenant: %+v", b)
			}
		}
	}
	if !found {
		t.Fatalf("tenant bot missing from gallery: %s", lw.Body.String())
	}

	// ---- launch resolver returns the tenant main.bot inline ----
	src, slug, ok := s.tenantBotSource(edCtx, "reviewer", "")
	if !ok || slug != "reviewer" || !strings.Contains(src, "printf ok") {
		t.Fatalf("tenantBotSource = %q %q %v", src, slug, ok)
	}
	// A team with no such bot resolves nothing (falls back to catalog).
	if _, _, ok := s.tenantBotSource(edCtx, "nope", ""); ok {
		t.Error("tenantBotSource should miss on an unknown slug")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
