package model

import (
	"github.com/SocialGouv/iterion/pkg/backend/effortlevel"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/apikit"
)

// effortRank returns a comparable ordinal for reasoning_effort levels
// (effortlevel.Rank).
func effortRank(e string) int { return effortlevel.Rank(e) }

// wireEffort maps an effort level to the value sent on the provider wire.
// "ultracode" is Claude Code's mode (xhigh + workflow-orchestration
// prerogative), not an API effort value — Anthropic only accepts up to
// xhigh/max — so it collapses to "xhigh". Every other level passes through.
func wireEffort(effort string) string {
	if effort == "ultracode" {
		return "xhigh"
	}
	return effort
}

// coerceEffort maps effort onto supported (effortlevel.Coerce).
func coerceEffort(effort string, supported []string, def string) string {
	return effortlevel.Coerce(effort, supported, def)
}

// coerceEffortForModel resolves the model's effort matrix via
// apikit.EffortCapabilities and coerces effort to a level the model
// accepts. Models claw-code-go does not recognise return the input
// unchanged; the provider call will surface a 400 with enough context
// to add the model to the registry.
func coerceEffortForModel(effort, modelID string) string {
	if effort == "" {
		return ""
	}
	// Collapse the "ultracode" mode to its wire effort (xhigh) before
	// coercing — otherwise the unknown token would rank below every
	// supported level and wrongly drop to the model's lowest effort.
	effort = wireEffort(effort)
	supported, def := apikit.EffortCapabilities(modelID)
	return coerceEffort(effort, supported, def)
}
