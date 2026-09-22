package platformcfg

import "testing"

// TestInfraNamesCoverTheEnforcementSwitches: botVarsInfraExact exists so a
// stored bot var can never become an infra override. The two project-trust
// switches are the ones that matter most — each lifts a refusal that stops an
// agent CLI from executing code out of the repository under review — and the
// two binary pins decide which binary runs.
func TestInfraNamesCoverTheEnforcementSwitches(t *testing.T) {
	for _, name := range []string{
		"ITERION_PI_TRUST_PROJECT",
		"ITERION_OPENCODE_TRUST_PROJECT",
		"ITERION_PI_BIN",
		"ITERION_OPENCODE_BIN",
	} {
		if !botVarsInfraExact[name] {
			t.Errorf("%s is writable as a bot var; it decides what runs or whether a refusal holds", name)
		}
		if botVarNameOK(name) {
			t.Errorf("%s passes botVarNameOK; the infra list is not consulted", name)
		}
	}
}
