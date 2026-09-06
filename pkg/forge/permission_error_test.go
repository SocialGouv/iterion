package forge

import (
	"errors"
	"strings"
	"testing"
)

// The typed refusal has ONE job: turn a bare status code into the permission
// to approve and where. Its text must carry all three — the call, the grant,
// the step — and the forge's own sentinel must stay reachable through it, so
// no caller that classified on ErrForbidden / ErrPermissionsNotGranted
// changes behaviour when the error grows a type.
func TestPermissionErrorNamesTheGrantAndKeepsTheSentinel(t *testing.T) {
	cause := errors.New("github: mint installation token: HTTP 422: The permissions requested are not granted to this installation.")
	err := &PermissionError{
		Provider: ProviderGitHub,
		Op:       "mint installation token",
		Missing:  []string{"checks:read", "statuses:read"},
		Remedy:   "approve them on the installation",
		Cause:    errors.Join(ErrPermissionsNotGranted, cause),
	}
	msg := err.Error()
	for _, want := range []string{"github", "mint installation token", "checks:read", "statuses:read", "approve them on the installation", "not granted to this installation"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to name %q", msg, want)
		}
	}
	if !errors.Is(err, ErrPermissionsNotGranted) {
		t.Error("the mint sentinel must stay reachable through the typed error")
	}
	var pe *PermissionError
	if !errors.As(err, &pe) || pe.Missing[0] != "checks:read" {
		t.Errorf("errors.As must recover the typed error with its Missing list, got %+v", pe)
	}

	// The call-time shape: a 403 the forge explained.
	refused := &PermissionError{Provider: ProviderGitHub, Op: "GET check-runs", Missing: []string{"checks:read"}, Cause: ErrForbidden}
	if !errors.Is(refused, ErrForbidden) {
		t.Error("a refused call must still read as ErrForbidden")
	}
	if !strings.Contains(refused.Error(), "missing permission checks:read") {
		t.Errorf("Error() = %q, want the missing grant named", refused.Error())
	}
}
