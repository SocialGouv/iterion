package runner

import (
	"errors"
	"fmt"
	"strings"
)

// ErrForgeCredentialRefused marks a git operation the forge refused although
// the runner presented the run's forge credential: the forge answered 401 to
// the token, so the token is expired, revoked or carries no access to the
// repository. errors.Is reads it through every wrap of the workspace
// preparation.
var ErrForgeCredentialRefused = errors.New("the forge refused the run's forge credential")

// gitAuthRefusalMarkers are the phrases git and the major forges print when
// an authenticated HTTPS request is answered 401. "could not read Username"
// is git falling back to an interactive prompt after the refusal — with
// GIT_TERMINAL_PROMPT=0 that prompt fails, and its message names a username
// nobody asked for instead of the refused token.
var gitAuthRefusalMarkers = []string{
	"could not read username",
	"could not read password",
	"authentication failed for",
	"invalid username or password",
	"invalid username or token",
	"http basic: access denied",
	"the requested url returned error: 401",
}

// forgeCredentialRefused types err as ErrForgeCredentialRefused when a git
// operation that presented credName's token failed on an authentication
// refusal, and returns err unchanged otherwise (credName empty — no token was
// presented — or another failure). git's own output stays wrapped for the
// operator.
func forgeCredentialRefused(err error, credName string) error {
	if err == nil || credName == "" {
		return err
	}
	detail := strings.ToLower(err.Error())
	for _, marker := range gitAuthRefusalMarkers {
		if strings.Contains(detail, marker) {
			return fmt.Errorf("%w: the forge answered 401 to secret %q — it is expired, revoked or has no access to this repository (a short-lived token, such as a GitHub App installation token, lives about one hour): %w",
				ErrForgeCredentialRefused, credName, err)
		}
	}
	return err
}
