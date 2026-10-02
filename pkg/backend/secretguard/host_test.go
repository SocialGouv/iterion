package secretguard

import (
	"encoding/base64"
	"math"
	"strings"
	"testing"
)

func TestMaterializeForHost_Scoping(t *testing.T) {
	const real = "ghp_REAL0123456789abcdefABCDEF0123456789"
	g := New([]Secret{
		{Name: "gh", Value: real, Placeholder: "__ITERION_SECRET_gh__", Hosts: []string{"api.github.com"}},
	}, DefaultConfig())
	ph := "__ITERION_SECRET_gh__"

	// Approved host (and parent-domain): substitute.
	if got, _ := g.MaterializeForHostWithin("Bearer "+ph, "api.github.com", math.MaxInt); got != "Bearer "+real {
		t.Errorf("approved host not materialised: %q", got)
	}
	// Unapproved host: placeholder left intact.
	if got, _ := g.MaterializeForHostWithin("Bearer "+ph, "evil.com", math.MaxInt); got != "Bearer "+ph {
		t.Errorf("unapproved host should not materialise: %q", got)
	}
}

// MaterializeForHostWithin refuses a result longer than its limit — the
// length known before the substitution allocates it — and returns its input
// untouched then; a result exactly at the limit, a value shorter than its
// placeholder, and a host the secret is not scoped to are no refusal.
func TestMaterializeForHostWithinRefusesAResultPastItsLimit(t *testing.T) {
	value := strings.Repeat("v", 100)
	g := New([]Secret{
		{Name: "a", Value: value, Placeholder: "__ITERION_SECRET_a__", Hosts: []string{"example.com"}},
		{Name: "b", Value: "short-b", Placeholder: "__ITERION_SECRET_b__"},
	}, DefaultConfig())
	in := "x" + strings.Repeat("__ITERION_SECRET_a__", 3)
	want := "x" + strings.Repeat(value, 3)
	if out, ok := g.MaterializeForHostWithin(in, "example.com", len(want)); !ok || out != want {
		t.Errorf("a result exactly at the limit: ok %v, %d bytes; want it substituted", ok, len(out))
	}
	if out, ok := g.MaterializeForHostWithin(in, "example.com", len(want)-1); ok || out != in {
		t.Errorf("a result one byte past the limit: ok %v, %d bytes; want the input back, refused", ok, len(out))
	}
	if out, ok := g.MaterializeForHostWithin(in, "evil.com", len(in)); !ok || out != in {
		t.Errorf("a host the secret is not scoped to: ok %v, %q", ok, out)
	}
	shrink := strings.Repeat("__ITERION_SECRET_b__", 4)
	if out, ok := g.MaterializeForHostWithin(shrink, "any.example", len(shrink)); !ok || out != strings.Repeat("short-b", 4) {
		t.Errorf("a value shorter than its placeholder: ok %v, %q", ok, out)
	}
	// Two substitutions, the first within the limit, the second past it
	// (whichever the guard makes first): the refusal hands back the input,
	// never a string half substituted.
	two := New([]Secret{
		{Name: "a", Value: value, Placeholder: "__ITERION_SECRET_a__"},
		{Name: "c", Value: strings.Repeat("c", 100), Placeholder: "__ITERION_SECRET_c__"},
	}, DefaultConfig())
	both := "__ITERION_SECRET_a__ __ITERION_SECRET_c__"
	if out, ok := two.MaterializeForHostWithin(both, "example.com", len(both)+80+79); ok || out != both {
		t.Errorf("the second substitution past the limit: ok %v, %q; want the input back, refused", ok, out)
	}
	// An input already past the limit, with nothing to substitute: the
	// input itself is the result the caller would hold.
	if _, ok := g.MaterializeForHostWithin("no placeholder here", "example.com", 4); ok {
		t.Error("an input already past the limit, with no placeholder, was accepted")
	}
	var none *Guard
	if _, ok := none.MaterializeForHostWithin("no guard at all", "example.com", 4); ok {
		t.Error("an input past the limit was accepted by a nil guard")
	}
	if out, ok := g.MaterializeForHostWithin(in, "example.com", math.MaxInt); !ok || out != want {
		t.Errorf("the largest limit: ok %v, %d bytes, want %d", ok, len(out), len(want))
	}
}

// A result that fits is substituted whichever order the secrets are
// declared in: one that shrinks frees room for one that grows, and the
// verdict is the result's length, not an intermediate one.
func TestMaterializeForHostWithinIsDecidedByTheResultNotTheOrder(t *testing.T) {
	grow := Secret{Name: "grow", Value: strings.Repeat("g", 300), Placeholder: "__ITERION_SECRET_grow__"}
	shrink := Secret{Name: "shrink", Value: "sh0rt", Placeholder: "__ITERION_SECRET_shrink__"}
	in := grow.Placeholder + " " + shrink.Placeholder
	want := grow.Value + " " + shrink.Value
	for _, order := range [][]Secret{{grow, shrink}, {shrink, grow}} {
		g := New(order, DefaultConfig())
		if out, ok := g.MaterializeForHostWithin(in, "any.example", len(want)); !ok || out != want {
			t.Errorf("declared %s first: ok %v, %d bytes; want the %d-byte result", order[0].Name, ok, len(out), len(want))
		}
		if out, ok := g.MaterializeForHostWithin(in, "any.example", len(want)-1); ok || out != in {
			t.Errorf("declared %s first, one byte under the result: ok %v, %d bytes; want the input back, refused", order[0].Name, ok, len(out))
		}
	}
}

func TestExfiltratesTo_Gate(t *testing.T) {
	const real = "ghp_REAL0123456789abcdefABCDEF0123456789"
	g := New([]Secret{
		{Name: "gh", Value: real, Placeholder: "__ITERION_SECRET_gh__", Hosts: []string{"github.com"}},
	}, DefaultConfig())

	// Real value to an unapproved host → exfiltration.
	if !g.ExfiltratesTo("Authorization: Bearer "+real, "evil.com") {
		t.Error("real value to unapproved host should be flagged")
	}
	// Parent-domain match: api.github.com is permitted by github.com.
	if g.ExfiltratesTo("Bearer "+real, "api.github.com") {
		t.Error("approved (parent-domain) host must not be flagged")
	}
	// base64-encoded value to an unapproved host → still caught.
	enc := base64.StdEncoding.EncodeToString([]byte(real))
	if !g.ExfiltratesTo("blob="+enc, "evil.com") {
		t.Error("base64-encoded value should be caught by DLP")
	}
}

func TestHostScoping_UnrestrictedSecret(t *testing.T) {
	const real = "sk-MODELKEY-0123456789abcdefABCDEF"
	// Empty Hosts = unrestricted (e.g. the model API key).
	g := New([]Secret{
		{Name: "model", Value: real, Placeholder: "__ITERION_SECRET_model__"},
	}, DefaultConfig())

	// Materialises anywhere; never flagged as exfiltration.
	if got, _ := g.MaterializeForHostWithin("__ITERION_SECRET_model__", "anything.example", math.MaxInt); !strings.Contains(got, real) {
		t.Errorf("unrestricted secret should materialise anywhere: %q", got)
	}
	if g.ExfiltratesTo(real, "anything.example") {
		t.Error("unrestricted secret must never be flagged as exfiltration")
	}
}
