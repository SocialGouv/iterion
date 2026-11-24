package commands

import (
	"testing"
)

type mockUsageTracker struct {
	model      string
	turns      int
	inputToks  int
	outputToks int
	costUSD    float64
}

func (m *mockUsageTracker) ModelName() string         { return m.model }
func (m *mockUsageTracker) TurnCount() int            { return m.turns }
func (m *mockUsageTracker) InputTokens() int          { return m.inputToks }
func (m *mockUsageTracker) OutputTokens() int         { return m.outputToks }
func (m *mockUsageTracker) EstimatedCostUSD() float64 { return m.costUSD }

func TestStatusCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockUsageTracker{
		model:      "claude-sonnet-4-6",
		turns:      5,
		inputToks:  1000,
		outputToks: 500,
		costUSD:    0.0123,
	}

	handled, err := r.Execute("/status", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCostCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockUsageTracker{
		inputToks:  2000,
		outputToks: 800,
		costUSD:    0.045,
	}

	handled, err := r.Execute("/cost", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUsageCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockUsageTracker{
		model:      "claude-sonnet-4-6",
		turns:      10,
		inputToks:  5000,
		outputToks: 2000,
		costUSD:    0.12,
	}

	handled, err := r.Execute("/usage", mock)
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestVersionCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	// Without version provider — should still work
	handled, err := r.Execute("/version", "not a version provider")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStatusWithoutTracker(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	// Pass non-tracker — should output fallback
	handled, err := r.Execute("/status", "not a tracker")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// mockStatsProvider implements the statsProvider interface.
type mockStatsProvider struct {
	duration   string
	turns      int
	inputToks  int
	outputToks int
	toolCalls  int
}

func (m *mockStatsProvider) SessionDuration() string { return m.duration }
func (m *mockStatsProvider) TurnCount() int          { return m.turns }
func (m *mockStatsProvider) InputTokens() int        { return m.inputToks }
func (m *mockStatsProvider) OutputTokens() int       { return m.outputToks }
func (m *mockStatsProvider) ToolCallCount() int      { return m.toolCalls }

func TestStatsCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockStatsProvider{
		duration:   "5m30s",
		turns:      10,
		inputToks:  3000,
		outputToks: 1500,
		toolCalls:  8,
	}

	handled, err := r.Execute("/stats", mock)
	if !handled || err != nil {
		t.Errorf("stats: handled=%v, err=%v", handled, err)
	}
}

func TestStatsCommandNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	handled, err := r.Execute("/stats", "not a provider")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTokensCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockUsageTracker{
		inputToks:  5000,
		outputToks: 2000,
	}

	handled, err := r.Execute("/tokens", mock)
	if !handled || err != nil {
		t.Errorf("tokens: handled=%v, err=%v", handled, err)
	}
}

func TestTokensCommandNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	handled, err := r.Execute("/tokens", "not a tracker")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// mockCacheTracker implements the cacheTracker interface.
type mockCacheTracker struct {
	hits         int
	misses       int
	cachedTokens int
}

func (m *mockCacheTracker) CacheHits() int    { return m.hits }
func (m *mockCacheTracker) CacheMisses() int  { return m.misses }
func (m *mockCacheTracker) CachedTokens() int { return m.cachedTokens }

func TestCacheCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockCacheTracker{hits: 15, misses: 5, cachedTokens: 10000}

	handled, err := r.Execute("/cache", mock)
	if !handled || err != nil {
		t.Errorf("cache: handled=%v, err=%v", handled, err)
	}
}

func TestCacheCommandNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	handled, err := r.Execute("/cache", "not a tracker")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// mockProviderLister implements the providerLister interface.
type mockProviderLister struct {
	providers []string
	active    string
}

func (m *mockProviderLister) ListProviders() []string { return m.providers }
func (m *mockProviderLister) ActiveProvider() string  { return m.active }

func TestProvidersCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockProviderLister{
		providers: []string{"anthropic", "bedrock", "vertex"},
		active:    "anthropic",
	}

	handled, err := r.Execute("/providers", mock)
	if !handled || err != nil {
		t.Errorf("providers: handled=%v, err=%v", handled, err)
	}
}

func TestProvidersCommandNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	handled, err := r.Execute("/providers", "not a lister")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// mockChangelogProvider implements the changelogProvider interface.
type mockChangelogProvider struct {
	changes []string
}

func (m *mockChangelogProvider) RecentChanges(count int) []string { return m.changes }

func TestChangelogCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockChangelogProvider{changes: []string{"v1.2: Bug fix", "v1.1: New feature"}}

	handled, err := r.Execute("/changelog", mock)
	if !handled || err != nil {
		t.Errorf("changelog: handled=%v, err=%v", handled, err)
	}
}

func TestChangelogCommandEmpty(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockChangelogProvider{changes: nil}

	handled, err := r.Execute("/changelog", mock)
	if !handled || err != nil {
		t.Errorf("changelog empty: handled=%v, err=%v", handled, err)
	}
}

// mockMetricsProvider implements the metricsProvider interface.
type mockMetricsProvider struct {
	avgTime   float64
	requests  int
	errorRate float64
}

func (m *mockMetricsProvider) AvgResponseTime() float64 { return m.avgTime }
func (m *mockMetricsProvider) TotalRequests() int       { return m.requests }
func (m *mockMetricsProvider) ErrorRate() float64       { return m.errorRate }

func TestMetricsCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockMetricsProvider{avgTime: 1.5, requests: 100, errorRate: 0.02}

	handled, err := r.Execute("/metrics", mock)
	if !handled || err != nil {
		t.Errorf("metrics: handled=%v, err=%v", handled, err)
	}
}

func TestMetricsCommandNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	handled, err := r.Execute("/metrics", "not a provider")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBenchmarksCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockMetricsProvider{avgTime: 0.5, requests: 50, errorRate: 0.01}

	handled, err := r.Execute("/benchmarks", mock)
	if !handled || err != nil {
		t.Errorf("benchmarks: handled=%v, err=%v", handled, err)
	}
}

func TestBenchmarksCommandNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	handled, err := r.Execute("/benchmarks", "not a provider")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// mockNotificationManager implements the notificationManager interface.
type mockNotificationManager struct {
	notifications []string
	cleared       bool
}

func (m *mockNotificationManager) ListNotifications() []string { return m.notifications }
func (m *mockNotificationManager) ClearNotifications() error   { m.cleared = true; return nil }

func TestNotificationsCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockNotificationManager{
		notifications: []string{"Build complete", "Test passed"},
	}

	handled, err := r.Execute("/notifications", mock)
	if !handled || err != nil {
		t.Errorf("notifications: handled=%v, err=%v", handled, err)
	}
}

func TestNotificationsCommandClear(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockNotificationManager{}

	handled, err := r.Execute("/notifications clear", mock)
	if !handled || err != nil {
		t.Errorf("notifications clear: handled=%v, err=%v", handled, err)
	}
	if !mock.cleared {
		t.Error("expected notifications to be cleared")
	}
}

func TestNotificationsCommandEmpty(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockNotificationManager{notifications: nil}

	handled, err := r.Execute("/notifications", mock)
	if !handled || err != nil {
		t.Errorf("notifications empty: handled=%v, err=%v", handled, err)
	}
}

func TestNotificationsCommandNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	handled, err := r.Execute("/notifications", "not a manager")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBillingCommand(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	mock := &mockUsageTracker{
		inputToks:  10000,
		outputToks: 5000,
		costUSD:    0.25,
	}

	handled, err := r.Execute("/billing", mock)
	if !handled || err != nil {
		t.Errorf("billing: handled=%v, err=%v", handled, err)
	}
}

func TestBillingCommandNoInterface(t *testing.T) {
	r := NewRegistry()
	RegisterStatusCommands(r)

	handled, err := r.Execute("/billing", "not a tracker")
	if !handled {
		t.Error("expected command to be handled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
