package server

// The digest rule's cloud half, every stored file of it: the bundle read
// (GET .../bot-sources/{slug}) issues a digest for every file it answers
// (file_sha256), so a write that REPLACES or DROPS any of them presents that
// digest — manifest.yaml and the bundle's other non-.bot files included.
// Absence is a write decided on no read: a 409 naming the file, never a
// silent deletion or a digest-less replacement. Bytes are asserted on every
// row.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
)

const everyFileSkill = "# skill\nDo the thing.\n"

// deleteBundle removes a stored bot the way a client does.
func deleteBundle(s *Server, ctx context.Context, slug string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("DELETE", "/api/teams/t1/bot-sources/"+slug, nil).WithContext(ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	s.handleDeleteBotSource(w, r)
	return w
}

// everyFileCreate stores a three-file bot and answers the bundle as a client
// read it: the files, the id, the version, every digest.
func everyFileCreate(t *testing.T, s *Server, ctx context.Context, slug string) (map[string]string, string, int, map[string]string) {
	t.Helper()
	files := map[string]string{
		botsource.MainBotFile: testBotMain,
		"skills/x.md":         everyFileSkill,
		"prompts/p.md":        "Say hello.\n",
	}
	if w := deleteBundle(s, ctx, slug); w.Code != http.StatusOK && w.Code != http.StatusNotFound {
		t.Fatalf("delete before create = %d %s", w.Code, w.Body.String())
	}
	if rec := putBundle(s, ctx, slug, files, 0); rec.Code != http.StatusOK {
		t.Fatalf("creation = %d %s", rec.Code, rec.Body.String())
	}
	stored, id, version, digests := getBundle(t, s, ctx, slug)
	if len(stored) != 3 || digests["skills/x.md"] != contentSHA256(everyFileSkill) {
		t.Fatalf("precondition: stored %d files, digests %v", len(stored), digests)
	}
	return stored, id, version, digests
}

// TestACloudWriteCarriesEveryStoredFilesDigest: the r8a shapes, now the rule.
// A bundle PUT that omits a non-workflow stored file — with the other files'
// digests presented, or with none at all — is a 409 naming the file and
// keeping its bytes; a per-file PUT of it without its digest is refused; the
// knowing deletion (the omission carrying the omitted file's digest) lands.
func TestACloudWriteCarriesEveryStoredFilesDigest(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	ctx := auth.WithIdentity(context.Background(), editor)
	assertKept := func(what string) {
		t.Helper()
		now, _, _, digests := getBundle(t, s, ctx, "ef")
		if _, held := now["skills/x.md"]; !held {
			t.Fatalf("%s: skills/x.md is GONE from the bundle (%d files)", what, len(now))
		}
		if now["skills/x.md"] != everyFileSkill {
			t.Fatalf("%s: skills/x.md is %q, want the stored bytes", what, now["skills/x.md"])
		}
		if digests["skills/x.md"] != contentSHA256(everyFileSkill) {
			t.Fatalf("%s: skills/x.md's digest = %q, want the stored bytes'", what, digests["skills/x.md"])
		}
	}

	// The omission with the kept files' digests presented (the old client's
	// silence on the file it drops): refused, the file standing.
	stored, id, version, digests := everyFileCreate(t, s, ctx, "ef")
	kept := map[string]string{botsource.MainBotFile: stored[botsource.MainBotFile], "prompts/p.md": stored["prompts/p.md"]}
	rec := putBundleWith(t, s, ctx, "ef", id, version, kept, map[string]string{
		botsource.MainBotFile: digests[botsource.MainBotFile],
		"prompts/p.md":        digests["prompts/p.md"],
	})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "skills/x.md") || !strings.Contains(rec.Body.String(), "digest") {
		t.Fatalf("1a the omission without the dropped file's digest = %d %s, want 409 naming skills/x.md and its digest", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "delete it by name") {
		t.Fatalf("1a the refusal %s carries no remedy for the omission it names", rec.Body.String())
	}
	assertKept("1a after the refused omission")

	// The same omission with NO stored_sha256 at all: refused.
	stored, id, version, _ = everyFileCreate(t, s, ctx, "ef")
	kept = map[string]string{botsource.MainBotFile: stored[botsource.MainBotFile], "prompts/p.md": stored["prompts/p.md"]}
	if rec := putBundleWith(t, s, ctx, "ef", id, version, kept, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "skills/x.md") {
		t.Fatalf("1b the omission with no digests at all = %d %s, want 409 naming skills/x.md", rec.Code, rec.Body.String())
	}
	assertKept("1b after the digest-less omission")

	// A per-file PUT replacing a non-workflow file without its digest: refused.
	if rec := putFileWith(t, s, ctx, "ef", "skills/x.md", "# replaced\n", id, version, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "digest") {
		t.Fatalf("1c the per-file PUT without a digest = %d %s, want 409 naming the digest", rec.Code, rec.Body.String())
	}
	assertKept("1c after the digest-less per-file PUT")

	// A wrong digest on a CHANGED non-workflow file: the 409 naming what
	// changed. (A byte-identical PUT of the same file changes nothing and is
	// skipped before any digest is asked.)
	if rec := putFileWith(t, s, ctx, "ef", "skills/x.md", "# replaced\n", id, version, "deadbeef"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not the text") {
		t.Fatalf("1e a wrong digest on a changed non-workflow file = %d %s, want 409 naming the text", rec.Code, rec.Body.String())
	}
	assertKept("1e after the wrong-digest PUT")

	// The rule's own power, unchanged: the omission carrying the omitted
	// file's digest is the knowing deletion, and lands.
	stored, id, version, digests = everyFileCreate(t, s, ctx, "ef")
	kept = map[string]string{botsource.MainBotFile: stored[botsource.MainBotFile], "prompts/p.md": stored["prompts/p.md"]}
	if rec := putBundleWith(t, s, ctx, "ef", id, version, kept, digests); rec.Code != http.StatusOK {
		t.Fatalf("the knowing deletion = %d %s, want 200", rec.Code, rec.Body.String())
	}
	now, _, _, nowDigests := getBundle(t, s, ctx, "ef")
	if _, held := now["skills/x.md"]; held {
		t.Fatal("the knowing deletion did not delete")
	}
	if _, held := nowDigests["prompts/p.md"]; !held {
		t.Fatal("the knowing deletion dropped a file it kept")
	}
}

// TestAWriteThatDeletesTheManifestMeetsTheEngineFloor: the engine-floor
// guard judges the RESULTING state. A write that deletes the manifest — a
// whole-bundle push omitting it, a per-file DELETE naming it — sheds the
// `requires.iterion` the stored manifest declared, checked by nothing after
// it lands, and is refused like a floor the manifest does not reach; forced,
// it lands carrying its warning. A write that keeps the manifest is judged by
// it, as before.
func TestAWriteThatDeletesTheManifestMeetsTheEngineFloor(t *testing.T) {
	pinServerBuild(t, "v3.116.4+deadbeef")
	s, editor, _ := newBotSourceTestServer(t)
	ctx := auth.WithIdentity(context.Background(), editor)
	manifest := "name: mf\nversion: 1.0.0\nrequires:\n  iterion: \">= 3.112.14\"\n"
	files := map[string]string{
		botsource.MainBotFile: "workflow main:\n  entry: done\n",
		"manifest.yaml":       manifest,
		"skills/x.md":         everyFileSkill,
	}
	create := func() (map[string]string, string, int, map[string]string) {
		if w := deleteBundle(s, ctx, "mf"); w.Code != http.StatusOK && w.Code != http.StatusNotFound {
			t.Fatalf("delete before create = %d %s", w.Code, w.Body.String())
		}
		if rec := putBundle(s, ctx, "mf", files, 0); rec.Code != http.StatusOK {
			t.Fatalf("creation = %d %s", rec.Code, rec.Body.String())
		}
		return getBundle(t, s, ctx, "mf")
	}
	assertManifest := func(what string) {
		t.Helper()
		now, _, _, _ := getBundle(t, s, ctx, "mf")
		if now["manifest.yaml"] != manifest {
			t.Fatalf("%s: the manifest moved: %q", what, now["manifest.yaml"])
		}
	}

	// The knowing deletion (the omitted manifest's digest presented): the
	// digest rule passes it, the engine guard refuses — the requirement dies
	// with the file.
	stored, id, version, digests := create()
	kept := map[string]string{botsource.MainBotFile: stored[botsource.MainBotFile], "skills/x.md": stored["skills/x.md"]}
	rec := putBundleWith(t, s, ctx, "mf", id, version, kept, digests)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "deletes the manifest") || !strings.Contains(rec.Body.String(), "3.112.14") {
		t.Fatalf("the manifest-deleting push = %d %s, want 409 naming the shed requirement", rec.Code, rec.Body.String())
	}
	assertManifest("after the refused deletion")

	// Forced, the same write lands carrying its warning.
	stored, id, version, digests = create()
	b, _ := json.Marshal(botSourcePutReq{
		Files:        map[string]string{botsource.MainBotFile: stored[botsource.MainBotFile], "skills/x.md": stored["skills/x.md"]},
		Version:      version,
		ID:           id,
		StoredSHA256: digests,
	})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/mf?force=1", bytes.NewReader(b)).WithContext(ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", "mf")
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "FORCED") || !strings.Contains(w.Body.String(), "deletes the manifest") {
		t.Fatalf("the forced deletion = %d %s, want 200 carrying the FORCED warning", w.Code, w.Body.String())
	}

	// The file door's DELETE of the manifest: the same refusal.
	_, id, version, _ = create()
	r = httptest.NewRequest("DELETE", "/api/teams/t1/bot-sources/mf/files/manifest.yaml?version="+strconv.Itoa(version)+"&bot_id="+id, nil).WithContext(ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", "mf")
	r.SetPathValue("path", "manifest.yaml")
	w = httptest.NewRecorder()
	s.handleDeleteBotSourceFile(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "deletes the manifest") {
		t.Fatalf("the DELETE of the manifest = %d %s, want 409 naming the shed requirement", w.Code, w.Body.String())
	}
	assertManifest("after the refused DELETE")

	// And a push that KEEPS the manifest is judged by it, as before: the
	// floor here admits 3.112.14, so the same files land.
	_, _, cur, _ := getBundle(t, s, ctx, "mf")
	if rec := putBundle(s, ctx, "mf", files, cur); rec.Code != http.StatusOK {
		t.Fatalf("a push keeping the manifest = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// TestADeletePresentsTheIdentityItsReadAnswered: the by-name DELETE carries
// the bot's id the way the PUTs present theirs — required with a version, and
// the presented id is what the store's identity guard judges, so #2678's
// second sequence (a bot deleted and re-created, the version free again) is a
// 409 through the DELETE door too. Absent, the delete stays last-write-wins.
func TestADeletePresentsTheIdentityItsReadAnswered(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	ctx := auth.WithIdentity(context.Background(), editor)
	files := map[string]string{botsource.MainBotFile: testBotMain, "skills/x.md": everyFileSkill}
	del := func(version int, botID string) *httptest.ResponseRecorder {
		t.Helper()
		q := ""
		if version != 0 {
			q = "?version=" + strconv.Itoa(version)
			if botID != "" {
				q += "&bot_id=" + botID
			}
		}
		r := httptest.NewRequest("DELETE", "/api/teams/t1/bot-sources/di/files/skills/x.md"+q, nil).WithContext(ctx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", "di")
		r.SetPathValue("path", "skills/x.md")
		w := httptest.NewRecorder()
		s.handleDeleteBotSourceFile(w, r)
		return w
	}
	assertThere := func(what string) {
		t.Helper()
		now, _, _, _ := getBundle(t, s, ctx, "di")
		if now["skills/x.md"] != everyFileSkill {
			t.Fatalf("%s: skills/x.md is %q (%d files), want the stored bytes", what, now["skills/x.md"], len(now))
		}
	}

	if w := deleteBundle(s, ctx, "di"); w.Code != http.StatusOK && w.Code != http.StatusNotFound {
		t.Fatalf("delete before create = %d %s", w.Code, w.Body.String())
	}
	if rec := putBundle(s, ctx, "di", files, 0); rec.Code != http.StatusOK {
		t.Fatalf("creation = %d %s", rec.Code, rec.Body.String())
	}
	_, _, version, _ := getBundle(t, s, ctx, "di")

	// Pinned to a version with no identity: refused like the PUTs'.
	if w := del(version, ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "identity") {
		t.Fatalf("a pinned delete with no bot_id = %d %s, want 409 naming the identity", w.Code, w.Body.String())
	}
	assertThere("after the identity-less delete")

	// A bot deleted and re-created takes no old client's delete: the presented
	// id names the old read, the store's identity guard refuses on its own
	// fresh read.
	_, oldID, _, _ := getBundle(t, s, ctx, "di")
	if w := deleteBundle(s, ctx, "di"); w.Code != http.StatusOK {
		t.Fatalf("delete for the re-creation = %d %s", w.Code, w.Body.String())
	}
	if rec := putBundle(s, ctx, "di", files, 0); rec.Code != http.StatusOK {
		t.Fatalf("re-creation = %d %s", rec.Code, rec.Body.String())
	}
	_, _, newVersion, _ := getBundle(t, s, ctx, "di")
	if w := del(newVersion, oldID); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "re-created") {
		t.Fatalf("#2678's second sequence through the DELETE door = %d %s, want 409 naming the re-creation", w.Code, w.Body.String())
	}
	assertThere("after the old-id delete on the re-created bot")

	// The same delete presenting the re-created bot's own id: lands.
	_, newID, newVersion, _ := getBundle(t, s, ctx, "di")
	if w := del(newVersion, newID); w.Code != http.StatusOK {
		t.Fatalf("the delete presenting the re-created bot's id = %d %s, want 200", w.Code, w.Body.String())
	}
	now, _, _, _ := getBundle(t, s, ctx, "di")
	if _, held := now["skills/x.md"]; held {
		t.Fatal("the delete with the right id did not delete")
	}

	// Unchecked (no version), the delete stays last-write-wins, as documented.
	if w := del(0, ""); w.Code != http.StatusOK {
		t.Fatalf("an unchecked delete = %d %s, want 200 (last-write-wins)", w.Code, w.Body.String())
	}
}
