package ir

import (
	"fmt"
	"net/netip"
	"path"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// ---------------------------------------------------------------------------
// C029–C036 — deep template reference validation
// ---------------------------------------------------------------------------

// refContext associates a Ref with the node that consumes it and a
// human-readable location string for diagnostics.
type refContext struct {
	Ref         *Ref
	NodeID      string // consuming node ID (edge with-mappings: the source)
	Location    string // e.g. "prompt 'sys' (node 'a')"
	IncludeSelf bool   // true for edge with-mappings: the source node itself is available
	EdgeTo      string // destination of an edge with-mapping; empty otherwise
	// EdgeID and Span locate the TEXT that holds the reference — the edge
	// line for a with-mapping, the prompt declaration for a prompt body —
	// so a diagnostic lands where the author has to edit, not on the
	// consuming node's header. Zero when the reference lives in the node
	// itself (a command, a script, an expression), where the node's own
	// declaration is the right place.
	EdgeID string
	Span   ast.Span
	// InWith is true for a reference that lives in a data mapping — an
	// edge `-> dst with { ... }`, a subbot node's own `with:`, an emit
	// node's `with:`, or a `fail message:`. The runtime resolves those
	// through `resolveMapping` / `resolveRef`, which has no arm for
	// `secrets` / `attachments` and reads `input.*` against the
	// parent's run inputs on a kind (subbot / emit) that has no
	// `input:` surface. Namespaces the site cannot honour are refused
	// (C149–C151) instead of resolving to nil at run time.
	InWith bool
	// InComputeExpr is true for a reference that lives inside a compute
	// node's `expr:` block. Compute expressions run through
	// `pkg/dsl/expr`, whose `evalNamespaces` (snapshot.go) excludes the
	// `secrets` and `attachments` namespaces on purpose; a `{{secrets.X}}`
	// or `{{attachments.X}}` in a compute expr therefore renders nil at
	// evaluation time, same defect class as the mapping refusal, so C150
	// and C151 fire there too. The check stays separate from InWith so
	// the two positions can carry their own phrasing.
	InComputeExpr bool
}

// refErrorf / refWarnf emit a template-reference diagnostic with the
// context's full attribution: node, edge and the span of the text that
// carries the reference.
func (c *compiler) refErrorf(rc refContext, code DiagCode, format string, args ...any) {
	c.emit(SeverityError, code, rc.NodeID, rc.EdgeID, rc.Span, "", format, args...)
}

func (c *compiler) refWarnf(rc refContext, code DiagCode, format string, args ...any) {
	c.emit(SeverityWarning, code, rc.NodeID, rc.EdgeID, rc.Span, "", format, args...)
}

// collectAllRefs gathers every template reference in the workflow together
// with the node that consumes it.
func collectAllRefs(w *Workflow, promptSpans map[string]ast.Span, edgeSpans map[*Edge]ast.Span, withSpans map[*DataMapping]ast.Span) []refContext {
	// Build reverse map: prompt name → list of consuming node IDs.
	promptUsers := make(map[string][]string)
	for _, n := range w.Nodes {
		for _, pname := range NodePromptRefs(n) {
			promptUsers[pname] = append(promptUsers[pname], n.NodeID())
		}
	}

	var out []refContext

	// Prompt template refs. The reference sits in the prompt's text, so the
	// prompt declaration is where the diagnostic points; the consuming node
	// stays the attribution (what the reference resolves against).
	for _, p := range w.Prompts {
		consumers := promptUsers[p.Name]
		for _, ref := range p.TemplateRefs {
			for _, nodeID := range consumers {
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   nodeID,
					Location: fmt.Sprintf("prompt %q (node %q)", p.Name, nodeID),
					Span:     promptSpans[p.Name],
				})
			}
		}
	}

	// Edge with-mapping refs. The mapping is evaluated when the edge
	// fires, so the source (From) has already produced its output.
	// {{input.x}} in that mapping is the source output (every router
	// copies its input onto its output, which is how pass-through
	// works) — not the source input schema, and not a silent fallback
	// to run-level inputs. C034 checks the source output schema; a
	// schemaless non-router source, or a mid-graph router whose
	// incoming with-keys do not include the field, warns C032. Use
	// {{vars.x}} for a launch-time value. IncludeSelf so
	// {{outputs.<source>}} is reachable too.
	for _, e := range w.Edges {
		for _, dm := range e.With {
			for _, ref := range dm.Refs {
				out = append(out, refContext{
					Ref:         ref,
					NodeID:      e.From,
					Location:    fmt.Sprintf("edge %s -> %s, with %q", e.From, e.To, dm.Key),
					IncludeSelf: true,
					EdgeTo:      e.To,
					EdgeID:      edgeID(e.From, e.To),
					Span:        edgeSpans[e],
					InWith:      true,
				})
			}
		}
	}

	// Node `with:` refs — the payload a subbot hands its child, the fields an
	// emit publishes. Edge mappings above were walked and these were not,
	// which is the same omission the tool-node comment below describes: the
	// value is a template, and an unvalidated reference is WORSE than a key
	// left unmapped. Measured on the engine: a bare `{{vars.typo}}` resolves
	// to nil and is handed over as nil, which suppresses the child's own
	// declared default for that key — the child then renders its own
	// `{{vars.depth}}` as source text. Embedded in surrounding text the
	// reference is spliced to empty instead. Omitting the key entirely is the
	// only form that lets the child's default stand.
	//
	// Nothing downstream re-reads the value, so here is the only place it can
	// be caught.
	//
	// The `secrets` and `attachments` namespaces do NOT resolve here:
	// `resolveMapping` has no arm for them and `pkg/dsl/expr` excludes
	// them from `evalNamespaces` — a reference to either renders nil
	// silently. This walk closes that gap via the `InWith` flag below,
	// which C150 / C151 read to refuse the reference at compile time
	// and name the real materialisation sinks (a tool's `command:` /
	// `script:` / `postcondition:`, a tool action's `params:` value,
	// or a prompt body).
	//
	// IncludeSelf stays off: the node has produced no output yet when its own
	// `with:` is resolved.
	for _, n := range w.Nodes {
		wn, ok := n.(WithNode)
		if !ok {
			continue
		}
		for _, dm := range wn.WithMappings() {
			for _, ref := range dm.Refs {
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   n.NodeID(),
					Location: fmt.Sprintf("%s node %q, with %q", n.NodeKind(), n.NodeID(), dm.Key),
					Span:     withSpans[dm],
					InWith:   true,
				})
			}
		}
	}

	// Tool node command + script refs. ScriptRefs used to be skipped,
	// so {{outputs.X.history}} inside a tool's script never went
	// through C029–C036 validation — typos were caught only at
	// runtime, after the script had already started executing.
	for _, n := range w.Nodes {
		if t, ok := n.(*ToolNode); ok {
			for _, ref := range t.CommandRefs {
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   t.ID,
					Location: fmt.Sprintf("tool node %q command", t.ID),
				})
			}
			for _, ref := range t.ScriptRefs {
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   t.ID,
					Location: fmt.Sprintf("tool node %q script", t.ID),
				})
			}
			// The third recipe's arguments, for the reason the script comment
			// above gives — and it is the same omission repeated: a new
			// recipe was added without walking every pass that reads the
			// other two. An unvalidated `{{outputs.typo.field}}` in an action
			// param renders empty and is SENT, so the vendor receives a
			// silently wrong argument instead of the author receiving C029.
			for _, p := range t.Params {
				for _, ref := range p.Refs {
					out = append(out, refContext{
						Ref:      ref,
						NodeID:   t.ID,
						Location: fmt.Sprintf("tool node %q action param %q", t.ID, p.Key),
					})
				}
			}
		}
	}

	// Fail node `message:` refs. Same argument the ScriptRefs comment
	// above makes: the message is what the operator reads INSTEAD of the
	// generic "workflow reached fail node", so a typo'd
	// `{{outputs.gat.pct}}` resolves to nil at fail time, renders empty,
	// and the outcome silently falls back to exactly the wording a typed
	// fail exists to remove — the one moment nobody is watching a
	// compiler.
	for _, n := range w.Nodes {
		fn, ok := n.(*FailNode)
		if !ok || fn.Message == nil {
			continue
		}
		for _, ref := range fn.Message.Refs {
			out = append(out, refContext{
				Ref:      ref,
				NodeID:   fn.ID,
				Location: fmt.Sprintf("fail node %q message", fn.ID),
				// The message goes through `resolveMapping`, same as an
				// edge/subbot/emit `with:` value: `resolveRef` has no
				// arm for secrets or attachments there, so the reference
				// would render to nil silently. Refuse it at compile
				// time like a with-mapping does.
				InWith: true,
			})
		}
	}

	// Compute node expressions. Each ComputeExpr.AST exposes its
	// vars/input/outputs/... references — convert them to ir.Ref
	// shape and feed them into the same C029–C036 pipeline so a
	// typo'd `outputs.unknown.field` in a compute expression is
	// caught at compile time instead of at first evaluation.
	for _, n := range w.Nodes {
		cn, ok := n.(*ComputeNode)
		if !ok {
			continue
		}
		for _, e := range cn.Exprs {
			if e.AST == nil {
				continue
			}
			for _, r := range e.AST.Refs() {
				ref := refFromExpr(r)
				if ref == nil {
					continue
				}
				out = append(out, refContext{
					Ref:           ref,
					NodeID:        cn.ID,
					Location:      fmt.Sprintf("compute node %q expr %q", cn.ID, e.Key),
					InComputeExpr: true,
				})
			}
		}
	}

	return out
}

// refFromExpr converts an [expr.Ref] (namespace + path) to an [ir.Ref]
// so the shared template-ref validator can check compute-node refs
// alongside prompt / edge / tool refs. Returns nil when the namespace
// isn't one of the kinds the template validator handles (e.g. `loop`,
// `run` — both legitimate but consumed by separate validators).
func refFromExpr(r expr.Ref) *Ref {
	var kind RefKind
	switch r.Namespace {
	case "vars":
		kind = RefVars
	case "input":
		kind = RefInput
	case "outputs":
		kind = RefOutputs
	case "artifacts":
		kind = RefArtifacts
	case "attachments":
		kind = RefAttachments
	case "secrets":
		// So C093 (unknown secret) fires for {{secrets.X}} in compute exprs too.
		kind = RefSecrets
	default:
		return nil
	}
	raw := r.Namespace
	for _, p := range r.Path {
		raw += "." + p
	}
	return &Ref{
		Kind: kind,
		Path: append([]string(nil), r.Path...),
		Raw:  "{{" + raw + "}}",
	}
}

// buildPredecessors computes, for each node, the set of all nodes that
// can execute before it (i.e. whose outputs are available). This follows
// ALL edges (including conditional and loop back-edges) to ensure zero
// false positives.
func buildPredecessors(w *Workflow) map[string]map[string]bool {
	// Build reverse adjacency list.
	revAdj := make(map[string][]string)
	for _, e := range w.Edges {
		revAdj[e.To] = append(revAdj[e.To], e.From)
	}

	// Identify nodes that are targets of loop back-edges.
	// These nodes are effectively their own predecessors because
	// a prior iteration's output is available on re-entry.
	loopTargets := make(map[string]bool)
	for _, e := range w.Edges {
		if e.LoopName != "" {
			loopTargets[e.To] = true
		}
	}

	result := make(map[string]map[string]bool)
	for id := range w.Nodes {
		preds := computePredecessors(id, revAdj)
		if loopTargets[id] {
			preds[id] = true
		}
		result[id] = preds
	}
	return result
}

// computePredecessors returns all transitive predecessors of nodeID via
// reverse BFS.
func computePredecessors(nodeID string, revAdj map[string][]string) map[string]bool {
	visited := make(map[string]bool)
	queue := revAdj[nodeID]
	for i := 0; i < len(queue); i++ {
		pred := queue[i]
		if visited[pred] || pred == nodeID {
			continue
		}
		visited[pred] = true
		queue = append(queue, revAdj[pred]...)
	}
	return visited
}

// buildArtifactProducers maps artifact names to their producing node IDs.
func buildArtifactProducers(w *Workflow) map[string]string {
	producers := make(map[string]string)
	for _, n := range w.Nodes {
		if pub := NodePublish(n); pub != "" {
			producers[pub] = n.NodeID()
		}
	}
	return producers
}

func (c *compiler) validateSecrets(w *Workflow) {
	if w == nil || len(w.Secrets) == 0 {
		return
	}
	for name, s := range w.Secrets {
		if s == nil {
			continue
		}
		switch s.As {
		case "", "value", "file":
			// ok
		default:
			c.errorf(DiagInvalidSecretFile,
				"secret %q: as must be \"value\" or \"file\" (got %q)", name, s.As)
		}
		if s.As != "file" && (s.MountPath != "" || s.Env != "") {
			c.errorf(DiagInvalidSecretFile,
				"secret %q: mount_path/env require as: file", name)
		}
		if s.MountPath != "" && !strings.HasPrefix(s.MountPath, "/") {
			c.errorf(DiagInvalidSecretFile,
				"secret %q: mount_path %q must be absolute", name, s.MountPath)
		}
		if s.MountPath != "" && (path.Clean(s.MountPath) != s.MountPath || s.MountPath == "/") {
			c.errorf(DiagInvalidSecretFile,
				"secret %q: mount_path %q must be a clean absolute file path", name, s.MountPath)
		}
		if s.Env != "" && !validEnvName(s.Env) {
			c.errorf(DiagInvalidSecretFile,
				"secret %q: env %q is not a valid environment variable name", name, s.Env)
		}
		for _, h := range s.Hosts {
			if !validSecretHost(h) {
				c.errorf(DiagInvalidSecretHost,
					"secret %q: hosts entry %q must be a bare hostname, parent domain, or IP without scheme/path", name, h)
			}
		}
	}
}

func validSecretHost(h string) bool {
	h = strings.TrimSpace(h)
	if h == "" || strings.Contains(h, "://") || strings.ContainsAny(h, "/?#@\\ \t\n\r\x00%") {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	if strings.Contains(h, ":") || len(h) > 253 || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if !validHostnameLabel(label) {
			return false
		}
	}
	return true
}

func validHostnameLabel(label string) bool {
	if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func (c *compiler) validateTemplateRefs(w *Workflow) {
	promptSpans := map[string]ast.Span{}
	if c.file != nil {
		for _, p := range c.file.Prompts {
			if _, seen := promptSpans[p.Name]; !seen {
				promptSpans[p.Name] = p.Span
			}
		}
	}
	refs := collectAllRefs(w, promptSpans, c.edgeSpans, c.withSpans)
	if len(refs) == 0 {
		return
	}

	predecessors := buildPredecessors(w)
	artifactProducers := buildArtifactProducers(w)

	for _, rc := range refs {
		switch rc.Ref.Kind {
		case RefOutputs:
			c.validateOutputsRef(w, rc, predecessors)
		case RefVars:
			c.validateVarsRef(w, rc)
		case RefInput:
			c.validateInputRef(w, rc)
		case RefArtifacts:
			c.validateArtifactsRef(w, rc, predecessors, artifactProducers)
		case RefAttachments:
			c.validateAttachmentsRef(w, rc)
		case RefSecrets:
			c.validateSecretsRef(w, rc)
		case RefLoop:
			c.validateLoopRef(w, rc)
		case RefRun:
			c.validateRunRef(w, rc)
		}
	}
}

// validateLoopRef warns of a {{loop.X.<field>}} reference whose loop X no
// edge declares, or whose field the loop namespace has not (C147). A
// warning, not an error: a bot in the field that carries the misspelling
// compiled yesterday, and the runtime no longer renders 0 for an unknown
// loop — the reference stays as written, a dry run names it — so the
// defect is visible on both sides without a break at upgrade.
func (c *compiler) validateLoopRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) < 2 {
		c.refWarnf(rc, DiagUnknownLoopRef,
			"%s: reference %s is incomplete (expected loop.<name>.iteration, .max or .previous_output)",
			rc.Location, rc.Ref.Raw)
		return
	}
	name, field := rc.Ref.Path[0], rc.Ref.Path[1]
	if w.Loops[name] == nil {
		c.refWarnf(rc, DiagUnknownLoopRef,
			"%s: reference %s targets undeclared loop %q",
			rc.Location, rc.Ref.Raw, name)
		return
	}
	switch field {
	case "iteration", "max":
		if len(rc.Ref.Path) > 2 {
			c.refWarnf(rc, DiagUnknownLoopRef,
				"%s: reference %s: loop.%s.%s has no sub-field",
				rc.Location, rc.Ref.Raw, name, field)
		}
	case "previous_output":
	default:
		c.refWarnf(rc, DiagUnknownLoopRef,
			"%s: reference %s uses unknown loop field %q (expected: iteration, max, previous_output)",
			rc.Location, rc.Ref.Raw, field)
	}
}

// validateRunRef warns of a {{run.X}} reference whose member the run
// namespace does not carry (C153). A warning, not an error, for the same
// reason validateLoopRef is one: the runtime renders no value for an
// unknown member, so yesterday's bot keeps compiling — and the defect is
// still named at validate time, because "renders empty" is exactly how an
// exclusion list vanishes from a prompt-carried scope gate's git command (#1464; on an
// executable command the preserved placeholder fails loudly instead — git
// refuses the pathspec it cannot match).
func (c *compiler) validateRunRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	member := rc.Ref.Path[0]
	for _, known := range RunMembers {
		if member == known {
			if len(rc.Ref.Path) > 1 {
				c.refWarnf(rc, DiagUnknownRunMember,
					"%s: reference %s has no sub-field — run.%s is a scalar value",
					rc.Location, rc.Ref.Raw, member)
			}
			return
		}
	}
	c.refWarnf(rc, DiagUnknownRunMember,
		"%s: reference %s uses unknown run member %q (known: %s)",
		rc.Location, rc.Ref.Raw, member, strings.Join(RunMembers, ", "))
}

// validateSecretsRef flags a {{secrets.X}} reference whose secret X is
// not declared in the workflow's `secrets:` block. In a data mapping —
// an edge / subbot / emit `with:` value, or a `fail message:` — the
// reference is refused: those all go through `resolveMapping`, and
// `resolveRef` has no arm for the secrets namespace there. The runtime
// materialises a secret only at an execution sink (a tool's
// `command:`/`script:`/`postcondition:` or a prompt body). A compute
// expression is NOT a sink — `pkg/dsl/expr` has no secrets/attachments
// resolver either (`evalNamespaces` in expr/snapshot.go excludes them
// explicitly). The mapping would resolve the reference to nil silently
// — worse than the omitted key it looks like a value for.
func (c *compiler) validateSecretsRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	name := rc.Ref.Path[0]
	if rc.InWith {
		c.refErrorf(rc, DiagWithSecretRef,
			"%s: reference %s cannot travel through a data mapping — a `with:` value or a fail `message:` resolves it to nil; move the reference to the execution sink that uses the secret (a tool's `command:`/`script:`/`postcondition:`, a tool action's `params:` value, or a prompt body), the only places the runtime materialises it",
			rc.Location, rc.Ref.Raw)
		return
	}
	if rc.InComputeExpr {
		c.refErrorf(rc, DiagWithSecretRef,
			"%s: reference %s cannot be resolved in a compute expression — `pkg/dsl/expr` has no secrets resolver (evalNamespaces excludes it), so the reference renders nil at evaluation; move the secret to an execution sink that materialises it (a tool's `command:`/`script:`/`postcondition:`, a tool action's `params:` value, or a prompt body)",
			rc.Location, rc.Ref.Raw)
		return
	}
	secret, ok := w.Secrets[name]
	if !ok {
		c.refErrorf(rc, DiagUnknownSecret,
			"%s: reference %s targets undeclared secret %q",
			rc.Location, rc.Ref.Raw, name)
		return
	}
	if len(rc.Ref.Path) == 1 {
		return
	}
	sub := rc.Ref.Path[1]
	if sub != "path" {
		c.refErrorf(rc, DiagSecretSubfield,
			"%s: reference %s uses unknown secret sub-field %q (expected: path)",
			rc.Location, rc.Ref.Raw, sub)
		return
	}
	if !secret.IsFile() {
		c.refErrorf(rc, DiagSecretSubfield,
			"%s: reference %s uses .path on non-file secret %q",
			rc.Location, rc.Ref.Raw, name)
	}
}

// validateAttachmentsRef flags an undeclared {{attachments.X}} reference
// or an unknown sub-field. In a data mapping — an edge / subbot / emit
// `with:` value, or a `fail message:` — the reference is refused: same
// rule as secrets, `resolveMapping` has no arm for the namespace and a
// compute expression cannot resolve it either. The mapping would render
// to nil silently. Execution sinks that DO materialise an attachment:
// a tool's `command:`/`script:`/`postcondition:`, or a prompt body.
func (c *compiler) validateAttachmentsRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	name := rc.Ref.Path[0]
	if rc.InWith {
		c.refErrorf(rc, DiagWithAttachmentRef,
			"%s: reference %s cannot travel through a data mapping — a `with:` value or a fail `message:` resolves it to nil; move the reference to the execution sink that uses the attachment (a tool's `command:`/`script:`/`postcondition:`, a tool action's `params:` value, or a prompt body), the only places the runtime materialises it",
			rc.Location, rc.Ref.Raw)
		return
	}
	if rc.InComputeExpr {
		c.refErrorf(rc, DiagWithAttachmentRef,
			"%s: reference %s cannot be resolved in a compute expression — `pkg/dsl/expr` has no attachments resolver (evalNamespaces excludes it), so the reference renders nil at evaluation; move the attachment reference to an execution sink (a tool's `command:`/`script:`/`postcondition:`, a tool action's `params:` value, or a prompt body)",
			rc.Location, rc.Ref.Raw)
		return
	}
	if _, ok := w.Attachments[name]; !ok {
		c.refErrorf(rc, DiagUnknownAttachment,
			"%s: reference %s targets undeclared attachment %q",
			rc.Location, rc.Ref.Raw, name)
		return
	}
	if len(rc.Ref.Path) >= 2 {
		sub := rc.Ref.Path[1]
		if _, ok := AttachmentSubFields[sub]; !ok {
			c.refErrorf(rc, DiagAttachmentSubfieldUnknown,
				"%s: reference %s uses unknown sub-field %q (expected one of: path, url, mime, size, sha256)",
				rc.Location, rc.Ref.Raw, sub)
		}
	}
}

func (c *compiler) validateOutputsRef(w *Workflow, rc refContext, predecessors map[string]map[string]bool) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	targetNodeID, fields := outputNodePath(w, rc.Ref.Path)

	// C029: referenced node must exist.
	targetNode := w.Nodes[targetNodeID]
	if targetNode == nil {
		c.refErrorf(rc, DiagUnknownRefNode,
			"%s: reference %s targets unknown node %q",
			rc.Location, rc.Ref.Raw, targetNodeID)
		return
	}

	// C036: referenced node must be reachable before consumer.
	if !checkReachable(rc, predecessors, targetNodeID) {
		c.refErrorf(rc, DiagRefNodeNotReachable,
			"%s: reference %s targets node %q which is not reachable before %q",
			rc.Location, rc.Ref.Raw, targetNodeID, rc.NodeID)
		return
	}

	// Field-level validation (only when accessing a specific field).
	if len(fields) == 0 {
		return
	}
	fieldName := fields[0]

	// Skip .history — already covered by C017.
	if fieldName == "history" {
		return
	}

	// Skip runtime-injected fields (e.g. _session_id) not declared in schemas.
	if isRuntimeInjectedField(fieldName) {
		return
	}

	// Nodes with a FIXED implicit output shape (no declared schema):
	// exactly those fields are valid, anything else is a hard error.
	if implicit := NodeImplicitOutputFields(targetNode); implicit != nil {
		if !slices.Contains(implicit, fieldName) {
			c.refErrorf(rc, DiagRefFieldNotInSchema,
				"%s: reference %s accesses field %q on %s node %q — its only output field(s): %s",
				rc.Location, rc.Ref.Raw, fieldName, targetNode.NodeKind(), targetNodeID, strings.Join(implicit, ", "))
		}
		return
	}

	// C032: node has no output schema — warn that field access can't be verified.
	outSchema := NodeOutputSchema(targetNode)
	if outSchema == "" {
		// A fan_out_each router declares no `output:` and cannot, so warning
		// about its per-element bindings hands the author a remedy they are
		// unable to follow. It is silenced only where the runtime guarantees
		// the binding: on a BRANCH HEAD, reading one of the element keys.
		//
		// Deliberately narrower than routerPassThroughKeys, which is an upper
		// bound built to suppress a warning on ONE edge. Read as a certificate
		// of resolvability it over-silences in three directions, each measured
		// to leak the raw template text at run time: past the join, where the
		// per-branch outputs are gone; on a key carried by one of several
		// mutually exclusive incoming edges; and on a key carried only by a
		// back-edge, absent on the first iteration.
		//
		// A node deeper in a branch than its head keeps warning. That is a
		// false positive left standing rather than a silence that cannot be
		// justified, and it is what this compiler did before the check existed.
		if r, isRouter := targetNode.(*RouterNode); isRouter &&
			routerElementKeys(r)[fieldName] && isBranchHead(w, targetNodeID, rc.NodeID) {
			return
		}
		c.refWarnf(rc, DiagRefNodeNoSchema,
			"%s: reference %s accesses field %q on node %q which has no output schema; cannot verify",
			rc.Location, rc.Ref.Raw, fieldName, targetNodeID)
		return
	}

	// C031: field must exist in the output schema.
	schema, ok := w.Schemas[outSchema]
	if !ok {
		return // already reported by C002
	}
	if findField(schema, fieldName) == nil {
		c.refErrorf(rc, DiagRefFieldNotInSchema,
			"%s: reference %s accesses field %q not found in output schema %q of node %q",
			rc.Location, rc.Ref.Raw, fieldName, outSchema, targetNodeID)
	}
}

func (c *compiler) validateVarsRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	varName := rc.Ref.Path[0]
	if _, ok := w.Vars[varName]; !ok {
		c.refErrorf(rc, DiagUndeclaredVar,
			"%s: reference %s targets undeclared variable %q",
			rc.Location, rc.Ref.Raw, varName)
	}
}

func (c *compiler) validateInputRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	fieldName := rc.Ref.Path[0]

	node, ok := w.Nodes[rc.NodeID]
	if !ok {
		return
	}

	if rc.EdgeTo != "" {
		c.validateEdgeInputRef(w, rc, node, fieldName)
		return
	}
	c.validateNodeInputRef(w, rc, node, fieldName)
}

// validateNodeInputRef is C034 for prompts, tool commands, and compute
// exprs: {{input.x}} is a field of the consuming node's input. On a
// `subbot` or `emit` `with:` the node kind has NO `input:` surface —
// the parser refuses `input:` there (`E012: unknown subbot property
// 'input'`) — so the early return "cannot verify yet" reads as "will
// never verify". The runtime resolves the reference against the
// parent's run inputs, which the CLI (`--var k=v` builds a map[string]any
// wholesale, `pkg/cli/run_inputs.go:buildRunInputs`) and the cloud
// launch path both populate with EVERY key the operator passed —
// declared as a var or not. So the reference is a real forwarding
// channel for an undeclared payload key from a parent's launch input
// into a subbot's child (which reads it as a bare `{{input.x}}` in
// the child's own prompts/commands, or through a `vars: x: string =
// "..."` default the parent's payload overrides). Absence of a key
// cannot be proven at compile time — the compiler does not know the
// launch payload — so C149 is a **warning** (philosophy: warn over
// reject when absence isn't provable); a typo still surfaces to the
// author, without shipping a permission that would refuse a
// legitimate forwarding.
func (c *compiler) validateNodeInputRef(w *Workflow, rc refContext, node Node, fieldName string) {
	inSchema := NodeInputSchema(node)
	if inSchema == "" {
		if rc.InWith {
			switch node.(type) {
			case *SubbotNode, *EmitNode:
				c.refWarnf(rc, DiagWithInputRefNoSchema,
					"%s: reference %s cannot be verified — a `%s` node has no `input:` surface, so `{{input.*}}` in its `with:` resolves against the parent's run inputs at run time. If the key is a launch-time value declared in `vars:` here, use `{{vars.%s}}` (checked at compile time); if it is forwarded from an undeclared parent payload key, keep it and be aware that a typo lands nil silently at run time (suppressing the child's declared default for the mapped key).",
					rc.Location, rc.Ref.Raw, node.NodeKind(), fieldName)
				return
			}
		}
		return
	}

	schema, ok := w.Schemas[inSchema]
	if !ok {
		return // already reported by C002
	}

	if findField(schema, fieldName) == nil {
		c.refErrorf(rc, DiagInputFieldNotInSchema,
			"%s: reference %s accesses field %q not found in input schema %q of node %q",
			rc.Location, rc.Ref.Raw, fieldName, inSchema, rc.NodeID)
	}
}

// validateEdgeInputRef is C034 for edge with-mappings: {{input.x}} is a
// field of the source node's output (the payload available when the
// edge fires). Routers copy their input onto their output (an llm
// router also records the selection). An entry router's input is the
// run payload (keys unknown at compile time); a mid-graph router's
// input is only its incoming with-keys plus mode-specific bindings, so
// a field in neither is C032 — otherwise dropping the run-input
// fallback would silently nil {{input.var}} on a router that never
// received it. Any other schemaless source also warns C032. Run-level
// inputs / vars are a different namespace ({{vars.x}}).
func (c *compiler) validateEdgeInputRef(w *Workflow, rc refContext, node Node, fieldName string) {
	if isRuntimeInjectedField(fieldName) {
		return
	}
	if implicit := NodeImplicitOutputFields(node); implicit != nil {
		if !slices.Contains(implicit, fieldName) {
			c.refErrorf(rc, DiagInputFieldNotInSchema,
				"%s: reference %s accesses field %q on %s node %q — its only output field(s): %s (edge with-mappings resolve {{input.*}} against the source node's output)",
				rc.Location, rc.Ref.Raw, fieldName, node.NodeKind(), rc.NodeID, strings.Join(implicit, ", "))
		}
		return
	}

	outSchema := NodeOutputSchema(node)
	if outSchema == "" {
		if r, isRouter := node.(*RouterNode); isRouter {
			c.validateRouterEdgeInput(w, rc, r, fieldName)
			return
		}
		msg := fmt.Sprintf("%s: reference %s accesses field %q on node %q which has no output schema; cannot verify (edge with-mappings resolve {{input.*}} against the source node's output, not run inputs)",
			rc.Location, rc.Ref.Raw, fieldName, rc.NodeID)
		if _, isVar := w.Vars[fieldName]; isVar {
			msg += fmt.Sprintf("; use {{vars.%s}} for a workflow variable", fieldName)
		}
		c.refWarnf(rc, DiagRefNodeNoSchema, "%s", msg)
		return
	}
	schema, ok := w.Schemas[outSchema]
	if !ok {
		return // already reported by C002
	}
	if findField(schema, fieldName) != nil {
		return
	}

	msg := fmt.Sprintf("%s: reference %s accesses field %q not found in output schema %q of source node %q (edge with-mappings resolve {{input.*}} against the source node's output)",
		rc.Location, rc.Ref.Raw, fieldName, outSchema, rc.NodeID)
	if _, isVar := w.Vars[fieldName]; isVar {
		msg += fmt.Sprintf("; use {{vars.%s}} for a workflow variable", fieldName)
	}
	if inSchema := NodeInputSchema(node); inSchema != "" {
		if s, ok := w.Schemas[inSchema]; ok && findField(s, fieldName) != nil {
			msg += fmt.Sprintf("; field %q is on the source node's input schema, not its output", fieldName)
		}
	}
	c.refErrorf(rc, DiagInputFieldNotInSchema, "%s", msg)
}

// validateRouterEdgeInput is C032 for a mid-graph router whose outgoing
// {{input.x}} is not in the pass-through namespace (incoming with-keys
// plus mode-specific bindings). An entry router always has the run
// payload as its input floor (even on loop re-entry), whose extra keys
// are not known at compile time, so it stays silent.
func (c *compiler) validateRouterEdgeInput(w *Workflow, rc refContext, r *RouterNode, fieldName string) {
	if rc.NodeID == w.Entry {
		return
	}
	if routerPassThroughKeys(w, r)[fieldName] {
		return
	}
	msg := fmt.Sprintf("%s: reference %s accesses field %q on router %q which does not pass it through (not supplied by an incoming with-mapping); edge with-mappings resolve {{input.*}} against the source node's output, not run inputs",
		rc.Location, rc.Ref.Raw, fieldName, rc.NodeID)
	if _, isVar := w.Vars[fieldName]; isVar {
		msg += fmt.Sprintf("; use {{vars.%s}} for a workflow variable", fieldName)
	}
	// The catalogue fix for C032 ("add an output: schema") cannot be
	// followed on a router, which has no output of its own: the remedy
	// here is to map the field onto the router or read a var.
	c.emit(SeverityWarning, DiagRefNodeNoSchema, rc.NodeID, rc.EdgeID, rc.Span,
		fmt.Sprintf("A router has no `output:` of its own — map the field onto it through an incoming edge (`… -> %s with { %s: \"…\" }`), or read `{{vars.%s}}` for a launch-time value.", rc.NodeID, fieldName, fieldName),
		"%s", msg)
}

// routerElementKeys are the per-element bindings a `fan_out_each` router puts
// in scope INSIDE a branch: the element under its `as:` name, the literal
// `item` the runtime binds alongside it, and the position pair. Nothing else
// — a `reasoning` or `selected_route` is a router OUTPUT and is not claimed
// here, and no other mode binds anything per element.
func routerElementKeys(r *RouterNode) map[string]bool {
	if r.RouterMode != RouterFanOutEach {
		return nil
	}
	bind := r.ItemBinding
	if bind == "" {
		bind = "item"
	}
	return map[string]bool{bind: true, "item": true, "index": true, "count": true}
}

// isBranchHead reports whether nodeID is a direct, non-iteration target of
// routerID — the one position where a fan-out's per-element bindings are
// guaranteed to be in scope, on every path and every iteration.
func isBranchHead(w *Workflow, routerID, nodeID string) bool {
	for _, e := range w.Edges {
		if e != nil && e.From == routerID && e.To == nodeID && !e.IsBoundedIteration() {
			return true
		}
	}
	return false
}

// routerPassThroughKeys is the set of keys a mid-graph router will have
// on its output — incoming with-keys, plus the fields each mode adds
// itself (llm selection, fan_out_each item binding).
func routerPassThroughKeys(w *Workflow, r *RouterNode) map[string]bool {
	keys := map[string]bool{}
	id := r.NodeID()
	for _, e := range w.Edges {
		if e.To != id {
			continue
		}
		for _, dm := range e.With {
			keys[dm.Key] = true
		}
	}
	switch r.RouterMode {
	case RouterLLM:
		keys["reasoning"] = true
		if r.RouterMulti {
			keys["selected_routes"] = true
		} else {
			keys["selected_route"] = true
		}
	case RouterFanOutEach:
		bind := r.ItemBinding
		if bind == "" {
			bind = "item"
		}
		keys[bind] = true
		// Runtime binds the element under BOTH the declared `as:` name
		// and the literal "item" (fan_out_each.go), so {{input.item}}
		// resolves even with a custom binding.
		keys["item"] = true
		keys["index"] = true
		keys["count"] = true
	}
	return keys
}

func (c *compiler) validateArtifactsRef(w *Workflow, rc refContext, predecessors map[string]map[string]bool, producers map[string]string) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	artifactName := rc.Ref.Path[0]

	// C035: artifact must be published by some node.
	producerID, ok := producers[artifactName]
	if !ok {
		c.refErrorf(rc, DiagUnknownArtifact,
			"%s: reference %s targets artifact %q which is not published by any node",
			rc.Location, rc.Ref.Raw, artifactName)
		return
	}

	// C036: producer must be reachable before consumer.
	if !checkReachable(rc, predecessors, producerID) {
		c.refErrorf(rc, DiagRefNodeNotReachable,
			"%s: reference %s targets artifact %q published by node %q which is not reachable before %q",
			rc.Location, rc.Ref.Raw, artifactName, producerID, rc.NodeID)
	}
}

// checkReachable reports whether targetID is reachable from rc.NodeID's
// predecessor set. When predecessors has no entry for rc.NodeID (reachability
// wasn't computed for it), the check is skipped and this returns true — the
// caller then does not error. For edge with-mappings, the source node itself
// has finished, so it and its predecessors are all available (IncludeSelf).
func checkReachable(rc refContext, predecessors map[string]map[string]bool, targetID string) bool {
	preds, ok := predecessors[rc.NodeID]
	if !ok {
		return true
	}
	reachable := preds[targetID]
	if !reachable && rc.IncludeSelf && targetID == rc.NodeID {
		reachable = true
	}
	return reachable
}

// outputNodePath uses the same longest-node-prefix rule as runtime templates
// and expressions, so an instance ID such as r1.gate is not split into a node
// named r1 and a field named gate.
func outputNodePath(w *Workflow, path []string) (string, []string) {
	for n := len(path); n > 0; n-- {
		id := strings.Join(path[:n], ".")
		if w.Nodes[id] != nil {
			return id, path[n:]
		}
	}
	if len(path) == 0 {
		return "", nil
	}
	return path[0], path[1:]
}
