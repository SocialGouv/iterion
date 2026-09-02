package delegate

import "testing"

// TestIsAuthErrorResult guards the auth-failure classifier that keeps a dead
// forfait token (or rejected API key) from masquerading as a structured-output
// "missing required field" error — the exact trap that once cost a full
// debugging session.
func TestIsAuthErrorResult(t *testing.T) {
	authy := []string{
		"Failed to authenticate. API Error: 401 Invalid bearer token",
		"API Error: 401 {\"type\":\"authentication_error\",\"message\":\"invalid x-api-key\"}",
		"invalid api key",
		"OAuth token has expired",
		"Not logged in \u00b7 Please run /login",
	}
	for _, s := range authy {
		if !isAuthErrorResult(s) {
			t.Errorf("isAuthErrorResult(%q) = false, want true", s)
		}
	}

	notAuthy := []string{
		"",
		"Hello! I'm ready to help you with your workflow.",
		// A real answer that merely discusses authentication must not trip it.
		"To fix the login flow, validate the bearer token server-side and reject an invalid api key with a 401 so the client can refresh; " +
			"add a regression test that a malformed token fails to authenticate cleanly instead of 500ing, and document the error shape for the SDK consumers.",
		// The length cap keeps a real answer that QUOTES the no-credential
		// render from tripping the classifier.
		"The health probe you asked about renders \u00ab Not logged in \u00b7 Please run /login \u00bb whenever the session cookie is absent; " +
			"to reproduce it, clear the cookie jar, reload the dashboard, and assert the banner text in the e2e spec so a regression is caught early.",
		// A rate-limit notice is a DIFFERENT class (handled by isRateLimitMessage).
		"You've hit your weekly limit · resets 9pm (Europe/Paris)",
	}
	for _, s := range notAuthy {
		if isAuthErrorResult(s) {
			t.Errorf("isAuthErrorResult(%q) = true, want false", s)
		}
	}
}

// The malformed-credential render: a secret that is not a token at all is
// quoted back inside an unbounded "API Error: Header …" line. Prefix-anchored
// so only a result that IS the error matches — an agent quoting one
// mid-answer must not.
func TestIsAuthErrorResult_malformedCredential(t *testing.T) {
	long := "API Error: Header '14' has invalid value: 'Bearer \x1b[?2004hWelcome to Claude Code v2.1.220\nWelcome to Claude Code v2.1.220\n\n · Opening browser to sign in'"
	if !isAuthErrorResult(long) {
		t.Fatal("the malformed-Authorization render must classify as an auth failure")
	}
	quoted := "The run failed because the log said: API Error: Header '14' has invalid value: 'Bearer x'. We should investigate the credential store."
	if isAuthErrorResult(quoted) {
		t.Fatal("prose quoting the error mid-answer must not classify")
	}
}
