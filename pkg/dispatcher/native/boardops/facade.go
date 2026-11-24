package boardops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/boardcaps"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// facade.go — the board as a CONNECTOR (docs/adr/151, iterion#2834): a
// facade spec.Package published over the same operations allTools declares,
// served by the native action transport so a `tool … action: board.*` node
// executes in the engine with no model, no credential and no per-run token
// in the loop. This is the second use of the recipe
// docs/adr/098-connector-catalog.md wrote, with this package as the facade
// it named its template.
//
// The filing/verifying surface (#2388): reads (list/get) and the mutating
// verbs the filing half needs (create/label/transition), each gated by the
// capability the native tool already names — the bot's explicit
// `capabilities:` declaration IS the opt-in (#78=A), and A15's check-only
// stays the default: a node granted nothing reads nothing.
//
// EFFECT HONESTY of the mutating verbs, stated because the engine's generic
// recovery re-runs a failed node: every operation validates ALL of its
// arguments before its one store mutation, so a refusal raised there has no
// effect and keeps its ordinary recovery. The store mutation itself is not
// atomic with its error — both stores commit the card, then write the
// event, and a failure after the commit still returns an error — so a
// failed store write comes back typed UndecidedWriteError (write_outcome.go),
// which the engine's recovery reads as an ambiguous effect and never
// retries. The facade adds no idempotency machinery on top; the matrix test
// pins both halves against a store that fails before and after it writes.
//
// The two-way matrix test holds the facade to the registry: every exposed
// action maps to a native tool, and every native tool is exposed or exempted
// WITH its reason in nativeExemptions — one hole either way fails.

// ConnectorID is the id the facade and its transport answer to — the first
// segment of `board.issue.list`. Re-exported from the vocabulary leaf like
// the capability names above.
const ConnectorID = boardcaps.ConnectorID

// actionBindings maps an exposed facade action id to the native tool that
// serves it. The ids follow the connector convention
// `<connector>.<resource>.<verb>`; the tool names are allTools'.
var actionBindings = map[string]string{
	"board.issue.get":        "get_issue",
	"board.issue.list":       "list_issues",
	"board.issue.create":     "create_issue",
	"board.issue.label":      "add_labels",
	"board.issue.transition": "transition_issue",
}

// nativeExemptions records why a native tool is NOT exposed as an action.
// An entry without a reason is as much a defect as a missing one: the
// matrix test requires the text, because "not exposed" without the why is
// exactly the catalog hole ADR-098's generation report exists to prevent.
var nativeExemptions = map[string]string{
	// The filing/verifying verbs (#2388) are exposed; what stays out is the
	// board's administrative surface — a deterministic node transcribes a
	// plan, it does not govern the board. Each lands the day a workflow
	// asks for it, with its capability already named here.
	"remove_labels": "outside the filing/verifying scope (#2388) — exposed on demand, not by default",
	"set_labels":    "an ABSOLUTE label rewrite — the relative add_labels carries the facade's label verb; exposed on demand",
	"close_issue":   "board.close — closing is a lifecycle act no filing workflow needs; exposed on demand",
	"comment_issue": "board.comment — exposed on demand",
	"assign_issue":  "board.assign — dispatch governance, not filing; exposed on demand",
	"set_bot":       "board.assign — dispatch governance, not filing; exposed on demand",
	// Read, but not part of the filing/verifying surface the facade exists
	// for; exposing it costs one entry later if a workflow asks.
	"list_labels": "read, but outside the facade's filing/verifying scope (#2388) — exposed on demand, not by default",
}

// FacadePackage builds the connector package the native transport serves.
//
// Built in CODE, not loaded from a directory: the package describes
// operations this same binary implements, so a file on disk would be a
// second source of truth free to drift from allTools — the two-loaders
// defect ADR-098's round three closed. The matrix test is what holds this
// table to the registry instead.
func FacadePackage() *spec.Package {
	str := func(key, description string, required bool) spec.Param {
		return spec.Param{Key: key, Name: key, In: spec.InQuery, Type: "string", Required: required, Description: description}
	}
	strs := func(key, description string, required bool) spec.Param {
		return spec.Param{Key: key, Name: key, In: spec.InQuery, Type: "array", Items: "string", Required: required, Description: description}
	}
	ops := []spec.Operation{
		{
			ID:            "board.issue.get",
			Resource:      "issue",
			Verb:          "get",
			Summary:       "Fetch one issue by ID or unambiguous prefix",
			Deterministic: true,
			Effect:        spec.EffectRead,
			Anonymous:     true, // no credential: the grant is the node's capabilities, not a connection
			Params:        []spec.Param{str("id", "Issue ID or unambiguous prefix.", true)},
		},
		{
			ID:            "board.issue.list",
			Resource:      "issue",
			Verb:          "list",
			Summary:       "List issues with optional filters",
			Deterministic: true,
			Effect:        spec.EffectRead,
			Anonymous:     true,
			Params: []spec.Param{
				str("state", "Only issues in this state.", false),
				str("label", "Only issues carrying this label.", false),
				str("assignee", "Only issues assigned to this handle.", false),
			},
		},
		{
			ID:            "board.issue.create",
			Resource:      "issue",
			Verb:          "create",
			Summary:       "File one issue on the board",
			Deterministic: true,
			Effect:        spec.EffectCreate,
			Anonymous:     true,
			Params: []spec.Param{
				str("title", "Short title.", true),
				str("body", "Markdown body.", false),
				str("state", "Initial state name (default: first state of the board).", false),
				strs("labels", "Labels the card carries from creation.", false),
			},
		},
		{
			ID:            "board.issue.label",
			Resource:      "issue",
			Verb:          "label",
			Summary:       "Add labels to an issue, leaving every label already on it in place",
			Deterministic: true,
			Effect:        spec.EffectUpdate,
			Anonymous:     true,
			Params: []spec.Param{
				str("id", "Issue ID or unambiguous prefix.", true),
				strs("labels", "Labels to add. At least one.", true),
			},
		},
		{
			ID:            "board.issue.transition",
			Resource:      "issue",
			Verb:          "transition",
			Summary:       "Move an issue to a different state",
			Deterministic: true,
			Effect:        spec.EffectUpdate,
			Anonymous:     true,
			Params: []spec.Param{
				str("id", "Issue ID or unambiguous prefix.", true),
				str("to", "Target state name.", true),
			},
		},
	}
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion,
			ID:            ConnectorID,
			DisplayName:   "iterion native board",
			Description:   "The instance's own kanban board, served in process — the connector facade's native transport.",
			// Version tracks the FACADE, not the board: bumped when the
			// exposed operation set changes (0.2.0 = the mutating verbs,
			// #2834's second lot).
			Version:  "0.2.0",
			Maturity: spec.MaturityQualified,
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion,
			Connector:     ConnectorID,
			Domain:        "issue",
			Operations:    ops,
		}},
	}
}

// ActionTransport is the board's native action transport: it serves
// FacadePackage and executes each operation through CallWithEnv, so the
// capability gate, the argument validation and the read-side masking
// (IssueForRead) are the SAME code the stdio and HTTP transports run —
// a fix lands in one place, as this package's doc has always promised.
//
// It satisfies model.ActionTransport STRUCTURALLY, never by import: this
// package must not link the backend (iterion#2857), and Go's structural
// interfaces are exactly what makes the registration one-directional.
type ActionTransport struct {
	store native.BoardStore
	env   CallEnv
}

// NewActionTransport builds the transport over the instance's board store —
// the fs store a local run opens, the tenant's Mongo board a cloud runner
// resolves (#2465: always the instance's native store, never a forge's).
// env carries the planner provenance (SpawnParentID) board.issue.create
// stamps on the card it files; the read verbs never read it.
func NewActionTransport(store native.BoardStore, env CallEnv) *ActionTransport {
	return &ActionTransport{store: store, env: env}
}

// Connector is the id this transport owns.
func (t *ActionTransport) Connector() string { return ConnectorID }

// Package is the facade this transport serves.
func (t *ActionTransport) Package() *spec.Package { return FacadePackage() }

// Call executes one facade operation against the store.
//
// The capabilities arrive as the node's effective grant (its own declared
// list, else the workflow's — #78=A's single authorization surface) and are
// handed to CallWithEnv UNFILTERED: the per-tool gate below is the one every
// transport shares, and a second filter here would be the second regime the
// arbitration refuses. A node granted nothing is refused by that gate, in
// the same words an agent's MCP call hears.
//
// The signature matches model.ActionTransport.Call with no import of that
// package — spec.Operation, a map and a slice are all it takes.
func (t *ActionTransport) Call(ctx context.Context, op spec.Operation, params map[string]any, capabilities []string) (any, error) {
	toolName, ok := actionBindings[op.ID]
	if !ok {
		// Unreachable while Call is driven by the facade's own package — the
		// executor resolved the operation from it. A hand-built caller gets
		// told rather than dispatched to nothing.
		return nil, fmt.Errorf("boardops: facade operation %q has no native binding", op.ID)
	}
	caps := Capabilities{}
	for _, c := range capabilities {
		caps[strings.TrimSpace(c)] = true
	}
	// The HTTP executor refuses an argument the operation does not declare
	// before any byte leaves; the native path owes the same honesty, since
	// the store's decode below would otherwise DROP the key in silence and
	// answer a call the workflow never made. Declared keys come from the
	// facade's own operation, so the refusal lists what the author may
	// write — the one definition, like exec's.
	declared := make(map[string]bool, len(op.Params))
	for _, p := range op.Params {
		declared[p.Key] = true
	}
	for key := range params {
		if !declared[key] {
			accepted := make([]string, 0, len(op.Params))
			for _, p := range op.Params {
				accepted = append(accepted, p.Key)
			}
			return nil, fmt.Errorf("boardops: %s takes no parameter %q (declared: %s)", op.ID, key, strings.Join(accepted, ", "))
		}
	}
	rawArgs, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("boardops: encode %s arguments: %w", op.ID, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t.store == nil {
		// A transport built without a store is the admission placeholder's
		// shape (its facade Package is all that check reads). Reaching Call on
		// it is a wiring bug, said so rather than a nil dereference.
		return nil, fmt.Errorf("boardops: %s: the native board transport holds no store", op.ID)
	}
	raw, err := CallWithEnv(t.store, caps, toolName, rawArgs, t.env)
	if err != nil {
		return nil, err
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("boardops: decode %s result: %w", op.ID, err)
	}
	return data, nil
}
