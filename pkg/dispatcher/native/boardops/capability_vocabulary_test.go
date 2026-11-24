package boardops

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/boardcaps"
)

// The vocabulary lives in pkg/boardcaps so the DSL compiler can read it
// without linking this package (iterion#2857); the two halves must not drift:
// a name in boardcaps no tool here gates on would VALIDATE and then do
// nothing, and a capability this package re-exports under a different string
// would be the second list the move exists to delete.
func TestTheCapabilityNamesAreTheLeafsAndEachGatesATool(t *testing.T) {
	aliases := map[string]string{
		"BoardRead":    CapBoardRead,
		"BoardCreate":  CapBoardCreate,
		"BoardMove":    CapBoardMove,
		"BoardAssign":  CapBoardAssign,
		"BoardLabel":   CapBoardLabel,
		"BoardClose":   CapBoardClose,
		"BoardComment": CapBoardComment,
	}
	leaf := map[string]string{
		"BoardRead":    boardcaps.BoardRead,
		"BoardCreate":  boardcaps.BoardCreate,
		"BoardMove":    boardcaps.BoardMove,
		"BoardAssign":  boardcaps.BoardAssign,
		"BoardLabel":   boardcaps.BoardLabel,
		"BoardClose":   boardcaps.BoardClose,
		"BoardComment": boardcaps.BoardComment,
	}
	for name, value := range leaf {
		if aliases[name] != value {
			t.Errorf("Cap%s = %q, the leaf says %q — the re-export drifted from the vocabulary", name, aliases[name], value)
		}
		gates := 0
		for _, tool := range allTools {
			if tool.Capability == value {
				gates++
			}
		}
		if gates == 0 {
			t.Errorf("%s (%q) gates no tool of this package — it would validate in the DSL and do nothing", name, value)
		}
	}
}

// The leaf's tool→capability table (what the DSL compiler's C328 reads) and
// this registry's pins are two spellings of one decision: drift either way
// and an author is told to declare a capability that opens nothing, or a
// tool answers to a capability no validator names.
func TestTheLeafsToolTableMatchesTheRegistry(t *testing.T) {
	seen := map[string]string{}
	for _, tool := range allTools {
		seen[tool.Name] = tool.Capability
		want, ok := boardcaps.ToolCapability[tool.Name]
		if !ok {
			t.Errorf("tool %q gates on %q but the leaf's ToolCapability has no row — the compiler cannot name what opens it", tool.Name, tool.Capability)
			continue
		}
		if want != tool.Capability {
			t.Errorf("tool %q gates on %q, the leaf says %q", tool.Name, tool.Capability, want)
		}
	}
	for name := range boardcaps.ToolCapability {
		if _, ok := seen[name]; !ok {
			t.Errorf("the leaf's ToolCapability names %q, which no tool of this registry serves", name)
		}
	}
}
