package recovery

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

func TestRateLimitRecipe_RetriesThenPauses(t *testing.T) {
	r := RateLimitRecipe(2)
	for i := 0; i < 2; i++ {
		act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeRateLimited}, i)
		if act.Kind != ActionRetrySameNode {
			t.Fatalf("attempt %d: expected RetrySameNode, got %v", i, act.Kind)
		}
		if act.Delay <= 0 {
			t.Errorf("attempt %d: expected positive delay, got %v", i, act.Delay)
		}
	}
	act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeRateLimited}, 2)
	if act.Kind != ActionPauseForHuman {
		t.Errorf("after retries exhausted: expected PauseForHuman, got %v", act.Kind)
	}
}

func TestRateLimitRecipe_BackoffCapsBeforeOverflow(t *testing.T) {
	r := RateLimitRecipe(1000)
	act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeRateLimited}, 100)
	if act.Kind != ActionRetrySameNode {
		t.Fatalf("expected RetrySameNode, got %v", act.Kind)
	}
	if act.Delay < 16*time.Second || act.Delay > 32*time.Second {
		t.Fatalf("expected capped jitter delay in [16s, 32s], got %v", act.Delay)
	}
}

func TestContextLengthRecipe_CompactThenFail(t *testing.T) {
	r := ContextLengthRecipe()
	for i := 0; i < 2; i++ {
		act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeContextLengthExceeded}, i)
		if act.Kind != ActionCompactAndRetry {
			t.Fatalf("attempt %d: expected CompactAndRetry, got %v", i, act.Kind)
		}
	}
	act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeContextLengthExceeded}, 2)
	if act.Kind != ActionFailTerminal {
		t.Errorf("after compaction failed twice: expected FailTerminal, got %v", act.Kind)
	}
}

func TestBudgetRecipe_AlwaysPauses(t *testing.T) {
	r := BudgetRecipe()
	for i := 0; i < 5; i++ {
		act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeBudgetExceeded}, i)
		if act.Kind != ActionPauseForHuman {
			t.Fatalf("attempt %d: expected PauseForHuman, got %v", i, act.Kind)
		}
	}
}

func TestTransientToolRecipe(t *testing.T) {
	r := TransientToolRecipe(2)
	for i := 0; i < 2; i++ {
		act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeToolFailedTransient}, i)
		if act.Kind != ActionRetrySameNode {
			t.Fatalf("attempt %d: expected RetrySameNode, got %v", i, act.Kind)
		}
	}
	act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeToolFailedTransient}, 2)
	if act.Kind != ActionFailTerminal {
		t.Errorf("after retries exhausted: expected FailTerminal, got %v", act.Kind)
	}
}

func TestPermanentToolRecipe(t *testing.T) {
	r := PermanentToolRecipe()
	act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeToolFailedPermanent}, 0)
	if act.Kind != ActionFailTerminal {
		t.Errorf("expected FailTerminal, got %v", act.Kind)
	}
}

func TestExecutionFailedRecipe_RetryThenTerminal(t *testing.T) {
	// First failure: retry. Second: fail terminal (engine converts to
	// failed_resumable via failRunWithCheckpoint, so the operator can
	// /resume after fixing the cause).
	r := ExecutionFailedRecipe(1)
	act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed}, 0)
	if act.Kind != ActionRetrySameNode {
		t.Errorf("first failure: expected RetrySameNode, got %v", act.Kind)
	}
	if act.Delay <= 0 {
		t.Errorf("first failure: expected positive delay, got %v", act.Delay)
	}
	act = r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed}, 1)
	if act.Kind != ActionFailTerminal {
		t.Errorf("after one retry: expected FailTerminal, got %v", act.Kind)
	}
}

func TestExecutionFailedRecipe_WiredInDefaults(t *testing.T) {
	// Regression guard: without a recipe registered for
	// ErrCodeExecutionFailed, Dispatch falls through to FailTerminal
	// with reason "no recipe registered", and the first unclassified
	// node failure is unrecoverable.
	recipes := DefaultRecipes()
	if _, ok := recipes[runtime.ErrCodeExecutionFailed]; !ok {
		t.Fatal("DefaultRecipes must register a recipe for ErrCodeExecutionFailed so generic node failures get a retry before going terminal")
	}
}

func TestClassify_RuntimeError(t *testing.T) {
	rerr := &runtime.RuntimeError{Code: runtime.ErrCodeBudgetExceeded}
	if got := Classify(rerr); got != runtime.ErrCodeBudgetExceeded {
		t.Errorf("expected BUDGET_EXCEEDED, got %v", got)
	}
}

func TestClassify_RateLimit429(t *testing.T) {
	apiErr := &api.APIError{StatusCode: 429, Message: "Too many requests"}
	if got := Classify(apiErr); got != runtime.ErrCodeRateLimited {
		t.Errorf("expected RATE_LIMITED, got %v", got)
	}
}

func TestClassify_AuthFailed_APIError(t *testing.T) {
	for _, code := range []int{401, 403} {
		apiErr := &api.APIError{StatusCode: code, Message: "unauthorized"}
		if got := Classify(apiErr); got != runtime.ErrCodeAuthFailed {
			t.Errorf("status %d: expected AUTH_FAILED, got %v", code, got)
		}
	}
}

func TestClassify_AuthFailed_StringPatterns(t *testing.T) {
	// The claw runner flattens its *api.APIError to a string by the time
	// it reaches Classify, so the expired-token 401 must be caught by the
	// authFailedNeedles fallback, not the 1-shot ExecutionFailed retry.
	cases := []string{
		"claw backend: runner: claw backend: text+tools generation: openai: API error 401: Provided authentication token is expired. Please try signing in again.",
		"openai: incorrect API key provided",
		"anthropic: authentication_error: invalid x-api-key",
	}
	for _, msg := range cases {
		if got := Classify(errors.New(msg)); got != runtime.ErrCodeAuthFailed {
			t.Errorf("expected AUTH_FAILED for %q, got %v", msg, got)
		}
	}
}

func TestAuthFailedRecipe_PausesForHuman(t *testing.T) {
	r := AuthFailedRecipe()
	for _, attempts := range []int{0, 1, 5} {
		act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeAuthFailed}, attempts)
		if act.Kind != ActionPauseForHuman {
			t.Fatalf("attempts=%d: expected pause-for-human (never retry an expired credential), got %v", attempts, act.Kind)
		}
	}
	if _, ok := DefaultRecipes()[runtime.ErrCodeAuthFailed]; !ok {
		t.Fatal("DefaultRecipes must register a recipe for ErrCodeAuthFailed")
	}
}

func TestClassify_DelegateRateLimited(t *testing.T) {
	// Regression: CLI-backend rate-limit signal arrives as the typed
	// delegate.ErrRateLimited (raised by isRateLimitMessage). Before
	// this entry was added, Classify fell through to EXECUTION_FAILED,
	// triggering the 1-retry-then-resumable path instead of the
	// RateLimitRecipe's backoff+pause-for-human cascade. Operators saw
	// `failed_resumable` for a 5h ZAI quota wall — bad UX.
	err := &delegate.ErrRateLimited{
		Provider: "claude_code",
		Detail:   "API Error: Request rejected (429) · Usage limit reached for 5 hour. Your limit will reset at 2026-05-13 20:59:41",
	}
	if got := Classify(err); got != runtime.ErrCodeRateLimited {
		t.Errorf("direct: expected RATE_LIMITED, got %v", got)
	}
	// Same chain shape the executor produces: fmt.Errorf %w wrapping.
	wrapped := fmt.Errorf("model: node %q: backend %q failed: %w", "align_code", "claude_code",
		fmt.Errorf("delegate: claude-code failed: %w", err))
	if got := Classify(wrapped); got != runtime.ErrCodeRateLimited {
		t.Errorf("wrapped: expected RATE_LIMITED, got %v", got)
	}
}

func TestClassify_ContextLengthFromMessage(t *testing.T) {
	apiErr := &api.APIError{StatusCode: 400, Message: "context_length_exceeded: 200000 tokens"}
	if got := Classify(apiErr); got != runtime.ErrCodeContextLengthExceeded {
		t.Errorf("expected CONTEXT_LENGTH_EXCEEDED, got %v", got)
	}
}

func TestClassify_GenericAPIError(t *testing.T) {
	// 4xx (non-429, non-context-length) stays EXECUTION_FAILED — these
	// are usually deterministic request issues (auth, validation) that
	// won't fix themselves on retry.
	apiErr := &api.APIError{StatusCode: 400, Message: "bad request"}
	if got := Classify(apiErr); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("expected EXECUTION_FAILED, got %v", got)
	}
}

func TestClassify_APIError5xx_NetworkTransient(t *testing.T) {
	// Live observation: ChatGPT-codex backend returns 503 mid-stream
	// ("upstream connect error or disconnect/reset before headers")
	// during incidents; OpenAI / Anthropic return 502/504 on internal
	// rolling deploys. All of these are textbook transient — route to
	// NETWORK_TRANSIENT so the 6-retry exponential-backoff recipe
	// absorbs them instead of the catch-all 1-shot ExecutionFailedRecipe.
	cases := []int{500, 502, 503, 504, 408}
	for _, code := range cases {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			apiErr := &api.APIError{
				StatusCode: code,
				Message:    "upstream connect error or disconnect/reset before headers. reset reason: connection termination",
			}
			if got := Classify(apiErr); got != runtime.ErrCodeNetworkTransient {
				t.Errorf("expected NETWORK_TRANSIENT for status %d, got %v", code, got)
			}
		})
	}
}

func TestClassify_APIError409_StaysExecutionFailed(t *testing.T) {
	// 409 Conflict is usually a logical conflict (write-after-write,
	// concurrent edit) rather than a transient outage; retrying it
	// blindly wastes time. Keep it on the generic ExecutionFailed path.
	apiErr := &api.APIError{StatusCode: 409, Message: "conflict"}
	if got := Classify(apiErr); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("expected EXECUTION_FAILED for 409, got %v", got)
	}
}

// Measured 2026-09-07 on run 01a07db7: a node whose `json` field emits a
// JSON Schema type UNION, which the serving backend read into a single
// string. The schema rides the IR and the request is never built, so no
// sample and no wait existed to help — and yet the parse failure wore
// EXECUTION_FAILED and was redelivered five times for four pods.
func TestClassify_ADeclaredSchemaTheBackendCannotRead(t *testing.T) {
	// Verbatim from the run: the claw runner is out of process, so the
	// typed error below is text by the time it reaches the classifier.
	measured := errors.New(`model: node "voter_v2": backend "claw" failed: claw backend: runner: ` +
		`claw backend: structured generation: parse ExplicitSchema: json: cannot unmarshal array ` +
		`into Go struct field InputSchema.properties.verdicts.type of type string (exit: exit status 1)`)

	cases := []struct {
		name string
		err  error
		want runtime.ErrorCode
	}{
		{"the flattened string from run 01a07db7", measured, runtime.ErrCodeSchemaUnusable},
		{"typed, in process", &delegate.ErrSchemaUnusable{Schema: "voter_output", Detail: "cannot unmarshal array"},
			runtime.ErrCodeSchemaUnusable},
		{"typed, wrapped by its caller", fmt.Errorf("parse ExplicitSchema: %w",
			&delegate.ErrSchemaUnusable{Schema: "voter_output"}), runtime.ErrCodeSchemaUnusable},

		// The other half, and the distinction the whole code rests on: a
		// request WAS served and the model's OUTPUT missed the schema.
		// The next sample may conform, so this must stay re-executable.
		{"output that missed its schema is not an unusable schema", &runtime.RuntimeError{
			Code: runtime.ErrCodeSchemaValidation, Message: `field "verdicts" is required`,
		}, runtime.ErrCodeSchemaValidation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.err); got != c.want {
				t.Errorf("Classify() = %v, want %v", got, c.want)
			}
		})
	}
}

// No retry: the declaration rides the IR, so a second attempt builds the
// same request from the same schema for the same parser to refuse.
func TestSchemaUnusableRecipe_FailsTerminalWithoutRetrying(t *testing.T) {
	r := SchemaUnusableRecipe()
	for _, attempts := range []int{0, 1, 5} {
		act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeSchemaUnusable}, attempts)
		if act.Kind != ActionFailTerminal {
			t.Fatalf("attempts=%d: Kind = %v, want ActionFailTerminal", attempts, act.Kind)
		}
	}
	if _, ok := DefaultRecipes()[runtime.ErrCodeSchemaUnusable]; !ok {
		t.Error("no recipe registered for SCHEMA_UNUSABLE")
	}
}

// Measured 2026-09-07 on run 01a07da6: a claw node asked for a model the
// ChatGPT backend gates on the client release claw announces. The 400 wore
// EXECUTION_FAILED, whose disposition promises that a later attempt can
// outlast the fault — so the node failed, the delivery naked, the
// redelivery synthesised a resume, and the identical verdict came back
// NINE times in 77 seconds: eight pods, eight clones, eight sandboxes.
// Nothing in the request was at fault, so no sample and no wait could have
// helped; only an operator changing the model or the image.
func TestClassify_ModelTheProviderWillNotServe(t *testing.T) {
	// The flattened string is the shape that actually reached the
	// classifier: the claw runner is out of process, so its typed error is
	// text by the time it bubbles up — verbatim from the run.
	measured := errors.New(`model: node "m_astra": backend "claw" failed: claw backend: runner: ` +
		`claw backend: structured generation: openai: API error 400: {"detail":"The 'gpt-6-astra' ` +
		`model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again."}`)

	cases := []struct {
		name string
		err  error
		want runtime.ErrorCode
	}{
		{"the flattened string from run 01a07da6", measured, runtime.ErrCodeModelUnavailable},
		{"typed, gated on a client release", &api.APIError{
			StatusCode: 400, Message: "The 'gpt-6-astra' model requires a newer version of Codex.",
		}, runtime.ErrCodeModelUnavailable},
		{"typed 404: the id reached no endpoint", &api.APIError{
			StatusCode: 404, Message: "The model `gpt-9` does not exist or you do not have access to it.",
		}, runtime.ErrCodeModelUnavailable},
		{"flattened 404", errors.New("claw backend: openai: API error 404: model_not_found"),
			runtime.ErrCodeModelUnavailable},

		// The other half of the rule. A bare 400 can be raised mid-turn on
		// the model's OWN tool arguments, and those differ on the next
		// sample — reading it as a model verdict would park a run that
		// recovers on its own.
		{"a bare 400 is not a model verdict", &api.APIError{StatusCode: 400, Message: "bad request"},
			runtime.ErrCodeExecutionFailed},
		// A version floor that is NOT about a model: any tool can announce
		// one, and reading it as a model verdict would park a run over a
		// devbox package.
		{"a version floor that names no model", errors.New("devbox: package pinned: requires a newer version of nix"),
			runtime.ErrCodeExecutionFailed},
		// The credential is read first: a rejection naming both is one to
		// re-authenticate, not one to re-model.
		{"401 naming a model is still a credential", &api.APIError{
			StatusCode: 401, Message: "unknown model gpt-9: authentication token is expired",
		}, runtime.ErrCodeAuthFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.err); got != c.want {
				t.Errorf("Classify() = %v, want %v", got, c.want)
			}
		})
	}
}

// No retry at all: the provider answered about the model, so a second call
// from the same image is told the same thing — and every attempt above
// this recipe costs a pod.
func TestModelUnavailableRecipe_FailsTerminalWithoutRetrying(t *testing.T) {
	r := ModelUnavailableRecipe()
	for _, attempts := range []int{0, 1, 5} {
		act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeModelUnavailable}, attempts)
		if act.Kind != ActionFailTerminal {
			t.Fatalf("attempts=%d: Kind = %v, want ActionFailTerminal", attempts, act.Kind)
		}
		if act.AttemptsLeft != 0 {
			t.Errorf("attempts=%d: AttemptsLeft = %d, want 0", attempts, act.AttemptsLeft)
		}
	}
	if _, ok := DefaultRecipes()[runtime.ErrCodeModelUnavailable]; !ok {
		t.Error("no recipe registered for MODEL_UNAVAILABLE — Dispatch would fail terminal with a reason that names the gap instead of the cure")
	}
}

func TestClassify_PlainError(t *testing.T) {
	if got := Classify(errors.New("something")); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("expected EXECUTION_FAILED for plain error, got %v", got)
	}
}

func TestClassify_NetworkTransient(t *testing.T) {
	// Live observation: anthropic claude CLI emits the FailedToOpenSocket
	// phrase verbatim when the host loses connectivity mid-request. We
	// also cover the other lower-layer phrasings so iterion routes them
	// all through the longer NetworkTransientRecipe instead of the
	// catch-all 1-shot ExecutionFailedRecipe.
	cases := []string{
		`API Error: Unable to connect to API (FailedToOpenSocket)`,
		`UNABLE TO CONNECT TO API`, // case-insensitive
		`dial tcp: connection refused`,
		`read: connection reset by peer`,
		`dial tcp: lookup api.anthropic.com on 1.1.1.1:53: no such host`,
		`Post "https://api.anthropic.com/v1/messages": context deadline exceeded`,
		`tls handshake timeout`,
		`read tcp 10.0.0.1:443->10.0.0.2:443: i/o timeout`,
		`network is unreachable`,
		`no route to host`,
		`unexpected EOF`,
		// http/2 transport timeouts surface as untyped errors when no
		// HTTP response was received — golang.org/x/net/http2 prefixes
		// them with "http2:". Live observation: ChatGPT-codex backend
		// for openai/gpt-5.5 reasoning=high regularly hits this when the
		// queue is saturated.
		`Post "https://chatgpt.com/backend-api/codex/responses": http2: timeout awaiting response headers`,
		`http2: server sent GOAWAY and closed the connection`,
		// envoy-style upstream errors surface in body text on 5xx; the
		// 5xx already routes via the APIError branch, but the raw string
		// can also appear when the connection drops before headers, so
		// keep a fallback needle.
		`Post "https://api.example.com/v1/x": upstream connect error or disconnect/reset before headers`,
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			if got := Classify(errors.New(msg)); got != runtime.ErrCodeNetworkTransient {
				t.Errorf("expected NETWORK_TRANSIENT for %q, got %v", msg, got)
			}
		})
	}
}

func TestNetworkTransientRecipe_BackoffShape(t *testing.T) {
	// Default-cap recipe should retry 6 times with exponential backoff
	// up to 60s. Past the cap → FailTerminal.
	r := NetworkTransientRecipe(0)
	cases := []struct {
		attempts  int
		wantKind  ActionKind
		wantDelay time.Duration
	}{
		{0, ActionRetrySameNode, 5 * time.Second},
		{1, ActionRetrySameNode, 10 * time.Second},
		{2, ActionRetrySameNode, 20 * time.Second},
		{3, ActionRetrySameNode, 40 * time.Second},
		{4, ActionRetrySameNode, 60 * time.Second}, // capped
		{5, ActionRetrySameNode, 60 * time.Second}, // capped
		{6, ActionFailTerminal, 0},
		{99, ActionFailTerminal, 0},
	}
	for _, tc := range cases {
		got := r.Apply(context.Background(), nil, tc.attempts)
		if got.Kind != tc.wantKind {
			t.Errorf("attempts=%d: kind=%v want %v", tc.attempts, got.Kind, tc.wantKind)
		}
		if got.Kind == ActionRetrySameNode && got.Delay != tc.wantDelay {
			t.Errorf("attempts=%d: delay=%v want %v", tc.attempts, got.Delay, tc.wantDelay)
		}
	}
}

func TestClassify_Nil(t *testing.T) {
	if got := Classify(nil); got != "" {
		t.Errorf("expected empty for nil err, got %v", got)
	}
}

func TestDefaultRecipes_HasAllClasses(t *testing.T) {
	r := DefaultRecipes()
	want := []runtime.ErrorCode{
		runtime.ErrCodeRateLimited,
		runtime.ErrCodeContextLengthExceeded,
		runtime.ErrCodeBudgetExceeded,
		runtime.ErrCodeToolFailedTransient,
		runtime.ErrCodeToolFailedPermanent,
		runtime.ErrCodeNetworkTransient,
	}
	for _, code := range want {
		if _, ok := r[code]; !ok {
			t.Errorf("DefaultRecipes missing %q", code)
		}
	}
}
