package claudesdk

import (
	"context"
	"encoding/json"
	"fmt"
)

// hookChannel is the ONE place a spawn registers its hooks and answers their
// callbacks. Both spawn paths hold one: the streaming Session and the one-shot
// Prompt.
//
// It exists as a type rather than as two copies of the same code because the
// bound a hook carries follows the CALL SITE, not the task: a spawn path that
// forgets to open the channel is a spawn with no gate, and nothing about the
// task says so. Prompt was exactly that path — configToProcess dropped
// cfg.hooks and handleControlRequestOneShot answered anything but can_use_tool
// with an error, so a claude_code node with an output_schema ran its second
// spawn, on the transcript the first one's untrusted content is in, with the
// PreToolUse gate absent.
type hookChannel struct {
	// callbacks maps the callback ids sent in the initialize request to the
	// handlers they dispatch to.
	callbacks map[string]HookCallback
}

func newHookChannel() *hookChannel {
	return &hookChannel{callbacks: make(map[string]HookCallback)}
}

// initializeBody turns a config's hooks into the CLI's initialize payload,
// recording which handler each generated callback id dispatches to. Callback
// ids are derived from the event and the matcher's index, so the same
// configuration always produces the same request.
func (h *hookChannel) initializeBody(hooks map[HookEvent][]HookMatcher) map[string]any {
	registered := make(map[string][]hookMatcherConfig, len(hooks))
	for event, matchers := range hooks {
		var configs []hookMatcherConfig
		for i, m := range matchers {
			callbackID := fmt.Sprintf("hook_%s_%d", event, i)
			h.callbacks[callbackID] = m.Handler
			configs = append(configs, hookMatcherConfig{
				Matcher:         m.Matcher,
				HookCallbackIDs: []string{callbackID},
				Timeout:         m.Timeout,
			})
		}
		registered[string(event)] = configs
	}
	return map[string]any{"hooks": registered}
}

// sendInitialize writes the initialize control request carrying the hooks and
// returns the request id it was sent under. It must reach stdin BEFORE the
// first user message — FIFO ordering is what makes the CLI register the hooks
// before it processes the prompt — and it does not wait for the response,
// because on both paths the response can only be read by the loop that has not
// started yet. The id is how a caller later tells a registration the engine
// ACCEPTED from one it refused.
func (h *hookChannel) sendInitialize(ctrl *controller, hooks map[HookEvent][]HookMatcher) (string, error) {
	return ctrl.sendRequestNoWaitWithID("initialize", h.initializeBody(hooks))
}

// handleCallback answers one hook_callback control request: it resolves the
// callback id to its handler, runs it, and writes the handler's verdict back in
// the shape the CLI validates.
func (h *hookChannel) handleCallback(ctx context.Context, cfg *config, ctrl *controller, req controlRequest) {
	var body struct {
		CallbackID string         `json:"callback_id"`
		Input      map[string]any `json:"input"`
		ToolUseID  *string        `json:"tool_use_id,omitempty"`
	}
	if err := json.Unmarshal(req.Request, &body); err != nil {
		if sendErr := ctrl.sendErrorResponse(req.RequestID, "parse error: "+err.Error()); sendErr != nil {
			cfg.errorf("claudesdk: failed to deliver hook parse-error response (request_id=%s): %v", req.RequestID, sendErr)
		}
		return
	}

	handler, ok := h.callbacks[body.CallbackID]
	if !ok {
		// An unknown callback_id signals a hook-registration bug — worth a
		// log line even when the error response is delivered fine.
		cfg.errorf("claudesdk: hook callback for unknown callback_id %s", body.CallbackID)
		if sendErr := ctrl.sendErrorResponse(req.RequestID, "unknown callback: "+body.CallbackID); sendErr != nil {
			cfg.errorf("claudesdk: failed to deliver unknown-callback error response (request_id=%s, callback_id=%s): %v", req.RequestID, body.CallbackID, sendErr)
		}
		return
	}

	inputJSON, _ := json.Marshal(body.Input)
	var input HookCallbackInput
	_ = json.Unmarshal(inputJSON, &input)
	input.ToolUseID = body.ToolUseID

	output, err := handler(ctx, input)
	if err != nil {
		if sendErr := ctrl.sendErrorResponse(req.RequestID, err.Error()); sendErr != nil {
			cfg.errorf("claudesdk: failed to deliver hook error response (request_id=%s, callback_id=%s, cause=%v): %v", req.RequestID, body.CallbackID, err, sendErr)
		}
		return
	}

	// Build hook-specific output. We only emit hookSpecificOutput when
	// at least one variant-discriminating field is populated — claude-code
	// CLI Zod-validates the inner object against a discriminated union
	// keyed on hookEventName, and a bare {hookEventName: X} (no other
	// fields) matches no variant and produces an "Invalid input" stderr
	// spam on every tool call from no-op hooks (e.g. our PostToolUse
	// inbox drainer that returns HookOutput{} when no operator messages
	// are queued).
	hookSpecific := hookSpecificFields(output)

	resp := map[string]any{}
	if len(hookSpecific) > 0 {
		hookSpecific["hookEventName"] = input.HookEventName
		resp["hookSpecificOutput"] = hookSpecific
	}
	if output.SystemMessage != "" {
		resp["systemMessage"] = output.SystemMessage
	}
	if output.Continue != nil {
		resp["continue"] = *output.Continue
	}
	// Stop-hook blocking: the Claude Code hook protocol blocks a stop with a
	// top-level {"decision":"block","reason":...}, distinct from PreToolUse's
	// permissionDecision (carried in hookSpecificOutput above). Without this the
	// Stop-hook's BlockStop/Reason were dropped, so our operator-message drainer
	// (claude_code.go HookStop) could never actually hold the session open to
	// inject a queued message — it was silently ineffective.
	if output.BlockStop {
		resp["decision"] = "block"
		if output.Reason != "" {
			resp["reason"] = output.Reason
		}
	}

	// A lost success response can drop a Stop-hook block decision — the
	// CLI would end the turn a hook meant to hold open.
	if sendErr := ctrl.sendResponse(req.RequestID, "success", resp); sendErr != nil {
		cfg.errorf("claudesdk: failed to deliver hook response (request_id=%s, callback_id=%s): %v", req.RequestID, body.CallbackID, sendErr)
	}
}
