package runner

import (
	"context"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestRecordOrgSpendKey pins the usage key the runner charges spend to:
// the message's OrgID (the key the launch gate metered on) with a
// TenantID fallback for pre-orgid messages and org-less teams. Charging
// the team key when an org exists left the org's cost-cap document at
// zero forever — the multi-team cost-cap bug.
func TestRecordOrgSpendKey(t *testing.T) {
	cases := []struct {
		name    string
		orgID   string
		wantKey string
	}{
		{"org message charges the org key", "org-1", "org-1"},
		{"pre-orgid message falls back to the tenant key", "", "team-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counter := orgusage.NewMemoryCounter()
			r := &Runner{cfg: Config{OrgUsage: counter, Logger: iterlog.New(iterlog.LevelError, nil)}}
			usage := newMetricsEmitter(nil, nil)
			usage.mu.Lock()
			usage.runCostUSD = 1.5
			usage.runInputTokens = 100
			usage.runOutputTokens = 50
			usage.mu.Unlock()

			r.recordOrgSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-a", OrgID: tc.orgID}, usage)

			now := time.Now().UTC()
			got, err := counter.Usage(context.Background(), tc.wantKey, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.CostUSD != 1.5 || got.InputTokens != 100 || got.OutputTokens != 50 {
				t.Fatalf("usage on %q = %+v, want cost 1.5 / 100 / 50", tc.wantKey, got)
			}
			// Nothing lands on the other key.
			other := "org-1"
			if tc.wantKey == "org-1" {
				other = "team-a"
			}
			if u, _ := counter.Usage(context.Background(), other, now); u.CostUSD != 0 {
				t.Fatalf("spend leaked onto %q: %+v", other, u)
			}
		})
	}
}

// TestGitOpTimeoutBoundsSubprocess proves runGit is wall-clock bounded
// even when the caller's ctx has no deadline — the exact shape of a run
// launched without --timeout, where a wedged clone used to pin the
// runner pod (one in-flight run each) forever.
func TestGitOpTimeoutBoundsSubprocess(t *testing.T) {
	old := gitOpTimeout
	gitOpTimeout = 300 * time.Millisecond
	defer func() { gitOpTimeout = old }()

	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	// A fetch against RFC 5737 TEST-NET-1 blackholes (SYN never answered)
	// on typical hosts, wedging git in the TCP connect — the shape of a
	// stalled remote. On environments that instead answer/refuse fast the
	// command errors immediately and the elapsed assertion still holds:
	// the property under test is "runGit RETURNS promptly", not the error.
	dir := t.TempDir()
	if err := r.runGit(context.Background(), dir, "", "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	start := time.Now()
	_ = r.runGit(context.Background(), dir, "", "fetch", "http://192.0.2.1/repo.git")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("runGit returned after %s — the per-op timeout did not bound the subprocess", elapsed)
	}
}

// TestRecordOrgSpend_NoOpShapes pins the guard clauses: no counter wired,
// zero accumulated spend, and a tenant-less message all record nothing
// (and never panic).
func TestRecordOrgSpend_NoOpShapes(t *testing.T) {
	t.Run("nil counter", func(t *testing.T) {
		r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
		usage := newMetricsEmitter(nil, nil)
		usage.mu.Lock()
		usage.runCostUSD = 1
		usage.mu.Unlock()
		r.recordOrgSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-a"}, usage)
	})
	t.Run("zero totals record nothing", func(t *testing.T) {
		counter := orgusage.NewMemoryCounter()
		r := &Runner{cfg: Config{OrgUsage: counter, Logger: iterlog.Nop()}}
		r.recordOrgSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-a", OrgID: "org-1"}, newMetricsEmitter(nil, nil))
		if u, _ := counter.Usage(context.Background(), "org-1", time.Now().UTC()); u.CostUSD != 0 || u.InputTokens != 0 {
			t.Fatalf("zero-spend attempt recorded usage: %+v", u)
		}
	})
	t.Run("tenant-less message records nothing", func(t *testing.T) {
		counter := orgusage.NewMemoryCounter()
		r := &Runner{cfg: Config{OrgUsage: counter, Logger: iterlog.Nop()}}
		usage := newMetricsEmitter(nil, nil)
		usage.mu.Lock()
		usage.runCostUSD = 2
		usage.mu.Unlock()
		r.recordOrgSpend(&queue.RunMessage{RunID: "run-1"}, usage)
		if u, _ := counter.Usage(context.Background(), "", time.Now().UTC()); u.CostUSD != 0 {
			t.Fatalf("tenant-less spend recorded: %+v", u)
		}
	})
	t.Run("nil usage", func(t *testing.T) {
		r := &Runner{cfg: Config{OrgUsage: orgusage.NewMemoryCounter(), Logger: iterlog.Nop()}}
		r.recordOrgSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-a"}, nil)
	})
}

// TestMetricsEmitter_RunTotalsAccumulate pins the per-run accumulation the
// org-spend charge reads: claw step events add input+output tokens and a
// cost for priced models; delegate events add their aggregated count to
// the input side (no price table → cost floor untouched).
func TestMetricsEmitter_RunTotalsAccumulate(t *testing.T) {
	m := newMetricsEmitter(&recordingEmitter{}, nil) // nil registry: totals still accumulate

	_, _ = m.AppendEvent(context.Background(), "run-1", store.Event{
		Type: store.EventLLMRequest, NodeID: "n1",
		Data: map[string]any{"model": "claude-sonnet-4-6"},
	})
	_, _ = m.AppendEvent(context.Background(), "run-1", store.Event{
		Type: store.EventLLMStepFinished, NodeID: "n1",
		Data: map[string]any{"input_tokens": float64(1000), "output_tokens": float64(500)},
	})
	_, _ = m.AppendEvent(context.Background(), "run-1", store.Event{
		Type: store.EventDelegateFinished, NodeID: "n2",
		Data: map[string]any{"backend": "claude_code", "tokens": float64(420)},
	})

	cost, in, out := m.RunTotals()
	if in != 1420 {
		t.Errorf("input tokens = %d, want 1420 (claw 1000 + delegate 420)", in)
	}
	if out != 500 {
		t.Errorf("output tokens = %d, want 500", out)
	}
	if cost <= 0 {
		t.Errorf("cost = %v, want > 0 for a priced claw model", cost)
	}
}
