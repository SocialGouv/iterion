package ir

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A program with a var of each shape a port can bind to, a node whose
// output schema has a field of each type, and a workflow naming the
// contract the fixture supplies.
const contractProgramFmt = `vars:
  goal: string
  depth: int

schema report:
  summary: string
  pr_url: string
  tags: string[]
  score: float

prompt build_user:
  Implement {{vars.goal}}.

agent build:
  model: "anthropic/claude-sonnet-4-6"
  user: build_user
  output: report

agent publisher:
  model: "anthropic/claude-sonnet-4-6"
  user: build_user
  output: report
  publish: brief_doc

%s
workflow feature_dev:
  contract: %s
  entry: build
  build -> publisher
  publisher -> done
`

func compileContractProgram(t *testing.T, contract, named string) *CompileResult {
	t.Helper()
	src := fmt.Sprintf(contractProgramFmt, contract, named)
	pr := parser.Parse("x.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("fixture does not parse: %s\n%s", d.Error(), src)
		}
	}
	return Compile(pr.File)
}

func diagWith(cr *CompileResult, code DiagCode, text string) *Diagnostic {
	for i := range cr.Diagnostics {
		d := &cr.Diagnostics[i]
		if d.Code == code && strings.Contains(d.Message, text) {
			return d
		}
	}
	return nil
}

func errorCodes(cr *CompileResult) []string {
	var codes []string
	for _, d := range cr.Diagnostics {
		if d.Severity == SeverityError {
			codes = append(codes, string(d.Code))
		}
	}
	return codes
}

// A contract the program keeps compiles clean and comes out bound: every
// input to its var, every output to its producer, the whole-output and the
// file forms included, the criterion to its evaluator.
func TestAContractBoundToTheProgramCompiles(t *testing.T) {
	cr := compileContractProgram(t, `contract feature:
  display_name: "Feature dev"
  responsibility: "Implements a feature"
  version: 2
  inputs:
    goal: string
      description: "What to build"
    depth: int
      required: false
      default: 2
  outputs:
    pr_url: string
      from: build.pr_url
    tags: string[]
      from: build.tags
      min_items: 0
      max_items: 5
    result: report
      from: build
    brief: string
      from: publisher
      file:
        media_type: "text/markdown"
  criteria:
    goal_long_enough:
      kind: min_length
      port: input.goal
      params: {min: 2}
    tagged:
      kind: min_length
      port: output.tags
      params: {min: 1}
    url_shape:
      kind: pattern
      port: output.pr_url
      params: {pattern: "^https://"}
  effects:
    opens_pr:
      description: "Opens a pull request"
      paid: true
`, "feature")
	if codes := errorCodes(cr); len(codes) != 0 {
		t.Fatalf("errors: %v — %v", codes, cr.Diagnostics)
	}
	c := cr.Workflow.Contract
	if c == nil || c.Name != "feature" || c.Version != 2 || cr.Workflow.Contracts["feature"] != c {
		t.Fatalf("the workflow's contract came out as %+v", c)
	}
	if in := c.Inputs[1]; in.Name != "depth" || in.Required || string(in.Default) != "2" {
		t.Errorf("depth came out as %+v", in)
	}
	if out := c.Outputs[0]; out.FromNode != "build" || out.FromField != "pr_url" {
		t.Errorf("pr_url came out as %+v", out)
	}
	if out := c.Outputs[2]; out.FromNode != "build" || out.FromField != "" || out.Type != "report" {
		t.Errorf("the whole-output port came out as %+v", out)
	}
	if out := c.Outputs[3]; out.File == nil || out.File.MediaType != "text/markdown" || out.FromNode != "publisher" {
		t.Errorf("the file port came out as %+v", out)
	}
	for _, k := range c.Criteria {
		if !k.Registered {
			t.Errorf("criterion %s is not registered", k.Name)
		}
	}
	if len(c.Effects) != 1 || !c.Effects[0].Paid {
		t.Errorf("effects came out as %+v", c.Effects)
	}
}

// Every way a contract can fail to describe the program, each refused with
// its code and a message that names the fix, at the port's own line.
func TestAContractTheProgramDoesNotKeepIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		contract string
		code     DiagCode
		text     string
	}{
		"input not a var":                         {"contract c:\n  inputs:\n    nope: string\n", DiagContractInput, "not a declared var"},
		"input of another type":                   {"contract c:\n  inputs:\n    depth: string\n", DiagContractInput, "the var depth is int"},
		"input with a producer":                   {"contract c:\n  inputs:\n    goal: string\n      from: build.summary\n", DiagContractInput, "carries `from:`"},
		"default on a required input":             {"contract c:\n  inputs:\n    goal: string\n      default: \"x\"\n", DiagContractInput, "has a default and is required"},
		"version 0":                               {"contract c:\n  version: 0\n", DiagContractInput, "starts at 1"},
		"duplicate input":                         {"contract c:\n  inputs:\n    goal: string\n    goal: string\n", DiagContractInput, "declared twice"},
		"items on a scalar":                       {"contract c:\n  inputs:\n    goal: string\n      min_items: 1\n", DiagContractInput, "not an array"},
		"output without a producer":               {"contract c:\n  outputs:\n    pr_url: string\n", DiagContractOutput, "names no producer"},
		"unknown producer":                        {"contract c:\n  outputs:\n    pr_url: string\n      from: nope.pr_url\n", DiagContractOutput, "does not declare"},
		"unknown field":                           {"contract c:\n  outputs:\n    pr_url: string\n      from: build.nope\n", DiagContractOutput, "has no field"},
		"output of another type":                  {"contract c:\n  outputs:\n    pr_url: int\n      from: build.pr_url\n", DiagContractOutput, "build.pr_url is string"},
		"whole output of another type":            {"contract c:\n  outputs:\n    result: string\n      from: build\n", DiagContractOutput, "whole output"},
		"file port naming a field":                {"contract c:\n  outputs:\n    brief: string\n      from: publisher.summary\n      file:\n", DiagContractOutput, "names its node alone"},
		"file port on a node that publishes none": {"contract c:\n  outputs:\n    brief: string\n      from: build\n      file:\n", DiagContractOutput, "publishes no file"},
		"file port on done":                       {"contract c:\n  outputs:\n    brief: string\n      from: done\n      file:\n", DiagContractOutput, "publishes no file"},
		"file schema not declared":                {"contract c:\n  outputs:\n    brief: string\n      from: publisher\n      file:\n        schema: nope\n", DiagContractOutput, `file schema "nope" is not a declared schema`},
		"input with a file":                       {"contract c:\n  inputs:\n    goal: string\n      file:\n", DiagContractInput, "declares file:"},
		"criterion on a file port":                {"contract c:\n  outputs:\n    brief: string\n      from: publisher\n      file:\n  criteria:\n    k:\n      kind: pattern\n      port: output.brief\n      params: {pattern: \"x\"}\n", DiagContractCriterion, "port output.brief is a file"},
		"default on an output":                    {"contract c:\n  outputs:\n    pr_url: string\n      from: build.pr_url\n      default: \"x\"\n", DiagContractOutput, "never defaulted"},
		"min above max":                           {"contract c:\n  outputs:\n    tags: string[]\n      from: build.tags\n      min_items: 3\n      max_items: 2\n", DiagContractOutput, "min_items 3 is above max_items 2"},
		"criterion without a kind":                {"contract c:\n  inputs:\n    goal: string\n  criteria:\n    k:\n      port: input.goal\n", DiagContractCriterion, "names no kind"},
		"criterion on an unknown port":            {"contract c:\n  criteria:\n    k:\n      kind: min_length\n      port: input.nope\n      params: {min: 1}\n", DiagContractCriterion, "no port input.nope"},
		"criterion port prefix":                   {"contract c:\n  inputs:\n    goal: string\n  criteria:\n    k:\n      kind: min_length\n      port: inputs.goal\n      params: {min: 1}\n", DiagContractCriterion, "input.<name>"},
		"criterion parameters":                    {"contract c:\n  inputs:\n    goal: string\n  criteria:\n    k:\n      kind: min_length\n      port: input.goal\n      params: {min: \"x\"}\n", DiagContractCriterion, `"min" must be an integer`},
		"criterion on a type it does not take":    {"contract c:\n  inputs:\n    depth: int\n  criteria:\n    k:\n      kind: min_length\n      port: input.depth\n      params: {min: 1}\n", DiagContractCriterion, "checks a string or array"},
		"default of another type":                 {"contract c:\n  inputs:\n    depth: int\n      required: false\n      default: \"deep\"\n", DiagContractCriterion, "is not an int"},
		"null default not nullable":               {"contract c:\n  inputs:\n    depth: int\n      required: false\n      default: null\n", DiagContractCriterion, "not nullable"},
	} {
		t.Run(name, func(t *testing.T) {
			cr := compileContractProgram(t, tc.contract, "c")
			d := diagWith(cr, tc.code, tc.text)
			if d == nil {
				t.Fatalf("no %s mentioning %q; diagnostics: %v", tc.code, tc.text, cr.Diagnostics)
			}
			if d.Severity != SeverityError {
				t.Fatalf("%s is a %s, want an error", tc.code, d.Severity)
			}
			if d.Line == 0 {
				t.Errorf("the refusal carries no line: %+v", d)
			}
		})
	}
}

// A contract declared twice, and a workflow naming a contract nobody
// declares, are refused by name.
func TestContractNamesAreHeld(t *testing.T) {
	cr := compileContractProgram(t, "contract c:\n  version: 1\n\ncontract c:\n  version: 2\n", "c")
	if diagWith(cr, DiagContractInput, "declared twice") == nil {
		t.Fatalf("a duplicate contract passed: %v", cr.Diagnostics)
	}
	cr = compileContractProgram(t, "contract c:\n  version: 1\n", "nope")
	if diagWith(cr, DiagContractInput, `names contract "nope"`) == nil {
		t.Fatalf("an unknown contract name passed: %v", cr.Diagnostics)
	}
	if cr.Workflow.Contract != nil || len(cr.Workflow.Contracts) != 1 {
		t.Fatalf("contracts came out as %+v / %+v", cr.Workflow.Contract, cr.Workflow.Contracts)
	}
}

// A criterion of an unregistered kind is declared, rendered and not
// evaluated: a warning names the kinds this build ships, the contract still
// compiles, and the criterion says it is not registered.
func TestAnUnregisteredCriterionKindIsAWarning(t *testing.T) {
	cr := compileContractProgram(t, "contract c:\n  inputs:\n    goal: string\n  criteria:\n    k:\n      kind: plugin.check\n      port: input.goal\n      params: {x: 1}\n", "c")
	if codes := errorCodes(cr); len(codes) != 0 {
		t.Fatalf("errors: %v", codes)
	}
	d := diagWith(cr, DiagContractUnknownKind, "min_length, pattern")
	if d == nil || d.Severity != SeverityWarning {
		t.Fatalf("no C303 warning naming the registered kinds: %v", cr.Diagnostics)
	}
	src := fmt.Sprintf(contractProgramFmt, "contract c:\n  inputs:\n    goal: string\n  criteria:\n    k:\n      kind: plugin.check\n      port: input.goal\n      params: {x: 1}\n", "c")
	criterionLine := 1 + strings.Count(src[:strings.Index(src, "    k:\n")], "\n")
	if d.Line != criterionLine {
		t.Fatalf("the warning is not at the criterion (line %d): %+v", criterionLine, d)
	}
	if k := cr.Workflow.Contract.Criteria[0]; k.Registered || k.Kind != "plugin.check" {
		t.Fatalf("the criterion came out as %+v", k)
	}
}

// A value the transport accepts and the text cannot write — a signed
// number — is refused where it is declared, so the writer never meets it.
func TestAContractDefaultTheTextCannotWriteIsRefusedAtCompile(t *testing.T) {
	src := fmt.Sprintf(contractProgramFmt, "contract c:\n  inputs:\n    depth: int\n      required: false\n      default: 1\n", "c")
	pr := parser.Parse("x.bot", src)
	pr.File.Contracts[0].Inputs[0].Default = json.RawMessage("-1")
	cr := Compile(pr.File)
	if diagWith(cr, DiagContractCriterion, "no signed number and no exponent") == nil {
		t.Fatalf("a signed default passed compilation: %v", cr.Diagnostics)
	}
}

// The compiled program carries the contract: two programs whose contracts
// differ are not the same program, and the difference is named.
func TestSameProgramSeesTheContract(t *testing.T) {
	a := compileContractProgram(t, "contract c:\n  inputs:\n    depth: int\n      required: false\n      default: 1\n", "c")
	b := compileContractProgram(t, "contract c:\n  inputs:\n    depth: int\n      required: false\n      default: 2\n", "c")
	if why := SameProgram(a, b); why != "contracts differ" {
		t.Fatalf("SameProgram: %q, want the contracts named", why)
	}
	if why := SameProgram(a, compileContractProgram(t, "contract c:\n  inputs:\n    depth: int\n      required: false\n      default: 1\n", "c")); why != "" {
		t.Fatalf("the same contract reads as different: %s", why)
	}
}

// A producer that is an instance of a group is named `<prefix>.<node>`:
// `from:` resolves the longest node id before the field, so the instance
// binds; an unknown producer names the instance rule.
func TestAContractBindsToAGroupInstance(t *testing.T) {
	src := "vars:\n  goal: string\n\nschema report:\n  summary: string\n\nprompt inspect_prompt:\n  Inspect {{vars.goal}}.\n\ngroup check(rule):\n  agent inspect:\n    model: \"anthropic/claude-sonnet-4-6\"\n    user: inspect_prompt\n    output: report\n\nuse check as security with {\n  rule: \"security\"\n}\n\ncontract c:\n  inputs:\n    goal: string\n  outputs:\n    summary: string\n      from: security.inspect.summary\n\nworkflow grouped:\n  contract: c\n  entry: security.inspect\n  security.inspect -> done\n"
	pr := parser.Parse("x.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("fixture: %s", d.Error())
		}
	}
	cr := Compile(pr.File)
	if codes := errorCodes(cr); len(codes) != 0 {
		t.Fatalf("errors: %v — %v", codes, cr.Diagnostics)
	}
	if out := cr.Workflow.Contract.Outputs[0]; out.FromNode != "security.inspect" || out.FromField != "summary" {
		t.Fatalf("the instance binding came out as %+v", out)
	}
	cr = Compile(parser.Parse("x.bot", strings.Replace(src, "from: security.inspect.summary", "from: security.nope.summary", 1)).File)
	if diagWith(cr, DiagContractOutput, "an instance of a group is named") == nil {
		t.Fatalf("an unknown instance was not named: %v", cr.Diagnostics)
	}
}

// A node whose output shape is built in (`await_answers` → answers) binds
// through its implicit field, typed json; a port of another type, or a
// field it does not have, is refused by name.
func TestAContractBindsToAnImplicitOutputField(t *testing.T) {
	base := "vars:\n  goal: string\n\nschema out:\n  text: string\n\nprompt work:\n  Ask about {{vars.goal}}.\n\nprompt reader:\n  The answers were {{outputs.gate.answers}}.\n\nagent asker:\n  model: \"anthropic/claude-sonnet-4-6\"\n  user: work\n  output: out\n  interaction: async\n\nawait_answers gate:\n  from: asker\n  timeout: \"10m\"\n\nagent reads:\n  model: \"anthropic/claude-sonnet-4-6\"\n  user: reader\n  output: out\n\ncontract c:\n  inputs:\n    goal: string\n  outputs:\n    answers: %s\n      from: %s\n\nworkflow w:\n  contract: c\n  entry: asker\n  asker -> gate\n  gate -> reads\n  reads -> done\n"
	compile := func(typ, from string) *CompileResult {
		pr := parser.Parse("x.bot", fmt.Sprintf(base, typ, from))
		return Compile(pr.File)
	}
	if cr := compile("json", "gate.answers"); len(errorCodes(cr)) != 0 || cr.Workflow.Contract.Outputs[0].FromField != "answers" {
		t.Fatalf("the implicit field did not bind: %v", cr.Diagnostics)
	}
	if cr := compile("string", "gate.answers"); diagWith(cr, DiagContractOutput, "the port takes json") == nil {
		t.Fatalf("a string port on the implicit field passed: %v", cr.Diagnostics)
	}
	if cr := compile("json", "gate.nope"); diagWith(cr, DiagContractOutput, "it has answers") == nil {
		t.Fatalf("an unknown implicit field passed: %v", cr.Diagnostics)
	}
	if cr := compile("json", "gate"); diagWith(cr, DiagContractOutput, "name its field") == nil {
		t.Fatalf("a whole-output binding on an implicit node passed: %v", cr.Diagnostics)
	}
}

// An output whose producer is on no path to done is produced only when the
// bot fails: a warning names the node, and the contract still compiles.
func TestAnOutputProducedOnlyOnFailureIsAWarning(t *testing.T) {
	src := "vars:\n  goal: string\n\nschema report:\n  summary: string\n  ok: bool\n\nprompt build_user:\n  Implement {{vars.goal}}.\n\nprompt rescue_user:\n  Explain the failure.\n\nagent build:\n  model: \"anthropic/claude-sonnet-4-6\"\n  user: build_user\n  output: report\n\nagent rescue:\n  model: \"anthropic/claude-sonnet-4-6\"\n  user: rescue_user\n  output: report\n\ncontract c:\n  inputs:\n    goal: string\n  outputs:\n    summary: string\n      from: %s.summary\n\nworkflow w:\n  contract: c\n  entry: build\n  build -> done when ok\n  build -> rescue else\n  rescue -> fail\n"
	cr := Compile(parser.Parse("x.bot", fmt.Sprintf(src, "rescue")).File)
	if len(errorCodes(cr)) != 0 {
		t.Fatalf("errors: %v", cr.Diagnostics)
	}
	d := diagWith(cr, DiagContractOutputOffSuccess, `node "rescue" is on no path to done`)
	if d == nil || d.Severity != SeverityWarning || d.Line == 0 {
		t.Fatalf("no positioned C304 warning: %v", cr.Diagnostics)
	}
	if cr := Compile(parser.Parse("x.bot", fmt.Sprintf(src, "build")).File); diagWith(cr, DiagContractOutputOffSuccess, "") != nil {
		t.Fatalf("a producer on the success path drew C304: %v", cr.Diagnostics)
	}
}

// An enum-constrained var carries its domain onto the contract's input,
// so no reader is told a wider one; the unknown-contract refusal is at the
// workflow's own line.
func TestAContractCarriesTheVarEnumAndPositionsTheWorkflowRefusal(t *testing.T) {
	src := "vars:\n  mode: string [enum: \"fast\", \"slow\"]\n\ncontract c:\n  inputs:\n    mode: string\n\nagent a:\n  model: \"m\"\n  description: \"d\"\n\nworkflow w:\n  contract: %s\n  entry: a\n  a -> done\n"
	cr := Compile(parser.Parse("x.bot", fmt.Sprintf(src, "c")).File)
	if got := cr.Workflow.Contract.Inputs[0].EnumValues; len(got) != 2 || got[0] != "fast" {
		t.Fatalf("the enum came out as %v (%v)", got, cr.Diagnostics)
	}
	cr = Compile(parser.Parse("x.bot", fmt.Sprintf(src, "nope")).File)
	d := diagWith(cr, DiagContractInput, `names contract "nope"`)
	if d == nil || d.Line != 12 {
		t.Fatalf("the workflow refusal is not at the workflow's line (12): %+v", d)
	}
}
