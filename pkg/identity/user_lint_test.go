package identity

import (
	"testing"
)

// UpdateUser replaces a user document WHOLE (ReplaceOneChecked). It is how a
// write founded on an earlier read reverts a field it never meant to touch:
// a password reset carrying the Status it read revives an account disabled
// in between, a login persisting a default team restores a just-revoked
// IsSuperAdmin and a just-rotated PasswordHash. Every production writer is a
// PatchUser now — the fields it owns, and a precondition where the write
// must not land on an account a deactivation reached first.
//
// This lint keeps it that way: it enumerates the callers from the METHOD, so
// a nineteenth writer cannot appear unnoticed the way the first eighteen
// did. A new non-test caller outside this package justifies itself here, in
// the allow-list below, and not in code.
func TestUpdateUserHasNoProductionCallers(t *testing.T) {
	// Paths allowed to keep a whole-row user write, with the reason. Empty
	// today: the conversion left none.
	allowed := map[string]string{}
	assertNoProductionCallers(t, "UpdateUser",
		"a whole-document user replace outside pkg/identity (use PatchUser with the fields this "+
			"writer owns; add an entry to `allowed` with a reason if it truly must replace the row)",
		allowed)
}
