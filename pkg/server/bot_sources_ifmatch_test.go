package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"

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

// putFile writes one file. version 0 sends no token at all — the digest it
// still presents: what the write replaces, it holds the digest of the stored
// text it was read at, token or not (the digest rule).
func (f *ifMatchFixture) putFile(t *testing.T, rel, content string, version int) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"content": content}
	if version != 0 {
		// The digest rule, as the drawer plays it: the id its read
		// answered, and the stored text's digest.
		body["version"] = version
		body["id"] = storedIDFor(f.s, f.ctx, f.slug)
	}
	if bs, err := f.s.botSources.GetBySlug(store.WithTenant(f.ctx, "t1"), "t1", f.slug); err == nil {
		if held, ok := bs.Files[rel]; ok {
			body["stored_sha256"] = contentSHA256(held)
		}
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
// token can be exercised as a client would send it. A query that pins a
// version carries the bot's id beside it, the way the PUTs present theirs
// (a delete pinned to a version names the bot that read answered).
func (f *ifMatchFixture) deleteFile(t *testing.T, rel, query string) *httptest.ResponseRecorder {
	t.Helper()
	if strings.Contains(query, "version=") {
		query += "&bot_id=" + storedIDFor(f.s, f.ctx, f.slug)
	}
	return f.deleteFileRaw(t, rel, query)
}

// deleteFileRaw sends the query VERBATIM, for the cases whose shape under
// test IS an absent or empty token.
func (f *ifMatchFixture) deleteFileRaw(t *testing.T, rel, query string) *httptest.ResponseRecorder {
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
	}, nil, "")
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
	}, nil, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("untokened create = %d: %s", w2.Code, w2.Body.String())
	}
}

// deleteBot removes the WHOLE bot. query is appended verbatim so a malformed
// token can be exercised as a client would send it; a query that pins a
// version carries the bot's id beside it, the way the per-file delete does.
func (f *ifMatchFixture) deleteBot(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	if strings.Contains(query, "version=") && !strings.Contains(query, "bot_id=") {
		query += "&bot_id=" + storedIDFor(f.s, f.ctx, f.slug)
	}
	return f.deleteBotRaw(t, query)
}

// deleteBotRaw sends the query VERBATIM — what the identity cases need: the
// shape under test IS an absent or empty token, which deleteBot fills in.
func (f *ifMatchFixture) deleteBotRaw(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("DELETE", "/api/teams/t1/bot-sources/"+f.slug+query, nil).WithContext(f.ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", f.slug)
	w := httptest.NewRecorder()
	f.s.handleDeleteBotSource(w, r)
	return w
}

// gone reports whether the bot is no longer in the store.
func (f *ifMatchFixture) gone(t *testing.T) bool {
	t.Helper()
	_, err := f.s.botSources.GetBySlug(f.ctx, "t1", f.slug)
	switch {
	case err == nil:
		return false
	case errors.Is(err, botsource.ErrNotFound):
		return true
	default:
		t.Fatalf("read back: %v", err)
		return false
	}
}

// Removing a WHOLE bot is the heaviest write of the family, and it used to be
// the only one with no if-match at all: deleteBotSourceFor read the row by
// slug and deleted whatever that read answered, so a delete decided on a
// bundle a client read ten minutes ago removed the version another editor
// published since, with no error anywhere (#2689 item 4).
func TestDeletingAWholeBotSourceHonoursTheIfMatchVersion(t *testing.T) {
	f := newIfMatchFixture(t)

	// A stale token is refused, and the bot is still there.
	w := f.deleteBot(t, "?version=99")
	if w.Code != http.StatusConflict {
		t.Fatalf("stale token = %d, want 409: %s", w.Code, w.Body.String())
	}
	if f.gone(t) {
		t.Fatal("refused delete removed the bot anyway")
	}

	// The fresh one goes through.
	if w := f.deleteBot(t, "?version=1"); w.Code != http.StatusOK {
		t.Fatalf("fresh token = %d: %s", w.Code, w.Body.String())
	}
	if !f.gone(t) {
		t.Fatal("accepted delete left the bot")
	}
}

func TestAWholeBotDeleteWithNoTokenIsLastWriteWins(t *testing.T) {
	f := newIfMatchFixture(t)
	// A caller that holds no token has to be able to delete, and 0 is what
	// "no if-match" is in both store twins. What the route may not do is
	// accept a token and then ignore it.
	if w := f.deleteBot(t, ""); w.Code != http.StatusOK {
		t.Fatalf("untokened delete = %d: %s", w.Code, w.Body.String())
	}
	if !f.gone(t) {
		t.Fatal("untokened delete left the bot")
	}
}

func TestAMalformedIfMatchVersionOnAWholeBotDeleteIsRefused(t *testing.T) {
	f := newIfMatchFixture(t)
	for _, query := range []string{"?version=abc", "?version=0", "?version=-1", "?version=1.5", "?version=", "?version=%20"} {
		w := f.deleteBot(t, query)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400: %s", query, w.Code, w.Body.String())
		}
		if f.gone(t) {
			t.Fatalf("%s removed the bot", query)
		}
	}
	// Read as absent, every one of those would have deleted the WHOLE bot
	// with no check at all — the caller asked for the guarantee and would
	// have got the one that does not guarantee, with no way to tell.
	if got := f.version(t); got != 1 {
		t.Fatalf("version moved under a refused delete: %d", got)
	}
}

// A version alone cannot name one incarnation of a slug: a bot deleted and
// re-created is v1 again (#2678's second sequence), so a stale client's
// delete pinned to v1 would remove the NEW bot. The identity the caller's
// read answered travels beside the version, and a mismatch is a 409 that
// says what happened.
func TestAWholeBotDeletePinsTheBotItsReadAnswered(t *testing.T) {
	f := newIfMatchFixture(t)
	stale := storedIDFor(f.s, f.ctx, f.slug)

	// A version with no identity beside it is refused: the route cannot
	// tell the incarnations apart, so it refuses rather than guess.
	if w := f.deleteBotRaw(t, "?version=1"); w.Code != http.StatusConflict {
		t.Fatalf("version with no identity = %d, want 409: %s", w.Code, w.Body.String())
	}
	if f.gone(t) {
		t.Fatal("identity-less delete removed the bot")
	}

	// `?bot_id=` — what `?bot_id=${id ?? ""}` produces — is the malformed
	// token, not an absent one: read as absent it would hand a caller that
	// asked for the identity check the one that does not check.
	for _, query := range []string{"?version=1&bot_id=", "?bot_id=", "?bot_id=%20"} {
		if w := f.deleteBotRaw(t, query); w.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400: %s", query, w.Code, w.Body.String())
		}
		if f.gone(t) {
			t.Fatalf("%s removed the bot", query)
		}
	}

	// The bot is deleted and re-created under the same slug: a new id, and
	// version 1 again.
	if w := f.deleteBot(t, ""); w.Code != http.StatusOK {
		t.Fatalf("seed delete = %d: %s", w.Code, w.Body.String())
	}
	created, err := f.s.botSources.Create(f.ctx, botsource.BotSource{
		TenantID: "t1",
		Slug:     f.slug,
		Files:    map[string]string{botsource.MainBotFile: testBotMain},
	})
	if err != nil {
		t.Fatalf("re-create: %v", err)
	}
	if created.ID == stale {
		t.Fatal("the re-created bot reused the old id; this test cannot see the defect")
	}
	if got := f.version(t); got != 1 {
		t.Fatalf("re-created version = %d, want 1 (the sequence this guards)", got)
	}

	// The old client's delete — a token that MATCHES the new row's version —
	// is refused on the identity, and the new bot survives.
	w := f.deleteBot(t, "?version=1&bot_id="+stale)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale identity = %d, want 409: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "deleted and re-created") {
		t.Fatalf("the refusal does not name the re-creation: %s", body)
	}
	if f.gone(t) {
		t.Fatal("a delete decided on another incarnation removed the new bot")
	}

	// The id the fresh read answers goes through.
	if w := f.deleteBot(t, "?version=1&bot_id="+created.ID); w.Code != http.StatusOK {
		t.Fatalf("fresh identity = %d: %s", w.Code, w.Body.String())
	}
	if !f.gone(t) {
		t.Fatal("accepted delete left the bot")
	}
}

// The admin twin shares deleteBotSourceFor, so it carries the same guard —
// asserted rather than assumed: the platform bots are the deployment-wide
// code, where a delete nobody checked costs the most.
func TestTheAdminWholeBotDeleteHonoursTheIfMatchVersion(t *testing.T) {
	s, _, admin := newBotSourceTestServer(t)
	ctx := auth.WithIdentity(context.Background(), admin)
	pctx := store.WithTenant(ctx, botsource.PlatformTenantID)
	created, err := s.botSources.Create(pctx, botsource.BotSource{
		TenantID: botsource.PlatformTenantID,
		Slug:     "platform-bot",
		Files:    map[string]string{botsource.MainBotFile: testBotMain},
	})
	if err != nil {
		t.Fatalf("seed platform bot: %v", err)
	}
	del := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("DELETE", "/api/admin/bots/platform-bot"+query, nil).WithContext(ctx)
		r.SetPathValue("slug", "platform-bot")
		w := httptest.NewRecorder()
		s.handleAdminDeletePlatformBot(w, r)
		return w
	}
	if w := del("?version=99&bot_id=" + created.ID); w.Code != http.StatusConflict {
		t.Fatalf("stale token = %d, want 409: %s", w.Code, w.Body.String())
	}
	if _, err := s.botSources.GetBySlug(pctx, botsource.PlatformTenantID, "platform-bot"); err != nil {
		t.Fatalf("refused delete removed the platform bot: %v", err)
	}
	if w := del("?version=" + strconv.Itoa(created.Version) + "&bot_id=" + created.ID); w.Code != http.StatusOK {
		t.Fatalf("fresh token = %d: %s", w.Code, w.Body.String())
	}
	if _, err := s.botSources.GetBySlug(pctx, botsource.PlatformTenantID, "platform-bot"); !errors.Is(err, botsource.ErrNotFound) {
		t.Fatalf("accepted delete left the platform bot: %v", err)
	}
}

// The identity token is parsed at ONE chokepoint (parseIfMatchBotID), so the
// per-file delete carries the same refusal as the whole-bot one: a present
// token with an empty value is the `?bot_id=${id ?? ""}` a client with no id
// in hand sends, and reading it as absent is the very hole the version
// token's own refusal exists to close.
func TestAnEmptyBotIdentityIsRefusedAtBothDeleteDoors(t *testing.T) {
	f := newIfMatchFixture(t)
	for _, query := range []string{"?bot_id=", "?version=1&bot_id=", "?bot_id=%20"} {
		w := f.deleteFileRaw(t, "skills/help.md", query)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("per-file %s = %d, want 400: %s", query, w.Code, w.Body.String())
		}
		if _, held := f.files(t)["skills/help.md"]; !held {
			t.Fatalf("per-file %s removed the file", query)
		}
		w = f.deleteBotRaw(t, query)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("whole-bot %s = %d, want 400: %s", query, w.Code, w.Body.String())
		}
		if f.gone(t) {
			t.Fatalf("whole-bot %s removed the bot", query)
		}
	}
}
