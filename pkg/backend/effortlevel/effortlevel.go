// Package effortlevel orders the reasoning-effort levels and coerces one onto
// a model's accepted set: the one rule the claw backend sends by
// (model.coerceEffortForModel) and the claude_code backend reports the claude
// CLI's sending by, so the two backends' records agree on the same model.
package effortlevel

// Rank returns a comparable ordinal for reasoning_effort levels.
// Higher rank = more compute. Unknown values get rank 0 so they sort
// below every known level — callers that pass a typo can still get a
// sensible coerced value rather than an outright drop.
func Rank(e string) int {
	switch e {
	case "none":
		return 1
	case "minimal":
		return 2
	case "low":
		return 3
	case "medium":
		return 4
	case "high":
		return 5
	case "xhigh":
		return 6
	case "max":
		return 7
	case "ultracode":
		// Not a wire value — a mode that maps to xhigh (model.wireEffort).
		// Ranked above max so any ordering comparison treats it as the top.
		return 8
	default:
		return 0
	}
}

// Coerce returns effort unchanged when it appears in supported.
// Otherwise it returns the highest supported level whose rank is <=
// effort's rank — the convention "fall back to the highest supported
// at or below" already used by the codex SDK adapter. When effort is
// below every supported level, the lowest supported level is returned
// (we always send *something* if the model accepts the parameter).
// When supported is empty the input is passed through unchanged: the
// model is unknown to apikit and the caller's value is trusted.
func Coerce(effort string, supported []string, def string) string {
	if effort == "" || len(supported) == 0 {
		return effort
	}
	for _, s := range supported {
		if s == effort {
			return effort
		}
	}
	target := Rank(effort)
	bestAtOrBelow := ""
	bestRankAtOrBelow := 0
	minSupported := ""
	minSupportedRank := 0
	for _, s := range supported {
		r := Rank(s)
		if minSupportedRank == 0 || r < minSupportedRank {
			minSupported = s
			minSupportedRank = r
		}
		if r <= target && r > bestRankAtOrBelow {
			bestAtOrBelow = s
			bestRankAtOrBelow = r
		}
	}
	if bestAtOrBelow != "" {
		return bestAtOrBelow
	}
	if minSupported != "" {
		return minSupported
	}
	return def
}
