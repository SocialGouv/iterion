package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestBusinessStoreBlocksAccumulate(t *testing.T) {
	const empty = "  business_stores:\n\n"
	const primary = "  business_stores:\n    primary:\n      kind: postgres\n      required: true\n"
	const secondary = "  business_stores:\n    secondary:\n      kind: postgres\n"
	for _, tc := range []struct {
		name, blocks      string
		count             int
		absent, duplicate bool
	}{
		{name: "absent", absent: true},
		{name: "empty", blocks: empty},
		{name: "two-empty", blocks: empty + empty},
		{name: "filled-then-empty", blocks: primary + empty, count: 1},
		{name: "empty-then-filled", blocks: empty + primary, count: 1},
		{name: "distinct", blocks: primary + secondary, count: 2},
		{name: "duplicate", blocks: primary + primary, count: 2, duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parsertest.Parse("blocks.bot", "tool inspect:\n  command: \"true\"\nworkflow test:\n"+tc.blocks+"  entry: inspect\n  inspect -> done\n")
			if hasParseErrors(parsed.Diagnostics) {
				t.Fatal(parsed.Diagnostics)
			}
			wire, err := ast.MarshalFile(parsed.File)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := ast.UnmarshalFile(wire)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range []*ast.File{parsed.File, restored} {
				compiled := ir.Compile(file)
				if compiled.HasErrors() {
					t.Fatal(compiled.Diagnostics)
				}
				refs := compiled.Workflow.BusinessStores
				if len(refs) != tc.count || (refs == nil) != tc.absent {
					t.Fatalf("business store blocks lost declarations: stores=%+v nil=%t want count=%d absent=%t", refs, refs == nil, tc.count, tc.absent)
				}
				context := &store.ExecutionContext{RunStore: store.ContextRef{ID: "run", Kind: "fixture"}}
				for _, ref := range refs {
					context.BusinessStores = append(context.BusinessStores, store.ContextRef{ID: ref.ID, Kind: ref.Kind, Namespace: ref.Namespace, Required: ref.Required})
				}
				err := context.Normalize()
				if tc.duplicate {
					if err == nil || !strings.Contains(err.Error(), "duplicate execution context business store") {
						t.Fatalf("repeated tuple bypassed Normalize: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
