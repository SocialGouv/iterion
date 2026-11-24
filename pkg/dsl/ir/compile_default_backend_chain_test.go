package ir

import "testing"

// C018 reads the node's route the way the source does (sourceBackend, by
// default): a decided name answers, a field the source writes but cannot
// answer is declared and not judged, and an empty or `auto` field falls
// through to the workflow default — which answers the same way. Only when
// nothing in the chain decides does the target's capability verdict speak.
func TestCompileBackendChainDecidesC018(t *testing.T) {
	absent := false
	cases := []struct {
		name      string
		nodeField string
		workflow  string
		wantC018  bool
	}{
		{"a decided workflow default answers", "", "  default_backend: \"claude_code\"\n", false},
		{"an undecided workflow default is declared, not judged", "", "  default_backend: \"${UNSET_VAR}\"\n", false},
		{"an auto workflow default falls through to the target", "", "  default_backend: auto\n", true},
		{"a decided node backend answers", "  backend: \"claw\"\n", "", false},
		{"an undecided node backend is declared, not judged", "  backend: \"${UNSET_VAR}\"\n", "", false},
		{"an auto node backend falls through to the target", "  backend: auto\n", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "schema s:\n  ok: bool\n\nagent a:\n  output: s\n" + tc.nodeField + "\nworkflow w:\n" + tc.workflow + "  entry: a\n  a -> done\n"
			r := CompileWithCapabilities(parseFile(t, src), CompileCapabilities{AutoBackendAvailable: &absent})
			found := false
			for _, d := range r.Diagnostics {
				if d.Code == DiagMissingModelOrBackend {
					found = true
				}
			}
			if found != tc.wantC018 {
				t.Fatalf("C018 = %v, want %v (diagnostics: %v)", found, tc.wantC018, r.Diagnostics)
			}
		})
	}
}
