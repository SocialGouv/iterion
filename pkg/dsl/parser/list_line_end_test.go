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
