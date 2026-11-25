package delegate

import (
	"errors"
	"testing"
)

// TestRenderedFailure_unflaggedRenderLeadIsNotEnough: an unflagged answer
// (is_error=false) that opens with the CLI's exact "API Error:" lead and
// goes on as prose is an answer — only a text that IS the render's whole
// grammar (one line, status, a JSON body) is classified past the cap.
func TestRenderedFailure_unflaggedRenderLeadIsNotEnough(t *testing.T) {
	answers := map[string]string{
		"quotes the render then diagnoses": "API Error: 401 {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"invalid x-api-key\"}} is the exact line the nightly job logged.\n\n" +
			"Root cause: the job reads ANTHROPIC_API_KEY from the old secret; the rotation on Tuesday only updated the new one. Fix: point the job at the rotated secret (PR #412) and add an alert on the 401 rate.",
		"reports a third-party 401": "API Error: 401 Bad credentials from api.github.com when pushing the release branch.\n\n" +
			"The GITHUB_TOKEN in the job's environment is a fine-grained token that expired on 2026-11-20; I could not open the pull request. " +
			"Regenerate the token with contents:write and pull_requests:write, update the secret, and re-run the release node.",
		"one paragraph explaining the status": "API Error: 401 means the gateway refused the bearer before routing: the token table has no row for the key the client sent, " +
			"so the handler answers authentication_error without touching the model. Rotating the client key and reloading the table fixes it; nothing changes in this PR.",
	}
	for name, s := range answers {
		if len(s) <= authProseCap {
			t.Fatalf("%s: fixture is %d bytes — it must exceed the prose cap", name, len(s))
		}
		err, readings := renderWithEvidence(t, s, false)
		if err != nil || len(readings) != 0 {
			t.Errorf("%s: err = %v, readings = %d, want the answer and no evidence", name, err, len(readings))
		}
	}

	// The long real single-line renders keep their verdict unflagged.
	for name, s := range longAuthRenders {
		if name == "status-less facade body" {
			continue // carries no status: classified only when flagged
		}
		err, _ := renderWithEvidence(t, s, false)
		var auth *ErrAuthFailed
		if !errors.As(err, &auth) {
			t.Errorf("%s: err = %v, want ErrAuthFailed", name, err)
		}
	}
}

// TestIsCLIRenderLine pins the grammar both ways.
func TestIsCLIRenderLine(t *testing.T) {
	for _, s := range []string{
		`API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
		`Failed to authenticate. API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired."}}`,
		`API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"}} · Please run /login`,
	} {
		if !isCLIRenderLine(s) {
			t.Errorf("isCLIRenderLine(%.60q) = false, want true", s)
		}
	}
	for _, s := range []string{
		`API Error: 401 {"type":"error"} is the line.`,
		"API Error: 401 {\"type\":\"error\"}\n\nRoot cause: …",
		`API Error: 401 Bad credentials from api.github.com`,
		`API Error: {"type":"error","error":{"type":"authentication_error"}}`,
		`API Error: 401 {"type":"error"`,
		`API Error: 401 {"error":"expired"} then {"error":"revoked"}`,
	} {
		if isCLIRenderLine(s) {
			t.Errorf("isCLIRenderLine(%.60q) = true, want false", s)
		}
	}
}
