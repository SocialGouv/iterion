package ir

import (
	"fmt"
	"net/netip"
	"path"
	"slices"
	"strings"

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
}

// collectAllRefs gathers every template reference in the workflow together
// with the node that consumes it.
func collectAllRefs(w *Workflow) []refContext {
	// Build reverse map: prompt name → list of consuming node IDs.
	promptUsers := make(map[string][]string)
	for _, n := range w.Nodes {
		for _, pname := range NodePromptRefs(n) {
			promptUsers[pname] = append(promptUsers[pname], n.NodeID())
		}
	}

	var out []refContext

	// Prompt template refs.
	for _, p := range w.Prompts {
		consumers := promptUsers[p.Name]
		for _, ref := range p.TemplateRefs {
			for _, nodeID := range consumers {
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   nodeID,
					Location: fmt.Sprintf("prompt %q (node %q)", p.Name, nodeID),
				})
			}
		}
	}

	// Edge with-mapping refs. The mapping is evaluated when the edge
	// fires, so the source (From) has already produced its output.
	// {{input.x}} in that mapping is the source output (a router copies
	// its input to its output, which is how pass-through works) — not
	// the source input schema, and not a silent fallback to run-level
	// inputs. C034 checks the source output schema; use {{vars.x}} for
	// a launch-time value. IncludeSelf so {{outputs.<source>}} is
	// reachable too.
	for _, e := range w.Edges {
		for _, dm := range e.With {
			for _, ref := range dm.Refs {
				out = append(out, refContext{
					Ref:         ref,
					NodeID:      e.From,
					Location:    fmt.Sprintf("edge %s -> %s, with %q", e.From, e.To, dm.Key),
					IncludeSelf: true,
					EdgeTo:      e.To,
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
					Ref:      ref,
					NodeID:   cn.ID,
					Location: fmt.Sprintf("compute node %q expr %q", cn.ID, e.Key),
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
	refs := collectAllRefs(w)
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
		}
	}
}

// validateSecretsRef flags a {{secrets.X}} reference whose secret X is
// not declared in the workflow's `secrets:` block.
func (c *compiler) validateSecretsRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	name := rc.Ref.Path[0]
	secret, ok := w.Secrets[name]
	if !ok {
		c.errorf(DiagUnknownSecret,
			"%s: reference %s targets undeclared secret %q",
			rc.Location, rc.Ref.Raw, name)
		return
	}
	if len(rc.Ref.Path) == 1 {
		return
	}
	sub := rc.Ref.Path[1]
	if sub != "path" {
		c.errorf(DiagSecretSubfield,
			"%s: reference %s uses unknown secret sub-field %q (expected: path)",
			rc.Location, rc.Ref.Raw, sub)
		return
	}
	if !secret.IsFile() {
		c.errorf(DiagSecretSubfield,
			"%s: reference %s uses .path on non-file secret %q",
			rc.Location, rc.Ref.Raw, name)
	}
}

func (c *compiler) validateAttachmentsRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	name := rc.Ref.Path[0]
	if _, ok := w.Attachments[name]; !ok {
		c.errorf(DiagUnknownAttachment,
			"%s: reference %s targets undeclared attachment %q",
			rc.Location, rc.Ref.Raw, name)
		return
	}
	if len(rc.Ref.Path) >= 2 {
		sub := rc.Ref.Path[1]
		if _, ok := AttachmentSubFields[sub]; !ok {
			c.errorf(DiagAttachmentSubfieldUnknown,
				"%s: reference %s uses unknown sub-field %q (expected one of: path, url, mime, size, sha256)",
				rc.Location, rc.Ref.Raw, sub)
		}
	}
}

func (c *compiler) validateOutputsRef(w *Workflow, rc refContext, predecessors map[string]map[string]bool) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	targetNodeID := rc.Ref.Path[0]

	// C029: referenced node must exist.
	targetNode, ok := w.Nodes[targetNodeID]
	if !ok {
		c.errorf(DiagUnknownRefNode,
			"%s: reference %s targets unknown node %q",
			rc.Location, rc.Ref.Raw, targetNodeID)
		return
	}

	// C036: referenced node must be reachable before consumer.
	if !checkReachable(rc, predecessors, targetNodeID) {
		c.errorf(DiagRefNodeNotReachable,
			"%s: reference %s targets node %q which is not reachable before %q",
			rc.Location, rc.Ref.Raw, targetNodeID, rc.NodeID)
		return
	}

	// Field-level validation (only when accessing a specific field).
	if len(rc.Ref.Path) < 2 {
		return
	}
	fieldName := rc.Ref.Path[1]

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
			c.errorf(DiagRefFieldNotInSchema,
				"%s: reference %s accesses field %q on %s node %q — its only output field(s): %s",
				rc.Location, rc.Ref.Raw, fieldName, targetNode.NodeKind(), targetNodeID, strings.Join(implicit, ", "))
		}
		return
	}

	// C032: node has no output schema — warn that field access can't be verified.
	outSchema := NodeOutputSchema(targetNode)
	if outSchema == "" {
		c.warnf(DiagRefNodeNoSchema,
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
		c.errorf(DiagRefFieldNotInSchema,
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
		c.errorf(DiagUndeclaredVar,
			"%s: reference %s targets undeclared variable %q",
			rc.Location, rc.Ref.Raw, varName)
	}
}

func (c *compiler) validateInputRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	fieldName := rc.Ref.Path[0]
	if isRuntimeInjectedField(fieldName) {
		return
	}

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
// exprs: {{input.x}} is a field of the consuming node's input.
func (c *compiler) validateNodeInputRef(w *Workflow, rc refContext, node Node, fieldName string) {
	inSchema := NodeInputSchema(node)
	if inSchema == "" {
		return
	}

	schema, ok := w.Schemas[inSchema]
	if !ok {
		return // already reported by C002
	}

	if findField(schema, fieldName) == nil {
		c.errorf(DiagInputFieldNotInSchema,
			"%s: reference %s accesses field %q not found in input schema %q of node %q",
			rc.Location, rc.Ref.Raw, fieldName, inSchema, rc.NodeID)
	}
}

// validateEdgeInputRef is C034 for edge with-mappings: {{input.x}} is a
// field of the source node's output (the payload available when the
// edge fires). A router has no output schema and copies its input to
// its output, so the check is skipped there — runtime still resolves
// from that pass-through map. Run-level inputs / vars are a different
// namespace ({{vars.x}}).
func (c *compiler) validateEdgeInputRef(w *Workflow, rc refContext, node Node, fieldName string) {
	if implicit := NodeImplicitOutputFields(node); implicit != nil {
		if !slices.Contains(implicit, fieldName) {
			c.errorf(DiagInputFieldNotInSchema,
				"%s: reference %s accesses field %q on %s node %q — its only output field(s): %s (edge with-mappings resolve {{input.*}} against the source node's output)",
				rc.Location, rc.Ref.Raw, fieldName, node.NodeKind(), rc.NodeID, strings.Join(implicit, ", "))
		}
		return
	}

	outSchema := NodeOutputSchema(node)
	if outSchema == "" {
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
	c.errorf(DiagInputFieldNotInSchema, "%s", msg)
}

func (c *compiler) validateArtifactsRef(w *Workflow, rc refContext, predecessors map[string]map[string]bool, producers map[string]string) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	artifactName := rc.Ref.Path[0]

	// C035: artifact must be published by some node.
	producerID, ok := producers[artifactName]
	if !ok {
		c.errorf(DiagUnknownArtifact,
			"%s: reference %s targets artifact %q which is not published by any node",
			rc.Location, rc.Ref.Raw, artifactName)
		return
	}

	// C036: producer must be reachable before consumer.
	if !checkReachable(rc, predecessors, producerID) {
		c.errorf(DiagRefNodeNotReachable,
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
