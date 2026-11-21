package author

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBusinessStoreBlockScalarIdentityComments(t *testing.T) {
	for _, field := range []string{"id", "kind", "namespace"} {
		for _, sep := range []string{"\n", "\u0085", "\u2028", "\u2029"} {
			for _, style := range []string{"|-", ">-", "|", "|+"} {
				t.Run(fmt.Sprintf("%s/%U/%s", field, []rune(sep)[0], style), func(t *testing.T) {
					source := "dsl: 2\nnodes:\n  - tool: inspect\n    command: \"true\"\nworkflow:\n  name: test\n  business_stores:\n    - id: ledger\n      kind: postgres\n      namespace: accounting\n      # Requirement must stay with this store.\n      required: true\n    - id: other\n      kind: mongo\n      namespace: audit\n      required: false\n  entry: inspect\n  edges:\n    - inspect -> done\n"
					value := map[string]string{"id": "ledger", "kind": "postgres", "namespace": "accounting"}[field]
					source = strings.Replace(source, field+": "+value, field+": "+style+"\n        one"+sep+"        two", 1)
					parsed := Parse("stores.bot.yaml", []byte(source))
					if style == ">-" && (sep == "\u2028" || sep == "\u2029") {
						if !parsed.HasErrors() {
							t.Fatal("folded LS/PS control must remain refused")
						}
						return
					}
					if parsed.HasErrors() {
						t.Fatalf("parse errors: %v", parsed.Diagnostics)
					}
					t.Logf("tuple=%q/%q/%q", parsed.File.Workflows[0].BusinessStores[0].Kind, parsed.File.Workflows[0].BusinessStores[0].Namespace, parsed.File.Workflows[0].BusinessStores[0].ID)
					slices.Reverse(parsed.File.Workflows[0].BusinessStores)
					written, e := WriteKeepingComments(parsed.File, []byte(source))
					if e != nil {
						t.Fatal(e)
					}
					if !strings.Contains(string(written), "Requirement must stay with this store.") {
						t.Fatalf("valid %s block scalar lost comment after normalized tuple match; output:\n%s", field, written)
					}
				})
			}
		}
	}
}

func TestBusinessStoreQuotedAndLiteralSeparatorsStayDistinct(t *testing.T) {
	for _, sep := range []string{"\u2028", "\u2029"} {
		t.Run(fmt.Sprintf("%U", []rune(sep)[0]), func(t *testing.T) {
			source := "dsl: 2\nnodes:\n  - tool: inspect\n    command: \"true\"\nworkflow:\n  name: test\n  business_stores:\n    - id: ledger\n      kind: postgres\n      namespace: |-\n        one" + sep + "        two\n      # Literal requirement.\n      required: true\n    - id: ledger\n      kind: postgres\n      namespace: \"one\\u" + fmt.Sprintf("%04x", []rune(sep)[0]) + "two\"\n      # Quoted requirement.\n      required: false\n  entry: inspect\n  edges:\n    - inspect -> done\n"
			parsed := Parse("stores.bot.yaml", []byte(source))
			if parsed.HasErrors() {
				t.Fatal(parsed.Diagnostics)
			}
			stores := parsed.File.Workflows[0].BusinessStores
			if stores[0].Namespace == stores[1].Namespace {
				t.Fatal("input must have distinct semantic identities")
			}
			want := map[string]string{stores[0].Namespace: "Literal requirement.", stores[1].Namespace: "Quoted requirement."}
			slices.Reverse(stores)
			written, e := WriteKeepingComments(parsed.File, []byte(source))
			if e != nil {
				t.Fatal(e)
			}
			var doc struct {
				Workflow struct {
					Stores []yaml.Node `yaml:"business_stores"`
				} `yaml:"workflow"`
			}
			if e := yaml.Unmarshal(written, &doc); e != nil {
				t.Fatal(e)
			}
			for _, n := range doc.Workflow.Stores {
				var ref struct{ Namespace string }
				if e := n.Decode(&ref); e != nil {
					t.Fatal(e)
				}
				comment := want[ref.Namespace]
				if comment == "" {
					t.Fatal("unknown tuple", ref.Namespace)
				}
				text, e := yaml.Marshal(&n)
				if e != nil {
					t.Fatal(e)
				}
				if !strings.Contains(string(text), comment) {
					t.Errorf("namespace=%q missing expected comment %q:\n%s", ref.Namespace, comment, text)
				}
				for other, w := range want {
					if other != ref.Namespace && strings.Contains(string(text), w) {
						t.Errorf("namespace=%q acquired another tuple comment %q", ref.Namespace, w)
					}
				}
			}
		})
	}
}

func TestBusinessStoreInlineAndQuotedIdentityComments(t *testing.T) {
	for _, field := range []string{"id", "kind", "namespace"} {
		for _, spelling := range []string{"one", "'one two'", "\"one two\"", "'one\u2028two'", "'one\u2029two'", "\"one\\Ltwo\"", "\"one\\Ptwo\"", "\"one\\ntwo\"", "\"a/b:c\\\\\\\"d\""} {
			t.Run(fmt.Sprintf("%s/%q", field, spelling), func(t *testing.T) {
				fields := map[string]string{"id": "ledger", "kind": "postgres", "namespace": "accounting"}
				fields[field] = spelling
				source := fmt.Sprintf("dsl: 2\nnodes:\n  - tool: inspect\n    command: \"true\"\nworkflow:\n  name: test\n  business_stores:\n    - {namespace: %s, kind: %s, id: %s, required: true} # Keep inline identity.\n  entry: inspect\n  edges:\n    - inspect -> done\n", fields["namespace"], fields["kind"], fields["id"])
				p := Parse("stores.bot.yaml", []byte(source))
				if p.HasErrors() {
					t.Fatal(p.Diagnostics)
				}
				out, e := WriteKeepingComments(p.File, []byte(source))
				if e != nil {
					t.Fatal(e)
				}
				if !strings.Contains(string(out), "Keep inline identity.") {
					t.Fatalf("lost inline comment:\n%s", out)
				}
			})
		}
	}
}
func TestBusinessStoreNormalizedAmbiguousTuplesAreNotGuessed(t *testing.T) {
	for _, field := range []string{"id", "kind", "namespace"} {
		t.Run(field, func(t *testing.T) {
			source := "dsl: 2\nnodes:\n  - tool: inspect\n    command: \"true\"\nworkflow:\n  name: test\n  business_stores:\n"
			for i, sep := range []string{"\u2028", "\n"} {
				values := map[string]string{"id": "ledger", "kind": "postgres", "namespace": "accounting"}
				values[field] = "|-\n        one" + sep + "        two"
				source += fmt.Sprintf("    - id: %s\n      kind: %s\n      namespace: %s\n      # Ambiguous %d.\n      required: true\n", values["id"], values["kind"], values["namespace"], i)
			}
			source += "  entry: inspect\n  edges:\n    - inspect -> done\n"
			p := Parse("stores.bot.yaml", []byte(source))
			if p.HasErrors() {
				t.Fatal(p.Diagnostics)
			}
			a, b := p.File.Workflows[0].BusinessStores[0], p.File.Workflows[0].BusinessStores[1]
			if a.ID != b.ID || a.Kind != b.Kind || a.Namespace != b.Namespace {
				t.Fatal("test must create duplicate semantic tuples")
			}
			out, e := WriteKeepingComments(p.File, []byte(source))
			if e != nil {
				t.Fatal(e)
			}
			if strings.Contains(string(out), "Ambiguous") {
				t.Fatalf("ambiguous normalized tuples acquired comments:\n%s", out)
			}
		})
	}
}
