package ast_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// A null where a declaration is expected used to reach a converter as a nil
// pointer and panic — a 500 per request on the studio's document endpoints.
// It is refused once, at the document level, naming the slot; no converter
// has to guard, present or future.
func TestUnmarshalRejectsNullElements(t *testing.T) {
	cases := map[string]string{
		`{"agents":[null]}`:                                                          "document.agents[0]",
		`{"groups":[null]}`:                                                          "document.groups[0]",
		`{"uses":[null]}`:                                                            "document.uses[0]",
		`{"groups":[{"name":"g","tools":[null]}]}`:                                   "document.groups[0].tools[0]",
		`{"groups":[{"name":"g","edges":[null]}]}`:                                   "document.groups[0].edges[0]",
		`{"uses":[{"group":"g","prefix":"p","with":[null]}]}`:                        "document.uses[0].with[0]",
		`{"workflows":[{"name":"w","edges":[null]}]}`:                                "document.workflows[0].edges[0]",
		`{"workflows":[{"name":"w","edges":[{"from":"a","to":"b","with":[null]}]}]}`: "document.workflows[0].edges[0].with[0]",
		`{"cursors":[{"name":"c","bands":[null]}]}`:                                  "document.cursors[0].bands[0]",
		`{"schemas":[{"name":"s","fields":[null]}]}`:                                 "document.schemas[0].fields[0]",
		`{"vars":{"fields":[null]}}`:                                                 "document.vars.fields[0]",
		`{"computes":[{"name":"c","expr":[null]}]}`:                                  "document.computes[0].expr[0]",
	}
	for doc, slot := range cases {
		t.Run(slot, func(t *testing.T) {
			_, err := ast.UnmarshalFile([]byte(doc))
			if err == nil {
				t.Fatalf("accepted %s", doc)
			}
			if !strings.Contains(err.Error(), slot) {
				t.Errorf("error does not name the slot %s: %v", slot, err)
			}
		})
	}
	// A document without nulls still decodes.
	if _, err := ast.UnmarshalFile([]byte(`{"agents":[{"name":"a"}],"groups":[{"name":"g","tools":[{"name":"t"}]}],"uses":[{"group":"g","prefix":"p","with":[{"key":"k","value":"v"}]}]}`)); err != nil {
		t.Fatalf("a well-formed document was refused: %v", err)
	}
}
