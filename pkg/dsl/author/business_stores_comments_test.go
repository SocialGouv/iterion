package author

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBusinessStoreCommentsFollowIdentity(t *testing.T) {
	type identity struct{ Kind, Namespace, ID string }
	for _, tc := range []struct {
		name               string
		stores             [2]identity
		kindFirst, reverse bool
	}{
		{name: "namespace", stores: [2]identity{{"postgres", "accounting", "ledger"}, {"postgres", "audit", "ledger"}}},
		{name: "kind", stores: [2]identity{{"postgres", "shared", "ledger"}, {"mongo", "shared", "ledger"}}},
		{name: "key-order", kindFirst: true, stores: [2]identity{{"postgres", "accounting", "ledger"}, {"postgres", "audit", "ledger"}}},
		{name: "reordered", reverse: true, stores: [2]identity{{"postgres", "accounting", "ledger"}, {"postgres", "audit", "ledger"}}},
		{name: "nul-tuple", reverse: true, stores: [2]identity{{"db", "x\x00y", "ledger"}, {"db\x00x", "y", "ledger"}}},
		{name: "empty-namespace", stores: [2]identity{{"postgres", "", "ledger"}, {"postgres", "audit", "ledger"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "dsl: 2\nnodes:\n  - tool: inspect\n    command: \"true\"\nworkflow:\n  name: test\n  business_stores:\n"
			wanted := map[identity]string{}
			for i, ref := range tc.stores {
				if tc.kindFirst {
					source += fmt.Sprintf("    - kind: %q\n      id: %q\n", ref.Kind, ref.ID)
				} else {
					source += fmt.Sprintf("    - id: %q\n      kind: %q\n", ref.ID, ref.Kind)
				}
				if ref.Namespace != "" {
					source += fmt.Sprintf("      namespace: %q\n", ref.Namespace)
				}
				comment := fmt.Sprintf("Store %d keeps its requirement.", i)
				wanted[ref] = comment
				source += fmt.Sprintf("      # %s\n      required: %t\n", comment, i == 0)
			}
			source += "  entry: inspect\n  edges:\n    - inspect -> done\n"
			parsed := Parse("comments.bot.yaml", []byte(source))
			if errs := errorsOf(parsed); len(errs) != 0 {
				t.Fatal(errs)
			}
			if tc.reverse {
				slices.Reverse(parsed.File.Workflows[0].BusinessStores)
			}
			written, err := WriteKeepingComments(parsed.File, []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Workflow struct {
					Stores []yaml.Node `yaml:"business_stores"`
				} `yaml:"workflow"`
			}
			if err := yaml.Unmarshal(written, &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc.Workflow.Stores) != 2 {
				t.Fatalf("stores=%d", len(doc.Workflow.Stores))
			}
			for _, node := range doc.Workflow.Stores {
				var ref identity
				if err := node.Decode(&ref); err != nil {
					t.Fatal(err)
				}
				comment, exists := wanted[ref]
				if !exists {
					t.Fatalf("unexpected identity %+v", ref)
				}
				var comments []string
				var walk func(*yaml.Node)
				walk = func(n *yaml.Node) {
					comments = append(comments, n.HeadComment, n.LineComment, n.FootComment)
					for _, child := range n.Content {
						walk(child)
					}
				}
				walk(&node)
				joined := strings.Join(comments, "\n")
				if !strings.Contains(joined, comment) {
					t.Fatalf("business store comment lost for %+v: %s", ref, written)
				}
				for other, text := range wanted {
					if other != ref && strings.Contains(joined, text) {
						t.Fatalf("comment moved from %+v to %+v", other, ref)
					}
				}
			}
		})
	}
}

func TestBusinessStoreCommentMatchingKeepsAmbiguityAndOtherSequences(t *testing.T) {
	for _, tc := range []struct{ name, source, target string }{
		{"ambiguous-tuple", "workflow:\n  business_stores:\n    - id: ledger\n      kind: db\n      # First\n      required: true\n    - id: ledger\n      kind: db\n      # Second\n      required: true\n", "workflow:\n  business_stores:\n    - id: ledger\n      kind: db\n      required: true\n    - id: ledger\n      kind: db\n      required: true\n"},
		{"unrelated-sequence", "vars:\n  business_stores:\n    - id: ledger\n      # First\n      kind: first\n    - id: ledger\n      # Second\n      kind: second\n", "vars:\n  business_stores:\n    - id: ledger\n      kind: first\n    - id: ledger\n      kind: second\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var source, target yaml.Node
			if err := yaml.Unmarshal([]byte(tc.source), &source); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal([]byte(tc.target), &target); err != nil {
				t.Fatal(err)
			}
			keepComments(&source, &target)
			written, err := yaml.Marshal(&target)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(written), "# First") || strings.Contains(string(written), "# Second") {
				t.Fatalf("ambiguous or unrelated sequence acquired store matching: %s", written)
			}
		})
	}
}
