package recovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// clawOpenAIError mirrors claw's openai provider: Message is error.message
// from the JSON envelope (extractOpenAIErrorMessage), Body keeps the raw body.
func clawOpenAIError(status int, body string) *api.APIError {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := body
	if err := json.Unmarshal([]byte(body), &parsed); err == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}
	return &api.APIError{Provider: "openai", StatusCode: status, Message: msg, Body: body}
}

func TestProbeG7RealisticBodies(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	type tc struct {
		name   string
		status int
		body   string
		want   runtime.ErrorCode
	}
	cases := []tc{
		// LiteLLM main >= 2026-09-16 (model_access_denied.py, fixed client message, 403)
		{"litellm-2026-09 key denial", 403, `{"error":{"message":"The requested model 'deepseek-v4-flash-0731' is not available for this API key, or the model name is invalid. Check the models available to you and try again.","type":"key_model_access_denied","param":"model","code":"403"}}`, runtime.ErrCodeModelUnavailable},
		{"litellm-2026-09 team denial", 403, `{"error":{"message":"The requested model 'deepseek-v4-flash-0731' is not available for this API key, or the model name is invalid. Check the models available to you and try again.","type":"team_model_access_denied","param":"model","code":"403"}}`, runtime.ErrCodeModelUnavailable},
		// LiteLLM 2026 before 09-15: 403 + internal message (no template configured)
		{"litellm-403 internal key message", 403, `{"error":{"message":"key not allowed to access model. This key can only access models=['gpt-oss-120b']. Tried to access deepseek-v4-flash-0731","type":"key_model_access_denied","param":"model","code":"403"}}`, runtime.ErrCodeModelUnavailable},
		{"litellm-403 team member message", 403, `{"error":{"message":"Team member not allowed to access model. User=u1, Team=t1, Model=deepseek-v4-flash-0731. Allowed member models = ['gpt-oss-120b']","type":"team_model_access_denied","param":"model","code":"403"}}`, runtime.ErrCodeModelUnavailable},
		// LiteLLM <= v1.77.7: 401 + same internal message
		{"litellm-v1.77 401 key denial", 401, `{"error":{"message":"key not allowed to access model. This key can only access models=['gpt-oss-120b']. Tried to access deepseek-v4-flash-0731","type":"key_model_access_denied","param":"model","code":"401"}}`, runtime.ErrCodeModelUnavailable},
		// OpenAI: project lacks the model (403, code model_not_found lives in the code field)
		{"openai 403 project lacks model", 403, `{"error":{"message":"Project ` + "`proj_abc`" + ` does not have access to model ` + "`gpt-5`" + `","type":"invalid_request_error","param":null,"code":"model_not_found"}}`, runtime.ErrCodeModelUnavailable},
		// Real credential failures that MUST keep the auth remedy
		{"litellm 401 invalid key", 401, `{"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = sk-...abcd, Key Hash (Token) =deadbeef. Unable to find token in cache or LiteLLM_VerificationTokenTable","type":"auth_error","param":"None","code":"401"}}`, runtime.ErrCodeAuthFailed},
		{"anthropic 403 permission_error", 403, `{"type":"error","error":{"type":"permission_error","message":"Your API key does not have permission to use the specified resource."}}`, runtime.ErrCodeAuthFailed},
		{"openai 401 invalid key", 401, `{"error":{"message":"Incorrect API key provided: sk-abc. You can find your API key at https://platform.openai.com/account/api-keys.","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`, runtime.ErrCodeAuthFailed},
		// The commit's own synthetic wording
		{"commit synthetic 403", 403, `{"error":{"message":"model not available for this credential"}}`, runtime.ErrCodeModelUnavailable},
		// Over-reach probe: a 403 that is a credential problem but mentions model availability
		{"403 disabled key mentioning model not available", 403, `{"error":{"message":"API key disabled by administrator: model not available until the key is re-enabled"}}`, runtime.ErrCodeAuthFailed},
	}
	for _, c := range cases {
		typed := clawOpenAIError(c.status, c.body)
		flat := errors.New("claw backend: " + typed.Error())
		for _, v := range []struct {
			form string
			err  error
		}{{"typed", typed}, {"flattened", flat}} {
			action, code := Dispatch(DefaultRecipes())(t.Context(), v.err, func(runtime.ErrorCode) int { return 0 })
			verdict := "OK  "
			if code != c.want {
				verdict = "MISS"
				t.Errorf("%s/%s: got %s, want %s", c.name, v.form, code, c.want)
			}
			fmt.Printf("%s %-48s %-9s status=%d got=%-22s want=%-22s action=%v\n", verdict, c.name, v.form, c.status, code, c.want, action.Kind)
		}
	}
	// pi and claude_code type a 401/403 as ErrAuthFailed before Classify reads any body.
	piErr := &delegate.ErrAuthFailed{Provider: delegate.BackendPi, Detail: "upstream 403: The requested model 'deepseek-v4-flash-0731' is not available for this API key, or the model name is invalid."}
	_, code := Dispatch(DefaultRecipes())(t.Context(), piErr, func(runtime.ErrorCode) int { return 0 })
	fmt.Printf("pi typed ErrAuthFailed(403 model denial) -> %s\n", code)
}

func TestProbeG7TypedModelUnavailable(t *testing.T) {
	for _, e := range []error{
		&delegate.ErrModelUnavailable{Provider: "claude_code", Model: "openai/gpt-5.5", Detail: "There's an issue with the selected model (openai/gpt-5.5). It may not exist or you may not have access to it."},
		&delegate.ErrModelUnavailable{Provider: "zai", Model: "claude-opus-5", Detail: "facade would substitute GLM under the requested label"},
		fmt.Errorf("node \"n\": %w", &delegate.ErrModelUnavailable{Provider: "openai_compatible", Model: "deepseek-v4-flash-0731"}),
	} {
		action, code := Dispatch(DefaultRecipes())(t.Context(), e, func(runtime.ErrorCode) int { return 0 })
		if code != runtime.ErrCodeModelUnavailable {
			t.Errorf("typed model denial got %s: %v", code, e)
		}
		fmt.Printf("TYPED %-60.60q -> code=%s action=%v reason=%.60q\n", e.Error(), code, action.Kind, action.Reason)
	}
}

func TestModelAccessDenialUsesAPIErrorBody(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, status := range []int{401, 403} {
		err := &api.APIError{Provider: "openai", StatusCode: status, Message: "Access denied", Body: `{"error":{"type":"team_model_access_denied"}}`}
		if got := Classify(err); got != runtime.ErrCodeModelUnavailable {
			t.Fatalf("status=%d body denial classified %s", status, got)
		}
		err.Body = `{"error":{"type":"team_model_access_denied","message":"invalid API key"}}`
		if got := Classify(err); got != runtime.ErrCodeAuthFailed {
			t.Fatalf("credential failure in body classified %s", got)
		}
	}
}
