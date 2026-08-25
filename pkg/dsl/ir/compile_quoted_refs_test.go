package ir

import (
	"strings"
	"testing"
)

// The runtime shell-quotes every ref of a tool command; an author quote around
// one does not nest with it, it CANCELS it — `BASE_REF='{{vars.base_ref}}'`
// resolves to `BASE_REF=”<value>”`, and a value carrying `;` is then shell
// SYNTAX. Proven end-to-end in
// model.TestAuthorQuotedRefWouldExecute; this pins the detector.
func TestRefInQuotes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    []string
	}{
		{"bare assignment is the safe shape", `BASE_REF={{vars.base_ref}} python3 -c "x"`, nil},
		{"author single-quoted", `BASE_REF='{{vars.base_ref}}' python3 -c "x"`, []string{"{{vars.base_ref}}"}},
		{
			// Double quotes are not a cancel but not containment either: the
			// runtime's single quotes survive as DATA (`main` arrives as
			// `'main'`), and a value carrying `"` closes the author's span and
			// injects. Both reproduced in
			// model.TestAuthorQuotedRefsAreNotContained.
			"author double-quoted corrupts and injects",
			`BASE_REF="{{vars.base_ref}}" true`,
			[]string{"{{vars.base_ref}}"},
		},
		{"ref plus suffix inside quotes", `--out '{{input.run_dir}}/audit.err'`, []string{"{{input.run_dir}}"}},
		{"ref inside a flag string", `STD='--standard {{input.standard}}'`, []string{"{{input.standard}}"}},
		{
			// The trap a regex falls into: this text lies BETWEEN two quoted
			// words, so the ref is OUTSIDE quotes and perfectly safe. A
			// detector that cannot tell an opening quote from a closing one
			// reports it, and a real hazard then hides in the noise.
			"between two quoted words is not inside either",
			`cmd --in "$RUN/a.json" --lang {{input.report_lang}} > "$RUN/b.log"`,
			nil,
		},
		{"multiple refs, both quoted kinds", `A={{vars.a}} B='{{vars.b}}' C="{{vars.c}}"`, []string{"{{vars.b}}", "{{vars.c}}"}},
		{"a ref inside a python -c body is quoted by its author too", `T={{vars.t}} python3 -c "limit = int({{vars.t}})"`, []string{"{{vars.t}}"}},
		{"escaped quote inside double quotes does not close the span", `X="a\" {{vars.x}}"`, []string{"{{vars.x}}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := refInQuotes(tc.command)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("refInQuotes(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}
