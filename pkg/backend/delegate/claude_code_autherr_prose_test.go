package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// renderWithEvidence drives renderedFailure on one result and returns its
// error and the usage-cap readings it filed.
func renderWithEvidence(t *testing.T, text string, isError bool) ([]usagecap.Reading, error) {
	t.Helper()
	var readings []usagecap.Reading
	task := Task{}
	task.Hooks.OnUsageWindow = func(r usagecap.Reading) error { readings = append(readings, r); return nil }
	res := text
	rm := &claudesdk.ResultMessage{Subtype: claudesdk.ResultSuccess, IsError: isError, Result: &res}
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	err := b.renderedFailure(context.Background(), rm, task, "pass 1", credSpawn{})
	return readings, err
}

// TestRenderedFailure_proseOpeningWithAPIErrorIsAnAnswer: an answer
// (is_error=false) that opens with "API error…" in prose is not the CLI's
// "API Error:" render: it ships, and no credential is benched for it.
func TestRenderedFailure_proseOpeningWithAPIErrorIsAnAnswer(t *testing.T) {
	answers := map[string]string{
		"log triage": "API errors in the last 24h: 1,204 requests returned 401 with an authentication_error envelope, all from the mobile client v3.2 sending an invalid x-api-key header after the key rotation of Tuesday. " +
			"No server-side regression; the fix is client-side: ship the rotated key in v3.2.1 and add an alert on the 401 rate.",
		"status first": "API error 401 is what the gateway returns when the bearer is not in its token table; the dashboard shows 'invalid bearer token' for those. " +
			"The handler itself is correct: it maps the missing token to 401 and the expired one to 403, and logs the request id. Nothing to change in this PR.",
	}
	for name, s := range answers {
		if len(s) <= authProseCap {
			t.Fatalf("%s: fixture is %d bytes — it must exceed the prose cap", name, len(s))
		}
		readings, err := renderWithEvidence(t, s, false)
		if err != nil {
			t.Errorf("%s: answer refused: %v", name, err)
		}
		if len(readings) != 0 {
			t.Errorf("%s: evidence filed for an answer: %+v", name, readings)
		}
	}
}

// TestAuthFailureFast_evidenceNeedsTheRenderShape: a short unflagged text
// carrying a strong signature still fails the node, but only the CLI's own
// anchored render — or a result the CLI flagged is_error — benches the
// credential fleet-wide.
func TestAuthFailureFast_evidenceNeedsTheRenderShape(t *testing.T) {
	prose := "The deploy step stopped: the registry answered invalid bearer token."
	readings, err := renderWithEvidence(t, prose, false)
	var auth *ErrAuthFailed
	if !errors.As(err, &auth) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if len(readings) != 0 {
		t.Fatalf("readings = %+v, want none for unanchored prose", readings)
	}
	for name, c := range map[string]struct {
		text    string
		isError bool
	}{
		"anchored render": {`API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"}}`, false},
		"flagged render":  {prose, true},
	} {
		readings, err := renderWithEvidence(t, c.text, c.isError)
		if !errors.As(err, &auth) || len(readings) != 1 {
			t.Errorf("%s: err = %v, readings = %+v, want ErrAuthFailed and one reading", name, err, readings)
		}
	}
}

// TestRenderedFailure_serverStatusDecidesBeforeAuthWords: a 5xx the CLI
// renders is the transient class even when the relayed body names an auth
// error type: the status is the provider's verdict on the request.
func TestRenderedFailure_serverStatusDecidesBeforeAuthWords(t *testing.T) {
	for _, s := range []string{
		`API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded while validating the authentication_error retry path"},"request_id":"req_011CY8aQ2wErTy6UiOp4AsDf","detail":"upstream auth service unavailable, retry later"}`,
		`API Error: 500 {"type":"error","error":{"type":"api_error","message":"authentication_error lookup timed out"}}`,
	} {
		for _, isError := range []bool{false, true} {
			readings, err := renderWithEvidence(t, s, isError)
			var transient *ErrTransient
			if !errors.As(err, &transient) {
				t.Errorf("is_error=%v %.40q: err = %v, want ErrTransient", isError, s, err)
			}
			if len(readings) != 0 {
				t.Errorf("is_error=%v %.40q: evidence filed for a server error: %+v", isError, s, readings)
			}
		}
	}
}
