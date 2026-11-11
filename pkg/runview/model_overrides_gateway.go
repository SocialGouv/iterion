package runview

import (
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The gateway crossing check. Dispatch judges every chain element it
// builds — backend, provider hint and model as resolved — with
// model.GatewayCrossingRefusal and refuses a crossing whatever the run
// funds: a gateway route off claw, a vendor hint beside a gateway model, the
// gateway hint beside a model that is not a gateway route. The per-entry
// write gate sees one cell; the launch composes the cells with each other
// and with the bot. This check rebuilds, per LLM node and per authored
// `fallbacks:` route, the elements dispatch builds from the folded
// overrides, and judges them with that same function:
//
//   - decided (every field it reads is a literal): refused at launch,
//     warned at the write screen;
//   - a field a slot dial or an undecided var decides at the runner: judged
//     on the dial's default text and warned, at launch and at write — the
//     pod's environment may serve it;
//   - a crossing no override field takes part in is the bot's own (C186 at
//     compile, the build refusal at dispatch): this check adds nothing.

// gatewayField is one field of a composed element and who wrote it.
type gatewayField struct {
	value string
	// origin names the folded entry that wrote the value
	// (foldedEntryOrigin); "" when the bot did.
	origin string
	// botSource says where the bot wrote it, for a value no entry wrote.
	botSource string
}

func (f gatewayField) fromOverride() bool { return f.origin != "" }

func (f gatewayField) describe(name string) string {
	from := f.origin
	if from == "" {
		from = f.botSource
	}
	return fmt.Sprintf("%s %q from %s", name, f.value, from)
}

// undecidedText reports whether a field's text is decided at the runner: a
// `${…}` slot dial the pod's environment expands, or a `{{…}}` reference.
func undecidedText(v string) bool {
	return strings.Contains(v, "${") || strings.Contains(v, "{{")
}

// gatewayElement is one chain element as dispatch builds it.
type gatewayElement struct {
	backend, hint, model gatewayField
}

// gatewaySite is one call site's composed fields before dispatch splits
// them into elements: a node's primary splits its provider chain into steps
// (parseProviderChain) and sends them all on a backend that walks the chain,
// the head alone on any other (collapseHintOnlyChain); a route carries one
// hint.
type gatewaySite struct {
	route string
	// chain marks the node's primary, whose provider is a chain of
	// `hint[:model]` steps; a route's provider is one hint.
	chain                   bool
	backend, provider, base gatewayField
}

// elements builds the site's elements from its field values.
func (s gatewaySite) elements() []gatewayElement {
	if !s.chain {
		return []gatewayElement{{backend: s.backend, hint: normalizedHint(s.provider), model: s.base}}
	}
	var out []gatewayElement
	for _, token := range strings.Split(s.provider.value, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		hint, stepModel, _ := ir.SplitProviderStep(token)
		el := gatewayElement{backend: s.backend, hint: normalizedHint(gatewayField{value: hint, origin: s.provider.origin, botSource: s.provider.botSource}), model: s.base}
		if stepModel != "" {
			el.model = gatewayField{value: stepModel, origin: s.provider.origin, botSource: "a step of " + s.provider.botSource}
		}
		out = append(out, el)
	}
	// A backend that does not walk a hint-only chain sends its head alone
	// (collapseHintOnlyChain): the steps behind it are never built. An
	// unnamed backend reads "" here and collapses too — the runner picks
	// claw for a chain that needs the gateway, and the head is the one step
	// every backend builds.
	if len(out) > 1 && !model.WalksProviderChain(s.backend.value) {
		out = out[:1]
	}
	if len(out) == 0 {
		out = append(out, gatewayElement{backend: s.backend, hint: gatewayField{origin: s.provider.origin, botSource: s.provider.botSource}, model: s.base})
	}
	return out
}

// normalizedHint is a hint in the form dispatch sends it: lower-case, an
// explicit `auto` deferring to the process.
func normalizedHint(f gatewayField) gatewayField {
	f.value = strings.ToLower(strings.TrimSpace(f.value))
	if f.value == "auto" {
		f.value = ""
	}
	return f
}

// judge returns the site's first element dispatch refuses, as a finding
// body: the refusal with the provenance of the fields its arm read.
// undecided reports a refusal judged on a dial's default; an element
// dispatch refuses whatever its model dial expands to is decided.
func (s gatewaySite) judge() (undecided bool, err error) {
	// Decided: a field a dial decides is read as the value that refuses
	// nothing on its own — a backend "" serves a gateway route, a hint ""
	// names no wire — so a refusal here holds whatever the dial expands to.
	decided := s
	if undecidedText(decided.backend.value) {
		decided.backend.value = ""
	}
	if undecidedText(decided.provider.value) {
		decided.provider = gatewayField{origin: s.provider.origin, botSource: s.provider.botSource}
	}
	for _, el := range decided.elements() {
		if el.model.value == "" {
			continue
		}
		if undecidedText(el.model.value) {
			if err := refusedWhateverTheModel(el); err != nil {
				return false, err
			}
			continue
		}
		if err := causedRefusal(el); err != nil {
			return false, err
		}
	}
	if !undecidedText(s.backend.value) && !undecidedText(s.provider.value) && !undecidedText(s.base.value) {
		return false, nil
	}
	// A dial decides a field at the runner: its default text is the
	// reading this launch can name.
	dial := s
	for _, f := range []*gatewayField{&dial.backend, &dial.provider, &dial.base} {
		if undecidedText(f.value) {
			f.value = strings.TrimSpace(ir.ExpandWithDefault(f.value, func(string) string { return "" }))
		}
	}
	for _, el := range dial.elements() {
		if el.model.value == "" || undecidedText(el.model.value) || undecidedText(el.backend.value) || undecidedText(el.hint.value) {
			continue
		}
		if err := causedRefusal(el); err != nil {
			return true, fmt.Errorf("%w — read on the defaults of the dials the runner expands (%s)", err, s.dialTexts())
		}
	}
	return false, nil
}

// gatewayModelProbe and otherModelProbe stand for every value a model
// dial may expand to, in the two classes GatewayCrossingRefusal tells
// apart: a gateway route, and any other model.
const (
	gatewayModelProbe = llmroute.ProviderOpenAICompatible + "/<gateway model id>"
	otherModelProbe   = "<a model that is not a gateway route>"
)

// refusedWhateverTheModel judges an element whose model a dial decides at
// the runner: dispatch refuses it whatever the dial expands to when it
// refuses a gateway route and any other model alike, each refusal caused
// by a field an override wrote (causedRefusal). The refusal names both.
func refusedWhateverTheModel(el gatewayElement) error {
	gateway, other := el, el
	gateway.model.value, other.model.value = gatewayModelProbe, otherModelProbe
	gatewayErr := causedRefusal(gateway)
	if gatewayErr == nil {
		return nil
	}
	otherErr := causedRefusal(other)
	if otherErr == nil {
		return nil
	}
	return fmt.Errorf("dispatch refuses this element whatever %s expands to — a gateway route: %v; any other model: %v", el.model.describe("model"), gatewayErr, otherErr)
}

// dialTexts names the site's fields a dial decides, as written.
func (s gatewaySite) dialTexts() string {
	var out []string
	for _, f := range []struct {
		name string
		f    gatewayField
	}{{"backend", s.backend}, {"provider", s.provider}, {"model", s.base}} {
		if undecidedText(f.f.value) {
			out = append(out, fmt.Sprintf("%s %q", f.name, f.f.value))
		}
	}
	return strings.Join(out, ", ")
}

// causedRefusal is dispatch's refusal of el when an override field takes
// part in it: the arm that refused read a field an override entry wrote.
// The refusal carries the provenance of every field the arm read and, for
// the backend arm on a backend the bot wrote, the remedy.
func causedRefusal(el gatewayElement) error {
	err := model.GatewayCrossingRefusal(el.backend.value, el.hint.value, el.model.value)
	if err == nil {
		return nil
	}
	var gce *model.GatewayCrossingError
	if !errors.As(err, &gce) {
		return err
	}
	var read []gatewayField
	var names []string
	for _, f := range []struct {
		on    bool
		name  string
		field gatewayField
	}{{gce.Backend, "backend", el.backend}, {gce.Hint, "provider", el.hint}, {gce.Model, "model", el.model}} {
		if f.on {
			read = append(read, f.field)
			names = append(names, f.field.describe(f.name))
		}
	}
	caused := false
	for _, f := range read {
		caused = caused || f.fromOverride()
	}
	if !caused {
		return nil
	}
	msg := fmt.Sprintf("%v (%s)", err, strings.Join(names, "; "))
	if gce.Backend && !el.backend.fromOverride() {
		msg += ` — pin "backend": "claw" in the cell, the backend that serves gateway routes`
	}
	return errors.New(msg)
}

// gatewayCrossingFindings judges every element the folded overrides
// compose, per node (its provider chain's steps) and per authored route.
func gatewayCrossingFindings(wf *ir.Workflow, overrides model.ModelOverrides, folded []sourcedOverrideEntry, runBackend string) []composedPairFinding {
	var out []composedPairFinding
	for _, id := range overrideSiteNodeIDs(wf) {
		node := wf.Nodes[id]
		fields := routingFieldsOf(node)
		if fields == nil {
			continue
		}
		res, src := overrides.ForNodeFieldSources(id, node.NodeKind())
		origin := func(fs model.FieldSource, value, field string) string {
			if value == "" {
				return ""
			}
			return foldedEntryOrigin(folded, fs, value, field)
		}
		backend := nodeGatewayBackend(wf, fields, runBackend, res.Backend, origin(src.Backend, res.Backend, llmroute.OverrideFieldBackend))
		provider := gatewayField{value: fields.Provider, botSource: "the node's own provider field"}
		if res.Provider != "" {
			provider = gatewayField{value: res.Provider, origin: origin(src.Provider, res.Provider, llmroute.OverrideFieldProvider)}
		}
		base := gatewayField{value: fields.Model, botSource: "the node's own model field"}
		if res.Model != "" {
			base = gatewayField{value: res.Model, origin: origin(src.Model, res.Model, llmroute.OverrideFieldModel)}
		}
		sites := []gatewaySite{{chain: true, backend: backend, provider: provider, base: base}}
		for _, r := range authoredRoutes(node, overrides) {
			if r.authored.Action == ir.FallbackActionSkip {
				continue
			}
			// The route as the run pins it (EffectiveFallback); a field the
			// route names neither way is its node's.
			site := model.FieldSource{Selector: r.site}
			routeField := func(value, pinned, field string) gatewayField {
				f := gatewayField{value: value, botSource: "the route's own " + field + " field"}
				if pinned != "" {
					f.origin = origin(site, pinned, field)
				}
				return f
			}
			rs := gatewaySite{route: r.site, backend: backend, base: base}
			if r.effective.Backend != "" {
				rs.backend = routeField(r.effective.Backend, r.pin.Backend, llmroute.OverrideFieldBackend)
			}
			rs.provider = routeField(r.effective.Provider, r.pin.Provider, llmroute.OverrideFieldProvider)
			if r.effective.Model != "" {
				rs.base = routeField(r.effective.Model, r.pin.Model, llmroute.OverrideFieldModel)
			}
			sites = append(sites, rs)
		}
		for _, s := range sites {
			if undecided, err := s.judge(); err != nil {
				out = append(out, composedPairFinding{kind: node.NodeKind(), id: id, route: s.route, err: err, element: true, undecided: undecided})
			}
		}
	}
	return out
}

// nodeGatewayBackend is the backend a node's elements inherit, in
// dispatch's precedence: the override, else the node's own `backend:`, else
// the run's default — the launch's run-level backend when one replaces the
// workflow's (runBackend), else the workflow's `default_backend:` — a field
// that falls through ("", `auto`) passing to the next. "" when none names
// one: the runner's environment or its detection decides, and detection
// picks claw for a chain that needs the gateway.
func nodeGatewayBackend(wf *ir.Workflow, fields *ir.LLMFields, runBackend, override, overrideOrigin string) gatewayField {
	if override != "" {
		return gatewayField{value: override, origin: overrideOrigin}
	}
	runDefault := gatewayField{value: wf.DefaultBackend, botSource: "the workflow's default_backend field"}
	if strings.TrimSpace(runBackend) != "" {
		runDefault = gatewayField{value: runBackend, botSource: "the launch's run-level backend"}
	}
	for _, f := range []gatewayField{
		{value: fields.Backend, botSource: "the node's own backend field"},
		runDefault,
	} {
		f.value = strings.TrimSpace(f.value)
		if !ir.BackendFallsThrough(f.value) {
			return f
		}
	}
	return gatewayField{}
}
