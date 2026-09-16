package delegate

import (
	"strings"
	"testing"
)

// Why a ONE-ENTRY allowlist is not a small thing: every native tool the entry
// does not name is removed from the agent's reach.
//
// This is the downstream half of the guard in
// ClawExecutor.assembleEffectiveTools. Measured in production: a node that
// declared no `tools:` but did declare `interaction: human` had `ask_user`
// promoted into its allowlist, arrived here with exactly one entry, and lost
// Bash, Read, Write, Edit, Glob and Grep in one step. Its only remaining call
// was the ask_user it had just been given, so it asked a placeholder question
// and the run paused — a fourteen-tool agent silently reduced to a doorbell.
//
// Kept here, beside the mechanism, so that widening the allowlist upstream
// cannot quietly reintroduce the failure: if this ever stops stripping, the
// upstream guard has become load-bearing for a different reason and must be
// re-read.
func TestClaudeNativeDisallowedTools_OneNonNativeEntryStripsEverything(t *testing.T) {
	stripped := claudeNativeDisallowedTools([]string{"ask_user"}, false)

	if len(stripped) == 0 {
		t.Fatal("a one-entry allowlist naming no native tool must strip the native surface")
	}
	for _, want := range []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep"} {
		var found bool
		for _, got := range stripped {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q is not stripped by a one-entry allowlist; got %s",
				want, strings.Join(stripped, ","))
		}
	}
}

// The other face: NO declaration must leave the native surface untouched.
// This is the legacy semantics the upstream guard exists to protect.
func TestClaudeNativeDisallowedTools_NoDeclarationRestrictsNothing(t *testing.T) {
	if got := claudeNativeDisallowedTools(nil, false); len(got) != 0 {
		t.Errorf("an undeclared tool set must restrict nothing, got %v", got)
	}
}
