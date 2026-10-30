package projectenv

import (
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The reference scan the raw-plant lanes warn with and this lane's own
// expander must agree BOTH ways: CarriesExpansionReference names exactly
// the values expandReferences changes, judged against the REAL expander and
// not a re-implementation of it — the day one grammar drifts, this reddens.
// Values carrying a backslash are outside the comparison on purpose: the
// expander RESOLVES a `\$` escape to one literal dollar while the scan
// rightly calls it no reference (the raw lanes keep those bytes, and a
// shell reading them later resolves the same escape) — the no-escape
// branch of the warning.
func TestTheReferenceScanMatchesWhatTheExpanderDoes(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == "SET" {
			return "value-of-set", true
		}
		return "", false
	}
	values := []string{
		"", "plain", "price: $5", "$$", "$ $", "trailing $",
		"$SET", "${SET}", "$UNSET", "${UNSET}", "pre/$SET/post",
		"$SET$SET", "$A1_${B2_}tail", "x$A1y",
		"${X:-fallback}", "${X@P}", "${X@U}", "$'quoted'",
		"${unterminated", "${not a name}", "${}", "$1digits",
		"a<b and a>b", "3<8 and 8>3", "<(nope) >(nope)",
	}
	for _, v := range values {
		expanded := expandReferences(v, lookup)
		got, want := envtrust.CarriesExpansionReference(v), expanded != v
		if got != want {
			t.Errorf("CarriesExpansionReference(%q) = %v and the expander %s — the scan and this lane's grammar disagree",
				v, got, map[bool]string{true: "changed the value", false: "kept it literal"}[want])
		}
	}
	for _, v := range []string{`\$SET`, `a\$b`, `\${HOME}/escaped`} {
		if envtrust.CarriesExpansionReference(v) {
			t.Errorf("%q is called a reference: an escape is not one, the raw lanes keep the bytes", v)
		}
	}
	// The forms the scan DOES answer are the ones this lane expands — one
	// shape end to end, both spellings.
	for _, v := range []string{"$SET", "${SET}"} {
		if expanded := expandReferences(v, lookup); expanded != "value-of-set" || !envtrust.CarriesExpansionReference(v) {
			t.Errorf("%q expanded to %q: the scan and the expander lost the simple reference", v, expanded)
		}
	}
}
