package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
)

// The per-file routes are what a CLIENT writes a bundle through — the studio's
// files drawer, and the platform twin under /api/admin. Both take an if-match
// token so a second editor's change is a refusal rather than a silent
// overwrite; the delete route used to hardcode the token away
// (`bs.Version = 0`), so sending one was a no-op nobody could see (#1650).

type ifMatchFixture struct {
	s    *Server
	ctx  context.Context
	slug string
}

func newIfMatchFixture(t *testing.T) *ifMatchFixture {
	t.Helper()
	s, editor, _ := newBotSourceTestServer(t)
	ctx := auth.WithIdentity(context.Background(), editor)
	f := &ifMatchFixture{s: s, ctx: ctx, slug: "reviewer"}
	body, _ := json.Marshal(botSourcePutReq{Files: map[string]string{
		botsource.MainBotFile: testBotMain,
		"skills/help.md":      "# help\n",
	}})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+f.slug, strings.NewReader(string(body))).WithContext(ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", f.slug)
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("seed = %d: %s", w.Code, w.Body.String())
	}
	if got := f.version(t); got != 1 {
		t.Fatalf("seeded version = %d, want 1", got)
	}
	return f
}

func (f *ifMatchFixture) version(t *testing.T) int {
	t.Helper()
	bs, err := f.s.botSources.GetBySlug(f.ctx, "t1", f.slug)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return bs.Version
}

func (f *ifMatchFixture) files(t *testing.T) map[string]string {
	t.Helper()
	bs, err := f.s.botSources.GetBySlug(f.ctx, "t1", f.slug)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return bs.Files
}

// putFile writes one file. version 0 sends no token at all.
func (f *ifMatchFixture) putFile(t *testing.T, rel, content string, version int) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"content": content}
	if version != 0 {
		body["version"] = version
	}
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+f.slug+"/files/"+rel, strings.NewReader(string(raw))).WithContext(f.ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", f.slug)
	r.SetPathValue("path", rel)
	w := httptest.NewRecorder()
	f.s.handlePutBotSourceFile(w, r)
	return w
}

// deleteFile removes one file. query is appended verbatim so a malformed
// token can be exercised as a client would send it.
func (f *ifMatchFixture) deleteFile(t *testing.T, rel, query string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/teams/t1/bot-sources/" + f.slug + "/files/" + rel + query
	r := httptest.NewRequest("DELETE", url, nil).WithContext(f.ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", f.slug)
	r.SetPathValue("path", rel)
	w := httptest.NewRecorder()
	f.s.handleDeleteBotSourceFile(w, r)
	return w
}

func TestPuttingABotSourceFileHonoursTheIfMatchVersion(t *testing.T) {
	f := newIfMatchFixture(t)

	if w := f.putFile(t, "skills/help.md", "# one\n", 1); w.Code != http.StatusOK {
		t.Fatalf("fresh token = %d: %s", w.Code, w.Body.String())
	}
	if got := f.version(t); got != 2 {
		t.Fatalf("version after write = %d, want 2", got)
	}

	// The same token again: a second editor wrote in between, which is
	// exactly what the drawer presenting a token it read at OPEN looks like
	// once someone else has saved.
	w := f.putFile(t, "skills/help.md", "# two\n", 1)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale token = %d, want 409: %s", w.Code, w.Body.String())
	}
	if got := f.files(t)["skills/help.md"]; got != "# one\n" {
		t.Fatalf("refused write landed anyway: %q", got)
	}
}

func TestDeletingABotSourceFileHonoursTheIfMatchVersion(t *testing.T) {
	f := newIfMatchFixture(t)

	// A stale token is refused, and the file is still there.
	w := f.deleteFile(t, "skills/help.md", "?version=99")
	if w.Code != http.StatusConflict {
		t.Fatalf("stale token = %d, want 409: %s", w.Code, w.Body.String())
	}
	if _, held := f.files(t)["skills/help.md"]; !held {
		t.Fatal("refused delete removed the file anyway")
	}

	// The fresh one goes through.
	if w := f.deleteFile(t, "skills/help.md", "?version=1"); w.Code != http.StatusOK {
		t.Fatalf("fresh token = %d: %s", w.Code, w.Body.String())
	}
	if _, held := f.files(t)["skills/help.md"]; held {
		t.Fatal("accepted delete left the file")
	}
}

func TestABotSourceFileWriteWithNoTokenIsLastWriteWins(t *testing.T) {
	f := newIfMatchFixture(t)
	// Not a defect to fix here: a caller that holds no token has to be able
	// to write, and 0 is what "no if-match" is in both store twins. What the
	// route may not do is accept a token and then ignore it.
	if w := f.putFile(t, "skills/help.md", "# one\n", 0); w.Code != http.StatusOK {
		t.Fatalf("untokened put = %d: %s", w.Code, w.Body.String())
	}
	if w := f.deleteFile(t, "skills/help.md", ""); w.Code != http.StatusOK {
		t.Fatalf("untokened delete = %d: %s", w.Code, w.Body.String())
	}
	if _, held := f.files(t)["skills/help.md"]; held {
		t.Fatal("untokened delete left the file")
	}
}

func TestAMalformedIfMatchVersionIsRefusedRatherThanReadAsAbsent(t *testing.T) {
	f := newIfMatchFixture(t)
	for _, query := range []string{"?version=abc", "?version=0", "?version=-1", "?version=1.5"} {
		w := f.deleteFile(t, "skills/help.md", query)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400: %s", query, w.Code, w.Body.String())
		}
		if _, held := f.files(t)["skills/help.md"]; !held {
			t.Fatalf("%s removed the file", query)
		}
	}
	// Read as absent, every one of those would have deleted the file with no
	// check at all — the caller asked for the guarantee and would have got
	// the one that does not guarantee, with no way to tell.
	if got := f.version(t); got != 1 {
		t.Fatalf("version moved under a refused delete: %d", got)
	}
}

// A stored bot whose main.bot does not parse is repairable through the
// per-file route — the capability the studio's files drawer routes the main's
// row to since #1659. On the local twin an author opens the file where it
// lives; on the cloud twin the bundle's files live in the store, so this
// route is the only surface that can reach them, and the drawer used to send
// the main to the canvas, which refuses a salvaged document at every write.
func TestABrokenMainIsRepairableThroughThePerFileRoute(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	ctx := auth.WithIdentity(context.Background(), editor)
	f := &ifMatchFixture{s: s, ctx: ctx, slug: "broken"}

	// The store holds a bundle whose main does not parse. The write routes
	// refuse to CREATE one, so it arrives the ways a broken main really
	// arrives: an out-of-band push, or a fork of a bundle that broke later.
	const brokenMain = "tool work:\n  command: `printf ok`\n  output: out\n\nworkflow main:\n  entry: work\n  work -> !!! not a route\n"
	if _, err := s.botSources.Create(ctx, botsource.BotSource{
		TenantID: "t1",
		Slug:     f.slug,
		Files:    map[string]string{botsource.MainBotFile: brokenMain},
	}); err != nil {
		t.Fatalf("seed broken bundle: %v", err)
	}

	// The drawer reads the file as stored — broken text and all — so the
	// author has something to repair.
	if got := f.files(t)[botsource.MainBotFile]; got != brokenMain {
		t.Fatalf("stored main is not the broken text: %q", got)
	}

	// The repair lands.
	if w := f.putFile(t, botsource.MainBotFile, testBotMain, 1); w.Code != http.StatusOK {
		t.Fatalf("repair = %d: %s", w.Code, w.Body.String())
	}
	if got := f.files(t)[botsource.MainBotFile]; got != testBotMain {
		t.Fatalf("repair did not land: %q", got)
	}

	// …and a write that leaves the main broken is still refused, with the
	// reason. The route gains a repair path, not a hole.
	if w := f.putFile(t, botsource.MainBotFile, brokenMain, 2); w.Code != http.StatusBadRequest {
		t.Fatalf("still-broken write = %d, want 400: %s", w.Code, w.Body.String())
	}
	if got := f.files(t)[botsource.MainBotFile]; got != testBotMain {
		t.Fatalf("refused write landed anyway: %q", got)
	}
}

// `?version=` — what `?version=${token ?? ""}` produces — must not read as
// "no token": `Query().Get` cannot tell an absent key from an empty one, and
// reading it as absent hands a caller that asked for the check the one that
// does not check.
func TestAnEmptyIfMatchVersionIsRefusedLikeAMalformedOne(t *testing.T) {
	f := newIfMatchFixture(t)
	for _, query := range []string{"?version=", "?version=%20"} {
		w := f.deleteFile(t, "skills/help.md", query)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400: %s", query, w.Code, w.Body.String())
		}
		if _, held := f.files(t)["skills/help.md"]; !held {
			t.Fatalf("%s removed the file", query)
		}
	}
}

// A token names a row the caller read. If the bot is deleted between the
// handler's read and the write's, the write must not fall through to a
// create and resurrect the bundle from that caller's stale snapshot — and
// the refusal must say the bot is GONE rather than reuse the version
// conflict, whose sentence sends a client looking for another editor's
// change and offers a reload that 404s.
func TestAnIfMatchTokenOnADeletedBotIsRefusedAsGoneNotResurrected(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	ctx := auth.WithIdentity(context.Background(), editor)
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/ghost", nil).WithContext(ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", "ghost")
	w := httptest.NewRecorder()
	s.writeBotSource(w, r, "t1", "ed", botsource.BotSource{
		TenantID: "t1",
		Slug:     "ghost",
		Files:    map[string]string{botsource.MainBotFile: testBotMain},
		Version:  7,
	})
	// 404 and not the version conflict: the bot was DELETED, and a client
	// told "another editor wrote to it, reload to see" is given a false
	// diagnosis and a reload that cannot succeed — the studio's drawer maps
	// every 409 to exactly that banner.
	if w.Code != http.StatusNotFound {
		t.Fatalf("token on a vanished bot = %d, want 404: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "deleted since you read") {
		t.Fatalf("the refusal does not say the bot was deleted: %s", body)
	}
	if _, err := s.botSources.GetBySlug(ctx, "t1", "ghost"); err == nil {
		t.Fatal("the refused write created the row anyway")
	}

	// …and a creation, which carries no token, still goes through.
	w2 := httptest.NewRecorder()
	s.writeBotSource(w2, r, "t1", "ed", botsource.BotSource{
		TenantID: "t1",
		Slug:     "ghost",
		Files:    map[string]string{botsource.MainBotFile: testBotMain},
	})
	if w2.Code != http.StatusOK {
		t.Fatalf("untokened create = %d: %s", w2.Code, w2.Body.String())
	}
}
