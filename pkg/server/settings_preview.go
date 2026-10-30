package server

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/backend/automemory"
	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
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
			envOperator("ITERION_PERMISSION"),
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
			envOperator("ITERION_DEFAULT_BACKEND"),
		)
		choices := make(map[string]previewBackendOption, len(backendNames))
		for _, backend := range backendNames {
			backend = strings.TrimSpace(backend)
			if backend == "" {
				continue
			}
			choice := previewBackendOption{}
			choice.UnavailableReason = ir.UngatedCrossingReasonForAskRules(
				backend,
				mode.String(),
				ir.EffectiveAskRules(llm, wf),
			)
			if choice.UnavailableReason == "" {
				choice.Warning = ir.ToolRestrictionLossReason(
					currentBackend,
					backend,
					llm.GetTools(),
				)
			}
			if backend == backendcaps.ClaudeCode {
				// The roster drift a claude_code choice carries whatever the
				// node's list says (iterion#2815) — the same projection the
				// delegate spawns from, never a second list.
				if drift := claudeCodeRosterDriftNotice(llm); drift != "" {
					if choice.Warning != "" {
						choice.Warning += " "
					}
					choice.Warning += drift
				}
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

// claudeCodeRosterDriftNotice is the launch picker's half of the
// capability-drift disclosure (iterion#2815): what a claude_code choice
// withholds from THIS node whatever its tools: list says, and the subagent
// pair a list without `agent` lets go. Read off the delegate's own
// projection (delegate.ClaudeCodeToolSurface) — the roster is the CLI's,
// not iterion's, and a second enumeration here is how #1651 happened. The
// reason per name is `iterion validate --explain`'s surface; the picker
// carries the one sentence.
func claudeCodeRosterDriftNotice(llm ir.LLMNode) string {
	tools := llm.GetTools()
	surface := delegate.ClaudeCodeToolSurface(delegate.Task{
		AllowedTools:    tools,
		ToolsDeclared:   toolcatalog.ToolsDeclared(tools),
		Readonly:        llm.GetLLMFields().Readonly,
		DiagnosticShell: slices.Contains(tools, "diagnostic_shell"),
	})
	var always []string
	subagentLeaves := false
	for _, w := range surface.Withheld {
		switch {
		case w.Always && w.OnRoster:
			always = append(always, w.Name)
		case !w.Always && (w.Name == "Agent" || w.Name == "Task"):
			subagentLeaves = true
		}
	}
	if len(always) == 0 && !subagentLeaves {
		return ""
	}
	var b strings.Builder
	if len(always) > 0 {
		fmt.Fprintf(&b, "claude_code withholds %s from every node whatever its tools: list says — a headless session has nobody to ask, no background shell and no later turn", strings.Join(always, ", "))
	}
	if subagentLeaves {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString("the subagent tools Agent/Task also leave: the tools: list does not declare `agent`")
	}
	b.WriteString(" (per name and reason: iterion validate --explain)")
	return b.String()
}

// effectivePreviewBackend mirrors the backend tail that is knowable before
// launch. A node pin wins over the run's default-backend override; "auto" is
// treated as unset at either layer. Credential auto-detection stays unknown,
// which only suppresses a best-effort tools-drift warning — never a gate
// refusal.
//
// Each layer is read through ir.SourceBackendName, the reading the compiler
// gives the same text: a `${VAR:-claw}` pin IS claw, and reading it raw made
// the picker treat it as a backend nobody has — so the capability-drift
// disclosure it owes the operator vanished on exactly the nodes that carry a
// dial.
func effectivePreviewBackend(node, run, workflow, env string) string {
	for _, value := range []string{node, run, workflow, env} {
		if resolved := ir.SourceBackendName(value); resolved != "" {
			return resolved
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
		// ITERION_COMPRESS stays a raw read: not operator-grade — a workflow
		// knob a bot's stored record may carry.
		Compress:       resolveKnob(wf.Compress, os.Getenv("ITERION_COMPRESS"), "auto"),
		AutoMemory:     resolveKnob(wf.AutoMemory, normalizedAutoMemoryEnv(), "off"),
		AmbientContext: resolveKnob(wf.AmbientContext, normalizedAmbientContextEnv(), ambient.Workspace.String()),
		Permission:     resolveKnob(wf.Permission, envOperator("ITERION_PERMISSION"), "off"),
		Backend:        resolveKnob(wf.DefaultBackend, envOperator("ITERION_DEFAULT_BACKEND"), "auto"),
	}
	for _, node := range wf.Nodes {
		switch n := node.(type) {
		case *ir.AgentNode:
			eff.Compress.NodePinned = eff.Compress.NodePinned || n.Compress != ""
			eff.AutoMemory.NodePinned = eff.AutoMemory.NodePinned || n.AutoMemory != ""
			eff.AmbientContext.NodePinned = eff.AmbientContext.NodePinned || n.AmbientContext != ""
			eff.Permission.NodePinned = eff.Permission.NodePinned || n.Permission != ""
			eff.Backend.NodePinned = eff.Backend.NodePinned || n.Backend != ""
		case *ir.JudgeNode:
			eff.Compress.NodePinned = eff.Compress.NodePinned || n.Compress != ""
			eff.AutoMemory.NodePinned = eff.AutoMemory.NodePinned || n.AutoMemory != ""
			eff.AmbientContext.NodePinned = eff.AmbientContext.NodePinned || n.AmbientContext != ""
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
	raw := strings.TrimSpace(envOperator(automemory.ModeEnv))
	if raw == "" {
		return ""
	}
	return automemory.ParseMode(raw).String()
}

// normalizedAmbientContextEnv reports what ITERION_AMBIENT_CONTEXT will
// ACTUALLY mean to a run: its canonical spelling when valid, and "" when unset
// or invalid. ambient.ResolveSourced ignores an invalid value, so the run
// falls back to the default, and the preview attributes it to the default
// rather than captioning a value no run would ever use.
func normalizedAmbientContextEnv() string {
	p, ok := ambient.Parse(envOperator(ambient.PolicyEnv))
	if !ok {
		return ""
	}
	return p.String()
}
