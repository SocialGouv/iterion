// Package boardops contains the capability-gated operations that the
// __mcp-board MCP server and the /api/v1/mcp/board HTTP handler share.
// Each operation takes a *native.Store, a granted capability set, and a
// JSON args blob, and returns either the JSON-encoded result or an error.
//
// The stdio and HTTP transports are thin wrappers around these operations:
// they handle JSON-RPC framing or HTTP request decoding, then call into
// this package. Keeping the logic here means a bug fix lands in one place.
package boardops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// Capability names. Use these constants instead of string literals so a
// typo at any call site becomes a compile error and the registry below
// (KnownCapabilities in pkg/dsl/ir) tracks the single source of truth.
const (
	CapBoardRead   = "board.read"
	CapBoardCreate = "board.create"
	CapBoardMove   = "board.move"
	CapBoardAssign = "board.assign"
	CapBoardLabel  = "board.label"
	CapBoardClose  = "board.close"
)

// Capabilities is a granted-cap set. Use NewCapabilities to parse a
// comma-separated env var.
type Capabilities map[string]bool

// NewCapabilities parses a comma-separated list of capability names and
// returns the corresponding set. Empty entries are ignored. Whitespace
// around each name is trimmed.
func NewCapabilities(csv string) Capabilities {
	caps := Capabilities{}
	for _, raw := range strings.Split(csv, ",") {
		name := strings.TrimSpace(raw)
		if name != "" {
			caps[name] = true
		}
	}
	return caps
}

// Has reports whether the named capability is granted.
func (c Capabilities) Has(name string) bool { return c[name] }

// ErrCapabilityDenied is returned when a granted-cap check fails.
var ErrCapabilityDenied = errors.New("capability denied")

// Tool describes one MCP-style tool exposed by the board. Description
// and InputSchema are JSON-encodable so the same struct serves both
// transports.
type Tool struct {
	Name        string          `json:"name"`
	Capability  string          `json:"capability"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// allTools is the sorted-by-name singleton consulted by every Tools(),
// ToolsFor(), and Call() invocation. Building it once eliminates the
// per-call slice allocation that ToolsFor used to pay and the linear
// scan Call used to perform.
var allTools = []Tool{
	{
		Name:        "assign_issue",
		Capability:  CapBoardAssign,
		Description: "Set the assignee on an issue.",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{
            "id":{"type":"string"},
            "assignee":{"type":"string","description":"Bot or user handle. Empty string clears the assignee."}
          },
          "required":["id","assignee"]
        }`),
	},
	{
		Name:        "close_issue",
		Capability:  CapBoardClose,
		Description: "Transition an issue to a terminal state. Defaults to the first terminal state on the board.",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{
            "id":{"type":"string"},
            "to":{"type":"string","description":"Optional explicit terminal state."}
          },
          "required":["id"]
        }`),
	},
	{
		Name:        "create_issue",
		Capability:  CapBoardCreate,
		Description: "Create a new issue on the native kanban board. Returns the created issue.",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{
            "title":{"type":"string","description":"Short title (required)."},
            "body":{"type":"string","description":"Markdown body (optional)."},
            "state":{"type":"string","description":"Initial state name (default: first state of the board)."},
            "labels":{"type":"array","items":{"type":"string"}},
            "priority":{"type":"integer","description":"Higher = more important. Default 0."},
            "assignee":{"type":"string","description":"Bot or user handle this issue is assigned to."},
            "blockers":{"type":"array","items":{"type":"string"},"description":"IDs of issues that must be terminal before this one is eligible."},
            "fields":{"type":"object","description":"Custom board fields (validated against board schema)."}
          },
          "required":["title"]
        }`),
	},
	{
		Name:        "get_issue",
		Capability:  CapBoardRead,
		Description: "Fetch one issue by ID or unambiguous prefix.",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{"id":{"type":"string"}},
          "required":["id"]
        }`),
	},
	{
		Name:        "list_issues",
		Capability:  CapBoardRead,
		Description: "List issues with optional filters.",
		// `required: []` is intentional: OpenAI's strict function-call
		// mode validates the schema and rejects "required" being absent
		// with "None is not of type 'array'". An empty array means
		// "no required fields" and is the correct shape.
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{
            "state":{"type":"string"},
            "label":{"type":"string"},
            "assignee":{"type":"string"}
          },
          "required":[]
        }`),
	},
	{
		Name:       "list_labels",
		Capability: CapBoardRead,
		Description: "List every distinct label currently on the board with usage count and last-used timestamp. " +
			"Sorted by count descending. Use this BEFORE assigning labels to new issues so " +
			"you reuse the operator-established vocabulary instead of inventing parallel names " +
			"(e.g. discovering an `epic:battle-tested` already exists instead of inventing " +
			"`source:battle-tested-plan-2026-05-24`). See the iterion-label-vocabulary skill for " +
			"the canonical namespace conventions.",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{},
          "required":[]
        }`),
	},
	{
		Name:        "set_labels",
		Capability:  CapBoardLabel,
		Description: "Replace the label list on an issue.",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{
            "id":{"type":"string"},
            "labels":{"type":"array","items":{"type":"string"}}
          },
          "required":["id","labels"]
        }`),
	},
	{
		Name:        "transition_issue",
		Capability:  CapBoardMove,
		Description: "Move an issue to a different state. Accepts short ID prefixes.",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{
            "id":{"type":"string","description":"Issue ID or unambiguous prefix."},
            "to":{"type":"string","description":"Target state name."}
          },
          "required":["id","to"]
        }`),
	},
}

// toolByName is the O(1) lookup index for Call. Populated once at init.
var toolByName = func() map[string]*Tool {
	m := make(map[string]*Tool, len(allTools))
	for i := range allTools {
		m[allTools[i].Name] = &allTools[i]
	}
	return m
}()

// dispatchByName maps a tool name to its handler. Populated once at init
// so Call can dispatch in O(1).
var dispatchByName = map[string]func(*native.Store, json.RawMessage) (json.RawMessage, error){
	"create_issue":     doCreate,
	"transition_issue": doTransition,
	"assign_issue":     doAssign,
	"set_labels":       doSetLabels,
	"close_issue":      doClose,
	"list_issues":      doList,
	"list_labels":      doListLabels,
	"get_issue":        doGet,
}

// Tools returns the seven board tools, sorted by name. The slice is a
// defensive copy so callers can sort/filter without mutating package state.
func Tools() []Tool {
	out := make([]Tool, len(allTools))
	copy(out, allTools)
	return out
}

// ToolsFor returns the subset of Tools() the granted capability set unlocks.
// Order matches Tools() (sorted by name) so output is deterministic.
func ToolsFor(caps Capabilities) []Tool {
	out := make([]Tool, 0, len(allTools))
	for i := range allTools {
		if caps.Has(allTools[i].Capability) {
			out = append(out, allTools[i])
		}
	}
	return out
}

// Call dispatches a tool invocation. The result is a JSON-encoded value
// suitable for direct embedding in an MCP `content[0].text` field or an
// HTTP response body.
func Call(store *native.Store, caps Capabilities, name string, rawArgs json.RawMessage) (json.RawMessage, error) {
	t, ok := toolByName[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	if !caps.Has(t.Capability) {
		return nil, fmt.Errorf("%w: tool %q needs capability %q", ErrCapabilityDenied, name, t.Capability)
	}
	return dispatchByName[name](store, rawArgs)
}

// ---------------------------------------------------------------------------
// Operation implementations
// ---------------------------------------------------------------------------

func doCreate(store *native.Store, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Title    string         `json:"title"`
		Body     string         `json:"body"`
		State    string         `json:"state"`
		Labels   []string       `json:"labels"`
		Priority int            `json:"priority"`
		Assignee string         `json:"assignee"`
		Blockers []string       `json:"blockers"`
		Fields   map[string]any `json:"fields"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Title) == "" {
		return nil, errors.New("title is required")
	}
	iss, err := store.Create(native.Issue{
		Title:    args.Title,
		Body:     args.Body,
		State:    args.State,
		Labels:   args.Labels,
		Priority: args.Priority,
		Assignee: args.Assignee,
		Blockers: args.Blockers,
		Fields:   args.Fields,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(iss)
}

func doTransition(store *native.Store, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		ID string `json:"id"`
		To string `json:"to"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.ID == "" || args.To == "" {
		return nil, errors.New("id and to are required")
	}
	resolved, err := store.Resolve(args.ID)
	if err != nil {
		return nil, err
	}
	iss, err := store.SetState(resolved, args.To)
	if err != nil {
		return nil, err
	}
	return json.Marshal(iss)
}

func doAssign(store *native.Store, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		ID       string `json:"id"`
		Assignee string `json:"assignee"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.ID == "" {
		return nil, errors.New("id is required")
	}
	resolved, err := store.Resolve(args.ID)
	if err != nil {
		return nil, err
	}
	iss, err := store.Update(resolved, native.Patch{Assignee: &args.Assignee})
	if err != nil {
		return nil, err
	}
	return json.Marshal(iss)
}

func doSetLabels(store *native.Store, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		ID     string   `json:"id"`
		Labels []string `json:"labels"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.ID == "" {
		return nil, errors.New("id is required")
	}
	if args.Labels == nil {
		args.Labels = []string{}
	}
	resolved, err := store.Resolve(args.ID)
	if err != nil {
		return nil, err
	}
	iss, err := store.Update(resolved, native.Patch{Labels: &args.Labels})
	if err != nil {
		return nil, err
	}
	return json.Marshal(iss)
}

func doClose(store *native.Store, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		ID string `json:"id"`
		To string `json:"to"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.ID == "" {
		return nil, errors.New("id is required")
	}
	resolved, err := store.Resolve(args.ID)
	if err != nil {
		return nil, err
	}
	target := args.To
	if target == "" {
		// Find the first terminal state on the board.
		for _, st := range store.Board().States {
			if st.Terminal {
				target = st.Name
				break
			}
		}
		if target == "" {
			return nil, errors.New("board has no terminal state; specify 'to' explicitly")
		}
	} else {
		st := store.Board().StateByName(target)
		if st == nil {
			return nil, fmt.Errorf("unknown state %q", target)
		}
		if !st.Terminal {
			return nil, fmt.Errorf("state %q is not terminal", target)
		}
	}
	iss, err := store.SetState(resolved, target)
	if err != nil {
		return nil, err
	}
	return json.Marshal(iss)
}

func doList(store *native.Store, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		State    string `json:"state"`
		Label    string `json:"label"`
		Assignee string `json:"assignee"`
	}
	if len(raw) > 0 {
		if err := unmarshalArgs(raw, &args); err != nil {
			return nil, err
		}
	}
	filter := native.ListFilter{Assignee: args.Assignee}
	if args.State != "" {
		filter.States = []string{args.State}
	}
	if args.Label != "" {
		filter.Labels = []string{args.Label}
	}
	issues, err := store.List(filter)
	if err != nil {
		return nil, err
	}
	return json.Marshal(issues)
}

func doListLabels(store *native.Store, _ json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(store.AggregateLabels())
}

func doGet(store *native.Store, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.ID == "" {
		return nil, errors.New("id is required")
	}
	resolved, err := store.Resolve(args.ID)
	if err != nil {
		return nil, err
	}
	iss, err := store.Get(resolved)
	if err != nil {
		return nil, err
	}
	return json.Marshal(iss)
}

func unmarshalArgs(raw json.RawMessage, dest any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
