package recovery_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
)

func r3Chain(errs []error) error {
	return fmt.Errorf("model: node %q: %w", "review", &model.ErrChainExhausted{Chain: "deepseek -> rescue", Errs: errs})
}

// r3Body is one real provider 400 (or near-400) body: the error.message the
// claw client extracts into APIError.Message, and the raw envelope it keeps
// in APIError.Body. Flattened = what the sandboxed claw runner hands the host
// ("claw backend: runner: " + the in-container error string, Message only).
type r3Body struct {
	name, kind, message, body string
	status                    int
}

var r3Bodies = []r3Body{
	{"deepseek effort (live, via LiteLLM)", "request-invalid",
		"litellm.BadRequestError: OpenAIException - reasoning_effort must be low, high, xhigh, max, or an integer within [1, 100]",
		`{"error":{"message":"litellm.BadRequestError: OpenAIException - reasoning_effort must be low, high, xhigh, max, or an integer within [1, 100]","type":null,"param":null,"code":"400"}}`, 400},
	{"openai max_tokens too large", "request-invalid",
		"max_tokens is too large: 50000. This model supports at most 16384 completion tokens, whereas you provided 50000.",
		`{"error":{"message":"max_tokens is too large: 50000. This model supports at most 16384 completion tokens, whereas you provided 50000.","type":"invalid_request_error","param":"max_tokens","code":"invalid_value"}}`, 400},
	{"openai chat context overflow", "context",
		"This model's maximum context length is 128000 tokens. However, your messages resulted in 130123 tokens. Please reduce the length of the messages.",
		`{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130123 tokens. Please reduce the length of the messages.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`, 400},
	{"openai responses context overflow", "context",
		"Your input exceeds the context window of this model. Please adjust your input and try again.",
		`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`, 400},
	{"groq/openai-compat 'Please reduce the length of the messages'", "context",
		"Please reduce the length of the messages or completion.",
		`{"error":{"message":"Please reduce the length of the messages or completion.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`, 400},
	{"anthropic prompt is too long", "context",
		"prompt is too long: 208310 tokens > 200000 maximum",
		`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 208310 tokens > 200000 maximum"}}`, 400},
	{"anthropic input + max_tokens > context limit", "context",
		"input length and `max_tokens` exceed context limit: 188240 + 21333 > 200000, decrease input length or `max_tokens` and try again",
		`{"type":"error","error":{"type":"invalid_request_error","message":"input length and ` + "`max_tokens`" + ` exceed context limit: 188240 + 21333 > 200000, decrease input length or ` + "`max_tokens`" + ` and try again"}}`, 400},
	{"litellm-wrapped anthropic overflow (PLIAGE shape)", "context",
		"litellm.ContextWindowExceededError: litellm.BadRequestError: AnthropicException - prompt is too long: 208310 tokens > 200000 maximum",
		`{"error":{"message":"litellm.ContextWindowExceededError: litellm.BadRequestError: AnthropicException - prompt is too long: 208310 tokens > 200000 maximum","type":null,"param":null,"code":"400"}}`, 400},
	{"bedrock input too long", "context",
		"Input is too long for requested model.",
		`{"message":"Input is too long for requested model."}`, 400},
	{"vertex/gemini input token count", "context",
		"The input token count (1210000) exceeds the maximum number of tokens allowed (1048576).",
		`{"error":{"code":400,"message":"The input token count (1210000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`, 400},
	{"llama.cpp exceed_context_size_error", "context",
		"the request exceeds the available context size, try increasing it",
		`{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":40000,"n_ctx":32768}}`, 400},
	{"vllm V1 max model length", "context",
		"The decoder prompt (length 270000) is longer than the maximum model length of 262144. Make sure that `max_model_len` is no smaller than the number of text tokens.",
		`{"object":"error","message":"The decoder prompt (length 270000) is longer than the maximum model length of 262144. Make sure that ` + "`max_model_len`" + ` is no smaller than the number of text tokens.","type":"BadRequestError","param":null,"code":400}`, 400},
	{"mistral prompt too large", "context",
		"Prompt contains 140000 tokens and 0 draft tokens, too large for model with 131072 maximum context length",
		`{"object":"error","message":"Prompt contains 140000 tokens and 0 draft tokens, too large for model with 131072 maximum context length","type":"invalid_request_invalid_args","param":null,"code":null}`, 400},
	{"anthropic image too large (tool screenshot)", "sample-dependent",
		"messages.4.content.1.image.source.base64: image exceeds 5 MB maximum: 6291456 bytes > 5242880 bytes",
		`{"type":"error","error":{"type":"invalid_request_error","message":"messages.4.content.1.image.source.base64: image exceeds 5 MB maximum: 6291456 bytes > 5242880 bytes"}}`, 400},
	{"anthropic tool_use without tool_result", "sample-dependent",
		"messages.2: `tool_use` ids were found without `tool_result` blocks immediately after: toolu_01ABC. Each `tool_use` block must have a corresponding `tool_result` block in the next message.",
		`{"type":"error","error":{"type":"invalid_request_error","message":"messages.2: ` + "`tool_use`" + ` ids were found without ` + "`tool_result`" + ` blocks immediately after: toolu_01ABC."}}`, 400},
	{"z.ai 1301 sensitive input-or-generated content", "sample-dependent",
		"系统检测到输入或生成内容可能包含不安全或敏感内容，请您避免输入易产生敏感内容的提示语，感谢您的配合。",
		`{"error":{"code":"1301","message":"系统检测到输入或生成内容可能包含不安全或敏感内容，请您避免输入易产生敏感内容的提示语，感谢您的配合。"}}`, 400},
	{"azure content_filter", "sample-dependent",
		"The response was filtered due to the prompt triggering Azure OpenAI's content management policy. Please modify your prompt and retry.",
		`{"error":{"message":"The response was filtered due to the prompt triggering Azure OpenAI's content management policy. Please modify your prompt and retry.","type":null,"param":"prompt","code":"content_filter","status":400}}`, 400},
	{"anthropic credit balance too low", "billing",
		"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits.",
		`{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`, 400},
	{"litellm (older) budget exceeded as 400", "billing",
		"Budget has been exceeded! Current cost: 10.52, Max budget: 10.0",
		`{"error":{"message":"Budget has been exceeded! Current cost: 10.52, Max budget: 10.0","type":"budget_exceeded","param":null,"code":"400"}}`, 400},
	{"litellm invalid model name", "model",
		"litellm.BadRequestError: Invalid model name passed in model=deepseek-v4.1-flahs. Call `/v1/models` to view available models for your key.",
		`{"error":{"message":"litellm.BadRequestError: Invalid model name passed in model=deepseek-v4.1-flahs. Call ` + "`/v1/models`" + ` to view available models for your key.","type":"invalid_request_error","param":"model","code":"400"}}`, 400},
}

func r3Action(err error) string {
	action, code := recovery.Dispatch(recovery.DefaultRecipes())(context.Background(), err, func(runtime.ErrorCode) int { return 0 })
	kind := map[runtime.RecoveryActionKind]string{runtime.RecoveryRetrySameNode: "RetrySameNode", runtime.RecoveryCompactAndRetry: "CompactAndRetry", runtime.RecoveryPauseForHuman: "PauseForHuman", runtime.RecoveryFailTerminal: "FailTerminal"}[action.Kind]
	return fmt.Sprintf("%s/%s", code, kind)
}

func TestR3G9ProviderBadRequestTable(t *testing.T) {
	for i, b := range r3Bodies {
		t.Run(b.name, func(t *testing.T) {
			typed := &api.APIError{Provider: "openai", StatusCode: b.status, Message: b.message, Body: b.body}
			want := runtime.ErrCodeExecutionFailed
			switch i {
			case 0, 1:
				want = runtime.ErrCodeProviderRequestInvalid
			case 2, 3, 4, 12:
				want = runtime.ErrCodeContextLengthExceeded
			}
			wantAction := string(want) + "/RetrySameNode"
			if want == runtime.ErrCodeProviderRequestInvalid {
				wantAction = string(want) + "/FailTerminal"
			}
			if want == runtime.ErrCodeContextLengthExceeded {
				wantAction = string(want) + "/CompactAndRetry"
			}
			if got := r3Action(fmt.Errorf("claw backend: structured generation: %w", typed)); got != wantAction {
				t.Errorf("typed=%s want=%s", got, wantAction)
			}
			// The parent treats flattened 400s as generic execution failures. Only a
			// typed IPC error can prove that this text came from a provider.
			flat := errors.New("claw backend: runner: " + typed.Error())
			if got := r3Action(flat); got != "EXECUTION_FAILED/RetrySameNode" {
				t.Errorf("flattened=%s want=EXECUTION_FAILED/RetrySameNode", got)
			}
		})
	}
}

func TestR3G9ChainRequiresEveryCause(t *testing.T) {
	bad := &api.APIError{Provider: "openai", StatusCode: 400, Message: "reasoning_effort must be low, high, xhigh, max, or an integer within [1, 100]"}
	down := &api.APIError{Provider: "openai", StatusCode: 503, Message: "Service Unavailable", Retryable: true}
	auth := &delegate.ErrAuthFailed{}
	reset := errors.New("dial tcp: connection refused")
	boom := errors.New("boom")
	typedBad := &runtime.RuntimeError{Code: runtime.ErrCodeProviderRequestInvalid, Cause: bad}
	nestedBad := &runtime.RuntimeError{Code: runtime.ErrCodeProviderRequestInvalid, Cause: r3Chain([]error{bad, bad})}
	for _, tc := range []struct {
		name   string
		causes []error
		want   string
	}{
		{"all invalid", []error{bad, bad}, "PROVIDER_REQUEST_INVALID/FailTerminal"},
		{"invalid then down", []error{bad, down}, "NETWORK_TRANSIENT/RetrySameNode"},
		{"down then invalid", []error{down, bad}, "NETWORK_TRANSIENT/RetrySameNode"},
		{"typed invalid then down", []error{typedBad, down}, "NETWORK_TRANSIENT/RetrySameNode"},
		{"nested invalid then down", []error{nestedBad, down}, "NETWORK_TRANSIENT/RetrySameNode"},
		{"invalid then generic", []error{bad, boom}, "EXECUTION_FAILED/RetrySameNode"},
		{"generic then invalid", []error{boom, bad}, "EXECUTION_FAILED/RetrySameNode"},
		{"invalid then auth", []error{bad, auth}, "AUTH_FAILED/PauseForHuman"},
		{"down then auth", []error{down, auth}, "AUTH_FAILED/PauseForHuman"},
		{"auth then down", []error{auth, down}, "AUTH_FAILED/PauseForHuman"},
		// Untyped causes retain the parent's last-message fallback, in either order.
		{"reset then generic", []error{reset, boom}, "EXECUTION_FAILED/RetrySameNode"},
		{"generic then reset", []error{boom, reset}, "NETWORK_TRANSIENT/RetrySameNode"},
		{"invalid reset generic", []error{bad, reset, boom}, "EXECUTION_FAILED/RetrySameNode"},
		{"generic reset invalid", []error{boom, reset, bad}, "NETWORK_TRANSIENT/RetrySameNode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := r3Chain(tc.causes)
			if got := r3Action(err); got != tc.want {
				t.Fatalf("chain=%s want=%s", got, tc.want)
			}
			if tc.want == "PROVIDER_REQUEST_INVALID/FailTerminal" {
				action, _ := recovery.Dispatch(recovery.DefaultRecipes())(context.Background(), err, nil)
				if !strings.Contains(action.Reason, bad.Message) {
					t.Errorf("wrong cause in remedy: %s", action.Reason)
				}
			}
		})
	}
	// An engine verdict enclosing the whole chain remains authoritative.
	enclosing := &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Cause: r3Chain([]error{bad, bad})}
	if got := r3Action(enclosing); got != "EXECUTION_FAILED/RetrySameNode" {
		t.Errorf("enclosing verdict=%s", got)
	}
}

func TestR3G9StreamPreservesParentFallback(t *testing.T) {
	for _, tc := range []struct{ message, want string }{
		{"unknown model deepseek-flahs", "MODEL_UNAVAILABLE/FailTerminal"},
		{"upstream: connection reset by peer", "NETWORK_TRANSIENT/RetrySameNode"},
		{"stream error: openai stream error: Background loop has errored already. (type=BadRequestError, code=400)", "EXECUTION_FAILED/RetrySameNode"},
		{"reasoning_effort must be high", "PROVIDER_REQUEST_INVALID/FailTerminal"},
		{"maximum context length exceeded", "CONTEXT_LENGTH_EXCEEDED/CompactAndRetry"},
	} {
		t.Run(tc.message, func(t *testing.T) {
			err := &model.APIError{StatusCode: 400, IsRetryable: true, Message: tc.message}
			if got := r3Action(err); got != tc.want {
				t.Fatalf("stream=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestR3G9ProviderClosedList(t *testing.T) {
	for _, msg := range []string{
		"DeepSeek V4.1 reasoning_effort must be low, high, xhigh, max, or an integer within [1, 100] in chat_template_kwargs",
		"Unsupported parameter: 'temperature' is not supported with this model.",
		"Unsupported value: 'temperature' does not support 0.5 with this model. Only the default (1) value is supported.",
		"max_tokens is too large: 50000. This model supports at most 16384 completion tokens.",
	} {
		err := &api.APIError{StatusCode: 400, Message: msg}
		if got := recovery.Classify(err); got != runtime.ErrCodeProviderRequestInvalid {
			t.Errorf("%q: %s", msg, got)
		}
		if remedy := runtime.ProviderRequestRemedy(fmt.Errorf("model prose status code 400: %w", err)); !strings.Contains(remedy, msg) || strings.Contains(remedy, "model prose") {
			t.Errorf("wrong remedy: %s", remedy)
		}
	}
	for _, msg := range []string{"bad request", "unsupported parameter", "unsupported value: messages[2].content", "messages[2]: unsupported parameter: 'temperature'", "reasoning_effort was discussed in the last message", "prompt is too long: 208310 tokens > 200000 maximum"} {
		if got := recovery.Classify(&api.APIError{StatusCode: 400, Message: msg}); got != runtime.ErrCodeExecutionFailed {
			t.Errorf("%q: %s", msg, got)
		}
	}
	err := &model.APIError{StatusCode: 400, IsRetryable: true, Message: "stream error: openai stream error: Background loop has errored already. (type=BadRequestError, code=400)"}
	if got := recovery.Classify(err); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("vLLM: %s", got)
	}
}

func TestR3G9EveryClosedParameter(t *testing.T) {
	parameters := []string{"reasoning_effort", "temperature", "top_p", "top_k", "max_tokens", "max_completion_tokens", "presence_penalty", "frequency_penalty", "seed", "stop", "logprobs", "top_logprobs", "n", "stream", "response_format"}
	for _, param := range parameters {
		for _, kind := range []string{"parameter", "value"} {
			msg := fmt.Sprintf("Unsupported %s: '%s' is not supported with this model.", kind, param)
			for _, status := range []int{400, 503} {
				err := &api.APIError{StatusCode: status, Message: msg}
				want := runtime.ErrCodeProviderRequestInvalid
				if status != 400 {
					want = runtime.ErrCodeNetworkTransient
				}
				if got := recovery.Classify(err); got != want {
					t.Errorf("%d %s: %s want %s", status, msg, got, want)
				}
			}
		}
	}
	msg := "max_completion_tokens is too large: 50000"
	if code := recovery.Classify(&api.APIError{StatusCode: 400, Message: msg}); code != runtime.ErrCodeProviderRequestInvalid {
		t.Errorf("%s: %s", msg, code)
	}
	// A wrapper around the chain may expose RuntimeErrors from its children;
	// errors.As must not let the first terminal child decide for them all.
	bad := &runtime.RuntimeError{Code: runtime.ErrCodeProviderRequestInvalid, Cause: &api.APIError{StatusCode: 400, Message: "reasoning_effort must be high"}}
	down := &api.APIError{StatusCode: 503, Message: "down", Retryable: true}
	if code := recovery.Classify(r3Chain([]error{bad, down})); code != runtime.ErrCodeNetworkTransient {
		t.Errorf("nested chain: %s", code)
	}
}
