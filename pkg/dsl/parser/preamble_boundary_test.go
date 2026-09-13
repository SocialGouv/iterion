package parser

import (
	"strings"
	"testing"
)

// The frozen rule's exact edge, pinned so nobody "fixes" it: the scan
// splits the file into at most 32 chunks, so lines 1 to 31 are read one by
// one and line 32 is read together with everything after it. A directive on
// line 32 is therefore read only when nothing follows it — the historical
// behaviour of every profile-1 file, kept as it is.
func TestTheDirectiveWindowEndsWhereItAlwaysDid(t *testing.T) {
	thirtyOne := strings.Repeat("## c\n", 31)
	if pre := ReadPreamble(thirtyOne + "## strict-escape: on\n"); !pre.StrictEscape {
		t.Fatalf("a directive on line 32 that ends the file is read")
	}
	if pre := ReadPreamble(thirtyOne + "## strict-escape: on\ntool t:\n  command: \"a\\nb\"\n"); pre.StrictEscape {
		t.Fatalf("a directive on line 32 followed by code was never read — the rule is frozen")
	}
	res := Parse("x.bot", thirtyOne+"## strict-escape: on\ntool t:\n  command: \"a\\nb\"\n")
	if got := res.File.Tools[0].Command; got != `a\nb` {
		t.Fatalf("command read as %q under a line-32 directive followed by code", got)
	}
}
