package automemory

import "testing"

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
	}{
		{"on", On},
		{"ON", On},
		{" on ", On},
		{"true", On},
		{"1", On},
		{"off", Off},
		{"false", Off},
		{"0", Off},
		{"", Off},
		// A typo is Off here on purpose: the compiler rejects it as C131,
		// so the only values that reach this function are canonical or
		// env/CLI-supplied, and "unrecognised means hermetic" is the safe
		// reading of an env var nobody validated.
		{"onn", Off},
	} {
		if got := ParseMode(tc.in); got != tc.want {
			t.Errorf("ParseMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestResolveSourced_Precedence(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		override, node, workflow, envDefault string
		wantMode                             Mode
		wantSource                           string
	}{
		{"all unset defaults off", "", "", "", "", Off, "default"},
		{"env alone", "", "", "", "on", On, "env"},
		{"workflow beats env", "", "", "on", "off", On, "workflow"},
		{"node beats workflow", "", "on", "off", "off", On, "node"},
		{"run override beats node", "on", "off", "off", "off", On, "run_override"},
		// The off direction must win just as hard: a run override of "off"
		// is the kill switch for a bot whose DSL turned memory on.
		{"run override can force off", "off", "on", "on", "on", Off, "run_override"},
		{"node can force off", "", "off", "on", "on", Off, "node"},
		{"workflow can force off", "", "", "off", "on", Off, "workflow"},
		// Whitespace is not a value: a blank env var must not out-rank the
		// workflow's declaration.
		{"blank levels are skipped", "  ", "\t", "on", "", On, "workflow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode, source := ResolveSourced(tc.override, tc.node, tc.workflow, tc.envDefault)
			if mode != tc.wantMode || source != tc.wantSource {
				t.Errorf("ResolveSourced(%q,%q,%q,%q) = (%v,%q), want (%v,%q)",
					tc.override, tc.node, tc.workflow, tc.envDefault,
					mode, source, tc.wantMode, tc.wantSource)
			}
			if got := Resolve(tc.override, tc.node, tc.workflow, tc.envDefault); got != tc.wantMode {
				t.Errorf("Resolve disagrees with ResolveSourced: %v vs %v", got, tc.wantMode)
			}
		})
	}
}

func TestModeString(t *testing.T) {
	if On.String() != "on" || Off.String() != "off" {
		t.Fatalf("Mode.String drifted: On=%q Off=%q", On.String(), Off.String())
	}
	if !On.Enabled() || Off.Enabled() {
		t.Fatal("Enabled() must be true only for On")
	}
}

// A typo in the flag must not read as "hermetic". ParseMode maps anything
// unrecognised to Off, which is the safe direction but a silent one: the
// operator who asked for memory would get none, with nothing to say why.
func TestValidateMode(t *testing.T) {
	for _, ok := range []string{"", "on", "off", "ON", " off ", "true", "false", "1", "0"} {
		if err := ValidateMode(ok); err != nil {
			t.Errorf("ValidateMode(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"noo", "ultra", "enabled", "yes", "auto"} {
		if err := ValidateMode(bad); err == nil {
			t.Errorf("ValidateMode(%q) accepted a value that resolves to a silent Off", bad)
		}
	}
	// Everything ValidateMode accepts must also be something ParseMode
	// understands — otherwise the flag passes validation and still lies.
	if ParseMode("on") != On || ParseMode("true") != On || ParseMode("1") != On {
		t.Error("ValidateMode accepts truthy spellings ParseMode does not resolve to On")
	}
}
