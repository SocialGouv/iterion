package parser

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// A list that is not written as one — a value with no `[`, a line ended on
// a dangling comma — is said where it stands and takes its line: the NEXT
// property is read as itself, never as the list's elements. Every element
// reader consumes at least one token, so the line end must never reach one.
func TestAListWithoutItsBracketNeverReadsTheNextProperty(t *testing.T) {
	nl := string(rune(10))
	head := strings.Join([]string{"workflow w:", "  entry: done", "  sandbox:"}, nl) + nl
	for _, c := range []struct {
		name, mounts string
		wantMounts   []string
		wantMessage  string
	}{
		{"a value with no bracket", `    mounts: "/x:/x"`, nil, "expected ["},
		{"a dangling comma", `    mounts: ["/x:/x",`, []string{"/x:/x"}, "expected ] to close the list"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", head+c.mounts+nl+`    image: "img"`+nl)
			sb := res.File.Workflows[0].Sandbox
			if sb == nil || sb.Image != "img" {
				t.Fatalf("the next property was read as elements: sandbox = %+v, diagnostics %v", sb, res.Diagnostics)
			}
			if !reflect.DeepEqual(sb.Mounts, c.wantMounts) {
				t.Errorf("mounts = %q, want %q", sb.Mounts, c.wantMounts)
			}
			if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, c.wantMessage) || res.Diagnostics[0].Line != 4 {
				t.Errorf("want one diagnostic on line 4 saying %q, got %v", c.wantMessage, res.Diagnostics)
			}
		})
	}
}

// The same invariant on the OTHER reader of an inline list: an agent/judge
// `tools:` reaches the element loop through parseDeclaredToolList, which —
// alone among the lists — falls through to it on a `[` that was NOT read, to
// keep a broken line from being salvaged into a binding `tools: []`. The
// refusal therefore has to live in parseBracketElems, where both paths meet;
// hoisted up into parseBracketList it would leave this one eating the next
// property. Reddens on that hoist: `tools` becomes ["model", "openai/gpt-5.5"]
// and the node loses its model — an undeclared surface read as a declared one.
func TestAToolListWithoutItsBracketNeverReadsTheNextProperty(t *testing.T) {
	nl := string(rune(10))
	src := strings.Join([]string{
		`prompt sys:`, `  """s"""`, ``,
		`prompt usr:`, `  """u"""`, ``,
		`judge j:`,
		`  system: sys`,
		`  user: usr`,
		`  tools: "bash"`,
		`  model: "openai/gpt-5.5"`, ``,
		`workflow w:`,
		`  entry: j`,
		`  j -> done`,
	}, nl) + nl
	res := Parse("x.bot", src)
	if len(res.File.Judges) != 1 {
		t.Fatalf("want one judge, got %d; diagnostics %v", len(res.File.Judges), res.Diagnostics)
	}
	j := res.File.Judges[0]
	if j.Model != "openai/gpt-5.5" {
		t.Errorf("the next property was read as elements: model = %q, want %q", j.Model, "openai/gpt-5.5")
	}
	// nil is UNDECLARED. A non-nil empty list would be the author declaring
	// "this node has no tools" (toolcatalog.ToolsDeclared), which a line the
	// parser refused must never assert on their behalf.
	if j.Tools != nil {
		t.Errorf("a refused tools: line must leave the surface undeclared, got %#v", j.Tools)
	}
	if len(res.Diagnostics) == 0 {
		t.Error("want the refused tools: line said, got no diagnostic")
	}
	for _, d := range res.Diagnostics {
		if d.Line != 10 {
			t.Errorf("diagnostic escaped the tools: line: %d %q", d.Line, d.Message)
		}
	}
}

// An inline list broken across lines — the closer, dedented, on a line of
// its own, the first text an author with YAML habits writes — is said where
// the line ends, and the property AFTER the list is still read. The broken
// line's DEDENT used to escape to the enclosing block's property loop: the
// block closed on it, the orphaned `]` drew "unexpected token at top level",
// and the sibling property was lost to the top-level skip (#1630).
func TestABrokenInlineListKeepsThePropertyAfterIt(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		want     string // substring of the one diagnostic
		line     int    // the list's line
		list     func(r *ParseResult) []string
		wantList []string
		next     func(r *ParseResult) string // the sibling property's value
	}{
		{"tools, dangling comma", "agent a:\n  tools: [bash,\n]\n  description: \"after\"\n",
			"expected ] to close the list", 2,
			func(r *ParseResult) []string { return r.File.Agents[0].Tools }, []string{"bash"},
			func(r *ParseResult) string { return r.File.Agents[0].Description }},
		{"tools, no comma", "agent a:\n  tools: [bash\n]\n  description: \"after\"\n",
			"expected ] to close the list", 2,
			func(r *ParseResult) []string { return r.File.Agents[0].Tools }, []string{"bash"},
			func(r *ParseResult) string { return r.File.Agents[0].Description }},
		{"rules", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [github.com,\n]\n      preset: trusted\n",
			"expected ] to close the list", 5,
			func(r *ParseResult) []string { return r.File.Workflows[0].Sandbox.Network.Rules }, []string{"github.com"},
			func(r *ParseResult) string { return r.File.Workflows[0].Sandbox.Network.Preset }},
		{"resource pool", "workflow w:\n  entry: done\n  resources:\n    gpu: [\"gpu-1\",\n]\n    cpu: 3\n",
			"expected ] to close the list", 4,
			func(r *ParseResult) []string { return r.File.Workflows[0].Resources.Members["gpu"] }, []string{"gpu-1"},
			func(r *ParseResult) string { return strconv.Itoa(r.File.Workflows[0].Resources.Capacities["cpu"]) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", c.src)
			if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, c.want) || res.Diagnostics[0].Line != c.line {
				t.Fatalf("want one diagnostic on line %d saying %q, got %v", c.line, c.want, res.Diagnostics)
			}
			if got := c.list(res); !reflect.DeepEqual(got, c.wantList) {
				t.Errorf("the list = %v, want %v", got, c.wantList)
			}
			if got := c.next(res); got == "" {
				t.Errorf("the property after the broken list was lost")
			}
		})
	}
}

// The JSON value form refuses a multi-line container the same way and
// shares the recovery: the port's next property survives, and the refused
// default stores NOTHING — asserted on Default itself, so a misread value
// fails the test.
func TestABrokenContractJSONKeepsThePropertyAfterIt(t *testing.T) {
	res := Parse("x.bot", "contract c:\n  outputs:\n    result: json\n      default: [1,\n]\n      description: \"d\"\n")
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "expected a JSON value or ']'") || res.Diagnostics[0].Line != 4 {
		t.Fatalf("want one diagnostic on line 4, got %v", res.Diagnostics)
	}
	port := res.File.Contracts[0].Outputs[0]
	if port.Default != nil {
		t.Errorf("the refused default was read as a value: %s", port.Default)
	}
	if port.Description != "d" {
		t.Errorf("the property after the broken value was lost: description %q", port.Description)
	}
}

// A list broken two blocks deep keeps the property an ANCESTOR block owns:
// `rules:` broken under `network:` (itself under `sandbox:`), the closer
// dedented to column 1, then `image:` under `sandbox:`. The resync used to
// bail on the less-indented line — the dedents closed every block as before
// and the top-level skip ate the ancestor's property (#2081).
func TestABrokenInlineListKeepsTheAncestorPropertyAfterIt(t *testing.T) {
	t.Run("sandbox property after a network list", func(t *testing.T) {
		res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [github.com,\n]\n    image: \"img\"\n")
		if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "expected ] to close the list") || res.Diagnostics[0].Line != 5 {
			t.Fatalf("want one diagnostic on line 5, got %v", res.Diagnostics)
		}
		sb := res.File.Workflows[0].Sandbox
		if got := sb.Network.Rules; !reflect.DeepEqual(got, []string{"github.com"}) {
			t.Errorf("rules = %v, want [github.com]", got)
		}
		if sb.Image != "img" {
			t.Errorf("the ancestor's property was lost: image = %q", sb.Image)
		}
	})
	// The JSON value form shares the recovery: a broken `default:` two
	// sections deep keeps the contract's NEXT section.
	t.Run("contract section after a port default", func(t *testing.T) {
		res := Parse("x.bot", "contract c:\n  outputs:\n    result: json\n      default: [1,\n]\n  inputs:\n    q: string\n")
		if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "expected a JSON value or ']'") || res.Diagnostics[0].Line != 4 {
			t.Fatalf("want one diagnostic on line 4, got %v", res.Diagnostics)
		}
		port := res.File.Contracts[0].Outputs[0]
		if port.Default != nil {
			t.Errorf("the refused default was read as a value: %s", port.Default)
		}
		if len(res.File.Contracts[0].Inputs) != 1 || res.File.Contracts[0].Inputs[0].Name != "q" {
			t.Errorf("the ancestor's section was lost: inputs = %+v", res.File.Contracts[0].Inputs)
		}
	})
	// The ancestor landing must not trade the property for the declarations
	// after the block: the loops above the ancestor each get the one closing
	// DEDENT the over-popping closer denied them, so the file past the block
	// reads as itself.
	t.Run("and the declaration after the block", func(t *testing.T) {
		res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [github.com,\n]\n    image: \"img\"\n\nagent a:\n  model: \"m\"\n")
		if res.File.Workflows[0].Sandbox.Image != "img" {
			t.Errorf("the ancestor's property was lost: image = %q", res.File.Workflows[0].Sandbox.Image)
		}
		if len(res.File.Agents) != 1 || res.File.Agents[0].Model != "m" {
			t.Errorf("the declaration after the block was lost: %+v", res.File.Agents)
		}
	})
}

// The same exact accounting for a SIBLING landing two blocks deep: the
// closer dedented to column 1 over-pops every level, only the list's own
// block re-indents with the sibling, and the loops in between would never
// see a DEDENT again — the declaration after the block used to read as a
// property of the broken block.
func TestABrokenInlineListKeepsTheDeclarationAfterARescuedSibling(t *testing.T) {
	res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [github.com,\n]\n      preset: trusted\n\nagent a:\n  model: \"m\"\n")
	nb := res.File.Workflows[0].Sandbox.Network
	if got := nb.Preset; got != "trusted" {
		t.Errorf("the sibling after the broken list was lost: preset = %q", got)
	}
	if got := nb.Rules; !reflect.DeepEqual(got, []string{"github.com"}) {
		t.Errorf("rules = %v, want [github.com]", got)
	}
	if len(res.File.Agents) != 1 || res.File.Agents[0].Model != "m" {
		t.Errorf("the declaration after the block was lost: %+v", res.File.Agents)
	}
}

// A junk continuation line that pushes a level of its own leaves the
// sibling's return dedent off the lexer's stack: the lexer pops, says E003,
// and pushes nothing back, so no DEDENT ever comes before the next
// top-level line. Landing on the sibling without restoring the count would
// eat the only DEDENTs the enclosing block can close on — every declaration
// after the block would read as its property — which is why the resync used
// to BAIL on this shape. It now lands on the sibling and splices the
// missing closing DEDENTs at the point the open loops close, so the block
// still gets its DEDENTs and the declarations after it survive intact
// (#2081).
func TestAnOffStackDedentKeepsTheSiblingAfterIt(t *testing.T) {
	cases := []struct {
		name string
		src  string
		next func(r *ParseResult) string // the sibling property
		want string
	}{
		{"junk over-indented, sibling at the property's column",
			"agent a:\n  tools: [bash,\n]\n   read,\n  description: \"after\"\n\nworkflow w:\n  entry: done\n",
			func(r *ParseResult) string { return r.File.Agents[0].Description }, "after"},
		{"contract default, junk over-indented",
			"contract c:\n  outputs:\n    result: json\n      default: [1,\n]\n       2,\n      description: \"d\"\n\nagent a:\n  model: \"m\"\n",
			func(r *ParseResult) string { return r.File.Contracts[0].Outputs[0].Description }, "d"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", c.src)
			if got := c.next(res); got != c.want {
				t.Errorf("the sibling after an off-stack dedent was lost: got %q, want %q", got, c.want)
			}
			// The block closure the rescue must keep: the trailing top-level
			// declaration is read as itself, never as a property of the
			// broken block.
			switch {
			case strings.Contains(c.name, "contract"):
				if len(res.File.Agents) != 1 || res.File.Agents[0].Name != "a" {
					t.Errorf("the declaration after the block was lost: %+v", res.File.Agents)
				}
			default:
				if len(res.File.Workflows) != 1 || res.File.Workflows[0].Entry != "done" {
					t.Errorf("the declaration after the block was lost: %+v", res.File.Workflows)
				}
			}
			var e002 bool
			e003 := 0
			for _, d := range res.Diagnostics {
				if d.Code == DiagExpectedToken {
					e002 = true
				}
				if d.Code == DiagBadIndentation {
					e003++
				}
			}
			if !e002 {
				t.Errorf("the broken list was not said: %v", res.Diagnostics)
			}
			if e003 != 1 {
				t.Errorf("want the off-stack dedent said exactly once, got %v", res.Diagnostics)
			}
		})
	}
}

// An off-stack dedent OUTSIDE any broken list — here a dash-list item
// misaligned after a deeper one — owns no rescue: the broken-list machinery
// must leave the ordinary recovery exactly as it was, with the block's
// later properties and the declaration after the block both intact (#2081's
// first rescue draft changed the lexer globally and lost them).
func TestAnOffStackDedentOutsideABrokenListKeepsTheBlockAfterIt(t *testing.T) {
	res := Parse("x.bot", "agent a:\n  model: \"m\"\n  tools:\n    - bash\n      - read\n   - write\n  description: \"d\"\n  user: \"u\"\n\nworkflow w:\n  entry: done\n")
	a := res.File.Agents[0]
	if a.Description != "d" || a.User == "" {
		t.Errorf("the properties after the misaligned item were lost: description = %q, user = %q", a.Description, a.User)
	}
	if len(res.File.Workflows) != 1 || res.File.Workflows[0].Entry != "done" {
		t.Errorf("the declaration after the block was lost: %+v", res.File.Workflows)
	}
	if len(res.Diagnostics) != 2 {
		t.Errorf("want the two indentation diagnoses and no more, got %v", res.Diagnostics)
	}
}

// Two broken lists in one file: the first rescue's close-point scan meets
// the SECOND list's over-dedented closer — junk no block owns — and must
// bail rather than splice ahead of it, or the second remainder swallows the
// spliced DEDENTs and the declaration after the block is destroyed. Losing
// the broken blocks' later properties to the top-level skip is main's
// behavior; losing a whole valid declaration never is (#2081).
func TestTwoBrokenInlineListsKeepTheDeclarationAfterThem(t *testing.T) {
	res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [a,\n]\n    image: \"img\"\n    mounts: [m,\n]\n    user: \"u\"\n\nagent a:\n  model: \"m\"\n")
	if len(res.File.Agents) != 1 || res.File.Agents[0].Model != "m" {
		t.Errorf("the declaration after the broken blocks was lost: %+v", res.File.Agents)
	}
	var e002 int
	for _, d := range res.Diagnostics {
		if d.Code == DiagExpectedToken {
			e002++
		}
	}
	if e002 == 0 {
		t.Errorf("the broken lists were not said: %v", res.Diagnostics)
	}
}

// A tab-indented line is an Error token that popped NOTHING — the level was
// never pushed off-stack — so the resync reads past it as ever: the sibling
// survives, the E003 is said once, and the block still closes on the real
// dedent after it.
func TestABrokenInlineListKeepsThePropertyAfterATabLine(t *testing.T) {
	res := Parse("x.bot", "agent a:\n  tools: [bash,\n\t]\n  description: \"after\"\n\nworkflow w:\n  entry: done\n")
	if got := res.File.Agents[0].Description; got != "after" {
		t.Errorf("the property after the broken list was lost: %q; diagnostics %v", got, res.Diagnostics)
	}
	if len(res.File.Workflows) != 1 || res.File.Workflows[0].Entry != "done" {
		t.Errorf("the declaration after the block was lost: %+v", res.File.Workflows)
	}
	e003 := 0
	for _, d := range res.Diagnostics {
		if d.Code == DiagBadIndentation {
			e003++
		}
	}
	if e003 != 1 {
		t.Errorf("want the tab said exactly once, got %v", res.Diagnostics)
	}
}

// The same broken text with a top-level declaration after it, not a sibling
// property: nothing is resynced away — the dedents belong to the blocks the
// list was in, and the declaration is read as ever. This pins the
// bail-consumes-nothing contract; it is not evidence the rescue works.
func TestABrokenInlineListKeepsTheTopLevelDeclarationAfterIt(t *testing.T) {
	res := Parse("x.bot", "agent a:\n  tools: [bash,\n]\nworkflow w:\n  entry: done\n")
	if len(res.File.Workflows) != 1 || res.File.Workflows[0].Name != "w" {
		t.Fatalf("the declaration after the broken list was lost: %+v", res.File.Workflows)
	}
	if got := res.File.Agents[0].Tools; !reflect.DeepEqual(got, []string{"bash"}) {
		t.Errorf("tools = %v, want [bash]", got)
	}
}

// An empty string is neither a rule nor a mount: the sandbox would refuse it
// only when it starts. The reader says it where it stands and leaves it out,
// in both written forms, and the rest of the list is read.
func TestAnEmptyStringIsNotARuleOrAMount(t *testing.T) {
	nl := string(rune(10))
	head := strings.Join([]string{"workflow w:", "  entry: done", "  sandbox:"}, nl) + nl
	for _, c := range []struct {
		name, mounts string
		wantMounts   []string
	}{
		{"inline", `    mounts: ["/x:/x", ""]` + nl, []string{"/x:/x"}},
		{"dash form", `    mounts:` + nl + `      - "/x:/x"` + nl + `      - ""` + nl, []string{"/x:/x"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", head+c.mounts)
			sb := res.File.Workflows[0].Sandbox
			if sb == nil || !reflect.DeepEqual(sb.Mounts, c.wantMounts) {
				t.Fatalf("mounts = %+v, want %q; diagnostics %v", sb, c.wantMounts, res.Diagnostics)
			}
			if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "empty string") {
				t.Errorf("want one diagnostic naming the empty string, got %v", res.Diagnostics)
			}
		})
	}
}

// A less-indented line that OPENS A DECLARATION — `agent b:` dedented one
// level too many, standing at an ancestor block's property column — is not
// a property the ancestor rescue may land on: a declaration keyword lexes
// as an ident, so the ancestor's property loop would eat the declaration
// whole. Main, in bailing, let the dedents close the blocks and the top
// level read the declaration as itself; the rescue keeps exactly that — a
// declaration starter never lands.
func TestAMisIndentedDeclarationAfterABrokenListStandsAsItself(t *testing.T) {
	res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [a,\n]\n  agent b:\n    model: \"m\"\n")
	if len(res.File.Workflows) != 1 || res.File.Workflows[0].Entry != "done" {
		t.Fatalf("workflow w lost: %+v; diagnostics %v", res.File.Workflows, res.Diagnostics)
	}
	if len(res.File.Agents) != 1 || res.File.Agents[0].Name != "b" || res.File.Agents[0].Model != "m" {
		t.Fatalf("the mis-indented declaration was eaten: %+v; diagnostics %v", res.File.Agents, res.Diagnostics)
	}
}

// The same for a declaration at the TOP LEVEL after a nested broken list:
// column 1 is never an ancestor's column — the top level owns the line, and
// the ordinary recovery reads it as itself.
func TestADeclarationAtTheTopLevelAfterABrokenNestedListStandsAsItself(t *testing.T) {
	res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [a,\n]\nagent b:\n  model: \"m\"\n")
	if len(res.File.Workflows) != 1 || res.File.Workflows[0].Entry != "done" {
		t.Fatalf("workflow w lost: %+v; diagnostics %v", res.File.Workflows, res.Diagnostics)
	}
	if len(res.File.Agents) != 1 || res.File.Agents[0].Name != "b" || res.File.Agents[0].Model != "m" {
		t.Fatalf("the top-level declaration was lost: %+v; diagnostics %v", res.File.Agents, res.Diagnostics)
	}
}

// An off-stack dedent whose close point the scan cannot trust — the next
// outdented line opens with junk (`]` again, not a property) — bails and
// consumes nothing: the plain landing there would eat the DEDENTs the open
// loops close on, and every declaration after the block with them.
func TestAnOffStackDedentWithAJunkCloseLineBailsAndKeepsTheDeclarationsAfterIt(t *testing.T) {
	res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      tools: [bash,\n]\n       read,\n      preset: trusted\n]\n\nagent a:\n  model: \"m\"\n")
	if len(res.File.Workflows) != 1 || res.File.Workflows[0].Entry != "done" {
		t.Fatalf("workflow w lost: %+v; diagnostics %v", res.File.Workflows, res.Diagnostics)
	}
	if len(res.File.Agents) != 1 || res.File.Agents[0].Model != "m" {
		t.Fatalf("the declaration after the block was lost: %+v; diagnostics %v", res.File.Agents, res.Diagnostics)
	}
}

// An off-stack dedent with a TAB line standing between the sibling and the
// close point bails too: the scan refuses a line-start lexer diagnosis
// before the point, and the rescue never guesses past one. The workflow
// after the block is read as itself, as on main.
func TestAnOffStackDedentWithATabLineBeforeTheCloseBailsAndKeepsTheWorkflowAfterIt(t *testing.T) {
	res := Parse("x.bot", "agent a:\n  tools: [bash,\n]\n   read,\n  description: \"d\"\n\ttabbed\n\nworkflow w:\n  entry: done\n")
	if len(res.File.Workflows) != 1 || res.File.Workflows[0].Entry != "done" {
		t.Fatalf("the workflow after the block was lost: %+v; diagnostics %v", res.File.Workflows, res.Diagnostics)
	}
	if len(res.File.Agents) != 1 || res.File.Agents[0].Name != "a" {
		t.Fatalf("the broken block's own agent was lost: %+v; diagnostics %v", res.File.Agents, res.Diagnostics)
	}
}

// The bail for an off-stack dedent consumes NOTHING — so the orphaned `]`
// that over-popped is said where IT stands, as a top-level stray on its own
// line. A landing that claimed the less-indented `description:` at column 1
// (a line the top level owns) would push the stray's diagnostic two lines
// down: the top-level column is never an ancestor's column, and the resync
// does not turn main's stray into a later one.
func TestAnOffStackDedentBailSaysTheStrayCloserWhereItStands(t *testing.T) {
	res := Parse("x.bot", "agent a:\n  tools: [bash,\n]\n   read,\ndescription: \"top\"\n\nagent a:\n  model: \"m\"\n")
	stray := false
	for _, d := range res.Diagnostics {
		if d.Code == DiagUnexpectedToken && d.Line == 3 {
			stray = true
		}
	}
	if !stray {
		t.Fatalf("the orphaned closer was not said on its own line 3: %v", res.Diagnostics)
	}
	if len(res.File.Agents) != 2 || res.File.Agents[1].Model != "m" {
		t.Fatalf("the declarations after the block were lost: %+v; diagnostics %v", res.File.Agents, res.Diagnostics)
	}
}

// A tab-indented diagnosis at a DEEPER column than the sibling — the one
// line-start error the junk arm does not cover — bails as well: the scan
// refuses it, the plain landing is not taken, and the workflow after the
// block is read as itself instead of becoming the broken block's property.
func TestAnOffStackDedentWithADeepTabLineBeforeTheCloseBailsAndKeepsTheWorkflowAfterIt(t *testing.T) {
	res := Parse("x.bot", "agent a:\n  tools: [bash,\n]\n   read,\n  description: \"d\"\n  \tdeep note\n\nworkflow w:\n  entry: done\n")
	if len(res.File.Workflows) != 1 || res.File.Workflows[0].Entry != "done" {
		t.Fatalf("the workflow after the block was lost: %+v; diagnostics %v", res.File.Workflows, res.Diagnostics)
	}
	if len(res.File.Agents) != 1 || res.File.Agents[0].Name != "a" {
		t.Fatalf("the broken block's own agent was lost: %+v; diagnostics %v", res.File.Agents, res.Diagnostics)
	}
}

// A mis-indented declaration standing at the landing line's own column —
// `judge j3:` one level too shallow, after a rescued property — is not the
// rescue's to keep: the landing keeps the ancestor's loop open across it,
// and that loop reads it whole as its property (an `expected ->` fight with
// the router-edge syntax). The rescue bails; the ordinary recovery closes
// the blocks and the top level reads the declaration as itself.
func TestADeclarationAtTheLandingColumnAfterARescueStandsAsItself(t *testing.T) {
	res := Parse("x.bot", "workflow w1:\n  entry: v0\n  sandbox:\n    network:\n      rules: [a1,\n       workflow w2:\n          model: v2\n  rules: [a1, b]\n  judge j3:\n    model: v3\n")
	if len(res.File.Agents) != 0 {
		t.Errorf("the mis-indented `workflow w2:` was claimed as an agent: %+v", res.File.Agents)
	}
	if len(res.File.Judges) != 1 || res.File.Judges[0].Name != "j3" || res.File.Judges[0].Model != "v3" {
		t.Fatalf("the declaration at the landing's column was eaten: %+v; diagnostics %v", res.File.Judges, res.Diagnostics)
	}
}

// The same guard one line further: the close point itself must not be a
// declaration a still-open block owns. The splice hands the loops above
// their closing DEDENTs, and the loop at the close line's column reads
// `judge j3:` whole — where main, bailing, read it at the top level. A
// declaration at the TOP LEVEL's column reads as itself after the splice,
// so only an owned column refuses.
func TestACloseLineADeclarationStandsAtIsNeverARescueClosePoint(t *testing.T) {
	res := Parse("x.bot", "workflow w1:\n  entry: v0\n  sandbox:\n    network:\n      rules: [a1,\n       workflow w2:\n          model: v2\n    allow: [a1, b]\n  judge j3:\n    model: v3\n  k: v1\n")
	if len(res.File.Agents) != 0 {
		t.Errorf("the mis-indented `workflow w2:` was claimed as an agent: %+v", res.File.Agents)
	}
	if len(res.File.Judges) != 1 || res.File.Judges[0].Name != "j3" || res.File.Judges[0].Model != "v3" {
		t.Fatalf("the declaration at the close point was eaten: %+v; diagnostics %v", res.File.Judges, res.Diagnostics)
	}
}

// A declaration starter mis-indented INSIDE the remainder — a `judge j4:`
// pushed deeper than the broken list — is not the junk continuation the
// off-stack read-past covers. Main bailed on the Error and let the ordinary
// recovery route the lines after it to the shallower block that owns them:
// the valid `mounts` list main reads whole, the read-past landing would
// hand to the list's own block, which refuses it. The read-past disarms.
func TestADeclarationInsideTheRemainderKeepsTheBailAndTheListMainReads(t *testing.T) {
	res := Parse("x.bot", "workflow w3:\n  entry: v0\n  sandbox:\n    network:\n      rules: [a3,\n         judge j4:\n       model: v4\n      mounts: [a3, b]\n    allow: [a3, b]\n")
	sb := res.File.Workflows[0].Sandbox
	if sb == nil || !reflect.DeepEqual(sb.Mounts, []string{"a3", "b"}) {
		t.Fatalf("the list main reads whole was lost to the list's own block: %+v; diagnostics %v", sb, res.Diagnostics)
	}
	if got := sb.Network.Rules; !reflect.DeepEqual(got, []string{"a3"}) {
		t.Errorf("rules = %v, want [a3]", got)
	}
}

// A less-indented line at a column NO open block owns — between the
// ancestor's column and the list's own — leaves the lexer no level to match
// (an off-stack dedent by construction), so the off-stack bail fires and
// the rescue keeps main's exact refusal: the stray is said on its line, the
// property behind it is lost the way main lost it, and the list's own
// property still reads.
func TestALineAtAStrangersColumnBailsAndKeepsMainRefusals(t *testing.T) {
	res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [a,\n   stray: x\n    image: \"img\"\n")
	sb := res.File.Workflows[0].Sandbox
	if sb == nil || !reflect.DeepEqual(sb.Network.Rules, []string{"a"}) {
		t.Fatalf("the list's own property was lost: %+v; diagnostics %v", sb, res.Diagnostics)
	}
	if sb.Image != "" {
		t.Errorf("the property behind an off-stack stray was rescued where main loses it: %+v", sb)
	}
	if len(res.Diagnostics) != 3 {
		t.Errorf("want main's three diagnostics (the list, the stray's off-stack dedent, the stray property), got %v", res.Diagnostics)
	}
}

// An ancestor landing whose close point hands the rest to the ordinary
// recovery: the property is saved, and the line after it at the shallower
// column is refused by the loop that owns it, exactly as main refused it.
func TestARescuedPropertyKeepsTheFollowingLineReadAsMainReadsIt(t *testing.T) {
	res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [a,\n    image: \"img\"\n  entry2: x\n")
	sb := res.File.Workflows[0].Sandbox
	if sb == nil || sb.Image != "img" {
		t.Fatalf("the ancestor's property was lost: %+v; diagnostics %v", sb, res.Diagnostics)
	}
	if len(res.Diagnostics) != 2 || !strings.Contains(res.Diagnostics[1].Message, "entry2") {
		t.Errorf("want the list's diagnostic and entry2 refused as main refuses it, got %v", res.Diagnostics)
	}
}

// A keyword that doubles as a property name is CLAIMABLE in its property
// form: `contract: c` standing at the workflow's column after a rescued
// list is the workflow's contract, not a mis-indented contract declaration
// — the header form wants a NAME between the keyword and the colon. The
// landing reads it whole; refusing every contract-starting line loses the
// contract and drags the orphaned closer's stray and an `expected contract
// name` fight behind it.
func TestAPropertyFormOfADeclarationKeywordIsStillClaimable(t *testing.T) {
	res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [github.com,\n]\n  contract: c\n")
	if res.File.Workflows[0].Contract != "c" {
		t.Fatalf("the workflow's contract was lost to the declaration refusal: %q; diagnostics %v", res.File.Workflows[0].Contract, res.Diagnostics)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Line != 5 {
		t.Errorf("want the list's diagnostic alone, got %v", res.Diagnostics)
	}
}

// A group holds agent, judge, router, human, tool and compute DECLARATIONS
// as members: a member header standing at the group's column after a
// rescued list is the next member, not a misplaced top-level declaration —
// refusing it silently drops the member, and a `use g as` would no longer
// instantiate it.
func TestAGroupMemberHeaderAfterARescuedListIsStillAMember(t *testing.T) {
	res := Parse("x.bot", "group g:\n  agent a:\n    tools: [bash,\n]\n  agent b:\n    model: m\n")
	if len(res.File.Groups) != 1 || len(res.File.Groups[0].Agents) != 2 {
		t.Fatalf("the group lost a member to the declaration refusal: %+v; diagnostics %v", res.File.Groups, res.Diagnostics)
	}
	if res.File.Groups[0].Agents[0].Name != "a" || res.File.Groups[0].Agents[1].Name != "b" || res.File.Groups[0].Agents[1].Model != "m" {
		t.Errorf("the members were not read whole: %+v", res.File.Groups[0].Agents)
	}
}

// reindented shifts every non-empty line of a declaration block by pad,
// so one witness block covers a declaration mis-indented to each open
// block's column.
func reindented(block, pad string) string {
	lines := strings.Split(block, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// A keyword that names NO property anywhere — secrets, presets, prompt,
// mcp_server, dsl — standing mis-indented after a broken inline list is a
// top-level declaration the file legally carries: no block can hold it, so
// the resync closes every open block and the parseFile dispatch reads it as
// itself, with the broken list's own property intact (#2259). A declaration
// keyword with a property form (contract, schema) is NOT in this set —
// TestAPropertyFormOfADeclarationKeywordIsStillClaimable pins that side.
func TestAKeywordThatNamesNoPropertyIsRescuedToTheTopLevel(t *testing.T) {
	for _, c := range []struct {
		name string
		line string
	}{
		{"secrets", "secrets:\n  k: \"v\"\n"},
		{"presets", "presets:\n  p1:\n    v: \"1\"\n"},
		{"mcp_server", "mcp_server fs:\n  command: x\n"},
		{"prompt", "prompt greet:\n  hi there\n"},
		{"dsl", "dsl: 2\n"},
	} {
		for _, ind := range []struct {
			name string
			pad  string
		}{
			{"at the workflow's column", "  "},
			{"at the sandbox's column", "    "},
		} {
			t.Run(c.name+" "+ind.name, func(t *testing.T) {
				res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [a,\n"+reindented(c.line, ind.pad))
				sb := res.File.Workflows[0].Sandbox
				if sb == nil || !reflect.DeepEqual(sb.Network.Rules, []string{"a"}) {
					t.Fatalf("the list's own property was lost: %+v; diagnostics %v", sb, res.Diagnostics)
				}
				switch c.name {
				case "secrets":
					if res.File.Secrets == nil || len(res.File.Secrets.Fields) != 1 ||
						res.File.Secrets.Fields[0].Name != "k" || res.File.Secrets.Fields[0].Value != "v" {
						t.Errorf("the mis-indented secrets block was not read at the top level: %+v; diagnostics %v", res.File.Secrets, res.Diagnostics)
					}
				case "presets":
					if res.File.Presets == nil || len(res.File.Presets.Entries) != 1 || res.File.Presets.Entries[0].Name != "p1" {
						t.Errorf("the mis-indented presets block was not read at the top level: %+v; diagnostics %v", res.File.Presets, res.Diagnostics)
					}
				case "mcp_server":
					if len(res.File.MCPServers) != 1 || res.File.MCPServers[0].Name != "fs" {
						t.Errorf("the mis-indented mcp_server was not read at the top level: %+v; diagnostics %v", res.File.MCPServers, res.Diagnostics)
					}
				case "prompt":
					if len(res.File.Prompts) != 1 || res.File.Prompts[0].Name != "greet" || res.File.Prompts[0].Body != "hi there" {
						t.Errorf("the mis-indented prompt was not read at the top level: %+v; diagnostics %v", res.File.Prompts, res.Diagnostics)
					}
				case "dsl":
					// A mid-file dsl: header is not applied — the lexer read
					// everything above it as profile 1 — so the surface stays
					// unset and the refusal names the line, cleanly, instead
					// of the unknown-property noise.
					if res.File.Profile != 0 {
						t.Errorf("a mid-file dsl: header was applied: profile %d", res.File.Profile)
					}
					misplaced := 0
					for _, d := range res.Diagnostics {
						if d.Code == DiagMisplacedHeader {
							misplaced++
							if d.Line != 6 {
								t.Errorf("the misplaced-header refusal escaped the dsl line: %d %q", d.Line, d.Message)
							}
						}
					}
					if misplaced != 1 {
						t.Errorf("want exactly one misplaced-header refusal on the dsl line, got %v", res.Diagnostics)
					}
				}
				if c.name == "dsl" {
					if len(res.Diagnostics) != 2 {
						t.Errorf("want the list's diagnostic and the header refusal and no more, got %v", res.Diagnostics)
					}
				} else if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "expected ] to close the list") || res.Diagnostics[0].Line != 5 {
					t.Errorf("want the broken list's diagnostic alone, got %v", res.Diagnostics)
				}
			})
		}
	}
}

// The same rescue when the mis-indented declaration lands at the LIST'S OWN
// column — the sibling the list's block would otherwise read as its next
// property and refuse with an unknown-property diagnostic: every open block
// closes, the top level reads the declaration (#2259).
func TestAKeywordAtTheListsOwnColumnIsRescuedToTheTopLevel(t *testing.T) {
	res := Parse("x.bot", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [a,\n      secrets:\n        k: \"v\"\n")
	sb := res.File.Workflows[0].Sandbox
	if sb == nil || !reflect.DeepEqual(sb.Network.Rules, []string{"a"}) {
		t.Fatalf("the list's own property was lost: %+v; diagnostics %v", sb, res.Diagnostics)
	}
	if res.File.Secrets == nil || len(res.File.Secrets.Fields) != 1 || res.File.Secrets.Fields[0].Name != "k" {
		t.Errorf("the sibling declaration was not read at the top level: %+v; diagnostics %v", res.File.Secrets, res.Diagnostics)
	}
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "expected ] to close the list") || res.Diagnostics[0].Line != 5 {
		t.Errorf("want the broken list's diagnostic alone, got %v", res.Diagnostics)
	}
}

// The contract's JSON value form shares the recovery: a mis-indented
// top-level declaration after a broken `default:` is rescued the same way,
// and the refused default still stores nothing.
func TestABrokenContractJSONKeepsTheMisIndentedDeclarationAfterIt(t *testing.T) {
	res := Parse("x.bot", "contract c:\n  outputs:\n    result: json\n      default: [1,\n  secrets:\n    k: \"v\"\n")
	port := res.File.Contracts[0].Outputs[0]
	if port.Default != nil {
		t.Errorf("the refused default was read as a value: %s", port.Default)
	}
	if res.File.Secrets == nil || len(res.File.Secrets.Fields) != 1 || res.File.Secrets.Fields[0].Name != "k" {
		t.Errorf("the mis-indented secrets block was not read at the top level: %+v; diagnostics %v", res.File.Secrets, res.Diagnostics)
	}
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "expected a JSON value or ']'") || res.Diagnostics[0].Line != 4 {
		t.Errorf("want the broken value's diagnostic alone, got %v", res.Diagnostics)
	}
}

// An off-stack dedent in the remainder — junk over-indented between the
// broken list and the mis-indented declaration — keeps the bail: the DEDENT
// accounting is uncertain there, and the ordinary recovery already routes
// what follows to its owner. The rescue never guesses past the Error; this
// pins the list's own property and the list's own diagnostic.
func TestAKeywordThatNamesNoPropertyIsStillRefusedAfterAnOffStackDedent(t *testing.T) {
	res := Parse("x.bot", "agent a:\n  tools: [bash,\n]\n   read,\n  secrets:\n    k: \"v\"\n")
	if len(res.File.Agents) != 1 {
		t.Fatalf("the broken block's own agent was lost: %+v; diagnostics %v", res.File.Agents, res.Diagnostics)
	}
	if got := res.File.Agents[0].Tools; !reflect.DeepEqual(got, []string{"bash"}) {
		t.Errorf("the list's own property was lost: %v", got)
	}
	e003 := 0
	for _, d := range res.Diagnostics {
		if d.Code == DiagBadIndentation {
			e003++
		}
	}
	if e003 != 1 {
		t.Errorf("want the off-stack dedent said exactly once, got %v", res.Diagnostics)
	}
}
