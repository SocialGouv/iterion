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
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// The counts a unit answer carries — what the studio quotes in "Fix the N
// errors in … before launching" — are the readings' own, not the length of
// a list that reports at most a thousand of each code.
func TestUnitAnswerCountsEveryError(t *testing.T) {
	t.Setenv("ITERION_DSL_READ_BUDGET", "")
	src := "agent a:\n" + strings.Repeat("  bogus: 1\n", 1500) + "\nworkflow w:\n  entry: a\n  a -> done\n"
	info := unitInfoOf(unit.Salvage(unit.LoadMap(map[string]string{"main.bot": src}, "main.bot")), "main.bot")
	if len(info.Files) != 1 || info.Files[0].ParseErrors != 1500 {
		t.Fatalf("parse_errors = %+v, want 1500 for main.bot", info.Files)
	}
	if info.LoadErrors != 1500 {
		t.Errorf("load_errors = %d, want 1500", info.LoadErrors)
	}
	if !strings.Contains(info.LoadError, `unknown agent property "bogus"`) {
		t.Errorf("load_error = %q, want the first error", info.LoadError)
	}
}

// A stored file the server no longer reads whole — the budget lowered since
// it was stored, or a file stored before the budget existed — holds no
// value the fold check can count: a push that folds one of its values is
// refused for that, never let through as "nothing spread" (#1612 fails
// closed).
func TestBotSourcePutRefusesAFoldItCannotCheck(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)
	put := func(main string, version int) *httptest.ResponseRecorder {
		req := botSourcePutReq{Files: map[string]string{botsource.MainBotFile: main}, Version: version}
		if version != 0 {
			// The digest rule: the push presents the id and the stored
			// digests its read answered, so the refusal it is here for is
			// the fold's, not the identity's.
			req.ID = storedIDFor(s, edCtx, "folds")
			req.StoredSHA256 = storedDigestsFor(s, edCtx, "folds")
		}
		body, _ := json.Marshal(req)
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/folds", strings.NewReader(string(body))).WithContext(edCtx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", "folds")
		w := httptest.NewRecorder()
		s.handlePutBotSource(w, r)
		return w
	}
	spread := "## strict-escape: on\n\n" + strings.Replace(testBotMain, "`printf ok`", "`printf ok\nprintf two`", 1) + strings.Repeat("## padding\n", 20)
	folded := "## strict-escape: on\n\n" + strings.Replace(testBotMain, "`printf ok`", "\"printf ok\\nprintf two\"", 1)
	t.Setenv("ITERION_DSL_READ_BUDGET", "")
	if w := put(spread, 0); w.Code != http.StatusOK {
		t.Fatalf("storing the spread form = %d: %s", w.Code, w.Body.String())
	}
	t.Setenv("ITERION_DSL_READ_BUDGET", "64")
	w := put(folded, 1)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "cannot be told") || !strings.Contains(w.Body.String(), "the stored text is not read") {
		t.Fatalf("a folding push over a stored file past the budget = %d: %s, want 422 saying the fold cannot be told", w.Code, w.Body.String())
	}
	// The per-file route, the studio's cloud single-file save, the same.
	b, _ := json.Marshal(map[string]any{"content": folded})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/folds/files/"+botsource.MainBotFile, strings.NewReader(string(b))).WithContext(edCtx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", "folds")
	r.SetPathValue("path", botsource.MainBotFile)
	fw := httptest.NewRecorder()
	s.handlePutBotSourceFile(fw, r)
	if fw.Code != http.StatusUnprocessableEntity || !strings.Contains(fw.Body.String(), "cannot be told") {
		t.Fatalf("a folding file write over a stored file past the budget = %d: %s, want 422 saying the fold cannot be told", fw.Code, fw.Body.String())
	}
}
