package model

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// backendFields holds the common fields extracted from AgentNode or JudgeNode
// for the executeBackend unified path.
type backendFields struct {
	id               string
	model            string
	backend          string
	provider         string
	command          string // node-level `command:` CLI binary override (honored by claude_code)
	systemPrompt     string
	userPrompt       string
	reasoningEffort  string
	outputSchema     string
	tools            []string
	toolMaxSteps     int
	maxTokens        int
	session          ir.SessionMode
	interaction      ir.InteractionMode
	activeMCPServers []string
	compaction       *ir.Compaction
	memory           *ir.Memory
	capabilities     []string
	skills           []string
	cursors          *ir.CursorInvocation
	compress         string   // node-level `compress:` value ("" = unset)
	permission       string   // node-level `permission:` mode override ("" = inherit)
	timeout          string   // node-level `timeout:` Go duration ("" = no per-node bound); may contain ${VAR} env refs
	readonly         bool     // node-level `readonly:` — force delegated agents into a read-only sandbox
	fullAccess       bool     // node-level `full_access:` — lift the codex sandbox to danger-full-access (network egress)
	images           []string // node-level `images:` — templated input image paths forwarded to codex as `-i` (i2i)
}

// extractBackendFields normalises the LLM-relevant fields shared by
// AgentNode and JudgeNode into a single struct. Returns an error for
// any other node type so a future ir/ addition can't crash the binary
// in production — the engine's executeNode dispatch already filters
// to these two cases, but defensive typing here keeps that contract
// localised.
func extractBackendFields(node ir.Node) (backendFields, error) {
	switch n := node.(type) {
	case *ir.AgentNode:
		return backendFields{
			id: n.ID, model: n.Model, backend: n.Backend, provider: n.Provider,
			command:      n.Command,
			systemPrompt: n.SystemPrompt, userPrompt: n.UserPrompt,
			reasoningEffort: n.ReasoningEffort, outputSchema: n.OutputSchema,
			tools: n.Tools, toolMaxSteps: n.ToolMaxSteps,
			maxTokens:        n.MaxTokens,
			session:          n.Session,
			interaction:      n.Interaction,
			activeMCPServers: n.ActiveMCPServers,
			compaction:       n.Compaction,
			memory:           n.Memory,
			capabilities:     n.Capabilities,
			skills:           n.Skills,
			cursors:          n.Cursors,
			compress:         n.Compress,
			permission:       n.Permission,
			timeout:          n.Timeout,
			readonly:         n.Readonly,
			fullAccess:       n.FullAccess,
			images:           n.Images,
		}, nil
	case *ir.JudgeNode:
		return backendFields{
			id: n.ID, model: n.Model, backend: n.Backend, provider: n.Provider,
			command:      n.Command,
			systemPrompt: n.SystemPrompt, userPrompt: n.UserPrompt,
			reasoningEffort: n.ReasoningEffort, outputSchema: n.OutputSchema,
			tools: n.Tools, toolMaxSteps: n.ToolMaxSteps,
			maxTokens:        n.MaxTokens,
			session:          n.Session,
			interaction:      n.Interaction,
			activeMCPServers: n.ActiveMCPServers,
			compaction:       n.Compaction,
			memory:           n.Memory,
			capabilities:     n.Capabilities,
			skills:           n.Skills,
			cursors:          n.Cursors,
			compress:         n.Compress,
			permission:       n.Permission,
			timeout:          n.Timeout,
			readonly:         n.Readonly,
			fullAccess:       n.FullAccess,
			images:           n.Images,
		}, nil
	default:
		return backendFields{}, fmt.Errorf("model: extractBackendFields called with unsupported node type %T", node)
	}
}

// resolvePermissionPolicy builds the effective tool-permission policy for
// a node. Mode precedence mirrors rtk (run override > node DSL > workflow
// DSL > ITERION_PERMISSION env > off); the allow/ask/deny rule lists are
// the union of the workflow-level lists and the run-level override lists.
// Returns a disabled policy (mode off) when nothing opts in. A malformed
// rule or unknown mode is an error (surfaced as a node execution error;
// compile-time validation already flags these via C110/C111).
func (e *ClawExecutor) resolvePermissionPolicy(nodeMode string) (*permission.Policy, error) {
	mode, err := permission.ParseMode(cmp.Or(e.permOverride, nodeMode, e.wfPermission, e.permEnvDefault))
	if err != nil {
		return nil, err
	}
	if mode == permission.ModeOff {
		return &permission.Policy{}, nil
	}
	return permission.NewPolicy(mode,
		slices.Concat(e.wfPermAllow, e.permAllowRules),
		slices.Concat(e.wfPermAsk, e.permAskRules),
		slices.Concat(e.wfPermDeny, e.permDenyRules),
	)
}

// stampDelegateOutputMeta writes per-call observability keys onto the
// output map: _tokens, _backend, _session_id, plus the effective
// model / context window / peak load / output cap (claude_code; left
// unset by backends that don't report them). The four "_model" /
// "_context_*" / "_max_output_tokens" keys drive the run-view's
// per-node model label and context-usage gauge.
//
// `output` is passed explicitly so the LLM router path can re-stamp
// after a `{"text": …}` fallback has reassigned to a fresh map.
func stampDelegateOutputMeta(output map[string]any, result delegate.Result, backendName string) {
	if output == nil {
		return
	}
	if output["_tokens"] == nil {
		output["_tokens"] = result.Tokens
	}
	output["_backend"] = backendName
	if result.SessionID != "" {
		output["_session_id"] = result.SessionID
	}
	if result.SessionFingerprint != "" {
		output["_session_fingerprint"] = result.SessionFingerprint
	}
	if result.EffectiveModel != "" {
		output["_model"] = result.EffectiveModel
	}
	if result.ContextWindow > 0 {
		output["_context_window"] = result.ContextWindow
	}
	if result.PeakInputTokens > 0 {
		output["_context_used"] = result.PeakInputTokens
	}
	if result.MaxOutputTokens > 0 {
		output["_max_output_tokens"] = result.MaxOutputTokens
	}
	if result.ThinkingTokens > 0 {
		output["_thinking_tokens"] = result.ThinkingTokens
	}
	if result.ThinkingMs > 0 {
		output["_thinking_ms"] = result.ThinkingMs
	}
}

// dispatchWithObservability wraps dispatchWithProviderFallback with the
// 3-hook lifecycle every agent/judge/LLM-router call paid by hand:
// OnDelegateStarted fires before dispatch; on error, OnDelegateError
// receives a DelegateInfo with the backend-reported name (or the
// requested name when the backend didn't report one) and the error is
// wrapped as `<errPrefix> %q: backend %q failed: %w`; on success,
// OnDelegateFinished fires with the result-derived DelegateInfo. The
// caller propagates the wrapped error untouched. Extracted from
// executeBackend / executeLLMRouterUnified — only the error wrap prefix
// differs (`model: node` vs `model: llm router`).
func (e *ClawExecutor) dispatchWithObservability(
	ctx context.Context,
	nodeID, backendName, errPrefix string,
	chain []providerStep,
	backend delegate.Backend,
	task *delegate.Task,
) (delegate.Result, error) {
	if e.hooks.OnDelegateStarted != nil {
		e.hooks.OnDelegateStarted(nodeID, backendName)
	}
	result, err := e.dispatchWithProviderFallback(ctx, nodeID, backendName, chain, backend, task)
	if err != nil {
		if e.hooks.OnDelegateError != nil {
			bn := result.BackendName
			if bn == "" {
				bn = backendName
			}
			di := delegateInfoFromResult(bn, result)
			di.Error = err
			e.hooks.OnDelegateError(nodeID, di)
		}
		return result, fmt.Errorf("%s %q: backend %q failed: %w", errPrefix, nodeID, backendName, err)
	}
	if e.hooks.OnDelegateFinished != nil {
		e.hooks.OnDelegateFinished(nodeID, delegateInfoFromResult(result.BackendName, result))
	}
	return result, nil
}

// executeBackend is the unified execution path for agent and judge nodes.
// It resolves the backend, builds a Task, and dispatches to the backend.
func (e *ClawExecutor) executeBackend(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	f, err := extractBackendFields(node)
	if err != nil {
		return nil, err
	}

	// Per-node `timeout:` bounds this node's whole backend interaction
	// (task build + dispatch + schema-retry). WithTimeout derives from the
	// incoming ctx, so whichever fires first — this bound or the workflow
	// budget deadline already carried on ctx — wins; expiry surfaces as
	// context.DeadlineExceeded and fails the node cleanly. Compile-time
	// validation (C122) already rejects malformed durations, so a parse
	// error here is defensive: skip the bound rather than fail the node.
	if f.timeout != "" {
		if d, derr := time.ParseDuration(ir.ExpandEnvWithDefault(f.timeout)); derr == nil && d > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}

	backendName := e.resolveBackendName(node)

	if e.backendRegistry == nil {
		return nil, fmt.Errorf("model: node %q uses backend %q but no backend registry configured", f.id, backendName)
	}

	backend, err := e.backendRegistry.Resolve(backendName)
	if err != nil {
		return nil, fmt.Errorf("model: node %q: %w", f.id, err)
	}

	task, err := e.buildTask(ctx, node, f, input, backendName)
	if err != nil {
		return nil, err
	}

	// For claw backends emit a tagged log line so the studio's per-node
	// Logs tab (which greps `[<nodeID>#<iter>/...]`) surfaces the call.
	// claude_code/codex subprocesses already produce equivalent tagged
	// lines from their stderr capture path. Iter is hardcoded to 0 —
	// same limitation as the per-tool tagging above; per-iter filtering
	// requires plumbing LoopIteration through the hook chain. Lives at
	// the executeBackend call site (the LLM-router path has no equivalent
	// log line) so dispatchWithObservability stays a pure 3-hook helper.
	if backendName == delegate.BackendClaw && e.logger != nil {
		toolSuffix := ""
		if n := len(task.AllowedTools); n > 0 {
			toolSuffix = fmt.Sprintf(", %d tools", n)
		}
		e.logger.Info("[%s#%d/claw] 🤖 LLM call: %s%s",
			f.id, 0, task.Model, toolSuffix)
	}

	result, err := e.dispatchWithObservability(ctx, f.id, backendName, "model: node", e.resolveProviderChain(node), backend, &task)
	if err != nil {
		return nil, err
	}

	// Flag if structured output parsing fell back to text wrapper.
	if result.ParseFallback {
		result.Output["_parse_fallback"] = true
	}

	// Attach metadata.
	stampDelegateOutputMeta(result.Output, result, backendName)

	// Check for a backend interaction signal BEFORE schema validation.
	// A `_needs_interaction` pause Result (e.g. an LLM ask_user call on a
	// node that ALSO declares an output schema) is a control signal, not a
	// schema-shaped data output — its Output is {_needs_interaction, …},
	// which never matches the node's schema. Validating it first would fail
	// and trigger the schema-validation backend retry below, which replays
	// the unanswered tool_call into a fresh generation (openai 400
	// "tool_call_ids did not have response messages" / Responses
	// "No tool output found"). Short-circuit here so ask_user pauses
	// cleanly on schema+tools nodes (e.g. claw + openai/forfait). See
	// docs/bot-runs/evolve.md.
	// A permission-gate `ask` pause also surfaces as `_needs_interaction`,
	// but the node need not have opted into `interaction:` — the gate is
	// its own reason to pause. Recognise it by the permission marker so
	// such a pause converts cleanly here too.
	needsInteraction, _ := result.Output["_needs_interaction"].(bool)
	questions, _ := result.Output["_interaction_questions"].(map[string]any)
	_, isPermissionPause := questions[permission.InteractionMarkerKey]
	if needsInteraction && (f.interaction != ir.InteractionNone || isPermissionPause) {
		{
			if questions == nil {
				questions = map[string]any{"input": "The backend needs your input to continue."}
			}
			delete(result.Output, "_needs_interaction")
			delete(result.Output, "_interaction_questions")
			return nil, &ErrNeedsInteraction{
				NodeID:           f.id,
				Questions:        questions,
				SessionID:        result.SessionID,
				Backend:          backendName,
				Conversation:     result.PendingConversation,
				PendingToolUseID: result.PendingToolUseID,
			}
		}
	}

	// Validate output against schema if present. Defence-in-depth: the
	// IR compiler should reject any node whose `output:` names a schema
	// absent from e.schemas (DiagUnknownSchema), so the "key missing"
	// branch here normally cannot happen. Log it loudly if it does —
	// silently skipping validation in that case would mask either an
	// IR regression or a programmatic schema-map mutation.
	if f.outputSchema != "" {
		if schema, ok := e.schemas[f.outputSchema]; ok {
			validated, err := e.validateAndRetry(ctx, f, backendName, backend, &task, result, schema)
			if err != nil {
				return nil, err
			}
			result = validated
		} else {
			e.logger.Warn("[%s#%d/%s] node declares output schema %q but no schema with that name is registered — IR compiler should have rejected this; output passes through unvalidated",
				f.id, task.Iteration, backendName, f.outputSchema)
		}
	}

	return result.Output, nil
}

// validateAndRetry validates result.Output against the node's schema. On
// success, the input result is returned unchanged. On a validation
// failure that one retry can plausibly fix (parse-fallback OR missing-
// required-field), one retry through retryDelegateLoop is attempted —
// inheriting the standard transient-backoff budget — and the retry
// result is re-validated. The retry does not replay the identical prompt:
// its UserPrompt (plus, for multimodal tasks, an extra text ContentBlock)
// is augmented with a delimited feedback block naming the validation error
// so the model can correct itself. The OnDelegateRetry observer hook fires
// for the schema-fallback retry (otherwise invisible to outer observers,
// which only see transient-error retries), token / duration are
// accumulated across the first attempt + retry so per-node accounting
// reflects the full cost paid, and stampDelegateOutputMeta is re-applied
// after the retry so observability keys remain consistent. Any other
// validation failure (type mismatch, enum violation) or a retry that
// still fails returns a wrapped error; the caller propagates it.
//
// Why retry on missing-field errors: a real-world failure mode (Seki's
// voter judges on gpt-5.5/forfait — see docs/bot-runs/sec-audit-source.md)
// is the backend returning *valid* JSON that simply omits required fields
// when the model is overwhelmed or the response is truncated. Without
// a retry, one transient empty response hard-fails the whole run on the
// first voter, even though the downstream aggregator (majority_verdict
// behind `await: best_effort`) is already designed to tolerate a missing
// voter. One retry on a missing-field error is the bounded-and-cheap
// counterpart to the existing parse-fallback retry. Type / enum errors
// stay non-retryable: those are the model returning the *wrong shape*,
// which a second call from the same model is unlikely to flip.
func (e *ClawExecutor) validateAndRetry(
	ctx context.Context,
	f backendFields,
	backendName string,
	backend delegate.Backend,
	task *delegate.Task,
	result delegate.Result,
	schema *ir.Schema,
) (delegate.Result, error) {
	err := ValidateOutput(result.Output, schema)
	if err == nil {
		return result, nil
	}
	// Retry-eligible: parse-fallback (LLM returned non-JSON the executor
	// wrapped in a text field) OR missing-required-field (LLM returned
	// truncated/empty JSON). Non-eligible (type mismatch, enum violation)
	// are returned by the model in a stable shape; a second call is
	// unlikely to change them.
	retryEligible := result.ParseFallback || isMissingFieldError(err)
	if !retryEligible {
		return result, fmt.Errorf("model: node %q: structured output invalid: %w", f.id, err)
	}
	e.logger.Warn("[%s#%d/%s] structured output validation failed, retrying backend: %v", f.id, task.Iteration, backendName, err)
	// Fire OnDelegateRetry so observers (Prometheus exporter, event sink)
	// see the retry attempt — previously the schema-validation retry was
	// invisible because the outer retryDelegateLoop only knows about
	// transient errors, not schema-shape failures.
	if e.hooks.OnDelegateRetry != nil {
		di := delegateInfoFromResult(backendName, result)
		di.Error = err
		di.Attempt = 1
		e.hooks.OnDelegateRetry(f.id, di)
	}
	// Build a retry copy of the task whose UserPrompt (and, for multimodal
	// tasks, an extra text ContentBlock) carries a delimited feedback block
	// naming the validation failure, so the model can correct its output
	// instead of blindly re-running the identical prompt. The ORIGINAL task
	// is preserved untouched — its token/duration accounting has already
	// been accumulated above and must not be disturbed.
	retryTask := *task
	feedback := formatSchemaRetryFeedback(err)
	retryTask.UserPrompt = appendSchemaRetryFeedback(retryTask.UserPrompt, feedback)
	if len(retryTask.UserContent) > 0 {
		// Copy the slice header so appending feedback to the retry task does
		// not mutate the original task's backing array.
		retryTask.UserContent = append(
			append([]delegate.ContentBlock(nil), retryTask.UserContent...),
			delegate.ContentBlock{Type: "text", Text: feedback},
		)
	}
	// Route the schema-fallback retry through retryDelegateLoop so it
	// inherits the same transient-error backoff every other delegate call
	// gets — a direct backend.Execute here skipped the retry budget and
	// gave up on the first transient SDK hiccup.
	retryResult, retryErr := e.retryDelegateLoop(ctx, f.id, backendName, func() (delegate.Result, error) {
		return backend.Execute(ctx, retryTask)
	})
	if retryErr != nil || retryResult.ParseFallback {
		// The same backend still couldn't emit schema-valid JSON. The steady
		// state here is claude_code under the Anthropic OAuth *forfait*, which
		// cannot produce native structured output at all (proven: even a
		// trivial 2-field schema fails, while free-form prompts succeed). The
		// forfait is usable ONLY by claude_code, so we can't just switch the
		// node to claw. Instead, keep the agent's reasoning (done under the
		// forfait) and extract the schema from its free-form text via claw with
		// whatever provider the host has (openai/anthropic, key or forfait) — so
		// a structured-output node works on EVERY backend×credential combo, not
		// only api-key Anthropic. Fires only on the already-failing path, so it
		// can only turn a hard failure into a success.
		if out, ok := e.extractStructuredViaClaw(ctx, f.id, task, retryResult, result, schema, backendName); ok {
			return out, nil
		}
		return result, fmt.Errorf("model: node %q: structured output invalid: %w", f.id, err)
	}
	// Accumulate token/duration from the first attempt so per-node
	// accounting reflects the full cost paid (dropping it understated
	// the run's real usage and broke budget enforcement at the margins).
	retryResult.Tokens += result.Tokens
	retryResult.Duration += result.Duration
	// Re-attach metadata and re-validate.
	stampDelegateOutputMeta(retryResult.Output, retryResult, backendName)
	if retryValErr := ValidateOutput(retryResult.Output, schema); retryValErr != nil {
		return retryResult, fmt.Errorf("model: node %q: structured output invalid after retry: %w", f.id, retryValErr)
	}
	return retryResult, nil
}

// extractStructuredViaClaw is the last-resort structured-output recovery. When
// a backend finished with free-form text but no schema-valid JSON (the
// steady-state failure for claude_code under the Anthropic OAuth forfait, which
// cannot emit native structured output), re-derive the schema from that text
// via a direct claw call using whatever provider the host detects
// (openai/anthropic; API key or forfait). Returns (result, true) only on a
// schema-valid extraction; otherwise (_, false) and the caller surfaces the
// original error. Purely additive — it runs only on the already-failing path.
func (e *ClawExecutor) extractStructuredViaClaw(
	ctx context.Context,
	nodeID string,
	task *delegate.Task,
	primary delegate.Result,
	secondary delegate.Result,
	schema *ir.Schema,
	sourceBackend string,
) (delegate.Result, bool) {
	if len(task.OutputSchema) == 0 {
		return delegate.Result{}, false
	}
	text := fallbackText(primary.Output)
	if strings.TrimSpace(text) == "" {
		text = fallbackText(secondary.Output)
	}
	if strings.TrimSpace(text) == "" {
		return delegate.Result{}, false
	}
	modelSpec := e.detectorSuggestedModel()
	if modelSpec == "" {
		e.logger.Warn("[%s] structured-output recovery skipped: no claw provider detected", nodeID)
		return delegate.Result{}, false
	}
	client, err := e.registry.Resolve(modelSpec)
	if err != nil {
		e.logger.Warn("[%s] structured-output recovery: resolve %q: %v", nodeID, modelSpec, err)
		return delegate.Result{}, false
	}
	genOpts := GenerationOptions{
		Model: modelSpec,
		System: "You convert an assistant's finished answer into the required structured JSON. " +
			"Use ONLY information present in the answer — never invent, add, or drop data. Populate every required field.",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{
			Type: "text",
			Text: "Convert the following answer into the required structured output:\n\n" + text,
		}}}},
		ExplicitSchema: task.OutputSchema,
	}
	obj, err := GenerateObjectDirect[map[string]any](ctx, client, genOpts)
	if err != nil {
		e.logger.Warn("[%s] structured-output recovery via claw (%s) failed: %v", nodeID, modelSpec, err)
		return delegate.Result{}, false
	}
	out := primary
	out.Output = obj.Object
	out.ParseFallback = false
	if verr := ValidateOutput(out.Output, schema); verr != nil {
		e.logger.Warn("[%s] structured-output recovery via claw (%s) still invalid: %v", nodeID, modelSpec, verr)
		return delegate.Result{}, false
	}
	stampDelegateOutputMeta(out.Output, out, sourceBackend)
	e.logger.Info("[%s] structured output recovered via claw (%s) — %s produced free-form text but no schema JSON (forfait structured-output gap)",
		nodeID, modelSpec, sourceBackend)
	return out, true
}

// fallbackText returns the free-form text a parse-fallback output wraps
// (delegate parse.go stores it under "text"), or "" when the output is not a
// text fallback.
func fallbackText(output map[string]any) string {
	if output == nil {
		return ""
	}
	t, _ := output["text"].(string)
	return t
}

// schemaRetryFeedbackMarker delimits the schema-validation feedback block
// injected into a retry prompt. Kept as a const so tests can assert on it
// without duplicating the literal.
const schemaRetryFeedbackMarker = "[OUTPUT SCHEMA VALIDATION FAILED]"

// formatSchemaRetryFeedback renders the delimited feedback block appended to
// a retry prompt when structured output fails validation. err is the concrete
// validation failure (missing field, parse fallback) so the model knows what
// to fix.
func formatSchemaRetryFeedback(err error) string {
	return fmt.Sprintf(
		"%s\n%s\nReturn a corrected response that matches the required output schema exactly. Output only the JSON object.",
		schemaRetryFeedbackMarker, err,
	)
}

// appendSchemaRetryFeedback appends the feedback block to an existing user
// prompt, separated by a blank line. An empty prompt yields the feedback alone.
func appendSchemaRetryFeedback(prompt, feedback string) string {
	if prompt == "" {
		return feedback
	}
	return prompt + "\n\n" + feedback
}

// buildTask assembles the delegate.Task for an agent/judge LLM node from the
// node's resolved fields, prompts, schema, reasoning effort, capabilities,
// tool set, and session/resume continuity. Split out of executeBackend to
// keep that method focused on dispatch + validation.
func (e *ClawExecutor) buildTask(ctx context.Context, node ir.Node, f backendFields, input map[string]any, backendName string) (delegate.Task, error) {
	td := TemplateDataFromContext(ctx)

	systemText := e.resolveSystemPrompt(f.systemPrompt, input, td)
	userText, userContent := e.buildUserPromptParts(f, input, td, backendName)

	// Emit prompt content for observability.
	if e.hooks.OnLLMPrompt != nil {
		e.hooks.OnLLMPrompt(f.id, systemText, userText)
	}

	outputSchema := e.resolveOutputSchema(f.outputSchema)

	effort := resolveReasoningEffort(f.reasoningEffort, input)
	// "ultracode" is a mode (xhigh + workflow-orchestration prerogative),
	// not a wire effort value. Remap to xhigh for the provider and carry the
	// mode separately so the task can enable the orchestration prompt + tool.
	ultracode := effort == "ultracode"
	compactRatio, compactPreserve := resolveCompaction(f.compaction, e.wfCompaction)

	resolvedModel := ir.ExpandEnvWithDefault(f.model)
	// Launch-time model override wins over the node's DSL model: (studio
	// dropdown / CLI --model). Applied before the claw suggested-model
	// fallback so an override of "" is impossible (the parser rejects it).
	if ov := e.modelOverrides.ForNode(node.NodeID(), node.NodeKind()); ov.Model != "" {
		resolvedModel = ov.Model
	}
	if resolvedModel == "" && backendName == delegate.BackendClaw {
		resolvedModel = e.detectorSuggestedModel()
	}

	effectiveCaps := f.capabilities
	if effectiveCaps == nil {
		effectiveCaps = e.wfCapabilities
	}

	// Resolve node-level image inputs (templated paths) so the codex backend can
	// forward them as `-i` for image-to-image. Empty/whitespace results (an
	// optional ref that didn't apply this run) are dropped.
	var resolvedImages []string
	for _, tmpl := range f.images {
		if p := strings.TrimSpace(e.resolveTemplate(tmpl, input, td)); p != "" {
			resolvedImages = append(resolvedImages, p)
		}
	}

	task := delegate.Task{
		NodeID:                f.id,
		SourceIssueID:         e.sourceIssueID,
		Iteration:             LoopIterationFromContext(ctx),
		SystemPrompt:          systemText,
		SystemPromptMode:      delegate.SystemPromptModeForBackend(backendName),
		UserPrompt:            userText,
		UserContent:           userContent,
		AllowedTools:          f.tools,
		Readonly:              f.readonly,
		FullAccess:            f.fullAccess,
		Images:                resolvedImages,
		Capabilities:          effectiveCaps,
		StoreDir:              e.storeDir,
		OutputSchema:          outputSchema,
		Model:                 resolvedModel,
		HasTools:              len(f.tools) > 0,
		ToolMaxSteps:          f.toolMaxSteps,
		MaxTokens:             f.maxTokens,
		WorkDir:               e.workDir,
		ReasoningEffort:       wireEffort(effort),
		Ultracode:             ultracode,
		InteractionEnabled:    f.interaction != ir.InteractionNone,
		SecretsHygiene:        e.secretGuard.HasKnownSecrets(),
		SecretFiles:           e.secretFileHints(),
		MaterializeSecrets:    e.secretMaterializer(),
		CompactThresholdRatio: compactRatio,
		CompactPreserveRecent: compactPreserve,
		Sandbox:               e.sandbox,
		// ProviderHint is set per-attempt by dispatchWithProviderFallback
		// as it walks the node's provider chain.
		Hooks:      e.delegateHooksFor(f.id, backendName, LoopIterationFromContext(ctx)),
		InboxDrain: e.bindInboxDrain(ctx),
	}
	// Per-node CLI binary override (env-expanded). Only claude_code consumes
	// it; other backends ignore Task.Command. Empty = backend default.
	task.Command = ir.ExpandEnvWithDefault(f.command)
	// Command-output compression mode (precedence: run override > node DSL >
	// workflow DSL > ITERION_COMPRESS env). Stored as a string so the delegate
	// layer + IPC wire form stay decoupled from the rewrite enum; "" (off) is
	// omitted. The rewriter chain (the enabled rewriter plugins, rtk by
	// default) travels on the Task so both the in-process claude_code hook and
	// the (possibly sandboxed, IPC) claw runner can rebuild it. claude_code
	// installs a PreToolUse hook when enabled; claw carries it into its tool
	// loop via ctx.
	//
	// Compression is opt-OUT on LLM (agent/judge) nodes: when a rewriter plugin
	// is enabled and its binary is present (chain available) and nothing set it
	// explicitly, the default is On — so rtk (the default-enabled rewriter)
	// compresses agent shell output out of the box. An explicit "off" at any
	// level still wins: per-run via --compress off / studio toggle (override),
	// or globally via `iterion plugin disable rtk` (chain empty → default Off)
	// or ITERION_COMPRESS=off (envDefault). Tool nodes stay opt-IN (handled in
	// executor_tool.go via ResolveToolNode) so deterministic output is never
	// silently compressed.
	compressDefault := rewrite.Off
	if e.chain.Available() {
		compressDefault = rewrite.On
	}
	if m := rewrite.ResolveWithDefault(e.compressOverride, f.compress, e.wfCompress, e.compressEnvDefault, compressDefault); m.Enabled() {
		task.CompressMode = m.String()
		task.Rewriters = e.chain.Specs()
	}
	// Tool-permission gate (precedence: run override > node DSL > workflow
	// DSL > ITERION_PERMISSION env; off = no gate). Rule lists are additive
	// (workflow + run override). The SAME resolved policy drives both the
	// claude_code PreToolUse hook and the claw executeToolsDirect gate.
	if pol, perr := e.resolvePermissionPolicy(f.permission); perr != nil {
		return delegate.Task{}, fmt.Errorf("model: node %q: %w", f.id, perr)
	} else if pol.Enabled() {
		// On resume after a permission `ask` pause, the runtime computed
		// the operator's grant rule and passed it via GrantInputKey; add it
		// so the agent's re-issued call passes the gate.
		if grant, ok := input[permission.GrantInputKey].(string); ok && grant != "" {
			pol.AddAllowRule(grant)
		}
		task.Permission = pol
	}
	e.applyMemorySpec(&task, f.memory)
	task.CursorFragments = resolveCursorFragments(f.cursors, e.cursors)
	task.SkillHints = e.resolveSkillHints(f.skills)
	e.applyPresetFragment(&task, input, td)

	effectiveTools := e.assembleEffectiveTools(f, backendName, effectiveCaps, ultracode)
	if !sameStringSlice(effectiveTools, f.tools) {
		task.AllowedTools = effectiveTools // CLI backends read this
		task.HasTools = len(effectiveTools) > 0
	}
	e.applyBoardEndpoint(&task, effectiveCaps)

	// Mark the tools the runtime opened for its OWN interaction/capability
	// plumbing as gate-exempt (registration-linked, so a future internal
	// tool can't accidentally be gated). The reserved-namespace check is a
	// backstop. nil/disabled policy → no-op.
	if task.Permission.Enabled() {
		task.Permission.MarkExempt(askUserToolName, "send_user_message")
		task.Permission.MarkExempt(delegate.BoardToolsFor(effectiveCaps)...)
	}

	// Resolve full tool definitions for backends that manage tool loops
	// internally (claw). CLI-based backends (claude_code, codex) handle tools
	// natively via AllowedTools and do not need ToolDefs.
	if len(effectiveTools) > 0 && backendName == delegate.BackendClaw {
		clawTools := effectiveTools
		// Ambient plugin-MCP parity with claude_code (the claude_code branch
		// below forwards every active plugin MCP server to the CLI via
		// --mcp-config). claude_code resolves those out-of-process; claw
		// resolves in-process, so splice each active server's tools in here as
		// an `mcp.<server>.*` wildcard. This is what lets a claw node reach the
		// firecrawl scrape/search MCP (self-hosted, searxng-backed) instead of
		// only claw's native direct-HTTP web_fetch — the wiring gap that made
		// firecrawl claude_code-only. resolveToolsForNode starts the servers and
		// expands + dedups the wildcards; the len(effectiveTools)>0 gate keeps
		// tool-less judges lean (no ambient fetch tools, no behaviour change).
		if e.mcpManager != nil {
			for _, srv := range f.activeMCPServers {
				clawTools = append(clawTools, "mcp."+srv+".*")
			}
		}
		toolDefs, toolErr := e.resolveToolsForNode(ctx, node, clawTools)
		if toolErr != nil {
			return delegate.Task{}, fmt.Errorf("model: node %q: %w", f.id, toolErr)
		}
		task.ToolDefs = toolDefs
		task.HasTools = true // claw needs the tool loop active for ask_user
	}

	// CLI backends (claude_code) can't resolve MCP tools in-process, so the
	// node's active user/plugin MCP servers are forwarded to the agent CLI
	// verbatim (delegate.wireUserMCP → --mcp-config). Additive only: it never
	// passes --tools, so native WebSearch/WebFetch stay on by default.
	if backendName == delegate.BackendClaudeCode && len(f.activeMCPServers) > 0 && e.mcpManager != nil {
		task.MCPServers = e.resolveTaskMCPServers(f.activeMCPServers)
	}

	e.applySessionContinuity(&task, f, input)
	applyResumeContinuity(&task, input)

	return task, nil
}

// resolveSystemPrompt returns the {{vars}}-resolved body of the named
// prompt block, or "" when the name is empty or unknown. Kept as a
// helper so buildTask's top reads as a flat assembly.
func (e *ClawExecutor) resolveSystemPrompt(promptName string, input map[string]any, td *TemplateData) string {
	if promptName == "" {
		return ""
	}
	p, ok := e.prompts[promptName]
	if !ok {
		// The IR compiler validates system-prompt references, so this is
		// a can't-happen defensive branch — surface it instead of
		// silently running the node with no system guidance.
		if e.logger != nil {
			e.logger.Warn("system prompt %q referenced but not registered — IR compiler should have rejected this; node runs with no system prompt", promptName)
		}
		return ""
	}
	return e.resolveTemplate(p.Body, input, td)
}

// buildUserPromptParts assembles the user-side prompt: the rendered
// text plus, for backends that accept inline image content blocks
// (claw), the multimodal ContentBlock variant. After rendering, any
// prior ask_user question + answer recorded on input is prepended so
// the (stateless) LLM doesn't lose the thread — without this, claw
// would re-ask the same question because its conversation history isn't
// persisted.
func (e *ClawExecutor) buildUserPromptParts(f backendFields, input map[string]any, td *TemplateData, backendName string) (string, []delegate.ContentBlock) {
	userText := e.buildUserMessage(f.userPrompt, input, td)
	// And the multimodal variant when this backend supports it AND the
	// resolved prompt references at least one image attachment.
	var userContent []delegate.ContentBlock
	if backendName == delegate.BackendClaw {
		_, userContent = e.buildUserContent(f.userPrompt, input, td, e.imageAttachs)
	}

	// On re-invocation after an ask_user pause, prepend the prior
	// question and the user's answer so the (stateless) LLM doesn't
	// lose the thread. Without this, claw would re-ask the same
	// question because its conversation history isn't persisted.
	userText = prependPriorAskUser(userText, input)
	return userText, userContent
}

// resolveOutputSchema returns the JSON-marshalled output schema body
// for the named schema block, or nil when the name is empty or
// unknown. The marshal error is intentionally swallowed (same as the
// inline original) — a malformed schema is reported by validation
// earlier in the pipeline.
func (e *ClawExecutor) resolveOutputSchema(schemaName string) json.RawMessage {
	if schemaName == "" {
		return nil
	}
	schema, ok := e.schemas[schemaName]
	if !ok {
		return nil
	}
	out, _ := SchemaToJSON(schema)
	return out
}

// applyMemorySpec wires the per-node memory spec onto the task when
// the node opts in (memory.enabled = true). RepoRoot is forwarded
// alongside so the memory layer can scope the per-project key.
func (e *ClawExecutor) applyMemorySpec(task *delegate.Task, m *ir.Memory) {
	if m == nil || !m.Enabled {
		return
	}
	task.Memory = &delegate.MemorySpec{
		Scope:            m.Scope,
		Autoload:         m.Autoload,
		Read:             m.Read,
		Write:            m.Write,
		PreCompactInject: m.PreCompactInject,
		ProjectRoot:      m.ProjectRoot,
		Visibility:       m.Visibility,
		BotID:            e.botID,
	}
	task.RepoRoot = e.repoRoot
}

// applyPresetFragment wires the launch-time preset bias ("## Focus")
// onto the task: the preset's prompt body {{vars}}-resolved against
// this node's context, plus an optional relevant-skills hint line.
// Applies run-wide to every LLM node, so a "sous-bot" focus (e.g.
// Willy as improve-quality SRE) shapes the reviewer and fixer alike
// without the author wiring it into each prompt.
func (e *ClawExecutor) applyPresetFragment(task *delegate.Task, input map[string]any, td *TemplateData) {
	if e.presetPrompt == "" && len(e.presetSkills) == 0 {
		return
	}
	frag := e.resolveTemplate(e.presetPrompt, input, td)
	if len(e.presetSkills) > 0 {
		if frag != "" {
			frag += "\n\n"
		}
		frag += "Relevant skills (consult before acting): " + strings.Join(e.presetSkills, ", ")
	}
	task.PresetFragment = frag
}

// resolveSkillHints builds the "## Skills" hint list for a node: the union of
// the node's `skills:` list and the workflow-level default, filtered to those
// that resolved in the library at run start (present in e.skillHints), sorted
// by name for prompt-cache stability. An unresolved reference is silently
// dropped here — the runtime mirror already logged a warning when it couldn't
// find it. Returns nil when the node references no resolved skills.
func (e *ClawExecutor) resolveSkillHints(nodeSkills []string) []delegate.SkillHint {
	if len(e.skillHints) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, s := range nodeSkills {
		if !seen[s] {
			seen[s] = true
			names = append(names, s)
		}
	}
	for _, s := range e.wfSkills {
		if !seen[s] {
			seen[s] = true
			names = append(names, s)
		}
	}
	var hints []delegate.SkillHint
	for _, n := range names {
		if desc, ok := e.skillHints[n]; ok {
			hints = append(hints, delegate.SkillHint{Name: n, Description: desc})
		}
	}
	slices.SortFunc(hints, func(a, b delegate.SkillHint) int { return cmp.Compare(a.Name, b.Name) })
	return hints
}

// assembleEffectiveTools produces the per-node tool allowlist by
// folding interaction / board / ultracode / image-attachment opt-ins
// over the author-declared `tools:` list. The returned slice is the
// final union the CLI backends honour via AllowedTools and the claw
// backend resolves into ToolDefs.
func (e *ClawExecutor) assembleEffectiveTools(f backendFields, backendName string, effectiveCaps []string, ultracode bool) []string {
	// When interaction is enabled, ensure `ask_user` is in the node's
	// tool list so the LLM can natively escalate. We don't require the
	// workflow author to declare it in their `tools:` field — the
	// presence of `interaction:` is the opt-in.
	effectiveTools := f.tools
	if f.interaction != ir.InteractionNone {
		effectiveTools = ensureToolPresent(effectiveTools, askUserToolName)
	}
	// When board capabilities are granted and the node already restricts
	// its tool set (non-empty tools:), append the board MCP tools so
	// the CLI backend's allowlist exposes them. Empty tools: means "no
	// restriction" — the MCP server is still registered and discoverable.
	if delegate.HasBoardCapability(effectiveCaps) && len(effectiveTools) > 0 {
		effectiveTools = append(effectiveTools, delegate.BoardToolsFor(effectiveCaps)...)
	}
	// Ultracode grants standing consent to orchestrate subagents. On claw,
	// the orchestration capability is the `agent` subagent tool; ensure it is
	// in the allowlist when the node restricts its tool set (mirrors the
	// board-tools append above). An unrestricted tool set already exposes the
	// claw builtins, and the claude_code backend orchestrates via its native
	// subagent mechanism, so neither needs the explicit append.
	if ultracode && backendName == delegate.BackendClaw && len(effectiveTools) > 0 {
		effectiveTools = ensureToolPresent(effectiveTools, "agent")
	}
	// Keep the agent's task list available so the per-run Session board
	// (Tasks tab) is populated regardless of a node's `tools:` list. Only
	// the claw backend needs the append: claude_code always has its native
	// TodoWrite under bypassPermissions, and an unrestricted claw set
	// already exposes `todo_write` via RegisterClawAll. The posture nudge
	// (agenticOperatingPosture) prompts claw to actually maintain it.
	if backendName == delegate.BackendClaw && len(effectiveTools) > 0 {
		effectiveTools = ensureToolPresent(effectiveTools, "todo_write")
	}
	// CLI-based backends can't accept inline images on stdin: forward
	// the image path via {{attachments.X}} text interpolation and
	// auto-enable `read_image` so the agent can pull the bytes itself.
	if backendName != delegate.BackendClaw && len(e.imageAttachs) > 0 && promptReferencesImage(f.userPrompt, e.prompts, e.imageAttachs) {
		effectiveTools = ensureToolPresent(effectiveTools, "read_image")
	}
	return effectiveTools
}

// applyBoardEndpoint wires the per-run board MCP HTTP transport onto
// the task for sandboxed board-cap nodes (C082): the gateway listener
// started with the sandbox so claude_code can reach the operator's
// board from inside the container, plus a per-node token scoped to
// exactly this node's board caps. Non-sandboxed runs use the stdio
// __mcp-board server; CLI runs without a server leave
// boardEndpoint/boardRegister unset → board-emit disabled (documented).
func (e *ClawExecutor) applyBoardEndpoint(task *delegate.Task, effectiveCaps []string) {
	if !delegate.HasBoardCapability(effectiveCaps) || e.sandbox == nil || e.boardEndpoint == "" || e.boardRegister == nil {
		return
	}
	task.BoardHTTPEndpoint = e.boardEndpoint
	// The source ticket rides the grant so board.create over the HTTP
	// transport auto-stamps parent_id / spawned_from exactly like the
	// stdio (__mcp-board) and in-process (claw) paths do — otherwise a
	// sandboxed planner publishes orphan tickets and the parent card
	// loses its children counter.
	task.BoardRunToken = e.boardRegister(effectiveCaps, e.sourceIssueID)
}

// applySessionContinuity wires the inherit / inherit_if_available /
// fork session modes onto the task. SessionInheritIfAvailable is a
// tolerant variant of SessionInherit added for workflows where the
// upstream session may legitimately not exist yet (e.g. the first
// iteration of an alternating loop where the producer hasn't run):
// when _session_id is empty the node falls back to fresh-session
// behaviour instead of routing into the backend with an empty session
// id (which has produced silent 0-token failures on at least the
// OpenAI provider).
func (e *ClawExecutor) applySessionContinuity(task *delegate.Task, f backendFields, input map[string]any) {
	if f.session != ir.SessionInherit && f.session != ir.SessionInheritIfAvailable && f.session != ir.SessionFork {
		return
	}
	if sid, ok := input["_session_id"].(string); ok && sid != "" {
		task.SessionID = sid
		if f.session == ir.SessionFork {
			task.ForkSession = true
		}
		// Forward the provider fingerprint that produced the parent
		// session so the backend can detect cross-provider forks
		// (which fail with 400 "Invalid signature in thinking block"
		// because thinking blocks carry provider-specific
		// signatures). Empty when the parent output predates this
		// field — backends treat absent as "unknown, proceed".
		if fp, ok := input["_session_fingerprint"].(string); ok && fp != "" {
			task.SessionFingerprint = fp
		}
	} else if f.session == ir.SessionInheritIfAvailable && e.logger != nil {
		// Tolerant fallback: surface the decision so authors
		// can tell whether the cache-hit path or the cold path
		// fired. Plain `inherit` stays silent here for BC.
		e.logger.Info("[%s/inherit_if_available] no upstream _session_id; running fresh", f.id)
	}
}

// applyResumeContinuity wires the runtime-relayed persisted backend
// conversation (claw's mid-tool-loop snapshot captured at the previous
// pause) onto the task. The backend uses these fields to rehydrate
// the LLM's exact pre-pause state instead of restarting from the
// rendered system+user prompts.
func applyResumeContinuity(task *delegate.Task, input map[string]any) {
	conv, ok := input[delegate.ResumeConversationKey].(json.RawMessage)
	if !ok || len(conv) == 0 {
		return
	}
	task.ResumeConversation = conv
	if id, ok := input[delegate.ResumePendingToolUseIDKey].(string); ok {
		task.ResumePendingToolUseID = id
	}
	if a, ok := input[delegate.ResumeAnswerKey].(string); ok {
		task.ResumeAnswer = a
	}
}
