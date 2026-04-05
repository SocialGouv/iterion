package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"
)

// Agent runs an agentic loop: generate → tool calls → re-generate, with
// streaming events, permission hooks, session persistence, and compaction.
type Agent struct {
	cfg     Config
	session *Session
	toolMap map[string]goai.Tool
}

// New creates a new Agent. If cfg.SessionID is set and cfg.SessionDir is
// non-empty, it resumes the existing session from disk.
func New(cfg Config) (*Agent, error) {
	cfg.applyDefaults()

	a := &Agent{
		cfg:     cfg,
		toolMap: buildToolMap(cfg.Tools),
	}

	// Load or create session.
	if cfg.SessionID != "" && cfg.SessionDir != "" {
		sess, err := loadSession(cfg.SessionDir, cfg.SessionID)
		if err != nil {
			return nil, fmt.Errorf("agent: resume session: %w", err)
		}
		a.session = sess
	} else {
		a.session = newSession()
	}

	return a, nil
}

// SessionID returns the current session identifier.
func (a *Agent) SessionID() string {
	return a.session.ID
}

// Session returns the current session (for inspection or external persistence).
func (a *Agent) Session() *Session {
	return a.session
}

// Close persists the session to disk if a SessionDir is configured.
func (a *Agent) Close() error {
	return a.session.save(a.cfg.SessionDir)
}

// Run executes the agent loop synchronously and returns the final result.
func (a *Agent) Run(ctx context.Context, prompt string) (*Result, error) {
	return a.runLoop(ctx, prompt, nil)
}

// RunStreaming executes the agent loop, emitting events to the provided channel.
// The channel is NOT closed by this method; the caller should close it after
// this returns.
func (a *Agent) RunStreaming(ctx context.Context, prompt string, events chan<- Event) (*Result, error) {
	return a.runLoop(ctx, prompt, events)
}

// runLoop is the core agentic loop shared by Run and RunStreaming.
func (a *Agent) runLoop(ctx context.Context, prompt string, events chan<- Event) (*Result, error) {
	// Append user message.
	a.session.Messages = append(a.session.Messages, provider.Message{
		Role: provider.RoleUser,
		Content: []provider.Part{
			{Type: provider.PartText, Text: prompt},
		},
	})

	var (
		allText      strings.Builder
		steps        []StepInfo
		totalUsage   provider.Usage
		lastTokens   int
	)

	for step := 1; a.cfg.MaxSteps == 0 || step <= a.cfg.MaxSteps; step++ {
		// Check for compaction before each step.
		if shouldCompact(lastTokens, a.session.Messages, a.cfg.CompactionMaxTokens, a.cfg.CompactionThreshold) {
			if err := compact(ctx, a.cfg.Model, a.session, a.cfg.CompactionKeepRecent); err != nil {
				// Non-fatal: log via event if streaming, otherwise ignore.
				if events != nil {
					trySendEvent(ctx, events, Event{
						Type: EventError,
						Err:  fmt.Errorf("compaction warning: %w", err),
					})
				}
			} else {
				// Prepend continuation marker.
				cont := continuationMessage(a.session.CompactionSummary)
				a.session.Messages = append([]provider.Message{cont}, a.session.Messages...)
			}
		}

		// Build generation parameters.
		params := a.buildParams()

		// Stream from the model.
		streamResult, err := a.cfg.Model.DoStream(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("agent: step %d stream: %w", step, err)
		}

		// Consume the stream, collecting text and tool calls.
		stepText, toolCalls, usage, err := a.consumeStream(ctx, streamResult.Stream, events)
		if err != nil {
			return nil, fmt.Errorf("agent: step %d consume: %w", step, err)
		}

		lastTokens = usage.InputTokens
		allText.WriteString(stepText)

		// Build and append assistant message.
		assistantMsg := buildAssistantMessage(stepText, toolCalls)
		a.session.Messages = append(a.session.Messages, assistantMsg)

		stepInfo := StepInfo{
			Number:       step,
			Text:         stepText,
			ToolCalls:    toolCalls,
			Usage:        usage,
			FinishReason: finishReasonFromToolCalls(toolCalls),
		}
		steps = append(steps, stepInfo)
		totalUsage = addUsage(totalUsage, usage)

		// Notify hooks.
		if a.cfg.OnUsage != nil {
			a.cfg.OnUsage(usage)
		}
		if a.cfg.OnStepFinish != nil {
			a.cfg.OnStepFinish(stepInfo)
		}
		if events != nil {
			trySendEvent(ctx, events, Event{Type: EventStepFinish, Step: stepInfo, Usage: usage})
		}

		// If no tool calls, we're done.
		if len(toolCalls) == 0 {
			break
		}

		// Execute tool calls.
		toolMessages, err := a.executeToolCalls(ctx, toolCalls, events)
		if err != nil {
			return nil, fmt.Errorf("agent: step %d tools: %w", step, err)
		}
		a.session.Messages = append(a.session.Messages, toolMessages...)
	}

	// Update session usage.
	a.session.TotalUsage = addUsage(a.session.TotalUsage, totalUsage)
	a.session.TotalTurns++

	// Persist session.
	if err := a.session.save(a.cfg.SessionDir); err != nil {
		// Non-fatal.
		if events != nil {
			trySendEvent(ctx, events, Event{Type: EventError, Err: err})
		}
	}

	if events != nil {
		trySendEvent(ctx, events, Event{Type: EventUsage, Usage: totalUsage})
		trySendEvent(ctx, events, Event{Type: EventDone})
	}

	return &Result{
		Text:      allText.String(),
		Usage:     totalUsage,
		SessionID: a.session.ID,
		Steps:     steps,
	}, nil
}

// buildParams constructs GenerateParams from the current session state.
func (a *Agent) buildParams() provider.GenerateParams {
	var tools []provider.ToolDefinition
	for _, t := range a.cfg.Tools {
		tools = append(tools, provider.ToolDefinition{
			Name:                   t.Name,
			Description:            t.Description,
			InputSchema:            t.InputSchema,
			ProviderDefinedType:    t.ProviderDefinedType,
			ProviderDefinedOptions: t.ProviderDefinedOptions,
		})
	}

	return provider.GenerateParams{
		Messages:        a.session.Messages,
		System:          a.cfg.SystemPrompt,
		Tools:           tools,
		MaxOutputTokens: a.cfg.MaxOutputTokens,
		Headers:         a.cfg.Headers,
		ProviderOptions: a.cfg.ProviderOptions,
		PromptCaching:   a.cfg.PromptCaching,
	}
}

// consumeStream reads from the stream channel, accumulating text and tool calls.
// It emits EventTextDelta events if streaming.
func (a *Agent) consumeStream(ctx context.Context, stream <-chan provider.StreamChunk, events chan<- Event) (string, []provider.ToolCall, provider.Usage, error) {
	var (
		text      strings.Builder
		toolCalls []provider.ToolCall
		usage     provider.Usage
	)

	for chunk := range stream {
		switch chunk.Type {
		case provider.ChunkText:
			text.WriteString(chunk.Text)
			if a.cfg.OnTextDelta != nil {
				a.cfg.OnTextDelta(chunk.Text)
			}
			if events != nil {
				if !trySendEvent(ctx, events, Event{Type: EventTextDelta, Text: chunk.Text}) {
					return text.String(), toolCalls, usage, ctx.Err()
				}
			}

		case provider.ChunkToolCall:
			toolCalls = append(toolCalls, provider.ToolCall{
				ID:    chunk.ToolCallID,
				Name:  chunk.ToolName,
				Input: json.RawMessage(chunk.ToolInput),
			})

		case provider.ChunkFinish:
			usage = chunk.Usage

		case provider.ChunkError:
			if chunk.Error != nil {
				return text.String(), toolCalls, usage, chunk.Error
			}
		}
	}

	return text.String(), toolCalls, usage, nil
}

// executeToolCalls runs each tool call with permission checks and returns
// tool result messages.
func (a *Agent) executeToolCalls(ctx context.Context, calls []provider.ToolCall, events chan<- Event) ([]provider.Message, error) {
	var msgs []provider.Message

	for _, tc := range calls {
		// Permission check.
		if a.cfg.OnPermissionCheck != nil {
			decision := a.cfg.OnPermissionCheck(tc.Name, tc.Input)

			switch decision {
			case PermDeny:
				msgs = append(msgs, goai.ToolMessage(tc.ID, tc.Name, "Permission denied"))
				if a.cfg.OnToolDone != nil {
					a.cfg.OnToolDone(tc.Name, "Permission denied", true)
				}
				if events != nil {
					trySendEvent(ctx, events, Event{
						Type: EventToolDone, ToolName: tc.Name,
						ToolResult: "Permission denied", ToolIsError: true,
					})
				}
				continue

			case PermAsk:
				if events != nil {
					replyCh := make(chan PermissionDecision, 1)
					if !trySendEvent(ctx, events, Event{
						Type: EventPermissionAsk, ToolName: tc.Name,
						ToolInput: tc.Input, PermReply: replyCh,
					}) {
						return msgs, ctx.Err()
					}
					// Wait for decision.
					select {
					case d := <-replyCh:
						if d == PermDeny {
							msgs = append(msgs, goai.ToolMessage(tc.ID, tc.Name, "Permission denied"))
							if a.cfg.OnToolDone != nil {
								a.cfg.OnToolDone(tc.Name, "Permission denied", true)
							}
							continue
						}
					case <-ctx.Done():
						return msgs, ctx.Err()
					}
				}
			}
		}

		// Emit tool start.
		if a.cfg.OnToolStart != nil {
			a.cfg.OnToolStart(tc.Name, tc.Input)
		}
		if events != nil {
			trySendEvent(ctx, events, Event{Type: EventToolStart, ToolName: tc.Name, ToolInput: tc.Input})
		}

		// Execute the tool.
		tool, ok := a.toolMap[tc.Name]
		if !ok {
			errMsg := fmt.Sprintf("error: unknown tool %q", tc.Name)
			msgs = append(msgs, goai.ToolMessage(tc.ID, tc.Name, errMsg))
			if a.cfg.OnToolDone != nil {
				a.cfg.OnToolDone(tc.Name, errMsg, true)
			}
			if events != nil {
				trySendEvent(ctx, events, Event{Type: EventToolDone, ToolName: tc.Name, ToolResult: errMsg, ToolIsError: true})
			}
			continue
		}

		output, err := tool.Execute(ctx, tc.Input)
		isError := err != nil
		result := output
		if err != nil {
			result = "error: " + err.Error()
		}

		msgs = append(msgs, goai.ToolMessage(tc.ID, tc.Name, result))

		if a.cfg.OnToolDone != nil {
			a.cfg.OnToolDone(tc.Name, result, isError)
		}
		if events != nil {
			trySendEvent(ctx, events, Event{Type: EventToolDone, ToolName: tc.Name, ToolResult: result, ToolIsError: isError})
		}
	}

	return msgs, nil
}

// buildAssistantMessage constructs an assistant message from text and tool calls.
func buildAssistantMessage(text string, toolCalls []provider.ToolCall) provider.Message {
	var parts []provider.Part
	if text != "" {
		parts = append(parts, provider.Part{Type: provider.PartText, Text: text})
	}
	for _, tc := range toolCalls {
		parts = append(parts, provider.Part{
			Type:       provider.PartToolCall,
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			ToolInput:  tc.Input,
		})
	}
	return provider.Message{Role: provider.RoleAssistant, Content: parts}
}

// buildToolMap creates a name→Tool lookup.
func buildToolMap(tools []goai.Tool) map[string]goai.Tool {
	m := make(map[string]goai.Tool, len(tools))
	for _, t := range tools {
		if t.Execute != nil {
			m[t.Name] = t
		}
	}
	return m
}

// finishReasonFromToolCalls returns the finish reason based on tool calls.
func finishReasonFromToolCalls(toolCalls []provider.ToolCall) provider.FinishReason {
	if len(toolCalls) > 0 {
		return provider.FinishToolCalls
	}
	return provider.FinishStop
}

// addUsage adds b's counts to a.
func addUsage(a, b provider.Usage) provider.Usage {
	return provider.Usage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
		ReasoningTokens:  a.ReasoningTokens + b.ReasoningTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
	}
}

// trySendEvent sends an event to the channel, returning false if the context is cancelled.
func trySendEvent(ctx context.Context, events chan<- Event, event Event) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}
