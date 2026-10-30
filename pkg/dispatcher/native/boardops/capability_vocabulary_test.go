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
