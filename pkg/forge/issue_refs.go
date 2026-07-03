package forge

import (
	"regexp"
	"strconv"
)

// IssueRefPattern matches "#<n>" issue-reference style substrings (e.g.
// "fixes #123", "closes #7") in free text. No forge exposes PR/MR → issue
// linkage as structured data, so every provider parses it out of the
// title/body the same way.
var IssueRefPattern = regexp.MustCompile(`#(\d+)`)

// ParseIssueRefs extracts every IssueRefPattern match out of texts and
// returns distinct issue numbers in first-seen order. When skipNonPositive
// is true, a literal "#0" is discarded (negative numbers can't occur — the
// pattern only matches digits).
func ParseIssueRefs(skipNonPositive bool, texts ...string) []int {
	seen := map[int]bool{}
	var out []int
	for _, text := range texts {
		for _, m := range IssueRefPattern.FindAllStringSubmatch(text, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil || seen[n] || (skipNonPositive && n <= 0) {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
