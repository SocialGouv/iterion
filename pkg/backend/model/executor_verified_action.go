package model

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Verified Action ladder (ADR-044).
//
// A tool node that declares a `postcondition:` becomes a "Verified Action":
// the postcondition — a cheap DETERMINISTIC shell check (exit 0 = met) — is
// the SINGLE SOURCE OF TRUTH for success, checked at every rung. This is
// what makes the adaptive recovery rungs safe: an agent cannot fake success
// past a deterministic property check.
//
// Escalation (cheapest → most adaptive):
//  1. Idempotent skip — postcondition already met before running? skip the
//     recipe (resume / retry safe).
//  2. Recipe — run the existing command/script. Postcondition met → done.
//     The recipe's own exit code is IGNORED when the postcondition holds
//     (exit codes lie: "nothing to commit" exits 1 though the goal may hold).
//  3. Self-repair (policy: recover) — an LLM proposes a CORRECTED command
//     from {goal, recipe, stdout, stderr}; the runtime re-runs it
//     deterministically (the corrected command is emitted as a tool_called
//     event — auditable, no blind side effect). Bounded.
//  4. Agent recovery (policy: recover, opt-in) — an agent achieves the goal
//     with real tools. Bounded; OFF unless max_agent_attempts > 0.
//  5. Policy — still unmet: required/recover → fail (resumable);
//     best_effort → warn + continue.
//
// Default fallback model for the self-repair / agent rungs when the node's
// recovery: block names none. Mirrors defaultRouterModel — a cheap, capable
// default that works for the common Anthropic-credentialled host.
const defaultVerifiedActionModel = "anthropic/claude-sonnet-4-6"

// selfRepairSchema is the structured-output contract for the self-repair
// rung: the model returns a corrected shell command (and its reasoning).
const selfRepairSchema = `{
  "type": "object",
  "properties": {
    "corrected_command": {"type": "string", "description": "A corrected shell command that achieves the goal."},
    "reasoning": {"type": "string", "description": "Why the original failed and what the correction changes."}
  },
  "required": ["corrected_command"]
}`

// executeVerifiedToolNode runs a tool node through the Verified Action
// escalation ladder. Only reached when node.Postcondition != "".
func (e *ClawExecutor) executeVerifiedToolNode(ctx context.Context, node *ir.ToolNode, input map[string]any) (map[string]any, error) {
	policy := node.Policy
	if policy == "" {
		policy = ir.PolicyRequired
	}

	// Rung 1 — idempotent skip.
	met, skipOut, pcErr := e.runPostcondition(ctx, node, input)
	if pcErr != nil {
		return nil, pcErr
	}
	if met {
		e.logVA(node.ID, "idempotent-skip: postcondition already met; recipe not run")
		return e.verifiedOutput(skipOut, nil, "idempotent_skip", true, policy), nil
	}

	// Rung 2 — recipe.
	res, repairable, setupErr := e.runVerifiedRecipe(ctx, node, input)
	if setupErr != nil {
		// Build / policy failure — not a recoverable recipe error.
		return nil, setupErr
	}
	met, pcOut, pcErr := e.runPostcondition(ctx, node, input)
	if pcErr != nil {
		return nil, pcErr
	}
	if met {
		// Recipe's own runErr is intentionally ignored — the goal holds.
		return e.verifiedOutput(res.output, pcOut, "recipe", true, policy), nil
	}

	lastRung := "recipe"

	// Rungs 3-4 only run under policy: recover.
	if policy == ir.PolicyRecover && node.Recovery != nil {
		// Rung 3 — self-repair (bounded). Only for command/script recipes.
		if repairable && node.Recovery.MaxRepairAttempts > 0 {
			lastStdout, lastStderr, lastCmd := res.stdout, res.stderr, res.resolved
			for i := 0; i < node.Recovery.MaxRepairAttempts; i++ {
				corrected, repairRes, rerr := e.selfRepair(ctx, node, lastStdout, lastStderr, lastCmd)
				if rerr != nil {
					e.logVA(node.ID, fmt.Sprintf("self-repair attempt %d aborted: %v", i+1, rerr))
					break
				}
				lastRung = "self_repair"
				met, pcOut, pcErr = e.runPostcondition(ctx, node, input)
				if pcErr != nil {
					return nil, pcErr
				}
				if met {
					e.logVA(node.ID, fmt.Sprintf("self-repair satisfied postcondition on attempt %d", i+1))
					return e.verifiedOutput(repairRes.output, pcOut, "self_repair", true, policy), nil
				}
				lastStdout, lastStderr, lastCmd = repairRes.stdout, repairRes.stderr, corrected
			}
		}

		// Rung 4 — agent recovery (opt-in; OFF unless budgeted).
		if node.Recovery.MaxAgentAttempts > 0 {
			for i := 0; i < node.Recovery.MaxAgentAttempts; i++ {
				if aerr := e.agentRecovery(ctx, node, input, lastRung); aerr != nil {
					e.logVA(node.ID, fmt.Sprintf("agent recovery attempt %d aborted: %v", i+1, aerr))
					break
				}
				lastRung = "agent_recovery"
				met, pcOut, pcErr = e.runPostcondition(ctx, node, input)
				if pcErr != nil {
					return nil, pcErr
				}
				if met {
					e.logVA(node.ID, fmt.Sprintf("agent recovery satisfied postcondition on attempt %d", i+1))
					return e.verifiedOutput(pcOut, nil, "agent_recovery", true, policy), nil
				}
			}
		}
	}

	// Rung 5 — policy.
	if policy == ir.PolicyBestEffort {
		e.logVA(node.ID, "postcondition unmet after recipe; policy: best_effort → warn + continue")
		out := e.verifiedOutput(res.output, nil, lastRung, false, policy)
		return out, nil
	}

	// required / recover: fail (resumable). The recipe's stderr is the most
	// useful diagnostic; surface it alongside the unmet-postcondition reason.
	return nil, fmt.Errorf("model: tool node %q: postcondition not met (policy: %s) after %s\nstdout: %s\nstderr: %s",
		node.ID, policy, lastRung, strings.TrimSpace(res.stdout), strings.TrimSpace(res.stderr))
}

// runVerifiedRecipe runs the node's recipe (rung 2) and reports whether it
// is repairable (command/script — a self-repair rung can propose a corrected
// command). Registry-tool recipes run via the standard path and are not
// command-repairable. The returned error is a setup error (build/policy),
// never a recipe run error (that lives in recipeResult.runErr).
func (e *ClawExecutor) runVerifiedRecipe(ctx context.Context, node *ir.ToolNode, input map[string]any) (recipeResult, bool, error) {
	switch recipeKindOf(node) {
	case recipeScript:
		resolve, buildCmd := e.scriptRecipe(ctx, node, input)
		res, err := e.runToolNodeCore(ctx, node, scriptToolNodeToolName(node), resolve, buildCmd)
		return res, true, err
	case recipeShell:
		resolve, buildCmd := e.shellRecipe(ctx, node, input)
		res, err := e.runToolNodeCore(ctx, node, shellToolNodeToolName(node), resolve, buildCmd)
		return res, true, err
	default:
		// Registry tool (bare name): run via the standard recipe path. No
		// command to self-repair, so report non-repairable.
		out, rerr := e.executeToolNodeRecipe(ctx, node, input)
		return recipeResult{output: out, runErr: rerr}, false, nil
	}
}

// runPostcondition resolves and runs the node's postcondition shell check.
// It is the truth oracle, evaluated at every rung. exit 0 = met. Its stdout
// (when valid JSON) becomes the skip / success output so authors can surface
// state (e.g. the resulting commit sha). Routed through runToolNodeCore so
// it is sandbox-aware and visible as a tool_called event.
func (e *ClawExecutor) runPostcondition(ctx context.Context, node *ir.ToolNode, input map[string]any) (met bool, output map[string]any, err error) {
	resolve := func() string {
		expanded := expandBracedEnv(node.Postcondition)
		expanded = resolveRunRefs(expanded, RunIDFromContext(ctx), TemplateDataFromContext(ctx), node.PostcondRefs, shellEscapeValue)
		// A postcondition is the node's second shell command body, resolved
		// with the same escaper, and its refs are validated against the same
		// input schema — so a `json`-declared field holding a list breaks out
		// of its assignment here exactly as it does in the command itself.
		return resolveCommandTemplate(expanded, node.PostcondRefs, e.jsonFieldsAsText(node, input), e.vars, e.secretGuard)
	}
	buildCmd := func(resolved string) (*exec.Cmd, func(), error) {
		materialized, env := e.secretGuard.MaterializeShellEnv(resolved)
		return e.toolNodeCommand(ctx, materialized, env), nil, nil
	}
	res, setupErr := e.runToolNodeCore(ctx, node, postcondToolName(node), resolve, buildCmd)
	if setupErr != nil {
		return false, nil, fmt.Errorf("model: tool node %q: postcondition: %w", node.ID, setupErr)
	}
	return res.runErr == nil, res.output, nil
}

// selfRepair (rung 3) asks an LLM for a corrected command from the failure
// context, then re-runs it deterministically as a shell command (sandbox-
// aware, secret-materialised). The resolved corrected command is emitted as
// a tool_called event — the model fixes the recipe, it never does the side
// effect blind. Returns the corrected command + its run result.
func (e *ClawExecutor) selfRepair(ctx context.Context, node *ir.ToolNode, stdout, stderr, lastCmd string) (string, recipeResult, error) {
	modelSpec := e.recoveryModel(node)
	client, err := e.registry.Resolve(modelSpec)
	if err != nil {
		return "", recipeResult{}, fmt.Errorf("resolve recovery model %q: %w", modelSpec, err)
	}

	userMsg := fmt.Sprintf(`A deterministic command failed to achieve its goal. Propose a single corrected shell command.

GOAL:
%s

FAILED COMMAND:
%s

STDOUT:
%s

STDERR:
%s

Return only the corrected command (one shell invocation, may use && / pipes). Do not explain in the command itself.`,
		strings.TrimSpace(node.Goal), strings.TrimSpace(lastCmd), truncate(stdout, 4000), truncate(stderr, 4000))

	genOpts := GenerationOptions{
		Model:          modelSpec,
		System:         "You are a precise build/release engineer. You correct a single failing shell command so it achieves the stated goal, accounting for the error output. You return strictly structured output.",
		Messages:       []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: userMsg}}}},
		ExplicitSchema: json.RawMessage(selfRepairSchema),
	}

	result, err := GenerateObjectDirect[map[string]any](ctx, client, genOpts)
	if err != nil {
		return "", recipeResult{}, fmt.Errorf("self-repair generation: %w", err)
	}
	corrected, _ := result.Object["corrected_command"].(string)
	corrected = strings.TrimSpace(corrected)
	if corrected == "" {
		return "", recipeResult{}, fmt.Errorf("self-repair returned an empty command")
	}

	// Run the corrected command deterministically as a shell command. The
	// resolved (placeholder-form) command is what hooks/events persist.
	resolve := func() string { return corrected }
	buildCmd := func(resolved string) (*exec.Cmd, func(), error) {
		materialized, env := e.secretGuard.MaterializeShellEnv(resolved)
		return e.toolNodeCommand(ctx, materialized, env), nil, nil
	}
	res, setupErr := e.runToolNodeCore(ctx, node, "self_repair:"+node.ID, resolve, buildCmd)
	if setupErr != nil {
		return corrected, recipeResult{}, setupErr
	}
	return corrected, res, nil
}

// agentRecovery (rung 4) dispatches a synthetic agent that achieves the
// goal with real tools. Opt-in (max_agent_attempts > 0). The agent's
// success is irrelevant on its own — the postcondition re-check after this
// call is the truth.
//
// TODO(ADR-044): this hand-builds an ir.AgentNode and smuggles the task via
// the input map (relying on buildUserMessage's no-schema fallback). It works
// for the opt-in first cut but bypasses the engine's per-node accounting
// (budget, node events, capability→tool opening). Replace with a typed
// recovery-agent entry point on the generation/backend layer.
func (e *ClawExecutor) agentRecovery(ctx context.Context, node *ir.ToolNode, input map[string]any, lastRung string) error {
	recovery := node.Recovery
	syn := &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: node.ID + "__recover"},
		LLMFields: ir.LLMFields{
			Model: e.recoveryModel(node),
		},
		Tools: recovery.AgentTools,
	}
	// The goal + failure context flow as the agent's user message (no output
	// schema → buildUserMessage serialises this input map).
	agentInput := map[string]any{
		"instructions":    "Achieve the GOAL below using your tools. A prior deterministic attempt did not satisfy the postcondition.",
		"goal":            node.Goal,
		"last_rung":       lastRung,
		"original_recipe": cmp.Or(node.Command, node.Script),
	}
	for k, v := range input {
		// Pass through the node's structured input so the agent has the same
		// context the recipe had (workspace_dir, base_sha, file lists, …).
		if _, clash := agentInput[k]; !clash {
			agentInput[k] = v
		}
	}
	if _, err := e.executeBackend(ctx, syn, agentInput); err != nil {
		return err
	}
	return nil
}

// recoveryModel resolves the model spec for the recovery rungs: the node's
// recovery.model (env-expanded) when set, else ITERION_VERIFIED_ACTION_MODEL,
// else the package default.
func (e *ClawExecutor) recoveryModel(node *ir.ToolNode) string {
	if node.Recovery != nil && node.Recovery.Model != "" {
		if m := ir.ExpandEnvWithDefault(node.Recovery.Model); m != "" {
			return m
		}
	}
	if m := ir.LookupEnv("ITERION_VERIFIED_ACTION_MODEL"); m != "" {
		return m
	}
	return defaultVerifiedActionModel
}

// verifiedOutput merges the recipe output with the postcondition's JSON
// stdout (postcondition wins on key clash — it observed the final state) and
// stamps the private _verified_action metadata the engine reads to emit the
// node_verified_action event. The key is stripped before schema validation.
func (e *ClawExecutor) verifiedOutput(primary, postcond map[string]any, rung string, met bool, policy string) map[string]any {
	out := map[string]any{}
	for k, v := range primary {
		out[k] = v
	}
	for k, v := range postcond {
		out[k] = v
	}
	out["_verified_action"] = map[string]any{
		"rung":              rung,
		"postcondition_met": met,
		"policy":            policy,
	}
	return out
}

func (e *ClawExecutor) logVA(nodeID, msg string) {
	if e.logger != nil {
		e.logger.Info("[%s/verified-action] %s", nodeID, msg)
	}
}

// postcondToolName is the virtual tool name used for the postcondition's
// policy check + hooks. Operators can allow it with "postcondition:<id>".
func postcondToolName(node *ir.ToolNode) string {
	return "postcondition:" + node.ID
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
