package spec_test

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// The registry describes more than property tables: the parts of a
// declaration header (a group's parameters, a use's prefix and map), the
// parts of an entry line (a var's type, constraint and default; a port's
// type; a resource's capacity), the node kinds a group holds. Each of these
// structures is held to the parser here, on witnesses: an accepted witness
// parses clean and its values reach the document; a refused one draws a
// diagnostic; and every part the registry names is exercised by a witness,
// so a part added on one side without the other fails.

// entryProbes is, for each block made of author-named entries, the document
// that hosts one entry, with `%s` where the entry line goes (continuation
// lines are re-indented under it).
var entryProbes = map[string]string{
	"vars":              "vars:\n  %s\n",
	"presets":           "presets:\n  %s\n",
	"attachments":       "attachments:\n  %s\n",
	"secrets":           "secrets:\n  %s\n",
	"resources":         "workflow w:\n  resources:\n    %s\n",
	"expr":              "compute c:\n  expr:\n    %s\n",
	"params":            "tool t:\n  params:\n    %s\n",
	"cursors":           "agent a:\n  cursors:\n    %s\n",
	"cursor.values":     "cursor c:\n  values:\n    %s\n",
	"cursor.bands":      "cursor c:\n  bands:\n    %s\n",
	"schema":            "schema s:\n  %s\n",
	"contract.ports":    "contract c:\n  inputs:\n    %s\n",
	"contract.criteria": "contract c:\n  criteria:\n    %s\n",
	"contract.effects":  "contract c:\n  effects:\n    %s\n",
	"fallbacks":         "agent a:\n  fallbacks:\n    %s\n",
}

// entryWitness is one entry as an author writes it: the parts of the entry
// line it exercises (registry Field names; a nested entries' part is
// `entries.<name>`), and the values the parsed document must carry.
type entryWitness struct {
	line   string
	parts  []string
	expect []string
}

var entryWitnesses = map[string]struct {
	accepted []entryWitness
	refused  []string
}{
	"vars": {
		accepted: []entryWitness{
			{"zz_k: string", []string{"type"}, []string{"zz_k"}},
			{"zz_k: int = 77", []string{"type", "default"}, []string{"zz_k", "77"}},
			{"zz_k: float = 1.5", []string{"type", "default"}, []string{"1.5"}},
			{"zz_k: bool = true", []string{"type", "default"}, []string{"zz_k"}},
			{`zz_k: json = "[1]"`, []string{"type", "default"}, []string{"[1]"}},
			{`zz_k: string[] = "[\"zz_a\"]"`, []string{"type", "default"}, []string{"zz_a"}},
			{`zz_k: string [enum: "zz_a", "zz_b"] = "zz_a"`, []string{"type", "enum", "default"}, []string{"zz_a", "zz_b"}},
			{`zz_k: string [matching: "^zz_[a-z]+$"]`, []string{"type", "matching"}, []string{"^zz_[a-z]+$"}},
			{`zz_k: string [enum: "zz_a"] [matching: "^zz"] = "zz_a"`, []string{"type", "enum", "matching", "default"}, []string{"^zz"}},
			{`zz_k: string [matching: "^zz"] [enum: "zz_a"]`, []string{"type", "enum", "matching"}, []string{"zz_a", "^zz"}},
		},
		refused: []string{"zz_k: zz_type", "zz_k: string = [a]", "zz_k: string [enum: zz_a]", "zz_k: string = zz_word", "zz_k", "zz_k:",
			"zz_k: string [matching: zz_bare]", `zz_k: string [matching: ""]`, `zz_k: string [enum: "a"] [enum: "b"]`, `zz_k: string [matching: "a"] [matching: "b"]`},
	},
	"presets": {
		accepted: []entryWitness{
			{"zz_p:\n  zz_v: 77", []string{"entries.value"}, []string{"zz_p", "zz_v", "77"}},
			{"zz_p:\n  zz_v: \"zz_s\"\n  zz_w: true", []string{"entries.value"}, []string{"zz_s", "zz_w"}},
		},
		refused: []string{"zz_p: 1", "zz_p:\n  zz_v: [a]", "zz_p:\n  zz_v: zz_word"},
	},
	"attachments": {
		accepted: []entryWitness{
			{"zz_k: file", []string{"type"}, []string{"zz_k"}},
			{"zz_k: image", []string{"type"}, []string{"zz_k"}},
			{"zz_k: file\n  required: true", []string{"type"}, []string{"zz_k"}},
		},
		refused: []string{"zz_k: string", "zz_k", `zz_k: "file"`, "zz_k: 1"},
	},
	"secrets": {
		accepted: []entryWitness{
			{`zz_k: "zz_val"`, []string{"value"}, []string{"zz_val"}},
			{"zz_k: zz_word", []string{"value"}, []string{"zz_word"}},
			{"zz_k:", nil, []string{"zz_k"}},
			{"zz_k:\n  as: file", nil, []string{"zz_k", "file"}},
		},
		refused: []string{"zz_k: 1", "zz_k: [a]", "zz_k: 1.5"},
	},
	"resources": {
		accepted: []entryWitness{
			{"zz_k: 3", []string{"capacity"}, []string{"zz_k", "3"}},
			{`zz_k: ["zz_a", "zz_b"]`, []string{"capacity"}, []string{"zz_a", "zz_b"}},
			{"zz_k:\n  - \"zz_a\"\n  - \"zz_b\"", []string{"capacity"}, []string{"zz_a", "zz_b"}},
		},
		refused: []string{`zz_k: "3"`, "zz_k: zz_word", "zz_k: 1.5", "zz_k"},
	},
	"expr": {
		accepted: []entryWitness{
			{`zz_k: "zz_x + 1"`, []string{"expression"}, []string{"zz_x + 1"}},
		},
		refused: []string{"zz_k: 1", "zz_k: [a]", "zz_k", "zz_k: zz_word"},
	},
	"params": {
		accepted: []entryWitness{
			{`zz_k: "zz_v"`, []string{"value"}, []string{"zz_k", "zz_v"}},
			{"zz_k: 30s", []string{"value"}, []string{"30s"}},
			{"zz_k: 3", []string{"value"}, []string{"3"}},
			{`"zz-k": "zz_v"`, []string{"value"}, []string{"zz-k"}},
		},
		refused: []string{"zz_k: [a]", "zz_k", "1: \"v\""},
	},
	"cursors": {
		accepted: []entryWitness{
			{"zz_k: zz_val", []string{"value"}, []string{"zz_k", "zz_val"}},
			{"zz_k: 0.7", []string{"value"}, []string{"0.7"}},
			{"zz_k: 1", []string{"value"}, []string{"zz_k"}},
			{`zz_k: "${ZZ}"`, []string{"value"}, []string{"${ZZ}"}},
		},
		refused: []string{"zz_k: [a]", "zz_k:", "zz_k"},
	},
	"cursor.values": {
		accepted: []entryWitness{
			{`zz_k: "zz frag"`, []string{"prompt"}, []string{"zz frag"}},
			{"zz_k: zz_word", []string{"prompt"}, []string{"zz_word"}},
		},
		refused: []string{"zz_k: 1", "zz_k: [a]", "zz_k"},
	},
	"cursor.bands": {
		accepted: []entryWitness{
			{`"0..0.5": "zz frag"`, []string{"prompt"}, []string{"0..0.5", "zz frag"}},
		},
		refused: []string{`zz_k: "f"`, `"0..1": 1`, `"0..1": [a]`},
	},
	"schema": {
		accepted: []entryWitness{
			{"zz_k: string", []string{"type"}, []string{"zz_k"}},
			{"zz_k: file", []string{"type"}, []string{"zz_k"}},
			{"zz_k: string[]", []string{"type"}, []string{"zz_k"}},
			{`zz_k: string [enum: "zz_a", "zz_b"]`, []string{"type", "enum"}, []string{"zz_a", "zz_b"}},
		},
		refused: []string{"zz_k: zz_type", "zz_k: string [enum: zz_a]", `zz_k: string = "x"`, "zz_k", "zz_k: 1"},
	},
	"contract.ports": {
		accepted: []entryWitness{
			{"zz_k: string", []string{"type"}, []string{"zz_k", "string"}},
			{"zz_k: string[]", []string{"type"}, []string{"string[]"}},
			{"zz_k: zz_schema[][]", []string{"type"}, []string{"zz_schema[][]"}},
			{"zz_k: int\n  required: false", []string{"type"}, []string{"zz_k"}},
		},
		refused: []string{`zz_k: "string"`, "zz_k: 1", "zz_k", "zz_k:"},
	},
	"contract.criteria": {
		accepted: []entryWitness{
			{"zz_k:", nil, []string{"zz_k"}},
			{"zz_k:\n  kind: min_length", nil, []string{"zz_k", "min_length"}},
		},
		refused: []string{"zz_k: 1", "zz_k: min_length", "zz_k"},
	},
	"contract.effects": {
		accepted: []entryWitness{
			{"zz_k:", nil, []string{"zz_k"}},
			{"zz_k:\n  paid: true", nil, []string{"zz_k"}},
		},
		refused: []string{"zz_k: 1", "zz_k: paid", "zz_k"},
	},
	"fallbacks": {
		accepted: []entryWitness{
			{"zz_k:\n  backend: claw", nil, []string{"zz_k", "claw"}},
			{"zz_k:", nil, []string{"zz_k"}},
		},
		refused: []string{"zz_k: 1", "zz_k: claw", "zz_k"},
	},
}

// fieldNames lists the parts of an entries structure, nested ones as
// `entries.<name>`.
func fieldNames(e *spec.Entries) []string {
	var out []string
	for _, f := range e.Fields {
		out = append(out, f.Name)
	}
	if e.Entries != nil {
		for _, n := range fieldNames(e.Entries) {
			out = append(out, "entries."+n)
		}
	}
	sort.Strings(out)
	return out
}

func entryDoc(tmpl, line string) string {
	at := strings.Index(tmpl, "%s")
	indent := tmpl[strings.LastIndex(tmpl[:at], "\n")+1 : at]
	return fmt.Sprintf(tmpl, strings.ReplaceAll(line, "\n", "\n"+indent))
}

// TestEntriesMatchTheParser holds each entries structure to the parser:
// every accepted witness parses clean and its values reach the document,
// every refused one draws a diagnostic, and the parts the witnesses
// exercise are exactly the parts the registry names.
func TestEntriesMatchTheParser(t *testing.T) {
	for kind, tmpl := range entryProbes {
		k, ok := spec.Lookup(kind)
		if !ok || k.Entries == nil {
			t.Errorf("%s: not a registered kind with entries", kind)
			continue
		}
		w, ok := entryWitnesses[kind]
		if !ok {
			t.Errorf("%s: no entry witnesses", kind)
			continue
		}
		exercised := map[string]bool{}
		for _, a := range w.accepted {
			doc := entryDoc(tmpl, a.line)
			res := parser.Parse("probe.bot", doc)
			if len(res.Diagnostics) > 0 {
				t.Errorf("%s: accepted entry %q drew %v", kind, a.line, res.Diagnostics)
				continue
			}
			raw, err := ast.MarshalFile(res.File)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range a.expect {
				if !strings.Contains(string(raw), e) {
					t.Errorf("%s: entry %q parsed but %q did not reach the document", kind, a.line, e)
				}
			}
			for _, p := range a.parts {
				exercised[p] = true
			}
		}
		for _, r := range w.refused {
			if res := parser.Parse("probe.bot", entryDoc(tmpl, r)); len(res.Diagnostics) == 0 {
				t.Errorf("%s: refused entry %q is accepted", kind, r)
			}
		}
		got := make([]string, 0, len(exercised))
		for p := range exercised {
			got = append(got, p)
		}
		sort.Strings(got)
		if want := fieldNames(k.Entries); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: the witnesses exercise parts %v, the registry names %v", kind, got, want)
		}
	}
	for _, k := range spec.Kinds {
		if k.Entries == nil {
			continue
		}
		if _, ok := entryProbes[k.Name]; !ok {
			t.Errorf("kind %q has entries but no entry probe", k.Name)
		}
	}
}

// TestEntriesKeysAreWhatTheParserNames: a quoted key is refused where the
// registry says the key is a bare name, and accepted where it says a
// quoted key is one (a band's range, a vendor's parameter name).
func TestEntriesKeysAreWhatTheParserNames(t *testing.T) {
	quotedKey := map[string]string{ // an entry with a quoted key and a value every form of that block takes
		"vars":              `"zz-k": string`,
		"presets":           "\"zz-p\":\n  zz_v: 1",
		"attachments":       `"zz-k": file`,
		"secrets":           `"zz-k": "v"`,
		"resources":         `"zz-k": 1`,
		"expr":              `"zz-k": "1"`,
		"params":            `"zz-k": "v"`,
		"cursors":           `"zz-k": 1`,
		"cursor.values":     `"zz-k": "f"`,
		"cursor.bands":      `"0..1": "f"`,
		"schema":            `"zz-k": string`,
		"contract.ports":    `"zz-k": string`,
		"contract.criteria": `"zz-k":`,
		"contract.effects":  `"zz-k":`,
		"fallbacks":         `"zz-k":`,
	}
	for kind, tmpl := range entryProbes {
		k, _ := spec.Lookup(kind)
		line, ok := quotedKey[kind]
		if !ok {
			t.Errorf("%s: no quoted-key witness", kind)
			continue
		}
		res := parser.Parse("probe.bot", entryDoc(tmpl, line))
		accepted := len(res.Diagnostics) == 0
		quotedOK := k.Entries.Key == spec.String || k.Entries.Key == spec.StringOrIdent
		if accepted != quotedOK {
			t.Errorf("%s: key form %q, yet a quoted key %q is accepted=%v (%v)", kind, k.Entries.Key, line, accepted, res.Diagnostics)
		}
	}
}

// TestHeadersMatchTheParser: the header parts the registry names for a
// group and a use are what the parser reads, and nothing else.
func TestHeadersMatchTheParser(t *testing.T) {
	g, _ := spec.Lookup("group")
	u, _ := spec.Lookup("use")
	if g.Header == nil || strings.Join(headerNames(g), ",") != "params" {
		t.Fatalf("group header: %+v", g.Header)
	}
	if u.Header == nil || strings.Join(headerNames(u), ",") != "as,with" || u.Entries != nil || len(u.Properties) != 0 {
		t.Fatalf("use header: %+v", u.Header)
	}
	body := "\n  agent x:\n    model: \"m\"\n  x -> done\n"
	res := parser.Parse("g.bot", "group g(zz_a, zz_b):"+body+"use g as zz_p with { zz_a: \"v\", zz_b: 3 }\n")
	if len(res.Diagnostics) != 0 {
		t.Fatalf("header witnesses: %v", res.Diagnostics)
	}
	if got := res.File.Groups[0].Params; strings.Join(got, ",") != "zz_a,zz_b" {
		t.Errorf("params: %v", got)
	}
	if use := res.File.Uses[0]; use.Group != "g" || use.Prefix != "zz_p" || len(use.With) != 2 || use.With[1].Value != "3" {
		t.Errorf("use: %+v", use)
	}
	// The optional parts left out: still a group, still a use.
	res = parser.Parse("g.bot", "group g:"+body+"use g as zz_p\n")
	if len(res.Diagnostics) != 0 || len(res.File.Groups[0].Params) != 0 || res.File.Uses[0].With != nil {
		t.Errorf("without the optional parts: %v %+v %+v", res.Diagnostics, res.File.Groups[0], res.File.Uses[0])
	}
	// A required part left out, or a part of the wrong form, is refused.
	for _, src := range []string{
		"group g:" + body + "use g\n",
		"group g:" + body + "use g as\n",
		"group g:" + body + "use g as zz_p with 1\n",
		"group g(1):" + body,
		"group g(\"a\"):" + body,
	} {
		if res := parser.Parse("g.bot", src); len(res.Diagnostics) == 0 {
			t.Errorf("accepted: %q", src)
		}
	}
}

func headerNames(k spec.Kind) []string {
	var out []string
	for _, f := range k.Header.Fields {
		out = append(out, f.Name)
	}
	return out
}

// TestAGroupHoldsWhatTheRegistrySays: each node kind the registry lists
// under Holds is accepted inside a group, every other node kind is refused
// by name.
func TestAGroupHoldsWhatTheRegistrySays(t *testing.T) {
	g, _ := spec.Lookup("group")
	if !g.Edges || len(g.Holds) == 0 {
		t.Fatalf("group: %+v", g)
	}
	minimal := map[string]string{
		"agent":         "model: \"m\"",
		"judge":         "model: \"m\"",
		"router":        "mode: fan_out_all",
		"human":         "interaction: human",
		"tool":          "command: \"x\"",
		"compute":       "expr:\n      f: \"1\"",
		"subbot":        "source: \"c.bot\"",
		"emit":          "event: \"e\"",
		"wait":          "event: \"e\"\n    timeout: \"1s\"",
		"await_answers": "timeout: \"1s\"",
		"fail":          "code: X",
	}
	holds := map[string]bool{}
	for _, h := range g.Holds {
		holds[h] = true
	}
	for _, k := range spec.Kinds {
		if k.Role != spec.Node {
			continue
		}
		body, ok := minimal[k.Name]
		if !ok {
			t.Errorf("%s: no minimal body for the group probe", k.Name)
			continue
		}
		res := parser.Parse("g.bot", "group g:\n  "+k.Name+" x:\n    "+body+"\n")
		refused := false
		for _, d := range res.Diagnostics {
			if strings.Contains(d.Message, "cannot be declared inside a group") {
				refused = true
			}
		}
		switch {
		case holds[k.Name] && len(res.Diagnostics) > 0:
			t.Errorf("%s: the registry says a group holds it, the parser says %v", k.Name, res.Diagnostics)
		case !holds[k.Name] && !refused:
			t.Errorf("%s: the registry says a group does not hold it, the parser did not refuse it by name: %v", k.Name, res.Diagnostics)
		}
	}
	// The workflow carries edges too; a prompt's body is text; a use has no body.
	for _, c := range []struct {
		kind  string
		check func(k spec.Kind) bool
	}{
		{"workflow", func(k spec.Kind) bool { return k.Edges && !k.Text }},
		{"prompt", func(k spec.Kind) bool { return k.Text && k.Entries == nil && len(k.Properties) == 0 }},
		{"use", func(k spec.Kind) bool { return !k.Text && !k.Edges && k.Entries == nil && k.Header != nil }},
	} {
		k, _ := spec.Lookup(c.kind)
		if !c.check(k) {
			t.Errorf("%s: %+v", c.kind, k)
		}
	}
}

var acceptedWordsRe = regexp.MustCompile(`\(([a-z_]+(?:, [a-z_]+)+)\)`)

// TestEnumValuesAreTheParsersList holds each enum's value list to the list
// the parser ENUMERATES in its own refusal — the one place the parser
// spells every word it accepts — so a value accepted but unlisted (as
// human_or_host was) reddens, where accepting the listed values and
// refusing one bogus word could not see it.
func TestEnumValuesAreTheParsersList(t *testing.T) {
	n := 0
	for kind, tmpl := range probes {
		k, _ := spec.Lookup(kind)
		for _, p := range k.Properties {
			if p.Form != spec.Enum && p.Form != spec.EnumOrEnv {
				continue
			}
			n++
			res := parser.Parse("probe.bot", fmt.Sprintf(strings.Replace(tmpl, "%s: 1", "%s", 1), p.Name+": zz_bogus"))
			var listed []string
			for _, d := range res.Diagnostics {
				if m := acceptedWordsRe.FindStringSubmatch(d.Message); m != nil {
					listed = strings.Split(m[1], ", ")
					break
				}
			}
			if listed == nil {
				t.Errorf("%s.%s: the parser's refusal does not enumerate the accepted words: %v", kind, p.Name, res.Diagnostics)
				continue
			}
			want := append([]string(nil), p.Values...)
			sort.Strings(listed)
			sort.Strings(want)
			if strings.Join(listed, ",") != strings.Join(want, ",") {
				t.Errorf("%s.%s: the parser accepts %v, the registry lists %v", kind, p.Name, listed, want)
			}
		}
	}
	if n < 8 {
		t.Fatalf("only %d enum properties checked", n)
	}
}
