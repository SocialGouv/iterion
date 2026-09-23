package unparse

import "testing"

// firstJSONDifference — the difference an E054 and a refused save name —
// walks a mapping's keys in sorted order, never in a map's iteration order:
// with several keys differing, missing or appeared, at any depth, the same
// one is named on every call.
func TestFirstJSONDifferenceNamesTheSameKeyOnEveryRun(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{`{"zeta":1,"mid":2,"alpha":3}`, `{"zeta":0,"mid":0,"alpha":0}`, "document.alpha: 3 became 0"},
		{`{"zeta":1,"alpha":2}`, `{}`, "document.alpha is missing after the round-trip"},
		{`{}`, `{"zeta":1,"alpha":2}`, "document.alpha appeared after the round-trip"},
		{`{"n":{"zeta":1,"alpha":2}}`, `{"n":{"zeta":0,"alpha":0}}`, "document.n.alpha: 2 became 0"},
	} {
		for i := 0; i < 30; i++ {
			if got := firstJSONDifference([]byte(tc.a), []byte(tc.b)); got != tc.want {
				t.Fatalf("%s vs %s, call %d: %q, want %q", tc.a, tc.b, i, got, tc.want)
			}
		}
	}
}
