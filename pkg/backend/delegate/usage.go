package delegate

import "github.com/SocialGouv/iterion/pkg/backend/cost"

// AnnotateResultUsage records harness usage independently of the model's
// output. A provider cost wins over the estimate, including for unknown models.
func AnnotateResultUsage(result *Result, model string, inputTokens, outputTokens int, usd float64) {
	result.Tokens = inputTokens + outputTokens
	result.CostUSD = usd
	if usd <= 0 {
		result.CostUSD = cost.EstimateUSD(model, inputTokens, outputTokens)
	}
	if result.EffectiveModel == "" {
		result.EffectiveModel = model
	}
	if result.Output == nil {
		return
	}
	result.Output["_tokens"] = result.Tokens
	result.Output["_model"] = model
	delete(result.Output, "_cost_usd")
	if result.CostUSD > 0 {
		result.Output["_cost_usd"] = result.CostUSD
	}
	cost.SetUnreportedCalls(result.Output, result.UnreportedCalls)
}
