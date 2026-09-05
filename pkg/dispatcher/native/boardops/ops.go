// Package boardops contains the capability-gated operations that the
// __mcp-board MCP server and the /api/v1/mcp/board HTTP handler share.
// Each operation takes a native.BoardStore (the filesystem *native.Store or a
// cloud Mongo-backed store), a granted capability set, and a JSON args blob,
// and returns either the JSON-encoded result or an error.
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
	CapBoardRead    = "board.read"
	CapBoardCreate  = "board.create"
	CapBoardMove    = "board.move"
	CapBoardAssign  = "board.assign"
	CapBoardLabel   = "board.label"
	CapBoardClose   = "board.close"
	CapBoardComment = "board.comment"
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

// AllCapabilities returns every capability required by at least one board
// tool, deduplicated, in allTools order. Callers that want to register the
// full tool surface (per-node access is gated downstream) use this instead
// of hand-maintaining a list that drifts when a capability is added.
func AllCapabilities() []string {
	var names []string
	seen := map[string]bool{}
	for _, t := range allTools {
		if !seen[t.Capability] {
			seen[t.Capability] = true
			names = append(names, t.Capability)
		}
	}
	return names
}

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

// allTools is the sorted-by-name singleton consulted by every ToolsFor()
// and Call() invocation. Building it once eliminates the per-call slice
// allocation that ToolsFor used to pay and the linear scan Call used to
// perform.
var allTools = []Tool{
	{
		Name:       "add_labels",
		Capability: CapBoardLabel,
		Description: "Add labels to an issue, leaving every label already on it in place (idempotent: a label already present is left as is). " +
			"PREFER this over set_labels for any incremental change — set_labels REPLACES the whole list, so a list composed from an " +
			"earlier read re-arms one-shot trigger labels (e.g. triage:auto) that were consumed in between.",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{
            "id":{"type":"string","description":"Issue ID or unambiguous prefix."},
            "labels":{"type":"array","items":{"type":"string"},"description":"Labels to add. At least one."}
          },
          "required":["id","labels"]
        }`),
	},
	{
		Name:        "assign_issue",
		Capability:  CapBoardAssign,
		Description: "Set the human/ownership assignee on an issue. To choose which BOT processes an issue, use set_bot instead — the dispatcher routes by bot first, and an assignee is only used as a bot selector when no bot is set (the path external trackers like GitHub/Forgejo rely on).",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{
            "id":{"type":"string"},
            "assignee":{"type":"string","description":"Owner handle (person or team). Empty clears it. To pick the dispatching bot, prefer set_bot."}
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
		Name:        "comment_issue",
		Capability:  CapBoardComment,
		Description: "Append a comment to an issue's discussion thread. Use to leave a trail on an issue — e.g. post the URL of a merge/pull request a run opened, back onto the source issue.",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{
            "id":{"type":"string","description":"Issue ID or unambiguous prefix."},
            "body":{"type":"string","description":"Markdown comment body."},
            "author":{"type":"string","description":"Optional display name; defaults to the bot."}
          },
          "required":["id","body"]
        }`),
	},
	{
		Name:        "create_issue",
		Capability:  CapBoardCreate,
		Description: "Create a new issue on the native kanban board. Returns the created issue. When called from a planner run, parent_id/spawned_from is auto-stamped from the source ticket unless overridden.",
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
            "parent_id":{"type":"string","description":"Planner ticket that spawned this one (provenance). Auto-filled from the creating run's source issue when omitted."},
            "fields":{"type":"object","description":"Custom board fields (validated against board schema)."},
            "bot":{"type":"string","description":"CANONICAL bot that runs this issue when the dispatcher picks it up (e.g. feature-dev). The dispatcher routes by bot first, else assignee."},
            "bot_args":{"type":"object","additionalProperties":{"type":"string"},"description":"Per-ticket workflow var overrides (--var key=value) applied at launch. Use spawned_from for planner provenance (synced with parent_id)."}
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
		Name:       "remove_labels",
		Capability: CapBoardLabel,
		Description: "Remove the given labels from an issue, leaving every other label in place (a label not present is ignored). " +
			"Prefer it over set_labels for incremental changes — see add_labels.",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{
            "id":{"type":"string","description":"Issue ID or unambiguous prefix."},
            "labels":{"type":"array","items":{"type":"string"},"description":"Labels to remove. At least one."}
          },
          "required":["id","labels"]
        }`),
	},
	{
		Name:        "set_bot",
		Capability:  CapBoardAssign,
		Description: "Set the explicit bot (dispatcher workflow) for an issue. This is the CANONICAL way to choose which bot runs an issue — prefer it over assign_issue, which sets the human/ownership assignee. The dispatcher routes by bot first, else assignee. Empty string clears it (falls back to assignee-based routing).",
		InputSchema: json.RawMessage(`{
          "type":"object",
          "properties":{
            "id":{"type":"string"},
            "bot":{"type":"string","description":"Bot TECHNICAL name exactly as the catalog lists it (dash form, e.g. feature-dev or whole-improve-loop). Empty string clears it."}
          },
          "required":["id","bot"]
        }`),
	},
	{
		Name:       "set_labels",
		Capability: CapBoardLabel,
		Description: "Replace the label list on an issue (ABSOLUTE). Only for a full rewrite from a fresh read; to add or remove a few " +
			"labels use add_labels / remove_labels — a replacement built from an earlier read re-arms one-shot trigger labels " +
			"(e.g. triage:auto) consumed in between.",
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
var dispatchByName = map[string]func(native.BoardStore, json.RawMessage) (json.RawMessage, error){
	"comment_issue": doComment,
	// create_issue is dispatched via doCreate in CallWithEnv (needs CallEnv).
	"transition_issue": doTransition,
	"assign_issue":     doAssign,
	"set_bot":          doSetBot,
	"set_labels":       doSetLabels,
	"add_labels":       doAddLabels,
	"remove_labels":    doRemoveLabels,
	"close_issue":      doClose,
	"list_issues":      doList,
	"list_labels":      doListLabels,
	"get_issue":        doGet,
}

// ToolsFor returns the subset of allTools the granted capability set unlocks,
// sorted by name (allTools' own order) so output is deterministic.
func ToolsFor(caps Capabilities) []Tool {
	out := make([]Tool, 0, len(allTools))
	for i := range allTools {
		if caps.Has(allTools[i].Capability) {
			out = append(out, allTools[i])
		}
	}
	return out
}

// CallEnv carries ambient context for a boardops invocation (e.g. the
// source issue of the run that is calling create_issue).
type CallEnv struct {
	// SpawnParentID is the issue id of the ticket that owns the calling
	// run. Used to auto-stamp parent_id / bot_args.spawned_from on create
	// when the agent omits them. Empty = no auto parent.
	SpawnParentID string
}

// Call dispatches a tool invocation. The result is a JSON-encoded value
// suitable for direct embedding in an MCP `content[0].text` field or an
// HTTP response body.
func Call(store native.BoardStore, caps Capabilities, name string, rawArgs json.RawMessage) (json.RawMessage, error) {
	return CallWithEnv(store, caps, name, rawArgs, CallEnv{})
}

// CallWithEnv is Call with ambient spawn context (planner → child stamp).
func CallWithEnv(store native.BoardStore, caps Capabilities, name string, rawArgs json.RawMessage, env CallEnv) (json.RawMessage, error) {
	t, ok := toolByName[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	if !caps.Has(t.Capability) {
		return nil, fmt.Errorf("%w: tool %q needs capability %q", ErrCapabilityDenied, name, t.Capability)
	}
	if name == "create_issue" {
		return doCreate(store, rawArgs, env)
	}
	return dispatchByName[name](store, rawArgs)
}

// ---------------------------------------------------------------------------
// Operation implementations
// ---------------------------------------------------------------------------

func doCreate(store native.BoardStore, raw json.RawMessage, env CallEnv) (json.RawMessage, error) {
	var args struct {
		Title    string            `json:"title"`
		Body     string            `json:"body"`
		State    string            `json:"state"`
		Labels   []string          `json:"labels"`
		Priority int               `json:"priority"`
		Assignee string            `json:"assignee"`
		Blockers []string          `json:"blockers"`
		ParentID string            `json:"parent_id"`
		Fields   map[string]any    `json:"fields"`
		Bot      string            `json:"bot"`
		BotArgs  map[string]string `json:"bot_args"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Title) == "" {
		return nil, errors.New("title is required")
	}
	botArgs := args.BotArgs
	if botArgs == nil {
		botArgs = map[string]string{}
	} else {
		// Defensive copy so we don't mutate the caller's map.
		cp := make(map[string]string, len(botArgs)+1)
		for k, v := range botArgs {
			cp[k] = v
		}
		botArgs = cp
	}
	parentID := strings.TrimSpace(args.ParentID)
	if parentID == "" {
		parentID = strings.TrimSpace(botArgs[native.BotArgSpawnedFrom])
	}
	if parentID == "" {
		parentID = strings.TrimSpace(env.SpawnParentID)
	}
	if parentID != "" {
		botArgs[native.BotArgSpawnedFrom] = parentID
	}
	// Empty map → nil for cleaner JSON.
	if len(botArgs) == 0 {
		botArgs = nil
	}
	iss, err := store.Create(native.Issue{
		Title:    args.Title,
		Body:     args.Body,
		State:    args.State,
		Labels:   args.Labels,
		Priority: args.Priority,
		Assignee: args.Assignee,
		Blockers: args.Blockers,
		ParentID: parentID,
		Fields:   args.Fields,
		Bot:      args.Bot,
		BotArgs:  botArgs,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(iss)
}

func doComment(store native.BoardStore, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		ID     string `json:"id"`
		Body   string `json:"body"`
		Author string `json:"author"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.ID == "" || strings.TrimSpace(args.Body) == "" {
		return nil, errors.New("id and body are required")
	}
	resolved, err := store.Resolve(args.ID)
	if err != nil {
		return nil, err
	}
	author := args.Author
	if author == "" {
		author = "bot"
	}
	iss, _, err := store.AddComment(resolved, author, args.Body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(iss)
}

func doTransition(store native.BoardStore, raw json.RawMessage) (json.RawMessage, error) {
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

func doAssign(store native.BoardStore, raw json.RawMessage) (json.RawMessage, error) {
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

// doSetBot sets the issue's explicit bot — the canonical dispatcher
// workflow selector. Mirrors doAssign but targets the Bot field so a
// triage agent can express "run bot X" without conflating it with the
// human/ownership assignee.
func doSetBot(store native.BoardStore, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		ID  string `json:"id"`
		Bot string `json:"bot"`
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
	iss, err := store.Update(resolved, native.Patch{Bot: &args.Bot})
	if err != nil {
		return nil, err
	}
	return json.Marshal(iss)
}

func doSetLabels(store native.BoardStore, raw json.RawMessage) (json.RawMessage, error) {
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

// doAddLabels / doRemoveLabels are the RELATIVE label writes: the delta
// goes to the store's atomic AdjustLabels and is applied to the card as
// it is — never composed from a read the agent took earlier, which is how
// set_labels re-armed a consumed one-shot (issue #666).
func doAddLabels(store native.BoardStore, raw json.RawMessage) (json.RawMessage, error) {
	return adjustLabels(store, raw, true)
}

func doRemoveLabels(store native.BoardStore, raw json.RawMessage) (json.RawMessage, error) {
	return adjustLabels(store, raw, false)
}

func adjustLabels(store native.BoardStore, raw json.RawMessage, add bool) (json.RawMessage, error) {
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
	labels := native.CleanLabels(args.Labels)
	if len(labels) == 0 {
		return nil, errors.New("labels is required: at least one non-empty label")
	}
	resolved, err := store.Resolve(args.ID)
	if err != nil {
		return nil, err
	}
	var iss *native.Issue
	if add {
		iss, _, err = store.AdjustLabels(resolved, labels, nil)
	} else {
		iss, _, err = store.AdjustLabels(resolved, nil, labels)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(iss)
}

func doClose(store native.BoardStore, raw json.RawMessage) (json.RawMessage, error) {
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
		// A card ALREADY in a terminal state stays where it is: closing a
		// closed ticket is not a request to re-file it into a different
		// sink (the terminal→terminal carve-out is an operator refiling;
		// this is a bot surface, and re-filing here also erased the
		// dispatcher's give-up stamp on a card the bot never asked to
		// move). No-op, current issue returned.
		if cur, err := store.Get(resolved); err == nil {
			if st := store.Board().StateByName(cur.State); st != nil && st.Terminal {
				// Still an acknowledgment: the give-up stamp goes (same
				// best-effort contract as below — nothing moved).
				_ = store.SetGaveUp(resolved, nil)
				if refreshed, gerr := store.Get(resolved); gerr == nil {
					cur = refreshed
				}
				return json.Marshal(cur)
			}
		}
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
	filed, err := store.SetState(resolved, target)
	if err != nil {
		return nil, err
	}
	// Closing acknowledges the ticket, so any dispatcher give-up stamp on it
	// goes. A move expires the stamp by itself; closing a ticket the
	// dispatcher already filed into this same state does not move it, and
	// would leave the card in the pipeline board's needs-attention lane after
	// it was closed.
	// Best-effort: SetState has already committed, so raising here would tell
	// the agent a close that DID happen failed. A surviving stamp costs a
	// card in the wrong lane, not correctness — the same call the pipeline
	// board's Close makes.
	_ = store.SetGaveUp(resolved, nil)
	// Same reasoning for the re-read. On a cloud board this Get is a network
	// round-trip that can fail transiently, and the close has already landed:
	// degrade to the pre-clear snapshot rather than tell the agent a close
	// that DID happen failed (it would retry, or report the ticket open).
	iss := filed
	if refreshed, getErr := store.Get(resolved); getErr == nil {
		iss = refreshed
	}
	return json.Marshal(iss)
}

func doList(store native.BoardStore, raw json.RawMessage) (json.RawMessage, error) {
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

func doListLabels(store native.BoardStore, _ json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(store.AggregateLabels())
}

func doGet(store native.BoardStore, raw json.RawMessage) (json.RawMessage, error) {
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
