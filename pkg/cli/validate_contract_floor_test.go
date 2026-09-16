package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// `iterion validate` on a bundle that declares a contract and no engine
// floor says so (C252), naming the release that reads `contract`; with the
// floor declared, the warning is gone — the author's half of the guard the
// push admission applies.
func TestRunValidate_AsksAContractedBundleForItsFloor(t *testing.T) {
	inTempWorkspace(t)
	dir := filepath.Join("bots", "contracted")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainBot := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(mainBot, []byte("vars:\n  goal: string\n\ncontract pub:\n  inputs:\n    goal: string\n\nworkflow w:\n  contract: pub\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: contracted\ndisplay_name: Contracted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidate(mainBot, jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if s := out.String(); !strings.Contains(s, "C252") || !strings.Contains(s, "`contract` (main.bot)") || !strings.Contains(s, parser.ContractSince) {
		t.Fatalf("validate did not ask for the contract floor:\n%s", s)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: contracted\ndisplay_name: Contracted\nrequires:\n  iterion: \">= "+parser.ContractSince+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out = jsonPrinter()
	if err := RunValidate(mainBot, jp); err != nil {
		t.Fatalf("validate with the floor: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "C252") {
		t.Fatalf("the floor is declared, yet asked for:\n%s", out.String())
	}
}
