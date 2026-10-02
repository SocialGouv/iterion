package spec

import (
	"strings"
	"testing"
)

// A form the renderer cannot say, a block whose body names no kind, an
// entry whose body names no kind: each is a rendering ERROR naming the gap
// — never a fragment that accepts any value while the artefact claims to
// describe the property. The combined schema is held to the same refusal.
func TestSchemaRenderingRefusesWhatItCannotSay(t *testing.T) {
	workflow := Kind{Name: "workflow", Role: Declaration, Edges: true, Properties: []Property{prop("entry", Ident, "x")}}
	node := func(p Property) []Kind {
		return []Kind{{Name: "thing", Role: Node, Doc: "A synthetic node.", Properties: []Property{p}}, workflow}
	}
	for _, tc := range []struct {
		name  string
		kinds []Kind
		want  string
	}{
		{"a form with no rendering", node(Property{Name: "odd", Form: Form("nonsense"), Doc: "d"}), `form "nonsense"`},
		{"a block whose body names no kind", node(Property{Name: "inner", Form: Block, Body: "nowhere", Doc: "d"}), `"nowhere"`},
		{"a block-or-ident whose body names no kind", node(Property{Name: "inner", Form: BlockOrIdent, Body: "nowhere", Doc: "d"}), `"nowhere"`},
		// The root reaches declarations by name: a synthetic `contract` (a
		// declaration the root renders from its entries) carries the gap.
		{"an entry whose body names no kind", []Kind{{Name: "contract", Role: Declaration, Doc: "d", Entries: &Entries{Body: "nowhere"}}, workflow}, `"nowhere"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, err := renderSchemaFor(tc.kinds, 2); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rendered without naming the gap: err = %v, want %s\n%s", err, tc.want, out)
			}
			if out, err := renderCombinedFor(tc.kinds, []int{1, 2}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("combined: rendered without naming the gap: err = %v, want %s\n%s", err, tc.want, out)
			}
		})
	}
}
