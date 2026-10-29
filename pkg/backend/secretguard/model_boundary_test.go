package secretguard

import (
	"strings"
	"testing"
)

// RedactForModel is the model boundary's pass: known values back to their
// placeholders, under the Layer 1 switch — placeholders off means the
// operator asked for values in prompts, and the boundary renders them too.
func TestRedactForModelFollowsThePlaceholderSwitch(t *testing.T) {
	const value = "tok-2e7a1b3d4f9c-model"
	g := newTestGuard(t, Secret{Name: "api", Value: value})
	ph := defaultPlaceholder("api")
	if got := g.RedactForModel("A said " + value + " twice " + value); strings.Contains(got, value) || strings.Count(got, ph) != 2 {
		t.Fatalf("RedactForModel = %q, want both values as %s", got, ph)
	}
	// Known values only: a token shape the heuristic would mark is not the
	// boundary's business — the model reads what the workspace holds.
	if got := g.RedactForModel("AKIAIOSFODNN7EXAMPLE stays"); got != "AKIAIOSFODNN7EXAMPLE stays" {
		t.Fatalf("RedactForModel ran the heuristic pass: %q", got)
	}
	off := DefaultConfig()
	off.Placeholders = false
	g2 := New([]Secret{{Name: "api", Value: value}}, off)
	if got := g2.RedactForModel("A said " + value); got != "A said "+value {
		t.Fatalf("placeholders off, yet RedactForModel redacted: %q", got)
	}
	var none *Guard
	if got := none.RedactForModel(value); got != value {
		t.Fatalf("a nil guard redacted: %q", got)
	}
}
