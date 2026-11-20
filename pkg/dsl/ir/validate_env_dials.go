package ir

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// The compile-time half of ADR-132: a run reads no environment. Every setting
// a run observes — its route (model, backend, provider, effort, the CLI
// binary, the per-node bound) and the knobs its bots read — comes from a
// database level: platform, org, team, bot manifest, binding, launch. An
// authored `${NAME}` in one of those fields makes the answer depend on the
// process the run happens to start in, which is the one source no tenant can
// write and no record can audit.
//
// This pass is stage 1 of the record's staged transition (§4): it NAMES every
// dial and changes nothing at runtime, so a deployment's pins keep working
// while the catalogue is de-dialled and the pools' pins become records. The
// hard floor over the shipped catalogue arrives with the last de-dialled bot
// group (§6); a third-party `.bot` outside the catalogue keeps its dials and
// keeps being warned — its reading at the launch gate does not change.
//
// The surfaces walked are the ones a run's settings are authored on: every
// route field (a node's, a fallback route's, a recovery agent's, a
// supervisor's model, the workflow's default backend), a var default, a
// shell-bound tool body, a cursor setting and the workflow budget's
// max_duration. Each is read the way its runtime reader expands it. Left
// out on purpose, because they configure the host rather than the run:
// sandbox network rules (the deployment's own hosts, #2099) and an
// `mcp_server`'s command, args and env (the process-env policy governs
// them). A bare `$NAME` in a tool body is the child shell's read (ADR-130).
//
// Scope, decided on the measured alphabet rather than on a prefix: ANY env
// name, whatever it is spelled. The ticket's first wording said
// `${ITERION_*}`. Measured when this guard was written: of the names the
// shipped catalogue dials in a routing field, 22 are NOT `ITERION_` — nine
// `VETTY_`, five `BMADY_`, four `FEED_`, two `REARCHI_`, `RESCUE_PROVIDER`
// and `DEVBOX_SETUP_MODEL` — on 46 of the 333 dialled routing lines. A
// doctrine with a prefix-shaped hole is not a doctrine: the author of the
// next bot would simply not use `ITERION_`, and `dep-update-guard`'s
// escalation model (`${VETTY_MODEL_ESCALATE:-…}`, a human node's companion
// route) would stay undecided at the launch gate forever while the guard
// that exists to find it reported nothing.
func (c *compiler) validateEnvDials(w *Workflow) {
	for _, s := range routeSites(w) {
		for _, name := range routeEnvNames(s.value) {
			msg := "%s reads its route from the environment: %s consults ${%s} — a run's route comes from the authored literal and the routing policy above it (platform < org < team < bot < binding < launch), never from the process environment a run happens to start in (ADR-132 §1). Write the value the author means here, and pin a tenant's choice as a policy `overrides` cell"
			switch {
			case s.supervisor != "":
				// A supervisor is no node: its declaration is the line the
				// author edits, and it carries no per-property spans.
				c.emit(SeverityWarning, DiagEnvDialInRunSetting, "", "", c.supervisorSpan(s.supervisor), "", "", msg, s.owner(), s.field, name)
			case s.nodeID == "":
				c.warnfAtProperty(DiagEnvDialInRunSetting, "workflow", w.Name, routeProperty(s.field), msg, s.owner(), s.field, name)
			default:
				c.warnfAtProperty(DiagEnvDialInRunSetting, "node", s.nodeID, routeProperty(s.field), msg, s.owner(), s.field, name)
			}
		}
	}

	// A cursor setting is a knob the runtime expands from the environment
	// before it renders the cursor's calibration (model.resolveCursorFragments,
	// only when the block is enabled): the same class as a var default.
	for id, n := range w.Nodes {
		cn, ok := n.(interface{ GetCursors() *CursorInvocation })
		if !ok {
			continue
		}
		inv := cn.GetCursors()
		if inv == nil || !inv.Enabled {
			continue
		}
		for _, set := range inv.Settings {
			for _, name := range routeEnvNames(set.Value) {
				c.warnfAtProperty(DiagEnvDialInRunSetting, "node", id, "cursors",
					"node %q's cursor %q reads the environment: ${%s} — a bot knob's value comes from its authored literal and the levels above it, never from the process environment a run happens to start in (ADR-132 §1). Write the literal here",
					id, set.Key, name)
			}
		}
	}

	// The workflow's wall-clock bound is a knob the runtime expands from the
	// environment (runtime.newSharedBudget, the sandbox's own deadline):
	// the same class as a var default, on a field no var carries.
	if w.Budget != nil {
		for _, name := range routeEnvNames(w.Budget.MaxDuration) {
			c.warnfAtProperty(DiagEnvDialInRunSetting, "workflow", w.Name, "budget",
				"the workflow's budget max_duration reads the environment: ${%s} — a bot knob's value comes from its authored literal and the launch's own budget override, never from the process environment a run happens to start in (ADR-132 §1). Write the literal here, and re-budget a run at launch",
				name)
		}
	}

	// A var default and a tool body are the knob half of the same class. The
	// engine-supplied names (EngineSuppliedVarNames, pinned against the
	// engine's own table by a pkg/runtime test) are carved out of the VAR
	// surface and only there, because that is the only surface where the
	// engine answers them: varExpandFn resolves them from run state
	// (engine_resolve.go's engineSuppliedVarFns), so `${PROJECT_DIR}` in a
	// var default is §1's engine WRITE, not a run read, and naming it would
	// fire on every bot that locates its own workspace with no successor to
	// offer. A tool BODY gets no such answer: lookupToolEnv consults the
	// `ITERION_` overlay and then os.LookupEnv
	// (pkg/backend/model/executor_tool.go), never run state — so a braced
	// read there IS a read of the engine process's environment whatever the
	// name, and carving the same list out of it would silence exactly the
	// defect this record exists to find.
	for _, s := range varDefaultEnvSites(w) {
		for _, name := range s.names {
			c.warnfAt(DiagEnvDialInRunSetting, "", "",
				"var %q defaults from the environment: ${%s} — a bot knob's value comes from its authored literal and the bot-var ladder above it (ADR-093's overlay, resolved from the record), never from the process environment a run happens to start in (ADR-132 §1). Write the literal here, and tune it per tenant on the ladder",
				s.varName, name)
		}
	}

	for _, s := range toolBodyEnvSites(w) {
		for _, name := range s.names {
			c.warnfAtProperty(DiagEnvDialInRunSetting, "tool", s.nodeID, s.where,
				"node %q reads the environment in its %s body: ${%s} is answered by the ENGINE, from the engine's own environment, before the body's shell ever runs (the same expansion C188 reads) — a run reads no environment (ADR-132 §1). Carry the value as a var the ladder can set, or, where the engine CONSTRUCTS the name into this process's environment, read it bare ($%s) so the child's own environment answers it",
				s.nodeID, s.where, name, name)
		}
	}
}

// routeSite is one authored field whose value selects where a run goes: the
// node it belongs to (empty for a workflow-level field), or the supervisor,
// the DSL spelling the author edits, and the text as written.
type routeSite struct {
	nodeID     string
	supervisor string
	field      string
	value      string
}

// owner names the site the way a message should open it: a node by its id, a
// supervisor by its name, a workflow-level field as the workflow's.
func (s routeSite) owner() string {
	switch {
	case s.supervisor != "":
		return fmt.Sprintf("supervisor %q", s.supervisor)
	case s.nodeID == "":
		return "the workflow"
	}
	return fmt.Sprintf("node %q", s.nodeID)
}

// supervisorSpan is the source span of the `supervisor <name>:` declaration
// a diagnostic about its compiled form points at. Zero when the compiler has
// no file.
func (c *compiler) supervisorSpan(name string) ast.Span {
	if c.file == nil {
		return ast.Span{}
	}
	for _, d := range c.file.Supervisors {
		if d.Name == name {
			return d.Span
		}
	}
	return ast.Span{}
}

// routeProperty is the property line a route field is written on: the
// field's own name without the colon the message spells it with, or the
// block that holds it — `recovery model:` is the tool's recovery: block,
// `fallback "spare" model:` the node's fallbacks: block.
func routeProperty(field string) string {
	switch {
	case strings.HasPrefix(field, "recovery "):
		return "recovery"
	case strings.HasPrefix(field, "fallback "):
		return "fallbacks"
	}
	return strings.TrimSuffix(field, ":")
}

// routeSites enumerates every authored field whose value selects a route, over
// every node type that carries one and the workflow's own default.
//
// This enumeration IS the class, and it is the only one: a route-shaped field
// on a node type this function does not reach would ship a dial no guard ever
// sees. TestEveryRouteFieldIsEnumerated walks the package's node structs by
// reflection and fails on exactly that, so the next node type extends this
// function instead of quietly escaping it.
//
// The walk takes the node map in whatever order it comes: no pass owes its
// own ordering, because the compiler sorts every finding before returning it
// — by file, line and column, then by code, node, edge and message, so a tie
// on the position is broken too (diag_positions.go). A sort here would read
// as the thing that makes the output stable while being dead weight.
func routeSites(w *Workflow) []routeSite {
	var sites []routeSite
	add := func(nodeID, field, value string) {
		if strings.ContainsRune(value, '$') {
			sites = append(sites, routeSite{nodeID: nodeID, field: field, value: value})
		}
	}

	add("", "default_backend:", w.DefaultBackend)

	// A supervisor's model is a route like a node's: the runtime expands it
	// from the environment before it spawns the supervisor
	// (supervise.ResolveModel), and a `bot/supervisor:<name>` cell
	// re-targets it.
	for _, sup := range w.Supervisors {
		if sup != nil && strings.ContainsRune(sup.Model, '$') {
			sites = append(sites, routeSite{supervisor: sup.Name, field: "model:", value: sup.Model})
		}
	}

	for id, n := range w.Nodes {
		if f := llmFieldsOf(n); f != nil {
			add(id, "model:", f.Model)
			add(id, "backend:", f.Backend)
			add(id, "provider:", f.Provider)
			add(id, "command:", f.Command)
			add(id, "reasoning_effort:", f.ReasoningEffort)
			add(id, "timeout:", f.Timeout)
		}
		if ln, ok := n.(LLMNode); ok {
			add(id, "interaction_model:", ln.GetInteractionFields().InteractionModel)
		}
		switch t := n.(type) {
		case *HumanNode:
			// A human node carries its companion route outside LLMFields —
			// and it is a real dial: dep-update-guard escalates on
			// `${VETTY_MODEL_ESCALATE:-…}` there.
			add(id, "model:", t.Model)
			add(id, "interaction_model:", t.InteractionModel)
		case *ToolNode:
			// A verified action's rung-4 recovery agent is a route like any
			// other (ADR-044).
			if t.Recovery != nil {
				add(id, "recovery model:", t.Recovery.Model)
			}
		}
		for _, fb := range nodeFallbacks(n) {
			// Every field of a `fallbacks:` route may carry ${VAR} refs, and
			// the rescue rungs are where the non-ITERION dials concentrate
			// (`RESCUE_PROVIDER`).
			where := fmt.Sprintf("fallback %q ", fb.Name)
			add(id, where+"backend:", fb.Backend)
			add(id, where+"model:", fb.Model)
			add(id, where+"provider:", fb.Provider)
		}
	}
	return sites
}

// llmFieldsOf returns the route group a node carries, or nil for a node type
// that carries none.
//
// The membership is the EMBEDDED GROUP's, not the LLMNode interface's: an
// `llm` router embeds LLMFields and routes a real model through it while
// implementing none of the interface's other accessors, so a walk that asked
// `n.(LLMNode)` would read every agent and judge and silently skip the
// router's own dial.
func llmFieldsOf(n Node) *LLMFields {
	switch t := n.(type) {
	case *AgentNode:
		return &t.LLMFields
	case *JudgeNode:
		return &t.LLMFields
	case *RouterNode:
		return &t.LLMFields
	}
	return nil
}

// nodeFallbacks returns a node's authored `fallbacks:` routes, or nothing for
// a node type that carries none.
func nodeFallbacks(n Node) []Fallback {
	switch t := n.(type) {
	case *AgentNode:
		return t.Fallbacks
	case *JudgeNode:
		return t.Fallbacks
	}
	return nil
}

// toolBodyEnvSite is one tool body and the env names the engine's expansion
// consults in it.
type toolBodyEnvSite struct {
	nodeID string
	where  string
	names  []string
}

// toolBodyEnvSites reports the env names the engine answers inside each tool
// body it expands: the engine's own segmentation (BracedEnvReads) over the
// bodies the engine actually rewrites. No name is carved out here — the
// body's reader is lookupToolEnv, which answers from the engine process's
// environment alone, so every name it reaches is a run reading the
// environment.
//
// A node that declines the expansion (`env_expansion: none`, ADR-130) is
// skipped whole: the engine leaves its bodies verbatim, so no read of theirs
// is answered from any environment of the engine's and there is nothing to
// name. A `script:` in another language is not shell-bound, so its `${…}` is
// that language's template literal, never an env read — the same split the
// C188 lint makes.
func toolBodyEnvSites(w *Workflow) []toolBodyEnvSite {
	var sites []toolBodyEnvSite

	for id, node := range w.Nodes {
		t, ok := node.(*ToolNode)
		if !ok || !t.ExpandsBracedEnv() {
			continue
		}
		for _, b := range []struct {
			where string
			text  string
			shell bool
		}{
			{"command", t.Command, true},
			{"script", t.Script, t.Language == "" || t.Language == "sh" || t.Language == "bash"},
			{"postcondition", t.Postcondition, true},
		} {
			if b.text == "" || !b.shell {
				continue
			}
			var names []string
			for _, r := range BracedEnvReads(b.text) {
				names = append(names, r.Name)
			}
			if names = sortedUnique(names); len(names) > 0 {
				sites = append(sites, toolBodyEnvSite{nodeID: id, where: b.where, names: names})
			}
		}
	}
	return sites
}

// varDefaultEnvSite is one declared var whose default consults the
// environment, and the names it consults.
type varDefaultEnvSite struct {
	varName string
	names   []string
}

// varDefaultEnvSites reports, per declared var, the env names its default
// consults — read the way dispatch expands that var's own type (probesVarText:
// the full reading for a scalar, the braced-only reading of a json document's
// string LEAVES, where a key is a name and never a reference). The
// engine-supplied names are left out, for the reason stated on the pass.
//
// The finding names the var because that is the line the author edits: a
// message that named only the variable would send a reader of a bot with
// twenty vars to find it themselves.
func varDefaultEnvSites(w *Workflow) []varDefaultEnvSite {
	var sites []varDefaultEnvSite
	for varName, v := range w.Vars {
		if v == nil || !v.HasDefault {
			continue
		}
		var names []string
		probesVarText(v.Default, v.Type, func(name string) bool {
			if !isEngineSuppliedName(name) {
				names = append(names, name)
			}
			// Recorded or carved out, the probe answers this name itself:
			// returning true keeps the walk off the host environment, whose
			// values would otherwise steer an indirected name's resolution
			// and make the finding set depend on the machine compiling.
			return true
		})
		if names = sortedUnique(names); len(names) > 0 {
			sites = append(sites, varDefaultEnvSite{varName: varName, names: names})
		}
	}
	return sites
}

// routeEnvNames are the env names dispatch consults in one route field (or a
// field the runtime expands the same way), read with that expansion itself:
// ExpandWithDefault under the full policy, as ResolveRoutingFieldWith runs it.
// The expander resolves a segment inside-out and consults every name it
// meets, so a nested default (`${A:-${B:-c}}`) reads both names and a bare
// `$NAME` is a read too. Only a C identifier is a name: `${X#y}`, `${#X}`,
// `${X:+y}` and a `${p.name}` template literal reach the lookup as text no
// environment variable carries, exactly as at dispatch.
func routeEnvNames(value string) []string {
	if !strings.ContainsRune(value, '$') {
		return nil
	}
	var names []string
	expandWithDefault(value, func(name string) string {
		if isEnvIdentifier(name) {
			names = append(names, name)
		}
		return ""
	}, expandPolicy{})
	return sortedUnique(names)
}

// isEnvIdentifier reports whether name is a C identifier, the shape of an
// environment variable name.
func isEnvIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// sortedUnique is one finding per name: a field reading `${X}` twice, or a
// body reading it on four lines, is ONE dial to replace, and the sort is what
// lets slices.Compact see the duplicates.
func sortedUnique(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := slices.Clone(names)
	sort.Strings(out)
	return slices.Compact(out)
}
