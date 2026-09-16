package parser

import (
	"strings"
	"testing"
)

const contractFixture = `schema report:
  ok: bool

contract c:
  display_name: "Feature dev"
  responsibility: "Implements a feature and opens a PR"
  version: 2
  inputs:
    goal: string
      description: "What to build"
    depth: int
      required: false
      default: 3
    labels: string[]
      min_items: 1
      max_items: 5
      nullable: true
      default: null
    model: string
    spec: report[]
    brief: string
      file:
        media_type: "text/markdown"
        min_bytes: 1
        schema: report
  outputs:
    pr_url: string
      from: open_pr.url
    artefact: string
      from: write
      file:
  criteria:
    goal_long_enough:
      kind: min_length
      port: input.goal
      params: {min: 2, "quoted key": [true, 1.5, "x", null], nested: {}}
    declared_only:
  effects:
    opens_pr:
      description: "Opens a pull request"
      paid: true
    silent:

workflow w:
  contract: c
  entry: a
  a -> done

agent a:
  description: "d"
`

// Every property of the contract surface reads into the AST: ports in
// order with their type, an explicit `required: false`, the three states
// of a default (absent, null, a value), the file block (empty or not), the
// producer of an output, criteria with JSON parameters, effects — and a
// port may bear a keyword's name. The workflow names its contract.
func TestContractDeclarationIsRead(t *testing.T) {
	res := Parse("x.bot", contractFixture)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("diagnostics: %v", res.Diagnostics)
	}
	if len(res.File.Contracts) != 1 {
		t.Fatalf("%d contracts", len(res.File.Contracts))
	}
	c := res.File.Contracts[0]
	if c.Name != "c" || c.DisplayName != "Feature dev" || c.Responsibility != "Implements a feature and opens a PR" || c.Version == nil || *c.Version != 2 {
		t.Errorf("header read as %+v", c)
	}
	if c.Span.Start.Line != 4 || c.Span.Start.File != "x.bot" {
		t.Errorf("the contract's span starts at %+v", c.Span.Start)
	}
	var names, types []string
	for _, p := range c.Inputs {
		names = append(names, p.Name)
		types = append(types, p.Type)
	}
	if got := strings.Join(names, ","); got != "goal,depth,labels,model,spec,brief" {
		t.Errorf("inputs %s", got)
	}
	if got := strings.Join(types, ","); got != "string,int,string[],string,report[],string" {
		t.Errorf("input types %s", got)
	}
	goal, depth, labels, brief := c.Inputs[0], c.Inputs[1], c.Inputs[2], c.Inputs[5]
	if goal.Description != "What to build" || goal.Default != nil || goal.Required != nil || !goal.IsRequired() {
		t.Errorf("goal read as %+v", goal)
	}
	if depth.Required == nil || *depth.Required || depth.IsRequired() || string(depth.Default) != "3" {
		t.Errorf("depth read as required=%v default=%s", depth.Required, depth.Default)
	}
	if string(labels.Default) != "null" || !labels.Nullable || labels.MinItems == nil || *labels.MinItems != 1 || labels.MaxItems == nil || *labels.MaxItems != 5 {
		t.Errorf("labels read as %+v", labels)
	}
	if brief.FileSpec == nil || brief.FileSpec.MediaType != "text/markdown" || brief.FileSpec.MinBytes != 1 || brief.FileSpec.Schema != "report" {
		t.Errorf("brief's file block read as %+v", brief.FileSpec)
	}
	if len(c.Outputs) != 2 || c.Outputs[0].From != "open_pr.url" || c.Outputs[1].From != "write" {
		t.Errorf("outputs read as %+v", c.Outputs)
	}
	if fs := c.Outputs[1].FileSpec; fs == nil || fs.MediaType != "" || fs.MinBytes != 0 || fs.Schema != "" {
		t.Errorf("the empty file block read as %+v", fs)
	}
	if len(c.Criteria) != 2 {
		t.Fatalf("%d criteria", len(c.Criteria))
	}
	if k := c.Criteria[0]; k.Name != "goal_long_enough" || k.Kind != "min_length" || k.Port != "input.goal" || string(k.Params) != `{"min":2,"nested":{},"quoted key":[true,1.5,"x",null]}` {
		t.Errorf("criterion read as %+v params %s", k, k.Params)
	}
	if k := c.Criteria[1]; k.Name != "declared_only" || k.Kind != "" || k.Port != "" || k.Params != nil {
		t.Errorf("the bare criterion read as %+v", k)
	}
	if len(c.Effects) != 2 || c.Effects[0].Name != "opens_pr" || !c.Effects[0].Paid || c.Effects[0].Description != "Opens a pull request" || c.Effects[1].Name != "silent" || c.Effects[1].Paid {
		t.Errorf("effects read as %+v", c.Effects)
	}
	if res.File.Workflows[0].Contract != "c" {
		t.Errorf("the workflow's contract read as %q", res.File.Workflows[0].Contract)
	}
}

// A bare header is an empty contract, at the end of the file or before a
// blank line — the rule every named declaration follows.
func TestAnEmptyContractIsABareHeader(t *testing.T) {
	for name, src := range map[string]string{
		"at EOF":             "contract c:\n",
		"before a blank":     "contract c:\n\nagent a:\n  description: \"d\"\n",
		"empty inputs block": "contract c:\n  inputs:\n\nagent a:\n  description: \"d\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			res := Parse("x.bot", src)
			if len(res.Diagnostics) != 0 {
				t.Fatalf("diagnostics: %v", res.Diagnostics)
			}
			if len(res.File.Contracts) != 1 || res.File.Contracts[0].Name != "c" || len(res.File.Contracts[0].Inputs) != 0 {
				t.Fatalf("contracts: %+v", res.File.Contracts)
			}
		})
	}
}

// Each refusal is reported where it stands, with its code, and the rest
// of the file is still read.
func TestContractRefusals(t *testing.T) {
	tail := "\nagent a:\n  description: \"d\"\n"
	for name, tc := range map[string]struct {
		src  string
		code DiagCode
		text string
	}{
		"unknown contract property":          {"contract c:\n  nope: 1\n", DiagUnknownProperty, "nope"},
		"unknown port property":              {"contract c:\n  inputs:\n    x: string\n      nope: 1\n", DiagUnknownProperty, "nope"},
		"unknown file property":              {"contract c:\n  inputs:\n    x: string\n      file:\n        nope: 1\n", DiagUnknownProperty, "nope"},
		"unknown criterion property":         {"contract c:\n  criteria:\n    k:\n      nope: 1\n", DiagUnknownProperty, "nope"},
		"unknown effect property":            {"contract c:\n  effects:\n    e:\n      nope: 1\n", DiagUnknownProperty, "nope"},
		"duplicate property":                 {"contract c:\n  version: 1\n  version: 2\n", DiagDuplicateBlock, "version"},
		"duplicate JSON key":                 {"contract c:\n  criteria:\n    k:\n      params: {a: 1, a: 2}\n", DiagDuplicateBlock, "a"},
		"not a JSON value":                   {"contract c:\n  inputs:\n    x: string\n      default: foo\n", DiagExpectedToken, "JSON value"},
		"not a bool":                         {"contract c:\n  inputs:\n    x: string\n      required: 1\n", DiagExpectedToken, "true or false"},
		"not a member":                       {"contract c:\n  \"x\"\n", DiagExpectedToken, "named contract member"},
		"reserved name":                      {"contract done:\n  version: 1\n", DiagReservedName, "done"},
		"workflow contract is an identifier": {"workflow w:\n  contract: \"c\"\n  entry: a\n  a -> done\n", DiagExpectedToken, ""},
	} {
		t.Run(name, func(t *testing.T) {
			res := Parse("x.bot", tc.src+tail)
			var found bool
			for _, d := range res.Diagnostics {
				if d.Code == tc.code && strings.Contains(d.Message, tc.text) {
					found = true
				}
			}
			if !found {
				t.Fatalf("no %s mentioning %q in %v", tc.code, tc.text, res.Diagnostics)
			}
			if len(res.File.Agents) != 1 {
				t.Fatalf("the declaration after the refusal was lost: %+v", res.File.Agents)
			}
		})
	}
}

// The text has no signed number and no exponent: a default written with
// one does not parse. What the transport may carry, and the compiler
// refuses at C302, is WritableJSONValue's rule; here the lexer's.
func TestAContractDefaultTheTextCannotWriteDoesNotParse(t *testing.T) {
	for _, value := range []string{"-1", "1e3", "[1, -2]"} {
		res := Parse("x.bot", "contract c:\n  inputs:\n    x: int\n      default: "+value+"\n")
		var errors int
		for _, d := range res.Diagnostics {
			if d.Severity == SeverityError {
				errors++
			}
		}
		if errors == 0 {
			t.Errorf("default: %s parsed, though the text has no such number", value)
		}
	}
}

// A scalar contract value is one value on its line. A container left open
// at the end of its line stops there — the declarations after it are read
// — and a number the lexer reads in two pieces (`1e3` is 1 and the
// identifier e3), a stray token, a dotted type or schema, are refused at
// the tail with nothing stored: never a truncated value read on as the next
// property.
func TestAContractValueEndsWithItsLine(t *testing.T) {
	tail := "\nagent a:\n  model: \"m\"\n\ntool t:\n  command: \"echo\"\n\nworkflow w:\n  entry: a\n  a -> done\n"
	for name, tc := range map[string]struct {
		body string // the port's lines under `r: json`
		text string
	}{
		"open object":              {"      default: {\n", "expected a JSON object key"},
		"open object after member": {"      default: {\"a\": 1,\n", "expected a JSON object key"},
		"open array":               {"      default: [\n", "expected a JSON value or ']'"},
		"exponent":                 {"      default: 1e3\n", "no signed number and no exponent"},
		"trailing dot":             {"      default: 1.\n", "no signed number and no exponent"},
		"two values":               {"      default: 1 2\n", "after the value"},
		"int with a tail":          {"      min_items: 1e2\n", "min_items takes one value on its line"},
		"dotted schema":            {"      file:\n        schema: a.b\n", "schema takes one value on its line"},
	} {
		t.Run(name, func(t *testing.T) {
			res := Parse("x.bot", "contract c:\n  inputs:\n    r: json\n"+tc.body+tail)
			var errors []string
			for _, d := range res.Diagnostics {
				if d.Severity == SeverityError {
					errors = append(errors, d.Error())
				}
			}
			if len(errors) != 1 || !strings.Contains(errors[0], tc.text) {
				t.Fatalf("want one error mentioning %q, got %v", tc.text, errors)
			}
			if len(res.File.Agents) != 1 || len(res.File.Tools) != 1 || len(res.File.Workflows) != 1 {
				t.Fatalf("the declarations after the refusal were lost: %d agents, %d tools, %d workflows", len(res.File.Agents), len(res.File.Tools), len(res.File.Workflows))
			}
			port := res.File.Contracts[0].Inputs[0]
			if port.Default != nil || port.MinItems != nil || (port.FileSpec != nil && port.FileSpec.Schema != "") {
				t.Fatalf("a truncated value was stored: %+v", port)
			}
		})
	}
	// A dotted type is refused at the port the same way.
	res := Parse("x.bot", "contract c:\n  inputs:\n    r: a.b\n"+tail)
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "a port's type takes one value") || len(res.File.Workflows) != 1 {
		t.Fatalf("dotted type: %v", res.Diagnostics)
	}
}

// A duplicate property or JSON key carries a remedy for that site, not the
// top-level blocks' one E014 was born with; a criterion's kind may be
// dotted (a plugin's evaluator).
func TestContractDuplicateRemedyAndDottedKind(t *testing.T) {
	for name, src := range map[string]string{
		"property": "contract c:\n  version: 1\n  version: 2\n",
		"JSON key": "contract c:\n  criteria:\n    k:\n      params: {a: 1, a: 2}\n",
	} {
		res := Parse("x.bot", src)
		var found bool
		for _, d := range res.Diagnostics {
			if d.Code == DiagDuplicateBlock {
				found = true
				if !strings.Contains(d.Hint, "keep one and delete the other") {
					t.Errorf("%s: the E014 hint is the top-level blocks' one: %q", name, d.Hint)
				}
			}
		}
		if !found {
			t.Errorf("%s: no E014 in %v", name, res.Diagnostics)
		}
	}
	res := Parse("x.bot", "contract c:\n  criteria:\n    k:\n      kind: plugin.check\n")
	if len(res.Diagnostics) != 0 || res.File.Contracts[0].Criteria[0].Kind != "plugin.check" {
		t.Fatalf("dotted kind: %v %+v", res.Diagnostics, res.File.Contracts[0].Criteria[0])
	}
}

// A port with no type but with properties is one error at the type, its
// properties still read (the failed read consumed the line's end, and the
// line rule accepts the indent that follows); a workflow's `contract:` is
// held to its line like the contract's own values, and the members after
// it are kept.
func TestATypelessPortAndTheWorkflowContractLine(t *testing.T) {
	res := Parse("x.bot", "contract c:\n  inputs:\n    r:\n      description: \"d\"\n\nagent a:\n  description: \"d\"\n")
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagExpectedToken || res.Diagnostics[0].Line != 3 {
		t.Fatalf("typeless port: %v", res.Diagnostics)
	}
	if p := res.File.Contracts[0].Inputs[0]; p.Type != "" || p.Description != "d" {
		t.Fatalf("the port's properties were lost: %+v", p)
	}
	if len(res.File.Agents) != 1 {
		t.Fatal("the declaration after the contract was lost")
	}
	res = Parse("x.bot", "workflow w:\n  contract: c extra\n  entry: a\n  a -> done\n\nagent a:\n  description: \"d\"\n")
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "contract takes one value on its line") {
		t.Fatalf("workflow contract with a tail: %v", res.Diagnostics)
	}
	if w := res.File.Workflows[0]; w.Contract != "" || w.Entry != "a" || len(w.Edges) != 1 {
		t.Fatalf("the members after the refused line were lost: contract=%q entry=%q edges=%d", w.Contract, w.Entry, len(w.Edges))
	}
}
