package runtime

import (
	"fmt"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A `json` var's document nests: a list of objects, a list of lists. Deciding
// that an element moved by comparing the two `any` values panics on every
// element Go cannot compare, which is exactly those shapes — so the remap
// walks what the value reader produces, and reports what the recursion
// returned instead.
func TestRemapRepoRootWalksANestedDocument(t *testing.T) {
	const repoRoot, projectDir = "/srv/repo", "/wt/run-1"
	for _, tc := range []struct {
		name string
		text string
		want any
	}{
		{"a list of objects", `[{"dir":"/srv/repo"},{"dir":"rel/x"}]`,
			[]any{map[string]any{"dir": projectDir}, map[string]any{"dir": "rel/x"}}},
		{"a list of lists", `[["/srv/repo"],["rel/x"]]`,
			[]any{[]any{projectDir}, []any{"rel/x"}}},
		{"a list of objects with nothing to move", `[{"dir":"rel/x"}]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The shape the product builds from the declared text, not a
			// hand-made one: `json` is parsed, then its leaves expanded.
			val, err := ir.ResolveVarText(tc.text, ir.VarJSON, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			got, moved := remapRepoRoot(val, repoRoot, projectDir)
			if tc.want == nil {
				if moved {
					t.Errorf("moved = true for a document with no repo root in it (%#v)", got)
				}
				return
			}
			if !moved {
				t.Fatalf("moved = false; the repo root in the document was not remapped (%#v)", got)
			}
			if fmtValue(got) != fmtValue(tc.want) {
				t.Errorf("remapped to %#v, want %#v", got, tc.want)
			}
		})
	}
}

// fmtValue renders a document for comparison without requiring it to be
// comparable — the property this test is about.
func fmtValue(v any) string {
	switch val := v.(type) {
	case []any:
		out := "["
		for i, e := range val {
			if i > 0 {
				out += ","
			}
			out += fmtValue(e)
		}
		return out + "]"
	case map[string]any:
		out := "{"
		for _, k := range sortedKeys(val) {
			out += k + ":" + fmtValue(val[k]) + ","
		}
		return out + "}"
	default:
		return fmtScalar(v)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func fmtScalar(v any) string { return fmt.Sprintf("%v", v) }
