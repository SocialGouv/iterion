package server

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
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

// storedFiles is the bundle a slug holds now.
func storedFiles(t *testing.T, s *Server, slug string) botsource.BotSource {
	t.Helper()
	bs, err := s.botSources.GetBySlug(store.WithTenant(context.Background(), "t1"), "t1", slug)
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

// deleteFile is the per-file DELETE: a file removed by name.
func deleteFile(s *Server, ctx context.Context, slug, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("DELETE", "/api/teams/t1/bot-sources/"+slug+"/files/"+path, nil).WithContext(ctx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", slug)
	r.SetPathValue("path", path)
	w := httptest.NewRecorder()
	s.handleDeleteBotSourceFile(w, r)
	return w
}

// A cloud bot the read budget its files SHARE cuts short — each file read
// whole alone, the bot not — opens as a salvage; a write of what the
// salvage holds (lib/b.bot without the declarations nobody read) is refused
// at both doors, whichever client sends it, and the stored bytes stay. A
// raw edit presenting the stored text's digest still lands.
func TestACloudBotItsSharedBudgetCutsShortIsNotWrittenFromItsSalvage(t *testing.T) {
	main := "import \"lib/a.bot\"\nimport \"lib/b.bot\"\n" + testBotMain
	a := "prompt pa:\n  a\n"
	var bb strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&bb, "prompt pb%d:\n  x\n", i)
	}
	b := bb.String()
	manifest := "name: short\nversion: 1.0.0\nrequires:\n  iterion: \">= " + parser.ImportSince + "\"\n"
	files := map[string]string{botsource.MainBotFile: main, "lib/a.bot": a, "lib/b.bot": b, "manifest.yaml": manifest}
	budget := spentAlone(t, "lib/b.bot", b) + 5
	emptied := "## lib/b.bot as the salvage holds it\n"
	for _, door := range []string{"whole-bundle push", "per-file write"} {
		t.Run(door, func(t *testing.T) {
			s, editor, _ := newBotSourceTestServer(t)
			ctx := auth.WithIdentity(context.Background(), editor)
			t.Setenv(parser.ReadBudgetEnv, "")
			if w := putBundle(s, ctx, "short", files, 0); w.Code != http.StatusOK {
				t.Fatalf("storing = %d %s", w.Code, w.Body.String())
			}
			t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
			for name, src := range files {
				if strings.HasSuffix(name, ".bot") {
					if _, err := parser.Parse(name, src); err != nil {
						t.Fatalf("precondition: %s alone reads whole: %v", name, err)
					}
				}
			}
			// The digest rule holds both doors: an open of the bot issued
			// NO digest for the file the shared budget refused, so the
			// editor's write — the whole-bundle push presenting every
			// digest BUT that file's, the per-file write presenting none —
			// is a 409 naming what it may not replace.
			var w *httptest.ResponseRecorder
			if door == "whole-bundle push" {
				digests := storedDigests(t, s, ctx, "short")
				delete(digests, "lib/b.bot")
				b2, _ := json.Marshal(botSourcePutReq{Files: map[string]string{botsource.MainBotFile: main, "lib/a.bot": a, "lib/b.bot": emptied, "manifest.yaml": manifest}, Version: 1, ID: storedID(t, s, ctx, "short"), StoredSHA256: digests})
				r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/short", bytes.NewReader(b2)).WithContext(ctx)
				r.SetPathValue("id", "t1")
				r.SetPathValue("slug", "short")
				w = httptest.NewRecorder()
				s.handlePutBotSource(w, r)
			} else {
				w = putFile(s, ctx, "short", "lib/b.bot", emptied, 1, "")
			}
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "digest") {
				t.Fatalf("a write of the salvage's lib/b.bot = %d %s, want 409 naming the digest no whole read issued", w.Code, w.Body.String())
			}
			if got := storedFiles(t, s, "short").Files["lib/b.bot"]; got != b {
				t.Fatalf("lib/b.bot is %d bytes after the refused write, want the %d stored", len(got), len(b))
			}
			// A raw edit presenting the stored text's digest replaces
			// knowingly — the power the files drawer has always had.
			if w := putFile(s, ctx, "short", "lib/b.bot", emptied, 1, contentSHA256(b)); w.Code != http.StatusOK {
				t.Fatalf("a raw edit presenting the stored text's digest = %d %s, want 200", w.Code, w.Body.String())
			}
		})
	}
}

// A whole-bundle push replaces the stored bundle with its own: a stored
// workflow file it OMITS is replaced by nothing. One the reader refuses as
// stored is refused like its replacement — nothing written — while the
// same file removed by name (the per-file DELETE) goes, and the omission of
// a workflow file the reader reads goes too.
func TestAWholeBundlePushThatOmitsAFileTheReaderRefusesDropsNothing(t *testing.T) {
	big := strings.Replace(testBotMain, "workflow main:", "workflow companion:", 1) + "\nprompt kept:\n" + strings.Repeat("  a line the author wrote and the studio never showed\n", 300)
	notes := strings.Replace(testBotMain, "workflow main:", "workflow notes:", 1)
	readme := "notes\n"
	s, editor, _ := newBotSourceTestServer(t)
	ctx := auth.WithIdentity(context.Background(), editor)
	t.Setenv(parser.ReadBudgetEnv, "")
	if w := putBundle(s, ctx, "big", map[string]string{botsource.MainBotFile: testBotMain, "companion.bot": big, "notes.bot": notes, "README.md": readme}, 0); w.Code != http.StatusOK {
		t.Fatalf("storing = %d %s", w.Code, w.Body.String())
	}
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(spentAlone(t, "main.bot", testBotMain)+20))
	if _, err := parser.Parse("companion.bot", big); err == nil {
		t.Fatal("precondition: companion.bot is refused alone now")
	}
	// The omission presents every digest BUT the omitted file's: the
	// editor's read never held one for it, and an omission without the
	// digest deletes what no whole read decided on.
	omitting := storedDigests(t, s, ctx, "big")
	delete(omitting, "companion.bot")
	{
		b2, _ := json.Marshal(botSourcePutReq{Files: map[string]string{botsource.MainBotFile: testBotMain, "notes.bot": notes, "README.md": readme}, Version: 1, ID: storedID(t, s, ctx, "big"), StoredSHA256: omitting})
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/big", bytes.NewReader(b2)).WithContext(ctx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", "big")
		w := httptest.NewRecorder()
		s.handlePutBotSource(w, r)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "companion.bot") || !strings.Contains(w.Body.String(), "delete it by name") {
			t.Fatalf("a push omitting a stored file without its digest = %d %s, want 409 naming it and the deletion by name", w.Code, w.Body.String())
		}
	}
	if got := storedFiles(t, s, "big"); got.Files["companion.bot"] != big || got.Version != 1 {
		t.Fatalf("companion.bot is %d bytes at version %d after the refused push, want the %d stored at 1", len(got.Files["companion.bot"]), got.Version, len(big))
	}
	// The same omission carrying the omitted file's digest is a knowing
	// removal: the raw flow the files drawer has always had.
	{
		known := storedDigestsFor(s, ctx, "big")
		b2, _ := json.Marshal(botSourcePutReq{Files: map[string]string{botsource.MainBotFile: testBotMain, "notes.bot": notes, "README.md": readme}, Version: 1, ID: storedID(t, s, ctx, "big"), StoredSHA256: known})
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/big", bytes.NewReader(b2)).WithContext(ctx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", "big")
		w := httptest.NewRecorder()
		s.handlePutBotSource(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("an omission carrying the omitted file's digest = %d %s, want 200", w.Code, w.Body.String())
		}
		if _, held := storedFiles(t, s, "big").Files["companion.bot"]; held {
			t.Fatal("companion.bot survived a knowing removal")
		}
	}
	// Removed by name, the file goes too.
	if w := putBundle(s, ctx, "big", map[string]string{botsource.MainBotFile: testBotMain, "lib/c.bot": "prompt c:\n  c\n", "README.md": readme}, 2); w.Code != http.StatusOK {
		t.Fatalf("re-storing with a file the reader reads = %d %s, want 200", w.Code, w.Body.String())
	}
	if w := deleteFile(s, ctx, "big", "lib/c.bot"); w.Code != http.StatusOK {
		t.Fatalf("the DELETE of lib/c.bot by name = %d %s, want 200", w.Code, w.Body.String())
	}
	if _, held := storedFiles(t, s, "big").Files["lib/c.bot"]; held {
		t.Fatal("lib/c.bot is still stored after its DELETE by name")
	}
	// The omission of a workflow file the reader reads is the push's to make.
	if w := putBundle(s, ctx, "big", map[string]string{botsource.MainBotFile: testBotMain, "README.md": readme}, storedFiles(t, s, "big").Version); w.Code != http.StatusOK {
		t.Fatalf("a push omitting notes.bot = %d %s, want 200", w.Code, w.Body.String())
	}
	if _, held := storedFiles(t, s, "big").Files["notes.bot"]; held {
		t.Fatal("notes.bot is still stored after a push that omitted it")
	}
}

// The disk twin: a bot on disk the read budget its files SHARE cuts short —
// each file read whole alone, the bot not — opens as a salvage; a save of
// one of its fragments through the file door, written from what the salvage
// holds, is refused, and the stored bytes stay.
func TestADiskBotItsSharedBudgetCutsShortIsNotSavedFromItsSalvage(t *testing.T) {
	var bb strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&bb, "prompt pb%d:\n  x\n", i)
	}
	b := bb.String()
	work := t.TempDir()
	main := "import \"lib/a.bot\"\nimport \"lib/b.bot\"\n" + unitFixtureNodes + "\nworkflow w:\n  entry: worker\n  worker -> done\n"
	writeFiles(t, work, map[string]string{"bots/short/main.bot": main, "bots/short/lib/a.bot": "prompt pa:\n  a\n", "bots/short/lib/b.bot": b})
	s := newOrgTestServer(t)
	s.cfg.WorkDir = work
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(spentAlone(t, "lib/b.bot", b)+5))
	if rec, opened := openPath(t, s, "bots/short/main.bot"); rec.Code != http.StatusOK || opened.Bindable || opened.ReadRefusal == nil {
		t.Fatalf("precondition: the open is a salvage: %d bindable=%v refusal=%v", rec.Code, opened.Bindable, opened.ReadRefusal)
	}
	// What a salvage holds of lib/b.bot: none of its declarations.
	t.Setenv(parser.ReadBudgetEnv, "")
	_, emptied := openPath(t, s, "bots/short/lib/a.bot")
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(spentAlone(t, "lib/b.bot", b)+5))
	// The editor's flow: the open issued no digest for lib/b.bot, and the
	// save presents none.
	{
		body, _ := json.Marshal(saveFileRequest{Path: "bots/short/lib/b.bot", Document: emptied.Document})
		rec := httptest.NewRecorder()
		s.handleSaveFile(rec, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body)))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "digest") {
			t.Fatalf("a save of the salvage's lib/b.bot = %d %s, want 409 naming the digest no whole read issued", rec.Code, rec.Body.String())
		}
	}
	assertFiles(t, work, map[string]string{"bots/short/lib/b.bot": b}, "after the refused save")
}

// bigPrompts is a fragment of n prompts named prefix0…: read whole alone,
// past a budget it shares with another file.
func bigPrompts(prefix string, n int) string {
	var bb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&bb, "prompt %s%d:\n  x\n", prefix, i)
	}
	return bb.String()
}

// Every workflow file is a unit's main to the reader — a main in a
// subdirectory, a workflow below lib/, a fragment that imports others — so
// a unit whose main is any of them, cut short by the read budget its files
// share, refuses a write of a file it did not read whole, at both doors,
// and the stored bytes stay. A file such a load read whole, before its cut,
// is written as any.
func TestACloudUnitWhoseMainIsAnyWorkflowFileIsNotWrittenFromItsSalvage(t *testing.T) {
	a := "prompt pa:\n  a\n"
	b := bigPrompts("pb", 300)
	emptied := "## the file as the salvage holds it\n"
	manifest := "name: short\nversion: 1.0.0\nrequires:\n  iterion: \">= " + parser.ImportSince + "\"\n"
	for _, l := range []struct {
		name   string
		files  map[string]string
		victim string
		doors  []string
	}{
		// The per-file write of flows/lib/b.bot is refused first by the
		// compile oracle, which reads a .bot outside the root lib/ as a
		// workflow: only the bundle push reaches the unit check.
		{"subdir-main", map[string]string{botsource.MainBotFile: testBotMain, "flows/x.bot": "import \"lib/a.bot\"\nimport \"lib/b.bot\"\n" + strings.Replace(testBotMain, "workflow main:", "workflow x:", 1), "flows/lib/a.bot": a, "flows/lib/b.bot": b, "manifest.yaml": manifest}, "flows/lib/b.bot", []string{"whole-bundle push"}},
		{"workflow-under-lib", map[string]string{botsource.MainBotFile: testBotMain, "lib/w.bot": "import \"c.bot\"\nimport \"d.bot\"\n" + strings.Replace(testBotMain, "workflow main:", "workflow w:", 1), "lib/c.bot": a, "lib/d.bot": b, "manifest.yaml": manifest}, "lib/d.bot", []string{"whole-bundle push", "per-file write"}},
		{"fragment-chain", map[string]string{botsource.MainBotFile: testBotMain, "lib/a.bot": "import \"b.bot\"\n" + a, "lib/b.bot": b, "manifest.yaml": manifest}, "lib/b.bot", []string{"whole-bundle push", "per-file write"}},
	} {
		for _, door := range l.doors {
			t.Run(l.name+"/"+door, func(t *testing.T) {
				s, editor, _ := newBotSourceTestServer(t)
				ctx := auth.WithIdentity(context.Background(), editor)
				t.Setenv(parser.ReadBudgetEnv, "")
				if w := putBundle(s, ctx, "short", l.files, 0); w.Code != http.StatusOK {
					t.Fatalf("storing = %d %s", w.Code, w.Body.String())
				}
				t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(spentAlone(t, l.victim, b)+5))
				for name, src := range l.files {
					if strings.HasSuffix(name, ".bot") {
						if _, err := parser.Parse(name, src); err != nil {
							t.Fatalf("precondition: %s alone reads whole: %v", name, err)
						}
					}
				}
				// The digest rule holds both doors: the open of this bot
				// issued NO digest for the file its shared budget refused,
				// so the editor's write — the push presenting every digest
				// BUT that file's, the per-file write none — is a 409.
				var w *httptest.ResponseRecorder
				if door == "whole-bundle push" {
					next := map[string]string{}
					for k, v := range l.files {
						next[k] = v
					}
					next[l.victim] = emptied
					digests := storedDigests(t, s, ctx, "short")
					delete(digests, l.victim)
					b2, _ := json.Marshal(botSourcePutReq{Files: next, Version: 1, ID: storedID(t, s, ctx, "short"), StoredSHA256: digests})
					r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/short", bytes.NewReader(b2)).WithContext(ctx)
					r.SetPathValue("id", "t1")
					r.SetPathValue("slug", "short")
					w = httptest.NewRecorder()
					s.handlePutBotSource(w, r)
				} else {
					w = putFile(s, ctx, "short", l.victim, emptied, 1, "")
				}
				if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "digest") {
					t.Fatalf("a write of the salvage's %s = %d %s, want 409 naming the digest no whole read issued", l.victim, w.Code, w.Body.String())
				}
				if got := storedFiles(t, s, "short").Files[l.victim]; got != b {
					t.Fatalf("%s is %d bytes after the refused write, want the %d stored", l.victim, len(got), len(b))
				}
			})
		}
	}
	t.Run("fragment-chain/a file read before the cut", func(t *testing.T) {
		s, editor, _ := newBotSourceTestServer(t)
		ctx := auth.WithIdentity(context.Background(), editor)
		files := map[string]string{botsource.MainBotFile: testBotMain, "lib/a.bot": "import \"b.bot\"\n" + a, "lib/b.bot": b, "manifest.yaml": manifest}
		t.Setenv(parser.ReadBudgetEnv, "")
		if w := putBundle(s, ctx, "short", files, 0); w.Code != http.StatusOK {
			t.Fatalf("storing = %d %s", w.Code, w.Body.String())
		}
		t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(spentAlone(t, "lib/b.bot", b)+5))
		edited := "import \"b.bot\"\nprompt pa:\n  edited\n"
		// lib/a.bot was read whole before the cut: the open issued its
		// digest, and the write presenting it lands.
		if w := putFile(s, ctx, "short", "lib/a.bot", edited, 1, contentSHA256(files["lib/a.bot"])); w.Code != http.StatusOK {
			t.Fatalf("a write of lib/a.bot, read whole before the cut = %d %s, want 200", w.Code, w.Body.String())
		}
		if got := storedFiles(t, s, "short").Files["lib/a.bot"]; got != edited {
			t.Fatalf("lib/a.bot is %q after its write, want the edit", got)
		}
	})
}

// The disk twin, for every file the reader opens as a unit: a fragment
// chain no main at the bot's root reaches (lib/a.bot imports b.bot) opens
// as its own unit, a salvage; a save of lib/b.bot written from it is
// refused whatever the document declares — membership is the stored files',
// so a document holding a workflow is held to it too — and the bytes stay.
func TestADiskFragmentChainIsNotSavedFromItsSalvage(t *testing.T) {
	b := bigPrompts("pb", 300)
	work := t.TempDir()
	writeFiles(t, work, map[string]string{
		"bots/chain/main.bot":  unitFixtureNodes + "\nworkflow w:\n  entry: worker\n  worker -> done\n",
		"bots/chain/lib/a.bot": "import \"b.bot\"\nprompt pa:\n  a\n",
		"bots/chain/lib/b.bot": b,
		"bots/chain/lib/c.bot": "prompt pc:\n  c\n",
	})
	s := newOrgTestServer(t)
	s.cfg.WorkDir = work
	t.Setenv(parser.ReadBudgetEnv, "")
	_, fragment := openPath(t, s, "bots/chain/lib/c.bot")
	_, workflow := openPath(t, s, "bots/chain/main.bot")
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(spentAlone(t, "lib/b.bot", b)+5))
	if rec, opened := openPath(t, s, "bots/chain/lib/a.bot"); rec.Code != http.StatusOK || opened.Bindable || opened.ReadRefusal == nil {
		t.Fatalf("precondition: lib/a.bot opens as a salvage: %d bindable=%v refusal=%v", rec.Code, opened.Bindable, opened.ReadRefusal)
	}
	for what, doc := range map[string]json.RawMessage{"a fragment": fragment.Document, "a workflow": workflow.Document} {
		// The editor's flow: the open of the chain issued no digest for
		// lib/b.bot, and the save presents none.
		body, _ := json.Marshal(saveFileRequest{Path: "bots/chain/lib/b.bot", Document: doc})
		rec := httptest.NewRecorder()
		s.handleSaveFile(rec, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body)))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "digest") {
			t.Errorf("a save of %s over lib/b.bot, which no whole read issued a digest for = %d %s, want 409 naming the digest", what, rec.Code, rec.Body.String())
		}
		assertFiles(t, work, map[string]string{"bots/chain/lib/b.bot": b}, "after the save of "+what)
	}
}

// A file that is no text the reader reads (E006) is the one file its bot's
// stored load does not read: a save of another fragment of the bot, read
// whole, lands. A bot whose MAIN is not text reads none of its fragments —
// its imports are never followed — and their saves are refused, naming the
// main's encoding and its remedy. A fragment that is there and not read
// (its permissions) opens its bot as a salvage.
func TestAFileTheReaderDoesNotReadHoldsOnlyWhatItsLoadLeftUnread(t *testing.T) {
	tail := "\nworkflow w:\n  entry: worker\n  worker -> done\n"
	work := t.TempDir()
	writeFiles(t, work, map[string]string{
		"bots/e6/main.bot":  "import \"lib/a.bot\"\nimport \"lib/b.bot\"\n" + unitFixtureNodes + tail,
		"bots/e6/lib/a.bot": "prompt pa:\n  caf\xe9\n",
		"bots/e6/lib/b.bot": "prompt pb:\n  b\n",
		"bots/e7/main.bot":  "import \"lib/b.bot\"\n" + strings.Replace(unitFixtureNodes, "Do the thing.", "Do the th\xefng.", 1) + tail,
		"bots/e7/lib/b.bot": "prompt pb:\n  b\n",
		"bots/u/main.bot":   "import \"lib/a.bot\"\nimport \"lib/b.bot\"\n" + unitFixtureNodes + tail,
		"bots/u/lib/a.bot":  "prompt pa:\n  keep me\n",
		"bots/u/lib/b.bot":  "prompt pb:\n  b\n",
	})
	s := newOrgTestServer(t)
	s.cfg.WorkDir = work
	edited := func(t *testing.T, path string) json.RawMessage {
		t.Helper()
		rec, opened := openPath(t, s, path)
		if rec.Code != http.StatusOK || !opened.Bindable {
			t.Fatalf("precondition: %s opens alone, read whole: %d %+v", path, rec.Code, opened.ReadRefusal)
		}
		return withPrompt(t, opened.Document, "", "added", "Added.")
	}
	rec, saved := savePath(t, s, "bots/e6/lib/b.bot", edited(t, "bots/e6/lib/b.bot"), "")
	if rec.Code != http.StatusOK || !strings.Contains(saved.Source, "added") {
		t.Errorf("a save of lib/b.bot beside a fragment that is not UTF-8 = %d %s, want 200", rec.Code, rec.Body.String())
	}
	// The fragment's own door judges the fragment alone: b.bot read whole
	// (its digest issued) is the edit's to make, whatever the main's
	// encoding is — what no whole read can issue a digest for is the E006
	// main itself, and ITS save is the refused one.
	rec, _ = savePath(t, s, "bots/e7/lib/b.bot", edited(t, "bots/e7/lib/b.bot"), "")
	if rec.Code != http.StatusOK {
		t.Errorf("a save of lib/b.bot, read whole alone, under a main that is not UTF-8 = %d %s, want 200", rec.Code, rec.Body.String())
	}
	e7main := readFixture(t, work, "bots/e7/main.bot")
	{
		_, refusedOpen := openPath(t, s, "bots/e7/main.bot")
		body, _ := json.Marshal(saveFileRequest{Path: "bots/e7/main.bot", Document: refusedOpen.Document})
		rec := httptest.NewRecorder()
		s.handleSaveFile(rec, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not read whole") || !strings.Contains(rec.Body.String(), "E006") {
			t.Errorf("a save of the E006 main from the marked document its refusal answered = %d %s, want 400 (the mark)", rec.Code, rec.Body.String())
		}
	}
	assertFiles(t, work, map[string]string{"bots/e7/main.bot": e7main}, "after the E006 main's refused save")

	a := filepath.Join(work, "bots/u/lib/a.bot")
	if err := os.Chmod(a, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(a, 0o644) })
	if _, err := os.ReadFile(a); err == nil {
		t.Skip("this user reads a mode-0 file")
	}
	if rec, opened := openPath(t, s, "bots/u/main.bot"); rec.Code != http.StatusOK || opened.Bindable || opened.ReadRefusal == nil || !strings.Contains(opened.ReadRefusal.Message, "lib/a.bot") {
		t.Errorf("the open of a bot with a fragment it cannot read = %d bindable=%v refusal=%+v, want a salvage naming lib/a.bot", rec.Code, opened.Bindable, opened.ReadRefusal)
	}
}
