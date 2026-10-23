package main

import (
	"strings"
	"testing"
)

// TestRemoteAdminUsersHelp_NamesTheRefusalOnOnesOwnAccount: the instance
// refuses `admin users reset-password <self>` (403) when the credential
// presented is a personal access token — which is what `remote login` stores,
// so it is what the CLI presents on every run. The help has to say so and
// point at the path that does work: the operator reading only this text would
// otherwise read the 403 as a permission problem on a super-admin account
// that has every permission.
func TestRemoteAdminUsersHelp_NamesTheRefusalOnOnesOwnAccount(t *testing.T) {
	// The text is hard-wrapped for a terminal, so the sentences are matched
	// on one line: a phrase broken across two source lines is the same
	// sentence to the reader.
	long := strings.Join(strings.Fields(remoteAdminUsersCmd.Long), " ")
	for _, want := range []string{
		"personal access token",
		"refuses it (403) on YOUR OWN account",
		"Change password",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("`admin users` help does not carry %q:\n%s", want, long)
		}
	}
}
