package model

import (
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

func capPolicy() usagecap.Policy {
	return usagecap.Policy{
		FiveHour: usagecap.WindowPolicy{MaxPercent: 85, Mode: usagecap.ModeSoft},
		Week:     usagecap.WindowPolicy{MaxPercent: 75, Mode: usagecap.ModeHard},
	}
}

func usageHook(t *testing.T, guard *usagecap.Guard, hooks EventHooks) func(usagecap.Reading) error {
	t.Helper()
	opts := []ClawExecutorOption{WithUsageGuard(guard)}
	opts = append(opts, WithEventHooks(hooks))
	e := NewClawExecutor(NewRegistry(), &ir.Workflow{}, opts...)
	h := e.delegateHooksFor("implement", delegate.BackendClaudeCode, 0)
	if h.OnUsageWindow == nil {
		t.Fatal("OnUsageWindow not wired — the backend would observe the provider and tell nobody")
	}
	return h.OnUsageWindow
}

// The whole point of the hard posture: iterion raises the SAME error the
// provider raises at its own wall, so the run parks and a durable retry
// brings it back — no new recovery path to get right.
func TestUsageGuardBridge_HardCapRaisesAUsageWindowError(t *testing.T) {
	resets := time.Date(2026, 8, 18, 21, 0, 0, 0, time.UTC)
	hook := usageHook(t, usagecap.NewGuard(capPolicy(), nil), EventHooks{})

	err := hook(usagecap.Reading{
		Window:      usagecap.WindowSevenDay,
		Utilization: 0.76,
		Status:      usagecap.StatusWarning,
		ResetsAt:    resets,
	})
	if err == nil {
		t.Fatal("want an error stopping the session at 76% against a 75% hard cap")
	}
	var rl *delegate.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("got %T (%v), want *delegate.ErrRateLimited", err, err)
	}
	// Kind is what routes the run to the park-and-resume path instead of
	// the DLQ; ResetAt is when it comes back.
	if rl.Kind != delegate.RateLimitKindUsageWindow {
		t.Errorf("Kind = %q, want %q", rl.Kind, delegate.RateLimitKindUsageWindow)
	}
	if !rl.ResetAt.Equal(resets) {
		t.Errorf("ResetAt = %v, want %v", rl.ResetAt, resets)
	}
}

// Soft is the promise that a half-finished run is never sacrificed to save
// minutes of a window that refills soon.
func TestUsageGuardBridge_SoftCapLetsTheRunFinish(t *testing.T) {
	hook := usageHook(t, usagecap.NewGuard(capPolicy(), nil), EventHooks{})
	if err := hook(usagecap.Reading{
		Window:      usagecap.WindowFiveHour,
		Utilization: 0.99,
		Status:      usagecap.StatusWarning,
	}); err != nil {
		t.Fatalf("soft cap returned %v — it must not stop work in flight", err)
	}
}

func TestUsageGuardBridge_UnderTheCapIsSilent(t *testing.T) {
	var fired []UsageCapInfo
	hook := usageHook(t, usagecap.NewGuard(capPolicy(), nil), EventHooks{
		OnUsageCap: func(_ string, info UsageCapInfo) { fired = append(fired, info) },
	})
	if err := hook(usagecap.Reading{Window: usagecap.WindowSevenDay, Utilization: 0.40}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fired) != 0 {
		t.Fatalf("emitted %d cap events below the cap, want none", len(fired))
	}
}

// The timeline must be able to tell "the provider refused us" from "we
// stopped ourselves" — both end a run the same way otherwise.
func TestUsageGuardBridge_EmitsTheCrossing(t *testing.T) {
	var fired []UsageCapInfo
	hooks := EventHooks{OnUsageCap: func(_ string, info UsageCapInfo) { fired = append(fired, info) }}
	hook := usageHook(t, usagecap.NewGuard(capPolicy(), nil), hooks)

	_ = hook(usagecap.Reading{Window: usagecap.WindowFiveHour, Utilization: 0.90})
	_ = hook(usagecap.Reading{Window: usagecap.WindowSevenDay, Utilization: 0.90})

	if len(fired) != 2 {
		t.Fatalf("got %d cap events, want 2", len(fired))
	}
	if fired[0].Stopped || fired[0].Mode != string(usagecap.ModeSoft) {
		t.Errorf("5h crossing = %+v, want a soft, non-stopping event", fired[0])
	}
	if !fired[1].Stopped || fired[1].Mode != string(usagecap.ModeHard) {
		t.Errorf("week crossing = %+v, want a hard, stopping event", fired[1])
	}
	if fired[1].Percent != 90 || fired[1].Cap != 75 {
		t.Errorf("percent/cap = %v/%v, want 90/75 on the operator-facing scale", fired[1].Percent, fired[1].Cap)
	}
}

func TestUsageGuardBridge_NoGuardNoHook(t *testing.T) {
	e := NewClawExecutor(NewRegistry(), &ir.Workflow{})
	if h := e.delegateHooksFor("implement", delegate.BackendClaudeCode, 0); h.OnUsageWindow != nil {
		t.Fatal("a run with no cap must not carry a usage hook at all")
	}
}

// Retrying a cap in place would spend exactly the quota it protects — and
// the provider, still under its own wall, would serve the call. A refusal
// that came FROM the provider keeps its historical in-place budget (the
// fallback chain of ADR-087 depends on it).
func TestSelfImposedRefusalIsNotLocallyRetried(t *testing.T) {
	capped := &delegate.ErrRateLimited{
		Kind:        delegate.RateLimitKindUsageWindow,
		SelfImposed: true,
		Detail:      "usage cap: seven_day window at 76% ≥ 75%",
	}
	if isDelegateRetryable(capped) {
		t.Error("iterion's own cap must not be retried in place")
	}
	providerWall := &delegate.ErrRateLimited{Kind: delegate.RateLimitKindUsageWindow, Detail: "weekly limit"}
	if !isDelegateRetryable(providerWall) {
		t.Error("a provider refusal keeps its existing in-place budget")
	}
	if !isDelegateRetryable(&delegate.ErrRateLimited{Kind: delegate.RateLimitKindTransient}) {
		t.Error("a plain throttle is still worth retrying soon")
	}
}

// The bridge must stamp it, or the run burns its retry budget against the
// cap before parking.
func TestUsageGuardBridge_MarksTheErrorSelfImposed(t *testing.T) {
	hook := usageHook(t, usagecap.NewGuard(capPolicy(), nil), EventHooks{})
	err := hook(usagecap.Reading{Window: usagecap.WindowSevenDay, Utilization: 0.99})
	var rl *delegate.ErrRateLimited
	if !errors.As(err, &rl) || !rl.SelfImposed {
		t.Fatalf("err = %v (self-imposed=%v), want a self-imposed refusal", err, rl != nil && rl.SelfImposed)
	}
	if isDelegateRetryable(err) {
		t.Error("the cap error must not be locally retryable")
	}
}
