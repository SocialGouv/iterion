package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

// waitInLock waits until a goroutine is inside fn AND blocked acquiring the
// authoring locks: the decision fn took before the lock is behind it. The
// wait is on that stack condition, never on a tick.
func waitInLock(t *testing.T, fn string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	buf := make([]byte, 1<<22)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, fn) && strings.Contains(g, "acquireAuthoringLocalLocks") {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never reached the authoring lock", fn)
}

// writeFiles writes files (slash paths under root) as another writer would.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, text := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// assertFiles asserts the BYTES of each file under root.
func assertFiles(t *testing.T, root string, want map[string]string, what string) {
	t.Helper()
	for rel, text := range want {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %s: %v", what, rel, err)
		}
		if string(got) != text {
			t.Fatalf("%s: %s is %d bytes, want the %d bytes the other writer wrote:\n%s", what, rel, len(got), len(text), got)
		}
	}
}

// A single-file save decides its route off the file BEFORE the lock. A
// writer holding the lock meanwhile replaces the file — with one the reader
// refuses whole, or with the main of a bot in several files — and the save,
// which then reaches the lock, must not write over it: the bytes it
// replaces under the lock are held to the ones its route was decided on
// (writeEditorFile, checkStoredFile).
func TestASaveRacingAWriterThatHeldTheLockIsAConflict(t *testing.T) {
	single := unitFixtureNodes + "\nworkflow w:\n  entry: worker\n  worker -> done\n"
	for _, c := range []struct {
		name  string
		other map[string]string
	}{
		{"the file grows past the read budget", map[string]string{
			"demo/main.bot": single + "\nprompt big:\n" + strings.Repeat("  x\n", 400),
		}},
		{"the file becomes the main of a bot in several files", map[string]string{
			"demo/main.bot":      "import \"lib/extra.bot\"\n\n" + strings.Replace(single, "worker -> done", "worker -> helper\n  helper -> done", 1),
			"demo/lib/extra.bot": "agent helper:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			budget := spentAlone(t, "main.bot", single+"\nprompt big:\n"+strings.Repeat("  x\n", 400)) - 50
			t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
			workdir := t.TempDir()
			writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": single})
			s := &Server{cfg: Config{WorkDir: workdir}}
			rec, opened := openPath(t, s, "demo/main.bot")
			if rec.Code != http.StatusOK || !opened.Bindable || opened.Unit != nil {
				t.Fatalf("main.bot does not open as one bindable file: %d %+v", rec.Code, opened)
			}
			doc := editDocument(t, opened.Document, func(m map[string]any) {
				prompts, _ := m["prompts"].([]any)
				m["prompts"] = append(prompts, map[string]any{"name": "note", "body": "An edit made in the canvas."})
			})
			abs := filepath.Join(workdir, "demo", "main.bot")

			// The other writer holds the lock; the save reads the file
			// unlocked, decides "one file", and waits for the lock; the
			// other writer writes its files and releases.
			locks, err := acquireAuthoringLocalLocks(context.Background(), []string{abs})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				// The digest the save presents is the open's: the decision
				// was taken on those bytes, and the other writer's replace
				// under the lock is what the check must catch.
				body, _ := json.Marshal(saveFileRequest{Path: "demo/main.bot", Document: doc, Digest: opened.Digest})
				rr := httptest.NewRecorder()
				s.handleSaveFile(rr, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body)))
				done <- rr
			}()
			waitInLock(t, "handleSaveFile")
			writeFiles(t, workdir, c.other)
			closeAuthoringLocalLocks(locks)
			var rr *httptest.ResponseRecorder
			select {
			case rr = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the save never returned")
			}
			if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "changed since") {
				t.Errorf("the save over a file replaced while it waited for the lock = %d %s, want 409 naming the change", rr.Code, rr.Body.String())
			}
			assertFiles(t, workdir, c.other, "after the racing save")
		})
	}
}

// A unit save writes each file it rewrites over the bytes the unit was read
// at: a writer that held a fragment's lock and replaced it makes the save a
// conflict, never a write over that fragment.
func TestAUnitSaveRacingAWriterThatHeldTheLockIsAConflict(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	if opened.Unit == nil {
		t.Fatal("the bot does not open as its unit")
	}
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		agentsOf(m)[0].(map[string]any)["model"] = "anthropic/claude-opus-5"
	})
	other := map[string]string{"demo/lib/nodes.bot": unitFixtureNodes + "\nprompt theirs:\n  Written by the other writer.\n"}
	fragment := filepath.Join(workdir, "demo", "lib", "nodes.bot")
	locks, err := acquireAuthoringLocalLocks(context.Background(), []string{fragment})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr, _ := savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision)
		done <- rr
	}()
	waitInLock(t, "saveUnit")
	writeFiles(t, workdir, other)
	closeAuthoringLocalLocks(locks)
	var rr *httptest.ResponseRecorder
	select {
	case rr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the save never returned")
	}
	if rr.Code != http.StatusConflict {
		t.Errorf("the unit save over a fragment replaced while it waited = %d %s, want 409", rr.Code, rr.Body.String())
	}
	assertFiles(t, workdir, other, "after the racing unit save")
}

// putFile is a cloud per-file write: the studio's single-file save (no
// digest, the bot's id with its version) or the files drawer (the stored
// text's digest). The id is read from the store, the way a client carries
// the one its open answered; a test that means to present a stale or
// missing id marshals its own request.
func putFile(s *Server, ctx context.Context, slug, path, content string, version int, digest string) *httptest.ResponseRecorder {
	body := map[string]any{"content": content}
	if version != 0 {
		body["version"] = version
		body["id"] = storedIDFor(s, ctx, slug)
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

// putBundle is a cloud whole-bundle write: the studio's unit save and every
// push of a whole bundle. The id rides along the way a client carries the
// one its open answered; a test that means to present none, or another
// bot's, marshals its own request (putBundleWith).
func putBundle(s *Server, ctx context.Context, slug string, files map[string]string, version int) *httptest.ResponseRecorder {
	req := botSourcePutReq{Files: files, Version: version}
	if version != 0 {
		// An update presents the id and the digests its read answered; a
		// creation decides on the bot's absence and reading it would
		// pre-empt a creation race the test means to exercise.
		req.ID = storedIDFor(s, ctx, slug)
		req.StoredSHA256 = storedDigestsFor(s, ctx, slug)
	}
	b, _ := json.Marshal(req)
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+slug, bytes.NewReader(b)).WithContext(ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	return w
}

// storedDigests is every stored file's digest at the CURRENT version —
// what a whole-bundle push presents for the files it replaces (the digest
// rule); a bot that is not there digests nothing.
func storedDigests(t *testing.T, s *Server, ctx context.Context, slug string) map[string]string {
	t.Helper()
	out := storedDigestsFor(s, ctx, slug)
	if out == nil {
		t.Fatalf("bot source %q is not stored", slug)
	}
	return out
}

// versionIfStored is the stored bundle's version, 0 when the bot is not
// stored — what a helper passes so its first push is a creation and its
// second an update.
func versionIfStored(s *Server) int {
	bs, err := s.botSources.GetBySlug(store.WithTenant(context.Background(), "t1"), "t1", "whole")
	if err != nil {
		return 0
	}
	return bs.Version
}

// storedIDFor and storedDigestsFor are storedID and storedDigests for the
// helpers that take no *testing.T: a bot that is not there has no id and
// digests nothing.
func storedIDFor(s *Server, ctx context.Context, slug string) string {
	bs, err := s.botSources.GetBySlug(store.WithTenant(ctx, "t1"), "t1", slug)
	if err != nil {
		return ""
	}
	return bs.ID
}

func storedDigestsFor(s *Server, ctx context.Context, slug string) map[string]string {
	bs, err := s.botSources.GetBySlug(store.WithTenant(ctx, "t1"), "t1", slug)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(bs.Files))
	for path, content := range bs.Files {
		out[path] = contentSHA256(content)
	}
	return out
}

func storedMain(t *testing.T, s *Server, slug string) string {
	t.Helper()
	bs, err := s.botSources.GetBySlug(store.WithTenant(context.Background(), "t1"), "t1", slug)
	if err != nil {
		t.Fatal(err)
	}
	return bs.Files[botsource.MainBotFile]
}

// A cloud bot whose stored main.bot the server no longer reads whole — the
// budget lowered since it was stored, or a file stored before the default
// budget existed — is written over only by a raw edit that presents the
// stored text's digest (the files drawer). The studio's document save of it
// and a push of the whole bundle are refused, nothing written: their text
// was not made from the stored one.
func TestACloudWriteOverAStoredFileTheReaderRefusesWritesNothing(t *testing.T) {
	stored := testBotMain + "\nprompt kept:\n" + strings.Repeat("  a line the author wrote and the studio never showed\n", 300)
	incoming := testBotMain
	setup := func(t *testing.T) (*Server, context.Context) {
		s, editor, _ := newBotSourceTestServer(t)
		ctx := auth.WithIdentity(context.Background(), editor)
		t.Setenv(parser.ReadBudgetEnv, "")
		if w := putBundle(s, ctx, "big", map[string]string{botsource.MainBotFile: stored}, 0); w.Code != http.StatusOK {
			t.Fatalf("storing the bot = %d: %s", w.Code, w.Body.String())
		}
		budget := spentAlone(t, "main.bot", incoming) + 20
		t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
		if _, err := parser.Parse("main.bot", stored); err == nil {
			t.Fatal("precondition: the stored main must be refused whole now")
		}
		return s, ctx
	}
	t.Run("the document save", func(t *testing.T) {
		s, ctx := setup(t)
		w := putFile(s, ctx, "big", botsource.MainBotFile, incoming, 1, "")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "digest") {
			t.Errorf("the per-file save over a refused stored main without its digest = %d %s, want 409 naming the digest no whole read issued", w.Code, w.Body.String())
		}
		if got := storedMain(t, s, "big"); got != stored {
			t.Fatalf("LOST: the stored main is %d bytes, was %d", len(got), len(stored))
		}
	})
	t.Run("the whole-bundle push", func(t *testing.T) {
		s, ctx := setup(t)
		// The editor's push presents every digest BUT the refused file's —
		// its open issued none — and is refused, nothing written.
		digests := storedDigestsFor(s, ctx, "big")
		delete(digests, botsource.MainBotFile)
		b2, _ := json.Marshal(botSourcePutReq{Files: map[string]string{botsource.MainBotFile: incoming}, Version: 1, ID: storedIDFor(s, ctx, "big"), StoredSHA256: digests})
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/big", bytes.NewReader(b2)).WithContext(ctx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", "big")
		w := httptest.NewRecorder()
		s.handlePutBotSource(w, r)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "digest") {
			t.Errorf("the bundle push over a refused stored main without its digest = %d %s, want 409 naming the digest", w.Code, w.Body.String())
		}
		if got := storedMain(t, s, "big"); got != stored {
			t.Fatalf("LOST: the stored main is %d bytes, was %d", len(got), len(stored))
		}
	})
	t.Run("the files drawer presents the stored digest", func(t *testing.T) {
		s, ctx := setup(t)
		r := httptest.NewRequest("GET", "/api/teams/t1/bot-sources/big", nil).WithContext(ctx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", "big")
		gw := httptest.NewRecorder()
		s.handleGetBotSource(gw, r)
		var view botSourceView
		if gw.Code != http.StatusOK || json.Unmarshal(gw.Body.Bytes(), &view) != nil {
			t.Fatalf("read = %d %s", gw.Code, gw.Body.String())
		}
		digest := view.FileSHA256[botsource.MainBotFile]
		if digest != contentSHA256(stored) {
			t.Fatalf("the read answers digest %q for main.bot, want the stored text's", digest)
		}
		// A digest of other bytes is not the stored text's.
		if w := putFile(s, ctx, "big", botsource.MainBotFile, incoming, view.Version, contentSHA256(stored+"x")); w.Code != http.StatusConflict {
			t.Errorf("a raw edit presenting another text's digest = %d %s, want 409", w.Code, w.Body.String())
		}
		if got := storedMain(t, s, "big"); got != stored {
			t.Fatalf("LOST: the stored main is %d bytes, was %d", len(got), len(stored))
		}
		if w := putFile(s, ctx, "big", botsource.MainBotFile, incoming, view.Version, digest); w.Code != http.StatusOK {
			t.Fatalf("the raw edit presenting the stored digest = %d %s, want 200", w.Code, w.Body.String())
		}
		if got := storedMain(t, s, "big"); got != incoming {
			t.Fatalf("the raw edit did not land: %q", got)
		}
	})
}

// racingBotSources lets another writer write right after the FIRST read a route
// makes: the window between the checks a route asks of the bundle it read
// and its write.
type racingBotSources struct {
	botsource.Store
	once sync.Once
	race func()
}

func (r *racingBotSources) GetBySlug(ctx context.Context, tenantID, slug string) (botsource.BotSource, error) {
	bs, err := r.Store.GetBySlug(ctx, tenantID, slug)
	r.once.Do(r.race)
	return bs, err
}

// A cloud write presenting no version is checked against the bundle as the
// route reads it, and lands only on that version: another writer's version
// written meanwhile — here a main past the server's read budget, which the
// checks never saw — makes it a conflict, never a write over that version.
func TestACloudWriteLandsOnlyOnTheVersionItChecked(t *testing.T) {
	for _, route := range []string{"per-file", "whole-bundle"} {
		t.Run(route, func(t *testing.T) {
			s, editor, _ := newBotSourceTestServer(t)
			ctx := auth.WithIdentity(context.Background(), editor)
			t.Setenv(parser.ReadBudgetEnv, "")
			if w := putBundle(s, ctx, "raced", map[string]string{botsource.MainBotFile: testBotMain}, 0); w.Code != http.StatusOK {
				t.Fatalf("storing the bot = %d: %s", w.Code, w.Body.String())
			}
			theirs := testBotMain + "\nprompt theirs:\n" + strings.Repeat("  written by the other writer\n", 300)
			inner := s.botSources
			s.botSources = &racingBotSources{Store: inner, race: func() {
				tctx := store.WithTenant(context.Background(), "t1")
				bs, err := inner.GetBySlug(tctx, "t1", "raced")
				if err != nil {
					t.Error(err)
					return
				}
				bs.Files = map[string]string{botsource.MainBotFile: theirs}
				if _, err := inner.Update(tctx, bs); err != nil {
					t.Error(err)
				}
			}}
			mine := strings.Replace(testBotMain, "printf ok", "printf mine", 1)
			var w *httptest.ResponseRecorder
			if route == "per-file" {
				w = putFile(s, ctx, "raced", botsource.MainBotFile, mine, 0, "")
			} else {
				w = putBundle(s, ctx, "raced", map[string]string{botsource.MainBotFile: mine}, 0)
			}
			if w.Code != http.StatusConflict {
				t.Errorf("a write racing another version = %d %s, want 409", w.Code, w.Body.String())
			}
			s.botSources = inner
			if got := storedMain(t, s, "raced"); got != theirs {
				t.Fatalf("LOST: the other writer's version (%d bytes) was written over: %q", len(theirs), got)
			}
		})
	}
}

// A creation is decided on the bot's absence: another writer creating the
// same slug between the route's read and its write makes it a conflict,
// never an update over the bundle that writer stored.
func TestACloudCreationRacingAnotherIsAConflict(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	ctx := auth.WithIdentity(context.Background(), editor)
	theirs := testBotMain + "\nprompt theirs:\n  written by the other writer\n"
	inner := s.botSources
	s.botSources = &racingBotSources{Store: inner, race: func() {
		if _, err := inner.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{TenantID: "t1", Slug: "fresh", Files: map[string]string{botsource.MainBotFile: theirs}}); err != nil {
			t.Error(err)
		}
	}}
	w := putBundle(s, ctx, "fresh", map[string]string{botsource.MainBotFile: testBotMain}, 0)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "created since this write was decided on its absence") {
		t.Errorf("a creation racing another = %d %s, want 409 naming the race", w.Code, w.Body.String())
	}
	s.botSources = inner
	if got := storedMain(t, s, "fresh"); got != theirs {
		t.Fatalf("LOST: the other writer's creation was written over: %q", got)
	}
}

// A write whose file cannot be read under the lock — here no regular file
// any more, a directory where the file was — is no replacement the check
// can judge: the save fails and nothing is written.
func TestAWriteOverAFileItCannotReadWritesNothing(t *testing.T) {
	workdir := t.TempDir()
	single := unitFixtureNodes + "\nworkflow w:\n  entry: worker\n  worker -> done\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": single})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	abs := filepath.Join(workdir, "demo", "main.bot")
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(abs, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	{
		body, _ := json.Marshal(saveFileRequest{Path: "demo/main.bot", Document: opened.Document, Digest: opened.Digest})
		s.handleSaveFile(rec, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body)))
	}
	if rec.Code < 400 {
		t.Fatalf("a save over a path that is no regular file = %d %s, want a refusal", rec.Code, rec.Body.String())
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		t.Fatalf("the refused save changed the path: %v %v", info, err)
	}
	locks, err := acquireAuthoringLocalLocks(context.Background(), []string{abs})
	if err != nil {
		t.Logf("the lock itself refuses a directory: %v", err)
		return
	}
	defer closeAuthoringLocalLocks(locks)
	if err := locks[0].writeEditorFile(storedCheck{name: "demo/main.bot", decided: storedAt([]byte(single))}, []byte("x"), false, nil, nil); err == nil {
		t.Fatal("writeEditorFile wrote over a path it cannot read")
	}
}

// The assistant's file changes are exact replacements of the bytes its
// snapshot read, which the change presents (expected_sha256): a raw edit,
// which lands over a .bot the reader refuses as stored — the rest of the
// file is the stored text, untouched.
func TestTheAssistantsExactEditsOfAFileTheReaderRefusesLand(t *testing.T) {
	s, editorPath := authoringFixture(t)
	bundleDir := filepath.Join(s.cfg.WorkDir, "project-bot")
	if err := os.WriteFile(filepath.Join(bundleDir, bundle.ManifestFile), []byte("schema_version: 1\nname: project-bot\nauthoring:\n  editable_files:\n    - {scope: bundle, path: main.bot}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep := "workflow main:\n  entry: done\n"
	big := "\nprompt big:\n" + strings.Repeat("  a long prompt line\n", 300)
	if err := os.WriteFile(filepath.Join(bundleDir, "main.bot"), []byte(keep+big), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(spentAlone(t, "main.bot", keep)+20))
	if _, err := parser.Parse("main.bot", keep+big); err == nil {
		t.Fatal("precondition: the stored main must be refused whole")
	}
	rec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	var snapshot authoringSnapshotResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &snapshot) != nil {
		t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
	}
	var digest string
	for _, f := range snapshot.Files {
		if f.Path == "main.bot" {
			digest = f.SHA256
		}
	}
	if digest != contentSHA256(keep+big) {
		t.Fatalf("the snapshot's digest of main.bot is %q, want the stored text's", digest)
	}
	change := authoringFileChange{Scope: bundle.AuthoringScopeBundle, Path: "main.bot", ExpectedSHA256: digest, Replacements: []authoringReplacement{{Before: big, After: ""}}}
	if rec := authoringCall(t, s, "/commit", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}}); rec.Code != http.StatusOK {
		t.Fatalf("the exact edit = %d %s, want 200", rec.Code, rec.Body.String())
	}
	assertFiles(t, bundleDir, map[string]string{"main.bot": keep}, "after the exact edit")
}

// The run file editor writes the text it was shown: the read answers its
// digest, the save presents it, and lands only over those bytes. A .bot the
// reader refuses as stored is written over only by a save presenting it.
func TestARunFileEditReplacesOnlyTheTextItWasShown(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	stored := "workflow w:\n  entry: done\n\nprompt big:\n" + strings.Repeat("  a long prompt line\n", 300)
	writeFiles(t, dir, map[string]string{"x.bot": stored})
	seedRunWithWorkDir(t, srv, "edit-run", dir, true)
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(spentAlone(t, "x.bot", "workflow w:\n  entry: done\n")+20))

	resp, shown := getContent(t, hs.URL, "edit-run", "x.bot")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || shown.SHA256 != contentSHA256(stored) {
		t.Fatalf("read: %d sha %q, want the stored text's digest", resp.StatusCode, shown.SHA256)
	}
	put := func(content, digest string) int {
		t.Helper()
		body, _ := json.Marshal(saveRunFileRequest{Path: "x.bot", Content: content, StoredSHA256: digest})
		req, err := http.NewRequest(http.MethodPut, hs.URL+"/api/runs/edit-run/files/content", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	mine := "workflow w:\n  entry: done\n"
	if code := put(mine, ""); code != http.StatusConflict {
		t.Errorf("a save presenting no digest over a .bot the reader refuses = %d, want 409 (the digest rule)", code)
	}
	assertFiles(t, dir, map[string]string{"x.bot": stored}, "after the undigested save")
	// The run writes the file after the read: the shown digest names bytes
	// no longer there.
	theirs := stored + "  one more line the run wrote\n"
	writeFiles(t, dir, map[string]string{"x.bot": theirs})
	if code := put(mine, shown.SHA256); code != http.StatusConflict {
		t.Errorf("a save presenting the digest of a text the run replaced = %d, want 409", code)
	}
	assertFiles(t, dir, map[string]string{"x.bot": theirs}, "after the stale save")
	if code := put(mine, contentSHA256(theirs)); code != http.StatusOK {
		t.Fatalf("a save presenting the stored text's digest = %d, want 200", code)
	}
	assertFiles(t, dir, map[string]string{"x.bot": mine}, "after the save")
}

// The transaction every multi-file writer publishes through holds each file
// to checkStoredFile under its lock: a .bot the reader refuses as stored is
// replaced only by an edit presenting the stored digest.
func TestATransactionReplacesARefusedFileOnlyForARawEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.bot")
	stored := "workflow w:\n  entry: done\n\nprompt big:\n" + strings.Repeat("  a long prompt line\n", 300)
	writeFiles(t, dir, map[string]string{"main.bot": stored})
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(spentAlone(t, "main.bot", "workflow w:\n  entry: done\n")+20))
	locks, err := acquireAuthoringLocalLocks(context.Background(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAuthoringLocalLocks(locks)
	tx, err := prepareAuthoringLocal(locks[0], authoringPreviewFile{Path: "main.bot", Operation: "replace", Before: stored, After: "workflow w:\n  entry: done\n"}, nil, nil)
	if tx != nil {
		tx.close()
	}
	var refused *storedFileRefusal
	if !errors.As(err, &refused) || refused.status != http.StatusConflict {
		t.Fatalf("a transaction decided from no raw text over a refused .bot = %v, want the 409 the digest rule answers", err)
	}
	assertFiles(t, dir, map[string]string{"main.bot": stored}, "after the refused transaction")
	tx, err = prepareAuthoringLocal(locks[0], authoringPreviewFile{Path: "main.bot", Operation: "replace", Before: stored, After: "workflow w:\n  entry: done\n", presented: contentSHA256(stored)}, nil, nil)
	if err != nil {
		t.Fatalf("a raw edit presenting the stored digest = %v, want it prepared", err)
	}
	tx.close()
}

// A tab bound to a file that was UTF-8 when it opened, the file re-encoded
// on disk since (Latin-1): the save refuses, nothing written, and says the
// remedy (E006's hint); the reopen answers the file's bytes, no document of
// them, and the refusal with its remedy — the author sees what to fix and
// where, never an empty editor.
func TestAFileThatTurnedNonUTF8IsRefusedWithItsRemedy(t *testing.T) {
	single := strings.Replace(unitFixtureNodes+"\nworkflow w:\n  entry: worker\n  worker -> done\n", "Do the thing.", "Do the thing — café.", 1)
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": single})
	s := &Server{cfg: Config{WorkDir: workdir}}
	rec, opened := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK || !opened.Bindable {
		t.Fatalf("open: %d %+v", rec.Code, opened)
	}
	latin1 := strings.ReplaceAll(single, "é", "\xe9")
	writeFiles(t, workdir, map[string]string{"demo/main.bot": latin1})
	hint := parser.HintFor(parser.DiagNotUTF8)
	// The editor's save presents the digest its open issued: the file has
	// moved under it (another writer re-encoded the bytes), and the save is
	// a conflict, never a write over what it did not decide on.
	{
		body, _ := json.Marshal(saveFileRequest{Path: "demo/main.bot", Document: opened.Document, Digest: opened.Digest})
		srec := httptest.NewRecorder()
		s.handleSaveFile(srec, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body)))
		if srec.Code != http.StatusConflict || !strings.Contains(srec.Body.String(), "changed since") {
			t.Errorf("the save over the re-encoded file = %d %s, want 409 naming what changed", srec.Code, srec.Body.String())
		}
	}
	assertFiles(t, workdir, map[string]string{"demo/main.bot": latin1}, "after the refused save")
	orec, reopened := openPath(t, s, "demo/main.bot")
	if orec.Code != http.StatusOK {
		t.Fatalf("reopen: %d %s", orec.Code, orec.Body.String())
	}
	if reopened.Source == "" || reopened.Bindable || reopened.ReadRefusal == nil || reopened.ReadRefusal.Hint != hint || !strings.Contains(reopened.ReadRefusal.Message, "[E006]") {
		t.Errorf("the reopen = source %d bytes, bindable %v, read_refusal %+v — want the bytes, unbindable, the refusal and its remedy", len(reopened.Source), reopened.Bindable, reopened.ReadRefusal)
	}
	if string(reopened.Document) == "null" || len(reopened.Document) == 0 {
		t.Errorf("the reopen answers document %q, want the empty document", reopened.Document)
	}
}
