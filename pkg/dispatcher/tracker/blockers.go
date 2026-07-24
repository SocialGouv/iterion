package tracker

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Issue-dependency gating for the external forge adapters (github / forgejo /
// gitlab). The native tracker already models hard blockers richly
// (pkg/dispatcher/native/blockers.go); the external adapters leave
// Issue.Blockers empty, so a forge issue that declares "Blocked by #41" in its
// body is dispatched anyway. These helpers let each adapter honor that
// user-declared dependency by parsing the body and holding an issue whose
// blocker is still open.
//
// Two deliberate design choices, both to avoid the failure mode the operator
// cares about most — an issue that silently never dispatches:
//
//   - FAIL-OPEN. A blocker is "unsatisfied" only when we can positively see it
//     is still open (its number is in the open-issue set the adapter just
//     fetched). A reference we cannot resolve — a typo'd number, a cross-repo
//     ref, an issue outside the fetched set — is treated as satisfied and does
//     NOT hold the issue. A regex mis-parse can therefore never cause an
//     invisible stall; the worst case is under-blocking, which the operator
//     prefers over a ticket wedged for a reason no one can see.
//   - SAME-REPO `#N` ONLY. Every forge writes an in-repo dependency as `#N`;
//     cross-repo / URL forms vary and are intentionally ignored (fail-open).

// blockerLineRe matches a line that opens with a dependency keyword, tolerating
// leading markdown quote/list markers ("> Blocked by …", "- Depends on …").
// The captured tail is then scanned for `#N` references.
var blockerLineRe = regexp.MustCompile(`(?im)^[\s>*+-]*(?:depends on|blocked by)\b(.*)$`)

// issueRefRe matches a same-repo issue reference `#N`.
var issueRefRe = regexp.MustCompile(`#(\d+)`)

// ParseBlockerRefs extracts the same-repo issue numbers a body declares as hard
// dependencies via "Depends on #N" / "Blocked by #N" lines (case-insensitive,
// one or more refs per line). Order-preserving and de-duplicated. Returns nil
// when the body declares none.
func ParseBlockerRefs(body string) []int {
	if body == "" {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, line := range blockerLineRe.FindAllStringSubmatch(body, -1) {
		for _, ref := range issueRefRe.FindAllStringSubmatch(line[1], -1) {
			n, err := strconv.Atoi(ref[1])
			if err != nil || n <= 0 || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// HeldByOpenBlockers returns the sorted subset of a body's declared blocker
// refs that are still open (present in openNums) — the reasons the issue must
// be held this scan. An empty result means the issue is free to dispatch
// (either it declares no blockers, or every declared blocker is satisfied /
// unresolvable — fail-open). openNums is the set of issue numbers the adapter
// observed open in the same repo this poll.
func HeldByOpenBlockers(body string, openNums map[int]bool) []int {
	refs := ParseBlockerRefs(body)
	if len(refs) == 0 {
		return nil
	}
	var held []int
	for _, n := range refs {
		if openNums[n] {
			held = append(held, n)
		}
	}
	sort.Ints(held)
	return held
}

// formatIssueRefs renders issue numbers as "#a, #b" for a log line.
func formatIssueRefs(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = "#" + strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}
