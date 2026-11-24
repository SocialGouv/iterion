package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A main.bot whose only prompt lives in prompts/mission.md — the
// multi-file gallery shape's mechanism, reduced.
const botSourceMainWithExternalPrompt = "agent campaign:\n" +
	"  model: \"m\"\n" +
	"  backend: \"claw\"\n" +
	"  system: mission\n" +
	"\n" +
	"workflow main:\n" +
	"  entry: campaign\n" +
	"  campaign -> done\n"

// The row's stored sibling: a root-level entry of its own whose only
// prompt lives in the SAME prompts/mission.md.
const botSourceWorkerWithExternalPrompt = "agent worker:\n" +
	"  model: \"m\"\n" +
	"  backend: \"claw\"\n" +
	"  system: mission\n" +
	"\n" +
	"workflow worker:\n" +
	"  entry: worker\n" +
	"  worker -> done\n"

// botSourceDoc parses a source and marshals it the way the editor sends it.
func botSourceDoc(t *testing.T, src, name string) string {
	t.Helper()
	pr := parsertest.Parse(name, src)
	if len(pr.Diagnostics) != 0 {
		t.Fatalf("parse %s: %v", name, pr.Diagnostics)
	}
	doc, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	return string(doc)
}

// TestValidate_BotSourcePathMergesItsPrompts: the cloud editor validates a
// bot from the tenant store with the prompts/*.md the store holds, when the
// request names the bot as the editor's `botsource://<team>/<slug>/…` path
// and the caller is in that team. The bundle's entrypoint AND its root
// entries share the prompts — the parity bundle.DirForEntry gives a disk
// sibling. Any other team's path, or a slug the store does not know, is a
// hint the server ignores.
func TestValidate_BotSourcePathMergesItsPrompts(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)

	bs := botsource.BotSource{TenantID: "t1", Slug: "mf", CreatedBy: editor.UserID, Files: map[string]string{
		botsource.MainBotFile: botSourceMainWithExternalPrompt,
		"worker.bot":          botSourceWorkerWithExternalPrompt,
		"prompts/mission.md":  "Do the thing.",
	}}
	if _, err := s.botSources.Create(store.WithTenant(edCtx, "t1"), bs); err != nil {
		t.Fatalf("Create: %v", err)
	}

	mainDoc := botSourceDoc(t, botSourceMainWithExternalPrompt, "main.bot")
	workerDoc := botSourceDoc(t, botSourceWorkerWithExternalPrompt, "worker.bot")

	validate := func(ctx context.Context, doc, path string) (int, validateResponse) {
		t.Helper()
		body := `{"document":` + doc + `,"path":` + strconvQuote(path) + `}`
		r := httptest.NewRequest(http.MethodPost, "/api/validate", strings.NewReader(body)).WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleValidate(w, r)
		var out validateResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("path %q: status=%d body=%s (%v)", path, w.Code, w.Body.String(), err)
		}
		return w.Code, out
	}

	if _, out := validate(edCtx, mainDoc, "botsource://t1/mf/main.bot"); !out.Valid {
		t.Fatalf("the editor's own bot validated without its stored prompts: %v", out.Diagnostics)
	}
	// The stored sibling validates WITH the bundle's prompts — its own
	// document first, then the row's main read AS the sibling: the path no
	// longer gates the merge.
	if _, out := validate(edCtx, workerDoc, "botsource://t1/mf/worker.bot"); !out.Valid {
		t.Fatalf("a stored sibling validated alone, without its bundle's prompts: %v", out.Diagnostics)
	}
	if _, out := validate(edCtx, mainDoc, "botsource://t1/mf/worker.bot"); !out.Valid {
		t.Fatalf("the bundle's prompts did not follow the sibling path: %v", out.Diagnostics)
	}
	for _, p := range []string{"", "botsource://t2/mf/main.bot", "botsource://t1/unknown/main.bot", "botsource://t1"} {
		if _, out := validate(edCtx, mainDoc, p); out.Valid {
			t.Errorf("path %q: the document validated as if the stored prompts were in scope", p)
		}
	}
}

// TestValidate_BotSourceStoreFailureSurfaces: only an unknown slug is the
// hint the server ignores — a store read that fails for any other reason is
// the server's, and must not read as "validated alone".
func TestValidate_BotSourceStoreFailureSurfaces(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)

	failing := &failingBotSources{MemoryStore: s.botSources.(*botsource.MemoryStore)}
	s.botSources = failing

	doc := botSourceDoc(t, botSourceMainWithExternalPrompt, "main.bot")
	body := `{"document":` + doc + `,"path":"botsource://t1/mf/main.bot"}`
	r := httptest.NewRequest(http.MethodPost, "/api/validate", strings.NewReader(body)).WithContext(edCtx)
	w := httptest.NewRecorder()
	s.handleValidate(w, r)
	// The body stays the server's generic internal-error shape (the detail
	// rides the log); what the caller must NOT get is the 200 verdict of a
	// document validated alone while the store was failing.
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a failing store read answered %d, want 500: body=%s", w.Code, w.Body.String())
	}
}

// failingBotSources fails every GetBySlug with a store error that is NOT a
// botsource.ErrNotFound — the one absence the validate handler may ignore.
type failingBotSources struct {
	*botsource.MemoryStore
}

func (f *failingBotSources) GetBySlug(ctx context.Context, tenantID, slug string) (botsource.BotSource, error) {
	return botsource.BotSource{}, errors.New("store unavailable")
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
