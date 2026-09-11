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

	// `retry: N` = N attempts BEYOND the first, and only where repeating is
	// safe. Every guard lives in exec.Error.Retryable, which refuses an
	// ambiguous outcome and refuses a mutation whose call carried no
	// idempotency key — so this loop can never be the thing that duplicates
	// an effect, however large N is.
	attempts := 1 + actionRetries(node)
	var res exec.Result
	for attempt := 1; ; attempt++ {
		var items []any
		complete := true
		var callErr error
		if op.Pagination != nil {
			items, complete, res, callErr = executor.CallPaged(callCtx, pkg, op, params, cred)
		} else {
			res, callErr = executor.Call(callCtx, pkg, op, params, cred)
		}
		if callErr != nil {
			// Not a vendor answer — a local refusal or a walk that could not
			// read its collection. Repeating cannot change it.
			return nil, e.finishAction(node, op, res, start, callErr)
		}
		if res.OK() {
			return e.actionOutput(node, op, res, items, complete, start), nil
		}
		if attempt >= attempts || !res.Err.Retryable(op, params) {
			return nil, e.finishAction(node, op, res, start, nil)
		}
		// The vendor's own Retry-After when it gave one; otherwise straight
		// on. iterion does not invent a backoff here: a delay the workflow
		// guessed would be worse than the one the service asked for.
		if wait := res.Err.RetryAfter; wait > 0 {
			select {
			case <-callCtx.Done():
				return nil, e.finishAction(node, op, res, start, callCtx.Err())
			case <-time.After(wait):
			}
		}
	}
}

// actionRetries reads the node's `retry:` as a count of EXTRA attempts.
// Compile refuses anything else (C265), so a malformed value here means a
// hand-built IR; zero is the safe reading.
func actionRetries(node *ir.ToolNode) int {
	if node.RetryPolicy == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(node.RetryPolicy))
	if err != nil || n < 0 {
		return 0
	}
	return n
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
	// Only when a node cost MORE than one request, so the common case stays as
	// small as it reads. A paginated action can spend twenty of a vendor's
	// rate-limit slots behind what looks like a single call, and nothing said
	// so — the operator found out on the vendor's dashboard.
	if res.Requests > 1 {
		out["requests"] = res.Requests
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
		// TWO renderings, because a parameter's value is one of two different
		// things and treating them alike corrupts one of them.
		//
		// When the whole value IS a reference (`index: "{{outputs.pick.n}}"`),
		// the author means the VALUE, so it is rendered as a JSON literal and
		// decoded back to its type below — that is what lets a template, which
		// is always text, deliver an integer to an integer field.
		//
		// When the reference is EMBEDDED in text (`body: "hello {{input.who}}"`),
		// the author means string interpolation. Rendering that as a JSON
		// literal produced `hello "Alice"` — quotes and all — and sent it to
		// the vendor, because the surrounding text makes the result un-decodable
		// so nothing downstream could undo it.
		rendered := ""
		if isWholeValueRef(p.Value, p.Refs) {
			rendered = resolveScriptTemplate(p.Value, p.Refs, input, e.vars, td, runID, e.secretGuard)
		} else {
			rendered = resolveTemplateWith(p.Value, p.Refs, input, e.vars, td, runID, e.secretGuard, rawTemplateValue, true)
		}
		// A `{{secrets.NAME}}` ref renders to a PLACEHOLDER, not a value —
		// the whole point, since a secret must not sit in a command line or
		// in a log. Every other recipe materialises it before use; this one
		// did not, so the literal text `__ITERION_SECRET_NAME__` travelled to
		// the vendor as the argument. The raw form is right here: nothing on
		// this path reaches a shell, so the shell-escaping variants would
		// corrupt the value.
		rendered = e.secretGuard.Materialize(rendered)
		decl, known := declared[p.Key]
		if !known {
			// exec refuses this too, and its message lists what the operation
			// accepts; letting it through keeps ONE definition of that error.
			out[p.Key] = rendered
			continue
		}
		v, err := coerceParam(decl, rendered, isWholeValueRef(p.Value, p.Refs))
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", p.Key, err)
		}
		out[p.Key] = v
	}
	return out, nil
}

// isWholeValueRef reports whether the parameter's value is exactly ONE
// reference and nothing else — the case where the author means the referenced
// value rather than a string containing it.
func isWholeValueRef(value string, refs []*ir.Ref) bool {
	if len(refs) != 1 {
		return false
	}
	return strings.TrimSpace(value) == strings.TrimSpace(refs[0].Raw)
}

// coerceParam converts a rendered template into the declared type.
//
// A whole-value reference arrives as a JSON literal — a string quoted, a
// number bare, an object as JSON text — so parsing it back is what lets a
// `.bot` write `index: "{{outputs.pick.issue}}"` and have an integer reach an
// integer field. Interpolated text arrives as text.
func coerceParam(decl spec.Param, rendered string, typed bool) (any, error) {
	// AUTHORED TEXT is not JSON, and reading it as JSON altered it.
	//
	// The renderer already knows which of the two this is — a whole-value
	// reference carries a typed value, anything else is a string the author
	// wrote — and that distinction was being discarded one line later. So a
	// literal `"  keep whitespace  "` reached the vendor trimmed, and a
	// literal `"null"` (a perfectly ordinary word to send) produced no request
	// body at all.
	//
	// Only a typed reference is decoded. A literal is what it says, and a
	// declared non-string type still converts it — `index: "42"` remains an
	// integer — because that conversion reads the DECLARATION rather than
	// guessing from the text's shape.
	if !typed {
		if decl.Type == "" || decl.Type == "string" {
			return rendered, nil
		}
		return coerceDecoded(decl, rendered)
	}

	trimmed := strings.TrimSpace(rendered)
	// Only the JSON null LITERAL means absent. An empty rendering used to
	// mean it too, which silently dropped an author's deliberate `body: ""`
	// — an empty string is a value, and for several vendors it is the one
	// that clears a field.
	if trimmed == "null" {
		return nil, nil
	}
	if rendered == "" {
		return "", nil
	}
	// UseNumber keeps an integer EXACT. Decoding into `any` gives float64,
	// whose 53-bit mantissa silently rewrites a large id:
	// 9007199254740993 came back as 9007199254740992, which addresses a
	// different resource and looks perfectly plausible in a log.
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err == nil && !dec.More() {
		return coerceDecoded(decl, decoded)
	}
	// Bare text the renderer left alone (an author who wrote a literal).
	return coerceDecoded(decl, trimmed)
}

// coerceDecoded converts a decoded value to the type the OPERATION declares,
// and REFUSES what cannot be converted.
//
// Refusing matters as much as converting. This used to fall through and send
// the value unchanged whenever the type did not match a case — so `1.5` reached
// a parameter declared `integer`, `123` reached one declared `boolean`, and an
// array reached a scalar. The vendor then answered 400 (at best) or coerced it
// silently in a way the workflow never sees, on a path whose whole promise is
// that the request is what was declared.
func coerceDecoded(decl spec.Param, v any) (any, error) {
	switch decl.Type {
	case "integer":
		switch t := v.(type) {
		case json.Number:
			// Kept as json.Number so an id beyond float64's 53-bit mantissa
			// survives to the wire exactly as written.
			if _, err := t.Int64(); err != nil {
				return nil, fmt.Errorf("%s is not an integer, and the operation declares one", t.String())
			}
			return t, nil
		case int, int64:
			return t, nil
		case float64:
			if t != float64(int64(t)) {
				return nil, fmt.Errorf("%v is not an integer, and the operation declares one", t)
			}
			return int64(t), nil
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%q is not an integer, and the operation declares one", t)
			}
			return n, nil
		}
		return nil, typeMismatch(decl, v)
	case "number":
		switch t := v.(type) {
		case json.Number:
			if _, err := t.Float64(); err != nil {
				return nil, fmt.Errorf("%s is not a number, and the operation declares one", t.String())
			}
			return t, nil
		case float64, int, int64:
			return t, nil
		case string:
			if n, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return n, nil
			}
			return nil, fmt.Errorf("%q is not a number, and the operation declares %s", t, decl.Type)
		}
		return nil, typeMismatch(decl, v)
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
		return nil, typeMismatch(decl, v)
	case "string":
		switch t := v.(type) {
		case string:
			return t, nil
		case json.Number:
			return t.String(), nil
		case bool:
			return strconv.FormatBool(t), nil
		case float64:
			return strconv.FormatFloat(t, 'f', -1, 64), nil
		}
		// A structure where a string is wanted is not a rendering question.
		return nil, typeMismatch(decl, v)
	case "array":
		if _, ok := v.([]any); ok {
			return v, nil
		}
		return nil, typeMismatch(decl, v)
	}
	return v, nil
}

// typeMismatch says what was given and what was wanted, in the vocabulary the
// author used — the declared type is what they can act on.
func typeMismatch(decl spec.Param, v any) error {
	return fmt.Errorf("%s is not a %s, and the operation declares %s for %q",
		describeJSONValue(v), decl.Type, decl.Type, decl.Key)
}

// describeJSONValue names a value's SHAPE rather than printing it, because
// the value may be a secret and this text reaches the run's events.
func describeJSONValue(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case json.Number, float64, int, int64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
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
