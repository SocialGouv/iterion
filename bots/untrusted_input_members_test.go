package bots

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// untrustedInputMembersFreeze is the class itself, frozen: every acting prompt
// of the catalogue and the reason that puts it there, one line each.
//
// The three guards beside it read the class from the outside — a member whose
// prompt lacks the paragraph, a member with a reason that names nothing, a
// non-member nobody named. None of them can show an operator WHICH prompts the
// class holds, nor report a reason that changed under a prompt that stayed in
// it. The frozen list is that report, and a diff of it is the review.
const untrustedInputMembersFreeze = "testdata/untrusted_input_boundary_members.txt"

// untrustedInputMembersOutEnv names the file the generator writes, and is the
// ONE way the frozen list is produced.
const untrustedInputMembersOutEnv = "ITERION_UNTRUSTED_INPUT_MEMBERS_OUT"

// untrustedInputMembersGenTask is the command that sets it, so a red guard
// hands the author the regeneration it asks for.
const untrustedInputMembersGenTask = "task bots:boundary:gen"

// untrustedInputMembersHeader says what the list is to whoever opens it.
var untrustedInputMembersHeader = []string{
	"# The untrusted-input boundary class, frozen: every acting prompt of the",
	"# catalogue and the reason it is in the class, one `<bundle>/<node><TAB>reason`",
	"# line per reason. Generated, never hand-edited — the generator is in",
	"# bots/untrusted_input_members_test.go, run by `task bots:boundary:gen`.",
	"#",
	"# A prompt in this list owes its system prompt the UNTRUSTED INPUT BOUNDARY",
	"# paragraph (docs/agents/bots/untrusted-input.md).",
}

// untrustedInputMemberLines reads the class over every shipped workflow, one
// `<bundle>/<node>\t<reason>` line per reason, sorted.
func untrustedInputMemberLines(t *testing.T) (lines []string, members int) {
	t.Helper()
	for _, sw := range shippedWorkflows(t) {
		for _, m := range actingPrompts(sw.wf) {
			members++
			for _, r := range m.reasons {
				lines = append(lines, sw.prefix+m.node+"\t"+r)
			}
		}
	}
	sort.Strings(lines)
	return lines, members
}

// TestRecordTheUntrustedInputBoundaryClass writes the frozen list from the
// current reading, and is the whole generator.
//
// It is a test of its own rather than a branch of the guard below so that one
// `-run` selects the generator alone — which is what the task does. Being a
// separate test is NOT what stops the gate reading back what this run just
// wrote: `go test ./bots/` runs both, this one first. The gate refuses the
// variable instead, where refusing is the only thing that holds.
func TestRecordTheUntrustedInputBoundaryClass(t *testing.T) {
	out := strings.TrimSpace(os.Getenv(untrustedInputMembersOutEnv))
	if out == "" {
		t.Skipf("the generator writes nowhere: run %s", untrustedInputMembersGenTask)
	}
	lines, members := untrustedInputMemberLines(t)
	body := strings.Join(append(append([]string{}, untrustedInputMembersHeader...), lines...), "\n") + "\n"
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		t.Fatalf("write the class to %s=%s: %v", untrustedInputMembersOutEnv, out, err)
	}
	t.Logf("wrote %d lines for %d acting prompts to %s", len(lines), members, out)
}

// TestTheUntrustedInputBoundaryClassIsTheFrozenOne compares the class to the
// frozen list, byte for byte.
//
// What it catches that no other guard here can: a prompt that joined or left in
// silence, and a reason that moved, was gained or was LOST under a prompt that
// stayed. Membership decides which system prompts owe an UNTRUSTED INPUT
// BOUNDARY paragraph, so it is a reviewed list, not a side effect of a
// predicate someone widened.
func TestTheUntrustedInputBoundaryClassIsTheFrozenOne(t *testing.T) {
	// The generator writes the very file this gate reads, and `go test ./bots/`
	// runs it first: a green under that variable would judge the class against
	// what the same run had just produced. Refused rather than trusted to a
	// file layout.
	if out := strings.TrimSpace(os.Getenv(untrustedInputMembersOutEnv)); out != "" {
		t.Fatalf("%s=%s is set: the recorder may have written the list this gate reads in the same run, so a green here would judge the class against itself — unset it, or run only %s",
			untrustedInputMembersOutEnv, out, untrustedInputMembersGenTask)
	}
	lines, members := untrustedInputMemberLines(t)
	data, err := os.ReadFile(untrustedInputMembersFreeze)
	if err != nil {
		t.Fatalf("read the frozen class: %v", err)
	}
	var frozen []string
	frozenLines, frozenKeys := map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("line %q of %s is not `node<TAB>reason`", line, untrustedInputMembersFreeze)
		}
		frozen = append(frozen, line)
		frozenLines[line], frozenKeys[key] = true, true
	}
	liveLines, liveKeys := map[string]bool{}, map[string]bool{}
	for _, line := range lines {
		key, _, _ := strings.Cut(line, "\t")
		liveLines[line], liveKeys[key] = true, true
	}
	var joined, left, dropped, reworded []string
	for _, line := range lines {
		if frozenLines[line] {
			continue
		}
		key, reason, _ := strings.Cut(line, "\t")
		if frozenKeys[key] {
			// The member stayed and carries a reason the frozen class does
			// not: it either moved — paired with a `dropped` line below — or
			// was gained. Worded for both, since this bucket cannot tell.
			reworded = append(reworded, fmt.Sprintf("%s: in the class for %q, which the frozen class does not record", key, reason))
			continue
		}
		joined = append(joined, line)
	}
	// Both directions, line by line. Skipping a frozen line because the NODE
	// still has some other live line hides a reason that stopped applying —
	// and 28 of the 143 members carry two or more, so that is most of the
	// board capabilities. The count below cannot see it either: the prompt
	// stays in the class, so 143 is still 143.
	for _, line := range frozen {
		if liveLines[line] {
			continue
		}
		key, reason, _ := strings.Cut(line, "\t")
		if liveKeys[key] {
			dropped = append(dropped, fmt.Sprintf("%s: no longer in the class for %q", key, reason))
			continue
		}
		left = append(left, line)
	}
	if len(left) > 0 {
		t.Errorf("%d prompt(s) left the class — each one stops owing an UNTRUSTED INPUT BOUNDARY paragraph, which is a decision, not a side effect; regenerate the list (%s) and review what changed:\n  %s",
			len(left), untrustedInputMembersGenTask, strings.Join(left, "\n  "))
	}
	if len(joined) > 0 {
		t.Errorf("%d prompt(s) joined the class — each one owes the paragraph; land it, then regenerate this list (%s):\n  %s",
			len(joined), untrustedInputMembersGenTask, strings.Join(joined, "\n  "))
	}
	if len(dropped) > 0 {
		t.Errorf("%d member(s) lost a reason the frozen class records — a reason that stops applying is a decision, not a side effect; regenerate the list (%s) and review what changed:\n  %s",
			len(dropped), untrustedInputMembersGenTask, strings.Join(dropped, "\n  "))
	}
	if len(reworded) > 0 {
		t.Errorf("%d member(s) are in the class for a reason the frozen one does not record — the reason is what an author reads to check the verdict, so regenerate the list (%s) and review the wording:\n  %s",
			len(reworded), untrustedInputMembersGenTask, strings.Join(reworded, "\n  "))
	}
	// The count is the frozen list's own, in PROMPTS rather than lines: a
	// member in the class for two reasons writes two lines, and a guard that
	// counted lines would read a reworded member as a prompt that left.
	if members != len(frozenKeys) {
		t.Errorf("the class reached %d acting prompts, the frozen class holds %d", members, len(frozenKeys))
	}
}
