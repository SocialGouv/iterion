package ir

import (
	"strings"
	"testing"
)

// baselineExpandWithDefault is the forward-scanning implementation this
// package used before the single pass, kept verbatim as the ORACLE: the
// rewrite is a performance change, so its whole claim is that it answers
// identically. Two implementations, one differential test — a rewrite
// "verified" by re-reading it proves nothing.
func baselineExpandWithDefault(s string, lookup func(string) string) string {
	if lookup == nil {
		lookup = lookupEnv
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '$' && i+1 < len(s) && s[i+1] != '{' {
			end := i + 1
			for end < len(s) && (isAlnum(s[end]) || s[end] == '_') {
				end++
			}
			if end > i+1 {
				b.WriteString(lookup(s[i+1 : end]))
				i = end
				continue
			}
		}
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '{' {
			depth := 1
			j := i + 2
			for j < len(s) && depth > 0 {
				if j+1 < len(s) && s[j] == '$' && s[j+1] == '{' {
					depth += 2 - 1
					j += 2
					continue
				}
				if s[j] == '}' {
					depth--
					if depth == 0 {
						break
					}
				}
				j++
			}
			if depth == 0 {
				inner := s[i+2 : j]
				expanded := baselineExpandWithDefault(inner, lookup)
				if idx := strings.Index(expanded, ":-"); idx >= 0 {
					name, fallback := expanded[:idx], expanded[idx+2:]
					if v := lookup(name); v != "" {
						b.WriteString(v)
					} else {
						b.WriteString(fallback)
					}
				} else {
					b.WriteString(lookup(expanded))
				}
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func expandCorpus() []string {
	return []string{
		"", "plain", "$", "${", "}", "${}", "$}", "{$}", "$$", "${{}}",
		"$HOME", "$HOME/sub", "costs $5", "100$", "a$b", "$_x", "$1", "$?",
		"awk '{print $1}'", `sed 's/${x}/y/'`, `c:\path\$x`, "50%", "a}b",
		"${SET}", "${UNSET}", "${UNSET:-}", "${UNSET:-fb}", "${SET:-fb}",
		"${A:-${B:-c}}", "${SET:-${UNSET:-c}}", "${UNSET:-${SET:-c}}",
		"${UNSET:-${UNSET2:-${UNSET3:-deep}}}", "${A:-b:-c}", "${A-b}",
		"pre ${SET} mid ${UNSET:-x} post", "${SET}${UNSET}${SET}",
		"${UNTERMINATED", "${A:-${B", "x${y", "${${SET}}", "${${UNSET:-SET}}",
		"[]", `{"cost":"$5"}`, `{"dir":"${SET}"}`, `["${UNSET:-a}","b"]`,
		"a,b", "${SET},${UNSET:-b}", "é${SET}ü", "\n${SET}\t",
		"}}}${SET}{{{", "${SET}}", "}${SET}", "${:-x}", "${:-}",
	}
}

// Every input of the corpus, under three lookup policies, must give the
// same answer as the oracle. Nothing in the corpus reaches the depth bound.
func TestExpandWithDefault_MatchesTheForwardScanner(t *testing.T) {
	lookups := map[string]func(string) string{
		"empty": func(string) string { return "" },
		"set": func(k string) string {
			return map[string]string{"SET": "V", "HOME": "/h", "A": "", "B": "", "x": "X"}[k]
		},
		"all": func(k string) string { return "<" + k + ">" },
	}
	for name, lookup := range lookups {
		for _, in := range expandCorpus() {
			want := baselineExpandWithDefault(in, lookup)
			if got := ExpandWithDefault(in, lookup); got != want {
				t.Errorf("lookup %s: ExpandWithDefault(%q) = %q, oracle %q", name, in, got, want)
			}
		}
		// Fallback chains at every depth the bound admits.
		for n := 1; n <= maxEnvExpansionDepth; n++ {
			in := strings.Repeat("${X:-", n) + "deep" + strings.Repeat("}", n)
			want := baselineExpandWithDefault(in, lookup)
			if got := ExpandWithDefault(in, lookup); got != want {
				t.Errorf("lookup %s: %d levels = %q, oracle %q", name, n, got, want)
			}
		}
	}
}

// The bound is stated in levels, so the boundary is where it must be
// pinned: maxEnvExpansionDepth levels resolve, one more does not.
func TestExpandWithDefault_ResolvesExactlyTheBoundedDepth(t *testing.T) {
	noEnv := func(string) string { return "" }
	at := strings.Repeat("${X:-", maxEnvExpansionDepth) + "deep" + strings.Repeat("}", maxEnvExpansionDepth)
	if got := ExpandWithDefault(at, noEnv); got != "deep" {
		t.Errorf("%d levels = %q, want %q", maxEnvExpansionDepth, got, "deep")
	}
	// A group AFTER the over-deep one, to pin that suppression ENDS: the
	// closing braces of a segment past the bound must be copied out with
	// it, not counted against a live segment, or everything after it is
	// emitted verbatim too.
	over := strings.Repeat("${X:-", maxEnvExpansionDepth+1) + "deep" +
		strings.Repeat("}", maxEnvExpansionDepth+1) + " tail ${Y:-resolved}"
	got := ExpandWithDefault(over, noEnv)
	if strings.HasPrefix(got, "deep") {
		t.Errorf("%d levels resolved completely: the bound is not holding", maxEnvExpansionDepth+1)
	}
	if !strings.Contains(got, "${X:-") || !strings.Contains(got, "deep") {
		t.Errorf("past the bound the segment must be written out as it stands, got %.60q", got)
	}
	if !strings.HasSuffix(got, " tail resolved") {
		t.Errorf("the group after the over-deep one did not resolve: %.80q", got)
	}
}

// The shapes a hostile launch payload takes. The forward scanner was
// quadratic on ALL THREE — and only ONE of them has a closing brace, which
// is why bounding the nesting left the other two untouched: nothing
// recursed, so nothing counted. The witness is the pass's own STEP COUNT,
// not a clock: a duration would say the same thing and flake on a shared
// runner (#1393).
func TestExpandWithDefault_VisitsEachPositionOnce(t *testing.T) {
	noEnv := func(string) string { return "" }
	shapes := map[string]func(int) string{
		"balanced":      func(n int) string { return strings.Repeat("${", n) + strings.Repeat("}", n) },
		"unterminated":  func(n int) string { return strings.Repeat("${", n) },
		"fallbacks":     func(n int) string { return strings.Repeat("${X:-", n) },
		"closing heavy": func(n int) string { return strings.Repeat("}", n) + strings.Repeat("${", n) },
		"interleaved":   func(n int) string { return strings.Repeat("${a$}", n) },
	}
	for name, shape := range shapes {
		for _, n := range []int{20000, 80000} {
			in := shape(n)
			_, visited := expandWithDefault(in, noEnv, expandPolicy{})
			if visited > len(in) {
				t.Errorf("%s (%d bytes): the pass visited %d positions — more than one per byte",
					name, len(in), visited)
			}
		}
	}
	// The oracle's cost for the same input, counted the same way, to show
	// the bench bites: the forward scanner re-reads the suffix from every
	// unmatched `${`.
	hostile := strings.Repeat("${", 2000)
	if steps := baselineSteps(hostile); steps <= len(hostile) {
		t.Fatalf("the forward scanner visited %d positions for %d bytes — the comparison proves nothing",
			steps, len(hostile))
	}
}

// baselineSteps counts the positions the forward scanner reads for s, the
// cost the single pass exists to remove.
func baselineSteps(s string) int {
	steps := 0
	for i := 0; i < len(s); {
		steps++
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '{' {
			depth := 1
			j := i + 2
			for j < len(s) && depth > 0 {
				steps++
				if j+1 < len(s) && s[j] == '$' && s[j+1] == '{' {
					depth++
					j += 2
					continue
				}
				if s[j] == '}' {
					depth--
					if depth == 0 {
						break
					}
				}
				j++
			}
			if depth == 0 {
				i = j + 1
				continue
			}
		}
		i++
	}
	return steps
}
