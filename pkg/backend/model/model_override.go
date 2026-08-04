package model

import (
	"fmt"
	"path"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// NodeModelOverride is the resolved launch-time override for a single node:
// which backend / model / provider to use instead of the node's DSL values.
// Empty fields mean "leave the node's resolved value unchanged".
type NodeModelOverride struct {
	Backend  string
	Model    string
	Provider string
}

// Empty reports whether the override carries no directive (all fields blank).
func (o NodeModelOverride) Empty() bool {
	return o.Backend == "" && o.Model == "" && o.Provider == ""
}

// modelOverrideRule is one selector→override directive, in insertion order.
type modelOverrideRule struct {
	selector string
	NodeModelOverride
}

// ModelOverrides is an ordered set of launch-time per-node/per-group
// backend+model+provider overrides, populated from the studio Launch
// dropdowns or the CLI --model / --backend flags. The zero value applies
// nothing, so a run with no overrides behaves exactly as before.
//
// A selector matches a node by (most specific first):
//   - exact node id            ("reviewer_claude")
//   - id glob                  ("reviewer_*", "fix_*")   — path.Match syntax
//   - node-kind keyword        ("agent", "judge", "router")
//   - "*"                      every LLM node (run-level default)
//
// For each field (backend/model/provider) the most specific matching rule
// that sets a non-empty value wins; ties resolve to the later rule. Because
// resolution is per-field, `--model 'reviewer_*=X'` and `--backend '*=claw'`
// compose (the reviewer gets model X on backend claw). Overrides sit at the
// TOP of the resolution chain — above the node's DSL backend:/model: — because
// the operator is explicitly re-targeting the bot at launch without editing
// the .bot.
type ModelOverrides struct {
	rules []modelOverrideRule
}

// Empty reports whether no rules are configured.
func (o ModelOverrides) Empty() bool { return len(o.rules) == 0 }

// SetModel adds a model directive for the selector.
func (o *ModelOverrides) SetModel(selector, model string) {
	o.add(selector, NodeModelOverride{Model: model})
}

// SetBackend adds a backend directive for the selector.
func (o *ModelOverrides) SetBackend(selector, backend string) {
	o.add(selector, NodeModelOverride{Backend: backend})
}

// SetProvider adds a provider directive for the selector.
func (o *ModelOverrides) SetProvider(selector, provider string) {
	o.add(selector, NodeModelOverride{Provider: provider})
}

func (o *ModelOverrides) add(selector string, ov NodeModelOverride) {
	selector = strings.TrimSpace(selector)
	if selector == "" || ov.Empty() {
		return
	}
	o.rules = append(o.rules, modelOverrideRule{selector: selector, NodeModelOverride: ov})
}

// ForNode resolves the effective override for a node id + kind. Returns an
// empty override when nothing matches (the caller then keeps the node's DSL
// values). Resolution is per-field: the most specific matching rule setting a
// non-empty value for that field wins.
func (o ModelOverrides) ForNode(id string, kind ir.NodeKind) NodeModelOverride {
	var out NodeModelOverride
	var bs, ms, ps int // best score seen per field
	for _, r := range o.rules {
		score, ok := selectorScore(r.selector, id, kind)
		if !ok {
			continue
		}
		if r.Backend != "" && score >= bs {
			out.Backend, bs = r.Backend, score
		}
		if r.Model != "" && score >= ms {
			out.Model, ms = r.Model, score
		}
		if r.Provider != "" && score >= ps {
			out.Provider, ps = r.Provider, score
		}
	}
	return out
}

// selectorScore returns a specificity score (higher = more specific) and
// whether the selector matches the node. Ordering: "*" < kind keyword < glob
// (by literal length) < exact id.
func selectorScore(selector, id string, kind ir.NodeKind) (int, bool) {
	switch selector {
	case "*":
		return 1, true
	case "agent":
		return 10, kind == ir.NodeAgent
	case "judge":
		return 10, kind == ir.NodeJudge
	case "router":
		return 10, kind == ir.NodeRouter
	}
	if selector == id {
		return 100000, true
	}
	if strings.ContainsAny(selector, "*?[") {
		if ok, err := path.Match(selector, id); err == nil && ok {
			return 100 + literalLen(selector), true
		}
		return 0, false
	}
	return 0, false
}

// literalLen counts the non-wildcard characters of a glob, used to rank two
// matching globs (the more literal one is more specific).
func literalLen(pat string) int {
	n := 0
	for _, r := range pat {
		if r != '*' && r != '?' && r != '[' && r != ']' {
			n++
		}
	}
	return n
}

// splitSelectorValue parses a "selector=value" flag argument. A bare value
// with no '=' targets every LLM node (selector "*"), so `--model X` sets X
// run-wide while `--model 'fix_*=X'` targets a group.
func splitSelectorValue(arg string) (selector, value string, err error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", "", fmt.Errorf("empty override")
	}
	i := strings.Index(arg, "=")
	if i < 0 {
		return "*", arg, nil
	}
	selector = strings.TrimSpace(arg[:i])
	value = strings.TrimSpace(arg[i+1:])
	if selector == "" {
		selector = "*"
	}
	if value == "" {
		return "", "", fmt.Errorf("override %q has empty value", arg)
	}
	return selector, value, nil
}

// ParseRunFallback parses the `--fallback` flag value into the
// operator's run-level route.
//
// The form is `<backend>:<model>` — split on the FIRST colon, so a model
// id that itself contains one survives. A bare value with no colon is
// read as a backend, since a model with no backend cannot be routed to
// (model specs are not portable across backends).
//
// Deliberately no trigger syntax here: the flag takes the default
// `on:` set, and an operator who needs a different one is authoring a
// chain, which belongs in the .bot.
func ParseRunFallback(arg string) (RunFallback, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return RunFallback{}, nil
	}
	backend, model, _ := strings.Cut(arg, ":")
	backend = strings.TrimSpace(backend)
	model = strings.TrimSpace(model)
	if backend == "" {
		return RunFallback{}, fmt.Errorf("--fallback %q: missing backend (expected <backend>:<model>)", arg)
	}
	return RunFallback{Backend: backend, Model: model}, nil
}

// ParseModelOverrides builds a ModelOverrides from repeatable CLI flag values.
// Each element of modelFlags/backendFlags is a "selector=value" (or a bare
// "value" targeting every node). Returns a descriptive error on a malformed
// element so the run fails fast instead of silently ignoring a typo.
func ParseModelOverrides(modelFlags, backendFlags []string) (ModelOverrides, error) {
	var o ModelOverrides
	for _, m := range modelFlags {
		sel, val, err := splitSelectorValue(m)
		if err != nil {
			return o, fmt.Errorf("--model: %w", err)
		}
		o.SetModel(sel, val)
	}
	for _, b := range backendFlags {
		sel, val, err := splitSelectorValue(b)
		if err != nil {
			return o, fmt.Errorf("--backend: %w", err)
		}
		o.SetBackend(sel, val)
	}
	return o, nil
}
