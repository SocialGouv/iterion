package server

import (
	"os"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// buildEffectiveSettings resolves the three launch-relevant knobs
// (compress / permission / backend) BELOW the run-override level —
// workflow DSL, then env, then default — and flags whether any node
// pins its own value (a run override won't reach those nodes). The
// Launch dialog layers the operator's own override on top client-side,
// so this stays a pure function of (workflow, env).
func buildEffectiveSettings(wf *ir.Workflow) *previewEffectiveSettings {
	if wf == nil {
		return nil
	}
	eff := &previewEffectiveSettings{
		Compress:   resolveKnob(wf.Compress, os.Getenv("ITERION_COMPRESS"), "auto"),
		Permission: resolveKnob(wf.Permission, os.Getenv("ITERION_PERMISSION"), "off"),
		Backend:    resolveKnob(wf.DefaultBackend, os.Getenv("ITERION_DEFAULT_BACKEND"), "auto"),
	}
	for _, node := range wf.Nodes {
		switch n := node.(type) {
		case *ir.AgentNode:
			eff.Compress.NodePinned = eff.Compress.NodePinned || n.Compress != ""
			eff.Permission.NodePinned = eff.Permission.NodePinned || n.Permission != ""
			eff.Backend.NodePinned = eff.Backend.NodePinned || n.Backend != ""
		case *ir.JudgeNode:
			eff.Compress.NodePinned = eff.Compress.NodePinned || n.Compress != ""
			eff.Permission.NodePinned = eff.Permission.NodePinned || n.Permission != ""
			eff.Backend.NodePinned = eff.Backend.NodePinned || n.Backend != ""
		case *ir.ToolNode:
			eff.Permission.NodePinned = eff.Permission.NodePinned || n.Permission != ""
		}
	}
	return eff
}

// resolveKnob applies the workflow > env > default tail of the
// precedence chain ("run_override" and "node" are the caller's/
// NodePinned's business).
func resolveKnob(workflow, env, def string) previewEffectiveKnob {
	if workflow != "" {
		return previewEffectiveKnob{Effective: workflow, Source: "workflow"}
	}
	if env != "" {
		return previewEffectiveKnob{Effective: env, Source: "env"}
	}
	return previewEffectiveKnob{Effective: def, Source: "default"}
}
