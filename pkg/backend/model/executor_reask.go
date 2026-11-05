package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// Schema re-asks continue the completed work with validation feedback. The
// runtime's durable output-correction allowance bounds every additional call.
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
	// ReaskExtract marks the last-resort structured-output recovery
	// (extractStructuredViaClaw): not a turn of the node's model, but one
	// direct claw call on the model host detection picked, which converts
	// the node's text into the schema.
	ReaskExtract = "extract"
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

	// Restart with feedback when no completed conversation can be read.
	// A pending pause is already answered: replaying it would discard the
	// new prompt and silently lose the validation error.
	restart := *task
	restart.ResumeConversation = nil
	restart.ResumePendingToolUseID = ""
	restart.ResumeAnswer = ""
	priorInteraction := ""
	if task.ResumeAnswer != "" {
		priorInteraction = "\n\n[PRIOR INTERACTION]\nOperator answer: " + task.ResumeAnswer
		restart.UserPrompt += priorInteraction
	}
	restart.UserPrompt = appendSchemaRetryFeedback(restart.UserPrompt, feedback)
	if len(restart.UserContent) > 0 {
		restart.UserContent = append(
			append([]delegate.ContentBlock(nil), restart.UserContent...),
			delegate.ContentBlock{Type: "text", Text: priorInteraction + "\n\n" + feedback},
		)
	}
	return schemaReask{task: restart, mode: ReaskRestart, why: why}
}

// completedClawConversation is the conversation the claw session store holds
// for the task — what the backend captured when the first answer's
// generation completed — sanitized into a provider-neutral replay, or nil
// when nothing was captured (no runtime context, or an in-container runner
// without the capture sink).
//
// The task's session key is the ONE address, on either side of the sandbox
// boundary: an in-process capture writes it and a sandboxed crossing's
// capture is mirrored under it, so no other entry is consulted. What a
// non-empty entry holds is the SESSION's conversation, which for a shared
// `session_slot:` is this crossing's once its own capture landed and the
// slot's earlier turns when it did not — the mirror is best-effort by
// contract, and a crossing the channel could not seed deliberately keeps its
// own turns out of the slot. Continuing the slot's earlier turns is then
// continuing a SIBLING's conversation: the answer under correction is absent
// from it, and the trailing-assistant guard below does not catch the case,
// because a sibling's entry ends on the sibling's own assistant turn. Telling
// the two apart needs an identity on the entry — which crossing wrote it —
// not a comparison of its text.
//
// The capture ends where the REQUEST ended: a tool loop that closed on text
// returns the final text beside its messages, so the capture's last turn is
// the prompt or the last tool result, and the answer the re-ask is about is
// put back as the last assistant turn from the output the backend parsed it
// into — the model must see what it answered to fix it. A capture that
// ALREADY ends on an assistant turn gets nothing appended: a structured
// call's own answer is that turn, and two consecutive assistant turns is
// not a conversation a provider accepts.
func (e *ClawExecutor) completedClawConversation(ctx context.Context, task *delegate.Task, result delegate.Result) json.RawMessage {
	runID, sessions := runtimeContextFrom(ctx)
	var messages []api.Message
	if runID != "" && sessions != nil {
		messages = sessions.load(runID, taskSessionKey(*task))
	}
	if len(messages) == 0 && len(task.ResumeConversation) > 0 && task.ResumePendingToolUseID != "" {
		if err := json.Unmarshal(task.ResumeConversation, &messages); err != nil {
			e.logger.Warn("[%s] schema re-ask: decode answered pause: %v", task.NodeID, err)
			return nil
		}
		messages = append(messages, api.Message{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: task.ResumePendingToolUseID, Content: task.ResumeAnswer}.ToContentBlock()}})
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
	usd := reask.CostUSD
	if sameSession && first.CostIsSessionTotal && reask.CostIsSessionTotal {
		usd -= first.CostUSD
		if usd < 0 {
			usd = 0
		}
	}
	return usd
}

// emitExtractionOutcome books the structured-output recovery's own call: a
// direct claw generation that no delegate or step event carries otherwise,
// so without it the run's meter never sees that spend while the node's
// figure includes it. The event is named after the call itself — claw, the
// detected model and its provider, and the credential keys of the claw arm's
// Spend its client was built from (spent.Credential) — and marked
// `reask: "extract"`; it fires on every outcome that spent (a recovery that
// failed after the model answered was billed like one that succeeded).
func (e *ClawExecutor) emitExtractionOutcome(nodeID, declaredModel string, spent delegate.Result, err error) {
	if spent.Tokens == 0 && spent.CostUSD == 0 && spent.UnreportedCalls == 0 {
		return
	}
	di := delegateInfoFromResult(delegate.BackendClaw, spent)
	di.DeclaredModel = declaredModel
	di.RouteModel = spent.EffectiveModel
	di.Provider = providerOfModelSpec(spent.EffectiveModel)
	di.Reask = ReaskExtract
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

// emitReaskOutcome fires the re-ask's own delegate_finished / delegate_error,
// marked with its attempt and mode, priced at what it added. The delegation's
// own pair fired once, around the first answer, before validation ran; the
// re-ask is a further real turn — the model's work, tokens, cost — and a
// timeline that showed only its llm steps left its end unaccounted.
//
// The re-ask runs on the element that served the first answer, so it names
// that element's provider the way the first answer's event did
// (servingProvider over the same backend and the task copy): the runner's
// credential-plan join reads that word first (#2288), and a re-derivation
// from the model spec names nothing for a bare id (codex's `gpt-*` under
// `provider: openai`) and the prefix's provider over a hint-steered route
// (pi's `anthropic/…` under `provider: openrouter`).
func (e *ClawExecutor) emitReaskOutcome(nodeID, backendName, declaredModel string, reask schemaReask, result delegate.Result, err error, marginalCost float64, attempt int) {
	di := delegateInfoFromResult(firstNonEmpty(result.BackendName, backendName), result)
	di.DeclaredModel = declaredModel
	di.RouteModel = reask.task.Model
	di.Provider = servingProvider(backendName, &reask.task)
	di.Attempt = attempt
	di.Reask = reask.mode
	di.CostUSD = marginalCost
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
