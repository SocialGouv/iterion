package bots

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/types"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A value on an LLM node's input reaches the MODEL only if one of THAT node's
// own channels consumes it. The input carries every field the node's input
// schema declares and every key an incoming edge's `with` maps onto the node —
// the runtime writes each mapped key, declared or not
// (pkg/runtime/engine_resolve.go). The channels, each read off the consumer
// rather than assumed:
//
//   - the node's system and user prompts, resolved against its input
//     (pkg/backend/model/executor_build_task.go);
//   - the node-level `images:` list — resolved against the same input and
//     forwarded as `-i` by the codex backend ONLY (pkg/backend/delegate/codex.go
//     is its one reader). On any other backend the entries are dropped;
//   - a node with NO user prompt is the exception: its whole input is
//     JSON-serialized as the user message
//     (pkg/backend/model/executor_template.go), so every field arrives.
//
// A launch-time preset fragment is also resolved against the input; it is
// chosen by the operator, not declared by the bot, and is out of scope here.
// So are the run inputs and variable defaults the entry node receives: they
// belong to the run, not to a mapping the bot wrote onto the node.
//
// HOW "consumes" is decided: not by reading the template syntax, but by
// rendering the node's channels through the REAL resolver
// (model.TemplateResolver) with a distinct marker as each field's value, and
// looking for the marker in the output. A reading of the syntax is a list of
// spellings — the bang form `{{!input.X}}` parses as a normal reference and
// renders as a literal placeholder (#1775); a sub-path renders on a json value
// and not on a string — and each rule added to such a list is one more rule
// the next form slips past. The renderer has no list. If it is ever changed to
// render a form it leaves verbatim today, this guard follows without an edit.
//
// What the renderer cannot know is the SHAPE a value has at runtime, so the
// marker is given that shape (inputFields, markerInput). It follows how the
// value arrives, not only what the schema declares: a mapping that passes a
// value through keeps its type, any other delivers a string. A `file` value is
// the upload descriptor, with its fixed keys; a typed value takes the shape of
// the node's own drills, because what it holds is data — a drill into a key
// the real value may lack still counts, the one limit of a static check; a
// string renders whole, and a drill into it stays verbatim.
//
// Routers are not read here (ir.LLMNode is agents and judges): a router passes
// its input through to its output, so a key its prompt does not render still
// travels on.
//
// Underscore fields (`_session_id`, `_reasoning_effort`, `_lease` …) are
// runtime plumbing by convention, read off the input by the runtime itself
// and never by a prompt, and are out of scope. That exemption is a
// convention: a `_` field the runtime does not read would be exempt too — none
// exists in the catalogue today.
//
// Paid twice. On #1598, `build_skipped` was added to four review schemas,
// mapped on four edges and reasoned about in the procedure — three greppable
// hits saying the plumbing was done — and no prompt rendered it. Then the first
// guard for that class concatenated EVERY prompt of the workflow, so a field
// rendered by some other agent's prompt satisfied it: re-measured per node,
// the catalogue held 29 such sites where that method counted 10.
//
// The debt is a ratchet, not an exemption. A field outside the baseline fails
// (a regression), and a baselined field that is no longer unconsumed fails too
// — so whoever fixes a site also shrinks the list. Growing it is an edit a
// reviewer sees; for a review gate's input, one this guard refuses.

// unrenderedBaseline is the debt measured on 2026-09-23 (#1740): per
// "<bot>/<node>" (or "<bot>/<workflow>/<node>" outside main.bot), the input
// fields no channel of that node consumes.
//
// Triage is per site, and a field is not necessarily a missing render. Three
// shapes are in this list: a mapping REDUNDANT with what the prompt already
// renders by another reference (`{{vars.…}}`, `{{outputs.…}}`) — drop the
// mapping; a value the node would have seen in an INHERITED session, on a
// backend that keeps one — make it explicit; and a field genuinely invisible —
// render it. A schema shared with another node's output cannot simply lose a
// field: give this node its own input schema.
var unrenderedBaseline = map[string][]string{
	"adr-cartograph/survey_code":             {"adr_count", "next_adr_number", "duplicates", "pre_verified_adrs", "bundle_self_path", "scope_notes"},
	"app-dev/plan_revise":                    {"assumptions", "risks"},
	"branch-improve-loop/plan_revise":        {"assumptions", "risks"},
	"copilot/copi":                           {"mode", "session_id", "session_fingerprint", "manager_scope_exclusions", "scope_guard_attempt", "actionless_clarification_count", "actionless_clarification_key"},
	"copilot/reflect":                        {"clarification_count"},
	"docs-refresh/campaign":                  {"hint_count"},
	"e2e-coverage/plan_revise":               {"assumptions", "risks"},
	"evolve/emit_backlog":                    {"workspace_dir"},
	"evolve/investigate":                     {"survey", "workspace_dir", "scope_notes"},
	"evolve/load_nexie_handoff":              {"workspace_dir"},
	"evolve/propose_evolutions":              {"workspace_dir"},
	"evolve/revise_vision":                   {"workspace_dir"},
	"evolve/survey":                          {"workspace_dir", "scope_notes"},
	"evolve/synthesize_vision":               {"survey", "investigation", "workspace_dir"},
	"feature-dev/plan_revise":                {"risks"},
	"feature-gap-fill/plan_revise":           {"assumptions", "risks"},
	"product-docs/campaign":                  {"hint_count"},
	"secured-renovacy/align_code":            {"workspace_dir"},
	"secured-renovacy/batch_upgrade_patches": {"workspace_dir"},
	"secured-renovacy/changelog_review":      {"workspace_dir"},
	"secured-renovacy/discover_outdated":     {"workspace_dir"},
	"secured-renovacy/fix_after_upgrade":     {"workspace_dir"},
	"secured-renovacy/install":               {"workspace_dir"},
	"secured-renovacy/security_audit":        {"workspace_dir"},
	"secured-renovacy/upgrade":               {"workspace_dir"},
	"secured-renovacy/validate_upgrade":      {"workspace_dir"},
	"test-coverage/plan_revise":              {"assumptions", "risks"},
	"whole-improve-loop/plan_revise":         {"assumptions", "risks"},
	"wiki-gen/author":                        {"workspace_dir"},
}

// imagesReachModel reports whether a node's `images:` entries are delivered to
// a model at all. Only the codex backend forwards them. The route read is the
// one the source DECLARES, as the compiler reads it: the node's own
// `backend:`, else the workflow's `default_backend:`, a `${X:-codex}` dial by
// its default — and a `{{vars.x}}` declares nothing, since it leaves the route
// to the launch, so it does not count as codex. A launch-time override (the
// studio's backend picker, `--backend`) is the operator re-routing what the
// source declared, out of scope like a preset fragment. A fallback onto
// another backend drops the entries on that route, so it disqualifies the
// channel too; a route naming no backend runs on the node's (a skip route
// never names one — the compiler refuses it).
func imagesReachModel(w *ir.Workflow, n ir.LLMNode) bool {
	if ir.SourceNodeBackendName(n.GetLLMFields().Backend, w.DefaultBackend) != "codex" {
		return false
	}
	for _, fb := range n.GetFallbacks() {
		if strings.TrimSpace(fb.Backend) != "" && ir.SourceBackendName(fb.Backend) != "codex" {
			return false
		}
	}
	return true
}

// reviewGateInput reports whether a schema is a review gate's input — the
// class #1598 paid for. It is named by the catalogue's convention: a schema
// whose name carries the word `review` (`review_input`, `plan_review_input`,
// `review_isolation_input` …). A convention, like the `_` exemption: a review
// gate whose input schema is named otherwise escapes it.
func reviewGateInput(schema string) bool {
	return slices.Contains(strings.Split(schema, "_"), "review")
}

// marker is the value a field carries during the render. Delimited on both
// sides, so the marker of `a` is not a substring of the marker of `ab`.
func marker(field string) string { return "MARKER\u00b7" + field + "\u00b7MARKER" }

var numericSegment = regexp.MustCompile(`^[0-9]+$`)

// valueShape is how a value on a node's input can be walked at runtime.
type valueShape int

const (
	// shapeText renders whole, and a sub-path on it stays verbatim.
	shapeText valueShape = iota
	// shapeData is walked by the node's own drills: what it holds is data.
	shapeData
	// shapeFile is the upload descriptor, with its fixed keys.
	shapeFile
)

// declaredShape is the shape of a value that arrives typed as declared.
func declaredShape(t types.FieldType) valueShape {
	switch t {
	case types.FieldTypeFile:
		return shapeFile
	case types.FieldTypeJSON:
		return shapeData
	}
	return shapeText
}

// inputField is one value an LLM node's input carries.
type inputField struct {
	name string
	// declared is false for a key an incoming edge maps onto the node that its
	// input schema does not declare.
	declared bool
	shape    valueShape
}

// inputFields lists what a node's input carries: its input schema's fields,
// then, sorted, every other key its incoming edges' `with` maps onto it.
//
// A value's shape follows how it ARRIVES. A mapping that is one whole
// `{{…}}` passes the value through with its type, whatever the field
// declares — a whole output reaches a `string` field as a map — while every
// other mapping delivers a string (ir.MappingArrivesAsText, the runtime's own
// resolveMapping decision). A value no mapping writes, and any value on the
// entry node, which the run's inputs reach unmapped, keeps its declared type.
// Where several routes reach one key the widest shape is taken: any of them
// may be the one that runs.
func inputFields(t *testing.T, w *ir.Workflow, node, schemaName string) []inputField {
	t.Helper()
	declared := map[string]types.FieldType{}
	var names []string
	if schemaName != "" {
		s := w.Schemas[schemaName]
		if s == nil {
			t.Fatalf("%s: input schema %q is not in the compiled workflow — a clean compile refuses that (C002)", node, schemaName)
		}
		for _, f := range s.Fields {
			if f == nil {
				continue
			}
			if _, dup := declared[f.Name]; !dup {
				declared[f.Name] = f.Type
				names = append(names, f.Name)
			}
		}
	}
	mapped, typed := map[string]bool{}, map[string]bool{}
	var undeclared []string
	for _, e := range w.Edges {
		if e == nil || e.To != node {
			continue
		}
		for _, dm := range e.With {
			if dm == nil {
				continue
			}
			if _, isDeclared := declared[dm.Key]; !isDeclared && !mapped[dm.Key] {
				undeclared = append(undeclared, dm.Key)
			}
			mapped[dm.Key] = true
			if !ir.MappingArrivesAsText(dm) {
				typed[dm.Key] = true
			}
		}
	}
	sort.Strings(undeclared)
	names = append(names, undeclared...)

	fields := make([]inputField, 0, len(names))
	for _, name := range names {
		ft, isDeclared := declared[name]
		shape := shapeText
		switch {
		case typed[name] && isDeclared && ft == types.FieldTypeFile:
			shape = shapeFile
		case typed[name]:
			shape = shapeData
		case isDeclared && (!mapped[name] || node == w.Entry):
			shape = declaredShape(ft)
		}
		fields = append(fields, inputField{name: name, declared: isDeclared, shape: shape})
	}
	return fields
}

// fileDescriptor is a `file` value as a gate upload delivers it — the
// producers' own descriptor plus the `path` the runtime adds
// (pkg/runtime/attachment_path.go) — each key carrying m. The other shape a
// file value can take, a bare path string, renders whole and never drilled.
func fileDescriptor(m string) map[string]any {
	d := store.AttachmentRecord{}.AnswerDescriptor()
	for k := range d {
		d[k] = m
	}
	d["path"] = m
	return d
}

// markerInput builds the input a node is rendered against: each field's
// marker, in the field's shape. A file value is the upload descriptor, so a
// drill renders exactly into the keys the descriptor has; a data value is
// shaped from the node's own drills, so that each finds a map to walk; a text
// value is the bare marker, and a sub-path on it stays verbatim.
func markerInput(fields []inputField, refs []*ir.Ref) map[string]any {
	input := map[string]any{}
	drillable := map[string]bool{}
	for _, f := range fields {
		switch f.shape {
		case shapeFile:
			input[f.name] = fileDescriptor(marker(f.name))
		case shapeData:
			input[f.name] = marker(f.name)
			drillable[f.name] = true
		default:
			input[f.name] = marker(f.name)
		}
	}
	for _, ref := range refs {
		if ref.Kind != ir.RefInput || len(ref.Path) < 2 || !drillable[ref.Path[0]] {
			continue
		}
		name := ref.Path[0]
		// A numeric segment is read as an array index, which the resolver
		// never walks — it walks maps only — so the drill ends there. A json
		// map keyed by digits would render it; where an index is the likely
		// intent, the report beats the benefit of the doubt. A drill that ends
		// at the field itself shapes nothing: the field keeps its marker, which
		// a whole-value render of it then shows.
		sub := ref.Path[1:]
		for i, seg := range sub {
			if numericSegment.MatchString(seg) {
				sub = sub[:i]
				break
			}
		}
		if len(sub) == 0 {
			continue
		}
		// Walk the value, turning each hop into a map; the leaf carries the
		// marker. A leaf that is already a map is kept: rendering it whole
		// still shows the marker inside it.
		m, ok := input[name].(map[string]any)
		if !ok {
			m = map[string]any{}
			input[name] = m
		}
		for i, seg := range sub {
			if i == len(sub)-1 {
				if _, isMap := m[seg].(map[string]any); !isMap {
					m[seg] = marker(name)
				}
				break
			}
			next, isMap := m[seg].(map[string]any)
			if !isMap {
				next = map[string]any{}
				m[seg] = next
			}
			m = next
		}
	}
	return input
}

// nodeReading is what one LLM node's channels deliver of its input.
type nodeReading struct {
	schema string // the node's input schema; "" when it declares none
	// missing are the input fields no channel delivers: schema fields in
	// declaration order, then undeclared mapped keys.
	missing []string
	// undeclared marks the missing keys that are mapped onto the node but
	// declared on no input schema of it.
	undeclared map[string]bool
}

// missingInputFields is THE reading, shared by the catalogue sweep and the
// fixtures below: for one node, the input fields none of its channels deliver
// to the model, decided by rendering those channels with the real resolver.
// ok is false for a node that is not an LLM node, or whose input carries
// nothing the bot put there — no input schema, and no key mapped onto it.
func missingInputFields(t *testing.T, w *ir.Workflow, node string) (r nodeReading, ok bool) {
	t.Helper()
	n, isLLM := w.Nodes[node].(ir.LLMNode)
	if !isLLM {
		return nodeReading{}, false
	}
	llm := n.GetLLMFields()
	r.schema = n.GetSchemaFields().InputSchema
	fields := inputFields(t, w, node, r.schema)
	if len(fields) == 0 {
		return nodeReading{}, false
	}
	// No user prompt: the whole input is serialized, every field arrives.
	if llm.UserPrompt == "" {
		return r, true
	}

	var bodies []string
	var refs []*ir.Ref
	for _, pn := range []string{llm.SystemPrompt, llm.UserPrompt} {
		if p := w.Prompts[pn]; p != nil {
			bodies = append(bodies, p.Body)
			refs = append(refs, p.TemplateRefs...)
		}
	}
	if imagesReachModel(w, n) {
		for _, img := range llm.Images {
			parsed, err := ir.ParseRefs(img)
			if err != nil {
				t.Fatalf("%s: images entry %q does not parse: %v", node, img, err)
			}
			bodies = append(bodies, img)
			refs = append(refs, parsed...)
		}
	}

	input := markerInput(fields, refs)
	resolver := &model.TemplateResolver{}
	var rendered strings.Builder
	for _, b := range bodies {
		rendered.WriteString(resolver.Resolve(b, input, nil))
		rendered.WriteString("\n")
	}
	out := rendered.String()

	for _, f := range fields {
		if strings.HasPrefix(f.name, "_") || strings.Contains(out, marker(f.name)) {
			continue
		}
		r.missing = append(r.missing, f.name)
		if !f.declared {
			if r.undeclared == nil {
				r.undeclared = map[string]bool{}
			}
			r.undeclared[f.name] = true
		}
	}
	return r, true
}

// siteKey names a node the way the baseline does: "<bot>/<node>" for a bot's
// main.bot, "<bot>/<workflow>/<node>" for a sibling workflow (extend.bot,
// reanchor.bot …), which launches by the same path and owes the same.
func siteKey(path, node string) string {
	dir := filepath.Dir(path)
	base := strings.TrimSuffix(filepath.Base(path), ".bot")
	if base == "main" {
		return dir + "/" + node
	}
	return dir + "/" + base + "/" + node
}

// isSiteOf reports whether key names a node of the workflow file at path — the
// inverse of siteKey.
func isSiteOf(key, path string) bool {
	node, ok := strings.CutPrefix(key, siteKey(path, ""))
	return ok && node != "" && !strings.Contains(node, "/")
}

type sweepResult struct {
	missing    map[string][]string        // per site, the input fields no channel delivers
	undeclared map[string]map[string]bool // per site, which of those no input schema declares
	schemaOf   map[string]string          // per examined site, its input schema ("" when none)
	examined   int
	// reviewCarriers counts the examined nodes taking a review gate's input;
	// siblings, those in a bot's workflow other than its main.bot.
	reviewCarriers, siblings int
	kinds                    map[string]int
	// uninspected are the files skipped for not compiling cleanly. Their
	// sites are neither checked nor held to the ratchet: the guard did not
	// read them, so it has nothing to say about their baseline entries.
	uninspected []string
}

// sweepFiles applies missingInputFields to every LLM node of every workflow
// file given — the catalogue's are catalogWorkflowFiles, the package's shared
// discovery: sibling workflows, examples and operator scripts included. A file
// that does not parse or compile cleanly is skipped and said — the
// parse/compile test owns that failure, and a program recovered from errors is
// not the one that runs.
func sweepFiles(t *testing.T, paths []string) sweepResult {
	t.Helper()
	paths = slices.Sorted(slices.Values(paths))

	r := sweepResult{
		missing: map[string][]string{}, undeclared: map[string]map[string]bool{},
		schemaOf: map[string]string{}, kinds: map[string]int{},
	}
	for _, path := range paths {
		pr := parseBotUnit(path)
		if pr.File == nil || parseHasErrors(pr) {
			t.Logf("%s: not inspected (does not parse cleanly — the parse/compile test owns that)", path)
			r.uninspected = append(r.uninspected, path)
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil || cr.HasErrors() {
			t.Logf("%s: not inspected (does not compile cleanly — the parse/compile test owns that)", path)
			r.uninspected = append(r.uninspected, path)
			continue
		}
		w := cr.Workflow
		// A bot's workflows live under bots/; examples and scripts sit
		// outside it, at a path starting with "..".
		sibling := filepath.Base(path) != "main.bot" && !strings.HasPrefix(path, "..")

		for _, name := range slices.Sorted(maps.Keys(w.Nodes)) {
			reading, ok := missingInputFields(t, w, name)
			if !ok {
				continue
			}
			r.kinds[fmt.Sprintf("%T", w.Nodes[name])]++
			r.examined++
			if sibling {
				r.siblings++
			}
			key := siteKey(path, name)
			r.schemaOf[key] = reading.schema
			if reviewGateInput(reading.schema) {
				r.reviewCarriers++
			}
			if len(reading.missing) > 0 {
				r.missing[key] = reading.missing
				r.undeclared[key] = reading.undeclared
			}
		}
	}
	return r
}

func parseHasErrors(pr *parser.ParseResult) bool {
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			return true
		}
	}
	return false
}

// floorViolations says why a sweep proves nothing, if it does not. The
// failure mode of a discovery is to match nothing and look green. The floors
// sit well under what was measured on 2026-09-24 (107 nodes: 90 agents and 17
// judges; 17 taking a review gate's input; 2 in sibling workflows) so that
// retiring a bot does not trip them, while a discovery that broke lands far
// below. Both node kinds are pinned: losing the judges alone — 17 nodes, ten of
// them review carriers — would pass every count-based floor.
func floorViolations(r sweepResult) []string {
	var v []string
	if r.examined < 50 {
		v = append(v, fmt.Sprintf("examined %d LLM nodes taking an input, want >= 50 — the discovery is stale "+
			"and this guard proves nothing", r.examined))
	}
	for _, kind := range []string{"*ir.AgentNode", "*ir.JudgeNode"} {
		if r.kinds[kind] == 0 {
			v = append(v, fmt.Sprintf("no %s examined (kinds seen: %v) — the discovery lost a whole kind of LLM node",
				kind, r.kinds))
		}
	}
	if r.reviewCarriers < 4 {
		v = append(v, fmt.Sprintf("found %d nodes taking a review gate's input, want >= 4 — the class #1598 paid for is no "+
			"longer being checked", r.reviewCarriers))
	}
	// Sibling workflows launch by the same path as main.bot and owe the same;
	// golden-master's extend and reanchor are the ones in the catalogue today.
	// Without this floor, narrowing the discovery back to */main.bot would stay
	// green, because both are clean.
	if r.siblings < 1 {
		v = append(v, "no LLM node examined in a bot's workflow other than its main.bot — sibling workflows are no "+
			"longer covered")
	}
	return v
}

// ratchetViolations holds a sweep to the baseline, both ways. kept lists the
// baseline entries left unjudged because their own file was not inspected.
func ratchetViolations(r sweepResult, baseline map[string][]string) (violations, kept []string) {
	for _, key := range slices.Sorted(maps.Keys(r.missing)) {
		allowed := toSet(baseline[key])
		for _, field := range r.missing[key] {
			if !allowed[field] {
				violations = append(violations, regressionMessage(key, field, r.schemaOf[key], r.undeclared[key][field]))
			}
		}
	}

	// The ratchet's other half: a site that got fixed must leave the list.
	for _, key := range slices.Sorted(maps.Keys(baseline)) {
		if slices.ContainsFunc(r.uninspected, func(path string) bool { return isSiteOf(key, path) }) {
			kept = append(kept, key)
			continue
		}
		still := toSet(r.missing[key])
		for _, field := range baseline[key] {
			if !still[field] {
				violations = append(violations, fmt.Sprintf("%s: %q is baselined as unconsumed but no longer is "+
					"(consumed, removed, or the node is gone) — remove it from unrenderedBaseline so the debt can "+
					"only shrink", key, field))
			}
		}
	}

	// A review gate's input is the class #1598 paid for: no debt may be
	// carried there, even by baselining it.
	for _, key := range slices.Sorted(maps.Keys(r.schemaOf)) {
		if reviewGateInput(r.schemaOf[key]) && len(baseline[key]) > 0 {
			violations = append(violations, fmt.Sprintf("%s takes %s, a review gate's input, and is baselined — "+
				"that class carries no debt", key, r.schemaOf[key]))
		}
	}
	return violations, kept
}

// regressionMessage names what a field that reaches no channel needs, by
// where the field comes from.
func regressionMessage(key, field, schema string, undeclared bool) string {
	switch {
	case !undeclared:
		return fmt.Sprintf("%s: input field %q is declared on this node's input schema (%s) and none of the "+
			"node's own prompts (nor, on codex, its images) delivers {{input.%s}} to the model — any "+
			"instruction reasoning about it is dead text. Either render it in the node's prompt; or, if the "+
			"prompt already renders the same value by another reference ({{vars.…}}, {{outputs.…}}), remove "+
			"the field from this input schema AND its `with` mapping; or, if the schema is shared with another "+
			"node, give this node its own input schema.",
			key, field, schema, field)
	case schema != "":
		return fmt.Sprintf("%s: key %q is mapped onto this node by an incoming edge's `with`, but its input "+
			"schema (%s) does not declare it — the runtime still writes it into the input, and no prompt of "+
			"the node may render an undeclared field (C034), so the value never reaches the model. Declare it "+
			"on the input schema and render it, or drop the mapping.",
			key, field, schema)
	default:
		return fmt.Sprintf("%s: key %q is mapped onto this node by an incoming edge's `with`, and none of the "+
			"node's own prompts (nor, on codex, its images) delivers {{input.%s}} to the model — the value "+
			"never reaches it. Render it in the node's prompt, or drop the mapping.",
			key, field, field)
	}
}

func toSet(xs []string) map[string]bool {
	s := make(map[string]bool, len(xs))
	for _, x := range xs {
		s[x] = true
	}
	return s
}

func TestLLMInputFieldsReachTheirOwnPrompt(t *testing.T) {
	r := sweepFiles(t, catalogWorkflowFiles())
	if v := floorViolations(r); len(v) > 0 {
		t.Fatalf("the sweep proves nothing:\n%s", strings.Join(v, "\n"))
	}
	t.Logf("examined %d LLM nodes taking an input (kinds %v, %d in sibling workflows, %d taking a review gate's input)",
		r.examined, r.kinds, r.siblings, r.reviewCarriers)

	violations, kept := ratchetViolations(r, unrenderedBaseline)
	for _, key := range kept {
		t.Logf("%s: not inspected this run — baseline entry kept", key)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

// The guard is only as good as its reading of a node's channels, so the
// reading is pinned on each runtime fact it rests on, against fixtures small
// enough to see whole. Every case goes through missingInputFields — the
// sweep's own reading, not a copy of it.
func TestLLMInputGuardReadsTheRuntimeRules(t *testing.T) {
	cases := []struct {
		name           string
		src            string
		node           string // the node read; "one" when empty
		wantMiss       []string
		wantUndeclared []string // the missing keys mapped onto the node but on no input schema of it
	}{
		{
			name: "a field rendered by ANOTHER node's prompt does not count",
			src: `schema in:
  a: string
  b: string
schema out:
  ok: bool
prompt p_one:
  uses {{input.a}}
prompt p_two:
  uses {{input.b}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  input: in
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two with { a: "x", b: "y" }
  two -> done
`,
			wantMiss: []string{"b"},
		},
		{
			name: "a node without a user prompt receives its whole input",
			src: `schema in:
  a: string
schema out:
  ok: bool
agent one:
  backend: "claude_code"
  input: in
  output: out
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
		{
			name: "a field rendered in the SYSTEM prompt counts",
			src: `schema in:
  a: string
schema out:
  ok: bool
prompt sys:
  context {{input.a}}
prompt usr:
  go
agent one:
  backend: "claude_code"
  input: in
  output: out
  system: sys
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
		{
			// The prompt renderer leaves `{{!input.a}}` in the text verbatim:
			// the model reads the placeholder, not the value.
			name: "a bang reference does not reach the model",
			src: `schema in:
  a: string
schema out:
  ok: bool
prompt usr:
  build skipped {{!input.a}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: []string{"a"},
		},
		{
			// The #1598 shape through a key the schema never declared: the
			// runtime writes it into the input all the same.
			name: "a key mapped onto a node but not on its input schema never reaches the model",
			src: `schema in:
  a: string
schema out:
  ok: bool
prompt p_one:
  go
prompt p_two:
  review {{input.a}}
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  input: in
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two with { a: "x", build_skipped: "{{outputs.one.ok}}" }
  two -> done
`,
			node:           "two",
			wantMiss:       []string{"build_skipped"},
			wantUndeclared: []string{"build_skipped"},
		},
		{
			name: "a node without an input schema is read on the keys mapped onto it",
			src: `schema out:
  ok: bool
prompt p_one:
  go
prompt p_two:
  review the change
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two with { build_skipped: "{{outputs.one.ok}}" }
  two -> done
`,
			node:           "two",
			wantMiss:       []string{"build_skipped"},
			wantUndeclared: []string{"build_skipped"},
		},
		{
			// An undeclared key holds whatever its mapping resolves to — here
			// a whole output — so a drill into it renders.
			name: "a mapped key its prompt drills into counts on a node without an input schema",
			src: `schema out:
  ok: bool
prompt p_one:
  go
prompt p_two:
  review {{input.ctx.ok}}
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two with { ctx: "{{outputs.one}}" }
  two -> done
`,
			node:     "two",
			wantMiss: nil,
		},
		{
			name: "a mapped key arrives with the whole input on a node without a user prompt",
			src: `schema out:
  ok: bool
prompt p_one:
  go
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  output: out
workflow w:
  entry: one
  one -> two with { build_skipped: "{{outputs.one.ok}}" }
  two -> done
`,
			node:     "two",
			wantMiss: nil,
		},
		{
			// One whole `{{…}}` passes the value through with its type, so the
			// `string` field holds the output map, and the drill renders.
			name: "a whole output mapped onto a string field arrives as data, and a drill into it counts",
			src: `schema in:
  ctx: string
schema out:
  ok: bool
prompt p_one:
  go
prompt p_two:
  review {{input.ctx.ok}}
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  input: in
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two with { ctx: "{{outputs.one}}" }
  two -> done
`,
			node:     "two",
			wantMiss: nil,
		},
		{
			// Any other mapping delivers the rendered text: the runtime never
			// decodes it, whatever the field declares (C152 only warns).
			name: "a json field every route delivers as text is a string, and a drill into it stays verbatim",
			src: `schema in:
  finding: json
schema out:
  ok: bool
prompt p_one:
  go
prompt p_two:
  review {{input.finding.file}}
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  input: in
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two with { finding: "see {{outputs.one.ok}}" }
  two -> done
`,
			node:     "two",
			wantMiss: []string{"finding"},
		},
		{
			name: "a field one route delivers typed can be drilled, whatever the other routes deliver",
			src: `schema in:
  finding: json
schema out:
  ok: bool
prompt p_one:
  go
prompt p_two:
  review {{input.finding.file}}
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  input: in
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two when ok with { finding: "{{outputs.one}}" }
  one -> two when not ok with { finding: "see {{outputs.one.ok}}" }
  two -> done
`,
			node:     "two",
			wantMiss: nil,
		},
		{
			name: "a file field every route delivers as text is a path string, and a drill into it stays verbatim",
			src: `schema in:
  upload: file
schema out:
  ok: bool
prompt p_one:
  go
prompt p_two:
  transcribe {{input.upload.path}}
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  input: in
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two with { upload: "/uploads/{{outputs.one.ok}}.png" }
  two -> done
`,
			node:     "two",
			wantMiss: []string{"upload"},
		},
		{
			name: "a file field a route delivers typed is still the descriptor, with its fixed keys",
			src: `schema in:
  upload: file
schema out:
  ok: bool
prompt p_one:
  go
prompt p_two:
  transcribe {{input.upload.name}}
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  input: in
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two with { upload: "{{outputs.one.ok}}" }
  two -> done
`,
			node:     "two",
			wantMiss: []string{"upload"},
		},
		{
			name: "a mapped key delivered as text is a string, and a drill into it stays verbatim",
			src: `schema out:
  ok: bool
prompt p_one:
  go
prompt p_two:
  review {{input.ctx.ok}}
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two with { ctx: "about {{outputs.one.ok}}" }
  two -> done
`,
			node:           "two",
			wantMiss:       []string{"ctx"},
			wantUndeclared: []string{"ctx"},
		},
		{
			// The run's inputs reach the entry node unmapped, typed as
			// launched: the back-edge's text is one route among two.
			name: "on the entry node a declared json field keeps its type, whatever a back-edge delivers",
			src: `schema in:
  finding: json
schema out:
  ok: bool
prompt p_one:
  review {{input.finding.file}}
prompt p_two:
  check
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two
  two -> one when not ok as again(2) with { finding: "retry {{outputs.two.ok}}" }
  two -> done
`,
			wantMiss: nil,
		},
		{
			// Whether such a field holds anything at all is the read-side
			// guard's question (TestCatalogInputReadsAreMappedByAnIncomingEdge);
			// here it keeps the shape its schema declares.
			name: "a declared field no edge maps keeps its declared shape",
			src: `schema in:
  finding: json
  a: string
schema out:
  ok: bool
prompt p_one:
  go
prompt p_two:
  review {{input.a}} {{input.finding.file}}
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent two:
  backend: "claude_code"
  input: in
  output: out
  user: p_two
workflow w:
  entry: one
  one -> two with { a: "x" }
  two -> done
`,
			node:     "two",
			wantMiss: nil,
		},
		{
			name: "a field consumed by images: counts",
			src: `schema in:
  frame: string
schema out:
  ok: bool
prompt usr:
  continue the sequence
agent one:
  backend: "codex"
  input: in
  output: out
  images: ["{{input.frame}}"]
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
		{
			// The model pins nothing about the route, which the node inherits;
			// it keeps the fixture compiling on a host with no credential to
			// detect (C018 reads the node's own fields only).
			name: "images: count on a node that inherits a codex default_backend",
			src: `schema in:
  frame: string
schema out:
  ok: bool
prompt usr:
  continue the sequence
agent one:
  model: "gpt-5.5"
  input: in
  output: out
  images: ["{{input.frame}}"]
  user: usr
workflow w:
  entry: one
  default_backend: "codex"
  one -> done
`,
			wantMiss: nil,
		},
		{
			name: "images: count on a node whose backend dial defaults to codex",
			src: `schema in:
  frame: string
schema out:
  ok: bool
prompt usr:
  continue the sequence
agent one:
  backend: "${ITERION_LLM_INPUT_GUARD_FIXTURE:-codex}"
  input: in
  output: out
  images: ["{{input.frame}}"]
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
		{
			name: "images: count on an auto node under a codex default_backend",
			src: `schema in:
  frame: string
schema out:
  ok: bool
prompt usr:
  continue the sequence
agent one:
  backend: "auto"
  input: in
  output: out
  images: ["{{input.frame}}"]
  user: usr
workflow w:
  entry: one
  default_backend: "codex"
  one -> done
`,
			wantMiss: nil,
		},
		{
			// A launch may override the variable: the source does not decide
			// the route, so the channel is not counted.
			name: "images: on a backend read from a variable deliver nothing the source can vouch for",
			src: `vars:
  b: string = "codex"
schema in:
  frame: string
schema out:
  ok: bool
prompt usr:
  continue the sequence
agent one:
  backend: "{{vars.b}}"
  input: in
  output: out
  images: ["{{input.frame}}"]
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: []string{"frame"},
		},
		{
			// codex is the only reader of Task.Images: on any other backend
			// the entries are dropped and the value never leaves the runtime.
			name: "images: on a non-codex backend delivers nothing",
			src: `schema in:
  frame: string
schema out:
  ok: bool
prompt usr:
  continue the sequence
agent one:
  backend: "claude_code"
  input: in
  output: out
  images: ["{{input.frame}}"]
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: []string{"frame"},
		},
		{
			// On the claude_code route the entries are dropped, and which
			// route serves a run is not known before it runs.
			name: "images: on codex with a fallback onto another backend delivers nothing",
			src: `schema in:
  frame: string
schema out:
  ok: bool
prompt usr:
  continue the sequence
agent one:
  backend: "codex"
  input: in
  output: out
  images: ["{{input.frame}}"]
  user: usr
  fallbacks:
    other:
      backend: "claude_code"
      model: "claude-sonnet-5"
      on: [any]
workflow w:
  entry: one
  one -> done
`,
			wantMiss: []string{"frame"},
		},
		{
			name: "images: on codex with a fallback dialled to codex by default count",
			src: `schema in:
  frame: string
schema out:
  ok: bool
prompt usr:
  continue the sequence
agent one:
  backend: "codex"
  input: in
  output: out
  images: ["{{input.frame}}"]
  user: usr
  fallbacks:
    other:
      backend: "${ITERION_LLM_INPUT_GUARD_FIXTURE:-codex}"
      model: "gpt-5.5"
      on: [any]
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
		{
			name: "images: on codex with a fallback that names no backend count",
			src: `schema in:
  frame: string
schema out:
  ok: bool
prompt usr:
  continue the sequence
agent one:
  backend: "codex"
  input: in
  output: out
  images: ["{{input.frame}}"]
  user: usr
  fallbacks:
    other:
      model: "gpt-5.5"
      on: [any]
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
		{
			name: "a sub-path counts on a json field and not on a scalar",
			src: `schema in:
  name: string
  finding: json
schema out:
  ok: bool
prompt usr:
  {{input.name.len}} {{input.finding.file}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: []string{"name"},
		},
		{
			// An upload is a descriptor map at runtime, so a drill into one of
			// its keys renders.
			name: "a drill into a file field renders",
			src: `schema in:
  upload: file
schema out:
  ok: bool
prompt usr:
  transcribe {{input.upload.path}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
		{
			// The descriptor's keys are fixed by its producers, not data: the
			// original name is `filename`, and `name` stays a placeholder.
			name: "a drill into a key the file descriptor lacks does not render",
			src: `schema in:
  upload: file
schema out:
  ok: bool
prompt usr:
  transcribe {{input.upload.name}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: []string{"upload"},
		},
		{
			// The resolver walks maps only, so an array index never renders,
			// and a numeric segment is read as one (markerInput says why).
			name: "a numeric index into a json field does not render",
			src: `schema in:
  items: json
schema out:
  ok: bool
prompt usr:
  first {{input.items.0}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: []string{"items"},
		},
		{
			name: "a numeric index next to the whole value leaves the whole value rendered",
			src: `schema in:
  items: json
schema out:
  ok: bool
prompt usr:
  all {{input.items}} first {{input.items.0}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
		{
			name: "a numeric index under a drilled key leaves that key rendered",
			src: `schema in:
  f: json
schema out:
  ok: bool
prompt usr:
  all {{input.f.a}} first {{input.f.a.0}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
		{
			name: "a numeric index beside a deeper drill keeps the deeper drill rendering",
			src: `schema in:
  f: json
schema out:
  ok: bool
prompt usr:
  file {{input.f.a.b}} first {{input.f.a.0}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
		{
			// The second drill walks the map the first one shaped: rebuilding
			// it would erase `f.a.c`, and the index never renders on its own.
			name: "drills that share a prefix each keep their path",
			src: `schema in:
  f: json
schema out:
  ok: bool
prompt usr:
  file {{input.f.a.c}} first {{input.f.a.b.0}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
		{
			// Session continuity and effort resolution read these off the
			// input directly; no prompt is meant to render them.
			name: "an underscore field is the runtime's, not the prompt's",
			src: `schema in:
  a: string
  _session_id: string
schema out:
  ok: bool
prompt usr:
  {{input.a}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`,
			wantMiss: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pr := parser.Parse("fixture.bot", c.src)
			if pr.File == nil || parseHasErrors(pr) {
				t.Fatalf("fixture did not parse cleanly: %v", pr.Diagnostics)
			}
			cr := ir.Compile(pr.File)
			if cr.Workflow == nil || cr.HasErrors() {
				t.Fatalf("fixture did not compile cleanly: %v", cr.Diagnostics)
			}
			node := c.node
			if node == "" {
				node = "one"
			}
			got, ok := missingInputFields(t, cr.Workflow, node)
			if !ok {
				t.Fatalf("fixture node %q is not an LLM node taking an input", node)
			}
			if strings.Join(got.missing, ",") != strings.Join(c.wantMiss, ",") {
				t.Errorf("node %s: missing=%v, want %v", node, got.missing, c.wantMiss)
			}
			undeclared := slices.Sorted(maps.Keys(got.undeclared))
			if strings.Join(undeclared, ",") != strings.Join(c.wantUndeclared, ",") {
				t.Errorf("node %s: undeclared=%v, want %v", node, undeclared, c.wantUndeclared)
			}
		})
	}
}

// The sweep's own bookkeeping — the files it could not read, the schema each
// site takes, the missing keys no schema declares — is what the verdict runs
// on. It is pinned on files written for the purpose, broken ones included: the
// catalogue compiles, so a sweep that stopped recording a broken file would
// stay green there.
func TestLLMInputSweepRecordsEachFile(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, src string) string {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mainBot := write("bot/main.bot", `schema review_input:
  diff: string
  build_skipped: bool
schema plan_review_input:
  plan: string
schema out:
  ok: bool
prompt p_one:
  go
prompt p_rev:
  review {{input.diff}}
prompt p_two:
  summarize
prompt p_plan:
  judge {{input.plan}}
agent one:
  backend: "claude_code"
  output: out
  user: p_one
agent rev:
  backend: "claude_code"
  input: review_input
  output: out
  user: p_rev
agent two:
  backend: "claude_code"
  output: out
  user: p_two
agent plan_rev:
  backend: "claude_code"
  input: plan_review_input
  output: out
  user: p_plan
workflow w:
  entry: one
  one -> rev with { diff: "x", build_skipped: "{{outputs.one.ok}}" }
  rev -> two with { verdict: "{{outputs.rev.ok}}" }
  two -> plan_rev with { plan: "x" }
  plan_rev -> done
`)
	sibling := write("bot/extend.bot", `schema in:
  a: string
schema out:
  ok: bool
prompt usr:
  {{input.a}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`)
	// The discovery reaches examples and scripts by a path out of bots/; a
	// workflow there is not a bot's sibling workflow, whatever its name.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.Rel(wd, write("demo/child.bot", `schema in:
  a: string
schema out:
  ok: bool
prompt usr:
  {{input.a}}
agent one:
  backend: "claude_code"
  input: in
  output: out
  user: usr
workflow w:
  entry: one
  one -> done
`))
	if err != nil || !strings.HasPrefix(outside, "..") {
		t.Fatalf("the test's temp dir %s lies inside the package (TMPDIR=%q): this fixture needs a path that leads out "+
			"of it, as the discovery's examples do — run the tests with TMPDIR outside the checkout (got %q, %v)",
			dir, os.Getenv("TMPDIR"), outside, err)
	}
	unparsable := write("unparsable/main.bot", "agent one:\n  input: in\n  ::::\n")
	uncompilable := write("uncompilable/main.bot", `schema out:
  ok: bool
agent one:
  backend: "claude_code"
  input: nowhere
  output: out
workflow w:
  entry: one
  one -> done
`)
	// Each broken fixture must fail where it claims to, or it tests the
	// other branch twice.
	if !parseHasErrors(parseBotUnit(unparsable)) {
		t.Fatalf("%s parses cleanly — it no longer exercises the parse-failure branch", unparsable)
	}
	if pr := parseBotUnit(uncompilable); parseHasErrors(pr) || !ir.Compile(pr.File).HasErrors() {
		t.Fatalf("%s must parse cleanly and fail to compile", uncompilable)
	}

	r := sweepFiles(t, []string{mainBot, sibling, outside, unparsable, uncompilable})

	if got, want := strings.Join(r.uninspected, ","), strings.Join(slices.Sorted(slices.Values([]string{unparsable, uncompilable})), ","); got != want {
		t.Errorf("uninspected = %s, want %s", got, want)
	}
	if r.examined != 5 || r.siblings != 1 || r.reviewCarriers != 2 || r.kinds["*ir.AgentNode"] != 5 {
		t.Errorf("examined %d (want 5), siblings %d (want 1), review carriers %d (want 2), kinds %v (want 5 agents)",
			r.examined, r.siblings, r.reviewCarriers, r.kinds)
	}
	rev, two := siteKey(mainBot, "rev"), siteKey(mainBot, "two")
	if r.schemaOf[rev] != "review_input" || r.schemaOf[two] != "" {
		t.Errorf("schemaOf = %v, want %s on review_input and %s on none", r.schemaOf, rev, two)
	}
	if got := strings.Join(r.missing[rev], ","); got != "build_skipped" || r.undeclared[rev]["build_skipped"] {
		t.Errorf("%s: missing %q undeclared %v, want the declared build_skipped", rev, got, r.undeclared[rev])
	}
	if got := strings.Join(r.missing[two], ","); got != "verdict" || !r.undeclared[two]["verdict"] {
		t.Errorf("%s: missing %q undeclared %v, want the undeclared verdict", two, got, r.undeclared[two])
	}
}

// The verdict over a sweep — the floors, and the ratchet both ways — is pinned
// on synthetic sweeps: on the real catalogue it has nothing to refuse, so a
// rule that stopped refusing would stay green there.
func TestLLMInputGuardVerdicts(t *testing.T) {
	healthy := func() sweepResult {
		return sweepResult{
			missing:        map[string][]string{"bot/a": {"x"}},
			undeclared:     map[string]map[string]bool{},
			schemaOf:       map[string]string{"bot/a": "in", "bot/r": "review_input"},
			examined:       60,
			reviewCarriers: 5,
			siblings:       2,
			kinds:          map[string]int{"*ir.AgentNode": 50, "*ir.JudgeNode": 10},
		}
	}
	baseline := map[string][]string{"bot/a": {"x"}}

	t.Run("a sweep that matches its baseline passes", func(t *testing.T) {
		r := healthy()
		if v := floorViolations(r); len(v) > 0 {
			t.Fatalf("floors: %v", v)
		}
		if v, kept := ratchetViolations(r, baseline); len(v) > 0 || len(kept) > 0 {
			t.Fatalf("ratchet: violations %v, kept %v", v, kept)
		}
	})

	floors := []struct {
		name   string
		damage func(*sweepResult)
		want   string
	}{
		{"too few nodes", func(r *sweepResult) { r.examined = 10 }, "examined 10"},
		{"no agent", func(r *sweepResult) { delete(r.kinds, "*ir.AgentNode") }, "no *ir.AgentNode"},
		{"no judge", func(r *sweepResult) { delete(r.kinds, "*ir.JudgeNode") }, "no *ir.JudgeNode"},
		{"too few review carriers", func(r *sweepResult) { r.reviewCarriers = 1 }, "found 1 nodes taking a review gate's input"},
		{"no sibling workflow", func(r *sweepResult) { r.siblings = 0 }, "sibling workflows"},
	}
	for _, f := range floors {
		t.Run("floor: "+f.name, func(t *testing.T) {
			r := healthy()
			f.damage(&r)
			v := floorViolations(r)
			if len(v) != 1 || !strings.Contains(v[0], f.want) {
				t.Fatalf("violations %v, want exactly one containing %q", v, f.want)
			}
		})
	}

	ratchet := []struct {
		name     string
		sweep    func(*sweepResult)
		baseline map[string][]string
		want     []string // one substring per expected violation, in order
		wantKept []string
	}{
		{
			name:     "a field outside the baseline fails",
			sweep:    func(r *sweepResult) { r.missing["bot/a"] = []string{"x", "y"} },
			baseline: baseline,
			want:     []string{`bot/a: input field "y" is declared`},
		},
		{
			name: "an undeclared mapped key is told apart from a declared field",
			sweep: func(r *sweepResult) {
				r.missing["bot/a"] = []string{"x", "k"}
				r.undeclared["bot/a"] = map[string]bool{"k": true}
				r.missing["bot/s"] = []string{"k"}
				r.undeclared["bot/s"] = map[string]bool{"k": true}
				r.schemaOf["bot/s"] = ""
			},
			baseline: baseline,
			want: []string{
				`bot/a: key "k" is mapped onto this node by an incoming edge's ` + "`with`" + `, but its input schema (in) does not declare it`,
				`bot/s: key "k" is mapped onto this node by an incoming edge's ` + "`with`" + `, and none`,
			},
		},
		{
			name:     "a baselined field that is no longer missing fails",
			sweep:    func(r *sweepResult) { delete(r.missing, "bot/a") },
			baseline: baseline,
			want:     []string{`bot/a: "x" is baselined as unconsumed but no longer is`},
		},
		{
			name: "a baseline entry is kept while its own file is not inspected",
			sweep: func(r *sweepResult) {
				delete(r.missing, "bot/a")
				r.uninspected = []string{"bot/main.bot"}
			},
			baseline: baseline,
			wantKept: []string{"bot/a"},
		},
		{
			name: "a broken sibling workflow does not suspend main.bot's entries",
			sweep: func(r *sweepResult) {
				delete(r.missing, "bot/a")
				r.uninspected = []string{"bot/extend.bot"}
			},
			baseline: baseline,
			want:     []string{`bot/a: "x" is baselined as unconsumed but no longer is`},
		},
		{
			name:     "a sibling workflow's entry is kept by its own file only",
			sweep:    func(r *sweepResult) { r.uninspected = []string{"bot/extend.bot", "bot/main.bot"} },
			baseline: map[string][]string{"bot/a": {"x"}, "bot/extend/b": {"y"}},
			wantKept: []string{"bot/a", "bot/extend/b"},
		},
		{
			name:     "a broken main.bot does not suspend a sibling workflow's entries",
			sweep:    func(r *sweepResult) { r.uninspected = []string{"bot/main.bot"} },
			baseline: map[string][]string{"bot/a": {"x"}, "bot/extend/b": {"y"}},
			want:     []string{`bot/extend/b: "y" is baselined as unconsumed but no longer is`},
			wantKept: []string{"bot/a"},
		},
		{
			name:     "an entry outside bots/ is kept while its own file is not inspected",
			sweep:    func(r *sweepResult) { r.uninspected = []string{"../examples/demo/main.bot"} },
			baseline: map[string][]string{"bot/a": {"x"}, "../examples/demo/n": {"z"}},
			wantKept: []string{"../examples/demo/n"},
		},
		{
			name: "a node taking review_input carries no debt, even baselined",
			sweep: func(r *sweepResult) {
				r.missing["bot/r"] = []string{"z"}
			},
			baseline: map[string][]string{"bot/a": {"x"}, "bot/r": {"z"}},
			want:     []string{"bot/r takes review_input, a review gate's input, and is baselined"},
		},
		{
			name: "every review gate's input carries no debt, by the word in its schema's name",
			sweep: func(r *sweepResult) {
				r.schemaOf["bot/p"] = "plan_review_input"
				r.schemaOf["bot/i"] = "review_isolation_input"
				r.missing["bot/p"] = []string{"z"}
				r.missing["bot/i"] = []string{"z"}
			},
			baseline: map[string][]string{"bot/a": {"x"}, "bot/p": {"z"}, "bot/i": {"z"}},
			want: []string{
				"bot/i takes review_isolation_input, a review gate's input",
				"bot/p takes plan_review_input, a review gate's input",
			},
		},
		{
			name: "a schema whose name merely contains the letters is not a review gate's input",
			sweep: func(r *sweepResult) {
				r.schemaOf["bot/v"] = "preview_input"
				r.missing["bot/v"] = []string{"z"}
			},
			baseline: map[string][]string{"bot/a": {"x"}, "bot/v": {"z"}},
		},
	}
	for _, c := range ratchet {
		t.Run("ratchet: "+c.name, func(t *testing.T) {
			r := healthy()
			c.sweep(&r)
			v, kept := ratchetViolations(r, c.baseline)
			if len(v) != len(c.want) {
				t.Fatalf("violations %q, want %d", v, len(c.want))
			}
			for i, want := range c.want {
				if !strings.Contains(v[i], want) {
					t.Errorf("violation %d = %q, want it to contain %q", i, v[i], want)
				}
			}
			if strings.Join(kept, ",") != strings.Join(c.wantKept, ",") {
				t.Errorf("kept %v, want %v", kept, c.wantKept)
			}
		})
	}
}
