package supervise

import "testing"

func TestDeclaredEnabledPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		override string
		env      string
		want     bool
		source   string
	}{
		{"default on", "", "", true, "default"},
		{"env off", "", "off", false, EnabledEnv},
		{"env 0", "", "0", false, EnabledEnv},
		{"env on", "", "on", true, EnabledEnv},
		{"whitespace env is unset", "", "  ", true, "default"},
		{"override off wins over env on", "off", "on", false, "run-level override"},
		{"override on wins over env off", "on", "off", true, "run-level override"},
		{"unreadable env falls through to on, named", "", "banana", true, EnabledEnv + ` unreadable ("banana") — default on`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnabledEnv, tc.env)
			got, source := DeclaredEnabled(tc.override)
			if got != tc.want || source != tc.source {
				t.Fatalf("DeclaredEnabled(%q) with env %q = (%v, %q); want (%v, %q)",
					tc.override, tc.env, got, source, tc.want, tc.source)
			}
		})
	}
}

func TestValidateSupervisorsMode(t *testing.T) {
	// The flag accepts exactly the env grammar, so a wrapper forwarding
	// ITERION_SUPERVISORS as --supervisors cannot break.
	for _, ok := range []string{"", "on", "off", "ON", " off ", "0", "1", "true", "false", "yes", "no"} {
		if err := ValidateSupervisorsMode(ok); err != nil {
			t.Errorf("ValidateSupervisorsMode(%q) = %v; want nil", ok, err)
		}
	}
	// A typo must be rejected, not silently read as "inherit".
	for _, bad := range []string{"o", "disabled", "banana"} {
		if err := ValidateSupervisorsMode(bad); err == nil {
			t.Errorf("ValidateSupervisorsMode(%q) = nil; want error", bad)
		}
	}
}
