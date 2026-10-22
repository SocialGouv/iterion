package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Structured-output recovery (#2651): what a structured turn does when the
// model's answer did not arrive as the synthetic tool block. A model behind
// an OpenAI-compatible gateway can ignore a forced tool_choice, answer in
// plain text or in its reasoning, call the tool only when it is not forced,
// or spend its whole output budget reasoning — the claw chat wire then
// reported a clean, empty turn. The pieces here read what the turn did
// carry before anything is paid for again, and decide what the one re-ask
// changes.
//
// What is read is deliberately narrow: an object a model QUOTES — a peer's
// verdict, an injection planted in a tool result, the prompt's own example,
// a draft it then revised — is not its answer, and a reply that merely
// contains one is refused. A reply counts only when it IS the object.

// structuredRecovered names where a structured answer came from when it was
// not the forced tool block (ObjectResult.Recovered).
const (
	recoveredFromText      = "text"
	recoveredFromReasoning = "reasoning"
	recoveredFromUnforced  = "unforced"
)

// unforcedReaskInstruction is the text of the one unforced re-ask: the
// synthetic tool stays declared, nothing forces it, and a JSON object in the
// reply is accepted too.
func unforcedReaskInstruction(schemaName string) string {
	return systemReminder(fmt.Sprintf(
		"Your previous reply did not contain the structured result. Reply now with that result only: "+
			"call the %q tool with arguments matching its schema, or reply with the JSON object alone — "+
			"no prose, no markdown fence.", schemaName))
}

// withTrailingUserText returns msgs with text as the conversation's next
// user words: one more text block on the last message when it is a user
// turn, a new user message otherwise. Two user turns in a row are refused by
// strict chat templates ("conversation roles must alternate"), and a
// structured call's conversation usually ends on one. msgs and its messages
// are left untouched.
func withTrailingUserText(msgs []api.Message, text string) []api.Message {
	out := append([]api.Message(nil), msgs...)
	block := api.ContentBlock{Type: "text", Text: text}
	if n := len(out); n > 0 && out[n-1].Role == "user" {
		last := out[n-1]
		last.Content = append(append([]api.ContentBlock(nil), last.Content...), block)
		out[n-1] = last
		return out
	}
	return append(out, api.Message{Role: "user", Content: []api.ContentBlock{block}})
}

// gatewayReasoningTruncatedMark ends a captured reasoning that outgrew the
// capture's bound: the text no longer ends where the model's did, and the
// mark keeps a harvest from reading its last object as the answer.
const gatewayReasoningTruncatedMark = "\n[reasoning truncated by iterion]"

// adoptGatewayReasoning makes the reasoning an OpenAI-compatible gateway
// streamed the turn's thinking when the stream itself carried no thinking
// block — claw's chat translator drops it (compatgw.ReasoningCapture). A
// provider that emits thinking blocks keeps its own.
func adoptGatewayReasoning(agg *aggregatedResponse, capture *compatgw.ReasoningCapture) {
	if agg == nil || agg.thinkingText != "" {
		return
	}
	reasoning := capture.Reasoning()
	if reasoning == "" {
		return
	}
	if capture.Truncated() {
		reasoning += gatewayReasoningTruncatedMark
	}
	agg.thinkingText = reasoning
}

// markBudgetCut relabels a turn that spent its whole output budget as cut
// at max_tokens. Two facts say so, whatever stop reason the wire translator
// synthesized: the gateway's own finish_reason "length", or a reported
// output count equal to the request's max_tokens. claw's
// chat-completions translator maps every finish_reason but "tool_calls" to
// end_turn, so on that wire a cut read as a clean stop — #2651's lots: the
// final turn and the recovery both out=8192 of 8192, text empty. Only a
// synthesized clean stop is relabelled; a tool_use turn and a stop reason a
// provider stated itself keep theirs.
func markBudgetCut(agg *aggregatedResponse, maxTokens int, rawFinish string) {
	if agg == nil || agg.err != nil {
		return
	}
	switch agg.stopReason {
	case "end_turn", "stop", "":
	default:
		return
	}
	// Exactly the budget: a provider that enforces max_tokens stops AT it,
	// and a count above it proves the budget was not applied (the
	// ChatGPT-Codex backend never receives one), so that turn was not cut.
	reached := maxTokens > 0 && agg.usage.UnreportedCalls == 0 && agg.usage.OutputTokens == maxTokens
	if rawFinish == "length" || reached {
		agg.stopReason = "max_tokens"
	}
}

// effectiveMaxTokens is the max_tokens opts asks for — before buildRequest
// fits a raised default into the window (StepResult.MaxTokens is what a
// request carried).
func effectiveMaxTokens(opts GenerationOptions) int {
	if opts.MaxTokens > 0 {
		return opts.MaxTokens
	}
	return defaultMaxTokens
}

// stepMaxTokens is the max_tokens step's request carried, or what opts asks
// for when the step does not say.
func stepMaxTokens(step StepResult, opts GenerationOptions) int {
	if step.MaxTokens > 0 {
		return step.MaxTokens
	}
	return effectiveMaxTokens(opts)
}

// spentOnReasoning reports whether a turn's output budget went, at least in
// part, to reasoning: reasoning it showed, reasoning tokens its provider
// counted, or no visible output at all — a turn cut with nothing to show
// spent its budget somewhere the wire did not carry. A turn cut while it
// wrote its answer, with no reasoning, was cut by the answer's own length,
// and a lower reasoning effort cannot shorten it.
func spentOnReasoning(step StepResult) bool {
	if step.Thinking != "" || step.Usage.ReasoningTokens > 0 {
		return true
	}
	if strings.TrimSpace(step.Text) != "" {
		return false
	}
	for _, tc := range step.ToolCalls {
		if len(tc.Input) > 0 {
			return false
		}
	}
	return true
}

// reasoningEffortOf is the reasoning effort opts sends, "" for none.
func reasoningEffortOf(opts GenerationOptions) string {
	effort, _ := opts.ProviderOptions["reasoning_effort"].(string)
	return effort
}

// effortAfterCut is opts with its reasoning effort lowered to "low" —
// coerced to a level the model accepts — for the turn that follows one cut
// at max_tokens: the cut turn spent the budget at that effort, and the same
// request would spend it the same way. ok is false when nothing can be
// lowered (no effort sent, an effort this code cannot rank, or one already
// at the model's lowest accepted level). The ProviderOptions map is copied,
// never written through.
func effortAfterCut(opts GenerationOptions) (lowered GenerationOptions, to string, ok bool) {
	from := wireEffort(reasoningEffortOf(opts))
	if from == "" {
		return opts, "", false
	}
	to = coerceEffortForModel("low", modelroute.Parse(opts.Model).CapabilityID())
	if to == "" || effortRank(to) >= effortRank(from) {
		return opts, "", false
	}
	po := make(map[string]any, len(opts.ProviderOptions))
	for k, v := range opts.ProviderOptions {
		po[k] = v
	}
	po["reasoning_effort"] = to
	opts.ProviderOptions = po
	return opts, to, true
}

// budgetRemedy is the advice a failure carries when a turn was cut at
// max_tokens before it produced an answer. Lowering the reasoning effort is
// advice only when the budget went to reasoning.
func budgetRemedy(maxTokens int, reasoned bool) string {
	if reasoned {
		return fmt.Sprintf("the output budget (max_tokens %d) was spent before an answer: raise the node's max_tokens, or lower its reasoning_effort", maxTokens)
	}
	return fmt.Sprintf("the answer is longer than the output budget (max_tokens %d): raise the node's max_tokens", maxTokens)
}

// callFailureKind names how a request failed without repeating its text: a
// provider's error body or a transport message carries the very phrases
// ("bad gateway", "connection reset") the retry classifiers read, and a
// re-ask's failure must not make the call it belongs to retryable.
func callFailureKind(err error) string {
	var clawErr *api.APIError
	if errors.As(err, &clawErr) && clawErr.StatusCode != 0 {
		return fmt.Sprintf("provider status %d", clawErr.StatusCode)
	}
	var modelErr *APIError
	if errors.As(err, &modelErr) && modelErr.StatusCode != 0 {
		return fmt.Sprintf("provider status %d", modelErr.StatusCode)
	}
	if isStreamIdleError(err) {
		return "the stream went silent"
	}
	return "the request did not complete"
}

// isContextEnd reports a cancellation or deadline error; when the call's own
// context has ended too, it is the run's, and it is never absorbed into a
// structured-output failure, so the caller still sees it.
func isContextEnd(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// StructuredAttempt is one structured turn as a failure reports it.
type StructuredAttempt struct {
	// Label names the turn ("forced structured_output", "unforced re-ask").
	Label string
	// Effort is the reasoning_effort the turn sent, "" for none.
	Effort string
	// Skipped, when set, is why the turn was not made; CallError, when set,
	// is how the request itself failed (callFailureKind: never the error's
	// own text). The fields below are then empty.
	Skipped, CallError string
	// Finish is the turn's finish reason, FinishLength for a max_tokens cut.
	Finish FinishReason
	// OutputTokens and MaxTokens are what the turn produced and could have.
	OutputTokens, MaxTokens int
	// TextLen and ReasoningLen are the sizes, in bytes, of the visible text
	// and the reasoning the turn carried.
	TextLen, ReasoningLen int
	// ToolBlock describes the synthetic tool block: "" for none, otherwise
	// why it could not be used.
	ToolBlock string
	// Reasoned reports a turn whose budget went to reasoning
	// (spentOnReasoning) — what the remedy of a cut names.
	Reasoned bool
}

// Cut reports a turn cut at max_tokens.
func (a StructuredAttempt) Cut() bool { return a.Finish == FinishLength }

func (a StructuredAttempt) String() string {
	label := a.Label
	if a.Effort != "" {
		label += " (reasoning_effort " + a.Effort + ")"
	}
	if a.Skipped != "" {
		return label + ": skipped — " + a.Skipped
	}
	if a.CallError != "" {
		return label + ": the call failed: " + a.CallError
	}
	block := "no tool block"
	if a.ToolBlock != "" {
		block = a.ToolBlock
	}
	return label + ": " + describeTurn(a.Finish, a.OutputTokens, a.MaxTokens, a.TextLen, a.ReasoningLen, block)
}

// describeTurn renders what a turn produced for a failure message; extra,
// when set, rides after the budget figures.
func describeTurn(finish FinishReason, outputTokens, maxTokens, textLen, reasoningLen int, extra string) string {
	parts := []string{fmt.Sprintf("finish=%s, output %d of max_tokens %d", finish, outputTokens, maxTokens)}
	if finish == FinishLength {
		parts[0] += " (cut at max_tokens)"
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	if textLen == 0 {
		parts = append(parts, "no text")
	} else {
		parts = append(parts, fmt.Sprintf("text %d bytes without a usable JSON object", textLen))
	}
	switch {
	case reasoningLen == 0:
		parts = append(parts, "no reasoning")
	case finish == FinishLength:
		parts = append(parts, fmt.Sprintf("reasoning %d bytes, cut mid-thought", reasoningLen))
	default:
		parts = append(parts, fmt.Sprintf("reasoning %d bytes without a usable JSON object", reasoningLen))
	}
	return strings.Join(parts, ", ")
}

// StructuredOutputError is the failure of a structured call that never got
// a usable answer: every turn it made (or skipped), what each came back
// with, and — when a turn was cut at max_tokens — the remedy. It is final:
// it neither unwraps to nor quotes the re-ask's own call error, so a
// transient failure of that extra turn never makes the call — or a whole
// tool loop around it — retryable. Cause keeps that error for a log line.
type StructuredOutputError struct {
	Schema   string
	Attempts []StructuredAttempt
	// Cause is the re-ask's call error, nil when every turn answered.
	Cause error
}

func (e *StructuredOutputError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "model did not produce a %q tool_use block", e.Schema)
	for i, a := range e.Attempts {
		if i == 0 {
			b.WriteString(" — ")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(a.String())
	}
	if maxTokens, reasoned, cut := e.cutBudget(); cut {
		b.WriteString(" — ")
		b.WriteString(budgetRemedy(maxTokens, reasoned))
	}
	return b.String()
}

// AnyCut reports whether a turn of the call was cut at max_tokens.
func (e *StructuredOutputError) AnyCut() bool {
	_, _, cut := e.cutBudget()
	return cut
}

// cutBudget is what the cut turns of the call say: the largest budget one
// was cut at, and whether any of them spent it on reasoning.
func (e *StructuredOutputError) cutBudget() (maxTokens int, reasoned, cut bool) {
	for _, a := range e.Attempts {
		if !a.Cut() {
			continue
		}
		cut = true
		maxTokens = max(maxTokens, a.MaxTokens)
		reasoned = reasoned || a.Reasoned
	}
	return maxTokens, reasoned, cut
}

// schemaShape is the part of a JSON schema a harvested object is checked
// against: its top-level required keys and declared properties.
type schemaShape struct {
	Required   []string                   `json:"required"`
	Properties map[string]json.RawMessage `json:"properties"`
}

// accepts reports whether obj has the schema's shape: every required key,
// or — for a schema that requires none — at least one declared property.
// A schema declaring neither accepts any object.
func (s schemaShape) accepts(obj map[string]any) bool {
	for _, k := range s.Required {
		if _, ok := obj[k]; !ok {
			return false
		}
	}
	if len(s.Required) > 0 || len(s.Properties) == 0 {
		return true
	}
	for k := range obj {
		if _, ok := s.Properties[k]; ok {
			return true
		}
	}
	return false
}

// harvestCandidateLimit bounds how many object openings a reasoning read
// tries, from its end: the answer closes the reasoning, and a reasoning full
// of braces must not cost a quadratic scan.
const harvestCandidateLimit = 512

// harvestStructured looks for the structured answer a turn carried outside
// the synthetic tool. Nothing is read from a turn cut at max_tokens: it is
// an unfinished thought, and its objects are drafts. The visible text counts
// only when the whole reply IS one JSON object with the schema's shape (a
// single markdown fence allowed) — what the unforced re-ask asks for; a
// reply that contains an object among prose quotes it, whatever the prose
// says about it. The reasoning counts only when the caller allows it
// (GenerationOptions.AnswerFromReasoning), the turn showed no text at all,
// and an object with the schema's shape ENDS the reasoning — the shape of a
// gateway model that answers in reasoning_content. Returns the object's JSON
// and where it came from, or nil.
func harvestStructured(schema json.RawMessage, text, reasoning string, cut, allowReasoning bool) (json.RawMessage, string) {
	if cut {
		return nil, ""
	}
	var shape schemaShape
	if len(schema) > 0 && json.Unmarshal(schema, &shape) != nil {
		return nil, ""
	}
	if raw := wholeShapedObject(text, shape); raw != nil {
		return raw, recoveredFromText
	}
	if allowReasoning && strings.TrimSpace(text) == "" {
		if raw := endingShapedObject(reasoning, shape); raw != nil {
			return raw, recoveredFromReasoning
		}
	}
	return nil, ""
}

// wholeShapedObject returns s when s, trimmed and out of one optional
// markdown fence, is exactly one JSON object with the schema's shape.
func wholeShapedObject(s string, shape schemaShape) json.RawMessage {
	body := strings.TrimSpace(s)
	if rest, fenced := strings.CutPrefix(body, "```"); fenced {
		inner, closed := strings.CutSuffix(strings.TrimSpace(rest), "```")
		if !closed {
			return nil
		}
		// The opening fence's language tag runs to its line's end.
		if nl := strings.IndexByte(inner, '\n'); nl >= 0 && !strings.HasPrefix(strings.TrimSpace(inner), "{") {
			inner = inner[nl+1:]
		}
		body = strings.TrimSpace(inner)
	}
	if !strings.HasPrefix(body, "{") {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(body), &obj) != nil || !shape.accepts(obj) {
		return nil
	}
	return json.RawMessage(body)
}

// endingShapedObject returns the object with the schema's shape that closes
// s — only whitespace and a closing markdown fence may follow it — or nil.
func endingShapedObject(s string, shape schemaShape) json.RawMessage {
	trimmed := strings.TrimRight(s, " \t\r\n")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimRight(trimmed, " \t\r\n")
	if !strings.HasSuffix(trimmed, "}") {
		return nil
	}
	end := len(trimmed)
	tried := 0
	for open := strings.LastIndex(trimmed, "{"); open >= 0 && tried < harvestCandidateLimit; open = strings.LastIndex(trimmed[:open], "{") {
		tried++
		// A shortcut, not a guard: only the object whose closing brace ends
		// the text can parse as the slice below (an object nested in the
		// answer closes before the answer does, and its slice runs on past
		// its own end), so the byte scan saves a parse doomed to fail.
		if matchingBrace(trimmed, open) != end-1 {
			continue
		}
		candidate := trimmed[open:end]
		var obj map[string]any
		if json.Unmarshal([]byte(candidate), &obj) != nil || !shape.accepts(obj) {
			continue
		}
		return json.RawMessage(candidate)
	}
	return nil
}

// matchingBrace returns the index of the '}' closing the '{' at open,
// skipping braces inside JSON string literals, or -1.
func matchingBrace(s string, open int) int {
	depth := 0
	inString := false
	escaped := false
	for i := open; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// lastStep is the final step of a tool loop's result — the turn that ended
// it — or the zero step when the loop recorded none.
func lastStep(r *TextResult) StepResult {
	if r == nil || len(r.Steps) == 0 {
		return StepResult{}
	}
	return r.Steps[len(r.Steps)-1]
}

// cutWithoutAnswer reports a step cut at max_tokens that produced nothing
// visible: no text, no tool call — its whole budget went to reasoning.
func cutWithoutAnswer(s StepResult) bool {
	return s.FinishReason == FinishLength && strings.TrimSpace(s.Text) == "" && len(s.ToolCalls) == 0
}

// recoveredLabel renders ObjectResult.Recovered for a log line.
func recoveredLabel(recovered string) string {
	switch recovered {
	case recoveredFromText:
		return "a JSON object in its text"
	case recoveredFromReasoning:
		return "a JSON object ending its reasoning"
	case recoveredFromUnforced:
		return "the unforced re-ask"
	}
	return recovered
}

// describeFinalTurn renders the tool loop's final turn for the failure of
// a tools+schema node. The budget remedy is added when that turn was cut
// and the recovery's own failure does not already carry it.
func describeFinalTurn(final StepResult, maxTokens int, recErr error) string {
	desc := describeTurn(final.FinishReason, final.Usage.OutputTokens, maxTokens,
		len(strings.TrimSpace(final.Text)), len(final.Thinking), "")
	var structured *StructuredOutputError
	remedied := errors.As(recErr, &structured) && structured.AnyCut()
	if final.FinishReason == FinishLength && !remedied {
		desc += " — " + budgetRemedy(maxTokens, spentOnReasoning(final))
	}
	return desc
}

// logReaskCallFailure logs the call error of a structured call's unforced
// re-ask: StructuredOutputError deliberately keeps that error's text out of
// its message (callFailureKind), so this line is where it is read.
func logReaskCallFailure(logger *iterlog.Logger, nodeID string, err error) {
	var structured *StructuredOutputError
	if logger == nil || !errors.As(err, &structured) || structured.Cause == nil {
		return
	}
	logger.Warn("[%s] the structured-output re-ask's call failed (not retried): %v", nodeID, structured.Cause)
}
