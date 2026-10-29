package ir

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/knowledge"
)

// InstallsWriteTool reports whether a run handed this declared `memory:`
// block gets the store's WRITE tool — the question
// model.installWorkspaceMemory answers at dispatch (`spec.Write &&
// knowledge.AuthorizeSpaceWrite(...) == nil`), asked of the declaration alone
// so a compiler, a linter and a corpus guard read one answer.
//
// A block is not a write path; the write TOOL is. Three declarations withhold
// it:
//
//   - a block the compiler dropped — `enabled: false` compiles to no block at
//     all, and a nil receiver is "no block";
//   - `write: false`, a first-class property (it defaults to true, so the
//     read-only shape is always explicit) and the remedy C319 prescribes;
//   - `visibility: "global"`, the instance-wide catalogue no run may write:
//     a run holds no platform authority, which is what C319 says in words —
//     memory_write is NOT installed for a run.
//
// Read-only arms (`read:`, `autoload:`, `pre_compact_inject:`) are not asked
// about: they install the READ tools, which put no bytes in the tree.
func (m *Memory) InstallsWriteTool() bool {
	if m == nil || !m.Enabled || !m.Write {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(m.Visibility), string(knowledge.VisibilityGlobal))
}
