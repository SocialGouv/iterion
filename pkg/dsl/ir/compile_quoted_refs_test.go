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
			// Double quotes are NOT the cancel: the runtime's single quotes are
			// literal inside them, so the value stays one word. Reporting it
			// would be noise, and noise is where a real hazard hides.
			"author double-quoted is contained",
			`BASE_REF="{{vars.base_ref}}" true`,
			nil,
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
		{"multiple refs, only the single-quoted one", `A={{vars.a}} B='{{vars.b}}' C="{{vars.c}}"`, []string{"{{vars.b}}"}},
		{"a ref in a python -c body is contained by the double quotes", `T={{vars.t}} python3 -c "limit = int({{vars.t}})"`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := refInQuotes(tc.command)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("refInQuotes(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}
