package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Connector-action execution (ADR-098) — the third `tool` recipe.
//
// This file is deliberately thin. Everything about HOW a call is built and
// read lives in pkg/connector/exec, which is tested against a real HTTP
// server; what happens here is the engine's half: resolve the package and the
// connection, render the arguments from the run's template namespaces, coerce
// them to the types the operation declares, and shape the answer into a node
// output a `.bot` can branch on.
//
// No LLM is involved at any point, and nothing on this path can introduce
// one: the compile-time refusals (C252/C253) removed the two that could.

// ConnectorResolver hands the executor the package and the credential an
// action needs.
//
// It is an INTERFACE rather than a concrete store because the two things it
// resolves come from places that do not exist yet in the same form: the
// package from the three-tier catalog, the credential from the generalized
// connection layer. Wiring the node against the seam now means neither of
// those has to be finished for a `.bot` to be authored, validated and — with
// a local resolver — run.
type ConnectorResolver interface {
	// ResolveAction returns the package an operation belongs to, the operation
	// itself, and the credential the named connection holds for it.
	//
	// The BASE URL comes back with them because it belongs to the connection,
	// not the package: the same Forgejo package serves codeberg.org and a
	// self-hosted instance, and only the connection knows which.
	ResolveAction(ctx context.Context, actionID, connection string) (*spec.Package, spec.Operation, exec.Credential, string, error)
}

// executeToolNodeAction runs a connector operation.
func (e *ClawExecutor) executeToolNodeAction(ctx context.Context, node *ir.ToolNode, input map[string]any) (map[string]any, error) {
	if e.connectors == nil {
		// Explicit rather than a silent no-op: a bot that declares an action
		// and runs somewhere with no catalog wired must say so, not report a
		// success it never performed.
		return nil, fmt.Errorf("model: tool node %q declares `action: %s` but this process has no connector catalog wired", node.ID, node.Action)
	}
	if err := e.checkToolNodePolicy(ctx, node, actionToolName(node)); err != nil {
		return nil, err
	}

	pkg, op, cred, baseURL, err := e.connectors.ResolveAction(ctx, node.Action, node.Connection)
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: %w", node.ID, err)
	}
	if err := checkOperationUsable(node, pkg, op); err != nil {
		return nil, err
	}

	params, err := e.renderActionParams(ctx, node, op, input)
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: %w", node.ID, err)
	}

	callCtx := ctx
	if node.CallTimeout != "" {
		d, perr := time.ParseDuration(node.CallTimeout)
		if perr != nil {
			// Compile refuses this (C255), so reaching it means the IR was
			// built by hand; failing loudly beats calling with no bound.
			return nil, fmt.Errorf("model: tool node %q: timeout %q: %w", node.ID, node.CallTimeout, perr)
		}
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	executor := &exec.Executor{Client: e.connectorClient, BaseURL: baseURL, UserAgent: connectorUserAgent}
	start := time.Now()
	e.emitToolNodeStarted(node.ID, actionToolName(node), len(params))

	var res exec.Result
	if op.Pagination != nil {
		items, complete, last, callErr := executor.CallPaged(callCtx, pkg, op, params, cred)
		if callErr != nil {
			return nil, e.finishAction(node, op, exec.Result{}, start, callErr)
		}
		res = last
		if res.OK() {
			return e.actionOutput(node, op, res, items, complete, start), nil
		}
	} else {
		var callErr error
		res, callErr = executor.Call(callCtx, pkg, op, params, cred)
		if callErr != nil {
			return nil, e.finishAction(node, op, res, start, callErr)
		}
		if res.OK() {
			return e.actionOutput(node, op, res, nil, true, start), nil
		}
	}
	return nil, e.finishAction(node, op, res, start, nil)
}

// checkOperationUsable refuses an operation the RESOLVER handed back that a
// node may not run — checked here, at the moment of use, rather than trusted
// from whoever produced the package.
//
// The two properties are the ones a `.bot` author cannot see. A package
// declares each operation `deterministic`, and the whole value of the action
// recipe is that the claim holds; `spotted` is the inert maturity level, the
// way a catalog entry says "recorded, promising nothing". Both are decided by
// data that arrives at run time from a tier the workflow never named — a
// platform override, a team package, a marketplace entry — so the compiler
// cannot have checked them, and a resolver is exactly the component that
// might be wrong.
func checkOperationUsable(node *ir.ToolNode, pkg *spec.Package, op spec.Operation) error {
	if pkg == nil {
		return fmt.Errorf("model: tool node %q: the connector resolver returned no package for %q", node.ID, node.Action)
	}
	if !op.Deterministic {
		return fmt.Errorf("model: tool node %q: operation %q is not marked deterministic, and `action:` is the recipe that certifies it is; "+
			"reach it through an agent's connector capability instead", node.ID, op.ID)
	}
	if m := pkg.EffectiveMaturity(op); !m.Attachable() {
		return fmt.Errorf("model: tool node %q: operation %q is at maturity %q, which promises nothing and may not be bound to a node", node.ID, op.ID, m)
	}
	return nil
}

// finishAction emits the finish hooks and turns a typed failure into the
// node's error.
//
// A connector failure is a NODE failure, not a result the node returns: the
// engine's resumable-failure machinery is what a `.bot` author already knows,
// and inventing a second convention ("the node succeeded, read `error`")
// would let an unchecked output flow into the next node as if the call had
// worked. Branching on a failure is what `fail`/`when` edges are for.
func (e *ClawExecutor) finishAction(node *ir.ToolNode, op spec.Operation, res exec.Result, start time.Time, callErr error) error {
	err := callErr
	if err == nil && res.Err != nil {
		err = res.Err
	}
	e.emitToolNodeFinish(node.ID, actionToolName(node), node.Action, "", "", time.Since(start), err)
	if callErr != nil {
		return fmt.Errorf("model: tool node %q: %w", node.ID, callErr)
	}
	if res.Err == nil {
		return nil
	}
	// The class leads the message because it is what an operator scans for,
	// and `unknown_outcome` in particular has to be unmissable: it means the
	// remote system may or may not have changed.
	//
	// WRAPPED, not formatted in: the engine's recovery dispatcher classifies
	// by type (`errors.As`), so a `%s` here left it reading plain text and
	// bucketing an ambiguous mutation as an ordinary retryable failure. The
	// typed error has to survive the node boundary for the no-retry guarantee
	// to reach the one component that acts on it.
	return fmt.Errorf("model: tool node %q: %s failed [%s]: %w", node.ID, op.ID, res.Err.Class, res.Err)
}

// actionOutput shapes a successful call into the node's output map.
//
// The shape is flat and small on purpose. `data` is the vendor's answer,
// `status` and `pending` are what a `when` edge branches on, and a paginated
// call adds `items` and `complete` — the second of which a workflow MUST be
// able to read, since a walk that stopped at its ceiling looks exactly like
// one that finished.
func (e *ClawExecutor) actionOutput(node *ir.ToolNode, op spec.Operation, res exec.Result, items []any, complete bool, start time.Time) map[string]any {
	out := map[string]any{
		"status":  res.Status,
		"pending": res.Pending,
	}
	if res.Data != nil {
		out["data"] = res.Data
	}
	if op.Pagination != nil {
		out["items"] = items
		out["complete"] = complete
	}
	rendered, _ := json.Marshal(out)
	e.emitToolNodeFinish(node.ID, actionToolName(node), node.Action, string(rendered), "", time.Since(start), nil)
	return out
}

// renderActionParams resolves each declared param's template refs against the
// run's namespaces, then coerces the result to the type the OPERATION
// declares.
//
// The coercion is not cosmetic. Templates render to strings, and a JSON body
// carrying `{"index": "42"}` where the vendor's schema says integer is a
// request many APIs reject and some silently mis-handle. The operation is the
// only place that knows which is which, so the conversion happens here rather
// than in the DSL, where every value is text.
func (e *ClawExecutor) renderActionParams(ctx context.Context, node *ir.ToolNode, op spec.Operation, input map[string]any) (map[string]any, error) {
	declared := make(map[string]spec.Param, len(op.Params))
	for _, p := range op.Params {
		declared[p.Key] = p
	}
	td := TemplateDataFromContext(ctx)
	runID := RunIDFromContext(ctx)

	out := make(map[string]any, len(node.Params))
	for _, p := range node.Params {
		// Rendered with the SCRIPT renderer, not the shell one: nothing here
		// reaches a shell, and shell-escaping would wrap every value in
		// quotes that would then travel to the vendor verbatim.
		rendered := resolveScriptTemplate(p.Value, p.Refs, input, e.vars, td, runID, e.secretGuard)
		decl, known := declared[p.Key]
		if !known {
			// exec refuses this too, and its message lists what the operation
			// accepts; letting it through keeps ONE definition of that error.
			out[p.Key] = rendered
			continue
		}
		v, err := coerceParam(decl, rendered)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", p.Key, err)
		}
		out[p.Key] = v
	}
	return out, nil
}

// coerceParam converts a rendered template into the declared type.
//
// resolveScriptTemplate produces JSON literals, so a string arrives quoted, a
// number bare and an object as JSON text. Parsing that back is what lets a
// `.bot` write `index: "{{outputs.pick.issue}}"` and have an integer reach an
// integer field.
func coerceParam(decl spec.Param, rendered string) (any, error) {
	trimmed := strings.TrimSpace(rendered)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	// A JSON literal (the common case: the renderer quotes strings) decodes
	// straight to the right Go type.
	var decoded any
	if json.Unmarshal([]byte(trimmed), &decoded) == nil {
		return coerceDecoded(decl, decoded)
	}
	// Bare text the renderer left alone (an author who wrote a literal).
	return coerceDecoded(decl, trimmed)
}

func coerceDecoded(decl spec.Param, v any) (any, error) {
	switch decl.Type {
	case "integer", "number":
		switch t := v.(type) {
		case float64, int, int64:
			return t, nil
		case string:
			if n, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return n, nil
			}
			return nil, fmt.Errorf("%q is not a number, and the operation declares %s", t, decl.Type)
		}
	case "boolean":
		switch t := v.(type) {
		case bool:
			return t, nil
		case string:
			if b, err := strconv.ParseBool(strings.TrimSpace(t)); err == nil {
				return b, nil
			}
			return nil, fmt.Errorf("%q is not a boolean, and the operation declares one", t)
		}
	case "string":
		if s, ok := v.(string); ok {
			return s, nil
		}
		// A number or a bool used where a string is wanted is unambiguous.
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("value cannot be rendered as a string")
		}
		return strings.Trim(string(raw), `"`), nil
	}
	return v, nil
}

// actionToolName is the virtual tool name an action node reports to the
// policy gate and the event hooks. It carries the OPERATION, so a permission
// rule can allow `connector:forgejo.issue.comment` rather than every action
// of every connector — the granularity an operator actually wants.
func actionToolName(node *ir.ToolNode) string {
	return "connector:" + node.Action
}

// connectorUserAgent identifies iterion to a vendor. A recognisable agent is
// what lets a vendor's support answer "what is hitting our API".
const connectorUserAgent = "iterion-connector"
