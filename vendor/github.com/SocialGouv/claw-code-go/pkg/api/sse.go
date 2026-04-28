package api

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
)

// SseParser is a byte-buffer state machine for parsing Server-Sent Events.
type SseParser = api.SseParser

// SseFrame is a raw parsed SSE frame before JSON deserialization.
type SseFrame = api.SseFrame

// NewSseParser creates a new SSE parser with an empty buffer.
func NewSseParser() *SseParser {
	return api.NewSseParser()
}
