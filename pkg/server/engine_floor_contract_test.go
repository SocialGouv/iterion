package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func pushContractBundle(requires string) string {
	manifest := "name: contracted\nversion: 1.0.0\n"
	if requires != "" {
		manifest += "requires:\n  iterion: \"" + requires + "\"\n"
	}
	body, _ := json.Marshal(map[string]any{"files": map[string]string{
		"main.bot":      "vars:\n  goal: string\n\nschema out:\n  ok: bool\n\ncontract pub:\n  inputs:\n    goal: string\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"x\"\n  output: out\n\nworkflow main:\n  contract: pub\n  entry: worker\n  worker -> done\n",
		"manifest.yaml": manifest,
	}})
	return string(body)
}

// A bundle that declares a contract pushed without a floor is refused: a
// runner older than the release that reads `contract` fails at its first
// parse of the main. With the floor declared, the push lands. The same
// predicate as `validate`'s C252 and the import guard, derived from the
// one registry of floors.
func TestAdminBotsPush_RefusesABundleWithAContractWithoutAFloor(t *testing.T) {
	pinServerBuild(t, "v"+parser.ContractSince+"+deadbeef")
	s, admin, _ := adminBotsServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v" + parser.ContractSince + "+abc123"}}

	w := adminBotsPutQuery(s, admin, "contracted", "", pushContractBundle(""))
	if w.Code != http.StatusConflict {
		t.Fatalf("push = %d %s, want 409 — a bot with a contract and no floor must not be stored", w.Code, w.Body.String())
	}
	for _, want := range []string{"`contract` (main.bot)", parser.ContractSince, "--force"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("refusal %q does not mention %q", w.Body.String(), want)
		}
	}
	w = adminBotsPutQuery(s, admin, "contracted", "", pushContractBundle(">= "+parser.ImportSince))
	if w.Code != http.StatusConflict {
		t.Fatalf("push with the import floor = %d, want 409 — a floor below the contract release admits runners that cannot read it", w.Code)
	}
	w = adminBotsPutQuery(s, admin, "contracted", "", pushContractBundle(">= "+parser.ContractSince))
	if w.Code/100 != 2 {
		t.Fatalf("push with the floor = %d %s, want success", w.Code, w.Body.String())
	}
}
