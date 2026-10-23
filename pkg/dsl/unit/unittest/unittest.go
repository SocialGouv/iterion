// Package unittest is how a test outside pkg/dsl/unit takes a loader's two
// answers as one unit: the whole load. A load the read budget cuts short has
// no unit to hand over — unit.Salvage shows what it read, through a view
// that hands out no unit — so a
// test that gives one here fails at the call, naming the refusal; a test
// about a short load reads the *unit.ShortError, or the view. Production
// code never imports this package (pkg/dsl/unit TestNoGuardIsReadAroundByTheBuild).
package unittest

import (
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// A binary that is no test binary and links this package refuses to start:
// production code reads through the unit loaders and answers its error. No
// production package of the module depends on this one (pkg/dsl/unit
// TestNoGuardIsReadAroundByTheBuild); this stops a binary that would.
func init() {
	if !testing.Testing() {
		panic("unittest is a test helper linked into a production binary: read through the unit loaders and answer its error")
	}
}

// Whole is a loader's answer for a load the test knows reads the bot whole:
// its unit. It panics, with the refusal, on a load cut short.
func Whole(u *unit.Unit, err error) *unit.Unit {
	if err != nil {
		panic(fmt.Sprintf("unittest.Whole: %v — a test reads a short load off its *unit.ShortError or unit.Salvage", err))
	}
	return u
}
