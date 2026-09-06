package delegate

import (
	"bytes"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestRenderedFailure pins the one predicate every result message goes
// through — pass 1, the formatting passes, the recovery pass — and its
// order: a window notice is a rate-limit verdict (with evidence) before it
// is a transient 429; a credential verdict is never retried; a server error
// is transient; an answer is an answer.
func TestRenderedFailure(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
	task := Task{NodeID: "n", Iteration: 1, Model: "claude-fable-5"}
	str := func(s string) *string { return &s }

	t.Run("nil result is not a failure", func(t *testing.T) {
		if err := b.renderedFailure(nil, task, "pass 1"); err != nil {
			t.Fatalf("nil message: %v", err)
		}
		if err := b.renderedFailure(&claudesdk.ResultMessage{}, task, "pass 1"); err != nil {
			t.Fatalf("nil result: %v", err)
		}
	})
	t.Run("an answer is an answer", func(t *testing.T) {
		rm := &claudesdk.ResultMessage{Result: str("Here is the plan: three lots, one gate each.")}
		if err := b.renderedFailure(rm, task, "pass 1"); err != nil {
			t.Fatalf("answer re-typed: %v", err)
		}
	})
	t.Run("a facade's bracketed 500 is transient, on every pass", func(t *testing.T) {
		rm := &claudesdk.ResultMessage{Result: str("API Error: [500][Operation failed][2026090610430471b2ed5a5eaa4de7]")}
		for _, pass := range []string{"pass 1", "formatting pass 1/2", "recovery formatting pass"} {
			var tr *ErrTransient
			err := b.renderedFailure(rm, task, pass)
			if !errors.As(err, &tr) || tr.Reason != "api_error_result" {
				t.Fatalf("%s: want ErrTransient{api_error_result}, got %v", pass, err)
			}
		}
	})
	t.Run("a bracketed 429 naming a window is a rate-limit verdict, not a retry", func(t *testing.T) {
		rm := &claudesdk.ResultMessage{Result: str("API Error: [429][Usage limit reached for 5 hour. Your limit will reset at 3pm][abc]")}
		var rl *ErrRateLimited
		var tr *ErrTransient
		err := b.renderedFailure(rm, task, "pass 1")
		if errors.As(err, &tr) {
			t.Fatalf("window notice retried as transient: %v", err)
		}
		if !errors.As(err, &rl) {
			t.Fatalf("want ErrRateLimited, got %v", err)
		}
	})
	t.Run("a bare 429 with no window is transient", func(t *testing.T) {
		rm := &claudesdk.ResultMessage{Result: str("API Error: 429 Too Many Requests")}
		var tr *ErrTransient
		if err := b.renderedFailure(rm, task, "pass 1"); !errors.As(err, &tr) {
			t.Fatalf("want ErrTransient, got %v", err)
		}
	})
	t.Run("a bracketed 401 is a credential verdict, never retried", func(t *testing.T) {
		rm := &claudesdk.ResultMessage{Result: str("API Error: [401][Unauthorized][2026090610430471b2ed5a5eaa4de7]")}
		var auth *ErrAuthFailed
		var tr *ErrTransient
		err := b.renderedFailure(rm, task, "pass 1")
		if errors.As(err, &tr) {
			t.Fatalf("credential verdict retried: %v", err)
		}
		if !errors.As(err, &auth) {
			t.Fatalf("want ErrAuthFailed, got %v", err)
		}
	})
	t.Run("an answer is an answer, whatever its text says — the SDK object or JSON text", func(t *testing.T) {
		rm := &claudesdk.ResultMessage{
			Result:           str("quota exceeded"),
			StructuredOutput: map[string]any{"status": "quota exceeded", "ok": false},
		}
		if err := b.renderedFailure(rm, task, "formatting pass 1/2"); err != nil {
			t.Fatalf("structured answer re-typed: %v", err)
		}
		// The formatting pass may deliver its answer as JSON in the text
		// with an empty structured object: the same definition applies.
		rm = &claudesdk.ResultMessage{
			Result:           str(`{"status": "usage limit reached", "ok": false}`),
			StructuredOutput: map[string]any{},
		}
		if err := b.renderedFailure(rm, task, "formatting pass 1/2"); err != nil {
			t.Fatalf("JSON-text answer re-typed: %v", err)
		}
		rm = &claudesdk.ResultMessage{Result: str("quota exceeded"), StructuredOutput: map[string]any{}}
		if err := b.renderedFailure(rm, task, "formatting pass 1/2"); err == nil {
			t.Fatalf("a bare quota notice with an empty object is not an answer: want the quota verdict")
		}
	})
	t.Run("a 403 is a credential verdict, as the pi backend mints it — never retried", func(t *testing.T) {
		for _, text := range []string{"API Error: 403 Forbidden", "API Error: [403][Request blocked][abc]"} {
			rm := &claudesdk.ResultMessage{Result: str(text)}
			err := b.renderedFailure(rm, task, "pass 1")
			var auth *ErrAuthFailed
			if !errors.As(err, &auth) {
				t.Fatalf("%q: want ErrAuthFailed, got %v", text, err)
			}
			if renderRetryable(err) {
				t.Fatalf("%q: a credential verdict must not be retried", text)
			}
		}
	})
	t.Run("the transient class and a bare throttle are retried on a formatting attempt; verdicts are not", func(t *testing.T) {
		for _, err := range []error{
			&ErrTransient{Provider: BackendClaudeCode, Reason: "api_error_result"},
			&ErrRateLimited{Provider: BackendClaudeCode, Kind: RateLimitKindTransient},
		} {
			if !renderRetryable(err) {
				t.Fatalf("%v: retryable class treated as terminal", err)
			}
		}
		for _, err := range []error{
			&ErrRateLimited{Provider: BackendClaudeCode, Kind: RateLimitKindUsageWindow},
			&ErrAuthFailed{Provider: BackendClaudeCode},
			errors.New("claude-code: model unavailable"),
		} {
			if renderRetryable(err) {
				t.Fatalf("%v: terminal verdict retried", err)
			}
		}
	})
	t.Run("a model the CLI cannot use fails fast", func(t *testing.T) {
		rm := &claudesdk.ResultMessage{Result: str("There's an issue with the selected model (x/y). It may not exist or you may not have access to it.")}
		err := b.renderedFailure(rm, task, "pass 1")
		var tr *ErrTransient
		if err == nil || errors.As(err, &tr) {
			t.Fatalf("want a fast, non-transient failure, got %v", err)
		}
	})
}
