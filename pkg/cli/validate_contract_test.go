package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const validateContractBot = "vars:\n  goal: string\n\nschema report:\n  pr_url: string\n\nprompt u:\n  Do {{vars.goal}}.\n\nagent build:\n  model: \"anthropic/claude-sonnet-4-6\"\n  user: u\n  output: report\n\ncontract feature:\n  responsibility: \"Implements a feature\"\n  version: 1\n  inputs:\n    goal: string\n  outputs:\n    pr_url: string\n      from: build.pr_url\n  criteria:\n    k:\n      kind: min_length\n      port: input.goal\n      params: {min: 1}\n  effects:\n    opens_pr:\n      paid: true\n\nworkflow w:\n  contract: feature\n  entry: build\n  build -> done\n"

// `iterion validate` shows the contract the workflow keeps, as the compiler
// bound it — in the JSON result (what the MCP local_validate tool returns)
// and in the human rendering — and shows none of a program that does not
// compile: a view is never given of a contract nothing keeps.
func TestRunValidate_ShowsTheBoundContract(t *testing.T) {
	inTempWorkspace(t)
	if err := os.WriteFile("c.bot", []byte(validateContractBot), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out := jsonPrinter()
	if err := RunValidate("c.bot", jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	var res ValidateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("the JSON result does not decode: %v\n%s", err, out.String())
	}
	c := res.PublicContract
	if c == nil || c.Name != "feature" || len(c.Outputs) != 1 || c.Outputs[0].FromNode != "build" || c.Outputs[0].FromField != "pr_url" || !c.Criteria[0].Registered {
		t.Fatalf("the public contract came out as %+v\n%s", c, out.String())
	}
	// The wire form is snake_case like every other key of the result, and an
	// absent default is no key while an explicit null is `null`.
	for _, want := range []string{`"public_contract"`, `"from_node": "build"`, `"from_field": "pr_url"`, `"registered": true`, `"required": true`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the JSON result lacks %s:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), `"FromNode"`) || strings.Contains(out.String(), `"default"`) {
		t.Errorf("the JSON result carries a Go field name or a default nobody declared:\n%s", out.String())
	}
	withNull := strings.Replace(validateContractBot, "  inputs:\n    goal: string\n", "  inputs:\n    goal: string\n    note: string\n      required: false\n      nullable: true\n      default: null\n", 1)
	withNull = strings.Replace(withNull, "vars:\n  goal: string\n", "vars:\n  goal: string\n  note: string\n", 1)
	if err := os.WriteFile("null.bot", []byte(withNull), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out = jsonPrinter()
	if err := RunValidate("null.bot", jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if strings.Count(out.String(), `"default": null`) != 1 {
		t.Errorf("the explicit null default is not the one `\"default\": null` of the result:\n%s", out.String())
	}
	// An input whose var has a default is optional with that default in
	// the view, written on the port or not: the wire carries the var's word.
	mirrored := strings.Replace(validateContractBot, "  inputs:\n    goal: string\n", "  inputs:\n    goal: string\n    note: string\n      nullable: true\n", 1)
	mirrored = strings.Replace(mirrored, "vars:\n  goal: string\n", "vars:\n  goal: string\n  note: string = \"n/a\"\n", 1)
	if err := os.WriteFile("mirrored.bot", []byte(mirrored), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out = jsonPrinter()
	if err := RunValidate("mirrored.bot", jp); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if s := out.String(); strings.Count(s, `"default": "n/a"`) != 1 || strings.Count(s, `"required": false`) != 1 {
		t.Errorf("the var's default is not the port's on the wire:\n%s", s)
	}
	hp, hout := testPrinter()
	if err := RunValidate("c.bot", hp); err != nil {
		t.Fatalf("validate: %v\n%s", err, hout.String())
	}
	for _, want := range []string{"feature v1 — Implements a feature", "goal: string", "pr_url: string ← build.pr_url", `k: min_length {"min":1} on input.goal`, "opens_pr (paid)"} {
		if !strings.Contains(hout.String(), want) {
			t.Errorf("the rendering lacks %q:\n%s", want, hout.String())
		}
	}

	broken := strings.Replace(validateContractBot, "from: build.pr_url", "from: build.nope", 1)
	if err := os.WriteFile("broken.bot", []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out = jsonPrinter()
	if err := RunValidate("broken.bot", jp); err == nil {
		t.Fatalf("an unbound contract validated:\n%s", out.String())
	}
	var refused ValidateResult
	if err := json.Unmarshal(out.Bytes(), &refused); err != nil {
		t.Fatalf("the JSON result does not decode: %v\n%s", err, out.String())
	}
	if refused.PublicContract != nil || !strings.Contains(out.String(), "C301") {
		t.Fatalf("a program that does not compile was given a public view:\n%s", out.String())
	}
}
