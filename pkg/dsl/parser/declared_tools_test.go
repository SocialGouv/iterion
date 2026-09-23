package parser

import "testing"

const declaredToolsSrc = `prompt sys:
  """s"""

prompt usr:
  """u"""

judge empty_tools:
  model: "openai/gpt-5.5"
  system: sys
  user: usr
  tools: []

judge unset_tools:
  model: "openai/gpt-5.5"
  system: sys
  user: usr

judge empty_caps:
  model: "openai/gpt-5.5"
  system: sys
  user: usr
  capabilities: []

workflow w:
  entry: empty_tools
  empty_tools -> unset_tools
  unset_tools -> empty_caps
  empty_caps -> done
`

// `tools: []` is a VALUE — the author declaring that the node has no tools —
// and an absent `tools:` line is not. Reddens on the mutation that sends the
// tools arm back through parseToolList (the shared, nil-returning helper).
func TestEmptyToolListParsesDeclaredWhileAnAbsentOneStaysNil(t *testing.T) {
	pr := Parse("declared.bot", declaredToolsSrc)
	for _, d := range pr.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("unexpected parse error: %s %s", d.Code, d.Message)
		}
	}
	byName := map[string]*judgeTools{}
	for _, j := range pr.File.Judges {
		byName[j.Name] = &judgeTools{tools: j.Tools, caps: j.Capabilities}
	}
	if got := byName["empty_tools"]; got == nil || got.tools == nil {
		t.Fatalf("`tools: []` must parse to a DECLARED empty list, got %#v", got)
	} else if len(got.tools) != 0 {
		t.Fatalf("`tools: []` must be empty, got %#v", got.tools)
	}
	if got := byName["unset_tools"]; got == nil || got.tools != nil {
		t.Fatalf("an absent `tools:` must stay nil (undeclared), got %#v", got)
	}
}

// The tools arm is the ONLY list whose empty inline form is a declaration.
// `capabilities: []` must keep parseBracketList's nil, because a nil
// capability list is what makes a node INHERIT the workflow's
// (executor_build_task.go: `if effectiveCaps == nil`). Reddens on the
// mutation that widens the declared parse to every bracket list.
func TestEmptyCapabilityListStaysNilSoTheNodeStillInheritsTheWorkflows(t *testing.T) {
	pr := Parse("declared.bot", declaredToolsSrc)
	for _, j := range pr.File.Judges {
		if j.Name != "empty_caps" {
			continue
		}
		if j.Capabilities != nil {
			t.Fatalf("`capabilities: []` must stay nil (inherit), got %#v", j.Capabilities)
		}
		return
	}
	t.Fatal("empty_caps not parsed")
}

type judgeTools struct {
	tools []string
	caps  []string
}

// `[]` — nothing between the brackets — is the ONE way to declare an empty
// tool surface. A bracket from which nothing could be READ is refused
// loudly instead of silently picking a side: nil would read as an absent
// list (the CLI backend's whole toolset), and an empty slice would turn
// `tools: [*]` into "this node has no tools", rewrite the author's line to
// `tools: []` on the next `fmt`, and raise the bundle's engine floor off a
// typo — all three measured.
//
// A line with no `[` at all does not declare either: `tools: x]` is broken,
// and salvaging it into a binding `tools: []` that the studio writes back
// would invent a bound nobody typed.
func TestOnlyAnEmptyBracketDeclaresAnEmptyToolSurface(t *testing.T) {
	for _, tc := range []struct {
		line     string
		declared bool
		wantErr  bool
	}{
		{"tools: []", true, false},
		{"tools: [ ]", true, false},
		{"tools: [read_file]", true, false},
		{`tools: [""]`, false, true},
		{"tools: [,]", false, true},
		{"tools: [*]", false, true},
		{"tools: [123]", false, true},
		{"tools: x]", false, true},
	} {
		src := "prompt p:\n  \"\"\"s\"\"\"\n\njudge j:\n  model: \"m\"\n  system: p\n  user: p\n  " + tc.line + "\n\nworkflow w:\n  entry: j\n  j -> done\n"
		pr := Parse("x.bot", src)
		if len(pr.File.Judges) == 0 {
			t.Fatalf("%s: no judge parsed", tc.line)
		}
		if got := pr.File.Judges[0].Tools != nil; got != tc.declared {
			t.Errorf("%s: declared = %v, want %v (tools = %#v)", tc.line, got, tc.declared, pr.File.Judges[0].Tools)
		}
		var errs int
		for _, d := range pr.Diagnostics {
			if d.Severity == SeverityError {
				errs++
			}
		}
		if (errs > 0) != tc.wantErr {
			t.Errorf("%s: %d errors, wantErr = %v — a line the parser cannot read must never pass in silence", tc.line, errs, tc.wantErr)
		}
	}
}
