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

%s
workflow feature_dev:
  contract: %s
  entry: build
  build -> done
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
      from: build
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
	if out := c.Outputs[3]; out.File == nil || out.File.MediaType != "text/markdown" || out.FromNode != "build" {
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
		"input not a var":                      {"contract c:\n  inputs:\n    nope: string\n", DiagContractInput, "not a declared var"},
		"input of another type":                {"contract c:\n  inputs:\n    depth: string\n", DiagContractInput, "the var depth is int"},
		"input with a producer":                {"contract c:\n  inputs:\n    goal: string\n      from: build.summary\n", DiagContractInput, "carries `from:`"},
		"default on a required input":          {"contract c:\n  inputs:\n    goal: string\n      default: \"x\"\n", DiagContractInput, "has a default and is required"},
		"version 0":                            {"contract c:\n  version: 0\n", DiagContractInput, "starts at 1"},
		"duplicate input":                      {"contract c:\n  inputs:\n    goal: string\n    goal: string\n", DiagContractInput, "declared twice"},
		"items on a scalar":                    {"contract c:\n  inputs:\n    goal: string\n      min_items: 1\n", DiagContractInput, "not an array"},
		"output without a producer":            {"contract c:\n  outputs:\n    pr_url: string\n", DiagContractOutput, "names no producer"},
		"unknown producer":                     {"contract c:\n  outputs:\n    pr_url: string\n      from: nope.pr_url\n", DiagContractOutput, "does not declare"},
		"unknown field":                        {"contract c:\n  outputs:\n    pr_url: string\n      from: build.nope\n", DiagContractOutput, "has no field"},
		"output of another type":               {"contract c:\n  outputs:\n    pr_url: int\n      from: build.pr_url\n", DiagContractOutput, "build.pr_url is string"},
		"whole output of another type":         {"contract c:\n  outputs:\n    result: string\n      from: build\n", DiagContractOutput, "whole output"},
		"file port naming a field":             {"contract c:\n  outputs:\n    brief: string\n      from: build.summary\n      file:\n", DiagContractOutput, "names its node alone"},
		"default on an output":                 {"contract c:\n  outputs:\n    pr_url: string\n      from: build.pr_url\n      default: \"x\"\n", DiagContractOutput, "never defaulted"},
		"min above max":                        {"contract c:\n  outputs:\n    tags: string[]\n      from: build.tags\n      min_items: 3\n      max_items: 2\n", DiagContractOutput, "min_items 3 is above max_items 2"},
		"criterion without a kind":             {"contract c:\n  inputs:\n    goal: string\n  criteria:\n    k:\n      port: input.goal\n", DiagContractCriterion, "names no kind"},
		"criterion on an unknown port":         {"contract c:\n  criteria:\n    k:\n      kind: min_length\n      port: input.nope\n      params: {min: 1}\n", DiagContractCriterion, "no port input.nope"},
		"criterion port prefix":                {"contract c:\n  inputs:\n    goal: string\n  criteria:\n    k:\n      kind: min_length\n      port: inputs.goal\n      params: {min: 1}\n", DiagContractCriterion, "input.<name>"},
		"criterion parameters":                 {"contract c:\n  inputs:\n    goal: string\n  criteria:\n    k:\n      kind: min_length\n      port: input.goal\n      params: {min: \"x\"}\n", DiagContractCriterion, `"min" must be an integer`},
		"criterion on a type it does not take": {"contract c:\n  inputs:\n    depth: int\n  criteria:\n    k:\n      kind: min_length\n      port: input.depth\n      params: {min: 1}\n", DiagContractCriterion, "checks a string or array"},
		"default of another type":              {"contract c:\n  inputs:\n    depth: int\n      required: false\n      default: \"deep\"\n", DiagContractCriterion, "is not an int"},
		"null default not nullable":            {"contract c:\n  inputs:\n    depth: int\n      required: false\n      default: null\n", DiagContractCriterion, "not nullable"},
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
			if d.Line == 0 && name != "version 0" {
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
	if d.Line != 23 { // the criterion's own line in the fixture
		t.Fatalf("the warning is not at the criterion (line 23): %+v", d)
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
