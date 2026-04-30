// Responses API support for OpenAI.
//
// OpenAI's /v1/chat/completions endpoint rejects the combination of
// `reasoning_effort` and `tools` for gpt-5.5+ (and likely other reasoning
// models in the future), with the explicit error:
//
//	Function tools with reasoning_effort are not supported for gpt-5.5
//	in /v1/chat/completions. Please use /v1/responses instead.
//
// This file implements a parallel streaming code path that targets the
// /v1/responses endpoint and translates its (very different) SSE event
// shape into the same Anthropic-style api.StreamEvent values that the
// rest of claw consumes. The dispatch lives in StreamResponse via
// shouldUseResponsesAPI(); when that returns false we keep using the
// well-tested chat completions path.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/api/httputil"
	"github.com/SocialGouv/claw-code-go/internal/api/sseutil"
)

// ----- Wire types: request --------------------------------------------------

// oaiResponsesRequest is the JSON body for POST /v1/responses.
//
// Differences from the chat completions request that matter here:
//   - `instructions` (string) replaces the system message.
//   - `input` is the array of user/assistant turns; each turn has
//     `role` and `content` where content items have a typed shape
//     ("input_text", "output_text", ...).
//   - Tools are flat: {type:"function", name, description, parameters}.
//     There is NO nested `function` object like in chat completions.
//   - `reasoning` is an object: {effort: "low"|"medium"|"high"}.
type oaiResponsesRequest struct {
	Model           string                `json:"model"`
	Instructions    string                `json:"instructions,omitempty"`
	Input           []oaiResponsesMessage `json:"input"`
	Tools           []oaiResponsesTool    `json:"tools,omitempty"`
	ToolChoice      any                   `json:"tool_choice,omitempty"`
	Reasoning       *oaiReasoningConfig   `json:"reasoning,omitempty"`
	Stream          bool                  `json:"stream"`
	MaxOutputTokens *int                  `json:"max_output_tokens,omitempty"`
}

type oaiReasoningConfig struct {
	Effort string `json:"effort,omitempty"`
}

type oaiResponsesMessage struct {
	Role    string                    `json:"role,omitempty"`
	Content []oaiResponsesContentPart `json:"content,omitempty"`

	// Tool-call rendering uses a separate top-level item rather than a
	// content part. The same struct represents three shapes selected by
	// Type:
	//   - "" (default): a regular role/content turn.
	//   - "function_call":   prior assistant tool_use → CallID, Name, Arguments.
	//   - "function_call_output": prior tool_result → CallID, Output.
	Type      string `json:"type,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type oaiResponsesContentPart struct {
	Type string `json:"type"`           // "input_text" | "output_text" | "input_image"
	Text string `json:"text,omitempty"` // text payload
}

type oaiResponsesTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ----- Wire types: streaming events -----------------------------------------

// oaiResponsesEvent is a generic decoder for events on the /v1/responses
// stream. Different events populate different fields; we read them in
// streamResponsesEvents based on Type.
type oaiResponsesEvent struct {
	Type string `json:"type"`

	// response.output_item.added (the .done variant is not consumed)
	OutputIndex int                     `json:"output_index"`
	Item        *oaiResponsesOutputItem `json:"item,omitempty"`

	// response.output_text.delta / done
	Delta string `json:"delta,omitempty"`

	// response.function_call_arguments.delta / done
	ItemID    string `json:"item_id,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// response.completed
	Response *oaiResponsesFinal `json:"response,omitempty"`
}

type oaiResponsesOutputItem struct {
	Type      string `json:"type"` // "message" | "function_call" | "reasoning"
	ID        string `json:"id"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Status    string `json:"status,omitempty"`
}

type oaiResponsesFinal struct {
	ID     string                   `json:"id"`
	Status string                   `json:"status"`
	Usage  *oaiResponsesUsage       `json:"usage,omitempty"`
	Output []oaiResponsesOutputItem `json:"output,omitempty"`
}

type oaiResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ----- Dispatch decision ----------------------------------------------------

// shouldUseResponsesAPI returns true when the request must be routed to
// /v1/responses instead of /v1/chat/completions.
//
// The chat completions endpoint rejects reasoning_effort+tools on gpt-5.5+,
// so the strict trigger is: ReasoningEffort is non-empty AND at least one
// tool is declared. This keeps every non-reasoning workflow on the
// well-tested chat completions path.
func shouldUseResponsesAPI(req api.CreateMessageRequest) bool {
	return req.ReasoningEffort != "" && len(req.Tools) > 0
}

// ----- Streaming entry point ------------------------------------------------

// streamResponses sends a streaming request to /v1/responses and returns
// a channel of api.StreamEvent values mapped into the Anthropic-shaped
// vocabulary used elsewhere in claw.
func (c *Client) streamResponses(ctx context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	respReq, err := c.buildResponsesRequest(req)
	if err != nil {
		return nil, fmt.Errorf("openai: build responses request: %w", err)
	}

	body, err := json.Marshal(respReq)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal responses request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: create responses request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: responses request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		bodyStr := string(errBody)
		return nil, &api.APIError{
			Provider:   "openai",
			StatusCode: resp.StatusCode,
			Message:    extractOpenAIErrorMessage(bodyStr),
			Body:       httputil.TruncateBody(bodyStr, httputil.BodyTruncateForLog),
			Retryable:  api.IsRetryableStatus(resp.StatusCode),
		}
	}

	ch := make(chan api.StreamEvent, 64)
	go c.streamResponsesEvents(ctx, resp, ch)
	return ch, nil
}

// ----- Request conversion ---------------------------------------------------

func (c *Client) buildResponsesRequest(req api.CreateMessageRequest) (*oaiResponsesRequest, error) {
	model := c.Model
	if req.Model != "" && !strings.HasPrefix(req.Model, "claude") {
		model = req.Model
	}
	wireModel := stripRoutingPrefix(model)

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = c.MaxTokens
	}

	tools, err := convertToolsToResponses(req.Tools)
	if err != nil {
		return nil, err
	}

	r := &oaiResponsesRequest{
		Model:        wireModel,
		Instructions: req.System,
		Input:        convertMessagesToResponsesInput(req.Messages),
		Tools:        tools,
		Stream:       true,
	}

	if maxTokens > 0 {
		r.MaxOutputTokens = &maxTokens
	}

	if req.ReasoningEffort != "" {
		r.Reasoning = &oaiReasoningConfig{Effort: req.ReasoningEffort}
	}

	if req.ToolChoice != nil {
		r.ToolChoice = convertToolChoiceToResponses(req.ToolChoice)
	}

	return r, nil
}

// convertMessagesToResponsesInput maps Anthropic-style messages onto the
// /v1/responses input array. The shape is broadly similar to chat
// completions but each content block carries a typed wrapper, and tool
// results live in a sibling "function_call_output" item rather than a
// dedicated tool role.
func convertMessagesToResponsesInput(messages []api.Message) []oaiResponsesMessage {
	var out []oaiResponsesMessage

	for _, msg := range messages {
		switch msg.Role {
		case "user":
			var parts []oaiResponsesContentPart
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						parts = append(parts, oaiResponsesContentPart{
							Type: "input_text",
							Text: block.Text,
						})
					}
				case "tool_result":
					out = append(out, oaiResponsesMessage{
						Type:   "function_call_output",
						CallID: block.ToolUseID,
						Output: httputil.ExtractText(block.Content),
					})
				}
			}
			if len(parts) > 0 {
				out = append(out, oaiResponsesMessage{
					Role:    "user",
					Content: parts,
				})
			}

		case "assistant":
			var parts []oaiResponsesContentPart
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						parts = append(parts, oaiResponsesContentPart{
							Type: "output_text",
							Text: block.Text,
						})
					}
				case "tool_use":
					args, _ := json.Marshal(block.Input)
					out = append(out, oaiResponsesMessage{
						Type:      "function_call",
						CallID:    block.ID,
						Name:      block.Name,
						Arguments: string(args),
					})
				}
			}
			if len(parts) > 0 {
				out = append(out, oaiResponsesMessage{
					Role:    "assistant",
					Content: parts,
				})
			}
		}
	}

	return out
}

// convertToolsToResponses maps tool definitions to the responses-API tool
// shape. The key difference vs chat completions is that the function name,
// description, and parameters are FLAT on the tool object (not nested
// under a `function` key).
//
// A marshal failure on any tool's input schema is propagated as an error
// rather than silently dropping the tool — see convertTools for rationale.
func convertToolsToResponses(tools []api.Tool) ([]oaiResponsesTool, error) {
	out := make([]oaiResponsesTool, 0, len(tools))
	for _, t := range tools {
		params, err := json.Marshal(map[string]interface{}{
			"type":       t.InputSchema.Type,
			"properties": t.InputSchema.Properties,
			"required":   t.InputSchema.Required,
		})
		if err != nil {
			return nil, fmt.Errorf("openai: marshal input schema for tool %q: %w", t.Name, err)
		}
		out = append(out, oaiResponsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  json.RawMessage(params),
		})
	}
	return out, nil
}

// convertToolChoiceToResponses adapts our ToolChoice to the responses-API
// shape. "auto" / "any" / "tool" map to "auto" / "required" / a typed
// {type:"function", name:...} object respectively.
func convertToolChoiceToResponses(tc *api.ToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "tool":
		return map[string]any{"type": "function", "name": tc.Name}
	case "any":
		return "required"
	case "auto":
		return "auto"
	}
	return nil
}

// ----- Streaming event translation ------------------------------------------

// streamResponsesEvents reads SSE frames from /v1/responses, decodes each
// event according to its `type` field, and emits Anthropic-shaped
// api.StreamEvent values on ch. The caller is the engine's normal stream
// aggregator; it does not know it's reading from /v1/responses.
func (c *Client) streamResponsesEvents(ctx context.Context, resp *http.Response, ch chan<- api.StreamEvent) {
	defer close(ch)
	defer resp.Body.Close()

	send := func(ev api.StreamEvent) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	if !send(api.StreamEvent{Type: api.EventMessageStart}) {
		return
	}

	// Use a 16 MiB scanner buffer. The default 64 KiB buffer (and the prior
	// 1 MiB limit) is silently exceeded by large reasoning chunks and big
	// tool-call argument blobs from o1/gpt-5; in that case Scan() returns
	// false with bufio.ErrTooLong. The post-loop scanner.Err() check
	// surfaces such truncations as an EventError instead of pretending the
	// stream ended cleanly.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	sendAll := func(events []api.StreamEvent) bool {
		for _, ev := range events {
			if !send(ev) {
				return false
			}
		}
		return true
	}

	type pendingText struct {
		blockIndex int
		started    bool
		closed     bool
	}

	var (
		nextBlockIndex = 0
		// Map keyed by message-item id to track per-text-block state. The
		// responses stream may interleave multiple message items with
		// function_call items; each new message item gets its own block
		// index so deltas from a later item don't collapse into the first
		// text block. Multiple text blocks may be open simultaneously —
		// each one is closed only by its own response.output_text.done
		// event or by the post-loop sweep.
		textByItem = make(map[string]*pendingText)
		// Map keyed by item_id (function_call output item id) to track
		// per-tool-call state. Some events use "item_id" directly, others
		// reach us via the Item embedded in output_item.added.
		fnByItem = make(map[string]*sseutil.ToolCallAccumulator)
		// textOpenOrder records the order text blocks were opened in, so
		// the post-loop sweep emits content_block_stop in a deterministic
		// (open-order) sequence rather than Go's randomised map order.
		textOpenOrder []string
		fnOpenOrder   []string
		stopReason    = "end_turn"
		outputTokens  int
	)

	closeText := func(itemID string) bool {
		if itemID == "" {
			return true
		}
		t := textByItem[itemID]
		if t == nil || !t.started || t.closed {
			return true
		}
		t.closed = true
		return send(api.StreamEvent{Type: api.EventContentBlockStop, Index: t.blockIndex})
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			if data == "[DONE]" {
				break
			}
			continue
		}

		var ev oaiResponsesEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "response.created":
			// Nothing to forward; message_start already emitted.

		case "response.output_item.added":
			if ev.Item == nil {
				continue
			}
			switch ev.Item.Type {
			case "function_call":
				// Each function_call item gets its own block index. Block
				// indices for sibling items (text, other tool calls,
				// reasoning) are independent — opening this block does
				// NOT close anything else. Text items remain open until
				// their own response.output_text.done.
				if _, exists := fnByItem[ev.Item.ID]; exists {
					continue
				}
				acc := sseutil.NewToolCallAccumulator(nextBlockIndex)
				nextBlockIndex++
				fnByItem[ev.Item.ID] = acc
				fnOpenOrder = append(fnOpenOrder, ev.Item.ID)
				if ev.Item.CallID != "" && ev.Item.Name != "" {
					if !send(acc.MarkStarted(ev.Item.CallID, ev.Item.Name)) {
						return
					}
				}
				stopReason = "tool_use"
			case "message":
				// Reserve a block index for this message item. The
				// content_block_start is emitted on the first delta, and
				// the block stays open until its own
				// response.output_text.done.
				if _, exists := textByItem[ev.Item.ID]; !exists {
					textByItem[ev.Item.ID] = &pendingText{blockIndex: nextBlockIndex}
					nextBlockIndex++
				}
			case "reasoning":
				// reasoning items are not surfaced as content blocks
			}

		case "response.output_text.delta":
			itemID := ev.ItemID
			t, ok := textByItem[itemID]
			if !ok {
				// Fallback: some streams may emit text deltas without a
				// preceding output_item.added for the message. Allocate a
				// block on demand so deltas aren't dropped.
				t = &pendingText{blockIndex: nextBlockIndex}
				nextBlockIndex++
				textByItem[itemID] = t
			}
			if t.closed {
				// The item's own done event already fired; ignore late deltas.
				continue
			}
			if !t.started {
				t.started = true
				textOpenOrder = append(textOpenOrder, itemID)
				if !send(api.StreamEvent{
					Type:         api.EventContentBlockStart,
					Index:        t.blockIndex,
					ContentBlock: api.ContentBlockInfo{Type: "text", Index: t.blockIndex},
				}) {
					return
				}
			}
			if ev.Delta != "" {
				if !send(api.StreamEvent{
					Type:  api.EventContentBlockDelta,
					Index: t.blockIndex,
					Delta: api.Delta{Type: "text_delta", Text: ev.Delta},
				}) {
					return
				}
			}

		case "response.output_text.done":
			// Close this specific text block. Other text blocks (from
			// concurrent message items) remain open until their own done
			// event or the post-loop sweep.
			if !closeText(ev.ItemID) {
				return
			}

		case "response.function_call_arguments.delta":
			acc := fnByItem[ev.ItemID]
			if acc == nil {
				continue
			}
			if !sendAll(acc.HandleDelta("", "", ev.Delta)) {
				return
			}

		case "response.function_call_arguments.done":
			// Final arguments are also delivered as deltas; nothing extra
			// to emit. The block is closed in the post-loop sweep.

		case "response.completed":
			if ev.Response != nil && ev.Response.Usage != nil {
				outputTokens = ev.Response.Usage.OutputTokens
			}
			// stopReason is the source-of-truth set when each output_item
			// was observed: "tool_use" the moment a function_call item
			// landed, otherwise the initial "end_turn". No recompute here.
		}
	}

	// Surface scanner errors (bufio.ErrTooLong on oversize SSE lines, read
	// failures, etc.) as an explicit EventError. Without this check, the
	// caller would see a clean MessageStop and silently commit a truncated
	// or partial response — a real source of data corruption in production.
	if err := scanner.Err(); err != nil {
		send(api.StreamEvent{
			Type:         api.EventError,
			ErrorMessage: fmt.Sprintf("openai responses stream read: %v", err),
		})
		return
	}

	// Close any text blocks that were opened but not explicitly closed
	// by a response.output_text.done event, in the order they were opened.
	for _, id := range textOpenOrder {
		t := textByItem[id]
		if t == nil || !t.started || t.closed {
			continue
		}
		t.closed = true
		if !send(api.StreamEvent{Type: api.EventContentBlockStop, Index: t.blockIndex}) {
			return
		}
	}
	for _, id := range fnOpenOrder {
		acc := fnByItem[id]
		if acc == nil {
			continue
		}
		if !sendAll(acc.Finish()) {
			return
		}
	}

	if !send(api.StreamEvent{
		Type:       api.EventMessageDelta,
		StopReason: stopReason,
		Usage:      api.UsageDelta{OutputTokens: outputTokens},
	}) {
		return
	}
	if !send(api.StreamEvent{Type: api.EventMessageStop}) {
		return
	}
}
