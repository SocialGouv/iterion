// Package types defines shared enum types used by both the AST and IR
// packages. These are stable value types that represent DSL concepts
// common to both the authoring surface and the execution model.
package types

// ---------------------------------------------------------------------------
// MCP Transport
// ---------------------------------------------------------------------------

// MCPTransport identifies the transport used by an MCP server.
type MCPTransport int

const (
	MCPTransportUnknown MCPTransport = iota
	MCPTransportStdio
	MCPTransportHTTP
	MCPTransportSSE
)

func (mt MCPTransport) String() string {
	switch mt {
	case MCPTransportStdio:
		return "stdio"
	case MCPTransportHTTP:
		return "http"
	case MCPTransportSSE:
		return "sse"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Field Type
// ---------------------------------------------------------------------------

// FieldType enumerates the V1 schema field types.
type FieldType int

const (
	FieldTypeString FieldType = iota
	FieldTypeBool
	FieldTypeInt
	FieldTypeFloat
	FieldTypeJSON
	FieldTypeStringArray
)

func (ft FieldType) String() string {
	switch ft {
	case FieldTypeString:
		return "string"
	case FieldTypeBool:
		return "bool"
	case FieldTypeInt:
		return "int"
	case FieldTypeFloat:
		return "float"
	case FieldTypeJSON:
		return "json"
	case FieldTypeStringArray:
		return "string[]"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Session Mode
// ---------------------------------------------------------------------------

// SessionMode represents the LLM session context strategy.
type SessionMode int

const (
	SessionFresh              SessionMode = iota // new context
	SessionInherit                               // inherit parent session — hard-requires _session_id
	SessionInheritIfAvailable                    // inherit when _session_id resolves, otherwise fall back to fresh (v0.6.0+)
	SessionArtifactsOnly                         // only persistent artifacts
	SessionFork                                  // non-consuming fork from parent session
)

func (sm SessionMode) String() string {
	switch sm {
	case SessionFresh:
		return "fresh"
	case SessionInherit:
		return "inherit"
	case SessionInheritIfAvailable:
		return "inherit_if_available"
	case SessionArtifactsOnly:
		return "artifacts_only"
	case SessionFork:
		return "fork"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Router Mode
// ---------------------------------------------------------------------------

// RouterMode represents the routing strategy.
type RouterMode int

const (
	RouterFanOutAll  RouterMode = iota // fan out to all targets
	RouterCondition                    // conditional routing
	RouterRoundRobin                   // round-robin: cycle through targets one at a time
	RouterLLM                          // LLM-based routing decision
	RouterFanOutEach                   // data-driven fan-out: one branch per element of a runtime array
)

func (rm RouterMode) String() string {
	switch rm {
	case RouterFanOutAll:
		return "fan_out_all"
	case RouterCondition:
		return "condition"
	case RouterRoundRobin:
		return "round_robin"
	case RouterLLM:
		return "llm"
	case RouterFanOutEach:
		return "fan_out_each"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Await Mode
// ---------------------------------------------------------------------------

// AwaitMode represents the convergence strategy when a node receives
// inputs from multiple parallel branches.
type AwaitMode int

const (
	AwaitNone       AwaitMode = iota // not a convergence point (or not explicitly set)
	AwaitWaitAll                     // wait for all incoming branches (default for convergence)
	AwaitBestEffort                  // proceed when possible, tolerate failures
)

func (am AwaitMode) String() string {
	switch am {
	case AwaitNone:
		return "none"
	case AwaitWaitAll:
		return "wait_all"
	case AwaitBestEffort:
		return "best_effort"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Interaction Mode
// ---------------------------------------------------------------------------

// InteractionMode controls how a node handles user interaction requests.
// It is available on agent, judge, and human nodes.
type InteractionMode int

const (
	InteractionNone       InteractionMode = iota // no interaction capability (default for agent/judge)
	InteractionHuman                             // always pause for human input (default for human nodes)
	InteractionLLM                               // LLM auto-answers interaction questions
	InteractionLLMOrHuman                        // LLM decides whether to answer or escalate to human
	InteractionReview                            // guided review-&-merge gate: companion-driven multi-turn dialogue ending in a squash-merge
	InteractionAsync                             // agent may post non-blocking questions (ask_user_async) and sync on demand (await_answers)
)

func (im InteractionMode) String() string {
	switch im {
	case InteractionNone:
		return "none"
	case InteractionHuman:
		return "human"
	case InteractionLLM:
		return "llm"
	case InteractionLLMOrHuman:
		return "llm_or_human"
	case InteractionReview:
		return "review"
	case InteractionAsync:
		return "async"
	default:
		return "unknown"
	}
}
