package openaiwire

import (
	"bufio"
	"bytes"
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
	// A read that ends — EOF or error — in the middle of a line still hands
	// the scanner that line's head as a last token. unterminated marks it: it
	// is what a cut connection left, never a frame the server sent.
	var unterminated bool
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		advance, token, err := bufio.ScanLines(data, atEOF)
		unterminated = atEOF && token != nil && bytes.IndexByte(data[:advance], '\n') < 0
		return advance, token, err
	})

	// Guard against a stalled stream that delivers no bytes and no error,
	// parking scanner.Scan() until an outer deadline. The watchdog closes
	// resp.Body after idle silence (any line resets it via Touch); Fired()
	// below turns the resulting read error into a clear stalled-stream
	// message. See sseutil.IdleWatchdog.
	idle := sseutil.StreamIdleTimeout()
	wd := sseutil.NewIdleWatchdog(ctx, resp.Body, idle)
	defer wd.Stop()

	var (
		textStarted  bool
		toolCalls    = make(map[int]*sseutil.ToolCallAccumulator)
		finishReason string
		outputTokens int
		inputTokens  int
		sawUsage     bool
		sawDone      bool
		// cut: the connection dropped inside a line, which may have been a
		// later usage chunk — the usage seen is no final account. A drop
		// exactly at a line boundary leaves no trace in the bytes: the usage
		// seen then stands as reported.
		cut bool
	)
	// soFar is the usage an error event carries: what the stream had
	// reported before failing, which is no final account of the call.
	soFar := func() api.UsageDelta {
		return api.UsageDelta{OutputTokens: outputTokens, InputTokens: inputTokens}
	}

	// eventName is the current SSE event's `event:` field; a blank line ends
	// the event and resets it.
	var eventName string
	for scanner.Scan() {
		wd.Touch() // any received line (incl. comments/keepalives) = stream alive
		line := scanner.Text()
		if unterminated && !strings.HasPrefix(line, "data:") {
			// The connection dropped inside a line that carried no data.
			cut = true
			break
		}
		if line == "" {
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		// SSE spec (W3C, WHATWG) makes the space after `data:`
		// optional. Many OpenAI-compatible providers (Ollama, vLLM,
		// some proxies) emit `data:{...}` without the space; the
		// strict prefix used to silently drop those frames.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		// Prefix match, like the openai-python stream decoder.
		if strings.HasPrefix(data, "[DONE]") {
			sawDone = true
			break
		}

		if data == "" {
			if unterminated {
				cut = true
				break
			}
			continue // keepalive
		}

		// A failure reported inside the stream ends it: what came before is
		// partial, and a [DONE] after it must not read as a clean finish.
		// An event named "error" is one whatever its payload's shape; a data
		// field that is no chunk is one too, unless its event is named
		// otherwise (a server ping whose data is a timestamp).
		var chunk Chunk
		var failure string
		failed := false
		decoded := json.Unmarshal([]byte(data), &chunk) == nil
		if !decoded {
			if unterminated && !json.Valid([]byte(data)) && eventName != "error" &&
				(strings.HasPrefix(data, "{") || strings.HasPrefix("[DONE]", data)) {
				// The connection was cut inside a chunk or the [DONE]
				// sentinel: a truncated stream, reported below as one — not
				// an unparseable frame. A line no chunk can start with is
				// what the endpoint wrote, and an `event: error` the error
				// it announces, what came of its data included.
				cut = true
				break
			}
			switch eventName {
			case "error":
				failure, failed = ErrorEventMessage(data), true
			case "":
				failure, failed = UnparsedFrameError(data)
			}
			if !failed {
				continue
			}
		} else if failure, failed = chunk.ErrorMessage(); !failed && eventName == "error" {
			failure, failed = ErrorEventMessage(data), true
		}

		// Capture usage from the final usage chunk (choices will be empty
		// there). Both directions ride UsageDelta: this endpoint has no
		// message_start-shaped frame to carry the prompt count on, so the
		// terminal chunk is the only place it is ever reported. A usage
		// object naming no counter reports nothing.
		frameUsage := decoded && chunk.Usage.Counted()
		if frameUsage {
			outputTokens = chunk.Usage.CompletionTokens
			inputTokens = chunk.Usage.PromptTokens
			sawUsage = true
		}
		if failed {
			usage := soFar()
			usage.Reported = frameUsage
			send(api.StreamEvent{
				Type:         api.EventError,
				ErrorMessage: "openai stream error: " + failure,
				Usage:        usage,
			})
			return
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

	// A tripped idle watchdog closed resp.Body; surface it as a clear,
	// retryable stalled-stream error instead of the generic read error the
	// forced Close() produces. Checked before scanner.Err() since the
	// watchdog is the cause of that error here.
	if wd.Fired() {
		send(api.StreamEvent{
			Type:         api.EventError,
			ErrorMessage: fmt.Sprintf("openai stream stalled: no data for %s — aborting (retryable; tune via CLAW_STREAM_IDLE_TIMEOUT)", idle),
			Usage:        soFar(),
		})
		return
	}

	// Surface scanner errors (bufio.ErrTooLong on oversize SSE lines, read
	// failures, etc.) as an explicit EventError. Without this check, the
	// caller would see a clean MessageStop and silently commit a truncated
	// or partial response.
	if err := scanner.Err(); err != nil {
		send(api.StreamEvent{
			Type:         api.EventError,
			ErrorMessage: fmt.Sprintf("openai stream read: %v", err),
			Usage:        soFar(),
		})
		return
	}

	// A complete OpenAI stream ends with a finish_reason and/or the [DONE]
	// sentinel. If the scanner stopped cleanly (no read error above) yet we
	// saw NEITHER, the connection closed mid-response — surface a retryable
	// EventError instead of synthesizing the misleading "end_turn" stop
	// below, which would let the caller commit a truncated response as if
	// it were complete. (Providers that send [DONE] without a finish_reason,
	// or vice-versa, are NOT flagged — only the both-absent truncation is.)
	// A cut inside an `event: error` block is that error, whatever came of
	// its data.
	if cut && eventName == "error" {
		send(api.StreamEvent{
			Type:         api.EventError,
			ErrorMessage: "openai stream error: error event with no data",
			Usage:        soFar(),
		})
		return
	}

	if !sawDone && finishReason == "" {
		send(api.StreamEvent{
			Type:         api.EventError,
			ErrorMessage: "openai stream truncated: closed without finish_reason or [DONE]",
			Usage:        soFar(),
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
		Usage:      api.UsageDelta{OutputTokens: outputTokens, InputTokens: inputTokens, Reported: sawUsage && !cut},
	})
	send(api.StreamEvent{Type: api.EventMessageStop})
}
