package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
	"github.com/SocialGouv/claw-code-go/pkg/apikit"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
)

// ---------------------------------------------------------------------------
// Core generation: text
// ---------------------------------------------------------------------------

// GenerateTextDirect generates text using api.APIClient.StreamResponse directly.
// It runs a tool loop: call model → execute tools → append results → repeat,
// up to MaxSteps iterations.
// guardNonEmptyConversation rejects a request that carries no message at all,
// turning the provider's opaque "messages: at least one message is required"
// 400 into a clear, actionable iterion error. The system prompt lives in
// opts.System (separate from Messages), so this fires exactly for the
// degenerate node — an entry agent/judge with an empty `user:` prompt and no
// input — never when any prior/session/tool message exists (then Messages is
// non-empty).
func guardNonEmptyConversation(messages []api.Message) error {
	if len(messages) == 0 {
		return fmt.Errorf("this node produced no message to send: give the agent/judge a `user:` prompt, or feed it input from an upstream node or workflow input — a `system:` prompt alone is not a conversation turn")
	}
	return nil
}

// fireUserPromptSubmit fires the UserPromptSubmit lifecycle event once,
// before a generation call's first model request — claude_code's
// prompt-submission parity point. The payload is the text of the last
// user-role message. A Block decision refuses the generation (the node
// fails with the hook's reason, exactly what an operator arming a
// prompt-screening plugin expects); the command-hook bridge has no channel
// to carry a Modify replacement, so only Block is honoured.
//
// Two deliberate skips: SkipUserPromptSubmit (the harness's own nudge /
// schema-recovery re-asks — the operator's prompt was already screened on
// the first pass, and the reminder text is not theirs); and an EMPTY
// extracted prompt (a resumed tool_result-only turn — the original prompt
// was screened before the pause, and a hook fired with no payload is
// noise).
func fireUserPromptSubmit(ctx context.Context, opts GenerationOptions) error {
	if opts.Hooks == nil || opts.SkipUserPromptSubmit {
		return nil
	}
	prompt := lastUserMessageText(opts.Messages)
	if prompt == "" {
		return nil
	}
	dec, _ := opts.Hooks.Fire(ctx, hooks.Context{
		Event:      hooks.UserPromptSubmit,
		UserPrompt: prompt,
	})
	if dec.Action == hooks.ActionBlock {
		reason := strings.TrimSpace(dec.Reason)
		if reason == "" {
			reason = "no reason given"
		}
		return fmt.Errorf("a UserPromptSubmit hook blocked this node's prompt: %s", reason)
	}
	return nil
}

// lastUserMessageText returns the concatenated text blocks of the last
// user-role message — the prompt a UserPromptSubmit hook screens. ""
// when the conversation carries no user text (a resumed tool_result-only
// turn).
func lastUserMessageText(messages []api.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		var b strings.Builder
		for _, c := range messages[i].Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		return b.String()
	}
	return ""
}

func GenerateTextDirect(ctx context.Context, client api.APIClient, opts GenerationOptions) (*TextResult, error) {
	if err := guardNonEmptyConversation(opts.Messages); err != nil {
		return nil, err
	}
	// UserPromptSubmit BEFORE the Stop defer: a prompt the hook blocks
	// never became a session, so it must not fire the session-end event
	// (Stop observers would see an end for a start that never happened).
	if err := fireUserPromptSubmit(ctx, opts); err != nil {
		return nil, err
	}
	if opts.Hooks != nil {
		defer func() {
			_, _ = opts.Hooks.Fire(ctx, hooks.Context{Event: hooks.Stop})
		}()
	}

	maxSteps := opts.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}

	toolMap := buildToolMap(opts.Tools)

	// Copy messages to avoid mutating caller's slice.
	messages := make([]api.Message, len(opts.Messages))
	copy(messages, opts.Messages)

	// Always-thinking models reject tool_choice:any. Keep the grounding
	// contract in the executor: an announced, malformed or denied call is
	// not an executed tool. Chain the observer rather than replacing it.
	requireExecutedTool := opts.ForceInitialToolUse && len(opts.Tools) > 0 && requiresAdaptiveThinking(opts.Model)
	executedTool := false
	if requireExecutedTool {
		observer := opts.OnToolStarted
		opts.OnToolStarted = func(info ToolCallInfo) {
			executedTool = true
			if observer != nil {
				observer(info)
			}
		}
		messages = append(messages, api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Before giving your final answer, execute at least one of the available tools to ground your answer in observed evidence."}}})
	}

	var steps []StepResult
	var totalUsage Usage
	var lastText string
	var lastToolCalls []ToolCall
	var lastFinish FinishReason

	// result assembles whatever has been accumulated so far. Error paths
	// return it as a best-effort partial result — the caller can stash the
	// conversation history for compaction-aware retries even when this
	// attempt fails (consult `err` first); the clean exit returns it as
	// the final result.
	result := func() *TextResult {
		return &TextResult{
			Text:         lastText,
			ToolCalls:    lastToolCalls,
			Steps:        steps,
			TotalUsage:   totalUsage,
			FinishReason: lastFinish,
			Messages:     messages,
		}
	}

	toolCallsSoFar := 0
	for step := 1; step <= maxSteps; step++ {
		// callWithContextRetry builds the request, calls the model, and on
		// a context-window rejection force-compacts `messages` (in place)
		// and retries — so a backend whose real window is smaller than the
		// model's advertised one (ChatGPT-forfait) recovers instead of
		// killing the run.
		agg, err := callWithContextRetry(ctx, client, opts, &messages, forcedInitialToolChoice(opts, toolCallsSoFar))
		if err != nil {
			// A call the provider served before failing was billed: its
			// partial usage, reported or not, joins the total the failure
			// path meters.
			if agg != nil {
				accumulateUsage(&totalUsage, agg.usage)
			}
			return result(), err
		}

		accumulateUsage(&totalUsage, agg.usage)
		stepResult := buildStepResult(step, agg)
		steps = append(steps, stepResult)
		toolCallsSoFar += len(stepResult.ToolCalls)

		if opts.OnStepFinish != nil {
			opts.OnStepFinish(stepResult)
		}

		lastText = agg.text
		lastToolCalls = stepResult.ToolCalls
		lastFinish = stepResult.FinishReason

		// If no tool calls or stop reason is not tool_use, we're done —
		// unless the operator inbox holds undelivered messages (e.g. an
		// async-question answer that landed while the model was
		// composing its final text). Final-drain parity with
		// claude_code's Stop/BlockStop hook: inject them and give the
		// model another turn, bounded by maxSteps.
		if len(agg.toolUses) == 0 || stepResult.FinishReason != FinishToolCalls {
			if step < maxSteps {
				if drained, ok := finalDrainInbox(ctx, messages, agg.text, opts); ok {
					messages = drained
					continue
				}
			}
			captureFinalTurn(opts, messages, step, stepResult)
			break
		}

		// Append assistant message with the tool_use blocks.
		messages = append(messages, assistantToolUseMessage(agg.text, agg.toolUses))

		// Execute tools and append tool_result message.
		toolResults, toolErr := executeToolsDirect(ctx, agg.toolUses, toolMap, opts.OnToolStarted, opts.OnToolCall, opts.Hooks, opts.MaterializeSecrets, opts.Permission)
		unmaterializeEchoingResults(toolResults, agg.toolUses, opts.MaterializeSecrets, opts.UnmaterializeSecrets, opts.NamesSecret)
		if toolErr != nil {
			// ErrAskUser (and any future suspension signal) bubbles up to
			// the backend, which converts it into iterion's pause flow.
			stashPauseConversation(toolErr, messages, opts)
			return result(), toolErr
		}
		messages = append(messages, api.Message{
			Role:    "user",
			Content: toolResults,
		})

		captureToolRoundTurn(opts, messages, step, stepResult)
		messages = compactBetweenIterations(ctx, messages, opts)
		messages = drainOperatorInbox(ctx, messages, opts)
	}

	if requireExecutedTool && !executedTool {
		return result(), fmt.Errorf("model %s returned before executing a required initial tool", opts.Model)
	}
	return result(), nil
}

// forcedInitialToolChoice is the agentic-parity lever: while
// ForceInitialToolUse is set and the model has not yet called a single
// tool, it pins tool_choice to "any" so the model MUST use a tool before
// it can answer — then reverts to auto (nil) so it can finish. Without
// this, gpt-5.5 (and other models) under the default "auto" choice skip
// the tools and answer from priors, producing ungrounded verdicts. No-op
// without tools.
func forcedInitialToolChoice(opts GenerationOptions, toolCallsSoFar int) *api.ToolChoice {
	if opts.ForceInitialToolUse && len(opts.Tools) > 0 && toolCallsSoFar == 0 && !requiresAdaptiveThinking(opts.Model) {
		return &api.ToolChoice{Type: "any"}
	}
	return nil
}

// requiresAdaptiveThinking reads the vendor's adaptive-thinking profile on
// the route's capability id. A vendor route behind an OpenAI-compatible host
// may carry the vendor's own namespace in its id
// ("openai/anthropic/claude-opus-5-5"), so the id's last segment is asked
// too: a false positive only trades a forced tool_choice for the
// nudge-and-check, which stays fail-closed. A gateway route never borrows a
// vendor's profile.
func requiresAdaptiveThinking(model string) bool {
	id := modelroute.Parse(model).CapabilityID()
	if id == "" {
		return false
	}
	if apikit.AnthropicProfile(id).RequiresAdaptiveThinking {
		return true
	}
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return apikit.AnthropicProfile(id[i+1:]).RequiresAdaptiveThinking
	}
	return false
}

// buildStepResult shapes one aggregated model response into the StepResult
// recorded on the step list and delivered to OnStepFinish.
func buildStepResult(step int, agg *aggregatedResponse) StepResult {
	return StepResult{
		Number:       step,
		Text:         agg.text,
		ToolCalls:    toolCallsFromBlocks(agg.toolUses),
		FinishReason: mapStopReason(agg.stopReason),
		Usage:        agg.usage,
		Thinking:     agg.thinkingText,
		MaxTokens:    agg.maxTokens,
	}
}

// captureFinalTurn fires OnTurnCapture for the terminal step. The live
// `messages` slice doesn't get this step's assistant response (the loop
// exits), so the snapshot synthesizes it by appending an assistant text
// block when the step produced text. The fork UX would never anchor here
// (final = no follow-up to resume), but the timeline still wants to
// display the turn.
func captureFinalTurn(opts GenerationOptions, messages []api.Message, step int, stepResult StepResult) {
	if opts.OnTurnCapture == nil {
		return
	}
	snap := append([]api.Message(nil), messages...)
	if stepResult.Text != "" {
		snap = append(snap, api.Message{
			Role: "assistant",
			Content: []api.ContentBlock{{
				Type: "text",
				Text: stepResult.Text,
			}},
		})
	}
	opts.OnTurnCapture(TurnCaptureInfo{
		Step:         step,
		Result:       stepResult,
		Conversation: snap,
	})
}

// captureToolRoundTurn fires OnTurnCapture at the natural end-of-iteration
// boundary: the live `messages` slice now contains everything the NEXT
// LLM call would see — exactly the snapshot the Fork API needs to
// rehydrate a child claw conversation. Takes a defensive copy because the
// loop reuses the slice after this point.
func captureToolRoundTurn(opts GenerationOptions, messages []api.Message, step int, stepResult StepResult) {
	if opts.OnTurnCapture == nil {
		return
	}
	snap := append([]api.Message(nil), messages...)
	opts.OnTurnCapture(TurnCaptureInfo{
		Step:         step,
		Result:       stepResult,
		Conversation: snap,
	})
}

// stashPauseConversation captures the conversation onto a
// *delegate.ErrAskUser suspension so the backend can persist it and resume
// mid-loop. At this point `messages` already contains the assistant
// message with the pending tool_use block. Pure-function compaction is
// applied before marshalling so a long transcript is bounded on disk; the
// pending tool_use stays in the preserved-recent window (default 4) so its
// ID remains addressable at resume time. Non-ask errors (and marshal
// failures) leave the error untouched.
func stashPauseConversation(toolErr error, messages []api.Message, opts GenerationOptions) {
	var askErr *delegate.ErrAskUser
	if !errors.As(toolErr, &askErr) {
		return
	}
	if convBytes, mErr := json.Marshal(maybeCompactPause(messages, opts.Model, opts.CompactThresholdRatio, opts.CompactPreserveRecent, askErr.PendingToolUseID)); mErr == nil {
		askErr.Conversation = convBytes
	}
}

// compactBetweenIterations compacts the running history before the next
// round if it's grown large. No-op for short transcripts; for long ones,
// older tool turns are summarised while the last 4 messages stay verbatim,
// so the assistant message that just dispatched tool_use blocks paired
// with our tool_results stays in the preserved-recent window. Without this
// the tool loop on a small-context model crashes with
// context_length_exceeded once history exceeds the budget.
//
// The threshold-triggered compaction is the claw firing point of the
// PreCompact / PostCompact lifecycle events (claude_code's auto-compact
// parity): PreCompact's Block skips THIS compaction, PostCompact observes
// the result. The forced compaction of callWithContextRetry — recovery
// from a context-window rejection — deliberately does not fire them: a
// Block there could only turn a recoverable overflow into a failed node.
func compactBetweenIterations(ctx context.Context, messages []api.Message, opts GenerationOptions) []api.Message {
	compacted, info, ok := maybeCompact(messages, opts.Model, opts.CompactThresholdRatio, opts.CompactPreserveRecent)
	if !ok {
		return messages
	}
	if opts.Hooks != nil {
		dec, _ := opts.Hooks.Fire(ctx, hooks.Context{Event: hooks.PreCompact, MessageCount: len(messages)})
		if dec.Action == hooks.ActionBlock {
			return messages
		}
	}
	// Compaction is about to fire: give OnBeforeCompact a chance to
	// inject content (e.g. a session-memory user turn) so the summary
	// preserves it. The injected slice feeds the summariser only; the
	// live history keeps the originals.
	if opts.OnBeforeCompact != nil {
		if modified := opts.OnBeforeCompact(messages); modified != nil {
			if reCompacted, reInfo, reOk := maybeCompact(modified, opts.Model, opts.CompactThresholdRatio, opts.CompactPreserveRecent); reOk {
				compacted, info = reCompacted, reInfo
			}
		}
	}
	messages = compacted
	if opts.OnCompact != nil {
		opts.OnCompact(info)
	}
	// Re-anchor the task list after the squeeze (Grok-style reseed):
	// the todo file survived on disk, the model's view of it did not.
	if hasTodoTool(opts.Tools) {
		messages = append(messages, todoReseedMessage())
	}
	if opts.Hooks != nil {
		_, _ = opts.Hooks.Fire(ctx, hooks.Context{Event: hooks.PostCompact, MessageCount: len(messages)})
	}
	return messages
}

// finalDrainInbox is the end-of-turn twin of drainOperatorInbox: called
// when the model produced a final (no-tool) answer, it checks the inbox
// one last time. When messages are waiting it appends the model's final
// text as an assistant turn (so the history stays coherent) followed by
// the operator message, and reports ok=true so the caller loops for one
// more turn instead of finishing — the claw analog of claude_code's
// Stop hook with BlockStop.
func finalDrainInbox(ctx context.Context, messages []api.Message, finalText string, opts GenerationOptions) ([]api.Message, bool) {
	operatorMsg, ok := consumeAndDrainInbox(ctx, opts)
	if !ok {
		return messages, false
	}
	if finalText != "" {
		messages = append(messages, api.Message{
			Role:    "assistant",
			Content: []api.ContentBlock{{Type: "text", Text: finalText}},
		})
	}
	return append(messages, operatorMsg), true
}

// drainOperatorInbox drains operator-queued chatbox messages AFTER
// compaction so they always land in the preserved-recent window.
func drainOperatorInbox(ctx context.Context, messages []api.Message, opts GenerationOptions) []api.Message {
	if operatorMsg, ok := consumeAndDrainInbox(ctx, opts); ok {
		messages = append(messages, operatorMsg)
	}
	return messages
}

// consumeAndDrainInbox is the shared drain core: Consume runs FIRST so
// the studio inbox transitions delivered → consumed in lockstep with the
// next request, then any newly-queued texts are wrapped into one
// synthetic operator turn.
func consumeAndDrainInbox(ctx context.Context, opts GenerationOptions) (api.Message, bool) {
	if opts.Inbox == nil {
		return api.Message{}, false
	}
	opts.Inbox.Consume(ctx)
	drained := opts.Inbox.Drain(ctx)
	if len(drained) == 0 {
		return api.Message{}, false
	}
	return buildOperatorMessage(drained), true
}

// buildOperatorMessage wraps any operator-queued chat messages into a
// single synthetic user turn the LLM observes between tool iterations.
// A <system-reminder> header marks the harness provenance while stating
// explicitly that the content below carries user authority — the agent
// applies it, adjusting its current plan if relevant.
func buildOperatorMessage(texts []string) api.Message {
	var sb strings.Builder
	sb.WriteString(systemReminder("The operator queued the message(s) below mid-run. " +
		"They are user instructions: apply them from now on, adjusting your current plan if needed."))
	sb.WriteString("\n\n")
	for i, t := range texts {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		sb.WriteString(t)
	}
	return api.Message{
		Role: "user",
		Content: []api.ContentBlock{{
			Type: "text",
			Text: sb.String(),
		}},
	}
}

// assistantToolUseMessage builds the assistant turn that contains text (if any)
// followed by tool_use content blocks.
// findPendingToolUse scans a rehydrated conversation for the tool_use
// content block with the given id, returning its tool name and input.
// Used on resume to tell whether a paused tool_use was the ask_user
// infra tool or a real action the permission gate suspended.
func findPendingToolUse(msgs []api.Message, id string) (name string, input map[string]any, ok bool) {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "tool_use" && b.ID == id {
				return b.Name, b.Input, true
			}
		}
	}
	return "", nil, false
}

func assistantToolUseMessage(text string, toolUses []toolUseBlock) api.Message {
	content := make([]api.ContentBlock, 0, len(toolUses)+1)
	if text != "" {
		content = append(content, api.ContentBlock{
			Type: "text",
			Text: text,
		})
	}
	for _, tu := range toolUses {
		// inputMap is the structured args the next API turn replays
		// back as the assistant message context. A nil Input on a
		// tool_use block produces a malformed-looking history that
		// confuses some providers; fall back to an empty object so
		// the block is at least syntactically intact. Malformed
		// PartialJSON at this point is rare (aggregateStream guards
		// against truncation) so we don't bubble it up — the
		// corresponding tool_result already carries the failure.
		inputMap := map[string]any{}
		if tu.PartialJSON != "" {
			_ = json.Unmarshal([]byte(tu.PartialJSON), &inputMap)
		}
		content = append(content, api.ContentBlock{
			Type:  "tool_use",
			ID:    tu.ID,
			Name:  tu.Name,
			Input: inputMap,
		})
	}
	return api.Message{Role: "assistant", Content: content}
}

// ---------------------------------------------------------------------------
// Core generation: structured output
// ---------------------------------------------------------------------------

// GenerateObjectDirect generates structured output by injecting a synthetic tool
// with the given schema and forcing the model to call it. The tool_use input
// is parsed as the result object of type T.
//
// A model that does not answer through the forced tool gets one more chance,
// never more (#2651). First, for free, the turn it did make is read
// (harvestStructured): a reply that IS the JSON object, or — when the caller
// allows it (AnswerFromReasoning) — an object ending the reasoning of a turn
// that showed no text; nothing is read from a turn cut at max_tokens, and a
// reply that merely contains an object is never taken for the answer. Then
// ONE unforced re-ask: the synthetic tool stays declared, nothing forces it,
// and the model may call it or reply with the JSON object alone — the shape
// a gateway model that ignores, or cannot honour, a named tool_choice still
// answers in. After a forced turn cut at max_tokens the re-ask runs at a
// lowered reasoning effort, and it is skipped when the effort cannot go
// lower or the turn was cut by the length of its own answer: either way it
// would be the request just cut, minus the tool force. A failure of the
// re-ask's own request ends the call, final. Every exit past the first
// request returns the usage of every turn it paid for; a call that never
// got an answer fails with a *StructuredOutputError naming each turn.
func GenerateObjectDirect[T any](ctx context.Context, client api.APIClient, opts GenerationOptions) (*ObjectResult[T], error) {
	schemaName := opts.SchemaName
	if schemaName == "" {
		schemaName = "structured_output"
	}

	if len(opts.ExplicitSchema) == 0 {
		return nil, fmt.Errorf("GenerateObjectDirect requires ExplicitSchema to be set")
	}
	if err := guardNonEmptyConversation(opts.Messages); err != nil {
		return nil, err
	}
	// UserPromptSubmit BEFORE the Stop defer, like GenerateTextDirect: a
	// blocked prompt never became a session and must not fire session-end.
	if err := fireUserPromptSubmit(ctx, opts); err != nil {
		return nil, err
	}
	if opts.Hooks != nil {
		defer func() {
			_, _ = opts.Hooks.Fire(ctx, hooks.Context{Event: hooks.Stop})
		}()
	}

	var inputSchema api.InputSchema
	if err := json.Unmarshal(opts.ExplicitSchema, &inputSchema); err != nil {
		// Typed AND worded: this call is made in-process by the executor
		// and out-of-process by the claw runner, whose subprocess
		// boundary flattens the type to text before it reaches the
		// classifier.
		return nil, fmt.Errorf("parse ExplicitSchema: %w", &delegate.ErrSchemaUnusable{
			Schema: schemaName, Detail: err.Error(),
		})
	}

	syntheticTool := api.Tool{
		Name:        schemaName,
		Description: "Return the structured output matching the required schema.",
		InputSchema: inputSchema,
	}
	toolChoice := &api.ToolChoice{Type: "tool", Name: schemaName}

	// Copy messages to avoid mutating caller's slice.
	messages := make([]api.Message, len(opts.Messages))
	copy(messages, opts.Messages)
	if requiresAdaptiveThinking(opts.Model) {
		// Native strict JSON cannot represent the DSL's arbitrary JSON fields.
		// Keep the existing tool schema and fail-closed parser below, using
		// auto plus an explicit instruction where forced choice is rejected.
		toolChoice = nil
		messages = append(messages, api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: fmt.Sprintf("Return the requested structured result by calling the %q tool with arguments matching its schema. Do not replace the tool call with prose or a JSON text block.", schemaName)}}})
	}

	// partial is the best-effort result the error paths BELOW the first
	// request return beside their error — the same contract
	// GenerateTextDirect's result() has, and for the same reason: the
	// provider billed every turn made, and this is the only place that
	// figure exists. Consult `err` first; Object is the zero value and only
	// TotalUsage is meaningful.
	//
	// The exits it serves are the expensive ones — a stream that died
	// mid-answer, a truncated tool_use, JSON the model malformed, a re-ask
	// that answered no better. Returning a bare nil there made a
	// fully-billed structured call indistinguishable from one that never
	// left the process.
	var totalUsage Usage
	var steps []StepResult
	partial := func() *ObjectResult[T] {
		var zero T
		return &ObjectResult[T]{Object: zero, TotalUsage: totalUsage}
	}

	// turn makes one structured request. A turn is anchored like a text
	// turn: OnStepFinish and a TurnCheckpoint each (capturing only in
	// GenerateTextDirect once left every `output:`-bearing node — most of
	// them in iterion — without a timeline and impossible to fork).
	// served is false when the stream never opened: nothing was billed.
	turn := func(o GenerationOptions, msgs []api.Message, tc *api.ToolChoice, number int) (agg *aggregatedResponse, step StepResult, served bool, err error) {
		reqOpts := o
		// Request-only overlay: buildRequest then carries the synthetic tool
		// alone, through extraTools.
		reqOpts.Tools = nil
		req, err := buildRequest(reqOpts, msgs, []api.Tool{syntheticTool}, tc)
		if err != nil {
			return nil, StepResult{}, false, err
		}
		fireOnRequest(o, len(msgs))
		agg, err = callAndAggregate(ctx, client, req, o)
		if err != nil {
			return nil, StepResult{}, false, err
		}
		accumulateUsage(&totalUsage, agg.usage)
		if agg.err != nil {
			return agg, StepResult{}, true, agg.err
		}
		step = StepResult{
			Number:       number,
			Text:         agg.text,
			ToolCalls:    toolCallsFromBlocks(agg.toolUses),
			FinishReason: mapStopReason(agg.stopReason),
			Usage:        agg.usage,
			Thinking:     agg.thinkingText,
			MaxTokens:    agg.maxTokens,
		}
		steps = append(steps, step)
		if o.OnStepFinish != nil {
			o.OnStepFinish(step)
		}
		captureFinalTurn(o, msgs, number, step)
		return agg, step, true, nil
	}

	// answer is the success shape: the object, and the replay-safe
	// conversation with the answer stored as assistant JSON text (never an
	// unmatched synthetic tool_use block).
	answer := func(obj T, raw string, msgs []api.Message, agg *aggregatedResponse, step StepResult, recovered string) *ObjectResult[T] {
		conversation := append([]api.Message(nil), msgs...)
		conversation = append(conversation, api.Message{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: raw}}})
		return &ObjectResult[T]{
			Object:       obj,
			Text:         agg.text,
			Steps:        steps,
			TotalUsage:   totalUsage,
			FinishReason: step.FinishReason,
			Messages:     conversation,
			Recovered:    recovered,
		}
	}

	// read takes a turn's answer: the synthetic tool block, else a JSON
	// object the turn carried outside it. blockProblem says why a block
	// that is there could not be used ("" when there is none).
	read := func(agg *aggregatedResponse, step StepResult) (obj T, raw string, recovered string, ok bool, blockProblem string) {
		for _, tu := range agg.toolUses {
			if tu.Name != schemaName {
				continue
			}
			if tu.PartialJSON == "" {
				blockProblem = "tool block with empty input"
				continue
			}
			if err := json.Unmarshal([]byte(tu.PartialJSON), &obj); err != nil {
				// Cap the raw payload so a 5 MB truncated JSON doesn't flood
				// logs.
				head := tu.PartialJSON
				if len(head) > 500 {
					head = head[:500] + "…"
				}
				blockProblem = fmt.Sprintf("tool block with malformed JSON: %v (raw: %s)", err, head)
				continue
			}
			return obj, tu.PartialJSON, "", true, ""
		}
		harvested, source := harvestStructured(opts.ExplicitSchema, agg.text, agg.thinkingText, step.FinishReason == FinishLength, opts.AnswerFromReasoning)
		if harvested != nil && json.Unmarshal(harvested, &obj) == nil {
			return obj, string(harvested), source, true, blockProblem
		}
		var zero T
		return zero, "", "", false, blockProblem
	}

	describe := func(label string, o GenerationOptions, agg *aggregatedResponse, step StepResult, blockProblem string) StructuredAttempt {
		return StructuredAttempt{
			Label:        label,
			Effort:       reasoningEffortOf(o),
			Finish:       step.FinishReason,
			OutputTokens: agg.usage.OutputTokens,
			MaxTokens:    stepMaxTokens(step, o),
			TextLen:      len(strings.TrimSpace(agg.text)),
			ReasoningLen: len(agg.thinkingText),
			ToolBlock:    blockProblem,
			Reasoned:     spentOnReasoning(step),
		}
	}

	firstLabel := "forced " + schemaName
	if toolChoice == nil {
		firstLabel = schemaName + " (instructed, not forced)"
	}
	agg, step, served, err := turn(opts, messages, toolChoice, 1)
	if err != nil {
		if !served {
			// The stream never opened — nothing was served, nothing is owed.
			return nil, err
		}
		return partial(), err
	}
	obj, raw, recovered, ok, problem := read(agg, step)
	if ok {
		return answer(obj, raw, messages, agg, step, recovered), nil
	}
	failure := &StructuredOutputError{
		Schema:   schemaName,
		Attempts: []StructuredAttempt{describe(firstLabel, opts, agg, step, problem)},
	}

	// The one re-ask. After a cut it must change what spent the budget —
	// the effort — or it would be the cut request minus the tool force; a
	// turn cut by the length of its own answer has no effort to give back.
	reaskOpts := opts
	if step.FinishReason == FinishLength {
		skip := ""
		lowered, _, canLower := effortAfterCut(opts)
		switch {
		case !spentOnReasoning(step):
			skip = "the turn above was cut by the length of its own answer, which a lower reasoning effort cannot shorten"
		case !canLower:
			skip = "the turn above was cut at max_tokens and reasoning_effort cannot go lower, so the same request would be cut the same way"
		}
		if skip != "" {
			failure.Attempts = append(failure.Attempts, StructuredAttempt{Label: "unforced re-ask", Skipped: skip})
			return partial(), failure
		}
		reaskOpts = lowered
	}
	reaskMessages := withTrailingUserText(messages, unforcedReaskInstruction(schemaName))
	agg2, step2, _, err2 := turn(reaskOpts, reaskMessages, nil, 2)
	if err2 != nil {
		if isContextEnd(err2) && ctx.Err() != nil {
			// The run's own cancellation or deadline stays what it is.
			return partial(), fmt.Errorf("structured output re-ask: %w", err2)
		}
		failure.Attempts = append(failure.Attempts, StructuredAttempt{Label: "unforced re-ask", Effort: reasoningEffortOf(reaskOpts), CallError: callFailureKind(err2)})
		failure.Cause = err2
		return partial(), failure
	}
	obj, raw, _, ok, problem = read(agg2, step2)
	if ok {
		return answer(obj, raw, reaskMessages, agg2, step2, recoveredFromUnforced), nil
	}
	failure.Attempts = append(failure.Attempts, describe("unforced re-ask", reaskOpts, agg2, step2, problem))
	return partial(), failure
}

// clawRawTools: the claw tools whose result shows what the workspace holds —
// a file, a command's output, a search over files. Their result is left as
// is: an agent editing a line that holds a secret must see the value the file
// holds. Every other tool's result — a fetch quoting its URL, a trigger its
// request, a message, a task or a structured payload echoing what it was
// given, an MCP tool whatever it reports — goes back to placeholders.
var clawRawTools = regexp.MustCompile(`^(read_file|bash|repl|grep|workspace_grep|glob|file_edit|lsp|read_image|screenshot|computer_use|diagnostic_shell)$`)

// unmaterializeEchoingResults turns the known secret values an echoing
// tool's result quotes back into their placeholders, in place.
func unmaterializeEchoingResults(results []api.ContentBlock, uses []toolUseBlock, materialize, unmaterialize func(string) string, namesSecret func(string) bool) {
	if unmaterialize == nil {
		return
	}
	// A workspace reader keeps what the workspace holds — unless its call
	// carried a secret (a grep's pattern, an image's URL): it may quote it.
	// Carried is read grant-agnostically first: a placeholder the proxy
	// resolves on the wire is a secret the call carried as much as one the
	// agent's materialiser resolved.
	names := make(map[string]string, len(uses))
	given := make(map[string]bool, len(uses))
	for _, tu := range uses {
		names[tu.ID] = tu.Name
		given[tu.ID] = (namesSecret != nil && namesSecret(tu.PartialJSON)) ||
			(materialize != nil && string(secretguard.MaterializeJSON([]byte(tu.PartialJSON), materialize)) != tu.PartialJSON)
	}
	for i := range results {
		if clawRawTools.MatchString(names[results[i].ToolUseID]) && !given[results[i].ToolUseID] {
			continue
		}
		for j := range results[i].Content {
			results[i].Content[j].Text = unmaterialize(results[i].Content[j].Text)
		}
	}
}
