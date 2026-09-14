package bundle

import "testing"

func TestToolAliasesRequireSufficientDeclaredEngineFloor(t *testing.T) {
	if AllowsToolAliases(nil) || AllowsToolAliases(&Manifest{}) {
		t.Fatal("bare/legacy workflow enabled aliases")
	}
	for _, tc := range []struct {
		floor string
		want  bool
	}{{"", false}, {">= 3.143.0", false}, {">= " + ToolAliasesSince, true}, {">= 99.0.0", true}, {"garbage", false}} {
		if got := AllowsToolAliases(&Manifest{Requires: &Requires{Iterion: tc.floor}}); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.floor, got, tc.want)
		}
	}
}
