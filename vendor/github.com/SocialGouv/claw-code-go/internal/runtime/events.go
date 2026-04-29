package runtime

// TurnEventType identifies the kind of streaming event from a conversation turn.
type TurnEventType int

const (
	TurnEventTextDelta     TurnEventType = iota // streaming text chunk
	TurnEventToolStart                          // tool execution starting
	TurnEventToolDone                           // tool execution complete
	TurnEventUsage                              // token usage update
	TurnEventDone                               // turn fully complete
	TurnEventError                              // error occurred
	TurnEventPermissionAsk                      // permission check required — send reply on PermReply
	TurnEventAskUser                            // agent needs user input — send reply on AskUserReply
)

// PermDecision is the user's response to a TurnEventPermissionAsk event.
type PermDecision int

const (
	PermDecisionAllowOnce   PermDecision = iota // allow this single invocation
	PermDecisionAllowAlways                     // allow this tool for the rest of the session
	PermDecisionDeny                            // deny execution
)

// TurnEvent carries information from a streaming API turn to the caller.
type TurnEvent struct {
	Type         TurnEventType
	Text         string            // TurnEventTextDelta: the chunk
	ToolName     string            // TurnEventToolStart/Done/PermissionAsk: tool name
	ToolInput    string            // TurnEventToolStart/PermissionAsk: brief input summary
	ToolResult   string            // TurnEventToolDone: output excerpt
	InputTokens  int               // TurnEventUsage/Done: input token count
	OutputTokens int               // TurnEventUsage/Done: output token count
	Err          error             // TurnEventError: the error
	PermReply    chan PermDecision // TurnEventPermissionAsk: caller sends decision here
	AskUserReply chan string       // TurnEventAskUser: caller sends user's answer here
}
