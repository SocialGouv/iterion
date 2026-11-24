package liveledger

import (
	"strings"
	"testing"
)

// The operator's screen is the one surface that reads the ledger, and the
// unobserved sentinel must not read there as a model named "unobserved:…".
// A spec renders as itself; the sentinel renders in words, carrying the
// spec a re-target asked for.
func TestRowDetailRendersTheUnobservedServedModelInWords(t *testing.T) {
	const asked = "openai_compatible/gpt-oss-120b"
	cases := []struct {
		name, served string
		want         []string
		refuse       []string
	}{
		{"a spec is itself", "anthropic/claude-haiku-4-5", []string{"served by anthropic/claude-haiku-4-5"}, nil},
		{"the bare sentinel", ServedModelUnobserved, []string{"served by a model the backend did not name"}, []string{"served by unobserved", "(asked"}},
		{"the sentinel with the asked-for spec", ServedModelUnobservedPrefix + asked, []string{"a model the backend did not name (asked " + asked + ")"}, []string{"served by unobserved:"}},
		{"a folded aggregate keeps each member readable", "anthropic/claude-haiku-4-5" + ListSep + ServedModelUnobservedPrefix + asked, []string{"anthropic/claude-haiku-4-5", "did not name (asked " + asked + ")"}, []string{"unobserved:"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rowDetail(Row{ServedModel: c.served})
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("rowDetail = %q, want it to carry %q", got, w)
				}
			}
			for _, r := range c.refuse {
				if strings.Contains(got, r) {
					t.Errorf("rowDetail = %q, must not spell %q", got, r)
				}
			}
		})
	}
}
