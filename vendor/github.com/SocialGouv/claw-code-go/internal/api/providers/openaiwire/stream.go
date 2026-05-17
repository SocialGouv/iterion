package openaiwire

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/api/sseutil"
)

// StreamEvents reads OpenAI Chat-Completions style SSE chunks from resp and
// emits api.StreamEvent values on ch in the Anthropic-shaped vocabulary the
// rest of claw consumes. The function takes ownership of the response body
// and the channel: it always closes both before returning.
//
// Behaviour matches the well-tested openai chat-completions translator:
//   - emits MessageStart up front
//   - opens a single text block at index 0 on the first content delta
//   - opens additional tool_use blocks at index (1+toolIndex) as tool deltas
//     arrive, accumulating id/name/arguments via sseutil.ToolCallAccumulator
//   - emits MessageDelta+MessageStop at end with the final stopReason and
//     output token count.
//
// The function does not surface input tokens because the public StreamEvent
// vocabulary used by claw treats them as informational and counts them via
// the provider-side request bookkeeping.
func StreamEvents(ctx context.Context, resp *http.Response, ch chan<- api.StreamEvent) {
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
	sendAll := func(events []api.StreamEvent) bool {
		for _, ev := range events {
			if !send(ev) {
				return false
			}
		}
		return true
	}

	// Emit a placeholder message start (token counts filled in at the end).
	if !send(api.StreamEvent{Type: api.EventMessageStart}) {
		return
	}

	// Use a 16 MiB scanner buffer. The default 64 KiB (and the prior 1 MiB
	// cap) is silently exceeded by large reasoning chunks and tool-call
	// argument blobs from o1/gpt-5; on overflow Scan() returns false with
	// bufio.ErrTooLong. The post-loop scanner.Err() check surfaces such
	// truncations as an EventError instead of a clean MessageStop with
	// partial content — a real source of silent data corruption.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	var (
		textStarted  bool
		toolCalls    = make(map[int]*sseutil.ToolCallAccumulator)
		finishReason string
		outputTokens int
	)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk Chunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		// Capture usage from the final usage chunk (choices will be empty
		// there). Only output tokens are surfaced via UsageDelta; input
		// tokens travel via the provider's own request bookkeeping and are
		// intentionally discarded here.
		if chunk.Usage != nil {
			outputTokens = chunk.Usage.CompletionTokens
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta

			// -- Text content delta --
			if delta.Content != nil && *delta.Content != "" {
				if !textStarted {
					textStarted = true
					if !send(api.StreamEvent{
						Type:         api.EventContentBlockStart,
						Index:        0,
						ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0},
					}) {
						return
					}
				}
				if !send(api.StreamEvent{
					Type:  api.EventContentBlockDelta,
					Index: 0,
					Delta: api.Delta{Type: "text_delta", Text: *delta.Content},
				}) {
					return
				}
			}

			// -- Tool call deltas --
			for _, tc := range delta.ToolCalls {
				idx := tc.Index
				acc, ok := toolCalls[idx]
				if !ok {
					// Reserve block index 1+idx so the text block at 0 is
					// always reachable.
					acc = sseutil.NewToolCallAccumulator(1 + idx)
					toolCalls[idx] = acc
				}
				if !sendAll(acc.HandleDelta(tc.ID, tc.Function.Name, tc.Function.Arguments)) {
					return
				}
			}

			// Remember finish reason for after the loop.
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
		}
	}

	// Surface scanner errors (bufio.ErrTooLong on oversize SSE lines, read
	// failures, etc.) as an explicit EventError. Without this check, the
	// caller would see a clean MessageStop and silently commit a truncated
	// or partial response.
	if err := scanner.Err(); err != nil {
		send(api.StreamEvent{
			Type:         api.EventError,
			ErrorMessage: fmt.Sprintf("openai stream read: %v", err),
		})
		return
	}

	// Close the text block.
	if textStarted {
		if !send(api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}) {
			return
		}
	}

	// Close all tool blocks in index order to keep things tidy.
	for i := 0; i < len(toolCalls); i++ {
		if acc, ok := toolCalls[i]; ok {
			if !sendAll(acc.Finish()) {
				return
			}
		}
	}

	// Map OpenAI finish_reason to our stop_reason vocabulary.
	stopReason := "end_turn"
	if finishReason == "tool_calls" {
		stopReason = "tool_use"
	}

	send(api.StreamEvent{
		Type:       api.EventMessageDelta,
		StopReason: stopReason,
		Usage:      api.UsageDelta{OutputTokens: outputTokens},
	})
	send(api.StreamEvent{Type: api.EventMessageStop})
}
