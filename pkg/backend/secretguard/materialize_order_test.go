package secretguard

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// A placeholder can extend another — `a__b`'s begins with `a`'s — and a
// value can spell a placeholder. Materialising swaps each placeholder for
// its value in ONE pass, the longest placeholder winning where two start at
// one position, so the answer depends on neither the order the guard's map
// is visited in nor on what a value spells (#3291). Each guard is built
// anew, so the map order varies across the iterations.
func TestMaterializeTakesTheLongestPlaceholderInOnePass(t *testing.T) {
	t.Parallel()
	a, ab, c := PlaceholderForName("a"), PlaceholderForName("a__b"), PlaceholderForName("c")
	values := map[string]string{"a": "va-it's", "a__b": "vab-0123", "c": a}
	text := "x " + ab + " y " + a + " z " + c
	want := "x vab-0123 y va-it's z " + a
	wantShell := "x vab-0123 y va-it'\\''s z " + a
	for range 200 {
		var secrets []Secret
		for name, v := range values {
			secrets = append(secrets, Secret{Name: name, Value: v, Sinks: []string{secretsink.Agent, secretsink.Command}})
		}
		g := New(secrets, DefaultConfig())
		for name, got := range map[string]string{
			"Materialize":             g.Materialize(text),
			"MaterializeFor(command)": g.MaterializeFor(secretsink.Command, text),
		} {
			if got != want {
				t.Fatalf("%s(%q) = %q, want %q", name, text, got, want)
			}
		}
		if got := g.MaterializeShell(text); got != wantShell {
			t.Fatalf("MaterializeShell(%q) = %q, want %q", text, got, wantShell)
		}
		if got := g.MaterializeShell(ab); strings.Contains(got, "va-it") {
			t.Fatalf("MaterializeShell(%q) = %q: a's value inside a__b's placeholder", ab, got)
		}
	}
}
