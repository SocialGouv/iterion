package commands

import (
	"fmt"
	"strings"
)

// UsageTracker provides session usage information for status commands.
type UsageTracker interface {
	ModelName() string
	TurnCount() int
	InputTokens() int
	OutputTokens() int
	EstimatedCostUSD() float64
}

// RegisterStatusCommands registers status-related slash commands.
func RegisterStatusCommands(r *Registry) {
	r.Register(Command{
		Name:            "status",
		Description:     "Show current session status",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			ut, ok := loop.(UsageTracker)
			if !ok {
				fmt.Println("Status not available in this context.")
				return nil
			}
			fmt.Printf("Model:         %s\n", ut.ModelName())
			fmt.Printf("Turns:         %d\n", ut.TurnCount())
			fmt.Printf("Input tokens:  %d\n", ut.InputTokens())
			fmt.Printf("Output tokens: %d\n", ut.OutputTokens())
			return nil
		},
	})

	r.Register(Command{
		Name:            "cost",
		Description:     "Show cumulative token usage for this session",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			ut, ok := loop.(UsageTracker)
			if !ok {
				fmt.Println("Cost tracking not available in this context.")
				return nil
			}
			fmt.Printf("Input tokens:  %d\n", ut.InputTokens())
			fmt.Printf("Output tokens: %d\n", ut.OutputTokens())
			fmt.Printf("Estimated cost: $%.4f\n", ut.EstimatedCostUSD())
			return nil
		},
	})

	r.Register(Command{
		Name:            "usage",
		Description:     "Show detailed API usage statistics",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			ut, ok := loop.(UsageTracker)
			if !ok {
				fmt.Println("Usage tracking not available in this context.")
				return nil
			}
			fmt.Printf("Session Usage Summary\n")
			fmt.Printf("  Model:         %s\n", ut.ModelName())
			fmt.Printf("  Turns:         %d\n", ut.TurnCount())
			fmt.Printf("  Input tokens:  %d\n", ut.InputTokens())
			fmt.Printf("  Output tokens: %d\n", ut.OutputTokens())
			fmt.Printf("  Estimated cost: $%.4f\n", ut.EstimatedCostUSD())
			return nil
		},
	})

	r.Register(Command{
		Name:            "version",
		Description:     "Show CLI version and build information",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			type versionProvider interface {
				Version() string
				Commit() string
			}
			if vp, ok := loop.(versionProvider); ok {
				fmt.Printf("Version: %s\n", vp.Version())
				fmt.Printf("Commit:  %s\n", vp.Commit())
			} else {
				fmt.Println("claw-code-go (version unknown)")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "stats",
		Description:     "Show session statistics",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			type statsProvider interface {
				SessionDuration() string
				TurnCount() int
				InputTokens() int
				OutputTokens() int
				ToolCallCount() int
			}
			sp, ok := loop.(statsProvider)
			if !ok {
				fmt.Println("Statistics not available in this context.")
				return nil
			}
			fmt.Printf("Session Statistics:\n")
			fmt.Printf("  Duration:      %s\n", sp.SessionDuration())
			fmt.Printf("  Turns:         %d\n", sp.TurnCount())
			fmt.Printf("  Input tokens:  %d\n", sp.InputTokens())
			fmt.Printf("  Output tokens: %d\n", sp.OutputTokens())
			fmt.Printf("  Tool calls:    %d\n", sp.ToolCallCount())
			return nil
		},
	})

	r.Register(Command{
		Name:            "tokens",
		Description:     "Show token count for current conversation",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			ut, ok := loop.(UsageTracker)
			if !ok {
				fmt.Println("Token tracking not available in this context.")
				return nil
			}
			total := ut.InputTokens() + ut.OutputTokens()
			fmt.Printf("Token Usage:\n")
			fmt.Printf("  Input:  %d\n", ut.InputTokens())
			fmt.Printf("  Output: %d\n", ut.OutputTokens())
			fmt.Printf("  Total:  %d\n", total)
			return nil
		},
	})

	r.Register(Command{
		Name:            "cache",
		Description:     "Show prompt cache statistics",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			type cacheTracker interface {
				CacheHits() int
				CacheMisses() int
				CachedTokens() int
			}
			ct, ok := loop.(cacheTracker)
			if !ok {
				fmt.Println("Cache tracking not available in this context.")
				return nil
			}
			total := ct.CacheHits() + ct.CacheMisses()
			hitRate := 0.0
			if total > 0 {
				hitRate = float64(ct.CacheHits()) / float64(total) * 100
			}
			fmt.Printf("Cache Statistics:\n")
			fmt.Printf("  Hits:         %d\n", ct.CacheHits())
			fmt.Printf("  Misses:       %d\n", ct.CacheMisses())
			fmt.Printf("  Hit rate:     %.1f%%\n", hitRate)
			fmt.Printf("  Cached tokens: %d\n", ct.CachedTokens())
			return nil
		},
	})

	r.Register(Command{
		Name:            "providers",
		Description:     "List available providers",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			type providerLister interface {
				ListProviders() []string
				ActiveProvider() string
			}
			pl, ok := loop.(providerLister)
			if !ok {
				fmt.Println("Provider listing not available in this context.")
				return nil
			}
			fmt.Printf("Active provider: %s\n", pl.ActiveProvider())
			providers := pl.ListProviders()
			if len(providers) > 0 {
				fmt.Printf("Available: %s\n", strings.Join(providers, ", "))
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "changelog",
		Description:     "Show recent changes",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			type changelogProvider interface {
				RecentChanges(count int) []string
			}
			cp, ok := loop.(changelogProvider)
			if !ok {
				fmt.Println("Changelog not available in this context.")
				return nil
			}
			changes := cp.RecentChanges(10)
			if len(changes) == 0 {
				fmt.Println("No recent changes.")
				return nil
			}
			fmt.Println("Recent Changes:")
			for _, c := range changes {
				fmt.Printf("  - %s\n", c)
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "metrics",
		Description:     "Show performance metrics",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			type metricsProvider interface {
				AvgResponseTime() float64
				TotalRequests() int
				ErrorRate() float64
			}
			mp, ok := loop.(metricsProvider)
			if !ok {
				fmt.Println("Metrics not available in this context.")
				return nil
			}
			fmt.Printf("Performance Metrics:\n")
			fmt.Printf("  Avg response time: %.2fs\n", mp.AvgResponseTime())
			fmt.Printf("  Total requests:    %d\n", mp.TotalRequests())
			fmt.Printf("  Error rate:        %.1f%%\n", mp.ErrorRate()*100)
			return nil
		},
	})

	r.Register(Command{
		Name:            "benchmarks",
		Description:     "Show benchmark results",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			type metricsProvider interface {
				AvgResponseTime() float64
				TotalRequests() int
				ErrorRate() float64
			}
			mp, ok := loop.(metricsProvider)
			if !ok {
				fmt.Println("Benchmark data not available in this context.")
				return nil
			}
			fmt.Printf("Benchmark Results:\n")
			fmt.Printf("  Avg response time: %.2fs\n", mp.AvgResponseTime())
			fmt.Printf("  Total requests:    %d\n", mp.TotalRequests())
			fmt.Printf("  Error rate:        %.1f%%\n", mp.ErrorRate()*100)
			fmt.Printf("  Throughput:        %.1f req/s\n", func() float64 {
				if mp.AvgResponseTime() > 0 {
					return 1.0 / mp.AvgResponseTime()
				}
				return 0
			}())
			return nil
		},
	})

	r.Register(Command{
		Name:            "notifications",
		Description:     "Show or manage notifications",
		ArgumentHint:    "[clear]",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			type notificationManager interface {
				ListNotifications() []string
				ClearNotifications() error
			}
			nm, ok := loop.(notificationManager)
			if !ok {
				fmt.Println("Notifications not available in this context.")
				return nil
			}
			if strings.TrimSpace(args) == "clear" {
				if err := nm.ClearNotifications(); err != nil {
					return err
				}
				fmt.Println("Notifications cleared.")
				return nil
			}
			notifs := nm.ListNotifications()
			if len(notifs) == 0 {
				fmt.Println("No notifications.")
				return nil
			}
			fmt.Println("Notifications:")
			for _, n := range notifs {
				fmt.Printf("  - %s\n", n)
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "billing",
		Description:     "Show billing and cost projections",
		ResumeSupported: true,
		Category:        CategoryStatus,
		Handler: func(args string, loop interface{}) error {
			ut, ok := loop.(UsageTracker)
			if !ok {
				fmt.Println("Billing information not available in this context.")
				return nil
			}
			cost := ut.EstimatedCostUSD()
			fmt.Printf("Billing Summary:\n")
			fmt.Printf("  Current session cost: $%.4f\n", cost)
			fmt.Printf("  Input tokens:         %d\n", ut.InputTokens())
			fmt.Printf("  Output tokens:        %d\n", ut.OutputTokens())
			return nil
		},
	})
}
