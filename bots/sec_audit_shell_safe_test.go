package bots

import (
	"os"
	"strings"
	"testing"
)

// A `command:` value in this bot's DSL is one backtick-delimited string. Its
// body may hold a plain shell script, python3 -c "<py>", python3 -c '<py>',
// or bash -c '<sh>'. Only the bash -c '<sh>' shape is fragile in a specific
// way that has bitten Seki once already (2026-09-19, per-pass export slot
// commit): everything between the opening apostrophe and the matching close
// is ONE shell argument. Inside a single-quoted shell argument no apostrophe
// can appear -- one would close the argument, the outer shell would re-parse
// the rest of the block as its own script, and the body would silently
// truncate. `iterion validate` sees nothing wrong (the DSL treats the command
// as opaque); the harness reports "exit status 2" and an empty envelope.
//
// The invariant this test enforces:
//
//	for each `command:` block whose body starts a `bash -c '`, the block
//	contains EXACTLY two apostrophes — the open of the bash -c argument and
//	its matching close. Any other apostrophe (in a comment, a message, or a
//	variable name) is a bug.
//
// This shape is the one Seki uses. A command that combines `bash -c '...'`
// with another single-quoted argument would need its own scanner, and none
// currently exists.
func TestSecAuditBashCBlocksHaveNoApostrophesOrBackticks(t *testing.T) {
	data, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	blocks := commandBackticks(string(data))
	if len(blocks) == 0 {
		t.Fatal("no `command:` blocks found in sec-audit-source/main.bot — " +
			"the detection heuristic is stale; re-anchor it")
	}
	checked := 0
	for _, blk := range blocks {
		if !strings.Contains(blk, "bash -c '") {
			continue
		}
		checked++
		apo := strings.Count(blk, "'")
		if apo != 2 {
			// Report the first apostrophe past the opening one so the author
			// lands on the offending character.
			off := strings.Index(blk, "bash -c '") + len("bash -c '")
			if off > len(blk) {
				off = len(blk)
			}
			extra := strings.Index(blk[off:], "'")
			var lineNo int
			var lineText string
			if extra >= 0 {
				lineNo, lineText = lineOfOffset(blk, off+extra)
			}
			t.Errorf("sec-audit-source/main.bot: a command block that opens `bash -c '<body>'` "+
				"contains %d apostrophe(s), want exactly 2 (the outer bash -c open + close). "+
				"An extra apostrophe closes the argument, the outer shell re-parses the rest as "+
				"its own script, and the body silently truncates -- iterion validate cannot see "+
				"this and the run reports exit 2 with an empty envelope. First stray at line %d: %q",
				apo, lineNo, lineText)
		}
		// A backtick inside the bash -c body would close the DSL command
		// value itself. The command block is EVERYTHING between two DSL
		// backticks, so a stray backtick could not survive: if one appeared,
		// commandBackticks would already have split the block short of the
		// intended close. Guard the failure mode explicitly by refusing an
		// unterminated `bash -c '` (no closing apostrophe).
		firstAt := strings.Index(blk, "bash -c '") + len("bash -c '")
		if !strings.Contains(blk[firstAt:], "'") {
			t.Errorf("sec-audit-source/main.bot: a `bash -c '<body>'` block never closes its " +
				"single-quoted argument -- either a backtick inside truncated the DSL command, " +
				"or the closing apostrophe was removed. The whole node body is unreachable.")
		}
	}
	if checked == 0 {
		t.Fatal("no `bash -c '<body>'` blocks found in sec-audit-source/main.bot — " +
			"the detection heuristic is stale; re-anchor it")
	}
}

func lineOfOffset(s string, off int) (int, string) {
	if off < 0 || off >= len(s) {
		return 0, ""
	}
	// Find start of the line containing off.
	lineStart := strings.LastIndexByte(s[:off], '\n') + 1
	// Find end of that line.
	rel := strings.IndexByte(s[lineStart:], '\n')
	end := len(s)
	if rel >= 0 {
		end = lineStart + rel
	}
	line := s[lineStart:end]
	// Compute 1-based line number.
	n := strings.Count(s[:lineStart], "\n") + 1
	return n, line
}
