package delegate

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// The projection's keys are the shared table's keys. A key that is not a
// canonical fixed point can never be reached, so the row would be dead
// config that reads as configured.
func TestEveryProjectionKeyIsACanonicalFixedPoint(t *testing.T) {
	for key := range claudeNativeForCanonical {
		if got := toolcatalog.CanonicalToolName(key); got != key {
			t.Errorf("projection key %q canonicalises to %q — no declaration can reach it", key, got)
		}
	}
}

// Claude Code's own spelling of a native tool must GRANT that tool and BOUND
// it: the roster is closed and iterion turns a `tools:` list into
// --disallowedTools over it, so a name the projection cannot reach is a tool
// nobody can declare — and a name the gate canonicalises elsewhere is a tool
// a rule cannot bound.
//
// This is what turns the NEXT missing row into a red test: add a fifteenth
// native tool, or drop a row from the shared table, and the pair stops
// agreeing here instead of in a run.
func TestEveryNativeToolIsGrantableAndBoundableByItsOwnSpelling(t *testing.T) {
	for _, native := range claudeNativeTools {
		key := toolcatalog.CanonicalToolName(native)
		granted, ok := claudeNativeForCanonical[key]
		if !ok {
			t.Errorf("native %q canonicalises to %q, which the projection does not grant: no `tools:` entry can keep it", native, key)
			continue
		}
		if !slices.Contains(granted, native) {
			t.Errorf("native %q canonicalises to %q, which grants %v instead", native, key, granted)
		}
		// …and the grant is honoured end to end: declaring the tool by its
		// own name must not put it on the disallow list.
		if slices.Contains(claudeNativeDisallowedTools([]string{native}, true, false), native) {
			t.Errorf("declaring %q disallows %q", native, native)
		}
	}
}

// The Claw alias tier is the third roster that names the same tools. An alias
// and the builtin it resolves to are one tool, so a rule must bound both.
func TestEveryClawAliasCanonicalisesWithTheBuiltinItResolvesTo(t *testing.T) {
	for _, alias := range []string{"Read", "Bash", "Grep"} {
		builtin := toolcatalog.BuiltinAlias(alias)
		if builtin == "" {
			t.Fatalf("%q is no longer a Claw alias — update this roster", alias)
		}
		if a, b := toolcatalog.CanonicalToolName(alias), toolcatalog.CanonicalToolName(builtin); a != b {
			t.Errorf("alias %q → %q canonicalise apart (%q vs %q): a rule bounds one and not the other", alias, builtin, a, b)
		}
	}
}

// The projection's rows must be EXACTLY the natives whose canonical key is
// that row. Inclusion alone is not enough: a row may not hand out an extra
// native, or `tools: [bash]` would silently keep `Write` while the table
// reads as a faithful projection.
func TestAProjectionRowGrantsOnlyTheNativesThatCanonicaliseOntoIt(t *testing.T) {
	for key, granted := range claudeNativeForCanonical {
		for _, native := range granted {
			if got := toolcatalog.CanonicalToolName(native); got != key {
				t.Errorf("row %q grants %q, which canonicalises to %q: the row hands out a tool no declaration of %q names", key, native, got, key)
			}
		}
	}
}
