package model

import (
	"math/rand"
	"strings"
	"testing"
)

// baselineExpandBracedEnv is the scanner expandBracedEnv was before it
// stopped at the first unterminated `${`, kept verbatim as the ORACLE: the
// change is a performance change, so its whole claim is that it answers
// identically. Two implementations, one differential test.
func baselineExpandBracedEnv(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end == -1 {
				out.WriteByte(s[i])
				i++
				continue
			}
			body := s[i+2 : i+2+end]
			if looksLikeEnvRef(body) && bracedEnvWouldExpand(body) {
				out.WriteString(resolveBracedEnvBody(body))
				i += 2 + end + 1
				continue
			}
			out.WriteString(s[i : i+2+end+1])
			i += 2 + end + 1
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// baselineBracedSteps counts the source positions the oracle reads for s —
// its loop's and its closing-brace searches' — the cost the early stop
// exists to remove.
func baselineBracedSteps(s string) int {
	steps := 0
	for i := 0; i < len(s); {
		steps++
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end == -1 {
				steps += len(s) - (i + 2)
				i++
				continue
			}
			steps += end + 1
			i += 2 + end + 1
			continue
		}
		i++
	}
	return steps
}

// Every input — a corpus of the shapes a tool command, a script body and a
// postcondition carry, then random strings over the characters the scanner
// branches on — under a process environment with a set, an empty and an
// unset variable, must give the oracle's answer.
func TestExpandBracedEnv_MatchesThePreviousScanner(t *testing.T) {
	t.Setenv("BRACED_TEST_SET", "V")
	t.Setenv("BRACED_TEST_EMPTY", "")
	corpus := []string{
		"", "plain", "$", "${", "}", "${}", "$}", "{$}", "$$", "${{}}",
		"$HOME", "ec=$?", "$1 $@", "awk '{print $1}'", "costs $5",
		"${BRACED_TEST_SET}", "${BRACED_TEST_UNSET}", "${BRACED_TEST_EMPTY}",
		"${BRACED_TEST_UNSET:-d}", "${BRACED_TEST_EMPTY:-d}", "${BRACED_TEST_SET:-d}",
		"${BRACED_TEST_UNSET:-}", "${A:-${B:-c}}", "${BRACED_TEST_UNSET:-${BRACED_TEST_SET}}",
		"${BRACED_TEST_UNSET:-${BRACED_TEST_SET}}}", "${${BRACED_TEST_SET}}",
		"`size=${batchPackages.length}`", "`${fname}`", "${sha.slice(0, 7)}", "${p.name}",
		"${UNTERMINATED", "x${y", "${BRACED_TEST_SET", "a ${ b ${ c",
		"${BRACED_TEST_SET}${", "${BRACED_TEST_SET} then ${ and ${BRACED_TEST_SET}",
		"${ ${BRACED_TEST_SET}", "${BRACED_TEST_SET}}", "}}}${BRACED_TEST_SET}{{{",
		"${A:-b:-c}", "${1}", "${_x}", "${x-y}", "${:-x}", "${:-}", "$${BRACED_TEST_SET}",
		"é${BRACED_TEST_SET}ü", "\n${BRACED_TEST_SET}\t", "${BRACED_TEST_UNSET:-a}b}",
	}
	for _, in := range corpus {
		if got, want := expandBracedEnv(in), baselineExpandBracedEnv(in); got != want {
			t.Errorf("expandBracedEnv(%q) = %q, oracle %q", in, got, want)
		}
	}
	names := []string{"BRACED_TEST_SET", "BRACED_TEST_EMPTY", "BRACED_TEST_UNSET", "x.y"}
	pieces := []string{"$", "{", "}", "${", ":-", "-", " ", "a", "1", "_"}
	rng := rand.New(rand.NewSource(1603))
	for n := 0; n < 5000; n++ {
		var b strings.Builder
		for k := rng.Intn(12); k >= 0; k-- {
			if rng.Intn(4) == 0 {
				b.WriteString(names[rng.Intn(len(names))])
				continue
			}
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		in := b.String()
		if got, want := expandBracedEnv(in), baselineExpandBracedEnv(in); got != want {
			t.Fatalf("expandBracedEnv(%q) = %q, oracle %q", in, got, want)
		}
	}
}

// The shapes a template takes, at two sizes four times apart. The scan reads
// each source position at most once — its own step count, not a clock, so
// the witness cannot flake on a shared runner (#1393). The oracle's count for
// the unterminated shape shows what it replaces: the suffix re-read from every
// unmatched `${`.
func TestExpandBracedEnv_ReadsEachPositionOnce(t *testing.T) {
	shapes := map[string]func(int) string{
		"unterminated":        func(n int) string { return strings.Repeat("${", n) },
		"terminated then not": func(n int) string { return "${A}" + strings.Repeat("${", n) },
		"balanced":            func(n int) string { return strings.Repeat("${", n) + strings.Repeat("}", n) },
		"closing heavy":       func(n int) string { return strings.Repeat("}", n) + strings.Repeat("${", n) },
		"interleaved":         func(n int) string { return strings.Repeat("${a$}", n) },
		"script literals":     func(n int) string { return strings.Repeat("`${x.y}` ", n) },
	}
	for name, shape := range shapes {
		for _, n := range []int{20000, 80000} {
			in := shape(n)
			if _, steps := expandBracedEnvSteps(in); steps > len(in) {
				t.Errorf("%s (%d bytes): the scan read %d positions — more than one per byte",
					name, len(in), steps)
			}
		}
	}
	hostile := strings.Repeat("${", 2000)
	if steps := baselineBracedSteps(hostile); steps <= 100*len(hostile) {
		t.Fatalf("the previous scanner read %d positions for %d bytes — the comparison proves nothing",
			steps, len(hostile))
	}
}
