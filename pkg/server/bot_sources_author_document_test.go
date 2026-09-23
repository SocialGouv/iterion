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

// A push carrying an author document is refused before the store is written,
// with the stable code a client acts on; the slug is never created.
func TestBotSourcePushRefusesADraft(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)
	body, _ := json.Marshal(botSourcePutReq{Files: map[string]string{
		botsource.MainBotFile: testBotMain,
		"main.bot.yaml":       "dsl: 2\n",
	}})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/drafted", strings.NewReader(string(body))).WithContext(edCtx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", "drafted")
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("push with a draft = %d: %s", w.Code, w.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["error_code"] != "author_document" {
		t.Fatalf("code %q, want author_document (%s)", got["error_code"], w.Body.String())
	}
	gr := httptest.NewRequest("GET", "/api/teams/t1/bot-sources/drafted", nil).WithContext(edCtx)
	gr.SetPathValue("id", "t1")
	gr.SetPathValue("slug", "drafted")
	gw := httptest.NewRecorder()
	s.handleGetBotSource(gw, gr)
	if gw.Code != http.StatusNotFound {
		t.Fatalf("a refused push created the bot source: %d %s", gw.Code, gw.Body.String())
	}
}
