package author

import (
	"strings"
	"testing"
)

// Comments lists every YAML comment of a document, wherever it sits — the
// head of the file, the end of a line, under a block — and nothing else: a
// `#` inside a scalar is text, and a document without comments has none.
func TestCommentsListsEveryYAMLCommentAndOnlyThem(t *testing.T) {
	doc := "# the head\ndsl: 2 # the profile\nprompts:\n  ask: |\n    # not a comment: a heading in the prompt\n    Say hello.\n  # under the block\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n# the foot\n"
	got := Comments([]byte(doc))
	want := []string{"# the head", "# the profile", "# under the block", "# the foot"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("comments = %q, want %q", got, want)
	}
	bare := strings.NewReplacer("# the head\n", "", " # the profile", "", "  # under the block\n", "", "# the foot\n", "").Replace(doc)
	if got := Comments([]byte(bare)); len(got) != 0 {
		t.Fatalf("a document without comments reports %q", got)
	}
	if got := Comments([]byte("not: [yaml")); got != nil {
		t.Fatalf("a source that is not YAML reports comments: %q", got)
	}
}
