// Package schedgate is the shared "should this scheduled bot fire now?"
// gate used by all three scheduled-launch paths: pkg/cli/schedule (host
// crontab), pkg/trigger.Scheduler (in-process spine), and pkg/cloudsched
// (multi-replica cloud ticker). It provides three composable pieces — an
// overlap decision over live runs, a guard-command executor, and a
// tick-record shape — with no I/O of its own beyond running the guard
// subprocess. Each surface keeps its own persistence (JSONL locally,
// pkg/audit in cloud mode).
package schedgate

import (
	"fmt"
	"strings"
	"time"
)

// Overlap policy values. An empty Overlap normalizes to OverlapSkip:
// firing while a previous run of the same schedule is still live is a
// latent-bug default (a nightly that overruns its window piles up
// concurrent runs on the same repo), so skip is the safe baseline and
// allow is the explicit opt-in.
//
// OverlapKeepalive is the "always-on agent" policy: at-most-one-live
// (like skip), but a live run that has gone silent past StaleAfter is
// treated as dead — dropped from the live set so the tick relaunches a
// fresh run, and surfaced for reaping. It is what keeps a bot
// continuously alive across short, individually-budgeted runs rather
// than one immortal run.
const (
	OverlapSkip      = "skip"
	OverlapAllow     = "allow"
	OverlapKeepalive = "keepalive"
)

// Guard defaults.
const (
	DefaultGuardTimeout = 30 * time.Second
	DefaultGuardVar     = "guard_output"
	// DefaultStaleAfter is the keepalive silence cutoff: a running run
	// whose last progress is older than this is treated as dead. Chosen
	// generously so a long-but-alive run is not falsely reaped; override
	// per schedule (ideally >= the bot's max_duration).
	DefaultStaleAfter = 5 * time.Minute
)

// Policy is the concurrency + guard contract shared by all three
// surfaces. Zero value is legal (skip, no guard); Normalize promotes it
// to explicit defaults. All fields are additive on their host schemas —
// old manifests / rows load unchanged.
type Policy struct {
	// Overlap is "skip" (default: don't fire while a previous run of
	// this schedule is live) or "allow".
	Overlap string `yaml:"overlap,omitempty" json:"overlap,omitempty" bson:"overlap,omitempty"`
	// MaxConcurrent caps live runs when Overlap is "allow", inclusive
	// of the run about to fire (2 = fire while fewer than 2 are live).
	// 0 with "allow" means unlimited. Invalid with "skip".
	MaxConcurrent int `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty" bson:"max_concurrent,omitempty"`
	// Guard is an optional `sh -lc` snippet run before any launch:
	// exit 0 fires the run and the guard's stdout becomes the run's
	// vars[GuardVar]; non-zero skips the tick.
	Guard string `yaml:"guard,omitempty" json:"guard,omitempty" bson:"guard,omitempty"`
	// GuardTimeout bounds the guard subprocess (Go duration string,
	// default 30s).
	GuardTimeout string `yaml:"guard_timeout,omitempty" json:"guard_timeout,omitempty" bson:"guard_timeout,omitempty"`
	// GuardVar names the workflow var receiving the guard's stdout
	// (default "guard_output").
	GuardVar string `yaml:"guard_var,omitempty" json:"guard_var,omitempty" bson:"guard_var,omitempty"`
	// StaleAfter is the keepalive silence cutoff (Go duration string): a
	// running run whose last progress is older than this counts as dead,
	// so a fresh run relaunches and the zombie is reaped. Only meaningful
	// with Overlap=keepalive; empty normalizes to DefaultStaleAfter.
	StaleAfter string `yaml:"stale_after,omitempty" json:"stale_after,omitempty" bson:"stale_after,omitempty"`
}

// Normalize returns p with defaults applied. Idempotent; never returns
// a Policy with empty Overlap, GuardTimeout or GuardVar.
func Normalize(p Policy) Policy {
	if p.Overlap == "" {
		p.Overlap = OverlapSkip
	}
	if p.GuardTimeout == "" {
		p.GuardTimeout = DefaultGuardTimeout.String()
	}
	if p.GuardVar == "" {
		p.GuardVar = DefaultGuardVar
	}
	if p.Overlap == OverlapKeepalive && p.StaleAfter == "" {
		p.StaleAfter = DefaultStaleAfter.String()
	}
	return p
}

// Validate reports whether p is coherent. Errors are user-facing and
// name the offending field. The guard command itself is not validated
// (any sh -lc snippet is legal).
func Validate(p Policy) error {
	switch p.Overlap {
	case "", OverlapSkip:
		if p.MaxConcurrent != 0 {
			return fmt.Errorf("schedgate: max_concurrent is only valid with overlap=allow")
		}
	case OverlapAllow:
		if p.MaxConcurrent < 0 {
			return fmt.Errorf("schedgate: max_concurrent must be >= 1 when set (0 = unlimited)")
		}
	case OverlapKeepalive:
		if p.MaxConcurrent != 0 {
			return fmt.Errorf("schedgate: max_concurrent is invalid with overlap=keepalive (at-most-one-live is the whole point)")
		}
		if p.StaleAfter != "" {
			d, err := time.ParseDuration(p.StaleAfter)
			if err != nil {
				return fmt.Errorf("schedgate: invalid stale_after %q: %w", p.StaleAfter, err)
			}
			if d <= 0 {
				return fmt.Errorf("schedgate: stale_after must be > 0 (got %q)", p.StaleAfter)
			}
		}
	default:
		return fmt.Errorf("schedgate: invalid overlap %q (want %q, %q or %q)", p.Overlap, OverlapSkip, OverlapAllow, OverlapKeepalive)
	}
	if p.StaleAfter != "" && p.Overlap != OverlapKeepalive {
		return fmt.Errorf("schedgate: stale_after is only valid with overlap=keepalive")
	}
	if p.GuardTimeout != "" {
		if _, err := time.ParseDuration(p.GuardTimeout); err != nil {
			return fmt.Errorf("schedgate: invalid guard_timeout %q: %w", p.GuardTimeout, err)
		}
	}
	if strings.ContainsAny(p.GuardVar, " \t\n") {
		return fmt.Errorf("schedgate: guard_var %q must not contain whitespace", p.GuardVar)
	}
	return nil
}

// GuardTimeoutDuration resolves the policy's guard timeout, falling
// back to the default on empty or unparseable values (Validate rejects
// the latter upstream; the fallback keeps runtime behavior total).
func (p Policy) GuardTimeoutDuration() time.Duration {
	if p.GuardTimeout == "" {
		return DefaultGuardTimeout
	}
	d, err := time.ParseDuration(p.GuardTimeout)
	if err != nil || d <= 0 {
		return DefaultGuardTimeout
	}
	return d
}

// StaleAfterDuration resolves the keepalive silence cutoff, falling back
// to DefaultStaleAfter on empty or unparseable values (Validate rejects
// the latter upstream; the fallback keeps runtime behavior total).
func (p Policy) StaleAfterDuration() time.Duration {
	if p.StaleAfter == "" {
		return DefaultStaleAfter
	}
	d, err := time.ParseDuration(p.StaleAfter)
	if err != nil || d <= 0 {
		return DefaultStaleAfter
	}
	return d
}

// Decision is the outcome of EvaluateOverlap.
type Decision int

const (
	// DecisionFire means the tick may proceed (to the guard, then launch).
	DecisionFire Decision = iota
	// DecisionSkipOverlap means firing would exceed the overlap policy.
	DecisionSkipOverlap
)

// EvaluateOverlap decides whether a tick may fire given the schedule's
// currently-live run IDs. Pure. On skip, the returned blocking ID is
// the first live run (the store lists them created_at-ascending, so
// "first" is the oldest — deterministic across replicas) for the audit
// reason.
func EvaluateOverlap(liveIDs []string, p Policy) (Decision, string) {
	p = Normalize(p)
	live := len(liveIDs)
	if live == 0 {
		return DecisionFire, ""
	}
	switch p.Overlap {
	case OverlapAllow:
		if p.MaxConcurrent == 0 || live < p.MaxConcurrent {
			return DecisionFire, ""
		}
		return DecisionSkipOverlap, liveIDs[0]
	default: // OverlapSkip and OverlapKeepalive: at-most-one-live.
		// For keepalive, staleness has already emptied the live set
		// upstream (LiveAndStaleRunsForSchedule), so any run reaching
		// here is genuinely alive and blocks the tick.
		return DecisionSkipOverlap, liveIDs[0]
	}
}
