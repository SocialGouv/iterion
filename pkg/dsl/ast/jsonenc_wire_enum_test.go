package ast

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// emitWords lists the words a jsonenc enum→string map can emit, sorted.
func emitWords[K comparable](m map[K]string) []string {
	words := make([]string, 0, len(m))
	for _, w := range m {
		words = append(words, w)
	}
	slices.Sort(words)
	return words
}

// The studio types its session/await/interaction wire unions by deriving
// them from the generated registry module (studio/src/api/types.ts ←
// iterDsl.generated.ts ← pkg/dsl/spec, gated by `task dsl:check`) — see
// #1935. That derivation is honest only while the words these maps emit
// on the wire ARE the registry words: the document endpoint serves them
// verbatim. Pin both directions, so a mode added to one side without the
// other reddens here instead of silently desyncing the studio's types.
func TestJSONWireEnumsMatchTheRegistry(t *testing.T) {
	// Union the enum words of every kind's property of this name, the way
	// spec.Monaco builds iterDslEnumValuesByProperty (interaction is
	// declared on agent, judge and human with the same words).
	registryWords := func(name string) []string {
		words := []string{}
		for _, kind := range spec.Kinds {
			for _, p := range kind.Properties {
				if p.Name != name || (p.Form != spec.Enum && p.Form != spec.EnumOrEnv) {
					continue
				}
				for _, v := range p.Values {
					if !slices.Contains(words, v) {
						words = append(words, v)
					}
				}
			}
		}
		slices.Sort(words)
		return words
	}
	for _, tc := range []struct {
		property string
		emit     []string
	}{
		{"session", emitWords(sessionModeToStr)},
		{"await", emitWords(awaitModeToStr)},
		{"interaction", emitWords(interactionModeToStr)},
	} {
		if want := registryWords(tc.property); !slices.Equal(tc.emit, want) {
			t.Errorf("%s: wire emits %v but the registry accepts %v — the studio unions derive from the registry, so the maps of jsonenc.go must emit exactly its words (and await's \"none\" must stay accepted-only, never emitted)",
				tc.property, tc.emit, want)
		}
	}
}
