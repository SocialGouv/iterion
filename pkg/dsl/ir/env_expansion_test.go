package ir

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// envExpansionSrc wraps one `tool` block in a minimal workflow.
func envExpansionSrc(tool string) string {
	return tool + "\nworkflow w:\n  entry: gate\n  gate -> done\n"
}

// toolNodeOf returns the compiled tool node named gate.
func toolNodeOf(t *testing.T, r *CompileResult) *ToolNode {
	t.Helper()
	if r.Workflow == nil {
		t.Fatalf("compile produced no workflow: %v", r.Diagnostics)
	}
	tn, ok := r.Workflow.Nodes["gate"].(*ToolNode)
	if !ok {
		t.Fatalf("node gate is not a tool node: %T", r.Workflow.Nodes["gate"])
	}
	return tn
}

// errorsNaming returns the error-severity diagnostics whose message carries
// want, and every error for the message when none does.
func errorsNaming(r *CompileResult, want string) (naming, all []string) {
	for _, d := range r.Diagnostics {
		if d.Severity != SeverityError {
			continue
		}
		line := string(d.Code) + ": " + d.Message
		all = append(all, line)
		if strings.Contains(d.Message, want) {
			naming = append(naming, line)
		}
	}
	return naming, all
}

// The declaration reaches the executor's subject: an `env_expansion:` the
// compiler dropped would read as the default at run time — the engine
// rewriting the body — which is the opposite of what the author wrote.
func TestEnvExpansionReachesTheIR(t *testing.T) {
	for _, want := range []string{EnvExpansionBraced, EnvExpansionNone} {
		r := compileText(t, envExpansionSrc("tool gate:\n  command: \"echo ${NAME:-d}\"\n  env_expansion: "+want+"\n"))
		if _, all := errorsNaming(r, ""); len(all) != 0 {
			t.Fatalf("env_expansion: %s is listed and must compile clean, got %v", want, all)
		}
		if got := toolNodeOf(t, r).EnvExpansion; got != want {
			t.Errorf("ir.ToolNode.EnvExpansion = %q, want %q", got, want)
		}
	}
	// Absent, the IR carries "" — not a third state: model.ToolBodyEnv reads
	// it as braced, today's behaviour, which is what keeps every shipped bot
	// unchanged.
	plain := compileText(t, envExpansionSrc("tool gate:\n  command: \"echo ok\"\n"))
	if got := toolNodeOf(t, plain).EnvExpansion; got != "" {
		t.Errorf("a plain tool's EnvExpansion = %q, want the empty string", got)
	}
}

// A typo must not read as the default, because here the default is "the
// engine rewrites your body" and the line was written to stop exactly that.
// It is refused as an ERROR, on the arm `language:` already uses, with the
// accepted values spelled in the message.
func TestInvalidEnvExpansionIsAnError(t *testing.T) {
	r := compileText(t, envExpansionSrc("tool gate:\n  command: \"echo ${NAME:-d}\"\n  env_expansion: off\n"))
	naming, all := errorsNaming(r, "off")
	if len(naming) == 0 {
		t.Fatalf("`env_expansion: off` drew no error naming the value — a typo would read as the default, which is the expansion the author declined; errors: %v", all)
	}
	for _, want := range []string{EnvExpansionBraced, EnvExpansionNone} {
		if !strings.Contains(naming[0], want) {
			t.Errorf("the refusal does not spell %q among the accepted values: %s", want, naming[0])
		}
	}
	if code := codeOfFirstError(r, "off"); code != DiagBadTemplateRef {
		t.Errorf("the refusal rides %s, want %s — the arm `language:` already uses, so no second code is minted for it", code, DiagBadTemplateRef)
	}
}

// codeOfFirstError is the code of the first error naming want.
func codeOfFirstError(r *CompileResult, want string) DiagCode {
	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError && strings.Contains(d.Message, want) {
			return d.Code
		}
	}
	return ""
}

// The property governs a body the engine prepares. A node declaring none of
// the three — `command:`, `script:`, `postcondition:` — prepares no body, so
// the line is inert there, and inert reads as configured: it is refused, like
// `language:` without `script:`. An `action:` node is the shape that carries
// the trap, its arguments being the connector's and never a shell's.
//
// The refusal is its own `if`, not an arm of the exclusive recipe switch: a
// node that ALSO declares `language:` without a `script:`, or no recipe at
// all, is refused for that too, and an author who reads only the first
// message fixes it, recompiles and ships the inert line unmentioned. Every
// bodyless shape below names `env_expansion:`.
//
// And the refusal explains itself with the document it is printed on: the
// recipe-less shape below declares no `action:`, so a message blaming one
// sends its author hunting a property they never wrote.
//
// Mutations that redden it: move the `if` back into the switch above — the
// two shapes that trip an earlier arm go silent about the inert line; put the
// `action:` tail back on the message — the shape without one is explained by
// a property it does not declare.
func TestEnvExpansionWithoutABodyIsRefused(t *testing.T) {
	for name, tool := range map[string]string{
		"action alone":                "tool gate:\n  action: forgejo.issue.comment\n  connection: forge\n  env_expansion: none\n",
		"action and a stray language": "tool gate:\n  action: forgejo.issue.comment\n  connection: forge\n  language: py\n  env_expansion: none\n",
		"no recipe at all":            "tool gate:\n  env_expansion: none\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, envExpansionSrc(tool))
			naming, all := errorsNaming(r, "env_expansion")
			if len(naming) == 0 {
				t.Fatalf("`env_expansion:` on a bodyless node drew no error naming it — the author reads a declaration nothing honours; errors: %v", all)
			}
			// The three bodies the property governs are what makes the
			// refusal actionable: the author adds one, or drops the line.
			for _, body := range []string{"`command:`", "`script:`", "`postcondition:`"} {
				if !strings.Contains(naming[0], body) {
					t.Errorf("the refusal does not name %s among the bodies the property governs: %s", body, naming[0])
				}
			}
			// Nothing in the refusal may rest on a property this document
			// does not declare. A per-shape explanation is allowed — it just
			// has to be true of its shape.
			if !strings.Contains(tool, "action:") {
				for _, msg := range naming {
					if strings.Contains(msg, "action:") {
						t.Errorf("the refusal explains itself with an `action:` this node does not declare: %s", msg)
					}
				}
			}
		})
	}
	// And it is NOT refused where a body exists: a postcondition is a body
	// the engine prepares, so a node whose only body is one keeps the knob.
	ok := compileText(t, envExpansionSrc("tool gate:\n  command: \"true\"\n  goal: \"g\"\n  postcondition: \"test -n ${X:-y}\"\n  env_expansion: none\n"))
	if naming, _ := errorsNaming(ok, "env_expansion"); len(naming) != 0 {
		t.Errorf("a node with a command and a postcondition was refused the property: %v", naming)
	}
}

// The `postcondition:` conjunct of that refusal, witnessed on the one shape
// that can exercise it. A postcondition IS one of the three bodies the engine
// prepares, so the property is not inert beside it — but C263 refuses a
// postcondition on an `action:`, and a postcondition without a command or a
// script has no other recipe, so no COMPILING document reaches the conjunct.
// The document below is refused for the missing recipe and must stay silent
// about `env_expansion:`: the refusal speaks of an inert line, and this line
// is not one.
//
// Mutation that reddens it: drop `&& t.Postcondition == ""` from the refusal
// in compileTools — the inert message joins the missing-recipe one.
func TestEnvExpansionBesideAPostconditionIsNotInert(t *testing.T) {
	r := compileText(t, envExpansionSrc("tool gate:\n  goal: \"g\"\n  postcondition: \"test -n ${X:-y}\"\n  env_expansion: none\n"))
	naming, all := errorsNaming(r, "env_expansion")
	if len(all) == 0 {
		t.Fatalf("the fixture compiles clean, so it witnesses nothing — it must be refused for its missing recipe")
	}
	if len(naming) != 0 {
		t.Errorf("a node whose body is a postcondition was told its `env_expansion:` is inert: %v", naming)
	}
	// C263 is what keeps the valid shape — `action:` plus `postcondition:` —
	// out of reach, so this test is the conjunct's only witness. Red here
	// means the refusal moved and the conjunct needs a real document.
	withAction := compileText(t, envExpansionSrc("tool gate:\n  action: forgejo.issue.comment\n  connection: forge\n  goal: \"g\"\n  postcondition: \"test -n ${X:-y}\"\n  env_expansion: none\n"))
	if _, all := errorsNaming(withAction, ""); len(all) == 0 {
		t.Errorf("`action:` with a `postcondition:` compiles clean — C263 no longer stands, and the conjunct is now reachable from a valid document")
	}
}

// The predicate every reader asks fails CLOSED (ADR-130): it expands only on
// the two spellings that mean "expand". A nil node — a call with no
// declaration carrier — and any word the compiler refuses answer false,
// because the alternative is to rewrite a body the author may have declared
// verbatim. Both are kept out of a run today (every call site passes a
// compiled node, C004 refuses a third word), so this is the floor under the
// next caller, not a live path.
//
// Mutation that reddens it: return true from the predicate's default arm, or
// drop its nil guard.
func TestExpandsBracedEnvFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		node *ToolNode
		want bool
	}{
		"absent":            {&ToolNode{}, true},
		"braced":            {&ToolNode{EnvExpansion: EnvExpansionBraced}, true},
		"none":              {&ToolNode{EnvExpansion: EnvExpansionNone}, false},
		"a word off by one": {&ToolNode{EnvExpansion: EnvExpansionNone + "e"}, false},
		"uppercase":         {&ToolNode{EnvExpansion: "NONE"}, false},
		"no node at all":    {nil, false},
	} {
		if got := tc.node.ExpandsBracedEnv(); got != tc.want {
			t.Errorf("%s: ExpandsBracedEnv() = %v, want %v — an unknown carrier must fall on the side that keeps the body", name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// The AST → IR class: every declared tool property surfaces on ir.ToolNode
// ---------------------------------------------------------------------------

// toolPropertyFields maps each `tool` property of the registry to the
// ir.ToolNode field the compiler must fill from it (an embedded BaseNode or
// SchemaFields field included — FieldByName walks them). A property whose IR
// counterpart has another name is listed here; one the IR deliberately does
// not carry is in toolPropertyNotInIR, with its reason.
var toolPropertyFields = map[string]string{
	"input":           "InputSchema",
	"output":          "OutputSchema",
	"artifact_labels": "PublishLabels",
	"await":           "AwaitMode",
	"retry":           "RetryPolicy",
	"timeout":         "CallTimeout",
	"params":          "Params",
	"recovery":        "Recovery",
}

// toolPropertyNotInIR names the tool properties the compiled node does not
// carry as a field of its own, with the reason — a property that reaches the
// executor some other way, or one whose value is folded into another field.
// EMPTY today: every declared tool property is a field of ir.ToolNode.
var toolPropertyNotInIR = map[string]string{}

// toolSweepDocs are the documents the sweep unions: a tool node cannot
// declare every property at once (the three recipes are exclusive, and
// `language:` needs `script:`), so each recipe brings its own node and the
// sweep asks the registry's whole list against the union.
var toolSweepDocs = []string{
	`schema verdict:
  ok: bool

tool gate:
  description: "runs the tests"
  command: "go test ./... ${NAME:-d}"
  env_expansion: none
  input: verdict
  output: verdict
  publish: test_output
  artifact_labels: [tests]
  await: wait_all
  sandbox: auto
  compress: ultra
  permission: off
  needs: gpu
  parallel_safe: true
  goal: "the tag exists"
  postcondition: "git rev-parse --verify v1"
  policy: recover
  recovery:
    max_repair_attempts: 2
    max_agent_attempts: 1
    model: "anthropic/claude-opus-5"
    agent_tools: [bash]

workflow w:
  resources:
    gpu: 1
  entry: gate
  gate -> done
`,
	`tool gate:
  script: "print('x')"
  language: python3

workflow w:
  entry: gate
  gate -> done
`,
	`tool gate:
  action: forgejo.issue.comment
  connection: forge
  params:
    owner: SocialGouv
  retry: 3
  timeout: 30s

workflow w:
  entry: gate
  gate -> done
`,
}

// TestEveryToolPropertyReachesTheIR is the AST → IR sweep the JSON transport
// sweeps do not cover (they watch ast ↔ jsonenc): every property the
// registry declares on a `tool` node is written in one of the documents
// above and arrives non-zero on the compiled ir.ToolNode. A property wired
// through the parser, the AST and the wire but FORGOTTEN in compileTools is
// silent — the executor simply never sees it — and this is where that
// silence reddens.
func TestEveryToolPropertyReachesTheIR(t *testing.T) {
	k, ok := spec.Lookup("tool")
	if !ok {
		t.Fatal("the registry has no `tool` kind")
	}
	// filled[property] is the value seen on some document's compiled node.
	filled := map[string]bool{}
	declared := map[string]bool{}
	for _, doc := range toolSweepDocs {
		r := compileText(t, doc)
		if _, all := errorsNaming(r, ""); len(all) != 0 {
			t.Fatalf("a sweep document does not compile clean, so it proves nothing: %v\n%s", all, doc)
		}
		tn := reflect.ValueOf(toolNodeOf(t, r)).Elem()
		for _, p := range k.Properties {
			if !declaresProperty(doc, p.Name) {
				continue
			}
			declared[p.Name] = true
			if _, skip := toolPropertyNotInIR[p.Name]; skip {
				continue
			}
			name := toolPropertyFields[p.Name]
			if name == "" {
				name = goFieldName(p.Name)
			}
			f := tn.FieldByName(name)
			if !f.IsValid() {
				t.Errorf("tool.%s: ir.ToolNode has no field %q — name it in toolPropertyFields, or in toolPropertyNotInIR with the reason", p.Name, name)
				continue
			}
			if !f.IsZero() {
				filled[p.Name] = true
			}
		}
	}
	for _, p := range k.Properties {
		if _, skip := toolPropertyNotInIR[p.Name]; skip {
			continue
		}
		if !declared[p.Name] {
			t.Errorf("tool.%s is declared by none of this sweep's documents — add the line, or the registry lists a property nothing holds to the compiler", p.Name)
			continue
		}
		if !filled[p.Name] {
			t.Errorf("tool.%s is written in a sweep document but its ir.ToolNode field is zero after compiling — compileTools drops it, and the executor never sees it", p.Name)
		}
	}
}

// declaresProperty reports whether doc writes `<name>:` as a property line
// (indented), rather than as a declaration header or a value.
func declaresProperty(doc, name string) bool {
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, " ") {
			continue
		}
		if strings.TrimSpace(line) == name+":" || strings.HasPrefix(strings.TrimSpace(line), name+": ") {
			return true
		}
	}
	return false
}

// goFieldName is a property name read as the Go field it maps to by default:
// `parallel_safe` → `ParallelSafe`.
func goFieldName(prop string) string {
	parts := strings.Split(prop, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// The property is the NODE's and nothing else's (ADR-130 §1): no workflow
// value, no run override, no environment variable. The registry is where a
// second tier would have to appear first — a workflow-level `env_expansion:`
// would turn off, from outside the file that depends on it, a mechanism
// bodies READ (`${ITERION_DISPATCHER_PORT:-4891}`, the ADR-093 dial). The
// day a bot wants the whole-file guarantee it lands as a tier with its
// precedence written in advance (node > workflow > default); until then this
// is the shape the docs promise.
func TestEnvExpansionIsDeclaredOnTheToolNodeAlone(t *testing.T) {
	var hosts []string
	for _, k := range spec.Kinds {
		if k.Has("env_expansion") {
			hosts = append(hosts, k.Name)
		}
	}
	if len(hosts) != 1 || hosts[0] != "tool" {
		t.Errorf("env_expansion is declared on %v, want the tool kind alone — a second tier changes what a body MEANS from outside the body", hosts)
	}
}
