package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/memory"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Input resolution
// ---------------------------------------------------------------------------

// resolveScope groups the five context maps + runState that every
// reference-resolution call needs. It exists purely to flatten what was
// a 5-param tail repeated across buildNodeInputRS / resolveMapping /
// resolveRef and all their call sites — no behaviour change. Fields are
// passed by reference value (map / pointer) just as before, so callers
// can still mutate the underlying maps where they did previously
// (e.g. fan_out's `merged` maps).
type resolveScope struct {
	vars      map[string]any
	outputs   map[string]map[string]any
	runInputs map[string]any
	artifacts map[string]map[string]any
	rs        *runState
	// incomingByNode, when non-nil, is the selected-incoming map used
	// instead of rs.selectedIncoming. Fan-out branches pass their
	// private copy so concurrent writers never race the trunk map.
	incomingByNode map[string][]store.IncomingEdge
}

// scope returns a resolveScope wired straight from rs — the common
// shape "use the run's own vars/outputs/runInputs/artifacts" that the
// vast majority of call sites need. Call sites that need to override
// any field (e.g. fan_out's merged outputs, resume's per-question
// runInputs) construct a literal resolveScope{} instead.
func (rs *runState) scope() resolveScope {
	return resolveScope{
		vars:      rs.vars,
		outputs:   rs.outputs,
		runInputs: rs.runInputs,
		artifacts: rs.artifacts,
		rs:        rs,
	}
}

// buildNodeInputRS constructs the input map for a node by looking at the
// edge `with` mappings that target this node. Only incoming edges that
// routing actually selected for the current visit contribute (issue
// #484); an unselected conditional sibling whose source has already
// produced output is ignored. When no selection was recorded (legacy
// checkpoint, or a direct test of the resolver) the pre-#484 fallback
// still applies: every incoming edge whose source has output. For
// convergence points, mappings from every *selected* incoming edge are
// merged. If no mappings exist, the run-level inputs are used for the
// entry node. The runState (sc.rs) is required so that `{{loop.*}}` /
// `{{run.*}}` references resolve against the run's iteration state. Pass
// nil for sc.rs only in tests that don't exercise those namespaces.
func (e *Engine) buildNodeInputRS(nodeID string, sc resolveScope) map[string]any {
	result := make(map[string]any)

	// Entry floor: launch payload + var defaults. Incoming with-mappings
	// overlay this, and bounded-iteration back-edges still win on a
	// shared key (applied last below). Gating the floor on
	// len(result)==0 dropped the run payload on re-entry the moment a
	// back-edge carried a with-mapping, so {{input.x}} on an entry
	// router's outgoing edges went nil from iteration 2 (R7dd005).
	if nodeID == e.workflow.Entry {
		for name, v := range e.workflow.Vars {
			if v.HasDefault {
				result[name] = v.Default
			}
		}
		for k, v := range sc.runInputs {
			result[k] = v
		}
	}

	// applyEdge merges one edge's with-mappings into result. A not-yet-run
	// source never contributes (the mapping is left to a later-firing
	// edge / the entry fallback). When routing recorded a selected
	// incoming set for this visit:
	//
	//  - exclusive FORWARD siblings (#484): only the selected edges
	//    contribute, so an unselected `when`/`else` cannot clobber.
	//  - loop-head RE-ENTRY: the recorded set is the single back-edge
	//    (selectEdgeRS replaces). A back-edge is an OVERLAY of the keys
	//    that change per iteration, not a replacement of the head's
	//    whole input — unmapped keys still come from forward/entry
	//    edges whose source has output. Filtering those out leaked
	//    raw `{{input.x}}` into campaign prompts (Rae4900).
	selected, tracked := incomingFor(nodeID, sc)
	if tracked && e.workflow != nil && !incomingMatchesWorkflow(nodeID, selected, e.workflow.Edges) {
		// Stale checkpoint: resume --force / rewind re-executes against
		// an edited .bot. Filtering on identities that match no current
		// edge would drop every with-mapping (R25212d).
		tracked = false
	}
	overlayForward := tracked && incomingOnlyBounded(selected)
	applyEdge := func(edge *ir.Edge) {
		if edge.To != nodeID || len(edge.With) == 0 {
			return
		}
		if _, ok := sc.outputs[edge.From]; !ok && edge.From != "" {
			return
		}
		if tracked {
			if overlayForward && !edge.IsBoundedIteration() {
				// keep the forward pass unfiltered
			} else if !edgeInIncoming(edge, selected) {
				return
			}
		}

		// {{input.X}} in a with-mapping is the source node's output —
		// the payload available when the edge fires. A router copies
		// its input to its output, so this is also how router
		// pass-through works (an entry router's input is the run-level
		// payload). Run-level inputs and vars are a different
		// namespace: use {{vars.X}}. There is no silent fallback to
		// runInputs — that coincidence is what C034 used to disagree
		// with the resolver about (#479).
		sourceOut := sc.outputs[edge.From]
		effectiveInputs := make(map[string]any, len(sourceOut))
		for k, v := range sourceOut {
			effectiveInputs[k] = v
		}

		// Per-edge scope: same maps as the caller's scope, except
		// runInputs is the source output so {{input.*}} refs in the
		// with-mapping resolve against that namespace.
		edgeScope := sc
		edgeScope.runInputs = effectiveInputs

		for _, dm := range edge.With {
			e.warnMissingEdgeInput(edge, dm, effectiveInputs)
			val := e.resolveMapping(dm, edgeScope)
			// Include nil values too: a ref that resolves to nil
			// (e.g. `{{outputs.fixer.pushback}}` before the fixer
			// has run, `{{loop.X.previous_output}}` on iteration 1)
			// is still a *valid* mapping — the field exists, its
			// value is just empty. Dropping it would leave
			// `{{input.<key>}}` placeholders unresolved in
			// downstream prompts, surfacing template syntax to the
			// LLM instead of an empty string.
			result[dm.Key] = val
		}
	}

	// Merge with-mappings from eligible incoming edges in TWO precedence
	// passes (eligible = selected for this visit, or the untracked
	// fallback of "source has produced output"):
	//
	//  1. Non-iteration (forward / entry) edges first.
	//  2. Bounded-iteration back-edges (loop / foreach) last, so they WIN on a
	//     shared key.
	//
	// The head of a bounded loop is targeted by both its entry edge(s) AND its
	// back-edge(s). On re-entry, both an entry edge's source and the back-edge's
	// source have produced output, so a naive single-pass merge in slice order
	// lets whichever edge is later in e.workflow.Edges clobber the other for a
	// shared key. When the entry edge lands last it re-applies the loop-ENTRY
	// value every iteration, freezing a fed-back cursor/counter at its initial
	// value — the loop then spins to its bound instead of converging (observed
	// authoring bots/whole-improve-loop, worked around there via an `advance`
	// compute). The back-edge represents "the value carried into the NEXT
	// iteration" and is the authoritative source of a re-entering head's input,
	// so it must take precedence. On FIRST entry the back-edge's source hasn't
	// run yet, so it contributes nothing and the entry edge supplies the value —
	// correct in both phases. Different-key convergence merges are unaffected
	// (each edge only overwrites the keys it sets).
	for _, edge := range e.workflow.Edges {
		if !edge.IsBoundedIteration() {
			applyEdge(edge)
		}
	}
	for _, edge := range e.workflow.Edges {
		if edge.IsBoundedIteration() {
			applyEdge(edge)
		}
	}

	// The reserved permission keys are engine-owned. Run inputs are
	// caller-supplied and land wholesale on the entry node above, so a
	// launch payload naming them would otherwise seed policy allow rules
	// straight into the gate that exists to bound the caller.
	delete(result, permission.GrantInputKey)
	delete(result, permission.RunGrantsInputKey)
	// Attach the grants this node has earned. Done here rather than only
	// on the resume path so an ordinary execution and a bounded-loop
	// re-entry carry them too — a re-entry is a fresh Execute, not a
	// re-invocation, and without this the operator re-authorizes the same
	// tool every time a repair loop re-enters the node. Per node on
	// purpose: Evaluate ranks allow rules above the mode default, so
	// lending one node's grant to another would beat a `permission: deny`
	// the other node declared for itself.
	if sc.rs != nil && len(sc.rs.permissionGrants[nodeID]) > 0 {
		result[permission.RunGrantsInputKey] = append([]string(nil), sc.rs.permissionGrants[nodeID]...)
	}

	return result
}

// warnMissingEdgeInput logs when a with-mapping {{input.x}} names a
// field the source output does not have. C032 is only a compile
// warning, and `iterion run` does not print those, so without this the
// nil would splice the literal `{{input.x}}` into a tool command and
// the run would still finish green (#479 / Rd285bb).
func (e *Engine) warnMissingEdgeInput(edge *ir.Edge, dm *ir.DataMapping, sourceOut map[string]any) {
	if e.logger == nil {
		return
	}
	for _, ref := range dm.Refs {
		if ref == nil || ref.Kind != ir.RefInput || len(ref.Path) == 0 {
			continue
		}
		field := ref.Path[0]
		if len(field) > 0 && field[0] == '_' {
			continue
		}
		if _, ok := sourceOut[field]; ok {
			continue
		}
		e.logger.Warn("runtime: edge %s -> %s with %q: {{input.%s}} is not on the source node's output (no run-input fallback; use {{vars.%s}} for a launch-time value)",
			edge.From, edge.To, dm.Key, field, field)
	}
}

// resolveMapping resolves a DataMapping's references to concrete values.
//
// A template that is nothing but a single reference — "{{outputs.n.field}}"
// — passes the referenced value through with its type intact, because a
// mapping must be able to carry an object, an array, a bool or a number
// into a typed field, not just text.
//
// Every other template is interpolated: each reference is rendered into
// the surrounding literal text. Returning the reference alone would drop
// that text ("app-concept/{{outputs.seed.token}}/resolved" collapsing to
// the bare token), and returning the raw template would leave a
// multi-reference path like "{{vars.scratch_dir}}/{{run.id}}/topics"
// unresolved — both silently, as a plausible-looking value.
func (e *Engine) resolveMapping(dm *ir.DataMapping, sc resolveScope) any {
	if len(dm.Refs) == 0 {
		return dm.Raw
	}
	// Spans come from Raw rather than from each Ref's own Raw field:
	// hand-built IR (tests, generators) routinely fills DataMapping.Raw
	// and leaves Ref.Raw empty, and an empty needle would splice the
	// value in front of every character.
	spans := templateSpans(dm.Raw)
	if len(spans) != len(dm.Refs) {
		if len(dm.Refs) == 1 {
			return e.resolveRef(dm.Refs[0], sc)
		}
		return dm.Raw
	}
	if len(spans) == 1 && spans[0].start == 0 && spans[0].end == len(dm.Raw) {
		return e.resolveRef(dm.Refs[0], sc)
	}
	var b strings.Builder
	prev := 0
	for i, sp := range spans {
		b.WriteString(dm.Raw[prev:sp.start])
		b.WriteString(renderMappingValue(e.resolveRef(dm.Refs[i], sc)))
		prev = sp.end
	}
	b.WriteString(dm.Raw[prev:])
	return b.String()
}

// templateSpan marks one `{{…}}` block in a template, end-exclusive.
type templateSpan struct{ start, end int }

// templateSpans locates every `{{…}}` block, using the same left-to-right
// fence scan as ir.ParseRefs so the spans line up one-for-one with the
// refs that call produced.
func templateSpans(s string) []templateSpan {
	var spans []templateSpan
	for i := 0; ; {
		start := strings.Index(s[i:], "{{")
		if start == -1 {
			return spans
		}
		start += i
		end := strings.Index(s[start:], "}}")
		if end == -1 {
			return spans
		}
		end += start + 2
		spans = append(spans, templateSpan{start: start, end: end})
		i = end
	}
}

// renderMappingValue converts a resolved reference to the text spliced
// into an interpolated mapping template. It mirrors the substitution
// rules the prompt renderer applies (model.formatValue): strings verbatim,
// nil as empty, everything else as compact JSON so a structured value
// stays machine-readable instead of degrading to Go's %v syntax.
func renderMappingValue(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case nil:
		return ""
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}
}

// resolveRef resolves a single Ref to a concrete value. The runState
// (sc.rs) is required for `loop` and `run` namespace resolution; pass
// nil to skip those (they'll resolve to nil).
func (e *Engine) resolveRef(ref *ir.Ref, sc resolveScope) any {
	switch ref.Kind {
	case ir.RefVars:
		if len(ref.Path) > 0 {
			return sc.vars[ref.Path[0]]
		}
	case ir.RefInput:
		if len(ref.Path) > 0 {
			return sc.runInputs[ref.Path[0]]
		}
	case ir.RefOutputs:
		if len(ref.Path) == 0 {
			return nil
		}
		// Resolve the node id as the LONGEST dotted prefix of the path that is
		// an actual output key. Group-instance nodes have dotted ids
		// (`prefix.name`), which collide with the dotted ref grammar:
		// {{outputs.r1.gate.id}} parses as [r1, gate, id] but the node is
		// "r1.gate". Longest-prefix-match disambiguates this for any nesting
		// depth (the field path is whatever follows the matched id).
		nodeOut, fieldPath := matchOutputNode(sc.outputs, ref.Path)
		if nodeOut == nil {
			return nil
		}
		if len(fieldPath) == 0 {
			return nodeOut
		}
		if len(fieldPath) == 1 {
			return nodeOut[fieldPath[0]]
		}
		// Deep path {{outputs.node.field.sub…}} — drill into nested maps
		// (e.g. a per-item object surfaced by fan_out_each:
		// {{outputs.dispatch.item.is_code}}).
		return drillPath(nodeOut[fieldPath[0]], fieldPath[1:])
	case ir.RefArtifacts:
		if len(ref.Path) > 0 {
			return sc.artifacts[ref.Path[0]]
		}
	case ir.RefLoop:
		if sc.rs == nil || len(ref.Path) < 2 {
			return nil
		}
		return e.resolveLoopPath(ref.Path, sc.rs)
	case ir.RefEach:
		if sc.rs == nil || len(ref.Path) < 2 {
			return nil
		}
		return e.resolveEachPath(ref.Path, sc)
	case ir.RefRun:
		if sc.rs == nil || len(ref.Path) == 0 {
			return nil
		}
		switch ref.Path[0] {
		case "id":
			return sc.rs.runID
		}
	}
	return nil
}

// resolveEachPath resolves a {{each.<name>.<field>[.subfield…]}} reference for
// a sequential foreach. Recognized fields:
//
//	item   — the current element (drills into sub-fields for object elements)
//	index  — current 0-based position (int64)
//	count  — collection length (int64)
//	first  — index == 0 (bool)
//	last   — index >= count-1, or count == 0 (bool)
//	empty  — count == 0 (bool)
func (e *Engine) resolveEachPath(path []string, sc resolveScope) any {
	name := path[0]
	fe, ok := e.workflow.Foreaches[name]
	if !ok {
		return nil
	}
	coll := e.resolveForeachCollection(fe, sc)
	idx := runStateIterationCounters(sc.rs)[foreachCounterKey(name)]
	count := len(coll)
	switch path[1] {
	case "item":
		if idx < 0 || idx >= count {
			return nil
		}
		if len(path) > 2 {
			return drillPath(coll[idx], path[2:])
		}
		return coll[idx]
	case "index":
		return int64(idx)
	case "count":
		return int64(count)
	case "first":
		return idx == 0
	case "last":
		return count == 0 || idx >= count-1
	case "empty":
		return count == 0
	}
	return nil
}

// foreachCounterKey namespaces a foreach's index inside the shared
// rs.loopCounters map so a foreach name can never collide with a loop of the
// same name (the "/" can't appear in a DSL identifier).
func foreachCounterKey(name string) string { return "foreach/" + name }

// resolveForeachCollection resolves a foreach's collection template to a slice,
// reusing the same coercion as fan_out_each (handles []interface{}, a
// JSON-string array, and reflected slices). A non-array resolves to nil, which
// foreach treats as an empty collection.
func (e *Engine) resolveForeachCollection(fe *ir.Foreach, sc resolveScope) []any {
	if len(fe.CollectionRefs) == 0 {
		return nil
	}
	arr, err := coerceToArray(e.resolveRef(fe.CollectionRefs[0], sc), fe.Name, fe.CollectionRaw)
	if err != nil {
		return nil
	}
	return arr
}

// resolveLoopPath resolves a {{loop.<name>.<field>[.subfield…]}} reference.
// Recognized fields:
//
//	iteration       — current loop counter (int64)
//	max             — effective cap (int64): the literal int for plain
//	                   caps, the resolved template value for templated caps
//	previous_output — snapshot of the source node output at the previous
//	                   traversal of this loop's edge; sub-fields drill in.
func (e *Engine) resolveLoopPath(path []string, rs *runState) any {
	loopName := path[0]
	switch path[1] {
	case "iteration":
		return int64(runStateIterationCounters(rs)[loopName])
	case "max":
		if l, ok := e.workflow.Loops[loopName]; ok {
			return int64(e.resolveLoopMax(l, rs))
		}
		return nil
	case "previous_output":
		return drillPath(loopPreviousOutputView(rs)[loopName], path[2:])
	}
	return nil
}

// resolveLoopMax returns the effective cap for a loop. Literal-int
// declarations (`as fix_loop(3)`) yield MaxIterations directly.
// Template declarations (`as fix_loop("{{outputs.X.cap}}")`) resolve
// the refs against the runState and coerce the result to int. The
// fallback when resolution / coercion fails is loop.MaxIterations
// (typically 0 for the template form) — that surfaces as a "loop
// exhausted on iteration 0" log line at the edge check, which is the
// loudest visible failure mode we can offer without aborting the run.
// defaultUnboundedFuel is the fuel ceiling applied to an `unbounded` loop that
// declares neither a per-loop fuel nor a workflow budget.max_iterations.
// Validation (C097) normally requires one of those, so this only guards a
// programmatically-constructed IR that bypassed the compiler — it must never be
// 0 (that would be a silent infinity).
const defaultUnboundedFuel = 1000

// maxLoopStall is the liveness threshold: when an unbounded loop's progress
// signal (the source node's output) is unchanged across this many consecutive
// back-edge crossings, the loop is judged stuck at a fixpoint and the back-edge
// falls through to the exit path. This catches PRACTICAL non-termination (the
// loop is making no progress) better than any static analysis could.
const maxLoopStall = 3

// loopStalled updates and reports the unbounded-loop liveness state: it hashes
// the source output into a progress signature and counts consecutive crossings
// where the signature is unchanged. Returns true once the loop has been stuck
// at the same fixpoint for maxLoopStall crossings.
func (e *Engine) loopStalled(loopName string, output map[string]any, rs *runState) bool {
	sig := outputSignature(output)
	if prev, ok := rs.loopProgressSig[loopName]; ok && prev == sig {
		rs.loopStaleness[loopName]++
	} else {
		rs.loopStaleness[loopName] = 0
	}
	rs.loopProgressSig[loopName] = sig
	return rs.loopStaleness[loopName] >= maxLoopStall
}

// outputSignature produces a stable string fingerprint of a node output for
// liveness comparison. json.Marshal sorts map keys, so the signature is
// deterministic for equal content.
func outputSignature(output map[string]any) string {
	if b, err := json.Marshal(output); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", output)
}

// resolveLoopMax returns the loop's effective iteration ceiling: the
// declared/expr/fuel base plus any live-steering grant (bump_loop). The
// grant applies for the remainder of the run; a loop re-entry still
// resets its COUNTER, so the raised ceiling governs each entry.
func (e *Engine) resolveLoopMax(loop *ir.Loop, rs *runState) int {
	base := e.resolveLoopMaxBase(loop, rs)
	if extra := rs.loopOverrides[loop.Name]; extra > 0 {
		return base + extra
	}
	return base
}

func (e *Engine) resolveLoopMaxBase(loop *ir.Loop, rs *runState) int {
	// Unbounded loops have no user iteration cap; the effective ceiling is the
	// fuel: the clause's per-loop fuel, else the workflow's max_iterations, else
	// a hard default (so there is never a silent infinity even if validation was
	// bypassed). The liveness monitor halts a no-progress loop before this.
	if loop.Unbounded {
		if loop.FuelCap > 0 {
			return loop.FuelCap
		}
		if e.workflow.Budget != nil && e.workflow.Budget.MaxIterations > 0 {
			return e.workflow.Budget.MaxIterations
		}
		return defaultUnboundedFuel
	}
	if loop.MaxIterationsExpr == "" || len(loop.MaxIterationsExprRefs) == 0 {
		return loop.MaxIterations
	}
	var resolved any
	for _, ref := range loop.MaxIterationsExprRefs {
		v := e.resolveRef(ref, rs.scope())
		if v != nil {
			resolved = v
		}
	}
	if resolved == nil {
		return loop.MaxIterations
	}
	if n, ok := coerceToInt(resolved); ok {
		return n
	}
	return loop.MaxIterations
}

// coerceToInt accepts the common shapes that an output/var ref can
// carry for a numeric value: native ints, float64 (the JSON decoder
// default), json.Number, and decimal-string scalars (some JS nodes
// emit numbers as strings). Returns false for anything else.
func coerceToInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case int32:
		return int(x), true
	case float64:
		return int(x), true
	case float32:
		return int(x), true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n), true
		}
		if f, err := x.Float64(); err == nil {
			return int(f), true
		}
	case string:
		if n, err := strconv.Atoi(x); err == nil {
			return n, true
		}
	}
	return 0, false
}

// buildTemplateData assembles a model.TemplateData snapshot from the
// current run state. It is attached to ctx before each node execution
// so the executor can resolve `outputs.*`, `loop.*`, `artifacts.*`,
// and `run.*` refs in prompt bodies. Maps are passed by reference —
// the executor must treat them as read-only.
func (e *Engine) buildTemplateData(rs *runState) *model.TemplateData {
	loopMax := make(map[string]int, len(e.workflow.Loops))
	for name, l := range e.workflow.Loops {
		if l != nil {
			loopMax[name] = e.resolveLoopMax(l, rs)
		}
	}
	return &model.TemplateData{
		Outputs:            rs.outputs,
		LoopCounters:       runStateIterationCounters(rs),
		LoopMaxIterations:  loopMax,
		LoopPreviousOutput: loopPreviousOutputView(rs),
		Artifacts:          rs.artifacts,
		RunID:              rs.runID,
		Attachments:        rs.attachments,
	}
}

// loadAttachmentInfos populates the per-run attachment view consumed
// by template references. Called once after CreateRun (and any
// promote callback) so Run.Attachments is authoritative.
//
// Path computation is delegated to attachmentPath, which resolves to
// whatever the executing nodes can actually open:
//   - Filesystem stores: <root>/runs/<id>/attachments/<name>/<filename>
//   - Filesystem stores UNDER SANDBOX: the bind-mount path
//     (/run/iterion/attachments/<name>/<filename>) — the host path does
//     not exist inside the container.
//   - Cloud / non-FS stores: Path is left empty; nodes that need bytes
//     access them via the URL accessor (presigned).
func (e *Engine) loadAttachmentInfos(ctx context.Context, runID string) map[string]model.AttachmentInfo {
	if e.store == nil {
		return nil
	}
	list, err := e.store.ListAttachments(ctx, runID)
	if err != nil || len(list) == 0 {
		return nil
	}
	out := make(map[string]model.AttachmentInfo, len(list))
	for _, rec := range list {
		info := model.AttachmentInfo{
			Name:             rec.Name,
			OriginalFilename: rec.OriginalFilename,
			MIME:             rec.MIME,
			Size:             rec.Size,
			SHA256:           rec.SHA256,
		}
		// Resolve to the path the NODES can open: the host path for an
		// ordinary run, the bind-mount path when the run is sandboxed
		// (where the host path simply does not exist). Empty for
		// cloud/non-FS stores, whose consumers use the presign accessor
		// below instead.
		info.Path = e.attachmentPath(rec)
		// Where THIS process can read the bytes. Distinct from Path,
		// which is the node's view and points inside the container on a
		// sandboxed run — reading that here fails, and image inlining
		// silently degrades to interpolating the path as text.
		info.HostPath = e.attachmentHostPath(rec)
		// Lazy presign — capture the loop var by value so each closure
		// targets its own attachment.
		recCopy := rec
		runIDCopy := runID
		store := e.store
		info.PresignURL = func() (string, error) {
			return store.PresignAttachment(ctx, runIDCopy, recCopy.Name, 10*time.Minute)
		}
		out[rec.Name] = info
	}
	return out
}

// drillPath walks a nested map[string]interface{} structure by the given
// path, returning the final value (or nil if any segment is missing or
// non-map). Used by every reference resolver that needs to descend into
// node outputs / artifacts / loop snapshots.
func drillPath(root any, path []string) any {
	cur := root
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

// matchOutputNode picks the node whose id is the LONGEST dotted prefix of the
// reference path that is an actual key in outputs, returning that node's output
// map and the remaining field path. This disambiguates dotted group-instance
// node ids (`prefix.name`) from the dotted ref grammar at any nesting depth:
// {{outputs.r1.gate.id}} with a node "r1.gate" yields (outputs["r1.gate"], ["id"]).
// Returns (nil, nil) when no prefix matches.
func matchOutputNode(outputs map[string]map[string]any, path []string) (map[string]any, []string) {
	for n := len(path); n >= 1; n-- {
		id := strings.Join(path[:n], ".")
		if out, ok := outputs[id]; ok {
			return out, path[n:]
		}
	}
	return nil, nil
}

// resolveVars builds the vars map from workflow variable defaults,
// coercing user-provided override strings to the declared type.
//
// Coercion is necessary because the CLI's --var flag and the HTTP
// /api/runs endpoint both deliver vars as raw strings. Without
// coercion, an explicit "--var loop_count=3" stores the var as the
// string "3", which then fails downstream comparisons against the
// typed defaults (e.g. "input.count >= vars.loop_count" tries to
// compare a number against a string and aborts the run with an
// opaque "cannot compare X >= string" error). Defaults from the
// .bot source are already typed by the IR compiler — we coerce
// only on overrides.
func (e *Engine) resolveVars(inputs map[string]any) map[string]any {
	vars := make(map[string]any)
	expandFn := e.varExpandFn()
	for name, v := range e.workflow.Vars {
		if v.HasDefault {
			if s, ok := v.Default.(string); ok {
				vars[name] = os.Expand(s, expandFn)
			} else {
				vars[name] = v.Default
			}
		}
	}
	for k, v := range inputs {
		decl, isVar := e.workflow.Vars[k]
		if !isVar {
			continue
		}
		coerced, err := coerceVarValue(v, decl.Type)
		if err != nil {
			// Fall back to whatever the caller passed; the engine's
			// downstream type checks will surface a clear error if
			// the value really is incompatible. The alternative —
			// failing the run here — would be more aggressive than
			// the previous behaviour.
			e.logger.Warn("runtime: var %q: coerce to %s failed: %v (using raw value)", k, decl.Type, err)
			vars[k] = v
			continue
		}
		if s, ok := coerced.(string); ok {
			coerced = os.Expand(s, expandFn)
		}
		vars[k] = coerced
	}

	// Foot-gun guard: a var explicitly set to the repo root — e.g.
	// `--var workspace_dir=$(pwd)` — points agents at the MAIN checkout
	// rather than the active worktree. Under worktree:auto the repo root
	// IS the worktree, and under sandbox the main-checkout path has `.git`
	// mounted but NO working-tree files, so git there reports a phantom
	// "all files deleted". Remap any repo-root-valued var to the same
	// target `${PROJECT_DIR}` resolves to (the worktree / in-container
	// workspace). No-op when that target already equals the repo root
	// (no worktree and no sandbox path remap), so a plain run is untouched.
	if e.repoRoot != "" {
		if projectDir := expandFn("PROJECT_DIR"); projectDir != "" && !samePath(projectDir, e.repoRoot) {
			for k, val := range vars {
				if s, ok := val.(string); ok && samePath(s, e.repoRoot) {
					vars[k] = projectDir
					if e.logger != nil {
						e.logger.Warn("runtime: var %q was set to the repo root %q; remapped to the worktree/sandbox workspace %q to avoid a phantom working-tree view. Prefer omitting it so it defaults to ${PROJECT_DIR}.", k, e.repoRoot, projectDir)
					}
				}
			}
		}
	}
	return vars
}

// varExpandFn returns the os.Expand callback var values are resolved
// with. It lets var values reference ${PROJECT_DIR} (resolved to the
// engine's workDir, possibly a worktree path) and any other env var.
// Applied to both string defaults AND string user-provided overrides:
// the studio's LaunchView pre-fills its form with the literal default
// (e.g. "${PROJECT_DIR}") so an unmodified submit re-sends it as an
// override, which would otherwise reach tool nodes verbatim and break
// `git -C '${PROJECT_DIR}'`. Expanding overrides in the same pass
// keeps `vars.workspace_dir` resolved to a real path regardless of
// whether it came from the workflow default or the form input.
// Shared by resolveVars and validateVarEnums so the launch gate checks
// exactly the value that flows into the run.
func (e *Engine) varExpandFn() func(string) string {
	return func(key string) string {
		if key == "PROJECT_DIR" {
			// In sandbox mode, ${PROJECT_DIR} must resolve to the
			// in-container bind-mount target (e.g. /workspace), not
			// the host worktree path. Tool nodes and prompts using
			// this var are consumed by processes RUNNING inside the
			// container — they cannot open /home/<host-user>/...
			// paths because they're not mounted there. The container
			// workspace IS the host worktree, just at a different
			// pathname.
			if e.containerWorkspace != "" {
				return e.containerWorkspace
			}
			return e.workDir
		}
		if key == "PROJECT_MEMORY_DIR" {
			// Project-rooted memory directory, keyed off the run's
			// repo_root (not the per-run workDir). Resolves to
			// ~/.iterion/projects/<encoded-repo-root>/memory/ so
			// dispatcher-spawned bots running in worktrees still share
			// a memory tree with a whats-next session at the repo root.
			// The same host path is bind-mounted inside the sandbox
			// (~/.iterion is auto-mounted by docs/sandbox.md's host_state
			// contract), so it works in both modes without remapping.
			base := e.repoRoot
			if base == "" {
				base = e.workDir
			}
			return memory.WorkspaceMemoryDir(base)
		}
		if key == "PROJECT_SCRATCH_DIR" {
			// Out-of-tree scratch dir for working files a bot must NOT leave
			// in the target repo (e.g. a chunked review's per-chunk diffs)
			// so they never pollute the worktree or the run diff.
			//
			// Sandboxed: resolve to a fixed container path rather than the
			// host path, because an image pinning a non-host User cannot
			// write a host-owned bind (observed EACCES:
			// branch-improve-loop's plan_chunks, sec-audit-deps'
			// update_cache).
			//
			// That path is BACKED by the per-project host dir, bound on by
			// applyScratchMount. The backing is load-bearing for any fan-in
			// through scratch: a sub-bot child runs in its OWN container, so
			// a purely container-local scratch means the child writes a file
			// the parent can never read — the child reports success, the
			// parent reads an empty directory, and the run only fails much
			// later as "not enough results" (observed on app-concept: four
			// topic syntheses written to
			// /tmp/iterion-scratch/<parent>/topics, none visible at fan-in).
			// It also makes scratch survive the container, so a crashed run
			// resumes from its own working state.
			if e.containerWorkspace != "" {
				return sandboxScratchContainerPath
			}
			// Non-sandboxed: host path keyed off repo_root, a sibling of
			// PROJECT_MEMORY_DIR at ~/.iterion/projects/<key>/scratch/.
			base := e.repoRoot
			if base == "" {
				base = e.workDir
			}
			return memory.WorkspaceScratchDir(base)
		}
		return os.Getenv(key)
	}
}

// validateVarEnums enforces declared `[enum: ...]` constraints on
// launch-provided var values — the runtime counterpart of the C126
// compile check on defaults (defaults are compile-validated, so only
// provided values are checked here). Values are checked after the same
// ${VAR} expansion resolveVars applies, i.e. against the exact value
// that flows into the run; upstream template rendering (dispatcher
// bot_args, preset overlay) has already happened by the time inputs
// reach the engine. Returns an error naming every violating var, its
// value, and the allowed list.
func (e *Engine) validateVarEnums(inputs map[string]any) error {
	if len(inputs) == 0 {
		return nil
	}
	expandFn := e.varExpandFn()
	var violations []string
	for _, k := range slices.Sorted(maps.Keys(inputs)) {
		decl, isVar := e.workflow.Vars[k]
		if !isVar || len(decl.EnumValues) == 0 || decl.Type != ir.VarString {
			continue
		}
		v := inputs[k]
		s, isStr := v.(string)
		if !isStr {
			violations = append(violations, fmt.Sprintf(
				"var %q: value %v (%T) is not one of the allowed values (%s)",
				k, v, v, quoteList(decl.EnumValues)))
			continue
		}
		if expanded := os.Expand(s, expandFn); !slices.Contains(decl.EnumValues, expanded) {
			violations = append(violations, fmt.Sprintf(
				"var %q: value %q is not one of the allowed values (%s)",
				k, expanded, quoteList(decl.EnumValues)))
		}
	}
	if len(violations) > 0 {
		return errors.New(strings.Join(violations, "; "))
	}
	return nil
}

// quoteList renders enum values as `"a", "b"` for error messages.
func quoteList(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(quoted, ", ")
}

// samePath reports whether two paths are the same after lexical cleaning.
// Used to detect a var explicitly set to the repo root so it can be
// remapped to the worktree/sandbox workspace.
func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// coerceVarValue narrows a user-provided override (typically a
// string from --var or POST /api/runs) to the type declared in the
// IR for that var. Already-typed values pass through.
func coerceVarValue(v any, vt ir.VarType) (any, error) {
	s, isStr := v.(string)
	if !isStr {
		return v, nil
	}
	switch vt {
	case ir.VarString:
		return s, nil
	case ir.VarBool:
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no", "":
			return false, nil
		default:
			return nil, fmt.Errorf("invalid bool %q", s)
		}
	case ir.VarInt:
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid int %q: %w", s, err)
		}
		return n, nil
	case ir.VarFloat:
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float %q: %w", s, err)
		}
		return n, nil
	case ir.VarJSON:
		// Parse JSON; if the user gave us non-JSON text, leave it
		// as a string — JSON expressions accept either.
		var out any
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return s, nil
		}
		return out, nil
	case ir.VarStringArray:
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return []any{}, nil
		}
		// Accept either JSON array form (["a","b"]) or
		// comma-separated (a,b).
		if strings.HasPrefix(trimmed, "[") {
			var arr []any
			if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
				return arr, nil
			}
		}
		parts := strings.Split(trimmed, ",")
		out := make([]any, len(parts))
		for i, p := range parts {
			out[i] = strings.TrimSpace(p)
		}
		return out, nil
	default:
		return s, nil
	}
}

// emitTerminalNodeEvents emits the NodeStarted+NodeFinished pair for a
// terminal node (DoneNode, FailNode). Both events fire so the run
// console renders the terminal step like any other; the iteration tag
// matches the loop-counter at the moment the node was reached. Bails
// on the first emit error.
func (e *Engine) emitTerminalNodeEvents(rs *runState, nodeID string) error {
	iter := map[string]any{"iteration": e.currentLoopIteration(nodeID, rs.loopCounters)}
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, nodeID, iter); err != nil {
		return err
	}
	return e.emit(rs.ctx, rs.runID, store.EventNodeFinished, nodeID, nil)
}
