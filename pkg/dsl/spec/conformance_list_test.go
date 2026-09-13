package spec_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// Every list-shaped property of every kind reads the same document from its
// inline form and from its `- item` form — derived from the registry, so a
// list path added without the second form fails here.
func TestEveryListPropertyReadsBothForms(t *testing.T) {
	n := 0
	for kind, tmpl := range probes {
		k, ok := spec.Lookup(kind)
		if !ok {
			continue
		}
		line := strings.Replace(tmpl, "%s: 1", "%s", 1)
		at := strings.Index(line, "%s")
		indent := line[strings.LastIndex(line[:at], "\n")+1 : at]
		for _, p := range k.Properties {
			var inline string
			switch p.Form {
			case spec.IdentList, spec.ToolList, spec.MixedList, spec.IdentOrList:
				inline = p.Name + ": [aa, bb]"
			case spec.StringList, spec.SkillList:
				inline = p.Name + `: ["aa", "bb"]`
			default:
				continue
			}
			n++
			items := strings.TrimSuffix(strings.TrimPrefix(inline[strings.Index(inline, "[")+1:], ""), "]")
			var dash strings.Builder
			dash.WriteString(p.Name + ":")
			for _, it := range strings.Split(items, ",") {
				dash.WriteString("\n" + indent + "  - " + strings.TrimSpace(it))
			}
			a := parser.Parse("inline.bot", fmt.Sprintf(line, inline))
			b := parser.Parse("dash.bot", fmt.Sprintf(line, dash.String()))
			if len(a.Diagnostics) != 0 || len(b.Diagnostics) != 0 {
				t.Errorf("%s.%s (%s): inline %v, dash %v\n%s", kind, p.Name, p.Form, a.Diagnostics, b.Diagnostics, fmt.Sprintf(line, dash.String()))
				continue
			}
			ja, _ := ast.MarshalFile(a.File)
			jb, _ := ast.MarshalFile(b.File)
			if !bytes.Equal(ja, jb) {
				t.Errorf("%s.%s (%s): the two forms read as different documents", kind, p.Name, p.Form)
			}
		}
	}
	if n < 10 {
		t.Fatalf("only %d list properties probed — the sweep is not reading the registry", n)
	}
}
