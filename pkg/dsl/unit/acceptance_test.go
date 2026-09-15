package unit_test

// An external test package: it compiles through pkg/dsl/ir, which reaches
// pkg/bundle and the unit loader — a cycle for an in-package test.

import (
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

const pinnedModel = "anthropic/claude-opus-4-8"

// acceptanceKind is one named kind: a declaration named after its argument
// that compiles clean beside acceptanceBase, and the node ids it puts on the
// graph (none for a kind that is not a node), a terminal one last.
type acceptanceKind struct {
	kind     string
	decl     func(name string) string
	targets  func(name string) []string
	terminal bool
}

func noNodes(string) []string   { return nil }
func oneNode(n string) []string { return []string{n} }
func useNode(n string) []string { return []string{n + ".inner"} }
func llmDecl(kind string) func(string) string {
	return func(n string) string {
		return kind + " " + n + ":\n  model: \"" + pinnedModel + "\"\n  description: \"d\"\n"
	}
}

// acceptanceKinds: every named kind of the language. TestSplitPreservesAcceptance
// derives from them what the compiler accepts in one file and holds the
// loader's namespaces to it.
var acceptanceKinds = []acceptanceKind{
	{"prompt", func(n string) string { return "prompt " + n + ":\n  Text.\n" }, noNodes, false},
	{"schema", func(n string) string { return "schema " + n + ":\n  ok: bool\n" }, noNodes, false},
	{"cursor", func(n string) string { return "cursor " + n + ":\n  description: \"d\"\n  values:\n    low: \"a\"\n" }, noNodes, false},
	{"mcp_server", func(n string) string { return "mcp_server " + n + ":\n  transport: stdio\n  command: \"true\"\n" }, noNodes, false},
	{"supervisor", func(n string) string {
		return "supervisor " + n + ":\n  watches: [n0]\n  model: \"" + pinnedModel + "\"\n  system: p_s\n"
	}, noNodes, false},
	{"group", func(n string) string {
		return "group " + n + ":\n  agent inner:\n    model: \"" + pinnedModel + "\"\n    description: \"d\"\n"
	}, noNodes, false},
	{"use", func(n string) string { return "use g as " + n + "\n" }, useNode, false},
	{"agent", llmDecl("agent"), oneNode, false},
	{"judge", llmDecl("judge"), oneNode, false},
	{"router", func(n string) string { return "router " + n + ":\n  mode: fan_out_all\n" }, oneNode, false},
	{"human", func(n string) string { return "human " + n + ":\n  instructions: p_h\n  interaction: human\n" }, oneNode, false},
	{"tool", func(n string) string { return "tool " + n + ":\n  command: \"true\"\n" }, oneNode, false},
	{"compute", func(n string) string { return "compute " + n + ":\n  expr:\n    ok: \"true\"\n" }, oneNode, false},
	{"emit", func(n string) string { return "emit " + n + ":\n  event: \"e\"\n" }, oneNode, false},
	{"wait", func(n string) string { return "wait " + n + ":\n  event: \"e\"\n  timeout: \"30s\"\n" }, oneNode, false},
	{"await_answers", func(n string) string { return "await_answers " + n + ":\n  timeout: \"30s\"\n" }, oneNode, false},
	{"subbot", func(n string) string { return "subbot " + n + ":\n  source: \"kid.bot\"\n" }, oneNode, false},
	{"fail", func(n string) string { return "fail " + n + ":\n  code: FAIL_X\n  message: \"m\"\n" }, oneNode, true},
}

const acceptanceBase = "prompt p_h:\n  H.\n\nprompt p_s:\n  S.\n\ngroup g:\n  agent inner:\n    model: \"" + pinnedModel + "\"\n    description: \"d\"\n\nagent n0:\n  model: \"" + pinnedModel + "\"\n  description: \"d\"\n"

// acceptanceWorkflow chains the entry through every node the kinds put on
// the graph, once each, a terminal one last, so nothing is unreachable
// (C016) and no node has two default edges (C010).
func acceptanceWorkflow(kinds ...acceptanceKind) string {
	var chain []string
	seen := map[string]bool{}
	terminal := false
	for _, k := range kinds {
		for _, id := range k.targets("x") {
			if seen[id] {
				continue
			}
			seen[id] = true
			if k.terminal {
				terminal = true
				continue
			}
			chain = append(chain, id)
		}
	}
	if terminal {
		chain = append(chain, "x")
	}
	var b strings.Builder
	b.WriteString("workflow w:\n  entry: n0\n")
	prev := "n0"
	for _, id := range chain {
		b.WriteString("  " + prev + " -> " + id + "\n")
		prev = id
	}
	if !terminal {
		b.WriteString("  " + prev + " -> done\n")
	}
	return b.String()
}

// acceptanceErrors compiles a program given as a files map and returns its
// error codes — the loader's and the compiler's — sorted, minus the ones a
// credential-less host adds to any bot.
func acceptanceErrors(files map[string]string) []string {
	u := unit.LoadMap(files, "main.bot")
	var codes []string
	for _, d := range u.Diagnostics {
		if d.Severity == parser.SeverityError {
			codes = append(codes, string(d.Code))
		}
	}
	if u.Merged != nil {
		for _, d := range ir.Compile(u.Merged).Diagnostics {
			if d.Severity == ir.SeverityError && d.Code != ir.DiagMissingModelOrBackend {
				codes = append(codes, string(d.Code))
			}
		}
	}
	sort.Strings(codes)
	return codes
}

// sameVerdict: the split refuses exactly what one file refuses, for the
// same reasons — the loader's own E010 beside a duplicate the compiler
// reports too is the one code the split may add.
func sameVerdict(oneFile, split []string) bool {
	set := func(codes []string, drop string) []string {
		var out []string
		for _, c := range codes {
			if c != drop && (len(out) == 0 || out[len(out)-1] != c) {
				out = append(out, c)
			}
		}
		return out
	}
	if len(oneFile) == 0 {
		return len(split) == 0
	}
	return reflect.DeepEqual(set(oneFile, ""), set(split, "E010"))
}

// A mechanical split changes nothing about what the language accepts: two
// declarations that share a name are refused in one file exactly when they
// are refused across two — the loader's namespaces are the compiler's own,
// derived here from the compiler itself, not from a second hand-kept list.
// The same inline text written in two files is one prompt, as it is in one.
func TestSplitPreservesAcceptance(t *testing.T) {
	for _, k := range acceptanceKinds {
		if errs := acceptanceErrors(map[string]string{"main.bot": acceptanceBase + "\n" + k.decl("x") + "\n" + acceptanceWorkflow(k)}); len(errs) != 0 {
			t.Fatalf("the %s fixture does not compile clean on its own: %v", k.kind, errs)
		}
	}
	for _, a := range acceptanceKinds {
		for _, b := range acceptanceKinds {
			wf := acceptanceWorkflow(a, b)
			oneFile := acceptanceErrors(map[string]string{"main.bot": acceptanceBase + "\n" + a.decl("x") + "\n" + b.decl("x") + "\n" + wf})
			split := acceptanceErrors(map[string]string{
				"main.bot":  "import \"lib/f.bot\"\n\n" + acceptanceBase + "\n" + a.decl("x") + "\n" + wf,
				"lib/f.bot": b.decl("x"),
			})
			if !sameVerdict(oneFile, split) {
				t.Errorf("%s x + %s x: one file %v, split %v — a split must not change what the language accepts", a.kind, b.kind, oneFile, split)
			}
		}
	}
	// The same inline text in two files.
	same := "agent one:\n  model: \"" + pinnedModel + "\"\n  system: \"You are careful.\"\n"
	other := "agent two:\n  model: \"" + pinnedModel + "\"\n  system: \"You are careful.\"\n"
	wf := "workflow w:\n  entry: n0\n  n0 -> one\n  one -> two\n  two -> done\n"
	if errs := acceptanceErrors(map[string]string{"main.bot": "import \"lib/f.bot\"\n\n" + acceptanceBase + "\n" + same + "\n" + wf, "lib/f.bot": other}); len(errs) != 0 {
		t.Fatalf("the same inline text in two files is refused: %v", errs)
	}
	if errs := acceptanceErrors(map[string]string{"main.bot": acceptanceBase + "\n" + same + "\n" + other + "\n" + wf}); len(errs) != 0 {
		t.Fatalf("the same inline text twice in one file is refused: %v", errs)
	}
	// A key declared in two files is refused, as it is twice in one block.
	base := acceptanceBase + "\n" + acceptanceWorkflow()
	if errs := acceptanceErrors(map[string]string{"main.bot": "import \"lib/f.bot\"\n\nvars:\n  x: string\n\n" + base, "lib/f.bot": "vars:\n  x: int\n"}); !reflect.DeepEqual(errs, []string{"E010"}) {
		t.Fatalf("a var declared in two files: %v", errs)
	}
	if errs := acceptanceErrors(map[string]string{"main.bot": "vars:\n  x: string\n  x: int\n\n" + base}); !reflect.DeepEqual(errs, []string{"E010"}) {
		t.Fatalf("a var declared twice in one block: %v", errs)
	}
}

// The same inline text in two files is ONE prompt in the merge, so the
// merged program writes and re-reads as itself — with or without a
// workflow, which is where the writer's check compares the documents.
func TestTheSameInlineTextInTwoFilesIsOnePromptInTheMerge(t *testing.T) {
	files := map[string]string{
		"main.bot":  "import \"lib/f.bot\"\n\nagent one:\n  model: \"" + pinnedModel + "\"\n  system: \"You are careful.\"\n",
		"lib/f.bot": "agent two:\n  model: \"" + pinnedModel + "\"\n  system: \"You are careful.\"\n",
	}
	u := unit.LoadMap(files, "main.bot")
	if u.HasErrors() {
		t.Fatalf("diagnostics: %v", u.Diagnostics)
	}
	inline := 0
	for _, p := range u.Merged.Prompts {
		if p.Inline {
			inline++
		}
	}
	if inline != 1 {
		t.Fatalf("the merge holds %d inline prompts for one text", inline)
	}
	flat := unparse.Unparse(u.Merged)
	if err := unparse.Verify(u.Merged, flat); err != nil {
		t.Fatalf("the merged program is not its own text: %v", err)
	}
}
