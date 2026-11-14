package secretguard

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// A secret whose placeholder spells another's redact-only form is never
// resolvable: the defuse writes `__ITERION_SECRET_pw_redacted__` where pw
// must resolve nowhere, and pw_redacted would otherwise turn it into its
// own value. The compiler refuses the name (C341); the guard holds the line
// for any registration that still carries one.
func TestARedactOnlyFormIsNeverALivePlaceholder(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"pw_redacted", "pw-redacted"} {
		g := New([]Secret{{Name: "pw", Value: "pw-value-0123456789"}, {Name: name, Value: "other-value-9876543210"}}, DefaultConfig())
		twin := redactOnlyPlaceholder(PlaceholderForName("pw"))
		if !secretsink.IsRedactedForm(PlaceholderForName(name)) {
			t.Fatalf("%s: scenario broken, its placeholder is no redact-only form", name)
		}
		for i := range 200 {
			out := g.DefuseAll("k=" + PlaceholderForName("pw"))
			if out != "k="+twin {
				t.Fatalf("%s, call %d: DefuseAll = %q, want %q", name, i, out, "k="+twin)
			}
			for _, resolved := range []string{g.Materialize(out), g.MaterializeShell(out), g.MaterializeFor(secretsink.Command, out)} {
				if strings.Contains(resolved, "other-value") {
					t.Fatalf("%s, call %d: the redact-only form resolved to %s's value", name, i, name)
				}
			}
		}
		if g.NamesMaterializableSecret(twin) {
			t.Errorf("%s: the redact-only form names a materialisable secret", name)
		}
		if strings.Contains(g.Redact("other-value-9876543210"), "other-value") {
			t.Errorf("%s: the reserved secret's value is no longer scrubbed", name)
		}
	}
}

// A defuse reads its input once: what it writes is never read again, so the
// answer does not depend on the order the placeholders are visited in — a
// placeholder that begins another (`a` and `a__b`) is the case where the
// order would show. Repeated, both defuses.
func TestADefuseIsOnePassWhateverTheOrder(t *testing.T) {
	t.Parallel()
	g := New([]Secret{
		{Name: "a", Value: "a-value-0123456789", Sinks: []string{secretsink.Command}},
		{Name: "a__b", Value: "ab-value-0123456789", Sinks: []string{secretsink.Command}},
	}, DefaultConfig())
	in := "x " + PlaceholderForName("a__b") + " y " + PlaceholderForName("a") + " z"
	want := "x " + redactOnlyPlaceholder(PlaceholderForName("a__b")) + " y " + redactOnlyPlaceholder(PlaceholderForName("a")) + " z"
	for i := range 200 {
		if got := g.DefuseAll(in); got != want {
			t.Fatalf("call %d: DefuseAll = %q, want %q", i, got, want)
		}
		if got := g.DefuseUngranted(secretsink.Agent, in); got != want {
			t.Fatalf("call %d: DefuseUngranted = %q, want %q", i, got, want)
		}
	}
}

// DefuseAllValue reads a map's keys as it reads its values: a key is sent
// as written, at every depth.
func TestDefuseAllValueDefusesKeys(t *testing.T) {
	t.Parallel()
	g := New([]Secret{{Name: "pw", Value: "pw-value-0123456789"}}, DefaultConfig())
	ph := PlaceholderForName("pw")
	twin := redactOnlyPlaceholder(ph)
	in := map[string]any{ph: "v", "list": []any{map[string]any{ph: 1}}}
	out, ok := g.DefuseAllValue(in).(map[string]any)
	if !ok {
		t.Fatalf("DefuseAllValue returned %T", out)
	}
	if _, live := out[ph]; live {
		t.Error("a top-level key kept its live placeholder")
	}
	if out[twin] != "v" {
		t.Errorf("the defused key lost its value: %v", out)
	}
	inner := out["list"].([]any)[0].(map[string]any)
	if _, live := inner[ph]; live || inner[twin] != 1 {
		t.Errorf("a nested key kept its live placeholder: %v", inner)
	}
	if _, untouched := in[ph]; !untouched {
		t.Error("the input map was mutated")
	}
}
