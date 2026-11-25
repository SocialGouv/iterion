package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// longAuthRenders are claude CLI auth-failure renders whose provider body,
// request id and remediation sentence push them past the 200-byte prose cap.
// Each is the result text of a subtype=success, is_error=true result message.
var longAuthRenders = map[string]string{
	"expired forfait, anthropic body":              `Failed to authenticate. API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired. Please obtain a new token or refresh your existing token."},"request_id":"req_011CVa9wXq3mRkT7pLzN2bHd"}`,
	"rejected key, anthropic body with login hint": `API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"},"request_id":"req_011CW4yHf8sD2kQ9vNmB6tXc"} · Please run /login · Check that ANTHROPIC_API_KEY holds a current key for this organization`,
	"facade proxy token refused":                   `API Error: 401 {"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = sk-...q7Xa, Key Hash (Token) =4c1f0e9a7b2d. Unable to find token in cache or LiteLLM_VerificationTokenTable","type":"auth_error","param":"None","code":"401"}}`,
	"status-less facade body":                      `API Error: {"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"},"request_id":"req_011CX2pLm9Qa4Rt6Yu8Io0PzAs","detail":"the bearer presented on this route was not issued by this gateway"}`,
}

// TestIsAuthErrorResult_lengthDoesNotHideARender: a render is classified on
// its shape and on the CLI's is_error flag, never on its length.
func TestIsAuthErrorResult_lengthDoesNotHideARender(t *testing.T) {
	for name, s := range longAuthRenders {
		if len(s) <= authProseCap {
			t.Fatalf("%s: fixture is %d bytes — it must exceed the prose cap to witness length independence", name, len(s))
		}
		if !isAuthErrorResult(s, false) {
			t.Errorf("%s: anchored render of %d bytes not classified as an auth failure", name, len(s))
		}
		if !isAuthErrorResult(s, true) {
			t.Errorf("%s: render flagged is_error not classified as an auth failure", name)
		}
	}

	if len(unanchoredAuthRender) <= authProseCap {
		t.Fatalf("fixture is %d bytes — it must exceed the prose cap", len(unanchoredAuthRender))
	}
	if !isAuthErrorResult(unanchoredAuthRender, true) {
		t.Error("an is_error render with an auth signature must classify whatever its length")
	}
}

// unanchoredAuthRender is a render the CLI flags is_error but that carries
// no anchored shape: the flag alone says the text is not an answer.
const unanchoredAuthRender = "Request failed after 10 attempts. Last error: OAuth token has expired. Please obtain a new token or refresh your existing token. " +
	"Run /login to sign in again, or export a fresh CLAUDE_CODE_OAUTH_TOKEN generated with `claude setup-token` before retrying."

// TestIsAuthErrorResult_longAnswerStaysAnAnswer: an answer (is_error=false)
// that discusses authentication — even one that opens with an auth phrase —
// is not an auth failure.
func TestIsAuthErrorResult_longAnswerStaysAnAnswer(t *testing.T) {
	answers := []string{
		"Failed to authenticate requests are the main cause of the 401 spike: the gateway logs show authentication_error bodies whenever a client sends an invalid api key, " +
			"and the dashboard's OAuth token has expired banner fires on the same requests. Rotate the key and add a probe.",
		"OAuth token has expired is what the mobile client prints when the refresh job lags; the fix is to refresh 5 minutes early, " +
			"surface 'Not logged in · Please run /login' only after the refresh fails, and log the request id so support can trace it.",
		"## Audit\n\nThe login handler rejects an invalid x-api-key with a 401 and an authentication_error envelope, which is correct. " +
			"The CLI wrapper, though, swallows the body and prints `Failed to authenticate. API Error: 401` with no request id; keep the id.",
	}
	for _, s := range answers {
		if len(s) <= authProseCap {
			t.Fatalf("fixture is %d bytes — it must exceed the prose cap", len(s))
		}
		if isAuthErrorResult(s, false) {
			t.Errorf("answer classified as an auth failure: %.80q", s)
		}
	}
}

// TestRenderedFailure_longAuthRenderIsTypedAuthFailure drives the public
// result path: a long render the CLI flags is_error surfaces as
// ErrAuthFailed — not the node's answer, not a transient retry — and a long
// answer flagged success ships.
func TestRenderedFailure_longAuthRenderIsTypedAuthFailure(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	renders := map[string]string{"unanchored is_error render": unanchoredAuthRender}
	for name, s := range longAuthRenders {
		renders[name] = s
	}
	for name, s := range renders {
		res := s
		rm := &claudesdk.ResultMessage{Subtype: claudesdk.ResultSuccess, IsError: true, Result: &res}
		err := b.renderedFailure(context.Background(), rm, Task{}, "pass 1", credSpawn{})
		var auth *ErrAuthFailed
		if !errors.As(err, &auth) {
			t.Errorf("%s: err = %v, want ErrAuthFailed", name, err)
			continue
		}
		var transient *ErrTransient
		if errors.As(err, &transient) {
			t.Errorf("%s: an auth failure must not be retried as transient", name)
		}
	}

	answer := "Failed to authenticate requests are the main cause of the 401 spike: the gateway logs show authentication_error bodies whenever a client sends an invalid api key, " +
		"and the dashboard's OAuth token has expired banner fires on the same requests. Rotate the key and add a probe."
	rm := &claudesdk.ResultMessage{Subtype: claudesdk.ResultSuccess, Result: &answer}
	if err := b.renderedFailure(context.Background(), rm, Task{}, "pass 1", credSpawn{}); err != nil {
		t.Fatalf("a long answer about authentication was refused: %v", err)
	}
}
