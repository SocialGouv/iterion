// Package elicitation provides typed structures for MCP elicitation handling.
package elicitation

import (
	"context"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/message"
)

// Mode identifies the elicitation UX expected by the CLI.
type Mode string

const (
	// ModeForm requests a form-style elicitation.
	ModeForm Mode = "form"
	// ModeURL requests a URL-based elicitation.
	ModeURL Mode = "url"
)

// Action identifies the SDK consumer's response to an elicitation request.
type Action string

const (
	// ActionAccept accepts the elicitation and optionally returns content.
	ActionAccept Action = "accept"
	// ActionDecline declines the elicitation.
	ActionDecline Action = "decline"
	// ActionCancel cancels the elicitation flow.
	ActionCancel Action = "cancel"
)

// Request contains an MCP elicitation request from the CLI.
type Request struct {
	MCPServerName   string
	Message         string
	Mode            *Mode
	URL             *string
	ElicitationID   *string
	RequestedSchema map[string]any
	ThreadID        string
	TurnID          *string
	Audit           *message.AuditEnvelope `json:"-"`
}

// Response contains the SDK consumer's elicitation decision.
type Response struct {
	Action  Action
	Content map[string]any
	Audit   *message.AuditEnvelope `json:"-"`
}

// Callback handles an MCP elicitation request.
type Callback func(ctx context.Context, req *Request) (*Response, error)
