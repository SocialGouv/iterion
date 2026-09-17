package server

import (
	"reflect"
	"strings"
	"testing"
)

// The audience a caller submits is normalised once, at the edge, so the
// resolver never has to reason about padding or repeats.
func TestBotAudienceIsNormalisedAtTheEdge(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"absent", nil, nil},
		{"empty clears the audience", []string{}, nil},
		{"trimmed", []string{"  sec-audit-source  "}, []string{"sec-audit-source"}},
		{"de-duplicated, order kept", []string{"a", "b", "a"}, []string{"a", "b"}},
		{"blanks among real entries are dropped", []string{"a", "   ", "b"}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		got, err := normalizeBotAudience(tc.in)
		if err != nil {
			t.Fatalf("%s: unexpected refusal: %v", tc.name, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %#v want %#v", tc.name, got, tc.want)
		}
	}
}

// A list of nothing but blanks is REFUSED rather than repaired. Dropping them
// silently would turn "only these bots" into "every bot" — the exact widening
// the field exists to prevent — and the operator would learn it from a key
// that funds the fleet. An empty list stays legal: that is how an audience is
// lifted, and the two must not collapse into each other.
func TestABlankOnlyAudienceIsRefusedNotWidened(t *testing.T) {
	for _, in := range [][]string{{""}, {"   "}, {"", "\t", " "}} {
		got, err := normalizeBotAudience(in)
		if err == nil {
			t.Fatalf("%#v was accepted as %#v — a mistyped audience became 'every bot'", in, got)
		}
		if !strings.Contains(err.Error(), "blank") {
			t.Errorf("%#v: refusal does not say what was wrong: %v", in, err)
		}
	}
}

// An allow-list is read on every credential resolution, so its size is
// bounded. The refusal is explicit rather than a silent truncation, which
// would drop the very bot an operator meant to admit.
func TestAnOversizedAudienceIsRefused(t *testing.T) {
	big := make([]string, maxBotAudienceEntries+1)
	for i := range big {
		big[i] = string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	if _, err := normalizeBotAudience(big); err == nil {
		t.Fatalf("an audience of %d entries was accepted", len(big))
	}
	if _, err := normalizeBotAudience(big[:maxBotAudienceEntries]); err != nil {
		t.Fatalf("an audience at the ceiling was refused: %v", err)
	}
}
