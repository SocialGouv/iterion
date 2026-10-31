package ir

import (
	"fmt"
	"net/netip"
	"path"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
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
	// `secrets` / `attachments` and reads `input.*` against the RUN's
	// inputs — the launch payload — on kinds (subbot, emit, fail) that
	// have no `input:` surface. Namespaces the site cannot honour are
	// refused (C149–C151) instead of resolving to nil at run time, and
	// the `input` namespace is C149's business there, never C308's: no
	// incoming edge's `with` can feed a text the node's input never
	// reaches.
	InWith bool
	// OnQuestions is true for a reference in a node's INTERACTION prompt
	// (`interaction_prompt:`). The stand-in model that answers the node's
	// questions is handed THE QUESTIONS as its input — keys the asking node
	// invents at run time, assembled into a synthetic input map
	// (pkg/backend/model/executor_human.go, ExecuteHumanLLMForInteraction).
	// So `{{input.X}}` there names a question key: neither a field of the
	// node's input schema (C034) nor a key an incoming edge maps (C308),
	// and nothing static can know it — the silence the entry floor keeps.
	OnQuestions bool
	// InComputeExpr is true for a reference that lives inside a compute
	// node's `expr:` block. Compute expressions run through
	// `pkg/dsl/expr`, whose `evalNamespaces` (snapshot.go) excludes the
	// `secrets` and `attachments` namespaces on purpose; a `{{secrets.X}}`
	// or `{{attachments.X}}` in a compute expr therefore renders nil at
	// evaluation time, same defect class as the mapping refusal, so C150
	// and C151 fire there too. The check stays separate from InWith so
	// the two positions can carry their own phrasing.
	InComputeExpr bool
	// Sink names the materialization sink the text that holds the reference
	// feeds (pkg/dsl/secretsink): a prompt body, a tool node's authored
	// command/postcondition/script, an action's params. Empty where the
	// site materialises nothing (a data mapping, a compute expression, an
	// edge condition) — those have their own refusals. A `{{secrets.X}}`
	// reference is refused where its secret does not grant the sink.
	Sink string
	// Prompt names the prompt whose body holds the reference, when it is a
	// prompt's: one prompt's references are read by every node that names
	// it, and validating each reference once PER READER drew R × N
	// diagnostics (#2779) — so the readers of one reference are validated
	// each (the verdict reads the reader's schema and its place in the
	// graph) and the findings that come out the same for several readers
	// merge into one that names them (validateSharedPromptRef).
	Prompt string
	// display is the reader's name as a finding composes it: readerMark
	// while a shared prompt's reference is validated against each reader —
	// the merge substitutes the readers the finding is said of — empty
	// otherwise, and displayID is NodeID.
	display string
}

// displayID is the consuming node's name as a diagnostic composes it: the
// reader mark while a shared prompt's reference is validated against each
// of its readers (validateSharedPromptRef), the node's own id otherwise.
func (rc refContext) displayID() string {
	if rc.display != "" {
		return rc.display
	}
	return rc.NodeID
}

// refErrorf / refWarnf emit a template-reference diagnostic with the
// context's full attribution: node, edge and the span of the text that
// carries the reference.
func (c *compiler) refErrorf(rc refContext, code DiagCode, format string, args ...any) {
	c.emit(SeverityError, code, rc.NodeID, rc.EdgeID, rc.Span, "", "", format, args...)
}

func (c *compiler) refWarnf(rc refContext, code DiagCode, format string, args ...any) {
	c.emit(SeverityWarning, code, rc.NodeID, rc.EdgeID, rc.Span, "", "", format, args...)
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
	// stays the attribution (what the reference resolves against) — and for
	// the node's INTERACTION prompt that is the question map, not the input
	// (OnQuestions).
	for _, p := range w.Prompts {
		consumers := promptUsers[p.Name]
		for _, ref := range p.TemplateRefs {
			for _, nodeID := range consumers {
				out = append(out, refContext{
					Ref:         ref,
					NodeID:      nodeID,
					Location:    fmt.Sprintf("prompt %q (node %q)", p.Name, nodeID),
					Span:        promptSpans[p.Name],
					OnQuestions: interactionPromptOnly(w.Nodes[nodeID], p.Name),
					Sink:        secretsink.Prompt,
					Prompt:      p.Name,
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
		// Edge `when` expressions. Only the run/loop kinds flow from here:
		// the edge's field conditions (`when not ok`) are validateConditionFields'
		// territory with different output semantics, and a double report would
		// fire its diagnostics twice. The expr namespaces the evaluator DOES
		// walk (`run`, `loop`) were never walked at all — `noise: run.tree_nose`
		// compiled clean — so validateRunRef (C153) and validateLoopRef (C147)
		// now see them. A `when` on the edge that DECLARES the loop is safe:
		// `w.Loops` is fully populated before validation runs, so the
		// self-edge's own loop resolves.
		if e.Expression != nil {
			for _, r := range e.Expression.Refs() {
				ref := refFromExpr(r)
				if ref == nil || (ref.Kind != RefRun && ref.Kind != RefLoop) {
					continue
				}
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   e.From,
					Location: fmt.Sprintf("edge %s -> %s, when %q", e.From, e.To, expr.Excerpt(e.ExpressionSrc)),
					Span:     edgeSpans[e],
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
					Sink:     secretsink.Command,
				})
			}
			for _, ref := range t.ScriptRefs {
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   t.ID,
					Location: fmt.Sprintf("tool node %q script", t.ID),
					Sink:     secretsink.Command,
				})
			}
			// The postcondition is rendered exactly as the command is
			// (postconditionBody) and materialises the same secrets, so it is
			// the same sink — and its references are validated here like the
			// command's, instead of resolving a typo at the one moment the
			// recipe's verdict depends on it.
			for _, ref := range t.PostcondRefs {
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   t.ID,
					Location: fmt.Sprintf("tool node %q postcondition", t.ID),
					Sink:     secretsink.Command,
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
						Sink:     secretsink.Params,
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
// alongside prompt / edge / tool refs. `run` and `loop` flow through
// too (validateRunRef / validateLoopRef — a `noise: run.tree_nose`
// used to compile clean); the edge-`when` walk keeps only those two
// kinds, so field conditions keep validateConditionFields' semantics.
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
	case "run":
		kind = RefRun
	case "loop":
		kind = RefLoop
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

// predecessorIndex answers "which nodes can execute before X" on demand.
// Materialising every node's transitive predecessor set is quadratic in the
// node count (2.0 GB at 8 000 nodes, OOM at 16 000 — #2776), so the sets are
// computed per consumer that actually queries reachability and memoised: a
// workflow whose caps and references never ask pays the reverse-adjacency
// build alone, which is linear. The sets themselves follow ALL edges
// (including conditional and loop back-edges) to ensure zero false positives.
type predecessorIndex struct {
	revAdj map[string][]string
	// loopTargets are the targets of loop back-edges: such a node is
	// effectively its own predecessor because a prior iteration's output is
	// available on re-entry.
	loopTargets map[string]bool
	known       map[string]bool
	memo        map[string]map[string]bool
}

func newPredecessorIndex(w *Workflow) *predecessorIndex {
	p := &predecessorIndex{
		revAdj:      make(map[string][]string),
		loopTargets: make(map[string]bool),
		known:       make(map[string]bool, len(w.Nodes)),
		memo:        make(map[string]map[string]bool),
	}
	for _, e := range w.Edges {
		p.revAdj[e.To] = append(p.revAdj[e.To], e.From)
		if e.LoopName != "" {
			p.loopTargets[e.To] = true
		}
	}
	for id := range w.Nodes {
		p.known[id] = true
	}
	return p
}

// sets returns the transitive predecessor set of nodeID, computing it once
// per queried node. ok is false when nodeID is not a node of the workflow:
// the caller then skips the reachability check, as the materialised map did
// when it carried no entry.
func (p *predecessorIndex) sets(nodeID string) (map[string]bool, bool) {
	if !p.known[nodeID] {
		return nil, false
	}
	preds, ok := p.memo[nodeID]
	if !ok {
		preds = computePredecessors(nodeID, p.revAdj)
		if p.loopTargets[nodeID] {
			preds[nodeID] = true
		}
		p.memo[nodeID] = preds
	}
	return preds, true
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
	c.validateSecretPlaceholders(w)
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
		c.validateSecretSinks(name, s)
	}
}

// validateSecretSinks checks a secret's `sinks:` grants against the
// vocabulary and against the declaration's own shape: a grant must name a
// sink that exists, once; a file secret must grant `file` when it grants
// anything (the mount IS its materialisation), and a value secret cannot
// grant `file` (nothing mounts it). An absent list grants every sink
// (ADR-146, axis 2) and is never refused here.
func (c *compiler) validateSecretSinks(name string, s *Secret) {
	if len(s.Sinks) == 0 {
		// No list, or the empty one: every sink (the parser reads `[]` as
		// no list, so no writer or transport can change its meaning).
		return
	}
	seen := map[string]bool{}
	for _, sink := range s.Sinks {
		if !secretsink.Known(sink) {
			c.errorf(DiagSecretSinkInvalid,
				"secret %q: sinks entry %q is not a materialization sink (known: %s)", name, sink, secretsink.List())
			continue
		}
		if seen[sink] {
			c.errorf(DiagSecretSinkInvalid,
				"secret %q: sinks names %q twice", name, sink)
		}
		seen[sink] = true
	}
	if s.IsFile() && !seen[secretsink.File] {
		c.errorf(DiagSecretSinkInvalid,
			"secret %q: as: file materialises the secret as a mounted file, which its sinks do not grant — add `file` to sinks: [%s], or drop the list", name, strings.Join(s.Sinks, ", "))
	}
	if !s.IsFile() && seen[secretsink.File] {
		c.errorf(DiagSecretSinkInvalid,
			"secret %q: sinks grants `file`, but the secret is a value (as: value) that nothing mounts — declare as: file, or drop `file` from the list", name)
	}
	// `prompt` hands a model the placeholder and resolves nothing: a list
	// that names it alone grants the secret to no sink that spends it, and
	// the run would carry a placeholder nothing resolves.
	if len(seen) == 1 && seen[secretsink.Prompt] {
		c.errorf(DiagSecretSinkInvalid,
			"secret %q: sinks names `prompt` alone, which hands a model the placeholder and resolves it nowhere — add the sink that spends the secret (agent, command, params, file or egress)", name)
	}
	// An explicit `egress` grant with no `hosts:` is substituted toward ANY
	// host, and the content DLP has no approval list to refuse against: the
	// author asked for the wire and left it open. A warning, not a refusal —
	// a secret with no `sinks:` has always gone toward any host.
	if seen[secretsink.Egress] && len(s.Hosts) == 0 {
		c.warnf(DiagSecretEgressUnscoped,
			"secret %q: sinks grants `egress` with no hosts: — the sandbox proxy substitutes the placeholder toward ANY host it is sent to; scope it with hosts: [\"api.example.com\"]", name)
	}
}

// validateSecretPlaceholders refuses two declarations whose names derive the
// same placeholder (`tok` and `_tok` both become `__ITERION_SECRET_tok__`):
// the guard would register one value under the token both references
// render, and the grants of one would govern the other's placeholder.
func (c *compiler) validateSecretPlaceholders(w *Workflow) {
	if w == nil || len(w.Secrets) == 0 {
		return
	}
	names := make([]string, 0, len(w.Secrets))
	for name := range w.Secrets {
		names = append(names, name)
	}
	slices.Sort(names)
	owner := map[string]string{}
	for _, name := range names {
		ph := secretsink.PlaceholderFor(name)
		if first, taken := owner[ph]; taken {
			c.errorf(DiagDuplicateSecret,
				"secrets %q and %q share the placeholder %s — rename one so each declaration renders its own token", first, name, ph)
			continue
		}
		owner[ph] = name
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

	predecessors := newPredecessorIndex(w)
	artifactProducers := buildArtifactProducers(w)

	// The references of one prompt stand consecutive in refs, one per reader
	// (collectAllRefs): validated against each reader — the verdict reads
	// the reader — and merged where the verdict is shared (validateSharedPromptRef).
	for i := 0; i < len(refs); {
		if refs[i].Prompt == "" {
			c.validateOneRef(w, refs[i], predecessors, artifactProducers)
			i++
			continue
		}
		j := i
		for j < len(refs) && refs[j].Prompt == refs[i].Prompt && refs[j].Ref == refs[i].Ref {
			j++
		}
		c.validateSharedPromptRef(w, refs[i:j], predecessors, artifactProducers)
		i = j
	}
}

func (c *compiler) validateOneRef(w *Workflow, rc refContext, predecessors *predecessorIndex, artifactProducers map[string]string) {
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

// readerMark stands for the reading node while one reference of a shared
// prompt is validated against each of its readers: composed into the
// finding wherever the reader's id would go, so two readers draw
// byte-identical diagnostics exactly when the verdict is the same for both,
// and those merge into one naming the readers (validateSharedPromptRef). It is no
// identifier (`‹` opens none), so it can never be a name the author wrote.
const readerMark = "‹reader›"

// maxReadersNamed is how many of a shared finding's readers are named; the
// rest are counted.
const maxReadersNamed = 32

// validateSharedPromptRef validates one reference of a prompt against each of its
// readers. The validation is the reader's — the same reference is clean
// against one reader's schema and refused against another's — but the
// FINDING is shared when the verdict is: each reader is validated in turn,
// the reader marked (readerMark) in what it draws, and the findings
// identical for several readers are emitted once, the mark replaced by the
// readers' names. One reader is no merge: its finding is said as it ever
// was. R references of a prompt read by N nodes draw O(R) findings, not the
// R × N that made a valid 65 KB bot OOM `validate --json` (#2779).
func (c *compiler) validateSharedPromptRef(w *Workflow, rcs []refContext, predecessors *predecessorIndex, artifactProducers map[string]string) {
	if len(rcs) == 1 {
		c.validateOneRef(w, rcs[0], predecessors, artifactProducers)
		return
	}
	// A fixed reader order: the nodes map's iteration is random, and the
	// first reader's id is the merged finding's attribution.
	slices.SortFunc(rcs, func(a, b refContext) int { return strings.Compare(a.NodeID, b.NodeID) })
	type finding struct {
		d       Diagnostic
		readers []string
	}
	var order []finding
	index := map[string]int{}
	for _, rc := range rcs {
		rc.display = readerMark
		rc.Location = fmt.Sprintf("prompt %q (node %q)", rc.Prompt, readerMark)
		base := len(c.diags)
		c.validateOneRef(w, rc, predecessors, artifactProducers)
		drawn := c.diags[base:]
		c.diags = c.diags[:base]
		for _, d := range drawn {
			key := fmt.Sprintf("%s\x00%d\x00%s\x00%d\x00%d\x00%s\x00%s\x00%s\x00%s",
				d.Code, d.Severity, d.File, d.Line, d.Column, d.EdgeID, d.Property, d.Message, d.Hint)
			if at, ok := index[key]; ok {
				if !slices.Contains(order[at].readers, rc.NodeID) {
					order[at].readers = append(order[at].readers, rc.NodeID)
				}
				continue
			}
			index[key] = len(order)
			order = append(order, finding{d: d, readers: []string{rc.NodeID}})
		}
	}
	for _, f := range order {
		quoted, bare := readerNames(f.readers)
		msg := strings.ReplaceAll(f.d.Message, `"`+readerMark+`"`, quoted)
		msg = strings.ReplaceAll(msg, readerMark, bare)
		hint := strings.ReplaceAll(f.d.Hint, `"`+readerMark+`"`, quoted)
		hint = strings.ReplaceAll(hint, readerMark, bare)
		sp := ast.Span{Start: ast.Pos{File: f.d.File, Line: f.d.Line, Column: f.d.Column}}
		c.emit(f.d.Severity, f.d.Code, f.readers[0], f.d.EdgeID, sp, f.d.Property, hint, "%s", msg)
	}
}

// readerNames is how a merged finding names its readers: quoted — `"a",
// "b"` — where the sentence quotes the name, bare — `a, b` — where it does
// not (an edge list's `src -> dst`). One reader is named as it ever was;
// past maxReadersNamed the first are named and the rest counted.
func readerNames(readers []string) (quoted, bare string) {
	if len(readers) == 1 {
		return `"` + readers[0] + `"`, readers[0]
	}
	named := readers
	more := ""
	if len(named) > maxReadersNamed {
		named = named[:maxReadersNamed]
		more = fmt.Sprintf(" and %d more", len(readers)-maxReadersNamed)
	}
	quoted = `"` + strings.Join(named, `", "`) + `"`
	return quoted + more, strings.Join(named, ", ") + more
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
			rc.Location, expr.Excerpt(rc.Ref.Raw))
		return
	}
	name, field := rc.Ref.Path[0], rc.Ref.Path[1]
	if w.Loops[name] == nil {
		c.refWarnf(rc, DiagUnknownLoopRef,
			"%s: reference %s targets undeclared loop %q",
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(name))
		return
	}
	switch field {
	case "iteration", "max":
		if len(rc.Ref.Path) > 2 {
			c.refWarnf(rc, DiagUnknownLoopRef,
				"%s: reference %s: loop.%s.%s has no sub-field",
				rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(name), expr.Excerpt(field))
		}
	case "previous_output":
	default:
		c.refWarnf(rc, DiagUnknownLoopRef,
			"%s: reference %s uses unknown loop field %q (expected: iteration, max, previous_output)",
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(field))
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
					rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(member))
			}
			return
		}
	}
	c.refWarnf(rc, DiagUnknownRunMember,
		"%s: reference %s uses unknown run member %q (known: %s)",
		rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(member), strings.Join(RunMembers, ", "))
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
			rc.Location, expr.Excerpt(rc.Ref.Raw))
		return
	}
	if rc.InComputeExpr {
		c.refErrorf(rc, DiagWithSecretRef,
			"%s: reference %s cannot be resolved in a compute expression — `pkg/dsl/expr` has no secrets resolver (evalNamespaces excludes it), so the reference renders nil at evaluation; move the secret to an execution sink that materialises it (a tool's `command:`/`script:`/`postcondition:`, a tool action's `params:` value, or a prompt body)",
			rc.Location, expr.Excerpt(rc.Ref.Raw))
		return
	}
	secret, ok := w.Secrets[name]
	if !ok {
		c.refErrorf(rc, DiagUnknownSecret,
			"%s: reference %s targets undeclared secret %q",
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(name))
		return
	}
	if len(rc.Ref.Path) == 1 {
		// The grant check: a VALUE secret's reference materialises the value
		// at the sink the text feeds, so the secret must grant that sink. A
		// file secret's reference renders its mounted path wherever it
		// stands — the value reaches the run through the mount, which the
		// `file` grant on the declaration covers — so it is not checked per
		// reference. A sub-field reference is judged below first: one the
		// secret has not (C095) is refused for that alone.
		if rc.Sink != "" && !secret.IsFile() && !secret.Grants(rc.Sink) {
			c.refErrorf(rc, DiagSecretSinkUngranted,
				"%s: reference %s reaches the %s sink, which secret %q does not grant (sinks: [%s]) — add `%s` to its sinks: list, or move the reference to a sink it grants",
				rc.Location, expr.Excerpt(rc.Ref.Raw), rc.Sink, expr.Excerpt(name), strings.Join(secret.Sinks, ", "), rc.Sink)
		}
		return
	}
	sub := rc.Ref.Path[1]
	if sub != "path" {
		c.refErrorf(rc, DiagSecretSubfield,
			"%s: reference %s uses unknown secret sub-field %q (expected: path)",
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(sub))
		return
	}
	if !secret.IsFile() {
		c.refErrorf(rc, DiagSecretSubfield,
			"%s: reference %s uses .path on non-file secret %q",
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(name))
	}
}

// validateAttachmentsRef flags an undeclared {{attachments.X}} reference
// or an unknown sub-field. In a compute node's `expr:` the reference is
// refused: `pkg/dsl/expr` has no attachments resolver (evalNamespaces
// excludes the namespace), so it would render nil silently. A data mapping
// — an edge / subbot / emit `with:` value, or a `fail message:` — is fine:
// `resolveRef` resolves the namespace there like a prompt body does (#2864).
// Execution sinks that DO materialise an attachment: a tool's
// `command:`/`script:`/`postcondition:`, or a prompt body.
func (c *compiler) validateAttachmentsRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	name := rc.Ref.Path[0]
	if rc.InComputeExpr {
		c.refErrorf(rc, DiagWithAttachmentRef,
			"%s: reference %s cannot be resolved in a compute expression — `pkg/dsl/expr` has no attachments resolver (evalNamespaces excludes it), so the reference renders nil at evaluation; move the attachment reference to an execution sink (a tool's `command:`/`script:`/`postcondition:`, a tool action's `params:` value, or a prompt body)",
			rc.Location, expr.Excerpt(rc.Ref.Raw))
		return
	}
	if _, ok := w.Attachments[name]; !ok {
		c.refErrorf(rc, DiagUnknownAttachment,
			"%s: reference %s targets undeclared attachment %q",
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(name))
		return
	}
	if len(rc.Ref.Path) >= 2 {
		sub := rc.Ref.Path[1]
		if _, ok := AttachmentSubFields[sub]; !ok {
			c.refErrorf(rc, DiagAttachmentSubfieldUnknown,
				"%s: reference %s uses unknown sub-field %q (expected one of: path, url, mime, size, sha256)",
				rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(sub))
		}
	}
}

func (c *compiler) validateOutputsRef(w *Workflow, rc refContext, predecessors *predecessorIndex) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	targetNodeID, fields := outputNodePath(w, rc.Ref.Path)

	// C029: referenced node must exist.
	targetNode := w.Nodes[targetNodeID]
	if targetNode == nil {
		c.refErrorf(rc, DiagUnknownRefNode,
			"%s: reference %s targets unknown node %q",
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(targetNodeID))
		return
	}

	// C036: referenced node must be reachable before consumer.
	if !checkReachable(rc, predecessors, targetNodeID) {
		c.refErrorf(rc, DiagRefNodeNotReachable,
			"%s: reference %s targets node %q which is not reachable before %q",
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(targetNodeID), rc.displayID())
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
				rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(fieldName), targetNode.NodeKind(), expr.Excerpt(targetNodeID), strings.Join(implicit, ", "))
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
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(fieldName), expr.Excerpt(targetNodeID))
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
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(fieldName), expr.Excerpt(outSchema), expr.Excerpt(targetNodeID))
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
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(varName))
	}
}

func (c *compiler) validateInputRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	if rc.OnQuestions {
		// An interaction prompt's input IS the question map, whose keys the
		// asking node invents at run time: the schema check would refuse the
		// only spelling a question key has, and the mapping check would
		// prescribe a `with` the text never reads.
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
	c.validateNodeBodyInputMapped(w, rc, node, fieldName)
}

// interactionPromptOf is the prompt a node's `interaction_prompt:` names, ""
// when it declares none — the one prompt of a node whose `{{input.*}}` the
// runtime resolves against the questions instead of the node's input.
func interactionPromptOf(n Node) string {
	switch x := n.(type) {
	case *HumanNode:
		return x.InteractionPrompt
	case LLMNode:
		if f := x.GetInteractionFields(); f != nil {
			return f.InteractionPrompt
		}
	}
	return ""
}

// interactionPromptOnly reports whether name is the node's interaction
// prompt and nothing else. A prompt the node ALSO reads as its system or
// user text is resolved against the node's own input there, so the strict
// reading stands: NodePromptRefs lists one entry per surface, so a single
// occurrence means the interaction prompt is the only reader.
func interactionPromptOnly(n Node, name string) bool {
	if name == "" || n == nil || interactionPromptOf(n) != name {
		return false
	}
	uses := 0
	for _, p := range NodePromptRefs(n) {
		if p == name {
			uses++
		}
	}
	return uses == 1
}

// validateNodeBodyInputMapped is C308 (#1455): an `{{input.X}}` a node body
// reads (a prompt, a tool command/script/param, a compute `expr:`) that NO
// incoming edge of the node maps. A node's input is built only from the
// `with` mappings of its incoming edges (buildNodeInputRS, applyEdge returns
// on len(edge.With)==0) plus the run-level payload on the entry node — so a
// field no edge maps is empty on every path, the certain defect the dry run
// could only name after a full pass. A warning, not an error: the fix is one
// `with` mapping (or `{{outputs.<producer>.X}}`), and the diagnostic names
// the first incoming edges by name and the rest by count, so the mapping
// has a place to land.
//
// The claim holds only where the text IS resolved against that input, so the
// surfaces the runtime resolves elsewhere are out: a mapping value (a
// subbot's or emit's `with:`, a fail `message:`) reads the run's inputs —
// C149 — and an interaction prompt reads the question map (rc.OnQuestions).
// Prescribing a `with` there would silence the warning and change nothing.
//
// The path-only case — mapped by another incoming edge (a loop's back-edge,
// a sibling `when` path), legitimately empty on some paths — is NOT a
// compile finding: it is a fact about a path, which the dry run tells apart
// (its `unmapped_on_path` kind, a warning).
func (c *compiler) validateNodeBodyInputMapped(w *Workflow, rc refContext, node Node, fieldName string) {
	if fieldName == "" || isRuntimeInjectedField(fieldName) {
		return
	}
	if rc.InWith {
		// A mapping value — a subbot's or an emit's `with:`, a fail node's
		// `message:` — goes through `resolveMapping`, whose `input` arm
		// reads the RUN's inputs (pkg/runtime/engine_resolve.go, resolveRef)
		// on kinds that have no `input:` surface. The `with` this check
		// prescribes would change nothing there, so the read is C149's
		// business, not an incoming-edge mapping's.
		return
	}
	if rc.NodeID == w.Entry {
		// The entry floor carries the run-level payload: keys the compiler
		// cannot know, the same silence C034 keeps.
		return
	}
	// The union of the with-keys across ALL incoming edges — a field the
	// back-edge or a sibling path maps is mapped, whatever the order the
	// edges are written in.
	var incoming []string
	mapped := false
	for _, e := range w.Edges {
		if e == nil || e.To != rc.NodeID {
			continue
		}
		incoming = append(incoming, incomingEdgeNameAs(e, rc.displayID()))
		for _, dm := range e.With {
			if dm.Key == fieldName {
				mapped = true
			}
		}
	}
	if len(incoming) == 0 {
		// No incoming edge: the entry shape aside, this is C016's (unreachable
		// node's) business — nothing is sure about any path, so nothing is
		// said here.
		return
	}
	if mapped {
		return
	}
	// The first few edges by name, the rest by count: the warning is said
	// once per unmapped reference, so naming every edge every time grew as
	// references × edges — a megabyte of input references into a node of a
	// thousand edges was a hundred megabytes of warnings.
	named := incoming
	if len(named) > maxIncomingNamed {
		named = named[:maxIncomingNamed]
	}
	quoted := make([]string, 0, len(named)+1)
	for _, name := range named {
		quoted = append(quoted, "`"+name+"`")
	}
	if more := len(incoming) - len(named); more > 0 {
		quoted = append(quoted, fmt.Sprintf("and %d more", more))
	}
	list := strings.Join(quoted, ", ")
	hint := fmt.Sprintf("map the field on an incoming edge (%s with { %s: \"{{outputs.<producer>.%s}}\" }) or read the producer's output directly ({{outputs.<producer>.%s}}); the incoming edges of %q: %s",
		quoted[0], expr.Excerpt(fieldName), expr.Excerpt(fieldName), expr.Excerpt(fieldName), rc.displayID(), list)
	c.emit(SeverityWarning, DiagInputFieldNeverMapped, rc.NodeID, rc.EdgeID, rc.Span, "", hint,
		"%s: reference %s reads a field %q that none of the node's incoming edges maps — empty on every path (incoming: %s)",
		rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(fieldName), list)
}

// maxIncomingNamed is how many of a node's incoming edges C308 names; the
// rest are counted.
const maxIncomingNamed = 5

// incomingEdgeNameAs is how a diagnostic names an edge: from -> to as the
// reader is displayed (displayID — the reader mark while a shared prompt's
// reference is validated against each of its readers), with the condition
// that guards it — two parallel edges between one pair of nodes (a `when`
// path and its sibling) must not read as one.
func incomingEdgeNameAs(e *Edge, to string) string {
	name := e.From + " -> " + to
	switch {
	case e.Condition != "":
		cond := expr.Excerpt(e.Condition)
		if e.Negated {
			cond = "not " + cond
		}
		name += " (when " + cond + ")"
	case e.ExpressionSrc != "":
		name += " (when " + expr.Excerpt(e.ExpressionSrc) + ")"
	case e.IsElse:
		name += " (else)"
	}
	return name
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
//
// A fail node's `message:` is the same reading on a third kind: it goes
// through `resolveMapping` too, a fail node accepts no `input:`, and the
// message is resolved at fail time against the run's namespaces
// (pkg/runtime/run_failure.go, failOutcome). So `{{input.x}}` there is the
// launch payload's key — measured: a run launched with `inv=…` renders it,
// a run without renders the empty string, and a `with { inv: … }` on the
// incoming edge changes neither.
func (c *compiler) validateNodeInputRef(w *Workflow, rc refContext, node Node, fieldName string) {
	inSchema := NodeInputSchema(node)
	if inSchema == "" {
		if rc.InWith {
			switch node.(type) {
			case *FailNode:
				c.refWarnf(rc, DiagWithInputRefNoSchema,
					"%s: reference %s resolves against the RUN's inputs — the launch payload — not against this node's input: a fail `message:` is rendered at fail time through the same resolver as a `with:` mapping, and a `fail` node has no `input:` surface, so no incoming edge can carry %q into it. If the value is declared in this workflow's `vars:`, use `{{vars.%s}}` (checked at compile time, and it carries the var's default); otherwise keep it and be aware that a launch that omits the key renders the empty string.",
					rc.Location, expr.Excerpt(rc.Ref.Raw), fieldName, fieldName)
				return
			case *SubbotNode, *EmitNode:
				c.refWarnf(rc, DiagWithInputRefNoSchema,
					"%s: reference %s cannot be verified — a `%s` node has no `input:` surface, so `{{input.*}}` in its `with:` resolves against the parent's run inputs at run time. If the key is a launch-time value declared in `vars:` here, use `{{vars.%s}}` (checked at compile time); if it is forwarded from an undeclared parent payload key, keep it and be aware that a typo lands nil silently at run time (suppressing the child's declared default for the mapped key).",
					rc.Location, expr.Excerpt(rc.Ref.Raw), node.NodeKind(), fieldName)
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
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(fieldName), expr.Excerpt(inSchema), rc.displayID())
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
				rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(fieldName), node.NodeKind(), rc.NodeID, strings.Join(implicit, ", "))
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
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(fieldName), rc.NodeID)
		if _, isVar := w.Vars[fieldName]; isVar {
			msg += fmt.Sprintf("; use {{vars.%s}} for a workflow variable", expr.Excerpt(fieldName))
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
		rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(fieldName), expr.Excerpt(outSchema), rc.NodeID)
	if _, isVar := w.Vars[fieldName]; isVar {
		msg += fmt.Sprintf("; use {{vars.%s}} for a workflow variable", expr.Excerpt(fieldName))
	}
	if inSchema := NodeInputSchema(node); inSchema != "" {
		if s, ok := w.Schemas[inSchema]; ok && findField(s, fieldName) != nil {
			msg += fmt.Sprintf("; field %q is on the source node's input schema, not its output", expr.Excerpt(fieldName))
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
		rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(fieldName), rc.NodeID)
	if _, isVar := w.Vars[fieldName]; isVar {
		msg += fmt.Sprintf("; use {{vars.%s}} for a workflow variable", expr.Excerpt(fieldName))
	}
	// The catalogue fix for C032 ("add an output: schema") cannot be
	// followed on a router, which has no output of its own: the remedy
	// here is to map the field onto the router or read a var.
	c.emit(SeverityWarning, DiagRefNodeNoSchema, rc.NodeID, rc.EdgeID, rc.Span, "",
		fmt.Sprintf("A router has no `output:` of its own — map the field onto it through an incoming edge (`… -> %s with { %s: \"…\" }`), or read `{{vars.%s}}` for a launch-time value.", rc.NodeID, expr.Excerpt(fieldName), expr.Excerpt(fieldName)),
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

func (c *compiler) validateArtifactsRef(w *Workflow, rc refContext, predecessors *predecessorIndex, producers map[string]string) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	artifactName := rc.Ref.Path[0]

	// C035: artifact must be published by some node.
	producerID, ok := producers[artifactName]
	if !ok {
		c.refErrorf(rc, DiagUnknownArtifact,
			"%s: reference %s targets artifact %q which is not published by any node",
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(artifactName))
		return
	}

	// C036: producer must be reachable before consumer.
	if !checkReachable(rc, predecessors, producerID) {
		c.refErrorf(rc, DiagRefNodeNotReachable,
			"%s: reference %s targets artifact %q published by node %q which is not reachable before %q",
			rc.Location, expr.Excerpt(rc.Ref.Raw), expr.Excerpt(artifactName), expr.Excerpt(producerID), rc.displayID())
	}
}

// checkReachable reports whether targetID is reachable from rc.NodeID's
// predecessor set. When the index does not know rc.NodeID (it is not a node
// of the workflow), the check is skipped and this returns true — the
// caller then does not error. For edge with-mappings, the source node itself
// has finished, so it and its predecessors are all available (IncludeSelf).
func checkReachable(rc refContext, predecessors *predecessorIndex, targetID string) bool {
	preds, ok := predecessors.sets(rc.NodeID)
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
