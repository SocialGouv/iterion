package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// Schema re-ask — the one more turn the executor spends when an LLM node's
// answer fails its output schema on a shape a second ask can fix (a missing
// required field, or text where JSON was expected). The re-ask CONTINUES the
// model's work rather than repeating it: the validation error is the model's
// next input, in the conversation it just finished. Which continuation a
// backend offers is what these modes name; `delegate_retry.reask` and the
// re-ask's own delegate events carry the one taken. One re-ask, never more:
// a second identical answer is the model's verdict, and the node fails on it.
const (
	// ReaskContinueConversation is claw's: the conversation the node just
	// completed (kept in the executor's session store) is replayed and the
	// validation error becomes its next user turn — one schema-forced call,
	// tools off, in-process. Every claw route gets it, sandboxed or not: the
	// store mirrors an in-container loop through the capture sink.
	ReaskContinueConversation = "continue_conversation"
	// ReaskResumeSession is the CLI backends' that resume by id
	// (claude_code, codex, pi): the session the first answer ran in is
	// resumed with the validation error as the new prompt, and the backend's
	// own structured-output pass runs on top of it.
	ReaskResumeSession = "resume_session"
	// ReaskRestart is what is left when nothing can be continued: a backend
	// that never resumes a session (kimi, grok), a session-capable backend
	// that reported no id, a claw node with no captured conversation. The
	// whole turn runs again with the validation error appended to the
	// prompt. Kept as the floor rather than refused: it is still a chance
	// the node would not otherwise get, and it is what every re-ask was
	// before the two continuation modes existed.
	ReaskRestart = "restart"
)

// schemaReask is a planned re-ask: the task to execute and the mode it runs
// under. why explains a restart (what could not be continued); empty on the
// two continuation modes.
type schemaReask struct {
	task delegate.Task
	mode string
	why  string
}

// label renders the mode for a log line, with the restart's reason.
func (r schemaReask) label() string {
	if r.why == "" {
		return r.mode
	}
	return r.mode + ", " + r.why
}

// planSchemaReask builds the re-ask for the first answer's task and result.
// The two continuation modes send ONLY the feedback as the new turn — the
// prompt is already in the conversation or the session — and never replay a
// pause: the paused conversation, if any, was resumed by the first answer
// and is part of what is continued now. A restart sends the prompt with the
// feedback appended, the task otherwise as it was. Nothing here executes.
func (e *ClawExecutor) planSchemaReask(ctx context.Context, backendName string, task *delegate.Task, result delegate.Result, feedback string) schemaReask {
	var why string
	switch {
	case backendName == delegate.BackendClaw:
		if conversation := e.completedClawConversation(ctx, task, result); len(conversation) > 0 {
			retryTask := *task
			retryTask.ContinueConversation = conversation
			retryTask.ResumeConversation = nil
			retryTask.ResumePendingToolUseID = ""
			retryTask.ResumeAnswer = ""
			retryTask.UserPrompt = feedback
			retryTask.UserContent = nil
			// Tools off: the model is asked for the answer it already has
			// the context to give, not for more work — and a tool-less turn
			// needs no sandbox, so the re-ask runs in-process on every route
			// (the backend refuses the pair, see Task.ContinueConversation).
			retryTask.HasTools = false
			retryTask.ToolDefs = nil
			retryTask.AllowedTools = nil
			retryTask.ToolMaxSteps = 0
			retryTask.Sandbox = nil
			return schemaReask{task: retryTask, mode: ReaskContinueConversation}
		}
		why = "no captured conversation for the node"
	case sessionResumeEligible(backendName):
		if sid := firstNonEmpty(result.SessionID, task.SessionID); sid != "" {
			retryTask := *task
			retryTask.SessionID = sid
			retryTask.ForkSession = false
			// Not best-effort: a re-ask whose session is gone has nothing
			// to continue, and a fresh session handed the feedback alone
			// would answer without the work it refers to. It fails loudly.
			retryTask.SessionOptional = false
			if result.SessionFingerprint != "" {
				retryTask.SessionFingerprint = result.SessionFingerprint
			}
			retryTask.ResumeConversation = nil
			retryTask.ResumePendingToolUseID = ""
			retryTask.ResumeAnswer = ""
			retryTask.UserPrompt = feedback
			retryTask.UserContent = nil
			return schemaReask{task: retryTask, mode: ReaskResumeSession}
		}
		why = "the backend reported no session id"
	default:
		why = fmt.Sprintf("backend %q does not resume a session", backendName)
	}

	// Restart: the turn again, the feedback appended to the prompt (and,
	// for a multimodal task, as an extra text block). The slice header is
	// copied so the original task's backing array stays untouched. On a
	// claw node resumed from a pause the resume form replays the pause and
	// discards the prompt, feedback included — the floor the continuation
	// modes exist to lift, reached only when no conversation was captured.
	restart := *task
	restart.UserPrompt = appendSchemaRetryFeedback(restart.UserPrompt, feedback)
	if len(restart.UserContent) > 0 {
		restart.UserContent = append(
			append([]delegate.ContentBlock(nil), restart.UserContent...),
			delegate.ContentBlock{Type: "text", Text: feedback},
		)
	}
	return schemaReask{task: restart, mode: ReaskRestart, why: why}
}

// completedClawConversation is the conversation the claw session store holds
// for the task — what the backend captured when the first answer's
// generation completed — sanitized into a provider-neutral replay and ended
// on the answer itself, or nil when nothing was captured (no runtime
// context, or an in-container runner without the capture sink). Looked up
// under the task's session key and, for a slot node, under the node id as
// well: the sandbox capture sink mirrors an in-container loop under the
// node id.
//
// The capture ends where the REQUEST ended: a structured call appends the
// assistant's answer to its conversation, a tool loop that closed on text
// returns the final text beside its messages, so the capture's last turn
// is the prompt or the last tool result. The re-ask is about that answer,
// so it is put back as the last assistant turn, from the output the backend
// parsed it into — the model must see what it answered to fix it.
func (e *ClawExecutor) completedClawConversation(ctx context.Context, task *delegate.Task, result delegate.Result) json.RawMessage {
	runID, sessions := runtimeContextFrom(ctx)
	if runID == "" || sessions == nil {
		return nil
	}
	messages := sessions.load(runID, taskSessionKey(*task))
	if len(messages) == 0 && task.SessionSlot != "" && task.NodeID != "" {
		messages = sessions.load(runID, task.NodeID)
	}
	if len(messages) == 0 {
		return nil
	}
	messages, _ = sanitizeToolPairs(messages, nil, true)
	if len(messages) == 0 {
		return nil
	}
	if messages[len(messages)-1].Role != "assistant" {
		if text := answeredText(result); text != "" {
			messages = append(messages, api.Message{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: text}}})
		}
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		e.logger.Warn("[%s] schema re-ask: encode captured conversation: %v", task.NodeID, err)
		return nil
	}
	return raw
}

// answeredText is the first answer as the model gave it: the raw text of a
// parse fallback, otherwise the JSON of the parsed output without the keys
// the executor stamped on it (`_tokens`, `_backend`, …), which the model
// never wrote. Empty when the answer carried nothing.
func answeredText(result delegate.Result) string {
	if result.ParseFallback {
		return strings.TrimSpace(fallbackText(result.Output))
	}
	answer := map[string]any{}
	for k, v := range result.Output {
		if !strings.HasPrefix(k, "_") {
			answer[k] = v
		}
	}
	if len(answer) == 0 {
		return ""
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return ""
	}
	return string(raw)
}

// reaskMarginalCost is what the re-ask ADDED to the node's bill — the figure
// its own delegate event carries. The runner's accumulator sums one cost per
// delegate_finished, and the first answer's event already carried that
// attempt's: on a backend whose figure is a session TOTAL (claude_code
// resuming the session it opened), the re-ask's `_cost_usd` already contains
// the first attempt's, so the marginal is the difference; everywhere else
// each call is priced on its own tokens and the re-ask's figure is its own.
// Tokens need no such care — every backend reports its own turn's.
func reaskMarginalCost(first, reask delegate.Result, sameSession bool) float64 {
	usd := cost.USDFromOutput(reask.Output)
	if sameSession && first.CostIsSessionTotal && reask.CostIsSessionTotal {
		usd -= cost.USDFromOutput(first.Output)
		if usd < 0 {
			usd = 0
		}
	}
	return usd
}

// emitReaskOutcome fires the re-ask's own delegate_finished / delegate_error,
// marked attempt 2 with its mode, priced at what it added. The delegation's
// own pair fired once, around the first answer, before validation ran; the
// re-ask is a further real turn — the model's work, tokens, cost — and a
// timeline that showed only its llm steps left its end unaccounted.
func (e *ClawExecutor) emitReaskOutcome(nodeID, backendName, declaredModel string, reask schemaReask, first, result delegate.Result, err error, sameSession bool) {
	di := delegateInfoFromResult(firstNonEmpty(result.BackendName, backendName), result)
	di.DeclaredModel = declaredModel
	di.Attempt = 2
	di.Reask = reask.mode
	di.CostUSD = reaskMarginalCost(first, result, sameSession)
	if err != nil {
		di.Error = err
		if e.hooks.OnDelegateError != nil {
			e.hooks.OnDelegateError(nodeID, di)
		}
		return
	}
	if e.hooks.OnDelegateFinished != nil {
		e.hooks.OnDelegateFinished(nodeID, di)
	}
}

// schemaReaskFailure is the error a node fails with when its re-ask did not
// deliver either: the original validation error stays the cause (the answer
// the node got was schema-invalid), and the re-ask's own failure travels
// beside it, wrapped — so a usage window hit during the re-ask is still the
// typed *ErrRateLimited the run-level retry keys on, and the report names
// what actually stopped the node instead of the symptom alone.
func schemaReaskFailure(subject string, validation error, reask schemaReask, reaskErr error) error {
	if reaskErr != nil {
		return fmt.Errorf("model: %s: structured output invalid: %w; the %s re-ask failed: %w", subject, validation, reask.mode, reaskErr)
	}
	return fmt.Errorf("model: %s: structured output invalid: %w; the %s re-ask returned unstructured text", subject, validation, reask.mode)
}
