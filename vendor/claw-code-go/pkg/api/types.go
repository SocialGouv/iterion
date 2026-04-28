// Package api re-exports the internal/api surface via type aliases.
// External consumers (e.g., iterion) import this package; the ~50 internal
// consumers continue importing internal/api unchanged. Type aliases ensure
// identical types at compile time — no conversion needed.
package api

import (
	"claw-code-go/internal/api"
)

// CacheControlMarker is the Anthropic prompt caching marker.
type CacheControlMarker = api.CacheControlMarker

// ContentBlock represents a single content block in a message.
type ContentBlock = api.ContentBlock

// ToolResult is a convenience wrapper for building tool_result content blocks.
type ToolResult = api.ToolResult

// Message represents a single message in the conversation.
type Message = api.Message

// Tool describes a tool that can be called by the model.
type Tool = api.Tool

// InputSchema is the JSON schema for tool inputs.
type InputSchema = api.InputSchema

// Property is a single JSON schema property definition.
type Property = api.Property

// ToolChoice controls which tool the model must use.
type ToolChoice = api.ToolChoice

// CreateMessageRequest is the request body for /v1/messages.
type CreateMessageRequest = api.CreateMessageRequest

// StreamEventType enumerates the SSE event types.
type StreamEventType = api.StreamEventType

// Delta represents the delta portion of a content_block_delta event.
type Delta = api.Delta

// MessageDelta is the delta in a message_delta event.
type MessageDelta = api.MessageDelta

// UsageDelta contains token usage info.
type UsageDelta = api.UsageDelta

// ContentBlockInfo holds info about a starting content block.
type ContentBlockInfo = api.ContentBlockInfo

// StreamEvent is a parsed SSE event from the streaming API.
type StreamEvent = api.StreamEvent

const (
	EventMessageStart      = api.EventMessageStart
	EventContentBlockStart = api.EventContentBlockStart
	EventContentBlockDelta = api.EventContentBlockDelta
	EventContentBlockStop  = api.EventContentBlockStop
	EventMessageDelta      = api.EventMessageDelta
	EventMessageStop       = api.EventMessageStop
	EventError             = api.EventError
	EventPing              = api.EventPing
)

// EphemeralCacheControl returns a cache_control marker with type "ephemeral".
var EphemeralCacheControl = api.EphemeralCacheControl
