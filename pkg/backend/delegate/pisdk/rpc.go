package pisdk

import "encoding/json"

// Ported from packages/coding-agent/src/modes/rpc/rpc-types.ts.
//
// pi's commands and responses are TypeScript discriminated unions; each is
// flattened here into one struct whose discriminant is Type, with the fields of
// other variants left zero. `omitempty` everywhere keeps the emitted JSON to
// exactly the variant's own keys, which matters because pi validates commands
// by shape.

// Command type discriminants.
const (
	// Prompting. The response to CmdPrompt fires at PREFLIGHT — it means
	// "accepted", not "finished". Completion is the EventAgentSettled event.
	CmdPrompt   = "prompt"
	CmdSteer    = "steer"
	CmdFollowUp = "follow_up"
	CmdAbort    = "abort"

	CmdNewSession = "new_session"
	CmdGetState   = "get_state"

	CmdSetModel            = "set_model"
	CmdGetAvailableModels  = "get_available_models"
	CmdSetThinkingLevel    = "set_thinking_level"
	CmdSetSteeringMode     = "set_steering_mode"
	CmdSetFollowUpMode     = "set_follow_up_mode"
	CmdCompact             = "compact"
	CmdSetAutoCompaction   = "set_auto_compaction"
	CmdSetAutoRetry        = "set_auto_retry"
	CmdAbortRetry          = "abort_retry"
	CmdBash                = "bash"
	CmdAbortBash           = "abort_bash"
	CmdGetSessionStats     = "get_session_stats"
	CmdSwitchSession       = "switch_session"
	CmdFork                = "fork"
	CmdClone               = "clone"
	CmdGetEntries          = "get_entries"
	CmdGetTree             = "get_tree"
	CmdGetLastAssistant    = "get_last_assistant_text"
	CmdSetSessionName      = "set_session_name"
	CmdGetMessages         = "get_messages"
	CmdGetCommands         = "get_commands"
	CmdExportHTML          = "export_html"
	CmdGetForkMessages     = "get_fork_messages"
	CmdCycleModel          = "cycle_model"
	CmdCycleThinkingLevel  = "cycle_thinking_level"
	CmdGetThinkingLevels   = "get_available_thinking_levels"
	CmdExtensionUIResponse = "extension_ui_response"
)

// Command is one line written to pi's stdin. ID correlates the response;
// commands are dispatched WITHOUT serialisation on pi's side, so responses can
// arrive out of order and must be matched by ID, never by arrival order.
type Command struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`

	// prompt / steer / follow_up
	Message string `json:"message,omitempty"`
	// prompt: "steer" | "followUp" — how a prompt arriving mid-run behaves.
	StreamingBehavior string `json:"streamingBehavior,omitempty"`

	// set_model
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"modelId,omitempty"`

	// set_thinking_level
	Level string `json:"level,omitempty"`

	// set_steering_mode / set_follow_up_mode: "all" | "one-at-a-time"
	Mode string `json:"mode,omitempty"`

	// set_auto_compaction / set_auto_retry
	Enabled *bool `json:"enabled,omitempty"`

	// compact
	CustomInstructions string `json:"customInstructions,omitempty"`

	// bash
	Command            string `json:"command,omitempty"`
	ExcludeFromContext *bool  `json:"excludeFromContext,omitempty"`

	// session ops
	ParentSession string `json:"parentSession,omitempty"`
	SessionPath   string `json:"sessionPath,omitempty"`
	EntryID       string `json:"entryId,omitempty"`
	Since         string `json:"since,omitempty"`
	Name          string `json:"name,omitempty"`
	OutputPath    string `json:"outputPath,omitempty"`
}

// Response is pi's reply to a Command.
type Response struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"` // always "response"
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// SessionState is the get_state payload.
type SessionState struct {
	Model               *ModelRef `json:"model,omitempty"`
	ThinkingLevel       string    `json:"thinkingLevel"`
	IsStreaming         bool      `json:"isStreaming"`
	IsCompacting        bool      `json:"isCompacting"`
	SteeringMode        string    `json:"steeringMode"`
	FollowUpMode        string    `json:"followUpMode"`
	SessionFile         string    `json:"sessionFile,omitempty"`
	SessionID           string    `json:"sessionId"`
	SessionName         string    `json:"sessionName,omitempty"`
	AutoCompactionOn    bool      `json:"autoCompactionEnabled"`
	MessageCount        int       `json:"messageCount"`
	PendingMessageCount int       `json:"pendingMessageCount"`
}

// ModelRef identifies a model in pi's catalogue.
type ModelRef struct {
	Provider      string `json:"provider"`
	ID            string `json:"id"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	Reasoning     bool   `json:"reasoning,omitempty"`
}

// SessionStats is the get_session_stats payload: the AUTHORITATIVE accounting
// for a session, superseding any per-message accumulation the caller did.
type SessionStats struct {
	Tokens struct {
		Input      int `json:"input"`
		Output     int `json:"output"`
		CacheRead  int `json:"cacheRead"`
		CacheWrite int `json:"cacheWrite"`
		Total      int `json:"total"`
	} `json:"tokens"`
	Cost float64 `json:"cost"`
	// ContextUsage is reported only when the model's context window is known.
	ContextUsage *struct {
		Tokens        int     `json:"tokens"`
		ContextWindow int     `json:"contextWindow"`
		Percentage    float64 `json:"percentage"`
	} `json:"contextUsage,omitempty"`
}

// UI request methods. An extension's ctx.ui.* call surfaces to the RPC client
// as one of these; the four dialog methods expect a UIResponse, while notify /
// setStatus / setWidget / setTitle / set_editor_text are fire-and-forget.
const (
	UIMethodSelect        = "select"
	UIMethodConfirm       = "confirm"
	UIMethodInput         = "input"
	UIMethodEditor        = "editor"
	UIMethodNotify        = "notify"
	UIMethodSetStatus     = "setStatus"
	UIMethodSetWidget     = "setWidget"
	UIMethodSetTitle      = "setTitle"
	UIMethodSetEditorText = "set_editor_text"
)

// EventExtensionUIRequest is the event type carrying a UIRequest.
const EventExtensionUIRequest = "extension_ui_request"

// UIRequest is an extension asking the client for input or telling it to
// display something. It is a SHARED channel: any loaded extension can emit on
// it, so a client that treats these as privileged must authenticate the
// payload rather than trusting the channel.
type UIRequest struct {
	Type   string `json:"type"` // "extension_ui_request"
	ID     string `json:"id"`
	Method string `json:"method"`

	Title       string   `json:"title,omitempty"`
	Message     string   `json:"message,omitempty"`
	Options     []string `json:"options,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Prefill     string   `json:"prefill,omitempty"`
	NotifyType  string   `json:"notifyType,omitempty"`
	TimeoutMS   int      `json:"timeout,omitempty"`

	StatusKey  string   `json:"statusKey,omitempty"`
	StatusText string   `json:"statusText,omitempty"`
	WidgetKey  string   `json:"widgetKey,omitempty"`
	WidgetLine []string `json:"widgetLines,omitempty"`
	Text       string   `json:"text,omitempty"`
}

// ExpectsReply reports whether this request blocks the extension until the
// client answers. Only the four dialog methods do.
func (r UIRequest) ExpectsReply() bool {
	switch r.Method {
	case UIMethodSelect, UIMethodConfirm, UIMethodInput, UIMethodEditor:
		return true
	default:
		return false
	}
}

// Prompt returns the human-facing text of the request, whichever field the
// method carries it in.
func (r UIRequest) Prompt() string {
	if r.Title != "" {
		return r.Title
	}
	return r.Message
}

// UIResponse answers a UIRequest. Exactly one of Value / Confirmed /
// Cancelled is meaningful, per method:
//
//	select | input | editor → Value
//	confirm                 → Confirmed
//	any                     → Cancelled (resolves the extension's call to the
//	                          safe default: undefined, or false for confirm)
type UIResponse struct {
	Type      string `json:"type"` // "extension_ui_response"
	ID        string `json:"id"`
	Value     string `json:"value,omitempty"`
	Confirmed *bool  `json:"confirmed,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// NewUIValue answers a select/input/editor request.
func NewUIValue(id, value string) UIResponse {
	return UIResponse{Type: "extension_ui_response", ID: id, Value: value}
}

// NewUIConfirm answers a confirm request.
func NewUIConfirm(id string, confirmed bool) UIResponse {
	return UIResponse{Type: "extension_ui_response", ID: id, Confirmed: &confirmed}
}

// NewUICancel declines a request, resolving the extension's call to its safe
// default. This is the correct answer to a request the client does not
// recognise — it neither blocks the agent nor fabricates an answer.
func NewUICancel(id string) UIResponse {
	return UIResponse{Type: "extension_ui_response", ID: id, Cancelled: true}
}

// boolPtr is a helper for the *bool command fields, where the zero value and
// "absent" must stay distinguishable.
func boolPtr(b bool) *bool { return &b }
