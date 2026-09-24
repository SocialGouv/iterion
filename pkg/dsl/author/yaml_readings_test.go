package author

import (
	"sort"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// agentDoc is a one-agent document with extra lines under its agent.
func agentDoc(agentLines string) string {
	return "dsl: 2\nnodes:\n  - agent: a\n    model: m\n" + agentLines + "workflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n"
}

// refusedWith reports whether src is refused by the converter with a
// diagnostic of code whose message holds want.
func refusedWith(src string, code parser.DiagCode, want string) (bool, string) {
	res := Parse("x.bot.yaml", []byte(src))
	var msgs []string
	for _, d := range res.Diagnostics {
		msgs = append(msgs, d.Error())
		if d.Code == code && d.Severity == parser.SeverityError && strings.Contains(d.Message, want) {
			return true, ""
		}
	}
	return false, strings.Join(msgs, "\n")
}

// errorsOfDoc renders the error diagnostics src is refused with.
func errorsOfDoc(src string) []string {
	var out []string
	for _, d := range Parse("x.bot.yaml", []byte(src)).Diagnostics {
		if d.Severity == parser.SeverityError {
			out = append(out, d.Error())
		}
	}
	return out
}

// compiledCodes are the codes of the diagnostics src compiles to, sorted,
// or ok false when the converter refuses it.
func compiledCodes(src string) (codes string, ok bool) {
	res := Parse("x.bot.yaml", []byte(src))
	if res.HasErrors() {
		return "", false
	}
	var out []string
	for _, d := range ir.Compile(res.File).Diagnostics {
		out = append(out, string(d.Code))
	}
	sort.Strings(out)
	return strings.Join(out, ","), true
}

// A plain YAML number spelled otherwise than the .bot spells it — or a
// bool where text is taken — is refused where it is written, once, with
// YAML's reading and the ways out the site takes, at every form that takes
// one: YAML reads `010` as the octal 8 where the .bot reads 10, and the
// twin would re-spell it in silence. Every way out a refusal names is one
// the site takes — substituted, it reads and compiles as a valid value
// there does — and a site that takes a number only never says to quote.
// A block left empty by the refusal is not read a second time as a syntax
// error on the line below.
func TestAValueYAMLReadsOtherwiseThanItIsWrittenIsRefused(t *testing.T) {
	emit := func(v string) string {
		return "dsl: 2\nnodes:\n  - agent: a\n    model: m\n  - emit: e\n    with:\n      code: " + v + "\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> e\n    - e -> done\n"
	}
	action := func(prop string) func(string) string {
		return func(v string) string {
			return "dsl: 2\nnodes:\n  - tool: c\n    action: forgejo.issue.comment\n    connection: forge-main\n" + strings.Replace(prop, "%v", v, 1) + "workflow:\n  name: w\n  entry: c\n  edges:\n    - c -> done\n"
		}
	}
	varDoc := func(typ string) func(string) string {
		return func(v string) string {
			return "dsl: 2\nvars:\n  n:\n    type: " + typ + "\n    default: " + v + "\n" + agentDoc("")[len("dsl: 2\n"):]
		}
	}
	preset := func(typ string) func(string) string {
		return func(v string) string {
			return "dsl: 2\nvars:\n  n: " + typ + "\npresets:\n  p:\n    n: " + v + "\n" + agentDoc("")[len("dsl: 2\n"):]
		}
	}
	maxTokens := func(v string) string { return agentDoc("    max_tokens: " + v + "\n") }
	budget := func(v string) string {
		return "dsl: 2\nnodes:\n  - agent: a\n    model: m\nworkflow:\n  name: w\n  entry: a\n  budget:\n    max_cost_usd: " + v + "\n  edges:\n    - a -> done\n"
	}
	resources := func(v string) string {
		return "dsl: 2\nnodes:\n  - agent: a\n    model: m\nworkflow:\n  name: w\n  entry: a\n  resources:\n    gpu: " + v + "\n  edges:\n    - a -> done\n"
	}
	for name, tc := range map[string]struct {
		doc          func(string) string
		value, valid string
		want         string
		accept       []string // the ways out named, each of which must read and compile as `valid` does
	}{
		"an integer property in octal":            {maxTokens, "010", "1", "YAML reads `010` as the octal number 8, not 10 — write the number you mean without its leading 0", []string{"8", "10"}},
		"an integer property in hex":              {maxTokens, "0x10", "1", "YAML reads `0x10` as 16 — write `16`", []string{"16"}},
		"an integer property signed":              {maxTokens, "+3", "1", "YAML reads `+3` as 3 — write `3`", []string{"3"}},
		"an integer with underscores":             {maxTokens, "1_000", "1", "YAML reads `1_000` as 1000 — write `1000`", []string{"1000"}},
		"an integer YAML reads as a float":        {maxTokens, "08", "1", "YAML reads `08` as a float (a leading 0 makes the digits octal, and an 8 or a 9 is not one) — write `8`", []string{"8"}},
		"an integer with an exponent":             {maxTokens, "1e2", "1", "YAML reads `1e2` as the float 100 — write `100`", []string{"100"}},
		"an integer ending in a point":            {maxTokens, "0.", "1", "YAML reads `0.` as the float 0 — write `0`", []string{"0"}},
		"an integer YAML reads as a fraction":     {maxTokens, "01.5", "1", "takes an integer written as digits; YAML reads `01.5` as 1.5", []string{}},
		"a count in hex":                          {resources, "0x10", "1", "YAML reads `0x10` as 16 — write `16`", []string{"16"}},
		"a count YAML reads as a float":           {resources, "08", "1", "YAML reads `08` as a float (a leading 0 makes the digits octal, and an 8 or a 9 is not one) — write `8`", []string{"8"}},
		"a budget beyond every integer":           {budget, "1e19", "1", "YAML reads `1e19` as 10000000000000000000.0 — write `10000000000000000000.0`", []string{"10000000000000000000.0"}},
		"a float var's default beyond integers":   {varDoc("float"), "1e19", "1.5", "YAML reads `1e19` as 10000000000000000000.0 — write `10000000000000000000.0`", []string{"10000000000000000000.0"}},
		"a json var's default, a signed zero":     {varDoc("json"), "-0", "1", "YAML reads `-0` as 0 — write `0`, or quote the value if it is text", []string{"0", "'-0'"}},
		"a json var's default, minus zero, octal": {varDoc("json"), "-00", "1", "YAML reads `-00` as 0 — write `0`, or quote the value if it is text", []string{"0", "'-00'"}}, "a budget with an exponent": {budget, "1e2", "1", "YAML reads `1e2` as 100 — write `100`", []string{"100"}},
		"a budget without its leading digit":      {budget, ".5", "1", "YAML reads `.5` as 0.5 — write `0.5`", []string{"0.5"}},
		"a budget of minus zero":                  {budget, "-0.0", "1", "YAML reads `-0.0` as 0 — write `0`", []string{"0"}},
		"a budget with a leading 0":               {budget, "01.5", "1", "YAML reads `01.5` as 1.5 — write `1.5`", []string{"1.5"}},
		"an int var's default in octal":           {varDoc("int"), "010", "1", "YAML reads `010` as the octal number 8, not 10 — write the number you mean without its leading 0", []string{"8", "10"}},
		"an int var's default YAML reads a float": {varDoc("int"), "08", "1", "YAML reads `08` as a float (a leading 0 makes the digits octal, and an 8 or a 9 is not one) — write `8`", []string{"8"}},
		"a float var's default, an exponent":      {varDoc("float"), "1.5e2", "1.5", "YAML reads `1.5e2` as the float 150 — write `150`", []string{"150"}},
		"a float var's default with a leading 0":  {varDoc("float"), "01.5", "1.5", "YAML reads `01.5` as 1.5 — write `1.5`", []string{"1.5"}},
		"a float var's default, minus zero":       {varDoc("float"), "-0.0", "1.5", "YAML reads `-0.0` as the float 0 — write `0`", []string{"0"}},
		"a float var's default YAML reads octal":  {varDoc("float"), "08", "1.5", "YAML reads `08` as a float (a leading 0 makes the digits octal, and an 8 or a 9 is not one) — write `8`", []string{"8"}},
		"a string var's default, a number":        {varDoc("string"), "010", "x", "of a string var takes a string; `010` reads as an integer — quote it", []string{"'010'"}},
		"a string var's default, signed":          {varDoc("string"), "-10", "x", "of a string var takes a string; `-10` reads as an integer — quote it", []string{"'-10'"}},
		"a string var's default, a bool":          {varDoc("string"), "True", "x", "of a string var takes a string; `True` reads as a bool — quote it", []string{"'True'"}},
		"a with value whose readings agree":       {emit, "007", "1", "YAML reads `007` as 7 — write `7`, or quote the value if it is text", []string{"7", "'007'"}},
		"a with value, a capitalised bool":        {emit, "True", "1", "YAML reads `True` as true — write `true`, or quote the value if it is text", []string{"true", "'True'"}},
		"a with value, a plus sign":               {emit, "+3", "1", "YAML reads `+3` as 3 — write `3`, or quote the value if it is text", []string{"3", "'+3'"}},
		"a with value, a signed octal":            {emit, "-010", "1", "YAML reads `-010` as the octal number -8, not -10", []string{"-8", "-10", "'-010'"}},
		"a with value, a signed zero in octal":    {emit, "-00", "1", "YAML reads `-00` as -0 — write `-0`, or quote the value if it is text", []string{"-0", "'-00'"}},
		"an action's retry with a leading zero":   {action("    retry: %v\n"), "03", "1", "YAML reads `03` as 3 — write `3`, or quote the value if it is text", []string{"3", "'03'"}},
		"a param, a capitalised bool":             {action("    params:\n      draft: %v\n"), "True", "1", "YAML reads `True` as true — write `true`, or quote the value if it is text", []string{"true", "'True'"}},
		"a param whose readings disagree":         {action("    params:\n      code: %v\n"), "010", "1", "YAML reads `010` as the octal number 8, not 10 — write the number you mean without its leading 0, or quote the value if it is text", []string{"8", "10", "'010'"}},
		"a param in hex, the only one of params":  {action("    params:\n      code: %v\n"), "0x1F", "1", "YAML reads `0x1F` as 31 — write `31`, or quote the value if it is text", []string{"31", "'0x1F'"}},
	} {
		t.Run(name, func(t *testing.T) {
			errs := errorsOfDoc(tc.doc(tc.value))
			if len(errs) != 1 || !strings.Contains(errs[0], "[E051]") || !strings.Contains(errs[0], tc.want) {
				t.Fatalf("want exactly one E051 saying %q, got:\n%s", tc.want, strings.Join(errs, "\n"))
			}
			quoteNamed := strings.Contains(errs[0], "quote")
			baseline, ok := compiledCodes(tc.doc(tc.valid))
			if !ok {
				t.Fatalf("the valid value %q does not read", tc.valid)
			}
			quoted := false
			for _, way := range tc.accept {
				quoted = quoted || strings.HasPrefix(way, "'")
				got, ok := compiledCodes(tc.doc(way))
				if !ok || got != baseline {
					t.Errorf("the way out %s does not read or compile as %q does (%q vs %q, read %v)", way, tc.valid, got, baseline, ok)
				}
			}
			if quoteNamed != quoted {
				t.Errorf("the refusal says quote: %v, the site takes a quoted value: %v — %s", quoteNamed, quoted, errs[0])
			}
		})
	}
	// A preset's var is declared elsewhere: its value takes a number or a
	// text, and the refusal names both ways out — the one an int var takes
	// and the one a string var takes each read and compile.
	// A JSON port's value is written bare the same way: a number or a text,
	// the type declared beside it.
	port := func(typ string) func(string) string {
		return func(v string) string {
			return "dsl: 2\ncontracts:\n  c:\n    version: 1\n    inputs:\n      n:\n        type: " + typ + "\n        default: " + v + "\n" + agentDoc("")[len("dsl: 2\n"):]
		}
	}
	for name, tc := range map[string]struct {
		doc                     func(string) string
		value, want, way, valid string
	}{
		"a preset of an int var in hex":             {preset("int"), "0x10", "YAML reads `0x10` as 16 — write `16`, or quote the value if it is text", "16", "1"},
		"a preset of a string var in octal":         {preset("string"), "010", "YAML reads `010` as the octal number 8, not 10 — write the number you mean without its leading 0, or quote the value if it is text", "'010'", "x"},
		"a preset of an int var, a signed zero":     {preset("int"), "-0", "YAML reads `-0` as 0 — write `0`, or quote the value if it is text", "0", "1"},
		"a preset of a string var, signed":          {preset("string"), "-10", "takes a non-negative integer: the .bot has no signed number, got -10 — quote the value if it is text", "'-10'", "x"},
		"an int port's default, a signed zero":      {port("int"), "-0", "YAML reads `-0` as 0 — write `0`, or quote the value if it is text", "0", "1"},
		"a float port's default, minus zero, octal": {port("float"), "-00", "YAML reads `-00` as 0 — write `0`, or quote the value if it is text", "0", "1"},
		// A JSON value may be text: `True` inside one is refused, never
		// re-spelled `true`.
		"a bool inside a json port's default": {port("json"), "{a: True}", "YAML reads `True` as true — write `true`, or quote the value if it is text", "{a: 'True'}", "{a: true}"},
	} {
		errs := errorsOfDoc(tc.doc(tc.value))
		if len(errs) != 1 || !strings.Contains(errs[0], "[E051]") || !strings.Contains(errs[0], tc.want) {
			t.Errorf("%s: want exactly one E051 saying %q, got:\n%s", name, tc.want, strings.Join(errs, "\n"))
		}
		baseline, _ := compiledCodes(tc.doc(tc.valid))
		if got, ok := compiledCodes(tc.doc(tc.way)); !ok || got != baseline {
			t.Errorf("%s: the way out %s does not read or compile as %q does (%q vs %q)", name, tc.way, tc.valid, got, baseline)
		}
	}
	for _, v := range []string{"10", "0", "0.5", "3.25", "1.50"} {
		res := Parse("x.bot.yaml", []byte(budget(v)))
		if res.HasErrors() {
			t.Fatalf("max_cost_usd: %s is refused: %v", v, res.Diagnostics)
		}
		if !strings.Contains(res.Text, "max_cost_usd: "+v+"\n") {
			t.Errorf("max_cost_usd: %s is not written as spelled:\n%s", v, res.Text)
		}
	}
	for want, src := range map[string]string{
		"the integer 18446744073709551616 is out of range": maxTokens("18446744073709551616"),
		"takes a non-negative integer":                     maxTokens("-08"),
	} {
		if ok, got := refusedWith(src, parser.DiagAuthorValue, want); !ok {
			t.Errorf("want E051 saying %q, got:\n%s", want, got)
		}
	}
	if res := Parse("x.bot.yaml", []byte(varDoc("float")("18446744073709551616"))); res.HasErrors() || !strings.Contains(res.Text, "= 18446744073709551616.0") {
		t.Errorf("digits YAML reads as a float are not written as spelled: %v\n%s", res.Diagnostics, res.Text)
	}
	// A bool var takes a bool, however YAML spells it: no text to mistake.
	if res := Parse("x.bot.yaml", []byte(varDoc("bool")("True"))); res.HasErrors() || !strings.Contains(res.Text, "n: bool = true") {
		t.Errorf("a bool var's default `True` does not read as true: %v\n%s", res.Diagnostics, res.Text)
	}
	// Digits beyond every integer are the float the .bot's number reads.
	if res := Parse("x.bot.yaml", []byte(budget("10000000000000000000"))); res.HasErrors() || !strings.Contains(res.Text, "max_cost_usd: 10000000000000000000\n") {
		t.Errorf("digits beyond int64 at a number site are not read as written: %v\n%s", res.Diagnostics, res.Text)
	}
	// A null is no value, never a text to quote: a string var's default.
	for _, v := range []string{"null", "~"} {
		if ok, got := refusedWith(varDoc("string")(v), parser.DiagAuthorValue, "takes a scalar literal"); !ok || strings.Contains(got, "quote it") {
			t.Errorf("a string var's default %s: want the refusal of no value, not a quote, got:\n%s", v, got)
		}
	}
	// A spelling the .bot has none of, where the value is text, is quoted.
	if ok, got := refusedWith(action("    params:\n      x: %v\n")(".inf"), parser.DiagAuthorValue, "YAML reads `.inf` as a number the .bot has no spelling for — quote the value if it is text"); !ok {
		t.Errorf("a param `.inf`: want the quote way out, got:\n%s", got)
	}
}

// Where a value is text — string|number, a setting, a parameter, a `with`
// value — the .bot reads a bare word or a number as the text it spells,
// and so does the document: a `-` is the text's own, `true` and `false`
// are words, and an integer beyond every range is digits, not a refusal.
func TestAValueWhereTextIsTakenIsTheTextItSpells(t *testing.T) {
	emit := func(v string) string {
		return "dsl: 2\nnodes:\n  - agent: a\n    model: m\n  - emit: e\n    with:\n      code: " + v + "\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> e\n    - e -> done\n"
	}
	action := func(prop string) string {
		return "dsl: 2\nnodes:\n  - tool: c\n    action: forgejo.issue.comment\n    connection: forge-main\n" + prop + "workflow:\n  name: w\n  entry: c\n  edges:\n    - c -> done\n"
	}
	for src, want := range map[string]string{
		emit("-3"):   `with { code: "-3" }`,
		emit("-0.5"): `with { code: "-0.5" }`,
		action("    params:\n      offset: -10\n      ratio: -0.5\n"):    "offset: \"-10\"\n    ratio: \"-0.5\"\n",
		action("    params:\n      draft: true\n      private: false\n"): "draft: \"true\"\n    private: \"false\"\n",
		action("    retry: true\n"):                                      `retry: "true"`,
		action("    params:\n      id: 9223372036854775808\n"):           `id: "9223372036854775808"`,
		action("    params:\n      id: 18446744073709551615\n"):          `id: "18446744073709551615"`,
	} {
		res := Parse("x.bot.yaml", []byte(src))
		if res.HasErrors() || !strings.Contains(res.Text, want) {
			t.Errorf("a value where text is taken does not read as the text written (%q): %v\n%s", want, res.Diagnostics, res.Text)
		}
	}
}

// The profile header is refused as a profile (E040): YAML's reading and the
// spelling to write, never a quote — a quoted profile is no profile.
func TestAProfileYAMLReadsOtherwiseIsRefusedAsAProfile(t *testing.T) {
	for v, want := range map[string]string{
		"02":                   "dsl: YAML reads `02` as 2 — write `2`",
		"+2":                   "dsl: YAML reads `+2` as 2 — write `2`",
		"0x2":                  "dsl: YAML reads `0x2` as 2 — write `2`",
		"010":                  "dsl: YAML reads `010` as the octal number 8, not 10 — write the number you mean without its leading 0",
		"08":                   "dsl: YAML reads `08` as a float (a leading 0 makes the digits octal, and an 8 or a 9 is not one) — write `8`",
		"99999999999999999999": "dsl: `99999999999999999999` is beyond every integer",
	} {
		errs := errorsOfDoc("dsl: " + v + "\n" + agentDoc("")[len("dsl: 2\n"):])
		if len(errs) != 1 || !strings.Contains(errs[0], "[E040]") || !strings.Contains(errs[0], want) || strings.Contains(errs[0], "quote") {
			t.Errorf("dsl: %s — want one E040 saying %q, and no quote, got:\n%s", v, want, strings.Join(errs, "\n"))
		}
	}
	// Refused, the header's profile as YAML reads it still reads the rest:
	// a paragraph break is not said dropped as profile 1 would drop it.
	for _, v := range []string{"02", "0x2", "2.0"} {
		res := Parse("x.bot.yaml", []byte("dsl: "+v+"\nprompts:\n  p: |\n    One.\n\n    Two.\n"+agentDoc("")[len("dsl: 2\n"):]))
		if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != parser.DiagUnknownProfile {
			t.Errorf("dsl: %s — want the E040 alone, got %v", v, res.Diagnostics)
		}
	}
}

// YAML reads a `!` where a scalar starts as a tag and drops it, without a
// mark on the node — before a plain value, a quoted one or a block alike:
// `command: ! grep -q x f` would reach the .bot as `grep -q x f`, the
// negation gone. It is refused where it is written, the source read line
// by line as yaml.v3 counts its lines (a lone CR, a line separator inside a
// quoted value above, a BOM); the same value quoted whole reads with its
// `!`, and a `!` inside a value or a block is text. A document that is not
// UTF-8 — yaml.v3 also reads UTF-16 — is refused before it is read.
func TestAValueWhoseBangYAMLDropsIsRefused(t *testing.T) {
	tool := func(line string) string {
		return "dsl: 2\nnodes:\n  - tool: t\n    command: \"true\"\n" + line + "workflow:\n  name: w\n  entry: t\n  edges:\n    - t -> done\n"
	}
	command := func(line string) string {
		return strings.Replace(tool(""), `    command: "true"`+"\n", line, 1)
	}
	const lsAbove = "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    description: \"one\u2028two\"\n  - tool: t\n    command: ! grep -q x f\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> t\n    - t -> done\n"
	for name, src := range map[string]string{
		"a negated command":             command("    command: ! grep -q x f\n"),
		"a negated postcondition":       tool("    postcondition: ! test -f build.lock\n"),
		"before a double-quoted value":  command("    command: ! \"grep -q x f\"\n"),
		"before a single-quoted value":  tool("    postcondition: ! 'test -f build.lock'\n"),
		"before a literal block":        command("    command: ! |\n      grep -q x f\n"),
		"before a folded block":         command("    command: ! >\n      grep -q x f\n"),
		"with lines ended by a lone CR": strings.ReplaceAll(command("    command: ! grep -q x f\n"), "\n", "\r"),
		"below a line separator":        lsAbove,
		"below a next-line character":   strings.Replace(lsAbove, "\u2028", "\u0085", 1),
		"on the first line, a BOM":      "\ufeff! dsl: 2\n" + agentDoc("")[len("dsl: 2\n"):],
		"after a key that is not ASCII": "dsl: 2\nnodes:\n  - tool: c\n    action: forgejo.issue.comment\n    connection: forge-main\n    params:\n      \"clé\": ! valeur\nworkflow:\n  name: w\n  entry: c\n  edges:\n    - c -> done\n",
	} {
		if ok, got := refusedWith(src, parser.DiagAuthorDocument, "YAML reads a `!` before a value as a tag and drops it"); !ok {
			t.Errorf("%s: want E050 naming the dropped `!`, got:\n%s", name, got)
		}
	}
	quoted := Parse("x.bot.yaml", []byte(tool("    postcondition: '! test -f build.lock'\n")))
	if quoted.HasErrors() || quoted.File.Tools[0].Postcondition != "! test -f build.lock" {
		t.Fatalf("the quoted value does not read with its `!`: %v", quoted.Diagnostics)
	}
	block := Parse("x.bot.yaml", []byte(command("    command: |\n      ! grep -q x f\n")))
	if block.HasErrors() || block.File.Tools[0].Command != "! grep -q x f\n" {
		t.Fatalf("a block whose text starts with `!` does not read with it: %v", block.Diagnostics)
	}
	for name, src := range map[string]string{
		"a `!` inside a value": tool("    postcondition: echo done!\n"),
		// Cut on LF alone, the lines after the separator are read one off,
		// and the `!` of the next line falls where `hello` starts.
		"a line separator above a value with `!` below it": "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    description: \"one\u2028two\"\n    system: hello\n    user: \"h!!!!!!!!\"\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n",
	} {
		if res := Parse("x.bot.yaml", []byte(src)); res.HasErrors() {
			t.Errorf("%s is refused: %v", name, res.Diagnostics)
		}
	}
	units := utf16.Encode([]rune(command("    command: ! grep -q x f\n")))
	utf16le := []byte{0xFF, 0xFE}
	for _, u := range units {
		utf16le = append(utf16le, byte(u), byte(u>>8))
	}
	for name, src := range map[string]string{
		"a UTF-16 document":            string(utf16le),
		"a Latin-1 byte on line three": "dsl: 2\nnodes:\n  - agent: caf\xe9\n",
	} {
		if ok, got := refusedWith(src, parser.DiagAuthorDocument, "the document is not UTF-8 text"); !ok {
			t.Errorf("%s: want E050 refusing a text that is not UTF-8, got:\n%s", name, got)
		}
	}
	if d := Parse("x.bot.yaml", []byte("dsl: 2\nnodes:\n  - agent: caf\xe9\n")).Diagnostics; len(d) != 1 || d[0].Line != 3 {
		t.Errorf("the byte that is not UTF-8 is not placed on its line: %v", d)
	}
}

// The scanner messages an author of the YAML twin meets name the spelling
// that avoids them: a template or a bracket written unquoted, a quote that
// closes early, an apostrophe inside single quotes (written twice), a text
// ending in `:`, a tab where YAML indents with spaces. A mapping written as
// a key names the template only when the value starts with `{{`.
func TestTheYAMLTwinsCommonTrapsNameTheirSpelling(t *testing.T) {
	edge := func(line string) string {
		return "dsl: 2\nnodes:\n  - agent: a\n    model: m\n  - agent: b\n    model: m\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - " + line + "\n    - b -> done\n"
	}
	for name, tc := range map[string]struct{ src, want string }{
		"a template written unquoted":        {agentDoc("    user: {{input.task}}\n"), "a value that starts with `{{` (a template) is read by YAML as a mapping: quote the whole value"},
		"a template leading a command":       {"dsl: 2\nnodes:\n  - tool: t\n    command: {{vars.cmd}} --flag\nworkflow:\n  name: w\n  entry: t\n  edges:\n    - t -> done\n", "a value that starts with `{` or `[`"},
		"a bracket leading a description":    {agentDoc("    description: [WIP] review\n"), "a value that starts with `{` or `[`"},
		"an apostrophe inside single quotes": {edge(`'a -> b with { s: "it's {{outputs.a.x}}" }'`), "a `'` inside written twice (`''`)"},
		"a text ending in a colon":           {agentDoc("    description: Steps:\n"), "a text ending in `:`"},
		"a tab indenting a list item":        {"dsl: 2\nnodes:\n\t- agent: a\n\t  model: m\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n", "a tab indents the line, where YAML takes spaces only"},
		"a tab after a key's indentation":    {"dsl: 2\nnodes:\n  - agent: a\n\tmodel: m\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n", "YAML indents with spaces only: replace the tab"},
		"a mapping as a key, no template":    {agentDoc("    {a: b}: c\n"), "a key is a plain word, not a mapping"},
	} {
		t.Run(name, func(t *testing.T) {
			if ok, got := refusedWith(tc.src, parser.DiagAuthorDocument, tc.want); !ok {
				t.Fatalf("want E050 saying %q, got:\n%s", tc.want, got)
			}
		})
	}
	if ok, got := refusedWith(agentDoc("    {a: b}: c\n"), parser.DiagAuthorDocument, "template"); ok {
		t.Fatalf("a mapping as a key with no `{{` is said to be a template:\n%s", got)
	}
	if ok, got := refusedWith(agentDoc("    description: <<\n"), parser.DiagAuthorValue, "`<<` reads as YAML's merge key — quote it"); !ok {
		t.Fatalf("a plain `<<` where text is taken is not named YAML's merge key:\n%s", got)
	}
	if res := Parse("x.bot.yaml", []byte(edge(`'a -> b with { s: "it''s {{outputs.a.x}}" }'`))); res.HasErrors() {
		t.Fatalf("the apostrophe written twice is refused: %v", res.Diagnostics)
	}
}

// Comments lists a document's comments in the order they are written — a
// node's head and line comments, then its children's, then its foot; a
// mapping pair's foot comment, which yaml.v3 hangs on the key, below every
// comment of the value; a flow collection's, the ones yaml.v3 drops
// included — so the first a note names is the first line an author sees:
// the one a ` #` inside a plain value started, above the comment written
// under it; and none is missing, since fmt refuses a document by them.
func TestCommentsAreListedInTheOrderTheyAreWritten(t *testing.T) {
	for name, src := range map[string]string{
		"head, line and foot of the document": "# 1 head of the document\ndsl: 2 # 2 on the header\nnodes:\n  - agent: a # 3 on the agent\n    model: m\n\n# 4 foot of nodes\nworkflow:\n  name: w # 5 on the name\n  entry: a\n  edges:\n    - a -> done\n\n# 6 foot of the document\n",
		"a foot under a value's line comment": "dsl: 2\nvars:\n  x: string # 1\n  # 2 foot of x\n",
		"a foot inside a list item":           "dsl: 2 # 1\nnodes:\n  - agent: a # 2\n    model: m # 3\n    # 4 foot inside the item\n  - agent: b # 5\n    model: m\n# 6 foot at the top\nworkflow: # 7\n  name: w\n  entry: a\n  edges:\n    - a -> done # 8\n# 9 end\n",
		"feet of nested mappings":             "dsl: 2\nworkflow:\n  name: w # 1\n  budget:\n    max_cost_usd: 1 # 2\n    # 3 foot inside the budget\n  # 4 foot of the budget\n  entry: a # 5\n",
		"a ` #` inside a plain value":         "dsl: 2\nnodes:\n  - tool: status\n    command: echo \"see # 1 cut\"\n    # 2 retries are handled by the caller\nworkflow:\n  name: w\n  entry: status\n  edges:\n    - status -> done\n",
		"feet of list items":                  "dsl: 2\nworkflow:\n  edges:\n    - a -> b # 1\n    # 2 foot of the first edge\n    - b -> done # 3\n  # 4 after the edges\n  name: w # 5\n",
		"heads of keys and values":            "dsl: 2\n# 1 head of vars\nvars:\n  # 2 head of x\n  x:\n    # 3 head of the value\n    type: string # 4\n  # 5 foot of x\n# 6 head of nodes\nnodes: []\n",
		// Inside a flow collection, read off the source: yaml.v3 drops the
		// comment after an opening bracket and hangs the one after the
		// closing bracket on the collection.
		"a flow mapping, comments inside":  "dsl: 2 # 1\nnodes:\n  - agent: a\n    model: m\n    sandbox:\n      env: {A: x, # 2\n        B: y} # 3\n# 4\n",
		"a flow sequence, comments inside": "dsl: 2 # 1\nnodes:\n  - agent: a\n    model: m\n    tools: [a, # 2\n      b] # 3\n# 4\n",
		"right after an opening bracket":   "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools: [ # 1\n      read_file, bash]\n",
		"right after an opening brace":     "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    sandbox:\n      env: { # 1\n        A: x, # 2\n        B: y } # 3\n",
		"an own line inside a flow list":   "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools: [\n      # 1 own line\n      read_file, bash]\n",
		"a quoted #, not a comment":        "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools: [\"a #b\", 'c #d', e] # 1\n",
		"an apostrophe in a flow plain":    "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools: [it's, b, # 1\n      c] # 2\n    description: d # 3\n",
		"a flow nested in a flow":          "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    sandbox:\n      env: {A: [x, # 1\n        y], # 2\n        B: z} # 3\n",
		"under a flow mapping's line":      "dsl: 2\nnodes:\n  - emit: e\n    with: {a: b} # 1\n    # 2 under with\n    event: x # 3\n",
		// yaml.v3 needs no blank where a token may start.
		"right after a comma":           "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools: [read_file,# 1 the only safe tool\n      bash]\n",
		"right after a bracket":         "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools: [# 1 kept\n      read_file, bash]\n",
		"right after a closed quote":    "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools: [\"read_file\"# 1 kept\n      , bash]\n",
		"a quote after an explicit key": "dsl: 2\ncontracts:\n  c:\n    version: 1\n    inputs:\n      n:\n        type: json\n        default: {? \"a #b\" : 1} # 1\n",
		// A `:` with no blank after it is text inside a plain scalar, and
		// the quote after it too; after a quoted key it is the indicator,
		// and the quote after it opens a scalar.
		"a quote after a colon in a plain":        "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools: [a, b:'c, # 1\n      d] # 2\n",
		"a double quote after a colon in a plain": "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools: [a, b:\"c, # 1\n      d]\n",
		"a permission pattern":                    "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    allow: [Bash(git log:'*), # 1\n      Read(**)]\n",
		"a with value":                            "dsl: 2\nnodes:\n  - emit: e\n    event: ev\n    with: {a: b:'c, # 1\n      d: e}\n",
		"a hash after a colon in a plain":         "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools: [a:#b, c] # 1\n",
		"a hash after a plain word and a blank":   "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools: [a # 1\n      , b] # 2\n",
		"a quote after a quoted key's colon":      "dsl: 2\nnodes:\n  - emit: e\n    event: ev\n    with: {\"a\":'b #c', d: e} # 1\n",
	} {
		got := Comments([]byte(src))
		for i, c := range got {
			if !strings.HasPrefix(c, "# ") || c[2] != byte('1'+i) {
				t.Fatalf("%s: comments out of the written order: %q", name, got)
			}
		}
		if want := strings.Count(src, "# "); len(got) != want {
			t.Fatalf("%s: want the %d comments, got %q", name, want, got)
		}
	}
}
