package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `iterion validate` on a bundle whose subbot child keeps a contract holds
// the parent's `with:` to it (C255), through the real command: a required
// input not passed is named; passed, the warning is gone. A child the
// bundle's collection does not hold is left to C253.
func TestRunValidate_HoldsASubbotToItsChildsContract(t *testing.T) {
	inTempWorkspace(t)
	dir := filepath.Join("bots", "parent")
	child := "vars:\n  goal: string\n  depth: int\n\nschema out:\n  url: string\n\nprompt u:\n  Do {{vars.goal}}.\n\nagent b:\n  model: \"anthropic/claude-sonnet-4-6\"\n  user: u\n  output: out\n\ncontract kid:\n  inputs:\n    goal: string\n    depth: int\n  outputs:\n    url: string\n      from: b.url\n\nworkflow k:\n  contract: kid\n  entry: b\n  b -> done\n"
	parent := func(with string) string {
		return "vars:\n  goal: string\n\nschema result:\n  url: string\n\nsubbot child:\n  source: \"kids/k.bot\"\n  with { " + with + " }\n  output: result\n\nworkflow p:\n  entry: child\n  child -> done\n"
	}
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.yaml", "name: parent\ndisplay_name: Parent\n")
	write("kids/k.bot", child)
	write("main.bot", parent(`goal: "{{vars.goal}}"`))
	jp, out := jsonPrinter()
	if err := RunValidate(filepath.Join(dir, "main.bot"), jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if s := out.String(); !strings.Contains(s, "C255") || !strings.Contains(s, `requires input \"depth\"`) {
		t.Fatalf("the missing input was not named:\n%s", s)
	}
	write("main.bot", parent(`goal: "{{vars.goal}}", depth: "3"`))
	jp, out = jsonPrinter()
	if err := RunValidate(filepath.Join(dir, "main.bot"), jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "C255") {
		t.Fatalf("every input is passed, yet asked for:\n%s", out.String())
	}
	// The child's var carries the default: the input is optional (the port
	// mirrors the var, C300), and a parent that does not pass it is not
	// asked to.
	write("kids/k.bot", strings.Replace(child, "  depth: int\n\n", "  depth: int = 3\n\n", 1))
	write("main.bot", parent(`goal: "{{vars.goal}}"`))
	jp, out = jsonPrinter()
	if err := RunValidate(filepath.Join(dir, "main.bot"), jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "C255") {
		t.Fatalf("the child's var has a default, yet the input is asked for:\n%s", out.String())
	}
	// A child that does not compile clean is left to its own validate: its
	// contract is not what it says it is, so the parent is not held to it.
	write("kids/k.bot", strings.Replace(child, "    depth: int\n  outputs:", "    depth: int\n    nope: string\n  outputs:", 1))
	jp, out = jsonPrinter()
	if err := RunValidate(filepath.Join(dir, "main.bot"), jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "C255") {
		t.Fatalf("a child with a C300 of its own was held to its contract:\n%s", out.String())
	}
	// A subbot declared in a fragment writes its source against the
	// fragment's directory; the unit loader makes it root-relative, and the
	// projection reads the child where every host resolves it.
	write("kids/k.bot", child)
	write("lib/nodes.bot", "subbot child:\n  source: \"../kids/k.bot\"\n  with { goal: \"{{vars.goal}}\" }\n  output: result\n")
	write("main.bot", "import \"lib/nodes.bot\"\n\nvars:\n  goal: string\n\nschema result:\n  url: string\n\nworkflow p:\n  entry: child\n  child -> done\n")
	jp, out = jsonPrinter()
	if err := RunValidate(filepath.Join(dir, "main.bot"), jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if s := out.String(); !strings.Contains(s, "C255") || !strings.Contains(s, `requires input \"depth\"`) {
		t.Fatalf("the child of a subbot declared in a fragment was not held:\n%s", s)
	}
}
