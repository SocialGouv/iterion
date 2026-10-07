package supervise

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// This file owns the shared half of spawning DSL-declared supervisors
// (`supervisor NAME:` blocks): kill-switch resolution with its single
// warn message, IR→Spec conversion (prompt-body resolution + monitor
// parsing), and the spawn/teardown loop. Every launch surface (CLI run,
// CLI resume, runview/studio) goes through these three so the switch
// cannot be forgotten and the log vocabulary cannot fork.

// DeclaredEnabledOrWarn resolves the kill switch for a run that
// declares `declared` supervisors and, when disabled, logs the skip —
// a declared capability never disappears in silence. Callers gate their
// spawn wiring on the result.
func DeclaredEnabledOrWarn(override string, declared int, logger *iterlog.Logger) bool {
	enabled, source := DeclaredEnabled(override)
	if logger != nil {
		if !enabled {
			logger.Warn("supervisors: %d declared supervisor(s) disabled by %s", declared, source)
		} else if strings.Contains(source, "unreadable") {
			logger.Warn("supervisors: %s — spawning %d declared supervisor(s)", source, declared)
		}
	}
	return enabled
}

// ParseMonitorSpecs parses monitor specs of the form
// "event_type=tool_error,tool_name=Bash" (the `--monitor` flag and the
// DSL `monitors:` list share this grammar) into Monitor values.
func ParseMonitorSpecs(specs []string) ([]Monitor, error) {
	var out []Monitor
	for _, spec := range specs {
		var m Monitor
		for _, kv := range strings.Split(spec, ",") {
			kv = strings.TrimSpace(kv)
			if kv == "" {
				continue
			}
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return nil, fmt.Errorf("supervise: malformed monitor %q (want key=val)", spec)
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if v == "" {
				return nil, fmt.Errorf("supervise: monitor %q: empty value for key %q", spec, k)
			}
			switch k {
			case "event_type":
				m.EventType = v
			case "node_id":
				m.NodeID = v
			case "tool_name":
				m.ToolName = v
			case "text_contains":
				m.TextContains = v
			case "cost_gt":
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return nil, fmt.Errorf("supervise: monitor cost_gt %q: %w", v, err)
				}
				// A NaN/negative/zero threshold either never fires or —
				// worse — used to fall through Monitor.matches into a
				// match-everything wildcard. Refuse it here so the class
				// cannot arm.
				if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
					return nil, fmt.Errorf("supervise: monitor cost_gt %q must be a finite number > 0", v)
				}
				m.CostGt = f
			default:
				return nil, fmt.Errorf("supervise: monitor unknown key %q", k)
			}
		}
		// A spec that set no field would arm a monitor that can never
		// fire (isEmpty ⇒ "never") — dead config, refuse it loudly.
		if m.isEmpty() {
			return nil, fmt.Errorf("supervise: monitor %q sets no field", spec)
		}
		out = append(out, m)
	}
	return out, nil
}

// SpecsFromWorkflow converts the workflow's compiled supervisor
// declarations into runnable Specs: the system prompt reference is
// resolved to its body and the declared monitor specs are parsed into
// the coordinator's seed set. A monitor spec that fails to parse at
// this point (the compiler warns on them first, C191) is dropped with
// a warning — a supervisor is an enhancement and degrades rather than
// blocking the run.
//
// vars is the run's RESOLVED var map (the engine's ResolveVars — defaults
// and coercions included, not the raw launch overrides); a supervisor's
// model pin resolves its {{vars.name}} references against it, the same
// values the nodes beside which the supervisor spawns see. nil resolves
// nothing — the estimate paths that hold no run.
func SpecsFromWorkflow(wf *ir.Workflow, vars map[string]any, logger *iterlog.Logger) []Spec {
	if wf == nil {
		return nil
	}
	specs := make([]Spec, 0, len(wf.Supervisors))
	for _, sup := range wf.Supervisors {
		system := ""
		if sup.System != "" {
			if p, ok := wf.Prompts[sup.System]; ok && p != nil {
				system = p.Body
			}
		}
		var monitors []Monitor
		for _, raw := range sup.Monitors {
			parsed, err := ParseMonitorSpecs([]string{raw})
			if err != nil {
				if logger != nil {
					logger.Warn("supervisors: %s: dropping monitor %q: %v", sup.Name, raw, err)
				}
				continue
			}
			monitors = append(monitors, parsed...)
		}
		model := resolveModelVarsRefs(sup.Model, vars)
		hint := ""
		if model == "" {
			hint = providerHintFromWatched(wf, sup.Watches)
		}
		specs = append(specs, Spec{
			Name:           sup.Name,
			Model:          model,
			ProviderHint:   hint,
			GatewayWatched: gatewayWatched(wf, sup.Watches),
			System:         system,
			Watches:        sup.Watches,
			Monitors:       monitors,
			Cooldown:       sup.Cooldown,
			MaxEvals:       sup.MaxEvals,
		})
	}
	return specs
}

// resolveModelVarsRefs substitutes the {{vars.name}} references of a
// compiled supervisor model pin against the run's resolved vars. The
// compiler holds the pin to whole {{vars.name}} references (C148 for
// anything else), and the hint derivation reads the RESOLVED pin — a
// var-resolved model is a pin, not an empty one. A reference the map does
// not hold stays as written, the keep-literal the executor's resolver
// honors on a node's routing field: C033 refused an undeclared name at
// compile, so a surviving reference is a declared var the launch did not
// set and whose default resolved empty, and the provider answers the
// failure loudly at the first evaluation.
func resolveModelVarsRefs(model string, vars map[string]any) string {
	if model == "" || vars == nil || !strings.Contains(model, "{{") {
		return model
	}
	var b strings.Builder
	rest := model
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			b.WriteString(rest)
			return b.String()
		}
		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			b.WriteString(rest)
			return b.String()
		}
		end += start + 2
		b.WriteString(rest[:start])
		span := rest[start:end]
		ref := strings.TrimSpace(span[2 : len(span)-2])
		if name, ok := strings.CutPrefix(ref, "vars."); ok && !strings.Contains(name, ".") {
			if v, held := vars[name]; held {
				fmt.Fprint(&b, v)
				rest = rest[end:]
				continue
			}
		}
		b.WriteString(span)
		rest = rest[end:]
	}
}

// gatewayWatched reports whether any route this supervisor may observe can
// be an OpenAI-compatible gateway route. It mirrors the walks that DISPATCH
// routes — the same shapes validateGatewayRoutes warns on: an LLM node's
// (agent OR judge) primary model, each `provider:` chain STEP (hint and
// step-model), each `fallbacks:` entry, and an `mode: llm` router's model.
// An unexpanded `{{…}}` COUNTS as gateway: the conservative side refuses
// supervision that would otherwise silently pick a vendor default for a
// gateway node's content.
func gatewayWatched(wf *ir.Workflow, watches []string) bool {
	ids := watches
	if len(ids) == 0 {
		for id := range wf.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	for _, id := range ids {
		node, ok := wf.Nodes[id]
		if !ok {
			continue
		}
		switch nn := node.(type) {
		case ir.LLMNode:
			f := nn.GetLLMFields()
			if maybeGatewayRoute(f.Model) || providerChainNamesGateway(f.Provider) {
				return true
			}
			for _, fb := range nn.GetFallbacks() {
				if maybeGatewayRoute(fb.Model) || providerChainNamesGateway(fb.Provider) {
					return true
				}
			}
		case *ir.RouterNode:
			if nn.RouterMode != ir.RouterLLM {
				continue
			}
			if maybeGatewayRoute(nn.Model) || providerChainNamesGateway(nn.Provider) {
				return true
			}
		}
	}
	return false
}

// providerChainNamesGateway splits a `provider:` field the way the
// executor's chain resolution does — env-expanded, then per step, each step
// a hint or a `hint:model` — and judges every token.
func providerChainNamesGateway(provider string) bool {
	expanded := ir.ExpandEnvWithDefault(strings.TrimSpace(provider))
	if strings.Contains(expanded, "{{") {
		return true
	}
	for _, tok := range strings.Split(expanded, ",") {
		hint, model, hasModel := ir.SplitProviderStep(strings.TrimSpace(tok))
		if maybeGatewayRoute(hint) {
			return true
		}
		if hasModel && maybeGatewayRoute(model) {
			return true
		}
	}
	return false
}

// maybeGatewayRoute expands one route string and reports whether it names
// the gateway — or still holds a template, which expansion at launch cannot
// resolve (see gatewayWatched).
func maybeGatewayRoute(route string) bool {
	expanded := ir.ExpandEnvWithDefault(strings.TrimSpace(route))
	if strings.Contains(expanded, "{{") {
		return true
	}
	// A bare "openai_compatible" (a `provider:` step, a fallback with no
	// model) is the gateway prefix itself.
	if expanded == modelroute.OpenAICompatible {
		return true
	}
	if prov, _, found := strings.Cut(expanded, "/"); found {
		return prov == modelroute.OpenAICompatible
	}
	return false
}

// providerHintFromWatched derives the provider family the watched nodes
// run on, so an unpinned evaluator can prefer the credential the run
// itself uses (see Spec.ProviderHint). Sources, per node, strongest
// first: the node's explicit provider: routing (walked to the first
// claw-routable entry), a "provider/" prefix on its model, then the
// backend's own family (claude_code → anthropic, codex → openai). CLI
// backends whose credential claw cannot reuse (pi, kimi, grok) yield no
// hint. A supervisor with no watches supervises the whole run, so its
// hint derives from ALL the workflow's agent/judge nodes (sorted for
// determinism — Nodes is a map).
func providerHintFromWatched(wf *ir.Workflow, watches []string) string {
	ids := watches
	if len(ids) == 0 {
		for id := range wf.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	for _, id := range ids {
		node, ok := wf.Nodes[id]
		if !ok {
			continue
		}
		var llm *ir.LLMFields
		switch n := node.(type) {
		case *ir.AgentNode:
			llm = &n.LLMFields
		case *ir.JudgeNode:
			llm = &n.LLMFields
		default:
			continue
		}
		if p := ir.ExpandEnvWithDefault(llm.Provider); p != "" {
			for _, entry := range strings.Split(p, ",") {
				if entry = strings.TrimSpace(entry); clawProviders[entry] {
					return entry
				}
			}
		}
		// A slash in a model string is only a provider prefix for claw
		// providers — kimi's canonical aliases (kimi-code/kimi-for-coding)
		// carry a slash that is an alias namespace, not a provider. An
		// unrecognized prefix yields nothing and the NEXT source (backend
		// family, then the next watched node) is consulted instead of
		// short-circuiting with garbage.
		model := ir.ExpandEnvWithDefault(llm.Model)
		if provider, _, found := strings.Cut(model, "/"); found && clawProviders[provider] {
			return provider
		}
		switch ir.ExpandEnvWithDefault(llm.Backend) {
		case "claude_code":
			return "anthropic"
		case "codex":
			return "openai"
		}
	}
	return ""
}

// clawProviders is the set of provider names claw can route (the same
// families pkg/backend/detect reports on). Kept as a literal because the
// hint is advisory: an out-of-date entry costs a fallback to detector
// order, never a failure.
var clawProviders = map[string]bool{
	"anthropic": true,
	"openai":    true,
	"zai":       true,
	"moonshot":  true,
	"xai":       true,
	"bedrock":   true,
	"vertex":    true,
	"foundry":   true,
}

// StartDeclared spawns one Coordinator per spec, each observing runID
// through obs and steering through inj, and returns a stop func the
// caller defers to drain them before the run goroutine exits. A spec
// whose coordinator fails to construct is skipped — supervision is an
// enhancement, never a hard dependency.
func StartDeclared(ctx context.Context, obs Observer, inj Injector, runID string, specs []Spec, logger *iterlog.Logger) (stop func()) {
	var coords []*Coordinator
	for _, spec := range specs {
		coord := New(obs, inj, runID, spec, nil, logger)
		if coord == nil {
			continue
		}
		coord.Start(ctx)
		coords = append(coords, coord)
	}
	return func() {
		for _, c := range coords {
			c.Close()
		}
	}
}
