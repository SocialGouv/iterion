package model

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawhooks "github.com/SocialGouv/claw-code-go/pkg/api/hooks"
	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// parentPermissionCtxKey carries the parent loop's tool-permission policy to
// the tool executors the loop invokes — the subagent runner among them — so
// a child conversation is gated by the policy the parent answers to.
type parentPermissionCtxKey struct{}

// WithParentPermission attaches the policy the parent loop evaluates its own
// tool calls under. The claw backend sets it where it arms its own loop, and
// NewSubagentRunner reads it off the ctx every tool call already carries.
func WithParentPermission(ctx context.Context, policy *permission.Policy) context.Context {
	return context.WithValue(ctx, parentPermissionCtxKey{}, policy)
}

// ParentPermission returns the policy the parent tool loop runs under, or
// nil when the caller is not a gated loop.
func ParentPermission(ctx context.Context) *permission.Policy {
	policy, _ := ctx.Value(parentPermissionCtxKey{}).(*permission.Policy)
	return policy
}

// parentLoopCtxKey carries the shape of the claw loop a delegation is
// spawned from — the model it bills, the tools its author declared and the
// step budget it runs under — to the tool executors that loop invokes.
type parentLoopCtxKey struct{}

// ParentLoop is what a child conversation inherits from the NODE that spawned
// it. The claw backend fills it where it arms its own loop; NewSubagentRunner
// reads it off the ctx every tool call already carries.
//
// Tools is the node's EFFECTIVE set — what the author declared, plus what the
// engine granted it — never the process registry: a node restricting itself
// to `tools: [read_file]` must not reach bash through a child, and the
// registry a host hands the runner holds every tool every node of the run can
// call. The distinction is load-bearing, which is why the presence of a
// parent loop (ParentLoopFromContext's second return) decides where the
// child's tools come from rather than the slice being empty or not: a node
// that declared `tools: []` hands its child nothing, it does not fall back to
// the registry.
type ParentLoop struct {
	// Model is the parent node's resolved model spec, the default a child
	// runs on when neither the model's own argument nor the host names one.
	Model string
	// Tools is the parent node's effective tool set.
	Tools []GenerationTool
	// MaxSteps is the parent node's tool-loop step budget; 0 leaves the
	// child on the runner's own default.
	MaxSteps int
	// WorkDir is the parent node's workspace: where a file-backed subagent
	// (`<workspace>/.claude/agents/<name>.md`, #1714) is resolved from.
	WorkDir string
}

// WithParentLoop attaches the parent node's loop shape.
func WithParentLoop(ctx context.Context, p ParentLoop) context.Context {
	return context.WithValue(ctx, parentLoopCtxKey{}, p)
}

// ParentLoopFromContext returns the parent node's loop shape and whether a
// claw loop declared one. The boolean is the authority on "a node spawned
// this", not the fields: a tool-less node yields a zero Tools slice.
func ParentLoopFromContext(ctx context.Context) (ParentLoop, bool) {
	p, ok := ctx.Value(parentLoopCtxKey{}).(ParentLoop)
	return p, ok
}

// subagentSpendCtxKey carries the per-node accumulator a delegation debits.
type subagentSpendCtxKey struct{}

// SubagentSpend accumulates what the child conversations of ONE node cost.
// The node's own result reports the parent's turns only — a child runs inside
// a tool call, after the parent's usage is already counted — so without this
// a claw node could spend without limit through subagents while the run's
// budget (pkg/runtime.SharedBudget, fed by delegate.Result) saw nothing.
//
// Each child is priced on the model IT ran, not the parent's: a haiku child
// of an opus node is cheaper, and the single cost figure a Result carries
// cannot say so after the fact. A child whose model has no known price adds
// its tokens and no cost, exactly as an unpriced node does.
//
// Safe for concurrent use: a parent loop may execute several tool calls at
// once, and each delegation debits from its own goroutine.
type SubagentSpend struct {
	mu      sync.Mutex
	usage   Usage
	costUSD float64
}

// WithSubagentSpend attaches the accumulator the delegations of one node
// debit. The claw backend arms one per Execute and folds the total into the
// node's result.
func WithSubagentSpend(ctx context.Context, s *SubagentSpend) context.Context {
	return context.WithValue(ctx, subagentSpendCtxKey{}, s)
}

// SubagentSpendFromContext returns the accumulator for this node, or nil when
// the caller is not a metered loop.
func SubagentSpendFromContext(ctx context.Context) *SubagentSpend {
	s, _ := ctx.Value(subagentSpendCtxKey{}).(*SubagentSpend)
	return s
}

// Record debits one child conversation: its usage, and its cost at the price
// of the model that served it. Nil-safe, so a caller with no accumulator
// needs no branch of its own.
func (s *SubagentSpend) Record(modelSpec string, u Usage) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	accumulateUsage(&s.usage, u)
	s.costUSD += cost.EstimateUSD(modelSpec, u.InputTokens, u.OutputTokens)
}

// Total is what the children of this node consumed: the summed usage, and the
// summed cost of the models that served them. Nil-safe.
func (s *SubagentSpend) Total() (Usage, float64) {
	if s == nil {
		return Usage{}, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage, s.costUSD
}

// defaultSubagentMaxSteps bounds a child's tool loop when the parent node
// declares no step budget of its own.
const defaultSubagentMaxSteps = 10

// NewSubagentRunner returns a closure suitable as the executor for
// the claw `agent` tool. When the LLM dispatches the agent tool, the
// closure spins up a SYNCHRONOUS child conversation against the same
// model registry and returns the child's final text alongside its
// agent_id — a real result, never a start the host did not make.
//
// Everything the child is bounded by comes from the node that spawned it,
// read off the context the tool call already carries:
//
//   - its tools, the node's effective set (ParentLoop.Tools), minus `agent`
//     itself, the node-scoped MCP tools and the ask channel (the child is
//     not the node — filterSubagentTools);
//   - its permission policy (ParentPermission), so a deny or ask row the
//     parent answers to stops at the delegation too;
//   - its step budget (ParentLoop.MaxSteps);
//   - its spend, debited to the node through SubagentSpendFromContext;
//   - its definition, when subagent_type names a file-backed agent of the
//     node's workspace (ParentLoop.WorkDir → LookupWorkspaceAgent, #1714):
//     the file's body is the child's system prompt, its `tools:` narrows the
//     child's set (intersected, never widening), and a `model:` key refuses
//     the delegation (#1717).
//
// Hooks are propagated to the parent's event stream with a
// "subagent:<agent_id>" nodeID so a live test can observe the full
// sub-tree.
//
// The returned executor never recurses into itself: the `agent` tool
// is filtered out of the child's tool set, regardless of the
// subagent type's allowlist. defaultModel is the HOST's default, used
// when the model's own argument omits one; pass an empty string to let
// the parent node's model serve, which is what a production host does.
func NewSubagentRunner(
	modelReg *Registry,
	toolReg *tool.Registry,
	eventHooks EventHooks,
	lifecycle *clawhooks.Runner,
	defaultModel string,
) func(ctx context.Context, input map[string]any) (string, error) {
	return func(ctx context.Context, input map[string]any) (string, error) {
		spec, err := clawtools.ValidateAgentInput(input)
		if err != nil {
			return "", err
		}

		// claw's ValidateAgentInput substitutes a hard-coded default
		// model when the caller omits it. That default is bare
		// ("claude-opus-4-6"), but iterion's model registry resolves
		// "provider/model-id". Treat an unset input["model"] as
		// "use our defaultModel" so workflow authors don't have to
		// specify the model on every agent call.
		parent, fromParentLoop := ParentLoopFromContext(ctx)

		// A named subagent may be file-backed: `subagent_type: auditor`
		// resolves `<workspace>/.claude/agents/auditor.md` of the NODE's
		// workspace (#1714) — the file a plugin's `contributes: agents`
		// mirrors there, which claude_code discovers natively. Read before
		// anything is resolved or spawned: a file whose frontmatter declares
		// `model:` refuses the delegation outright (#1717), and a name that
		// spells traversal or a file the containment boundary refuses never
		// starts a child.
		var agent WorkspaceAgent
		agentFound := false
		if fromParentLoop && parent.WorkDir != "" {
			a, found, err := LookupWorkspaceAgent(parent.WorkDir, spec.SubagentType, workspaceAgentMaxBytes())
			if err != nil {
				return "", err
			}
			if found {
				if a.Model != "" {
					return "", fmt.Errorf("%w: %s (%s) declares model: %q — a contributed file does not re-route the node's spend; drop the key, or pass the model as the agent tool's own argument (#1717)",
						ErrWorkspaceAgentModel, a.Name, a.Path, a.Model)
				}
				agent, agentFound = a, true
			}
		}

		raw, _ := input["model"].(string)
		modelSpec := raw
		if modelSpec == "" {
			modelSpec = defaultModel
		}
		// Last, so a host that names a cheap default keeps it: the parent
		// node's model is what a production host leaves this runner to find,
		// since the model is a property of the NODE and this closure is built
		// once per run.
		if modelSpec == "" {
			modelSpec = parent.Model
		}
		if modelSpec == "" {
			return "", fmt.Errorf("subagent: no model specified, no host default and no parent node model")
		}

		client, err := modelReg.Resolve(modelSpec)
		if err != nil {
			return "", fmt.Errorf("subagent: resolve model %q: %w", modelSpec, err)
		}
		if _, _, err := ParseModelSpec(modelSpec); err != nil {
			return "", fmt.Errorf("subagent: parse model spec: %w", err)
		}

		allowed := clawtools.AllowedToolsForSubagent(spec.SubagentType)
		// A file-backed agent's `tools:` narrows the child further — and only
		// further: intersected with the per-type allowlist below and with the
		// node's effective set at the filter, so a contributed file can never
		// hand the child a tool the node does not hold.
		if agentFound {
			allowed = intersectToolSets(allowed, agent.ToolSet())
		}
		// Spawned from a node, the child selects from the NODE's effective
		// tools; spawned by a stand-alone host, from the registry that host
		// handed us. Never both: the registry holds every tool every node of
		// the run can call, so reading it under a node would widen the
		// author's declared surface at the delegation.
		genTools := buildSubagentTools(toolReg, allowed)
		if fromParentLoop {
			genTools = filterSubagentTools(parent.Tools, allowed)
		}
		maxSteps := defaultSubagentMaxSteps
		if parent.MaxSteps > 0 {
			maxSteps = parent.MaxSteps
		}

		// The child conversation's system prompt is the agent file's body
		// when one resolved — the file IS the agent's definition — and the
		// generic instruction otherwise.
		system := "You are a sub-agent. Complete the task with the tools available and return your final answer concisely."
		if agentFound && agent.Body != "" {
			system = agent.Body
		}

		opts := GenerationOptions{
			Model:  modelSpec,
			System: system,
			Messages: []api.Message{{
				Role:    "user",
				Content: []api.ContentBlock{{Type: "text", Text: spec.Prompt}},
			}},
			Tools:    genTools,
			MaxSteps: maxSteps,
			Hooks:    lifecycle,
			// The child inherits the policy the parent loop answers to: a
			// deny or ask row over a tool class bounds the calls the child
			// makes, the same calls the parent could not make itself. A nil
			// (or off) parent policy leaves the child ungated, exactly like
			// the parent's own loop.
			Permission: ParentPermission(ctx),
		}
		applyHooks("subagent:"+spec.AgentID, LoopIterationFromContext(ctx), eventHooks, &opts)

		result, err := GenerateTextDirect(ctx, client, opts)
		// Debited before the error branch: tokens a child burned are billed
		// whether or not it answered, and GenerateTextDirect returns the
		// usage it accumulated with its error.
		SubagentSpendFromContext(ctx).Record(modelSpec, partialUsage(result))
		if err != nil {
			return "", fmt.Errorf("subagent %s: %w", spec.AgentID, err)
		}

		out := map[string]any{
			"agent_id":      spec.AgentID,
			"name":          spec.Name,
			"description":   spec.Description,
			"subagent_type": spec.SubagentType,
			"model":         modelSpec,
			"status":        "completed",
			"text":          result.Text,
			"input_tokens":  result.TotalUsage.InputTokens,
			"output_tokens": result.TotalUsage.OutputTokens,
		}
		// A file-backed resolution is reported, so the delegating model (and
		// a human reading the tool result) can tell a named agent ran its own
		// definition rather than the generic shape.
		if agentFound {
			out["workspace_agent"] = agent.Name
			if agent.Description != "" {
				out["workspace_agent_description"] = agent.Description
			}
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		return string(b), nil
	}
}

// buildSubagentTools selects tools from the iterion registry to
// expose to a subagent, applying claw's per-type allowlist (nil =
// general-purpose, all tools allowed). The `agent` tool itself is
// always filtered to prevent recursion.
//
// MCP is withheld entirely, in both of its shapes: the per-server
// `mcp.<server>.<tool>` tools, and the three builtins that take a server NAME
// the model writes (list_mcp_resources, read_mcp_resource, mcp_auth). Which
// MCP servers may be reached is a property of the NODE — its `mcp:` block —
// and a child conversation is not that node: it answers a prompt the parent
// MODEL wrote, so a server reached there is reached on the model's word
// rather than on the author's. The parent relays what the child needs
// instead. Withholding also covers the stand-alone host below, which selects
// from the process registry, where the set spans every node of the run and
// claw's own per-type allowlist does not help (nil means "all").
func buildSubagentTools(reg *tool.Registry, allowed map[string]bool) []GenerationTool {
	defs := reg.List()
	tools := make([]GenerationTool, 0, len(defs))
	for _, td := range defs {
		tools = append(tools, GenerationTool{
			Name:        td.QualifiedName,
			Description: td.Description,
			InputSchema: td.InputSchema,
			Execute:     td.Execute,
		})
	}
	return filterSubagentTools(tools, allowed)
}

// filterSubagentTools is the withholding rule, applied to whichever set the
// child draws from: no recursion into `agent`, no node-scoped MCP tool, no
// ask-channel tool, and claw's per-type allowlist on top (nil =
// general-purpose, every name allowed).
//
// The ask family (ask_user and the async pair) is withheld for the reason
// the MCP tools are: the child is not the node. ask_user raises
// delegate.ErrAskUser to pause the RUN, and a child's call raises it from
// inside the parent's tool call — the parent's askUserResult path still
// recognises it through the wrap, so the run pauses on a question the child
// asked, attributed to nobody, and the child's conversation is not persisted
// for the resume to return to (#2812). A child that needs an operator's
// answer reports the question back to its parent, whose OWN ask channel is
// intact. The withholding covers the stand-alone host too, like the MCP one
// above: a pause raised outside any run is an error with no handler at all.
//
// Known residual, out of this withholding's reach: the child inherits the
// parent's permission POLICY (ParentPermission), and an `ask` verdict of a
// permission rule on a tool the child holds — bash, say — raises the same
// ErrAskUser from inside the child's tool call (permissionGateOutcome). The
// withholding covers the ask TOOLS, not the policy's ask VERDICT; closing
// that second path is its own decision (refuse, or downgrade to deny, at the
// delegation).
func filterSubagentTools(tools []GenerationTool, allowed map[string]bool) []GenerationTool {
	out := make([]GenerationTool, 0, len(tools))
	for _, t := range tools {
		if t.Name == "agent" {
			continue
		}
		if isNodeScopedMCPTool(t.Name) {
			continue
		}
		if isRunPausingAskTool(t.Name) {
			continue
		}
		if allowed != nil && !allowed[t.Name] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// isRunPausingAskTool names the ask channel: the blocking ask_user and the
// async pair. Read from the same spellings the runtime appends them under
// (executor_build_task.go), never a fresh string.
func isRunPausingAskTool(name string) bool {
	switch name {
	case askUserToolName, delegate.AskUserAsyncToolName, delegate.AwaitAnswersToolName:
		return true
	}
	return false
}

// isNodeScopedMCPTool reports whether a tool's reach is decided by the NODE
// that declared it rather than by the tool itself: every per-server MCP tool,
// and the three builtins that name their server in the model's own argument.
//
// A caller with no node must not hand these out — there is nothing to scope
// them by.
func isNodeScopedMCPTool(qualified string) bool {
	if _, ok := tool.MCPServerOf(qualified); ok {
		return true
	}
	return mcpServerNamingTools[qualified]
}
