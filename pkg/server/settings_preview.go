package server

import (
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/automemory"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// buildBackendOverrideOptions screens the backend names currently offered by
// the Studio for every LLM node. The request carries those names rather than
// duplicating a backend catalog here: a newly detected backend is therefore
// fail-closed by the same IR predicate without requiring a UI capability
// table update.
func buildBackendOverrideOptions(
	wf *ir.Workflow,
	runPermission string,
	runBackend string,
	backendNames []string,
) map[string]map[string]previewBackendOption {
	if wf == nil || len(backendNames) == 0 {
		return nil
	}

	result := make(map[string]map[string]previewBackendOption)
	for _, node := range wf.Nodes {
		llm, ok := node.(ir.LLMNode)
		if !ok {
			continue
		}
		mode, _, err := permission.ResolveModeSourced(
			runPermission,
			llm.GetPermission(),
			wf.Permission,
			os.Getenv("ITERION_PERMISSION"),
		)
		if err != nil {
			// Validation remains authoritative for malformed modes, but the
			// picker still fails closed instead of advertising every backend
			// while the workflow itself is unlaunchable.
			choices := make(map[string]previewBackendOption, len(backendNames))
			for _, backend := range backendNames {
				backend = strings.TrimSpace(backend)
				if backend != "" {
					choices[backend] = previewBackendOption{UnavailableReason: fmt.Sprintf(
						"cannot assess permission safety until the invalid permission mode is fixed: %v",
						err,
					)}
				}
			}
			if len(choices) > 0 {
				result[llm.NodeID()] = choices
			}
			continue
		}
		currentBackend := effectivePreviewBackend(
			llm.GetLLMFields().Backend,
			runBackend,
			wf.DefaultBackend,
			os.Getenv("ITERION_DEFAULT_BACKEND"),
		)
		choices := make(map[string]previewBackendOption, len(backendNames))
		for _, backend := range backendNames {
			backend = strings.TrimSpace(backend)
			if backend == "" {
				continue
			}
			choice := previewBackendOption{}
			choice.UnavailableReason = ir.UngatedCrossingReason(
				backend,
				mode.String(),
				len(wf.PermissionAsk) > 0,
			)
			if choice.UnavailableReason == "" {
				choice.Warning = ir.ToolRestrictionLossReason(
					currentBackend,
					backend,
					llm.GetTools(),
				)
			}
			choices[backend] = choice
		}
		if len(choices) > 0 {
			result[llm.NodeID()] = choices
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// effectivePreviewBackend mirrors the backend tail that is knowable before
// launch. A node pin wins over the run's default-backend override; "auto" is
// treated as unset at either layer. Credential auto-detection stays unknown,
// which only suppresses a best-effort tools-drift warning — never a gate
// refusal.
func effectivePreviewBackend(node, run, workflow, env string) string {
	for _, value := range []string{node, run, workflow, env} {
		value = strings.TrimSpace(value)
		if value != "" && value != "auto" {
			return value
		}
	}
	return ""
}

// buildEffectiveSettings resolves the launch-relevant knobs
// (compress / auto_memory / permission / backend) BELOW the run-override level —
// workflow DSL, then env, then default — and flags whether any node pins its
// own value. The Launch dialog layers the operator's override on top using
// each knob's precedence (permission/compress/memory run overrides win node
// pins; the default-backend setting does not), so this stays a pure function
// of (workflow, env).
func buildEffectiveSettings(wf *ir.Workflow) *previewEffectiveSettings {
	if wf == nil {
		return nil
	}
	eff := &previewEffectiveSettings{
		Compress:   resolveKnob(wf.Compress, os.Getenv("ITERION_COMPRESS"), "auto"),
		AutoMemory: resolveKnob(wf.AutoMemory, normalizedAutoMemoryEnv(), "off"),
		Permission: resolveKnob(wf.Permission, os.Getenv("ITERION_PERMISSION"), "off"),
		Backend:    resolveKnob(wf.DefaultBackend, os.Getenv("ITERION_DEFAULT_BACKEND"), "auto"),
	}
	for _, node := range wf.Nodes {
		switch n := node.(type) {
		case *ir.AgentNode:
			eff.Compress.NodePinned = eff.Compress.NodePinned || n.Compress != ""
			eff.AutoMemory.NodePinned = eff.AutoMemory.NodePinned || n.AutoMemory != ""
			eff.Permission.NodePinned = eff.Permission.NodePinned || n.Permission != ""
			eff.Backend.NodePinned = eff.Backend.NodePinned || n.Backend != ""
		case *ir.JudgeNode:
			eff.Compress.NodePinned = eff.Compress.NodePinned || n.Compress != ""
			eff.AutoMemory.NodePinned = eff.AutoMemory.NodePinned || n.AutoMemory != ""
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

// normalizedAutoMemoryEnv reports what ITERION_AUTO_MEMORY will ACTUALLY mean
// to a run, rather than what it says.
//
// automemory.ParseMode maps anything it does not recognise to off, so echoing
// the raw value made the launch dialog caption a mode no run would ever be in
// — `ITERION_AUTO_MEMORY=banana` was announced as the effective setting while
// every node ran hermetically.
//
// An UNSET variable still resolves to "": the preview distinguishes "the
// environment said nothing" (fall through to the default) from "the
// environment said off", and collapsing the two would attribute the default to
// a source that never spoke.
func normalizedAutoMemoryEnv() string {
	raw := strings.TrimSpace(os.Getenv(automemory.ModeEnv))
	if raw == "" {
		return ""
	}
	return automemory.ParseMode(raw).String()
}
