package model

import (
	"errors"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// routeCooldownKey identifies the effective call rather than the authored
// label. Provider is the credential-routing hint; backend and model keep two
// different chain elements that happen to use the same hint independent.
type routeCooldownKey struct {
	Backend  string
	Provider string
	Model    string
}

type routeCooldown struct {
	Category delegate.FallbackCategory
	Until    time.Time
	// Cause preserves the typed provider condition that armed the entry.
	// A later dispatch may skip the call, but if its fallback also fails the
	// chain's terminal error must still expose this cause to the durable
	// usage-window retry and credential-pool health classifiers.
	Cause error
}

// maxRouteCooldown bounds how far ahead a remembered reset may sit before
// the ledger stops believing it. The longest real provider window is weekly,
// so eight days covers it with room for timezone skew in the reset notice —
// the same reasoning (and the same figure) as retrypolicy.DefaultMaxWait,
// which caps the durable run-level wait built from this very instant.
//
// The ledger needs its own guard because it reads ResetAt from an
// UNVALIDATED source: delegate.parseResetHint takes an absolute provider
// datetime verbatim, "with no plausibility window", so one garbled notice
// ("resets 9999-01-01 00:00" from a facade) would otherwise keep the primary
// route dark for the whole run — every later node degrading to its fallback
// and stamping `_fallback_used`, which fails a deterministic gate closed.
// Where the runner CLAMPS an over-long wait, the ledger REFUSES the entry:
// a clamp still hides the route for longer than any run lasts, whereas
// declining to record costs one refused spawn and keeps dispatch fail-open.
const maxRouteCooldown = 8 * 24 * time.Hour

// routeCooldownLedger is deliberately process-memory only. A missed entry
// costs one refused spawn, while persisting an uncertain entry could suppress
// a healthy credential across runs. Entries expire when read; no sweeper is
// needed.
type routeCooldownLedger struct {
	mu      sync.Mutex
	entries map[routeCooldownKey]routeCooldown
}

func (l *routeCooldownLedger) active(key routeCooldownKey, now time.Time) (routeCooldown, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cd, ok := l.entries[key]
	if !ok {
		return routeCooldown{}, false
	}
	if !now.Before(cd.Until) {
		delete(l.entries, key)
		return routeCooldown{}, false
	}
	return cd, true
}

func (l *routeCooldownLedger) record(key routeCooldownKey, cd routeCooldown, now time.Time) {
	if cd.Until.IsZero() || !now.Before(cd.Until) {
		return
	}
	if cd.Until.Sub(now) > maxRouteCooldown {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[routeCooldownKey]routeCooldown)
	}
	// Concurrent branches can observe different windows for the same route.
	// The later reset is the safe one: retrying before every observed window
	// has reopened only buys another refusal.
	if prev, ok := l.entries[key]; ok && !cd.Until.After(prev.Until) {
		return
	}
	l.entries[key] = cd
}

func (e *ClawExecutor) cooldownNow() time.Time {
	if e.now != nil {
		return e.now().UTC()
	}
	return time.Now().UTC()
}

func cooldownForFailure(err error, cat delegate.FallbackCategory) routeCooldown {
	switch cat {
	case delegate.FallbackUsageWindow:
		var rl *delegate.ErrRateLimited
		if errors.As(err, &rl) {
			return routeCooldown{Category: cat, Until: rl.ResetAt, Cause: err}
		}
	case delegate.FallbackUnavailable:
		var unavailable *delegate.ErrModelUnavailable
		if errors.As(err, &unavailable) {
			return routeCooldown{Category: cat, Until: unavailable.ResetAt, Cause: err}
		}
	}
	return routeCooldown{}
}

func cooldownKey(backendName string, task *delegate.Task) routeCooldownKey {
	return routeCooldownKey{
		Backend:  backendName,
		Provider: task.ProviderHint,
		Model:    task.Model,
	}
}
