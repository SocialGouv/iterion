package model

import (
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestNewClawExecutorReadsRouteCooldownKillSwitch(t *testing.T) {
	for _, tt := range []struct {
		mode     string
		disabled bool
	}{
		{mode: "", disabled: false},
		{mode: "on", disabled: false},
		{mode: "off", disabled: true},
		{mode: " OFF ", disabled: true},
		{mode: "invalid", disabled: false},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			t.Setenv(routeCooldownModeEnv, tt.mode)
			e := NewClawExecutor(NewRegistry(), &ir.Workflow{})
			if e.routeCooldowns.disabled != tt.disabled {
				t.Errorf("disabled = %v, want %v for %s=%q", e.routeCooldowns.disabled, tt.disabled, routeCooldownModeEnv, tt.mode)
			}
		})
	}
}

func TestRouteCooldownLedgerDisabledIsFailOpen(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	key := routeCooldownKey{Backend: delegate.BackendClaudeCode}
	l := routeCooldownLedger{disabled: true}
	l.record(key, routeCooldown{Category: delegate.FallbackUsageWindow, Until: now.Add(time.Hour)}, now)
	if _, ok := l.active(key, now); ok {
		t.Fatal("disabled ledger returned a cooldown")
	}
	if len(l.entries) != 0 {
		t.Fatalf("disabled ledger recorded entries: %v", l.entries)
	}
}

// The ledger's later-reset-wins branch decides which of two observations of
// the same route survives. Parallel branches reach it concurrently, so it is
// asserted here directly rather than only through dispatch: a dispatch test
// cannot arrange two different windows on one key without a race.
func TestRouteCooldownLedgerKeepsTheLaterReset(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	key := routeCooldownKey{Backend: delegate.BackendClaudeCode, Model: "claude-opus-5"}
	early := now.Add(time.Hour)
	late := now.Add(4 * time.Hour)

	for _, tt := range []struct {
		name  string
		order []time.Time
	}{
		{name: "ascending", order: []time.Time{early, late}},
		{name: "descending", order: []time.Time{late, early}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var l routeCooldownLedger
			for _, until := range tt.order {
				l.record(key, routeCooldown{Category: delegate.FallbackUsageWindow, Until: until}, now)
			}
			cd, ok := l.active(key, now)
			if !ok {
				t.Fatal("entry absent after recording")
			}
			if !cd.Until.Equal(late) {
				t.Errorf("Until = %s, want the later reset %s", cd.Until, late)
			}
		})
	}
}

// Two branches of one fan-out can refuse the same route in the same instant.
// The merge must stay data-race free and still settle on the later window.
func TestRouteCooldownLedgerIsConcurrencySafe(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	key := routeCooldownKey{Backend: delegate.BackendClaudeCode, Model: "claude-opus-5"}
	latest := now.Add(time.Duration(16) * time.Hour)

	var l routeCooldownLedger
	var wg sync.WaitGroup
	for n := 1; n <= 16; n++ {
		wg.Add(1)
		go func(hours int) {
			defer wg.Done()
			l.record(key, routeCooldown{
				Category: delegate.FallbackUsageWindow,
				Until:    now.Add(time.Duration(hours) * time.Hour),
			}, now)
			l.active(key, now)
		}(n)
	}
	wg.Wait()

	cd, ok := l.active(key, now)
	if !ok {
		t.Fatal("entry absent after concurrent records")
	}
	if !cd.Until.Equal(latest) {
		t.Errorf("Until = %s, want the latest observed window %s", cd.Until, latest)
	}
}

// An entry is not swept in the background: reading it at or after its own
// reset instant is what drops it, so the route goes hot again on the next
// dispatch and no goroutine outlives the run.
func TestRouteCooldownLedgerExpiresOnRead(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	key := routeCooldownKey{Backend: delegate.BackendClaudeCode}
	reset := now.Add(time.Hour)

	var l routeCooldownLedger
	l.record(key, routeCooldown{Category: delegate.FallbackUsageWindow, Until: reset}, now)
	if _, ok := l.active(key, now); !ok {
		t.Fatal("entry absent before its reset")
	}
	if _, ok := l.active(key, reset); ok {
		t.Error("entry still active at its own reset instant")
	}
	if len(l.entries) != 0 {
		t.Errorf("expired entry retained: %v", l.entries)
	}
}
