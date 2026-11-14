package secretguard

import (
	"maps"
	"slices"
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

// A defuse leaves no live placeholder, whatever the order it visits them in
// and however they overlap. Placeholders overlap on their shared `__`
// (`…x__ITERION_SECRET_y__`), a redact-only form ends in the `__` the next
// one starts with, and one placeholder may begin another (`a`, `a__b`): in
// each, what comes out is resolved through every materialiser and must
// carry no value. Repeated, both defuses.
func TestADefuseLeavesNoLivePlaceholder(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"x":    "x-value-0123456789",
		"y":    "y-value-0123456789",
		"a":    "a-value-0123456789",
		"a__b": "ab-value-0123456789",
	}
	var all, commandOnly []Secret
	for name, v := range values {
		all = append(all, Secret{Name: name, Value: v})
		commandOnly = append(commandOnly, Secret{Name: name, Value: v, Sinks: []string{secretsink.Command, secretsink.Params}})
	}
	open, kept := New(all, DefaultConfig()), New(commandOnly, DefaultConfig())
	x, y := PlaceholderForName("x"), PlaceholderForName("y")
	inputs := []string{
		x + y[2:],                        // x then y, sharing x's closing `__`
		y + x[2:],                        // y then x
		x + x[2:],                        // x then x
		x + x[2:] + x[2:],                // three in a chain
		redactOnlyPlaceholder(x) + y[2:], // a placeholder abutting a redact-only form
		PlaceholderForName("a__b"),       // a placeholder that begins another
		"k=" + PlaceholderForName("a") + "b__ " + y,
	}
	leaks := func(resolved ...string) bool {
		for _, r := range resolved {
			for _, v := range values {
				if strings.Contains(r, v) {
					return true
				}
			}
		}
		return false
	}
	for i := range 200 {
		for _, in := range inputs {
			out := open.DefuseAll(in)
			shell, env := open.MaterializeShellEnv(out)
			if leaks(open.Materialize(out), shell, strings.Join(slices.Collect(maps.Values(env)), " "), open.MaterializeFor(secretsink.Command, out)) {
				t.Fatalf("call %d: DefuseAll(%q) = %q still holds a live placeholder", i, in, out)
			}
			out = kept.DefuseUngranted(secretsink.Agent, in)
			authored, aenv := kept.MaterializeAuthoredShellEnv(out)
			if leaks(kept.MaterializeFor(secretsink.Command, out), kept.MaterializeShell(out), authored, strings.Join(slices.Collect(maps.Values(aenv)), " ")) {
				t.Fatalf("call %d: DefuseUngranted(%q) = %q still holds a live placeholder", i, in, out)
			}
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
