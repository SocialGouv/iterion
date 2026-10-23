package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A document whose indicators could open more values than its reader
// decodes — twice the read budget — is refused before yaml.v3 builds a node
// of it: a flow mapping of 1.1 million null-valued keys, a value a byte,
// would cost its decode some 450 MiB. The refusal is the reading's one
// diagnostic, placed on the indicator the count passed the ceiling at, and
// placing it costs nothing of the text's length: the same mapping after
// seven MiB of blank lines is refused as cheaply.
func TestADocumentPastTheDecodeCeilingIsNotDecoded(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	dense := "x: {" + strings.Repeat("a,", 1_100_000) + "a}\n"
	for _, c := range []struct {
		name, src string
		line      int
	}{
		{"on line 2", "dsl: 2\n" + dense, 2},
		{"after seven MiB of blank lines", "dsl: 2\n" + strings.Repeat("\n", 7<<20) + dense, 2 + 7<<20},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := []byte(c.src)
			var res *Result
			alloc := allocatedBy(func() { res = Parse("dense.bot.yaml", src) })
			t.Logf("allocated %d MiB", alloc>>20)
			if len(res.Diagnostics) != 1 {
				t.Fatalf("want the one refusal, got %d diagnostics (first: %v)", len(res.Diagnostics), res.Diagnostics[:min(len(res.Diagnostics), 1)])
			}
			if d := res.Diagnostics[0]; d.Code != parser.DiagAuthorDocument || !strings.Contains(d.Message, "may hold more values than its reader decodes, 2097152: counted before it is read, each `:` `,` `?` `-` `[` `{` and `#` it holds") || d.Line != c.line || d.Hint != docDecodeHint {
				t.Errorf("refusal = %s, want E050 naming the ceiling of the default budget and every indicator counted, on line %d", d.Error(), c.line)
			}
			// Refused before the decode, the document costs the count: the
			// text holds no block, so no skeleton of it is built either.
			if limit := uint64(2 << 20); alloc > limit {
				t.Errorf("a document past the decode ceiling allocated %d MiB, want under %d MiB: it was decoded, or its prefix cut into lines, before it was refused", alloc>>20, limit>>20)
			}
		})
	}
}

// Every reading of a document's YAML is held to the bounds its Parse is:
// the comment list, the reading another package asks for (ReadOne) and the
// rewrite that keeps the comments. A document whose indicators could open
// more values than the reader decodes is decoded by none of them — not
// read, each says — and one past the read budget is not read either.
func TestEveryReadingOfADocumentIsBounded(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	dense := []byte("dsl: 2\nx: {" + strings.Repeat("a,", 1_500_000) + "a}\n")
	readings := map[string]func(src []byte) bool{
		"Comments": func(src []byte) bool { return Comments(src) != nil },
		"ReadOne":  func(src []byte) bool { _, err := ReadOne(src); return err == nil },
		"WriteKeepingComments": func(src []byte) bool {
			_, err := WriteKeepingComments(Parse("x.bot.yaml", []byte(authorProbeDoc)).File, src)
			return err == nil
		},
	}
	for name, read := range readings {
		var ok bool
		alloc := allocatedBy(func() { ok = read(dense) })
		if ok {
			t.Errorf("%s reads a document past the decode ceiling", name)
		}
		if limit := uint64(16 << 20); alloc > limit {
			t.Errorf("%s allocated %d MiB on a document past the decode ceiling, want under %d MiB: it was decoded", name, alloc>>20, limit>>20)
		}
	}
	t.Setenv(readBudgetEnv, "1000")
	past := []byte("dsl: 2\n# kept\nx:\n" + strings.Repeat("- a\n", 1500))
	for name, read := range readings {
		if read(past) {
			t.Errorf("%s reads a document past the read budget", name)
		}
	}
	t.Setenv(readBudgetEnv, "")
	for name, read := range readings {
		if !read(past) {
			t.Errorf("%s does not read the same document within the budget", name)
		}
	}
}

// authorProbeDoc is a document that reads: what WriteKeepingComments is
// handed the program of.
const authorProbeDoc = "dsl: 2\nprompts:\n  p: hi\n"
