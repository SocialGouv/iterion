package parser

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A reading reports a thousand errors of a code: the last one it keeps
// says how many more there were, exactly, and the ones before it are the
// first the reading met. The count to show is the reading's own.
func TestParserReportsAThousandErrorsOfACode(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	src := "agent a:\n" + strings.Repeat("  bogus: 1\n", 1500)
	res := parseLenient("many.bot", src)
	if len(res.Diagnostics) != 1000 {
		t.Fatalf("1500 unknown properties: want 1000 diagnostics reported, got %d", len(res.Diagnostics))
	}
	if res.ErrorCount != 1500 {
		t.Errorf("ErrorCount = %d, want 1500: the count is the reading's, not the report's", res.ErrorCount)
	}
	for i, d := range res.Diagnostics {
		if d.Code != DiagUnknownProperty || d.Line != i+2 {
			t.Fatalf("diagnostic %d is %s at line %d, want E012 at line %d: the first ones are kept, in order", i, d.Code, d.Line, i+2)
		}
	}
	last := res.Diagnostics[999].Message
	m := regexp.MustCompile(`and (\d+) more E012 errors not reported`).FindStringSubmatch(last)
	if m == nil {
		t.Fatalf("the last diagnostic kept does not say how many followed: %q", last)
	}
	if n, _ := strconv.Atoi(m[1]); n != 500 {
		t.Errorf("the last diagnostic kept counts %d more, want 500", n)
	}
	if !strings.HasPrefix(last, `unknown agent property "bogus"`) {
		t.Errorf("the count is added to the diagnostic's own message, not in its place: %q", last)
	}
}

// A mistake of another code is reported whatever flood precedes it: the
// directive at the head of the file, which the parser names once it has
// read the whole file, behind a thousand unknown properties.
func TestADiagnosticOfAnotherCodeOutlivesAFlood(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	res := parseLenient("order.bot", "dsl: 2\n## strict-escape: on\n\nagent a:\n"+strings.Repeat("  bogus: 1\n", 1001))
	var directive *Diagnostic
	for i, d := range res.Diagnostics {
		if d.Code == DiagDirectiveInProfile {
			directive = &res.Diagnostics[i]
		}
	}
	if directive == nil || directive.Line != 2 {
		t.Fatalf("the E042 on line 2 is not reported behind 1001 E012: %d diagnostics", len(res.Diagnostics))
	}
	if res.ErrorCount != 1002 {
		t.Errorf("ErrorCount = %d, want 1002", res.ErrorCount)
	}
}

// With the read budget removed, the parser reports every error.
func TestNoReadBudgetParserReportsEveryError(t *testing.T) {
	t.Setenv(readBudgetEnv, "0")
	res := parseLenient("many.bot", "agent a:\n"+strings.Repeat("  bogus: 1\n", 1500))
	if len(res.Diagnostics) != 1500 || res.ErrorCount != 1500 {
		t.Fatalf("with no budget: %d reported of %d, want 1500 of 1500", len(res.Diagnostics), res.ErrorCount)
	}
}

// What a refused element costs is its token, not a diagnostic too: a list
// of a hundred thousand bare commas — each refused — allocates what the
// same list of names does, give or take the thousand diagnostics kept.
func TestADiagnosticFloodCostsWhatItsTokensCost(t *testing.T) {
	const n = 100_000
	names := "agent a:\n  model: \"m\"\n  tools: [" + strings.Repeat("a,", n) + "a]\n"
	commas := "agent a:\n  model: \"m\"\n  tools: [" + strings.Repeat(",", 2*n) + "]\n"
	var flood *ParseResult
	base := allocatedBy(func() { parseLenient("names.bot", names) })
	alloc := allocatedBy(func() { flood = parseLenient("commas.bot", commas) })
	if len(flood.Diagnostics) > 1000 {
		t.Fatalf("the comma list reports %d diagnostics, want at most 1000", len(flood.Diagnostics))
	}
	// Each refusal still builds its message before the report counts it
	// (about 130 bytes, garbage at once): 13 MiB for the 100 000 here.
	// Kept, each cost 2 KiB more.
	if extra := int64(alloc) - int64(base); extra > 32<<20 {
		t.Errorf("the refused commas allocated %d MiB more than the same list of names, want under 32 MiB: each refusal costs more than its token", extra>>20)
	}
}
