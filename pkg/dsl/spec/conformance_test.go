package spec_test

import (
	"fmt"
	goast "go/ast"
	goparser "go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// probes puts `<name>: 1` in the body of each kind that has a fixed property
// table, as the minimal document the parser reads. The value is irrelevant:
// E012 is decided on the NAME, before the value is read, so an accepted name
// draws at most a value error while a refused one draws E012 — the one
// signal this test reads, from the REAL parser, which is what makes the
// registry unable to drift from it: a property added to the parser without
// its registry line fails here, and so does a registry line the parser does
// not honour.
var probes = map[string]string{
	"agent":           "agent x:\n  %s: 1\n",
	"judge":           "judge x:\n  %s: 1\n",
	"router":          "router x:\n  %s: 1\n",
	"human":           "human x:\n  %s: 1\n",
	"tool":            "tool x:\n  %s: 1\n",
	"compute":         "compute x:\n  %s: 1\n",
	"subbot":          "subbot x:\n  %s: 1\n",
	"emit":            "emit x:\n  %s: 1\n",
	"wait":            "wait x:\n  %s: 1\n",
	"await_answers":   "await_answers x:\n  %s: 1\n",
	"fail":            "fail x:\n  %s: 1\n",
	"workflow":        "workflow w:\n  %s: 1\n",
	"supervisor":      "supervisor s:\n  %s: 1\n",
	"cursor":          "cursor c:\n  %s: 1\n",
	"mcp_server":      "mcp_server m:\n  %s: 1\n",
	"auth":            "mcp_server m:\n  auth:\n    %s: 1\n",
	"mcp":             "workflow w:\n  mcp:\n    %s: 1\n",
	"budget":          "workflow w:\n  budget:\n    %s: 1\n",
	"compaction":      "workflow w:\n  compaction:\n    %s: 1\n",
	"memory":          "agent a:\n  memory:\n    %s: 1\n",
	"sandbox":         "workflow w:\n  sandbox:\n    %s: 1\n",
	"sandbox.build":   "workflow w:\n  sandbox:\n    build:\n      %s: 1\n",
	"sandbox.network": "workflow w:\n  sandbox:\n    network:\n      %s: 1\n",
	"recovery":        "tool t:\n  recovery:\n    %s: 1\n",
	"fallback":        "agent a:\n  fallbacks:\n    r:\n      %s: 1\n",
	"attachment":      "attachments:\n  a: file\n    %s: 1\n",
	"secret":          "secrets:\n  s:\n    %s: 1\n",
	"cursors":         "agent a:\n  cursors:\n    %s: 1\n",
}

// freeEntryProbes is one arbitrary entry name in each block whose body is
// author-named entries; the parser must take it without a diagnostic.
var freeEntryProbes = map[string]string{
	"vars":          "vars:\n  zz_probe: string\n",
	"presets":       "presets:\n  zz_probe:\n    a: 1\n",
	"attachments":   "attachments:\n  zz_probe: file\n",
	"secrets":       "secrets:\n  zz_probe: \"v\"\n",
	"resources":     "workflow w:\n  resources:\n    zz_probe: 1\n",
	"expr":          "compute c:\n  expr:\n    zz_probe: \"1\"\n",
	"params":        "tool t:\n  params:\n    zz_probe: \"1\"\n",
	"cursors":       "agent a:\n  cursors:\n    zz_probe: 1\n",
	"cursor.values": "cursor c:\n  values:\n    zz_probe: \"f\"\n",
	"cursor.bands":  "cursor c:\n  bands:\n    \"0..1\": \"f\"\n",
	"schema":        "schema s:\n  zz_probe: string\n",
}

func parserAccepts(tmpl, name string) bool {
	res := parser.Parse("probe.bot", fmt.Sprintf(tmpl, name))
	for _, d := range res.Diagnostics {
		if d.Code == parser.DiagUnknownProperty {
			return false
		}
	}
	return true
}

// candidateNames is every name the parser could accept as a property: the
// lexer's keywords (a property matched by token type is reachable only by
// its keyword) and every identifier-shaped string literal in the parser's
// sources (a property matched by `case "name":` or `t.Value == "name"`),
// plus the registry's own names. A superset costs nothing — a name nobody
// accepts and nobody lists is consistent.
func candidateNames(t *testing.T) []string {
	t.Helper()
	set := map[string]bool{}
	for _, k := range parser.Keywords() {
		set[k] = true
	}
	for _, k := range spec.Kinds {
		for _, n := range k.Names() {
			set[n] = true
		}
	}
	files, err := filepath.Glob(filepath.Join("..", "parser", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("parser sources: %v (%d files)", err, len(files))
	}
	ident := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := goparser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		goast.Inspect(af, func(n goast.Node) bool {
			lit, ok := n.(*goast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && ident.MatchString(s) {
				set[s] = true
			}
			return true
		})
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// TestRegistryMatchesTheParser holds the registry to the parser in both
// directions, kind by kind, over every candidate name.
func TestRegistryMatchesTheParser(t *testing.T) {
	names := candidateNames(t)
	if len(names) < 100 {
		t.Fatalf("only %d candidate names — the sweep is not reading the parser", len(names))
	}
	for kind, tmpl := range probes {
		k, ok := spec.Lookup(kind)
		if !ok {
			t.Errorf("probe %q names a kind the registry does not have", kind)
			continue
		}
		free := k.Entries != nil // any name is an entry: only the listed ones are checked
		for _, name := range names {
			accepted := parserAccepts(tmpl, name)
			listed := k.Has(name)
			switch {
			case listed && !accepted:
				t.Errorf("%s: the registry lists %q but the parser refuses it", kind, name)
			case accepted && !listed && !free:
				t.Errorf("%s: the parser accepts %q but the registry does not list it", kind, name)
			}
		}
	}
}

// sampleValues writes the property line (or lines) with a WELL-FORMED value
// of the property's declared form — EVERY listed value when the form names
// some, since each is rendered to authors as accepted — so the parser's
// acceptance is proven on a clean document rather than inferred from the
// absence of E012 alone: a listed property whose form was wrong, a listed
// value the parser refuses, or a name refused through another code, draws a
// diagnostic here. Continuation lines are indented relative to the property.
func sampleValues(p spec.Property) []string {
	each := func(values []string) []string {
		out := make([]string, 0, len(values))
		for _, v := range values {
			out = append(out, p.Name+": "+v)
		}
		return out
	}
	switch p.Form {
	case spec.String:
		if len(p.Values) > 0 {
			// A quoted value the compiler narrows (memory.visibility): each
			// listed word, in the quotes the parser wants.
			quoted := make([]string, 0, len(p.Values))
			for _, v := range p.Values {
				quoted = append(quoted, `"`+v+`"`)
			}
			return each(quoted)
		}
		return []string{p.Name + `: "x"`}
	case spec.Ident, spec.StringOrIdent:
		if len(p.Values) > 0 {
			return each(p.Values)
		}
		return []string{p.Name + ": x"}
	case spec.Int, spec.Number:
		return []string{p.Name + ": 1"}
	case spec.Bool:
		return []string{p.Name + ": true", p.Name + ": false"}
	case spec.Enum, spec.BlockOrIdent:
		return each(p.Values)
	case spec.IdentList, spec.ToolList, spec.MixedList:
		if len(p.Values) > 0 {
			return []string{p.Name + ": [" + strings.Join(p.Values, ", ") + "]"}
		}
		return []string{p.Name + ": [a]"}
	case spec.StringList, spec.SkillList:
		return []string{p.Name + `: ["a"]`}
	case spec.IdentOrList:
		return []string{p.Name + ": a", p.Name + ": [a, b]"}
	case spec.Map:
		return []string{p.Name + `: { A: "v" }`}
	case spec.WithMap:
		return []string{p.Name + ` { k: "v" }`}
	case spec.Block:
		switch p.Body {
		case "expr":
			return []string{"expr:\n  f: \"1\""}
		case "cursor.values":
			return []string{"values:\n  a: \"f\""}
		case "cursor.bands":
			return []string{"bands:\n  \"0..1\": \"f\""}
		case "fallback":
			return []string{"fallbacks:\n  r:\n    backend: \"claw\""}
		}
		return []string{p.Name + ":"} // an empty block: the bare header
	}
	return []string{p.Name + ": 1"}
}

// TestEveryListedPropertyParsesCleanWithItsForm proves the other half of
// "listed ⇒ accepted": with a value of its declared form — each listed value
// in turn — every property of every kind parses with NO diagnostic at all,
// not merely without E012.
func TestEveryListedPropertyParsesCleanWithItsForm(t *testing.T) {
	n := 0
	for kind, tmpl := range probes {
		k, _ := spec.Lookup(kind)
		line := strings.Replace(tmpl, "%s: 1", "%s", 1)
		at := strings.Index(line, "%s")
		indent := line[strings.LastIndex(line[:at], "\n")+1 : at]
		for _, p := range k.Properties {
			for _, sample := range sampleValues(p) {
				n++
				value := strings.ReplaceAll(sample, "\n", "\n"+indent)
				doc := fmt.Sprintf(line, value)
				if res := parser.Parse("probe.bot", doc); len(res.Diagnostics) > 0 {
					t.Errorf("%s.%s (%s): %q drew %v", kind, p.Name, p.Form, doc, res.Diagnostics)
				}
			}
			// An enum's values are the PARSER's: a word outside the list
			// must be refused here, or the list is decoration.
			if p.Form == spec.Enum {
				doc := fmt.Sprintf(line, p.Name+": zz_bogus")
				if res := parser.Parse("probe.bot", doc); len(res.Diagnostics) == 0 {
					t.Errorf("%s.%s (enum): a value outside %v is accepted", kind, p.Name, p.Values)
				}
			}
		}
	}
	if n < 300 {
		t.Fatalf("only %d documents probed — the sweep is not covering the registry", n)
	}
}

// TestEveryFixedTableHasAProbe: a kind added to the registry with properties
// but no probe would be a table nothing holds to the parser.
func TestEveryFixedTableHasAProbe(t *testing.T) {
	for _, k := range spec.Kinds {
		if len(k.Properties) == 0 {
			continue
		}
		if _, ok := probes[k.Name]; !ok {
			t.Errorf("kind %q has a property table but no probe document", k.Name)
		}
	}
}

// TestFreeEntryBlocksTakeAnyName: the registry says these blocks are made of
// author-named entries; the parser must take an arbitrary one cleanly.
func TestFreeEntryBlocksTakeAnyName(t *testing.T) {
	for kind, doc := range freeEntryProbes {
		k, ok := spec.Lookup(kind)
		if !ok {
			t.Errorf("probe %q names a kind the registry does not have", kind)
			continue
		}
		if k.Entries == nil {
			t.Errorf("%s: probed as free entries but the registry declares no Entries", kind)
		}
		res := parser.Parse("probe.bot", doc)
		if len(res.Diagnostics) > 0 {
			t.Errorf("%s: an arbitrary entry drew %v", kind, res.Diagnostics)
		}
	}
	for _, k := range spec.Kinds {
		if k.Entries == nil || k.Name == "prompt" || k.Name == "group" || k.Name == "use" {
			continue
		}
		if _, ok := freeEntryProbes[k.Name]; !ok {
			t.Errorf("kind %q declares free entries but has no probe", k.Name)
		}
	}
}

// TestBlocksNameTheirHostsAndOpeners: a block's Hosts must be kinds the
// registry knows, and its Body references must resolve, or the hints built
// on them point at nothing.
func TestBlocksNameTheirHostsAndOpeners(t *testing.T) {
	for _, k := range spec.Kinds {
		for _, h := range k.Hosts {
			if h == "file" {
				continue
			}
			if _, ok := spec.Lookup(h); !ok {
				t.Errorf("%s: host %q is not a registered kind", k.Name, h)
			}
		}
		if (k.Role == spec.BlockRole || k.Role == spec.Entry) && (k.Opener == "" || len(k.Hosts) == 0) {
			t.Errorf("%s: a block or entry needs an opener and at least one host", k.Name)
		}
		for _, p := range k.Properties {
			if p.Form == spec.Block || p.Form == spec.BlockOrIdent {
				body, ok := spec.Lookup(p.Body)
				if !ok {
					t.Errorf("%s.%s: body kind %q is not registered", k.Name, p.Name, p.Body)
					continue
				}
				hosted := false
				for _, h := range body.Hosts {
					if h == k.Name {
						hosted = true
					}
				}
				if !hosted {
					t.Errorf("%s.%s: body kind %q does not list %q among its hosts", k.Name, p.Name, p.Body, k.Name)
				}
			}
			if p.Form == spec.Enum && len(p.Values) == 0 {
				t.Errorf("%s.%s: an enum needs values", k.Name, p.Name)
			}
		}
	}
}
