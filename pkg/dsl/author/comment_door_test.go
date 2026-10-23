package author

import (
	"strings"
	"testing"
)

// At the document's door (r3b F1): one comment a value — `#` and `-` on
// lines of their own — whose values alone stay within the decode ceiling is
// refused before its decode, by the count that weighs each `#` two; never
// decoded first and refused after, by the reading's honest count, with the
// memory spent.
func TestADocumentOfOneCommentAValueIsRefusedBeforeItsDecode(t *testing.T) {
	t.Setenv(readBudgetEnv, "1000")
	// 2 + 3 + 3 + 700 = 708 values, under the ceiling of 2000; 1400 more
	// for the comments.
	src := []byte("dsl: 2\nd:\n" + strings.Repeat("#\n-\n", 700))
	res := Parse("comments.bot.yaml", src)
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "may hold more values than its reader decodes") || !strings.Contains(res.Diagnostics[0].Message, "`#`") {
		t.Fatalf("diagnostics %v, want the one refusal before the decode, the `#` among what it counts", res.Diagnostics)
	}
}
