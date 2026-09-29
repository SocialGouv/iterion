package spec

import (
	"slices"
	"testing"
)

// Monaco unions the enum words of every property that shares a name across
// kinds (unionWords), so iterDslEnumValuesByProperty has one list per
// property NAME — and the studio's dropdowns read it as the property's
// values. Two same-named enum properties with divergent Values would
// silently over-offer on both surfaces: the dropdown of the narrower
// property would offer words its own parser refuses. Hold the invariant the
// union assumes: same name, same words (#1888).
func TestSameNamedEnumPropertiesAgreeOnTheirWords(t *testing.T) {
	wordsByName := map[string][]string{}
	ownerByName := map[string]string{}
	for _, kind := range Kinds {
		for _, p := range kind.Properties {
			if p.Form != Enum && p.Form != EnumOrEnv {
				continue
			}
			words := slices.Clone(p.Values)
			slices.Sort(words)
			if known, seen := wordsByName[p.Name]; seen {
				if !slices.Equal(known, words) {
					t.Errorf("%s.%s declares %v, but %s.%s declares %v — one iterDslEnumValuesByProperty entry unions them, and the studio reads it as each property's values",
						kind.Name, p.Name, words, ownerByName[p.Name], p.Name, known)
				}
				continue
			}
			wordsByName[p.Name] = words
			ownerByName[p.Name] = kind.Name
		}
	}
}
