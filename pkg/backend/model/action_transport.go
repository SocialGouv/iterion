package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Native action transports (docs/adr/151, iterion#2834) — the seam that lets
// a `tool … action:` node execute IN THE ENGINE rather than over HTTP.
//
// pkg/connector/exec (HTTP) stays the default transport: an action whose
// connector no registration claims resolves and executes exactly as ADR-098
// wrote it. A registered transport OWNS its connector id — resolution and
// execution both — and the engine never branches on the id itself:
// registration IS the seam (amendment G110), so the next in-process
// connector adds a registration, never an engine `if`.
//
// The first registration is the board: boardops is the facade ADR-098 named
// "the template for a connector facade", and its operations already run in
// process over a native.BoardStore behind one capability-gated Call. What
// the seam adds is the third transport that store answers — beside the
// __mcp-board stdio child and the /api/v1/mcp/board handler — so a
// deterministic node reads (and, with the write verbs, files) the board
// with no model and no per-run token in the loop.

// ActionTransport executes one connector's operations in process.
//
// The interface is satisfied STRUCTURALLY: the implementation lives beside
// the store it serves (pkg/dispatcher/native/boardops for the board), which
// must not import this package — the dispatcher linking the whole backend
// is the packaging defect iterion#2857 filed. Go's structural satisfaction
// is what makes the registration one-directional.
type ActionTransport interface {
	// Connector is the id this transport owns — the first segment of every
	// action id it serves ("board" for board.issue.list). One transport per
	// id: registering a second is a wiring error, reported at registration.
	Connector() string
	// Package is the facade the transport serves: the operations a `.bot`
	// may name, each deterministic with its effect typed. The executor runs
	// the SAME usability checks on it as on an HTTP-resolved package
	// (checkOperationUsable), so a facade operation that is not marked
	// deterministic never executes.
	Package() *spec.Package
	// Call executes one operation. The arguments arrive rendered and coerced
	// to the types the operation declares; capabilities carry the node's
	// EFFECTIVE grant (its own `capabilities:` list when declared, else the
	// workflow's — #78=A's one rule). The transport applies the grant
	// itself — boardops.Call's caps.Has check is the ONE gate every board
	// transport shares, and a seam that re-gated beside it would be a
	// second authorization regime, which #78=A refuses.
	//
	// The arguments keep a secret's PLACEHOLDER, never its value: a native
	// transport serves one of iterion's own stores, which persists what it
	// receives and shows it to every reader — the form an agent's board MCP
	// call files. The `params` sink materialises for a vendor call, and a
	// native action makes none (docs/secrets.md).
	//
	// The signature is deliberately free of any type from THIS package:
	// spec.Operation, a map and a slice are all an implementation beside
	// its store needs, so structural satisfaction stays possible.
	Call(ctx context.Context, op spec.Operation, params map[string]any, capabilities []string) (any, error)
}

// WithActionTransports registers the native action transports this run
// executes in process. HTTP stays the default for everything unregistered;
// an action whose connector no transport owns and no catalog serves keeps
// its explicit refusal.
func WithActionTransports(transports ...ActionTransport) ClawExecutorOption {
	return func(e *ClawExecutor) {
		if e.actionTransports == nil {
			e.actionTransports = map[string]ActionTransport{}
		}
		for _, t := range transports {
			if t == nil {
				continue
			}
			// One owner per connector id: two registrations answering the
			// same id is a wiring ambiguity the run must not resolve by
			// arrival order. The FIRST registration wins and the duplicate is
			// logged rather than silently replacing an operator's choice
			// (philosophy 1) — the same posture the bot-source registry takes.
			if prev, dup := e.actionTransports[t.Connector()]; dup {
				e.logger.Warn("model: action transport for connector %q registered twice (%T already serves it) — the later %T is ignored",
					t.Connector(), prev, t)
				continue
			}
			e.actionTransports[t.Connector()] = t
		}
	}
}

// actionTransportFor answers which registered transport owns the connector
// an action id names, or nil — the HTTP path then stays the default.
func (e *ClawExecutor) actionTransportFor(action string) ActionTransport {
	if len(e.actionTransports) == 0 {
		return nil
	}
	connector, ok := cutActionConnector(action)
	if !ok {
		return nil
	}
	return e.actionTransports[connector]
}

// cutActionConnector splits the connector id off an action id. Shared by the
// transport lookup and the admission check so the two never parse the same
// string two ways.
func cutActionConnector(action string) (string, bool) {
	connector, rest, ok := strings.Cut(action, ".")
	return connector, ok && rest != ""
}

// executeToolNodeNativeAction runs an action on a registered native
// transport — the in-process counterpart of the HTTP path in this file.
//
// What is deliberately SHARED with the HTTP path: the policy gate, the
// operation-usability check, the parameter rendering/coercion, the timeout
// and the started/finish hooks, so a `.bot` cannot tell which transport
// served it by anything but the answer — and by a secret's form: the
// arguments keep its placeholder, since the store they reach keeps and
// shows them (ActionTransport.Call). The OUTPUT differs on purpose: the
// HTTP shape carries `{status, pending}` (and `{items, complete}` when the
// operation paginates) because a wire answer needs them; a native call has
// no status to report and the facade does not paginate, so the node renders
// `{data}` alone. What is deliberately NOT shared: the retry ladder. Its
// guards (Retryable, idempotency keys, ambiguous effects) are HTTP-answer
// semantics; a native call's failure is a local refusal or a store error,
// and re-issuing it in a sleep loop would invent the behaviour ADR-098's
// rounds six times refused to invent. `retry:` is therefore inert on a
// native action — C335 says so at compile time, and the ADR says why.
func (e *ClawExecutor) executeToolNodeNativeAction(ctx context.Context, node *ir.ToolNode, transport ActionTransport, input map[string]any) (map[string]any, error) {
	pkg := transport.Package()
	if pkg == nil {
		return nil, fmt.Errorf("model: tool node %q: the native transport for %q serves no facade package", node.ID, node.Action)
	}
	op, ok := pkg.Operation(node.Action)
	if !ok {
		return nil, fmt.Errorf("model: tool node %q: the native facade for connector %q exposes no operation %q", node.ID, transport.Connector(), node.Action)
	}
	if err := checkOperationUsable(node, pkg, op); err != nil {
		return nil, err
	}

	params, err := e.renderActionParams(ctx, node, op, input, secretsAsPlaceholders)
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: %w", node.ID, e.scrubActionError(err))
	}

	callCtx := ctx
	if node.CallTimeout != "" {
		d, perr := time.ParseDuration(node.CallTimeout)
		if perr != nil {
			return nil, fmt.Errorf("model: tool node %q: timeout %q: %w", node.ID, node.CallTimeout, perr)
		}
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	start := time.Now()
	e.emitToolNodeStarted(node.ID, actionToolName(node), len(params))
	data, callErr := transport.Call(callCtx, op, params, e.nodeCapabilities(node))
	if callErr != nil {
		callErr = e.scrubActionError(callErr)
		_ = e.emitToolNodeFinish(node.ID, actionToolName(node), node.Action, "", "", time.Since(start), callErr)
		return nil, fmt.Errorf("model: tool node %q: %s failed: %w", node.ID, op.ID, callErr)
	}

	out := map[string]any{}
	if data != nil {
		out["data"] = data
	}
	rendered, _ := json.Marshal(out)
	_ = e.emitToolNodeFinish(node.ID, actionToolName(node), node.Action, string(rendered), "", time.Since(start), nil)
	return out, nil
}
