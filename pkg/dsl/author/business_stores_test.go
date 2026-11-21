package author

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

func TestBusinessStoresSurviveEveryAuthoringAndTransportSurface(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		want        []ir.BusinessStore
	}{
		{"declared", `  business_stores:
    ## The ledger stays required.
    "ledger/main":
      kind: postgres
      namespace: "org/accounting"
      required: true
    "ledger/main":
      kind: postgres
      namespace: "org/audit"
      required: false
`, []ir.BusinessStore{{ID: "ledger/main", Kind: "postgres", Namespace: "org/accounting", Required: true}, {ID: "ledger/main", Kind: "postgres", Namespace: "org/audit"}}},
		{"empty", "  business_stores:\n\n", []ir.BusinessStore{}},
		{"draft", "  business_stores:\n    pending:\n", []ir.BusinessStore{{ID: "pending"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "tool inspect:\n  command: \"true\"\n\nworkflow test:\n" + tc.block + "\n  entry: inspect\n  inspect -> done\n"
			parsed := parsertest.Parse("stores.bot", source)
			if hasParseErrors(parsed.Diagnostics) {
				t.Fatalf("parse: %v", parsed.Diagnostics)
			}
			encoded, err := ast.MarshalFile(parsed.File)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := ast.UnmarshalFile(encoded)
			if err != nil {
				t.Fatal(err)
			}
			document, err := Write(parsed.File)
			if err != nil {
				t.Fatal(err)
			}
			authored := Parse("stores.bot.yaml", document)
			if errors := errorsOf(authored); len(errors) != 0 {
				t.Fatalf("author errors=%v\n%s", errors, document)
			}
			written := unparse.Unparse(parsed.File)
			if err := unparse.Verify(parsed.File, written); err != nil {
				t.Fatalf("unparse: %v\n%s", err, written)
			}
			reparsed := parsertest.Parse("stores-written.bot", written)
			if hasParseErrors(reparsed.Diagnostics) {
				t.Fatalf("reparse: %v", reparsed.Diagnostics)
			}
			for name, file := range map[string]*ast.File{"direct": parsed.File, "transport": restored, "author": authored.File, "unparse": reparsed.File} {
				compiled := ir.Compile(file)
				if compiled.HasErrors() {
					t.Fatalf("%s compile: %v", name, compiled.Diagnostics)
				}
				if !reflect.DeepEqual(compiled.Workflow.BusinessStores, tc.want) {
					t.Errorf("%s stores=%+v want %+v", name, compiled.Workflow.BusinessStores, tc.want)
				}
				if tc.name == "declared" {
					declared := file.Workflows[0].BusinessStores
					if declared[1].Required == nil || *declared[1].Required {
						t.Errorf("%s loses explicit required:false", name)
					}
				}
			}
			if tc.name == "declared" {
				if !strings.Contains(written, "The ledger stays required.") {
					t.Fatal("business-store .bot comment lost while saving")
				}
				document = []byte(strings.Replace(string(document), "business_stores:", "# Declared stores\n  business_stores:", 1))
				draft := Parse("commented.bot.yaml", document)
				if errs := errorsOf(draft); len(errs) != 0 {
					t.Fatal(errs)
				}
				rewritten, err := WriteKeepingComments(draft.File, document)
				if err != nil || !strings.Contains(string(rewritten), "# Declared stores") {
					t.Fatalf("YAML comment lost: err=%v\n%s", err, rewritten)
				}
			}
		})
	}
}
