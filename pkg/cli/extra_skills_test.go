package cli

import (
	"strings"
	"testing"
)

func joined(names []string) string { return strings.Join(names, ",") }

func TestResolveExtraSkills(t *testing.T) {
	cases := []struct {
		name       string
		flags      []string
		env        string
		wantNames  string
		wantOrigin string
	}{
		{name: "nothing set", wantNames: "", wantOrigin: ""},
		{name: "flag only", flags: []string{"a"}, wantNames: "a", wantOrigin: "flag"},
		{name: "env only", env: "a", wantNames: "a", wantOrigin: "env"},
		// The decisive case. A machine-wide house standard and a per-run
		// addition are BOTH things the operator asked for; the usual
		// override chain would silently drop one of them.
		{name: "both are kept", flags: []string{"a"}, env: "b", wantNames: "a,b", wantOrigin: "flag+env"},
		{name: "flag order first", flags: []string{"z", "a"}, wantNames: "z,a", wantOrigin: "flag"},
		{name: "deduped across sources", flags: []string{"a"}, env: "a", wantNames: "a", wantOrigin: "flag"},
		{name: "env is comma separated", env: "a, b ,c", wantNames: "a,b,c", wantOrigin: "env"},
		{name: "blank entries ignored", env: ",, ,", wantNames: "", wantOrigin: ""},
		{name: "flag whitespace trimmed", flags: []string{"  a  "}, wantNames: "a", wantOrigin: "flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvExtraSkills, tc.env)
			names, origin := ResolveExtraSkills(tc.flags)
			if got := joined(names); got != tc.wantNames {
				t.Errorf("names: got %q, want %q", got, tc.wantNames)
			}
			if origin != tc.wantOrigin {
				t.Errorf("origin: got %q, want %q", origin, tc.wantOrigin)
			}
		})
	}
}
