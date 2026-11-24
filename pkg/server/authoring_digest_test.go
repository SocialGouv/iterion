package server

// The digest rule — what an open decided, the write carries. A write of an
// existing stored bot file presents the digest the whole read that seeded
// its document issued: the stored file must still hash to it, or the write
// is a 409 naming what changed. An open that did not read a file whole
// issues no digest for it; a refused open issues none. A creation decides
// on the file's absence and needs none. Bytes are asserted on every row.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botscaffold"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

// saveDoc is a single-file save: the digest rule's disk door.
func saveDoc(t *testing.T, s *Server, path string, document json.RawMessage, digest string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(saveFileRequest{Path: path, Document: document, Digest: digest})
	rec := httptest.NewRecorder()
	s.handleSaveFile(rec, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(b)))
	return rec
}

const digestRuleSingle = unitFixtureNodes + "\nworkflow w:\n  entry: worker\n  worker -> done\n"

// TestASaveCarriesTheDigestItsOpenIssued: the whole read an open is issues
// the digest; a save presents it and lands; a save without it is refused
// before anything is written; a save whose digest another writer's edit has
// made stale is a 409 naming what changed, and the other writer's bytes
// stand.
func TestASaveCarriesTheDigestItsOpenIssued(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": digestRuleSingle})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	if opened.Digest == "" {
		t.Fatal("the open issued no digest for a file it read whole")
	}
	if want := contentSHA256(opened.Source); opened.Digest != want {
		t.Fatalf("the open's digest = %s, want the read bytes' %s", opened.Digest, want)
	}
	doc := editDocument(t, opened.Document, func(m map[string]any) {
		prompts, _ := m["prompts"].([]any)
		m["prompts"] = append(prompts, map[string]any{"name": "note", "body": "An edit made in the canvas."})
	})
	// No digest, an existing file: refused, nothing written.
	if rec := saveDoc(t, s, "demo/main.bot", doc, ""); rec.Code != http.StatusConflict {
		t.Fatalf("a save without a digest = %d %s, want 409", rec.Code, rec.Body.String())
	}
	assertFiles(t, workdir, map[string]string{"demo/main.bot": digestRuleSingle}, "after the digest-less save")

	// The open's digest: the write lands, and its answer issues the new
	// bytes' digest for the next save.
	rec, out := savePathDigest(t, s, "demo/main.bot", doc, opened.Digest)
	if rec.Code != http.StatusOK {
		t.Fatalf("a save carrying the open's digest = %d %s, want 200", rec.Code, rec.Body.String())
	}
	written, err := os.ReadFile(filepath.Join(workdir, "demo", "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != string(written) {
		t.Fatalf("the save's answer holds %d bytes, the file %d — the answer is not what was written", len(out.Source), len(written))
	}
	if out.Digest != contentSHA256(string(written)) {
		t.Fatalf("the save answered digest %s, want the written bytes' %s", out.Digest, contentSHA256(string(written)))
	}
	// Another writer edits on disk; the stale digest (the open's, the bytes
	// this document was decided on) refuses the save and keeps the other
	// writer's bytes, the refusal naming what changed.
	other := string(written) + "\nprompt theirs:\n  Written by the other writer.\n"
	writeFiles(t, workdir, map[string]string{"demo/main.bot": other})
	if rec := saveDoc(t, s, "demo/main.bot", doc, opened.Digest); rec.Code != http.StatusConflict {
		t.Fatalf("a save on a stale digest = %d %s, want 409", rec.Code, rec.Body.String())
	}
	assertFiles(t, workdir, map[string]string{"demo/main.bot": other}, "after the stale-digest save")
}

// savePathDigest is a single-file save presenting digest, decoded.
func savePathDigest(t *testing.T, s *Server, path string, document json.RawMessage, digest string) (*httptest.ResponseRecorder, saveFileResponse) {
	t.Helper()
	b, _ := json.Marshal(saveFileRequest{Path: path, Document: document, Digest: digest})
	rec := httptest.NewRecorder()
	s.handleSaveFile(rec, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(b)))
	var out saveFileResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode save response: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, out
}

// TestAnOpenThatDidNotReadWholeIssuesNoDigest: the digest exists only where
// a whole read does. A unit its budget cut short answers no digest for the
// files at and past its cut; a file the reader refuses whole — not UTF-8 —
// opens as its refusal, its document marked, no digest; and a save of the
// unread file is refused with its bytes kept, whatever digest it invents,
// even after the refusal's remedy (the budget) is applied.
func TestAnOpenThatDidNotReadWholeIssuesNoDigest(t *testing.T) {
	fragment := "prompt s:\n  x\n"
	big := fragment + "prompt big:\n" + strings.Repeat("  a long prompt line the budget eats\n", 200)
	budget := spentAlone(t, "x.bot", big) + 5
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))

	main := "import \"lib/x.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{
		"bots/b/main.bot":  main,
		"bots/b/lib/x.bot": big,
	})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "bots/b/main.bot")
	if opened.Unit == nil || opened.Bindable {
		t.Fatalf("the unit did not open as a short unit: %+v", opened)
	}
	// The unit answers the main's digest — read whole before the cut — and
	// none for the fragment the budget refused.
	digests := map[string]string{}
	for _, f := range opened.Unit.Files {
		digests[f.Rel] = f.Digest
	}
	if digests["main.bot"] == "" {
		t.Fatal("the main, read whole before the cut, answers no digest")
	}
	if digests["lib/x.bot"] != "" {
		t.Fatalf("the fragment the budget refused answers a digest (%s) — no whole read issued one", digests["lib/x.bot"])
	}

	// The same rule on one file: a text the reader refuses whole opens as
	// its refusal — the document marked, no digest — and the save of the
	// empty document it held is refused, its bytes kept, whatever digest a
	// client invents, even once the remedy (here: re-encoding the file) is
	// applied.
	refused := "# R\xe9sum\xe9 written in Latin-1\n\nworkflow w:\n  entry: worker\n"
	writeUnitFixture(t, workdir, map[string]string{"bots/lat/main.bot": refused})
	got := openPathAny(t, s, "bots/lat/main.bot")
	if got.ReadRefusal == nil || got.Digest != "" {
		t.Fatalf("a refused open = refusal %v, digest %q — want the refusal and no digest", got.ReadRefusal != nil, got.Digest)
	}
	if !strings.Contains(string(got.Document), "salvage") {
		t.Fatalf("the refused open's document carries no salvage mark: %s", got.Document)
	}
	if rec := saveDoc(t, s, "bots/lat/main.bot", got.Document, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("the marked document decodes = %d %s, want 400 (ast.ErrSalvage)", rec.Code, rec.Body.String())
	}
	kept, err := os.ReadFile(filepath.Join(workdir, "bots", "lat", "main.bot"))
	if err != nil || string(kept) != refused {
		t.Fatalf("the refused file's bytes moved: %v (%d bytes)", err, len(kept))
	}
	// The refusal's remedy applied (the file re-encoded), a save still
	// carries no digest — the open that seeded this document issued none —
	// and the re-encoded bytes stand.
	writeUnitFixture(t, workdir, map[string]string{"bots/lat/main.bot": "# Resume written in UTF-8\n\nworkflow w:\n  entry: worker\n"})
	if rec := saveDoc(t, s, "bots/lat/main.bot", got.Document, contentSHA256("# Resume written in UTF-8\n\nworkflow w:\n  entry: worker\n")); rec.Code != http.StatusBadRequest {
		t.Fatalf("the marked document decodes after the remedy = %d %s, want 400", rec.Code, rec.Body.String())
	}
	assertFiles(t, workdir, map[string]string{"bots/lat/main.bot": "# Resume written in UTF-8\n\nworkflow w:\n  entry: worker\n"}, "after the marked save")

	// Back to the short unit: the budget raised, a save of the fragment the
	// client's own open never read whole carries no digest — the open
	// issued none — and is refused with the fragment's bytes kept.
	t.Setenv(parser.ReadBudgetEnv, "")
	salvageBytes, err := os.ReadFile(filepath.Join(workdir, "bots", "b", "lib", "x.bot"))
	if err != nil {
		t.Fatal(err)
	}
	if rec := saveDoc(t, s, "bots/b/lib/x.bot", json.RawMessage(`{"prompts":[]}`), ""); rec.Code != http.StatusConflict {
		t.Fatalf("a save of the unread fragment without a digest = %d %s, want 409", rec.Code, rec.Body.String())
	}
	assertFiles(t, workdir, map[string]string{"bots/b/lib/x.bot": big}, "after the digest-less fragment save")
	// The boundary the rule draws on purpose: a RAW edit that presents the
	// stored bytes' digest replaces knowingly — the same power the files
	// drawer has always had — and says so by the digest it alone could
	// have gotten from a whole read of the bytes.
	if rec := saveDoc(t, s, "bots/b/lib/x.bot", json.RawMessage(`{}`), contentSHA256(string(salvageBytes))); rec.Code != http.StatusOK {
		t.Fatalf("a raw edit presenting the stored digest = %d %s, want 200", rec.Code, rec.Body.String())
	}
	assertFiles(t, workdir, map[string]string{"bots/b/lib/x.bot": ""}, "after the raw replacement")
}

// openPathAny is what either open answer carries (a bot in one file, a bot
// in several files), as far as these witnesses read it.
type openAny struct {
	Source      string           `json:"source"`
	Document    json.RawMessage  `json:"document"`
	Path        string           `json:"path"`
	Bindable    bool             `json:"bindable"`
	ReadRefusal *readRefusalInfo `json:"read_refusal,omitempty"`
	Digest      string           `json:"digest,omitempty"`
	Unit        *unitInfo        `json:"unit"`
}

func openPathAny(t *testing.T, s *Server, path string) openAny {
	t.Helper()
	body, _ := json.Marshal(openFileRequest{Path: path})
	req := httptest.NewRequest(http.MethodPost, "/api/files/open", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleOpenFile(rec, req)
	var out openAny
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode open response: %v\n%s", err, rec.Body.String())
	}
	return out
}

// --- the cloud twin ---

// getBundle reads a stored bundle as a client does: the files, the bot's id
// and version, each file's digest.
func getBundle(t *testing.T, s *Server, ctx context.Context, slug string) (map[string]string, string, int, map[string]string) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/teams/t1/bot-sources/"+slug, nil).WithContext(ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	s.handleGetBotSource(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET bot source = %d %s", w.Code, w.Body.String())
	}
	var v botSourceView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.Files, v.ID, v.Version, v.FileSHA256
}

// putBundleWith is a whole-bundle write carrying the digest rule's claim.
func putBundleWith(t *testing.T, s *Server, ctx context.Context, slug, id string, version int, files map[string]string, digests map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(botSourcePutReq{Files: files, Version: version, ID: id, StoredSHA256: digests})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+slug, bytes.NewReader(b)).WithContext(ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	return w
}

// putFileWith is a per-file write carrying the digest rule's claim.
func putFileWith(t *testing.T, s *Server, ctx context.Context, slug, path, content, id string, version int, digest string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"content": content}
	if version != 0 {
		body["version"] = version
		body["id"] = id
	}
	if digest != "" {
		body["stored_sha256"] = digest
	}
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+slug+"/files/"+path, bytes.NewReader(b)).WithContext(ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", slug)
	r.SetPathValue("path", path)
	w := httptest.NewRecorder()
	s.handlePutBotSourceFile(w, r)
	return w
}

// TestACloudWriteCarriesTheDigestItsReadIssued: the bundle fetch an editor
// opens with issues the bot's id, the version and each file's digest; the
// write presents them; a write without the digests, or with digests of
// bytes that are no longer stored, is a 409 naming what changed; a bot
// deleted and re-created takes no old client's write (#2678's second
// sequence).
func TestACloudWriteCarriesTheDigestItsReadIssued(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	ctx := auth.WithIdentity(context.Background(), editor)
	main := testBotMain
	fragment := "prompt p:\n  Fragment.\n"
	files := map[string]string{botsource.MainBotFile: main, "lib/f.bot": fragment}
	if rec := putBundle(s, ctx, "m", files, 0); rec.Code != http.StatusOK {
		t.Fatalf("the creation = %d %s, want 200", rec.Code, rec.Body.String())
	}
	_, id, version, digests := getBundle(t, s, ctx, "m")
	if id == "" || version == 0 || digests[botsource.MainBotFile] != contentSHA256(main) {
		t.Fatalf("the read issued id %q version %d digests %v", id, version, digests)
	}
	edited := main + "\nprompt edited:\n  By the editor.\n"

	// Per-file write without the digest: refused; with it: lands.
	if rec := putFileWith(t, s, ctx, "m", botsource.MainBotFile, edited, id, version, ""); rec.Code != http.StatusConflict {
		t.Fatalf("a per-file write without a digest = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if rec := putFileWith(t, s, ctx, "m", botsource.MainBotFile, edited, id, version, digests[botsource.MainBotFile]); rec.Code != http.StatusOK {
		t.Fatalf("a per-file write with the digest = %d %s, want 200", rec.Code, rec.Body.String())
	}

	// The write's answer issues the new state; a bundle push decided on the
	// OLD read (its digests now stale for main.bot) is a 409, and the
	// edited file stands.
	_, id, version, digests = getBundle(t, s, ctx, "m")
	whole := map[string]string{botsource.MainBotFile: main, "lib/f.bot": fragment}
	staleDigests := map[string]string{botsource.MainBotFile: contentSHA256(main)}
	if rec := putBundleWith(t, s, ctx, "m", id, version, whole, staleDigests); rec.Code != http.StatusConflict {
		t.Fatalf("a bundle push on stale digests = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if got := storedMain(t, s, "m"); got != edited {
		t.Fatalf("the edited main moved: %d bytes", len(got))
	}

	// The whole bundle with every digest: lands, and drops nothing.
	whole["lib/extra.bot"] = "prompt e:\n  Extra.\n"
	fresh := map[string]string{}
	for path, digest := range digests {
		fresh[path] = digest
	}
	if rec := putBundleWith(t, s, ctx, "m", id, version, whole, fresh); rec.Code != http.StatusOK {
		t.Fatalf("a bundle push with every digest = %d %s, want 200", rec.Code, rec.Body.String())
	}

	// Omitting a stored file DELETES it: the omission carries the digest
	// (the studio's opened read held every file's), and without one it is
	// refused.
	// The omission of lib/f.bot carries ITS digest: the opened read held
	// every file's, and a knowing removal says so.
	omit := map[string]string{botsource.MainBotFile: edited, "lib/extra.bot": "prompt e:\n  Extra.\n"}
	_, _, _, digests = getBundle(t, s, ctx, "m")
	if rec := putBundleWith(t, s, ctx, "m", id, version+1, omit, digests); rec.Code != http.StatusOK {
		t.Fatalf("an omission carrying the digests = %d %s, want 200", rec.Code, rec.Body.String())
	}
	_, id, version, digests = getBundle(t, s, ctx, "m")
	// The files' presence reads off the digests: getBundle answers both
	// from the same row.
	if _, held := digests["lib/f.bot"]; held {
		t.Fatal("the omitted file survived a push that omitted it with its digest")
	}
	if _, held := digests["lib/extra.bot"]; !held {
		t.Fatal("extra.bot did not survive a push that kept it")
	}
	{
		// The editor's omission: no digest for the file it drops — its open
		// was never issued one for a bot that does not hold it — and the
		// push is refused, the file standing.
		b2, _ := json.Marshal(botSourcePutReq{Files: map[string]string{botsource.MainBotFile: edited, "lib/f.bot": fragment}, Version: version, ID: id})
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/m", bytes.NewReader(b2)).WithContext(ctx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", "m")
		w := httptest.NewRecorder()
		s.handlePutBotSource(w, r)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "digest") {
			t.Fatalf("an omission without a digest = %d %s, want 409", w.Code, w.Body.String())
		}
	}

	// A bot deleted and re-created (a new id, version 1 again) takes no old
	// client's write: the id names the bot, and the refusal says what
	// happened.
	if err := s.botSources.Delete(store.WithTenant(ctx, "t1"), storedID(t, s, ctx, "m"), 0); err != nil {
		t.Fatal(err)
	}
	if rec := putBundle(s, ctx, "m", map[string]string{botsource.MainBotFile: "workflow other:\n  entry: done\n"}, 0); rec.Code != http.StatusOK {
		t.Fatalf("the re-creation = %d %s, want 200", rec.Code, rec.Body.String())
	}
	_, _, version2, _ := getBundle(t, s, ctx, "m")
	if rec := putBundleWith(t, s, ctx, "m", id, version2, map[string]string{botsource.MainBotFile: edited}, staleDigests); rec.Code != http.StatusConflict {
		t.Fatalf("a write pinned to the deleted bot = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if got := storedMain(t, s, "m"); got != "workflow other:\n  entry: done\n" {
		t.Fatalf("the re-created bot was overwritten: %d bytes", len(got))
	}
	// The id is the guard on its own when the bytes coincide: a bot
	// re-created with THE SAME files and digests is still not the bot the
	// old read answered, and the write pinned to the old id is refused.
	if err := s.botSources.Delete(store.WithTenant(ctx, "t1"), storedID(t, s, ctx, "m"), 0); err != nil {
		t.Fatal(err)
	}
	if rec := putBundle(s, ctx, "m", map[string]string{botsource.MainBotFile: edited}, 0); rec.Code != http.StatusOK {
		t.Fatalf("the identical re-creation = %d %s, want 200", rec.Code, rec.Body.String())
	}
	_, _, version2, digests = getBundle(t, s, ctx, "m")
	if rec := putBundleWith(t, s, ctx, "m", id, version2, map[string]string{botsource.MainBotFile: edited}, digests); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "re-created") {
		t.Fatalf("a write pinned to the old id over an identical bundle = %d %s, want 409 naming the re-creation", rec.Code, rec.Body.String())
	}
	if got := storedMain(t, s, "m"); got != edited {
		t.Fatalf("the re-created bot was overwritten by the old id's write: %d bytes", len(got))
	}
}

// storedID is a stored bundle's id, read from the store.
func storedID(t *testing.T, s *Server, ctx context.Context, slug string) string {
	t.Helper()
	bs, err := s.botSources.GetBySlug(store.WithTenant(ctx, "t1"), "t1", slug)
	if err != nil {
		t.Fatal(err)
	}
	return bs.ID
}

// --- the run file editor ---

// TestARunFileEditorRefusesAnInstalledPackage: a run that executed in place
// works in the operator's workspace, .botz included — and a manifest edited
// there is undone whole by the next update. The editor refuses the write
// (403 naming the overlay toggle), the package's manifest keeps its bytes,
// and a forced update finds no disable to undo.
func TestARunFileEditorRefusesAnInstalledPackage(t *testing.T) {
	srv, hs := newTestServer(t)
	work := srv.cfg.WorkDir
	src := t.TempDir()
	writeFiles(t, src, map[string]string{
		"pkg-bot/main.bot":      metaBakedBot,
		"pkg-bot/manifest.yaml": "name: pkg-bot\nversion: 1.0.0\ndescription: a package\n",
	})
	body := fmt.Sprintf(`{"url":%q,"force":false}`, filepath.Join(src, "pkg-bot"))
	w := httptest.NewRecorder()
	srv.handleBotInstall(w, httptest.NewRequest(http.MethodPost, "/api/v1/bots/install", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("install = %d %s", w.Code, w.Body.String())
	}
	before, err := os.ReadFile(filepath.Join(work, ".botz", "pkg-bot", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// A run that executed in place: its files are the workspace's.
	seedRunWithWorkDir(t, srv, "edit-run", work, false)
	disabled := string(before) + "enabled: false\n"
	b, _ := json.Marshal(saveRunFileRequest{Path: ".botz/pkg-bot/manifest.yaml", Content: disabled})
	req, err := http.NewRequest(http.MethodPut, hs.URL+"/api/runs/edit-run/files/content", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("the run file editor's write into .botz = %d, want 403", res.StatusCode)
	}
	kept, err := os.ReadFile(filepath.Join(work, ".botz", "pkg-bot", "manifest.yaml"))
	if err != nil || string(kept) != string(before) {
		t.Fatalf("the installed manifest moved: %v", err)
	}
}

// --- the scaffold's leftovers ---

// TestAScaffoldLeftoverIsNoBot: a create killed between its staging
// directory and its publish leaves bots/.<slug>.scaffold-*/ — discovery
// lists no bot from it, claims no name for it, and the operator's own
// bots/<slug> routes; a start sweeps the stale ones away.
func TestAScaffoldLeftoverIsNoBot(t *testing.T) {
	workdir := t.TempDir()
	bots := filepath.Join(workdir, "bots")
	left := filepath.Join(bots, ".ghost.scaffold-424242")
	if err := os.MkdirAll(left, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := "workflow w:\n  entry: done\n"
	writeFiles(t, left, map[string]string{
		"main.bot":      stale,
		"manifest.yaml": "name: ghost\nversion: 1.0.0\ndescription: a leftover\n",
		// The marker Scaffold writes into its staging: the sweep's provenance
		// (botscaffold.StagingMarkerName). A staging-shaped directory without
		// one is never swept — it may be the operator's own.
		botscaffold.StagingMarkerName: "iterion scaffold staging",
	})
	writeFiles(t, bots, map[string]string{
		"ghost/main.bot":      stale,
		"ghost/manifest.yaml": "name: ghost\nversion: 2.0.0\ndescription: the operator's own\n",
	})
	entries, _, err := botregistry.ListWithSchemaDiagnostics(botregistry.ListOptions{Paths: botregistry.DefaultPaths(workdir), Workdir: workdir})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range entries {
		if e.Name == "ghost" {
			found++
			if !strings.Contains(e.Path, filepath.Join("bots", "ghost")) {
				t.Fatalf("the name routes to %s, want the operator's own bundle", e.Path)
			}
		}
	}
	if found != 1 {
		t.Fatalf("the catalog holds %d ghost entries, want exactly the operator's own", found)
	}
	if removed := botscaffold.SweepStale(bots, 0); len(removed) != 1 || !strings.Contains(removed[0], ".ghost.scaffold-") {
		t.Fatalf("the sweep removed %v, want the leftover", removed)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatalf("the leftover survived the sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bots, "ghost", "main.bot")); err != nil {
		t.Fatalf("the sweep took the operator's bundle: %v", err)
	}
}

// putRunFileUnused keeps the linter honest about imports the witnesses use
// through helpers shared with the suite (parser's budget, store's tenant).
var (
	_ = parser.ReadBudgetEnv
	_ = store.WithTenant
)
