package ir

import "testing"

// A wrapped fragment may open with `dsl: 2`: the harness hoists the header
// above the synthetic wrapper, where a profile header has to be, so a
// documentation fence can show a profile-2 form (a `- item` list under a
// workflow property, a chain of arrows) and still be compiled.
func TestDocsFencesHoistTheProfileHeaderAboveTheWrapper(t *testing.T) {
	edges := docSnippet{file: "x.md", tag: "fragment:edges", body: "dsl: 2\na -> b -> c when ok\nb -> done\n"}
	parseErrs, compileErrs, err := compileSnippet(edges)
	if err != nil || len(parseErrs) != 0 {
		t.Fatalf("edges fragment: err=%v parse=%v compile=%v", err, parseErrs, compileErrs)
	}
	workflow := docSnippet{file: "x.md", tag: "fragment:workflow", body: "dsl: 2\nentry: a\nallow:\n  - \"Read(**)\"\na -> done\n"}
	parseErrs, _, err = compileSnippet(workflow)
	if err != nil || len(parseErrs) != 0 {
		t.Fatalf("workflow fragment: err=%v parse=%v", err, parseErrs)
	}
	// Without the hoist the header would sit inside the wrapper: E041.
	if header, rest := hoistProfileHeader("## a note\ndsl: 2\nentry: a\n"); header != "dsl: 2\n" || rest != "## a note\nentry: a\n" {
		t.Fatalf("hoist = %q, %q", header, rest)
	}
	if header, rest := hoistProfileHeader("entry: a\n"); header != "" || rest != "entry: a\n" {
		t.Fatalf("no header: %q, %q", header, rest)
	}
}
