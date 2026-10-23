// Package parsertest is how a test outside pkg/dsl/parser reads a source in
// one value: the strict reading (parser.Parse). A source the reader refuses
// whole has no reading to hand over — no door of the parser hands one — so a
// test that gives one here fails at the call, naming the refusal; a test
// about a refusal reads it off parser.Parse's error. Production code reads
// through parser.Parse and answers the error; it never imports this package
// (pkg/dsl/unit TestNoGuardIsReadAroundByTheBuild).
package parsertest

import (
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A binary that is no test binary and links this package refuses to start:
// production code reads through parser.Parse and answers its error. No
// production package of the module depends on this one (pkg/dsl/unit
// TestNoGuardIsReadAroundByTheBuild); this stops a binary that would.
func init() {
	if !testing.Testing() {
		panic("parsertest is a test helper linked into a production binary: read through parser.Parse and answer its error")
	}
}

// Parse is parser.Parse for a source the test knows the reader reads whole:
// its reading, with whatever errors it reports. It panics, with the
// refusal, on a source the reader refuses.
func Parse(filename, src string) *parser.ParseResult {
	pr, err := parser.Parse(filename, src)
	if err != nil {
		panic(fmt.Sprintf("parsertest.Parse(%q): %v — a test reads a refusal off parser.Parse's error", filename, err))
	}
	return pr
}
