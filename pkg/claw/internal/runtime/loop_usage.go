package runtime

import (
	"fmt"
	"time"
)

// --- UsageTracker interface (status_cmds.go) ---

// ModelName returns the active model name.
func (a *LoopAdapter) ModelName() string {
	if a.loop != nil && a.loop.Config != nil {
		return a.loop.Config.Model
	}
	return "unknown"
}

// TurnCount returns the number of completed turns.
func (a *LoopAdapter) TurnCount() int {
	if a.hasUsage {
		return a.loop.Usage.Turns
	}
	if a.loop != nil && a.loop.Session != nil {
		return a.loop.Session.TotalTurns
	}
	return 0
}

// InputTokens returns total input tokens.
func (a *LoopAdapter) InputTokens() int {
	if a.hasUsage {
		return a.loop.Usage.TotalInput
	}
	if a.loop != nil && a.loop.Session != nil {
		return a.loop.Session.TotalInputTokens
	}
	return 0
}

// OutputTokens returns total output tokens.
func (a *LoopAdapter) OutputTokens() int {
	if a.hasUsage {
		return a.loop.Usage.TotalOutput
	}
	if a.loop != nil && a.loop.Session != nil {
		return a.loop.Session.TotalOutputTokens
	}
	return 0
}

// EstimatedCostUSD returns the estimated cost for the session.
func (a *LoopAdapter) EstimatedCostUSD() float64 {
	if a.hasUsage {
		cost := a.loop.Usage.CostEstimate()
		if cost >= 0 {
			return cost
		}
	}
	return 0.0
}

// --- statsProvider interface (status_cmds.go) ---

// SessionDuration returns a human-readable duration since session start.
func (a *LoopAdapter) SessionDuration() string {
	d := time.Since(a.startTime)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// ToolCallCount returns the number of tool calls in this session.
// Reads from the ConversationLoop's atomic counter when available.
func (a *LoopAdapter) ToolCallCount() int {
	if a.loop != nil {
		return a.loop.ToolCallCount()
	}
	return 0
}

// --- cacheTracker interface (status_cmds.go) ---

// CacheHits returns prompt cache hit count.
func (a *LoopAdapter) CacheHits() int {
	if a.hasUsage {
		return a.loop.Usage.CacheRead
	}
	return 0
}

// CacheMisses returns prompt cache miss count.
func (a *LoopAdapter) CacheMisses() int {
	if a.hasUsage {
		return a.loop.Usage.CacheWrite
	}
	return 0
}

// CachedTokens returns total cached tokens.
func (a *LoopAdapter) CachedTokens() int {
	if a.hasUsage {
		return a.loop.Usage.CacheRead + a.loop.Usage.CacheWrite
	}
	return 0
}

// --- providerLister interface (status_cmds.go) ---

// ListProviders returns the names of available providers.
func (a *LoopAdapter) ListProviders() []string {
	return []string{"anthropic", "bedrock", "vertex", "foundry"}
}

// ActiveProvider returns the active provider name.
func (a *LoopAdapter) ActiveProvider() string {
	if a.loop != nil && a.loop.Config != nil && a.loop.Config.ProviderName != "" {
		return a.loop.Config.ProviderName
	}
	return "anthropic"
}

// --- metricsProvider interface (status_cmds.go) ---

// AvgResponseTime returns average response time in seconds.
func (a *LoopAdapter) AvgResponseTime() float64 {
	if a.TurnCount() == 0 {
		return 0
	}
	elapsed := time.Since(a.startTime).Seconds()
	return elapsed / float64(a.TurnCount())
}

// TotalRequests returns the total number of API requests made.
func (a *LoopAdapter) TotalRequests() int {
	return a.TurnCount()
}

// ErrorRate returns the fraction of requests that resulted in errors.
func (a *LoopAdapter) ErrorRate() float64 {
	// Currently not tracked at this level; return 0.
	return 0.0
}

// --- versionProvider interface (status_cmds.go) ---

// Version returns the CLI version string.
// When build-time info has been wired via SetBuildInfo(), it returns the real
// version; otherwise falls back to "dev".
func (a *LoopAdapter) Version() string {
	if a.buildVersion != "" {
		return a.buildVersion
	}
	return "dev"
}

// Commit returns the git commit hash.
// When build-time info has been wired via SetBuildInfo(), it returns the real
// commit; otherwise falls back to "unknown".
func (a *LoopAdapter) Commit() string {
	if a.buildCommit != "" {
		return a.buildCommit
	}
	return "unknown"
}

// --- changelogProvider interface (status_cmds.go) ---

// RecentChanges returns recent changes (placeholder).
func (a *LoopAdapter) RecentChanges(count int) []string {
	return nil
}

// --- notificationManager interface (status_cmds.go) ---

// ListNotifications returns pending notifications.
func (a *LoopAdapter) ListNotifications() []string {
	return a.notifications
}

// ClearNotifications clears all notifications.
func (a *LoopAdapter) ClearNotifications() error {
	a.notifications = nil
	return nil
}
