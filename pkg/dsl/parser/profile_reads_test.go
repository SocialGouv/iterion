package parser

import (
	"reflect"
	"testing"
)

// A profile-1 file reports the places profile 2 would read otherwise — a
// backslash in a quoted literal (once per literal), a blank line INSIDE a
// prompt body — and nothing else: not a raw string or a block scalar,
// whose text is the same in both profiles, not the blank lines after a body,
// which both profiles drop, and nothing at all once the file declares
// profile 2 or opts into the strict escapes.
func TestProfileReadsNameWhereProfileTwoReadsOtherwise(t *testing.T) {
	src := "tool t:\n  command: \"a\\nb \\\"q\\\"\"\n  description: `raw \\n`\n  script: |\n    printf 'x\\n'\n\nprompt p:\n  First.\n\n  Second.\n   \n  Third.\n\n\nprompt q:\n  Single.\n\n\nagent a:\n  description: \"plain\"\n"
	res := Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	want := []ProfileRead{{Line: 2, Kind: "escape"}, {Line: 9, Kind: "paragraph"}, {Line: 11, Kind: "paragraph"}}
	if !reflect.DeepEqual(res.ProfileReads, want) {
		t.Fatalf("reads = %+v, want %+v", res.ProfileReads, want)
	}
	if r := Parse("x.bot", "dsl: 2\n"+src); len(r.ProfileReads) != 0 {
		t.Fatalf("a profile-2 file reports reads: %+v", r.ProfileReads)
	}
	if r := Parse("x.bot", "## strict-escape: on\n"+src); len(r.ProfileReads) != 2 || r.ProfileReads[0].Kind != "paragraph" {
		t.Fatalf("a strict profile-1 file still reports its literal: %+v", r.ProfileReads)
	}
	if r := Parse("x.bot", "agent a:\n  description: \"plain\"\n\nprompt p:\n  One line.\n"); len(r.ProfileReads) != 0 {
		t.Fatalf("a file both profiles read alike reports %+v", r.ProfileReads)
	}
}
