package bundle

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// aboveFloor is the floor one MAJOR above the constant.
//
// Derived rather than written: the case it covers ("a higher floor enables
// too") used to be the literal 99.0.0, which silently became a floor BELOW the
// constant the day the constant moved. A test whose meaning depends on a number
// staying bigger than another number is the same rot the constant itself has.
func aboveFloor(t *testing.T) string {
	t.Helper()
	major, _, ok := strings.Cut(ToolAliasesSince, ".")
	n, err := strconv.Atoi(major)
	if !ok || err != nil {
		t.Fatalf("ToolAliasesSince = %q: no numeric major to build an upper case from", ToolAliasesSince)
	}
	return fmt.Sprintf(">= %d.0.0", n+1)
}

func TestToolAliasesRequireSufficientDeclaredEngineFloor(t *testing.T) {
	if AllowsToolAliases(nil) || AllowsToolAliases(&Manifest{}) {
		t.Fatal("bare/legacy workflow enabled aliases")
	}
	for _, tc := range []struct {
		floor string
		want  bool
	}{{"", false}, {">= 3.143.0", false}, {">= " + ToolAliasesSince, true}, {aboveFloor(t), true}, {"garbage", false}} {
		if got := AllowsToolAliases(&Manifest{Requires: &Requires{Iterion: tc.floor}}); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.floor, got, tc.want)
		}
	}
}

// TestTheUnsetFloorFailsClosed is what the sentinel is FOR: while the real
// release is unknown, no manifest anyone could plausibly write may switch the
// resolver on. Asserted against versions that actually exist rather than a
// made-up one, so it keeps meaning something as the project ships.
func TestTheUnsetFloorFailsClosed(t *testing.T) {
	if ToolAliasesSince != "9999.0.0" {
		t.Skip("the floor names a real release; the deadlock this guards is over")
	}
	for _, floor := range []string{">= 3.143.0", ">= 3.146.6", ">= 4.0.0", ">= 100.0.0"} {
		if AllowsToolAliases(&Manifest{Requires: &Requires{Iterion: floor}}) {
			t.Errorf("%q enabled the resolver while its release is still unknown — the floor must fail CLOSED, not merely be documented as provisional", floor)
		}
	}
}
