package sandbox

// Resolve produces the effective [Spec] for a node from the precedence
// chain node > workflow > global > implicit-none.
//
// Precedence rules:
//
//   - If `node.Mode` is non-empty (anything other than [ModeInherit]),
//     the node spec wins outright. This is how a tool node opts out of
//     a sandboxed workflow with `sandbox: none`.
//   - Else if `workflow.Mode` is non-empty, the workflow spec wins.
//   - Else if `global.Mode` is non-empty, the global spec wins.
//   - Else the result is a Spec with [ModeNone] — explicit no-sandbox.
//
// Network rules at node scope are merged into the parent according to
// `node.Network.Inherit` ([InheritMerge] | [InheritReplace] |
// [InheritAppend]); see [mergeNetwork].
//
// Any of node/workflow/global may be nil — a nil spec contributes
// nothing.
//
// The returned Spec is always non-nil; for explicit opt-out it carries
// `Mode: none` (rather than nil) so callers can distinguish "user said
// no sandbox" from "no resolver state".
func Resolve(global, workflow, node *Spec) *Spec {
	resolved := pickActive(global, workflow, node)

	// Node-level network override: even if the node didn't claim the
	// activation (Mode inherited from workflow), a node may still
	// refine the network rules of the inherited spec.
	if node != nil && node.Network != nil && resolved != nil {
		resolved = withMergedNetwork(resolved, node.Network)
	}

	if resolved == nil {
		return &Spec{Mode: ModeNone}
	}
	return resolved
}

// pickActive walks node > workflow > global and returns the first one
// whose Mode is set (anything other than [ModeInherit]). The returned
// pointer is a *copy* — callers can mutate without disturbing the
// inputs. Nil is returned only when all three inputs are nil or all
// have Mode == ModeInherit.
func pickActive(global, workflow, node *Spec) *Spec {
	if node != nil && node.Mode != ModeInherit {
		c := *node
		return &c
	}
	if workflow != nil && workflow.Mode != ModeInherit {
		c := *workflow
		return &c
	}
	if global != nil && global.Mode != ModeInherit {
		c := *global
		return &c
	}
	return nil
}

// withMergedNetwork returns a copy of base with its Network field
// composed with the override per the override's Inherit field.
//
// The base's Inherit field is ignored (it is a node-only directive);
// the override's Inherit drives the merge.
func withMergedNetwork(base *Spec, override *Network) *Spec {
	c := *base
	c.Network = mergeNetwork(base.Network, override)
	return &c
}

// mergeNetwork composes a parent and a node-level Network spec.
//
// Semantics:
//
//   - InheritReplace: the node spec replaces the parent entirely.
//     Even Mode and Preset come from the node.
//   - InheritMerge (default) or InheritAppend: parent rules first,
//     node rules appended after. With last-match-wins semantics in
//     the proxy, this means node rules can override parent decisions
//     for specific hosts.
//   - When the node leaves Mode unset, the parent's Mode is preserved.
//   - When the node sets Preset, it replaces the parent's Preset
//     (presets are atomic — composing two presets is ambiguous).
func mergeNetwork(parent, node *Network) *Network {
	if node == nil {
		return parent
	}
	if parent == nil || node.Inherit == InheritReplace {
		// Strip the Inherit marker — it has no meaning post-merge.
		c := *node
		c.Inherit = InheritMerge
		return &c
	}

	merged := &Network{
		Mode:   parent.Mode,
		Preset: parent.Preset,
		Rules:  append([]string(nil), parent.Rules...),
	}
	if node.Mode != NetworkModeUnset {
		merged.Mode = node.Mode
	}
	if node.Preset != "" {
		merged.Preset = node.Preset
	}
	merged.Rules = append(merged.Rules, node.Rules...)
	return merged
}
