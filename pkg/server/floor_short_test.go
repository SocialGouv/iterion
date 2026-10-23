package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The push admission does not certify a floor its walk read off a source
// cut short (r3a F4): child.bot's fragments are past the read budget and
// one of them declares `tools: []`, which needs a release past the
// manifest's floor — refused (409, C256) naming child.bot and the variable
// that moves the budget, never stored as the floor the `import` alone asks
// for; `--force` is the operator's explicit way past it, and says so.
func TestAPushWhoseSourceIsCutShortIsRefused(t *testing.T) {
	pinServerBuild(t, "v3.200.0+deadbeef")
	main := "prompt p:\n  Do.\n\nagent a:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: p\n\nworkflow main:\n  entry: a\n  a -> done\n"
	child := "import \"lib/big.bot\"\nimport \"lib/k.bot\"\n\nworkflow c:\n  entry: helper\n  helper -> done\n"
	k := "prompt mission:\n  Do.\n\nagent helper:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  tools: []\n"
	budget := 400
	big := "prompt big:\n" + strings.Repeat("  x\n", budget-spentAlone(t, "child.bot", child)-2)
	files := map[string]string{
		"main.bot": main, "child.bot": child, "lib/big.bot": big, "lib/k.bot": k,
		"manifest.yaml": "name: floorbot\nversion: 1.0.0\nrequires:\n  iterion: \">= " + parser.ImportSince + "\"\n",
	}
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
	push := func(slug, query string) *httptest.ResponseRecorder {
		s, editor, _ := newBotSourceTestServer(t)
		s.runnerBuilds = &fakeBuildObserver{builds: []string{"v3.200.0+abc123"}}
		body, _ := json.Marshal(botSourcePutReq{Files: files})
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+slug+query, strings.NewReader(string(body))).WithContext(auth.WithIdentity(context.Background(), editor))
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", slug)
		w := httptest.NewRecorder()
		s.handlePutBotSource(w, r)
		return w
	}
	w := push("floorbot", "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "child.bot") || !strings.Contains(w.Body.String(), "C256") || !strings.Contains(w.Body.String(), parser.ReadBudgetEnv) {
		t.Errorf("a push whose child is cut short: %d %s — want 409 naming child.bot, C256 and the budget's variable", w.Code, w.Body.String())
	}
	if w := push("floorbot2", "?force=1"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "FORCED") {
		t.Errorf("the forced push: %d %s — want 200 carrying the override", w.Code, w.Body.String())
	}
}

// A push forced past a source the reader cut short is forced past that
// guard alone: the engine requirement its manifest declares is still
// checked, and its warning rides beside — and the short source's warning
// names the source, its refusal and the variable that lifts it.
func TestAForcedPushPastAShortSourceIsToldEveryGuardItPassed(t *testing.T) {
	pinServerBuild(t, "v3.200.0+deadbeef")
	main := "prompt p:\n  Do.\n\nagent a:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: p\n\nworkflow main:\n  entry: a\n  a -> done\n"
	child := "import \"lib/big.bot\"\nimport \"lib/k.bot\"\n\nworkflow c:\n  entry: helper\n  helper -> done\n"
	k := "prompt mission:\n  Do.\n\nagent helper:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n"
	budget := 400
	big := "prompt big:\n" + strings.Repeat("  x\n", budget-spentAlone(t, "child.bot", child)-2)
	manifest := "name: floorbot\nversion: 1.0.0\nrequires:\n  iterion: \">= 9.9.9\"\n"
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
	s, editor, _ := newBotSourceTestServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v3.200.0+abc123"}}
	body, _ := json.Marshal(botSourcePutReq{Files: map[string]string{"main.bot": main, "child.bot": child, "lib/big.bot": big, "lib/k.bot": k, "manifest.yaml": manifest}})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/short?force=1", strings.NewReader(string(body))).WithContext(auth.WithIdentity(context.Background(), editor))
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", "short")
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	var view botSourceView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatalf("the forced push = %d %s", w.Code, w.Body.String())
	}
	var syntax, engine string
	for _, warning := range view.Warnings {
		switch {
		case strings.Contains(warning, "syntax-floor"):
			syntax = warning
		case strings.Contains(warning, "engine-requirement"):
			engine = warning
		}
	}
	if engine == "" || !strings.Contains(engine, ">= 9.9.9") {
		t.Errorf("the forced push past a short source carries no engine-requirement warning: %q", view.Warnings)
	}
	if !strings.Contains(syntax, "child.bot") || !strings.Contains(syntax, "lib/big.bot") || !strings.Contains(syntax, parser.ReadBudgetEnv) {
		t.Errorf("the forced short warning names neither the source, its cut nor the budget: %q", syntax)
	}
}
