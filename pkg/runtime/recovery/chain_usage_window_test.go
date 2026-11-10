package recovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// An exhausted fallback chain that met a usage window on one route and a dead
// credential on another parks on the window (#3032). Measured on the
// buildkit-operator lane: the chain `zai,run-fallback,run-fallback` ran
// zai [1308] → platform subscription "OAuth session expired" → zai [1308],
// was classified AUTH_FAILED, and ten runs paused for a human for fifteen
// hours on what was a five-hour wait.

func chainUsageWindow() error {
	return &delegate.ErrRateLimited{
		Provider: "zai",
		Detail:   "429 · [1308][Usage limit reached for 5 hour. Your limit will reset at 2026-11-04 00:01:00]",
		Kind:     delegate.RateLimitKindUsageWindow,
		ResetAt:  time.Date(2026, 11, 4, 0, 1, 0, 0, time.UTC),
	}
}

func chainDeadCredential() error {
	return &delegate.ErrAuthFailed{Provider: delegate.BackendClaudeCode, Detail: "OAuth session expired and could not be refreshed"}
}

func chainThrottle() error {
	return &delegate.ErrRateLimited{Provider: "zai", Detail: "429 too many requests", Kind: delegate.RateLimitKindTransient}
}

// engineWrapped is the shape a node failure reaches the dispatcher in: the
// executor wraps the exhausted chain with the node it ran.
func engineWrapped(routes ...error) error {
	return fmt.Errorf("model: node %q: %w", "security_audit", &model.ErrChainExhausted{Chain: "zai,run-fallback,run-fallback", Errs: routes})
}

// permutations returns every ordering of routes (duplicates included: the
// orders are what the classification must not depend on).
func permutations(routes []func() error) [][]func() error {
	if len(routes) <= 1 {
		return [][]func() error{append([]func() error(nil), routes...)}
	}
	var out [][]func() error
	for i := range routes {
		rest := append(append([]func() error(nil), routes[:i]...), routes[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]func() error{routes[i]}, p...))
		}
	}
	return out
}

func TestClassify_AUsageWindowWinsAnExhaustedChainInEveryOrder(t *testing.T) {
	names := map[string]func() error{"usage_window": chainUsageWindow, "auth": chainDeadCredential, "throttle": chainThrottle}
	nameOf := func(fn func() error) string {
		for n, f := range names {
			if fmt.Sprintf("%p", f) == fmt.Sprintf("%p", fn) {
				return n
			}
		}
		return "?"
	}
	cases := []struct {
		routes []func() error
		want   runtime.ErrorCode
	}{
		// The ticket's probe table, as multisets: every order is driven.
		{[]func() error{chainUsageWindow, chainDeadCredential, chainUsageWindow}, runtime.ErrCodeUsageLimitBlocked},
		{[]func() error{chainUsageWindow, chainThrottle, chainDeadCredential}, runtime.ErrCodeUsageLimitBlocked},
		{[]func() error{chainUsageWindow, chainUsageWindow, chainUsageWindow}, runtime.ErrCodeUsageLimitBlocked},
		// A chain of only dead credentials holds no window to wait for.
		{[]func() error{chainDeadCredential, chainDeadCredential, chainDeadCredential}, runtime.ErrCodeAuthFailed},
		{[]func() error{chainDeadCredential, chainThrottle}, runtime.ErrCodeAuthFailed},
	}
	for _, tc := range cases {
		for _, order := range permutations(tc.routes) {
			routes := make([]error, len(order))
			labels := make([]string, len(order))
			for i, fn := range order {
				routes[i] = fn()
				labels[i] = nameOf(fn)
			}
			if got := Classify(engineWrapped(routes...)); got != tc.want {
				t.Errorf("chain %s → %s, want %s", strings.Join(labels, ","), got, tc.want)
			}
		}
	}
}

// The engine half of the seam: a node whose chain met a window and a dead
// credential fails terminal on USAGE_LIMIT_BLOCKED — no pause for a human, so
// the runner's usage park arms — and the node_recovery diagnostic still names
// the dead credential, route by route.
func TestUsageWindowChain_FailsOnTheWindowAndNamesTheDeadRoute(t *testing.T) {
	exec := &usageWindowExecutor{target: "synthesize", failErr: engineWrapped(chainUsageWindow(), chainDeadCredential(), chainUsageWindow())}
	s := seamStore(t)
	eng := runtime.New(seamWorkflow(), s, exec, runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))

	err := eng.Run(context.Background(), "run-chain-window", nil)
	if errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("the run paused for a human on a chain that holds a usage window: %v", err)
	}
	var rtErr *runtime.RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != runtime.ErrCodeUsageLimitBlocked {
		t.Fatalf("terminal error = %v, want a RuntimeError coded %s", err, runtime.ErrCodeUsageLimitBlocked)
	}
	r, loadErr := s.LoadRun(context.Background(), "run-chain-window")
	if loadErr != nil {
		t.Fatalf("LoadRun: %v", loadErr)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want %s — the usage park arms on a resumable failure", r.Status, store.RunStatusFailedResumable)
	}

	events, err := s.LoadEvents(context.Background(), "run-chain-window")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, ev := range events {
		if ev.Type != store.EventNodeRecovery {
			continue
		}
		if code, _ := ev.Data["code"].(string); code != string(runtime.ErrCodeUsageLimitBlocked) {
			t.Errorf("node_recovery code = %q, want %s", code, runtime.ErrCodeUsageLimitBlocked)
		}
		routes := chainRoutesOf(ev.Data["chain_routes"])
		if len(routes) != 3 {
			t.Fatalf("node_recovery chain_routes = %#v, want the three routes the chain walked", ev.Data["chain_routes"])
		}
		if code, _ := routes[1]["code"].(string); code != string(runtime.ErrCodeAuthFailed) {
			t.Errorf("route 1 code = %q, want %s — the dead credential must stay in the evidence", code, runtime.ErrCodeAuthFailed)
		}
		if msg, _ := routes[1]["error"].(string); !strings.Contains(msg, "OAuth session expired") {
			t.Errorf("route 1 error = %q, want the credential's own refusal", msg)
		}
		return
	}
	t.Fatal("no node_recovery event recorded")
}

// chainRoutesOf reads the evidence as the store hands it back: the in-memory
// shape, or the JSON round trip of the filesystem store.
func chainRoutesOf(v any) []map[string]any {
	switch routes := v.(type) {
	case []map[string]any:
		return routes
	case []any:
		out := make([]map[string]any, 0, len(routes))
		for _, r := range routes {
			m, _ := r.(map[string]any)
			out = append(out, m)
		}
		return out
	}
	return nil
}
