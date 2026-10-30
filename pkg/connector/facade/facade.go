// Package facade is the MCP offer over a connector package (ADR-098 §2,
// #2758): a connector's operations exposed to an agent as MCP tools, with
// the model choosing among what the operator curated.
//
// The facade is GENERATED FROM THE PACKAGE at run time — one source of
// operations, or the node surface and the agent surface drift. The curated
// subset (the overlay's `mcp: true` operations) is listed as tools by name
// AND is the whole callable set: the `search_operations` /
// `call_operation` pair discovers the rest (ids and summaries, so a
// 500-operation API does not bury an agent's context) without serving it,
// because what a model may call is what an operator read and curate —
// an operation outside that list is refused, whatever the run's trust.
//
// The package boundary is the security property: facade code NEVER touches a
// credential. Every call goes out through the ExecFunc the server hands in,
// which resolves the connection (tenant-scoped, `agent` capability) and
// performs the HTTP call there. What an agent can exfiltrate from here is
// what the vendor answered, never what authenticated the question.
package facade

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// ConnArg is the reserved argument every tool carries: the alias of the
// connection that authenticates the call — the same name a `.bot` writes in
// `connection:`. Reserved rather than accepted inline so a package whose
// operation legitimately has a parameter with this name cannot produce a
// tool the facade misreads.
const ConnArg = "connection"

// SearchTool and CallTool are the two tools every facade exposes regardless
// of curation: discovery and the long tail.
const (
	SearchTool = "search_operations"
	CallTool   = "call_operation"
)

// ExecFunc performs one operation's call for one connection alias. The server
// side resolves (tenant, alias, operation) and executes; the facade sees only
// the answer. A business failure (the vendor's 404) is a RESULT, not an
// error — the facade renders it as an MCP isError content either way, so the
// distinction is the implementer's, matched to exec.Result's.
type ExecFunc func(ctx context.Context, operation, alias string, params map[string]any) (result any, err error)

// Tool is one entry of a tools/list answer.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// OperationHit is one search_operations result: enough to choose, not enough
// to call — call_operation takes the id and answers with the parameters.
type OperationHit struct {
	ID       string `json:"id"`
	Summary  string `json:"summary,omitempty"`
	Curated  bool   `json:"curated,omitempty"`
	Resource string `json:"resource"`
	Verb     string `json:"verb"`
}

// Facade is the MCP surface of one or more connector packages. A server
// multiplexes its whole catalog through ONE facade: tool names carry the
// connector prefix (forgejo_issue_comment), operation ids do too, so the
// discovery pair needs no per-connector variant.
type Facade struct {
	pkgs map[string]*spec.Package
	// curated is tool name → operation. byID is operation id → operation for
	// the long tail (call_operation).
	curated map[string]spec.Operation
	byID    map[string]spec.Operation
	// connectors is the sorted id list, for the discovery pair's descriptions.
	connectors []string
}

// New builds the facade over pkgs, refusing a set whose tools cannot be
// served: two operations mapping to one tool name, or one shadowing the
// reserved pair, would list a tool that calls the wrong operation. Two
// packages answering to one connector id are refused for the same reason.
func New(pkgs ...*spec.Package) (*Facade, error) {
	f := &Facade{
		pkgs:    map[string]*spec.Package{},
		curated: map[string]spec.Operation{},
		byID:    map[string]spec.Operation{},
	}
	for _, pkg := range pkgs {
		if pkg == nil {
			return nil, fmt.Errorf("facade: a nil package")
		}
		id := pkg.Connector.ID
		if _, dup := f.pkgs[id]; dup {
			return nil, fmt.Errorf("facade: connector %q was handed in twice", id)
		}
		f.pkgs[id] = pkg
		f.connectors = append(f.connectors, id)
		for _, op := range pkg.Operations() {
			f.byID[op.ID] = op
			if !op.MCP {
				continue
			}
			name := ToolName(op.ID)
			if name == SearchTool || name == CallTool {
				return nil, fmt.Errorf("facade: operation %q maps to the reserved tool %q", op.ID, name)
			}
			if prev, clash := f.curated[name]; clash {
				return nil, fmt.Errorf("facade: operations %q and %q both map to tool %q — rename one in the overlay", prev.ID, op.ID, name)
			}
			f.curated[name] = op
		}
	}
	sort.Strings(f.connectors)
	return f, nil
}

// ToolName maps an operation id to its MCP tool name. MCP names match
// [a-zA-Z0-9_-]{1,64} and an operation id is `connector.resource.verb`, so
// the dots become underscores; an id longer than the limit is truncated at a
// segment boundary where one exists.
func ToolName(opID string) string {
	name := strings.ReplaceAll(opID, ".", "_")
	if len(name) <= 64 {
		return name
	}
	if i := strings.LastIndex(name[:64], "_"); i > 0 {
		return name[:i]
	}
	return name[:64]
}

// Tools is the tools/list answer: the curated operations by name, plus the
// discovery pair. Sorted, so two listings of one package are byte-identical.
func (f *Facade) Tools() []Tool {
	names := make([]string, 0, len(f.curated))
	for name := range f.curated {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Tool, 0, len(names)+2)
	for _, name := range names {
		op := f.curated[name]
		out = append(out, Tool{
			Name:        name,
			Description: toolDescription(op),
			InputSchema: inputSchema(op),
		})
	}
	out = append(out, Tool{
		Name: SearchTool,
		Description: fmt.Sprintf("Search the %d operations of the served connectors (%s) by id, summary or tag. "+
			"Returns operation ids; call one with %s (or a named tool when it is listed).",
			len(f.byID), strings.Join(f.connectors, ", "), CallTool),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "substring or word to match, e.g. \"issue\" or \"comment\""},
				"limit": map[string]any{"type": "integer", "description": "maximum hits (default 20)"},
			},
			"required": []string{"query"},
		},
	}, Tool{
		Name: CallTool,
		Description: fmt.Sprintf("Call any operation by id — including the ones not listed as named tools. "+
			"arguments is the operation's params map; %s names the connection that authenticates.",
			ConnArg),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{"type": "string", "description": "the operation id, e.g. " + f.exampleOpID()},
				ConnArg:     map[string]any{"type": "string", "description": "the connection alias authenticating the call"},
				"arguments": map[string]any{"type": "object", "description": "the operation's parameters"},
			},
			"required": []string{"operation", ConnArg},
		},
	})
	return out
}

// exampleOpID is the placeholder the discovery tool's schema shows, or the
// bare shape when the facade serves nothing yet (an empty facade must still
// answer tools/list, not panic).
func (f *Facade) exampleOpID() string {
	if len(f.connectors) == 0 {
		return "connector.resource.verb"
	}
	return f.connectors[0] + ".issue.comment"
}

// toolDescription is what the model reads when choosing: the summary, then
// the effect, so a mutating operation announces itself as one.
func toolDescription(op spec.Operation) string {
	desc := op.Summary
	if desc == "" {
		desc = op.Description
	}
	if op.Effect.Known() {
		desc += fmt.Sprintf(" (effect: %s)", op.Effect)
	}
	return desc
}

// inputSchema builds the tool's JSON schema from the operation's flattened
// params — the agent writes ONE map, like a `.bot`'s `params:` — plus the
// reserved connection argument.
func inputSchema(op spec.Operation) map[string]any {
	props := map[string]any{
		ConnArg: map[string]any{"type": "string", "description": "the connection alias authenticating the call"},
	}
	required := []string{ConnArg}
	for _, p := range op.Params {
		prop := map[string]any{"type": jsonType(p)}
		if p.Description != "" {
			prop["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			prop["enum"] = p.Enum
		}
		if p.Default != nil {
			prop["default"] = p.Default
		}
		props[p.Key] = prop
		if p.Required {
			required = append(required, p.Key)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// jsonType maps the package's OpenAPI-flavoured type to JSON schema's.
func jsonType(p spec.Param) string {
	switch p.Type {
	case "string", "integer", "number", "boolean", "object":
		return p.Type
	case "array":
		return "array"
	default:
		return "string"
	}
}

// SearchOperations answers the discovery tool: operations whose id, summary,
// description or tags match the query, curated first, then by id. A blank
// query lists the curated set — the answer to "what is this connector for".
//
// The second return is the UNTRUNCATED match count: the page is capped, and a
// cap applied in silence reads as "these twenty are all of them" — a model
// that cannot see the truncation cannot narrow its query, and the operation
// it needed stays invisible while search answers 200.
func (f *Facade) SearchOperations(query string, limit int) ([]OperationHit, int) {
	if limit <= 0 {
		limit = 20
	}
	query = strings.ToLower(strings.TrimSpace(query))
	var hits []OperationHit
	for _, op := range f.byID {
		if query != "" && !matches(op, query) {
			continue
		}
		hits = append(hits, OperationHit{ID: op.ID, Summary: op.Summary, Curated: op.MCP, Resource: op.Resource, Verb: op.Verb})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Curated != hits[j].Curated {
			return hits[i].Curated
		}
		return hits[i].ID < hits[j].ID
	})
	total := len(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, total
}

// matches is the substring test over everything an agent would guess at.
func matches(op spec.Operation, query string) bool {
	for _, field := range append([]string{op.ID, op.Summary, op.Description, op.Resource, op.Verb}, op.Tags...) {
		if strings.Contains(strings.ToLower(field), query) {
			return true
		}
	}
	return false
}

// Call dispatches one tools/call: a curated tool by name, the discovery tool,
// or the long tail. The CONNECTION is extracted here and never mixed into the
// operation's params, so a package parameter of the same wire name cannot be
// confused with the grant that authenticates the call.
func (f *Facade) Call(ctx context.Context, tool string, args json.RawMessage, execFn ExecFunc) (any, error) {
	if execFn == nil {
		return nil, fmt.Errorf("facade: no executor wired — the server resolves the connection and performs the call")
	}
	switch tool {
	case SearchTool:
		var a struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("facade: %s: invalid arguments: %w", SearchTool, err)
		}
		hits, total := f.SearchOperations(a.Query, a.Limit)
		return map[string]any{"operations": hits, "total": total}, nil
	case CallTool:
		var a struct {
			Operation string          `json:"operation"`
			Alias     string          `json:"connection"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("facade: %s: invalid arguments: %w", CallTool, err)
		}
		op, found := f.byID[a.Operation]
		if !found {
			return nil, fmt.Errorf("facade: no operation %q among the served connectors (%s) — search for it with %s first",
				a.Operation, strings.Join(f.connectors, ", "), SearchTool)
		}
		// THE CURATED SET IS THE BOUND. The operator reads tools/list as what
		// a model may call; a package ships 85 deletes and 58 updates the
		// curated list does not name, and this back door reached them all — a
		// model choosing an operation nobody offered it is an EFFECT nobody
		// granted, which is the one thing the curation exists to decide. The
		// deterministic node path needs no such bound: a workflow DECLARED its
		// call, in a file a human reviewed. The remedy is the operator's:
		// curate the operation (`mcp: true` in the overlay) and it is served.
		if !op.MCP {
			return nil, fmt.Errorf("facade: operation %q is not in the curated set, so no agent may call it — an operator curates what a model may call (the overlay's `mcp: true`), and %s does not reach past that list", op.ID, CallTool)
		}
		params, err := paramsFrom(a.Arguments)
		if err != nil {
			return nil, fmt.Errorf("facade: %s: %w", CallTool, err)
		}
		return execFn(ctx, op.ID, a.Alias, params)
	default:
		op, found := f.curated[tool]
		if !found {
			return nil, fmt.Errorf("facade: no tool %q — the curated set is listed by tools/list, the long tail is %s", tool, CallTool)
		}
		params, alias, err := splitArgs(args)
		if err != nil {
			return nil, fmt.Errorf("facade: %s: %w", tool, err)
		}
		return execFn(ctx, op.ID, alias, params)
	}
}

// splitArgs separates the reserved connection argument from the operation's
// own params on a curated call.
func splitArgs(raw json.RawMessage) (params map[string]any, alias string, err error) {
	all, err := paramsFrom(raw)
	if err != nil {
		return nil, "", err
	}
	if v, ok := all[ConnArg]; ok {
		s, isStr := v.(string)
		if !isStr {
			return nil, "", fmt.Errorf("%s must be a string (the connection alias)", ConnArg)
		}
		alias = s
		delete(all, ConnArg)
	}
	return all, alias, nil
}

func paramsFrom(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("arguments must be an object: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}
