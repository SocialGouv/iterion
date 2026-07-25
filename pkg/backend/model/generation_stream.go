package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

const (
	// defaultMaxSteps is the default tool-loop iteration limit.
	defaultMaxSteps = 10

	// defaultMaxTokens is the default max tokens per response.
	defaultMaxTokens = 8192

	// maxToolInputJSONSize caps the accumulated input_json_delta
	// fragments for a single tool_use block. A misbehaving provider
	// (or a malformed stream that never sends content_block_stop)
	// would otherwise grow the PartialJSON buffer without bound and
	// OOM the runner. 10 MB is well above any realistic tool input
	// while still cheap to fail loud on.
	maxToolInputJSONSize = 10 * 1024 * 1024

	// maxTextBlockSize caps the accumulated text_delta/thinking_delta
	// content for a single content block. Same rationale as
	// maxToolInputJSONSize: a misbehaving provider (or a malformed
	// stream that never sends content_block_stop) would otherwise grow
	// bs.text without bound and OOM the runner. 50 MB comfortably
	// covers even a very large extended-thinking transcript while
	// still cheap to fail loud on.
	maxTextBlockSize = 50 * 1024 * 1024
)

// ErrToolInputTooLarge signals that a streamed tool_use block's
// accumulated input JSON exceeded maxToolInputJSONSize.
var ErrToolInputTooLarge = errors.New("aggregateStream: tool_use input exceeded max size")

// ErrTextBlockTooLarge signals that a streamed text/thinking block's
// accumulated content exceeded maxTextBlockSize.
var ErrTextBlockTooLarge = errors.New("aggregateStream: text/thinking block exceeded max size")

// ---------------------------------------------------------------------------
// Stream aggregation
// ---------------------------------------------------------------------------

// toolUseBlock is a collected tool_use block from a streamed response.
type toolUseBlock struct {
	ID          string
	Name        string
	PartialJSON string // concatenated input_json_delta fragments
}

// aggregatedResponse is the result of consuming a StreamEvent channel.
type aggregatedResponse struct {
	text         string
	toolUses     []toolUseBlock
	usage        Usage
	stopReason   string
	thinkingText string // concatenated extended-thinking content (all thinking blocks)
	thinkingMs   int    // wall-clock spent inside thinking blocks (start→stop)
	err          error
}

// blockState tracks a single content block during stream aggregation.
type blockState struct {
	blockType     string // "text", "tool_use", or "thinking"
	text          string // text content, or thinking content for thinking blocks
	toolUse       toolUseBlock
	stopped       bool
	thinkingStart time.Time // when a thinking block opened (zero for non-thinking)
	thinkingMs    int       // finalized thinking duration (set on content_block_stop)
}

// growText appends delta to bs.text, enforcing maxTextBlockSize. Shared by
// the text_delta and thinking_delta cases so a misbehaving provider (or a
// malformed stream that never sends content_block_stop) can't grow either
// buffer without bound — the same protection input_json_delta already has.
func (bs *blockState) growText(delta string) error {
	if len(bs.text)+len(delta) > maxTextBlockSize {
		return fmt.Errorf("%w: %d bytes", ErrTextBlockTooLarge, maxTextBlockSize)
	}
	bs.text += delta
	return nil
}

// aggregateStream reads all events from ch and builds an aggregatedResponse.
// It tracks content blocks by index and concatenates deltas.
//
// On any early return, the upstream goroutine inside claw-code-go's
// StreamResponse is still trying to push the rest of the response into
// ch. If we return immediately, that goroutine blocks at the next send
// (ch is buffered ~64) and never releases the underlying TCP connection.
// A deferred drainer wraps every exit path so the upstream goroutine
// completes — the old code spawned a drainer only on the ctx-cancel
// branch and silently leaked the connection on tool-input-too-large or
// EventError early returns.
func aggregateStream(ctx context.Context, ch <-chan api.StreamEvent) aggregatedResponse {
	var res aggregatedResponse
	blocks := make(map[int]*blockState)
	drained := false
	sawStop := false
	defer func() {
		if drained {
			return
		}
		go func() {
			for range ch {
			}
		}()
	}()

	for {
		select {
		case <-ctx.Done():
			res.err = ctx.Err()
			return res
		case event, ok := <-ch:
			if !ok {
				drained = true
				res.text, res.toolUses, res.thinkingText, res.thinkingMs = collectBlocks(blocks)
				for _, bs := range blocks {
					if bs.blockType == "tool_use" && !bs.stopped {
						// Retryable: a truncated tool_use is a dropped
						// stream, not a permanent failure.
						res.err = &APIError{
							Message:     fmt.Sprintf("incomplete tool_use block: %s (content_block_stop not received)", bs.toolUse.Name),
							IsRetryable: true,
						}
						return res
					}
				}
				// Truncation backstop: a complete response always ends with a
				// message_delta carrying a stop_reason AND a message_stop. If
				// the channel closed with NEITHER terminal signal (and no
				// explicit stream error fired), the connection dropped
				// mid-stream. Surface a retryable error so the retry loop
				// re-issues the request instead of silently accepting a
				// truncated partial turn — which otherwise reads as a clean
				// but degenerate response (e.g. a reviewer's narration cut off
				// before it ever calls a tool).
				if res.err == nil && res.stopReason == "" && !sawStop {
					res.err = &APIError{
						Message:     "incomplete stream: connection closed before completion (no stop_reason or message_stop received)",
						IsRetryable: true,
					}
				}
				return res
			}

			switch event.Type {
			case api.EventMessageStart:
				res.usage.InputTokens = event.InputTokens
				res.usage.CacheReadTokens = event.CacheReadInputTokens
				res.usage.CacheWriteTokens = event.CacheCreationInputTokens

			case api.EventContentBlockStart:
				bs := &blockState{blockType: event.ContentBlock.Type}
				if event.ContentBlock.Type == "tool_use" {
					bs.toolUse = toolUseBlock{
						ID:   event.ContentBlock.ID,
						Name: event.ContentBlock.Name,
					}
				}
				if event.ContentBlock.Type == "thinking" {
					bs.thinkingStart = time.Now()
				}
				blocks[event.ContentBlock.Index] = bs

			case api.EventContentBlockDelta:
				bs, ok := blocks[event.Index]
				if !ok {
					bs = &blockState{blockType: "text"}
					blocks[event.Index] = bs
				}
				switch event.Delta.Type {
				case "text_delta":
					if growErr := bs.growText(event.Delta.Text); growErr != nil {
						res.err = growErr
						res.text, res.toolUses, res.thinkingText, res.thinkingMs = collectBlocks(blocks)
						return res
					}
				case "thinking_delta":
					if growErr := bs.growText(event.Delta.Thinking); growErr != nil {
						res.err = growErr
						res.text, res.toolUses, res.thinkingText, res.thinkingMs = collectBlocks(blocks)
						return res
					}
				case "signature_delta":
					// Signature signs the thinking block for cross-turn replay;
					// it carries no token/timing signal, so we ignore it here.
				case "input_json_delta":
					if len(bs.toolUse.PartialJSON)+len(event.Delta.PartialJSON) > maxToolInputJSONSize {
						res.err = fmt.Errorf("%w: tool %q exceeded %d bytes", ErrToolInputTooLarge, bs.toolUse.Name, maxToolInputJSONSize)
						res.text, res.toolUses, res.thinkingText, res.thinkingMs = collectBlocks(blocks)
						return res
					}
					bs.toolUse.PartialJSON += event.Delta.PartialJSON
				}

			case api.EventContentBlockStop:
				if bs, ok := blocks[event.Index]; ok {
					bs.stopped = true
					if bs.blockType == "thinking" && !bs.thinkingStart.IsZero() {
						bs.thinkingMs = int(time.Since(bs.thinkingStart) / time.Millisecond)
					}
				}

			case api.EventMessageDelta:
				res.usage.OutputTokens = event.Usage.OutputTokens
				// Exact billed thinking tokens (raw internal reasoning,
				// independent of thinking.display); 0 when the provider
				// omits the breakdown.
				res.usage.ReasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
				res.stopReason = event.StopReason

			case api.EventError:
				// Transport / truncation stream errors are classified
				// retryable so the retry loop re-issues the request; a
				// permanent provider error (quota, overflow) stays terminal.
				res.err = classifyStreamEventError(event.ErrorMessage)
				res.text, res.toolUses, res.thinkingText, res.thinkingMs = collectBlocks(blocks)
				return res

			case api.EventMessageStop:
				sawStop = true
			case api.EventPing:
				// No action needed.
			}
		}
	}
}

// collectBlocks extracts text, tool_use, and thinking blocks from the block
// state map, ordered by block index. It returns the concatenated visible text,
// the tool_use blocks, the concatenated thinking content, and the total
// wall-clock spent inside thinking blocks (milliseconds).
func collectBlocks(blocks map[int]*blockState) (string, []toolUseBlock, string, int) {
	if len(blocks) == 0 {
		return "", nil, "", 0
	}

	maxIdx := 0
	for idx := range blocks {
		if idx > maxIdx {
			maxIdx = idx
		}
	}

	var text string
	var toolUses []toolUseBlock
	var thinkingText string
	var thinkingMs int
	for i := 0; i <= maxIdx; i++ {
		bs, ok := blocks[i]
		if !ok {
			continue
		}
		switch bs.blockType {
		case "text":
			text += bs.text
		case "tool_use":
			toolUses = append(toolUses, bs.toolUse)
		case "thinking":
			thinkingText += bs.text
			// Duration is finalized on content_block_stop (persisted via the
			// *blockState pointer). A block that never stopped reports 0 — we
			// don't attribute stream-close latency to thinking.
			thinkingMs += bs.thinkingMs
		}
	}
	return text, toolUses, thinkingText, thinkingMs
}

// ---------------------------------------------------------------------------
// Finish reason mapping
// ---------------------------------------------------------------------------

// mapStopReason converts an Anthropic stop_reason string to a FinishReason.
func mapStopReason(reason string) FinishReason {
	switch reason {
	case "end_turn", "stop":
		return FinishStop
	case "tool_use":
		return FinishToolCalls
	case "max_tokens":
		return FinishLength
	case "content_filter":
		return FinishContentFilter
	default:
		return FinishOther
	}
}
