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
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
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

// TestValidate_BotSourcePathMergesItsPrompts: the cloud editor validates
// a bot from the tenant store with the prompts/*.md the store holds, when
// the request names the bot as the editor's `botsource://<team>/<slug>/…`
// path and the caller is in that team. Any other team's path, or a slug
// the store does not know, is a hint the server ignores.
func TestValidate_BotSourcePathMergesItsPrompts(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)

	bs := botsource.BotSource{TenantID: "t1", Slug: "mf", CreatedBy: editor.UserID, Files: map[string]string{
		botsource.MainBotFile: botSourceMainWithExternalPrompt,
		"prompts/mission.md":  "Do the thing.",
	}}
	if _, err := s.botSources.Create(store.WithTenant(edCtx, "t1"), bs); err != nil {
		t.Fatalf("Create: %v", err)
	}

	pr := parser.Parse("main.bot", botSourceMainWithExternalPrompt)
	if len(pr.Diagnostics) != 0 {
		t.Fatalf("parse: %v", pr.Diagnostics)
	}
	doc, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}

	validate := func(ctx context.Context, path string) validateResponse {
		t.Helper()
		body := `{"document":` + string(doc) + `,"path":` + strconvQuote(path) + `}`
		r := httptest.NewRequest(http.MethodPost, "/api/validate", strings.NewReader(body)).WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleValidate(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("path %q: status=%d body=%s", path, w.Code, w.Body.String())
		}
		var out validateResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if out := validate(edCtx, "botsource://t1/mf/main.bot"); !out.Valid {
		t.Fatalf("the editor's own bot validated without its stored prompts: %v", out.Diagnostics)
	}
	for _, p := range []string{"", "botsource://t2/mf/main.bot", "botsource://t1/mf/worker.bot", "botsource://t1/unknown/main.bot", "botsource://t1"} {
		if out := validate(edCtx, p); out.Valid {
			t.Errorf("path %q: the document validated as if the stored prompts were in scope", p)
		}
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
