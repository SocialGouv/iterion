package model

import (
	"context"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// extractStructuredCorrection is the legacy extraction with its error retained:
// the shared round must not erase a typed provider refusal from this route.
func (e *ClawExecutor) extractStructuredCorrection(
	ctx context.Context,
	nodeID string,
	task *delegate.Task,
	primary delegate.Result,
	secondary delegate.Result,
	schema *ir.Schema,
	sourceBackend string,
) (out delegate.Result, spent delegate.Result, ok bool, cause error) {
	if len(task.OutputSchema) == 0 {
		return delegate.Result{}, delegate.Result{}, false, nil
	}
	text := fallbackText(primary.Output)
	if strings.TrimSpace(text) == "" {
		text = fallbackText(secondary.Output)
	}
	if strings.TrimSpace(text) == "" {
		return delegate.Result{}, delegate.Result{}, false, nil
	}
	// A gateway-served node's recovery would be a VENDOR call the
	// deployment never asked for: the detector picks a vendor default,
	// silently crossing the gateway node's spend to another provider — the
	// leak #2028 exists to close. Skip, named.
	if modelroute.Parse(task.Model).Gateway() {
		e.logger.Warn("[%s] structured-output recovery skipped: the node is served by the OpenAI-compatible gateway, and the recovery would spend a vendor default", nodeID)
		return delegate.Result{}, delegate.Result{}, false, nil
	}
	modelSpec := e.detectorSuggestedModel()
	if modelSpec == "" {
		e.logger.Warn("[%s] structured-output recovery skipped: no claw provider detected", nodeID)
		return delegate.Result{}, delegate.Result{}, false, nil
	}
	modelSpec, err := e.allowlistedDetectorPick(nodeID, modelSpec)
	if err != nil {
		// The node has already failed — the original error surfaces; the
		// recovery just does not add a spend the list forbids.
		e.logger.Warn("[%s] structured-output recovery skipped: %v", nodeID, err)
		return delegate.Result{}, delegate.Result{}, false, err
	}
	client, credential, err := e.registry.resolveBeside(ctx, modelSpec)
	if err != nil {
		e.logger.Warn("[%s] structured-output recovery: resolve %q: %v", nodeID, modelSpec, err)
		return delegate.Result{}, delegate.Result{}, false, err
	}
	genOpts := GenerationOptions{
		Model:      modelSpec,
		Credential: credential,
		System: "You convert an assistant's finished answer into the required structured JSON. " +
			"Use ONLY information present in the answer — never invent, add, or drop data. Populate every required field.",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{
			Type: "text",
			Text: "Convert the following answer into the required structured output:\n\n" + text,
		}}}},
		ExplicitSchema: task.OutputSchema,
	}
	// The recovery is a claw call on the model this function picked, not on
	// the node's own — pricing its usage is the task's only role here.
	recoveryTask := delegate.Task{Model: modelSpec}
	bill := func(u Usage) delegate.Result {
		r := meteredFailure(ctx, recoveryTask, u)
		r.Credential = credential
		return r
	}
	// This observer wraps only the APIClient seam used by direct generation;
	// optional client capabilities are not consulted on this path. Both internal
	// turns keep this same resolved client and model.
	observed := &extractionClient{inner: client}
	genOpts.OnResponse = observed.observeResponse
	var usage Usage
	defer func() {
		if !observed.finish() {
			return
		}
		info := newStructuredExtraction(ctx, modelSpec, usage, ok)
		info.Credential = credential
		// One final rate reading feeds all three accounting channels. The result
		// stays legacy-shaped; only this local observer certifies a zero tariff.
		observeStructuredExtractionUsage(ctx, info)
		spent.CostUSD = info.CostUSD
		if spent.Output != nil {
			delete(spent.Output, "_cost_usd")
			if info.CostUSD > 0 {
				spent.Output["_cost_usd"] = info.CostUSD
			}
		}
		if e.hooks.OnStructuredExtraction != nil {
			e.hooks.OnStructuredExtraction(nodeID, info)
		}
	}()
	obj, err := GenerateObjectDirect[map[string]any](ctx, observed, e.generationForModel(genOpts))
	usage = observed.settleUsage(objectUsage(obj))
	if err != nil {
		e.logger.Warn("[%s] structured-output recovery via claw (%s) failed: %v", nodeID, modelSpec, err)
		return delegate.Result{}, bill(usage), false, err
	}
	out = primary
	out.Output = obj.Object
	out.ParseFallback = false
	if verr := ValidateOutput(out.Output, schema); verr != nil {
		e.logger.Warn("[%s] structured-output recovery via claw (%s) still invalid: %v", nodeID, modelSpec, verr)
		return delegate.Result{}, bill(usage), false, verr
	}
	if out.Output == nil {
		out.Output = map[string]any{}
	}
	// Replacing the answer does not replace its bill: the primary's typed
	// counters survive the map swap. The stamp restores tokens AND cost;
	// CostIsSessionTotal still describes that same reading for the fold.
	stampDelegateOutputMeta(out.Output, out, sourceBackend)
	e.logger.Info("[%s] structured output recovered via claw (%s) — %s produced free-form text but no schema JSON (forfait structured-output gap)",
		nodeID, modelSpec, sourceBackend)
	return out, bill(usage), true, nil
}
