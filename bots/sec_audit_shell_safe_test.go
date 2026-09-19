package bots

import (
	"os"
	"regexp"
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

// The engine expands every BRACED env reference in a tool command at launch
// — `${NAME:-default}` ALWAYS, and `${NAME}` whenever NAME is set in the
// ENGINE process environment — BEFORE the command template resolves
// (pkg/backend/model/executor_tool.go, expandBracedEnv). Inside a bash -c
// body that reads a value the command itself sets in its env prefix
// (RUN_ID, SCAN_DIR_TTL_DAYS), the braced form therefore reads the ENGINE
// environment: unset there, so `${RUN_ID:-}` resolved to "" on every
// production pass and the run id guard refused them all, while every
// harness test stayed green — the harness substitutes the {{…}} refs
// plainly and never had the engine rule. Observed on the real binary with
// a probe bot: guard=refused beside run_id_seen_by_bash=<the real run id>.
//
// The invariant this pins: no command block of this bot carries a braced
// env reference at all — with a default (expanded unconditionally) or
// without (expanded whenever the engine env carries the name, a second
// source for a value the command sets itself). Bare $NAME references pass
// through to bash untouched. The deepsec harness mirrors the engine rule
// too (expandEngineBracedEnv in sec_audit_deepsec_coverage_test.go), so a
// regression reddens in the behavioural tests as well.
func TestSecAuditCommandBlocksCarryNoBracedEnvReference(t *testing.T) {
	data, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	re := regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?\}`)
	blocks := commandBackticks(string(data))
	if len(blocks) == 0 {
		t.Fatal("no `command:` blocks found — the detection heuristic is stale; re-anchor it")
	}
	for i, blk := range blocks {
		if loc := re.FindStringIndex(blk); loc != nil {
			lineNo, lineText := lineOfOffset(blk, loc[0])
			t.Errorf("sec-audit-source/main.bot: command block %d, offending line %d: %q carries the braced env reference %q. The engine expands it at launch from ITS environment — a default never reaches bash, and a value the command sets in its own env prefix (RUN_ID, SCAN_DIR_TTL_DAYS) is invisible to it. In a shell body, reference the variable bare; inside a python or other non-shell body, the sequence must not appear at all (the engine would splice its environment into the text) — reword it.",
				i, lineNo, lineText, blk[loc[0]:loc[1]])
		}
	}
}
