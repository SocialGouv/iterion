package github

import (
	"reflect"
	"testing"
)

// A bot that opens a PR then waits on its checks runs `gh pr checks` with the
// RUN token. That token carried neither CI read grant, so the command failed
// on an installation whose owner had already approved both — the manifest
// requests them by default.
func TestRuntimePermissionsForCarriesTheCIReadGrants(t *testing.T) {
	granted := map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write",
		"metadata": "read", "repository_hooks": "write",
		PermissionChecks: "read", PermissionStatuses: "write",
	}
	got := RuntimePermissionsFor(granted)
	if got[PermissionChecks] != "read" {
		t.Errorf("checks = %q, want read — `gh pr checks` reads the check-runs list", got[PermissionChecks])
	}
	if got[PermissionStatuses] != "read" {
		t.Errorf("statuses = %q, want read — iterion's own merge gate is a commit STATUS, not a check-run", got[PermissionStatuses])
	}
	if got["contents"] != "write" {
		t.Errorf("the baseline must survive: %v", got)
	}
}

// The same rule as every other grant: never request what the installation did
// not approve — one over-request 422s the whole mint.
func TestRuntimePermissionsForSkipsUngrantedCIReads(t *testing.T) {
	granted := map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write",
		"metadata": "read", "repository_hooks": "write",
	}
	got := RuntimePermissionsFor(granted)
	for _, absent := range []string{PermissionChecks, PermissionStatuses} {
		if _, ok := got[absent]; ok {
			t.Errorf("requested %q which the installation did not grant", absent)
		}
	}
}

// The management token is a different token with a different job (hooks,
// repos, the gate's own status write). The CI reads belong to the RUN token;
// folding them into the baseline would hand them to every server-side call.
func TestManagementPermissionsForDoesNotAcquireTheCIReads(t *testing.T) {
	granted := map[string]string{
		"contents": "write", "metadata": "read",
		PermissionChecks: "read", PermissionStatuses: "read",
	}
	got := ManagementPermissionsFor(granted)
	if !reflect.DeepEqual(got, map[string]string{"contents": "write", "metadata": "read"}) {
		t.Errorf("got %v, want the baseline ∩ grant", got)
	}
}
