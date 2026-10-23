package secretguard

import "testing"

// A name is declared by an exact table entry, never by the placeholder its
// spelling can generate. Replacing the lookup with defaultPlaceholder reddens
// the unknown, sanitized alias, RedactOnly and custom-placeholder rows.
func TestResolveSecretRefRequiresAnExactDeclaration(t *testing.T) {
	for _, placeholders := range []bool{false, true} {
		cfg := DefaultConfig()
		cfg.Placeholders = placeholders
		g := New([]Secret{
			{Name: "ITERION-A", Value: "declared-value-12345"},
			{Name: "CUSTOM", Placeholder: "ITERION_CUSTOM", Value: "custom-value-12345"},
			{Name: "PRIVATE", Value: "private-value-12345", RedactOnly: true},
		}, cfg)
		for _, name := range []string{"ITERION_UNKNOWN", "ITERION_A", "PRIVATE", ""} {
			if got := g.ResolveSecretRef(name); got != "" {
				t.Errorf("placeholders=%v: undeclared name %q resolves to %q", placeholders, name, got)
			}
		}
		for _, tc := range []struct{ name, placeholder, value string }{
			{"ITERION-A", "__ITERION_SECRET_ITERION_A__", "declared-value-12345"},
			{"CUSTOM", "ITERION_CUSTOM", "custom-value-12345"},
		} {
			want := tc.value
			if placeholders {
				want = tc.placeholder
			}
			if got := g.ResolveSecretRef(tc.name); got != want {
				t.Errorf("placeholders=%v: declared name %q resolves to %q, want %q", placeholders, tc.name, got, want)
			}
		}
	}
}
