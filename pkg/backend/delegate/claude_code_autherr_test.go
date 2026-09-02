package delegate

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/usagecap"
)

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
	// The paid real-world shape is a whole CLI login transcript quoted
	// back as the header value — comfortably past the 200-byte prose cap,
	// which is exactly why the prefix branch sits ABOVE that cap. The
	// assertion on len pins the property the fixture exists for.
	long := "API Error: Header '14' has invalid value: 'Bearer \x1b[?2004h\x1b[?1004h\x1b[?2031hWelcome to Claude Code v2.1.220\nWelcome to Claude Code v2.1.220\n\n · Opening browser to sign in…\nPaste code here if prompted > \nWelcome to Claude Code v2.1.220\n · Opening browser to sign in…'"
	if len(long) <= 200 {
		t.Fatalf("fixture is %d bytes — it must exceed the 200-byte prose cap to guard the branch ordering", len(long))
	}
	if !isAuthErrorResult(long) {
		t.Fatal("the malformed-Authorization render must classify as an auth failure")
	}
	quoted := "The run failed because the log said: API Error: Header '14' has invalid value: 'Bearer x'. We should investigate the credential store."
	if isAuthErrorResult(quoted) {
		t.Fatal("prose quoting the error mid-answer must not classify")
	}
}

// The evidence write: an auth failure must leave a rejected WindowAuth
// reading behind (through the task hook, so the Source stamp applies)
// before the typed error surfaces — otherwise the next resolution
// re-picks the same dead credential.
func TestAuthFailureFast_recordsEvidence(t *testing.T) {
	var got []usagecap.Reading
	task := Task{}
	task.Hooks.OnUsageWindow = func(r usagecap.Reading) error { got = append(got, r); return nil }

	res := "Failed to authenticate. API Error: 401 Invalid bearer token"
	err := authFailureFast(&res, task)
	var auth *ErrAuthFailed
	if !errors.As(err, &auth) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if len(got) != 1 || got[0].Window != usagecap.WindowAuth || got[0].Status != usagecap.StatusRejected {
		t.Fatalf("readings = %+v, want one rejected WindowAuth reading", got)
	}

	ok := "All done, docs aligned."
	if authFailureFast(&ok, task) != nil || len(got) != 1 {
		t.Fatal("a normal result must produce neither error nor evidence")
	}
	if authFailureFast(nil, task) != nil {
		t.Fatal("a nil result must be a no-op")
	}
}

// R7cac91: the short-garbage half of the malformed-Authorization render —
// a 54-byte result must classify, not panic on a fixed-width slice.
func TestIsAuthErrorResult_shortMalformedCredential(t *testing.T) {
	for _, s := range []string{
		"API Error: Header '14' has invalid value: 'Bearer abc'",
		"API Error: Header '1' has invalid value: 'Bearer '",
	} {
		if !isAuthErrorResult(s) {
			t.Errorf("isAuthErrorResult(%q) = false, want true", s)
		}
	}
}

// Two thresholds, two blast radii: every auth render fails the node, but
// only the CLI's own high-confidence shapes write skip evidence — a terse
// agent answer containing "not logged in" must not bench a healthy
// credential fleet-wide for an hour.
func TestAuthFailureFast_looseSignatureFailsWithoutEvidence(t *testing.T) {
	var got []usagecap.Reading
	task := Task{}
	task.Hooks.OnUsageWindow = func(r usagecap.Reading) error { got = append(got, r); return nil }

	prose := "The deploy step failed: the gh CLI says you are not logged in."
	if authFailureFast(&prose, task) == nil {
		t.Fatal("the loose signature must still fail the node fast")
	}
	if len(got) != 0 {
		t.Fatalf("readings = %+v, want NONE — prose must not arm the credential skip", got)
	}

	// The CLI's own render of the same words, prefix-anchored, does.
	cli := "Not logged in · Please run /login"
	if authFailureFast(&cli, task) == nil {
		t.Fatal("the CLI render must fail the node")
	}
	if len(got) != 1 {
		t.Fatalf("readings = %+v, want one — the CLI's own render is high-confidence", got)
	}
}

// The rejected secret must never reach durable state: the Detail (run
// document, failure events) and the operator log both go through
// redactAuthRender, which drops everything after the identifying prefix.
func TestAuthFailureFast_redactsTheQuotedCredential(t *testing.T) {
	res := "API Error: Header '14' has invalid value: 'Bearer sk-ant-oat01-SECRETSECRETSECRET'"
	err := authFailureFast(&res, Task{})
	if err == nil {
		t.Fatal("want an auth failure")
	}
	if msg := err.Error(); strings.Contains(msg, "SECRETSECRET") || strings.Contains(msg, "Bearer sk-") {
		t.Fatalf("Detail leaks the credential: %q", msg)
	}
	if !strings.Contains(err.Error(), "has invalid value: <redacted>") {
		t.Fatalf("Detail should keep the identifying prefix: %q", err.Error())
	}
}
