package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func pushMultiFileBundle(requires string) string {
	manifest := "name: multi\nversion: 1.0.0\n"
	if requires != "" {
		manifest += "requires:\n  iterion: \"" + requires + "\"\n"
	}
	body, _ := json.Marshal(map[string]any{"files": map[string]string{
		"main.bot":      "import \"lib/nodes.bot\"\n\nworkflow main:\n  entry: worker\n  worker -> done\n",
		"lib/nodes.bot": "schema out:\n  ok: bool\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"x\"\n  output: out\n",
		"manifest.yaml": manifest,
	}})
	return string(body)
}

// A bot in several files pushed without a floor is refused: a runner older
// than the release that reads `import` parses the fragments as text and
// fails there, after admission. With the floor declared, the push lands.
func TestAdminBotsPush_RefusesABundleThatImportsWithoutAFloor(t *testing.T) {
	pinServerBuild(t, "v3.145.0+deadbeef")
	s, admin, _ := adminBotsServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v3.145.0+abc123"}}

	w := adminBotsPutQuery(s, admin, "multi", "", pushMultiFileBundle(""))
	if w.Code != http.StatusConflict {
		t.Fatalf("push = %d %s, want 409 — a bot in several files without a floor must not be stored", w.Code, w.Body.String())
	}
	for _, want := range []string{"`import` (main.bot)", parser.ImportSince, "--force"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("refusal %q does not mention %q", w.Body.String(), want)
		}
	}
	w = adminBotsPutQuery(s, admin, "multi", "", pushMultiFileBundle(">= "+parser.ImportSince))
	if w.Code/100 != 2 {
		t.Fatalf("push with the floor = %d %s, want success", w.Code, w.Body.String())
	}
}
