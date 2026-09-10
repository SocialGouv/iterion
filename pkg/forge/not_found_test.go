package forge

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// StatusErr is the one place every provider maps a status onto an error, so
// it is the one place a 404 can stop meaning "hook". A `GET pull` that misses
// must not read "forge: hook not found" — the message an operator takes to
// the webhook settings of a repo whose PR simply does not exist.
func TestStatusErr_NotFoundIsTypedPerOperation(t *testing.T) {
	for _, op := range []string{"GET hooks", "create hook", "update hook", "delete hook"} {
		err := StatusErr("github", op, http.StatusNotFound)
		if !errors.Is(err, ErrHookNotFound) {
			t.Errorf("StatusErr(%q, 404) = %v, want ErrHookNotFound — deprovision reads it as \"already gone\"", op, err)
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("StatusErr(%q, 404) = %v, want it to answer errors.Is(ErrNotFound) too", op, err)
		}
	}
	for _, op := range []string{"GET pull", "GET issue", "GET /user", "GET check-runs"} {
		err := StatusErr("github", op, http.StatusNotFound)
		if errors.Is(err, ErrHookNotFound) {
			t.Errorf("StatusErr(%q, 404) = %v — a missing %s is not a missing hook", op, err, op)
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("StatusErr(%q, 404) = %v, want ErrNotFound", op, err)
		}
		if !strings.Contains(err.Error(), op) {
			t.Errorf("StatusErr(%q, 404) = %q, want the operation named", op, err)
		}
	}
}

// A 404 whose caller knows the grants the endpoint is gated on carries them:
// GitHub answers 404 (not 403) for a resource a credential may not see, so
// naming the grants is what lets an operator tell an absent object from a
// withheld permission.
func TestNotFoundError_NamesTheGrantsWhenKnown(t *testing.T) {
	err := error(&NotFoundError{Provider: ProviderGitHub, Op: "GET pull", MayNeed: []string{"pull_requests:read", "contents:read"}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("NotFoundError must unwrap to ErrNotFound, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"github", "GET pull", "pull_requests:read", "contents:read"} {
		if !strings.Contains(msg, want) {
			t.Errorf("NotFoundError message %q must contain %q", msg, want)
		}
	}
	var pe *PermissionError
	if errors.As(err, &pe) {
		t.Error("a 404 must not claim a permission gap it cannot prove")
	}
}
