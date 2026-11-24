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
	"github.com/SocialGouv/iterion/pkg/store"
)

// The fold refusal on the two routes that take the AUTHOR'S OWN bytes
// (#1612, #1679): a bound with no override is a defect, so both name their
// escape hatch — `allow_fold` on the request body, default off. A write
// that names it lands, and says what it did in the response's warnings;
// one that does not is still refused, and the refusal now names the hatch.
//
// The writer-rendered paths (handleSaveFile, saveUnit, unparseUnitFiles)
// keep the refusal with NO hatch: there a fold is iterion destroying the
// author's file, never the author's choice.

// A bot whose main writes its command over two lines (the backtick raw
// string), and the same bot with that value folded onto one. Both compile.
// Profile 2: the escape in the folded form reads as a real newline, which
// is what makes the fold a fold. The profile's floor rides the manifest.
const foldManifest = "name: folder\nversion: 1.0.0\nrequires:\n  iterion: \">= 3.141.0\"\n"

const foldSpreadMain = "dsl: 2\n\ntool work:\n" +
	"  command: `printf one\nprintf two`\n" +
	"  output: out\n" +
	"\n" +
	"schema out:\n" +
	"  ok: bool\n" +
	"\n" +
	"workflow main:\n" +
	"  entry: work\n" +
	"  work -> done\n"

const foldFoldedMain = "dsl: 2\n\ntool work:\n" +
	"  command: \"printf one\\nprintf two\"\n" +
	"  output: out\n" +
	"\n" +
	"schema out:\n" +
	"  ok: bool\n" +
	"\n" +
	"workflow main:\n" +
	"  entry: work\n" +
	"  work -> done\n"

func createFoldBot(t *testing.T, s *Server, edCtx context.Context, slug string) botSourceView {
	t.Helper()
	pinServerBuild(t, "9.9.9")
	body, _ := json.Marshal(botSourcePutReq{Files: map[string]string{
		botsource.MainBotFile: foldSpreadMain,
		"manifest.yaml":       foldManifest,
	}})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+slug, strings.NewReader(string(body))).WithContext(edCtx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var view botSourceView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func storedFoldFile(t *testing.T, s *Server, slug string) string {
	t.Helper()
	stored, err := s.botSources.GetBySlug(store.WithTenant(context.Background(), "t1"), "t1", slug)
	if err != nil {
		t.Fatal(err)
	}
	return stored.Files[botsource.MainBotFile]
}

func putFoldFile(s *Server, edCtx context.Context, slug string, view botSourceView, allowFold bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(botSourceFilePutReq{
		Content:      foldFoldedMain,
		Version:      view.Version,
		ID:           view.ID,
		StoredSHA256: view.FileSHA256[botsource.MainBotFile],
		AllowFold:    allowFold,
	})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+slug+"/files/main.bot", strings.NewReader(string(body))).WithContext(edCtx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", slug)
	r.SetPathValue("path", "main.bot")
	w := httptest.NewRecorder()
	s.handlePutBotSourceFile(w, r)
	return w
}

func putFoldBundle(s *Server, edCtx context.Context, slug string, view botSourceView, allowFold bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(botSourcePutReq{
		Files: map[string]string{
			botsource.MainBotFile: foldFoldedMain,
			"manifest.yaml":       foldManifest,
		},
		Version:      view.Version,
		ID:           view.ID,
		StoredSHA256: map[string]string{botsource.MainBotFile: view.FileSHA256[botsource.MainBotFile]},
		AllowFold:    allowFold,
	})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+slug, strings.NewReader(string(body))).WithContext(edCtx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	return w
}

func TestBotSourceFilePut_FoldRefusalNamesAllowFold(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)
	view := createFoldBot(t, s, edCtx, "folder")

	w := putFoldFile(s, edCtx, "folder", view, false)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a folding write without allow_fold = %d, want 422: %s", w.Code, w.Body.String())
	}
	// The refusal names the file, the line — and, since #1679, the hatch.
	if !strings.Contains(w.Body.String(), "allow_fold") {
		t.Fatalf("the refusal does not name its escape hatch: %s", w.Body.String())
	}
	// Nothing was written.
	if got := storedFoldFile(t, s, "folder"); got != foldSpreadMain {
		t.Fatal("a refused write moved the stored file")
	}
}

func TestBotSourceFilePut_AllowFoldWritesAndWarns(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)
	view := createFoldBot(t, s, edCtx, "folder")

	w := putFoldFile(s, edCtx, "folder", view, true)
	if w.Code != http.StatusOK {
		t.Fatalf("a folding write naming allow_fold = %d: %s", w.Code, w.Body.String())
	}
	var out botSourceView
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "main.bot") || !strings.Contains(out.Warnings[0], "fold") {
		t.Fatalf("the write that folded says so in its warnings: %+v", out.Warnings)
	}
	if got := storedFoldFile(t, s, "folder"); got != foldFoldedMain {
		t.Fatal("an allowed fold did not land")
	}
}

func TestBotSourcePut_FoldRefusalNamesAllowFold(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)
	view := createFoldBot(t, s, edCtx, "folder")

	w := putFoldBundle(s, edCtx, "folder", view, false)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a folding push without allow_fold = %d, want 422: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "allow_fold") {
		t.Fatalf("the refusal does not name its escape hatch: %s", w.Body.String())
	}
}

func TestBotSourcePut_AllowFoldWritesAndWarns(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)
	view := createFoldBot(t, s, edCtx, "folder")

	w := putFoldBundle(s, edCtx, "folder", view, true)
	if w.Code != http.StatusOK {
		t.Fatalf("a folding push naming allow_fold = %d: %s", w.Code, w.Body.String())
	}
	var out botSourceView
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "main.bot") {
		t.Fatalf("the push that folded says so in its warnings: %+v", out.Warnings)
	}
	if got := storedFoldFile(t, s, "folder"); got != foldFoldedMain {
		t.Fatal("an allowed fold did not land")
	}
}

// A bound the READER hit is not a fold the author chose: allow_fold lifts
// the refusal on a fold that is SEEN, never the one that cannot be told —
// there the write stays refused, hatch or not (fail closed).
func TestBotSourceFilePut_AllowFoldCannotLiftAnUnreadableStored(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)
	t.Setenv("ITERION_DSL_READ_BUDGET", "64")
	// Stored main is past the budget: Folds answers err, and no hatch asks
	// the write to proceed on a question nobody could answer.
	big := foldSpreadMain + strings.Repeat("\ntool u:\n  command: \"x\"\n", 40)
	created, err := s.botSources.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{
		TenantID: "t1",
		Slug:     "folder",
		Files:    map[string]string{botsource.MainBotFile: big},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(botSourceFilePutReq{
		Content:      foldFoldedMain,
		Version:      created.Version,
		ID:           created.ID,
		StoredSHA256: contentSHA256(big),
		AllowFold:    true,
	})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/folder/files/main.bot", strings.NewReader(string(body))).WithContext(edCtx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", "folder")
	r.SetPathValue("path", "main.bot")
	w := httptest.NewRecorder()
	s.handlePutBotSourceFile(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a fold that cannot be told stays refused, allow_fold or not: got %d: %s", w.Code, w.Body.String())
	}
}
