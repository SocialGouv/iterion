// Package retrypolicy is the shared "should this failed run be retried,
// and when?" contract — the after-the-failure counterpart to pkg/schedgate's
// before-the-launch gate. Like schedgate it is pure: no I/O, no store, no
// clock of its own. Each surface keeps its own persistence (a schedule row
// in Mongo, a schedules.yaml entry, a bot manifest) and projects onto the
// same Policy.
//
// It exists because a provider usage window (the Anthropic forfait 5h /
// session / daily / weekly caps) cannot clear inside a node's retry budget
// or a message-queue redelivery window: a weekly reset is up to seven days
// out. Blind redelivery against that wall just burns pods — on 2026-07-27,
// seven scheduled prod runs each reprovisioned eight pods against a reset
// ~35h away before parking in the DLQ. The cure is to wait for the reset,
// which means the wait must be durable and its shape must be configurable
// by whoever owns the schedule, the bot, or the platform.
package retrypolicy

import (
	"fmt"
	"strings"
	"time"
)

// UsageWindow policy values. Empty normalizes to UsageWindowResume: a run
// that died only because the provider's quota window was exhausted is the
// clearest possible case for an automatic retry — nothing about the run is
// wrong, and the operator would otherwise re-launch it by hand. Off is the
// explicit opt-out for a run whose output is worthless late (a "what
// changed in the last hour" digest, say).
const (
	UsageWindowResume = "resume"
	UsageWindowOff    = "off"
)

// Defaults.
const (
	// DefaultMaxAttempts bounds how many times a single run may be retried
	// across its whole lifetime. The counter is never reset, so this is a
	// hard anti-loop ceiling, not a per-window allowance.
	DefaultMaxAttempts = 5
	// DefaultMaxWait caps how far ahead a retry may be scheduled. The
	// longest real window is weekly, so eight days covers it with room for
	// timezone skew in the provider's reset notice while still refusing an
	// absurd "retry in three months" from a misparsed hint.
	DefaultMaxWait = 8 * 24 * time.Hour
	// DefaultJitter spreads retries that share one reset instant. Several
	// schedules commonly die on the same window (five feed-watch digests
	// did), and resuming them simultaneously at the reset can exhaust the
	// fresh window immediately.
	DefaultJitter = 10 * time.Minute
)

// Policy is the retry contract shared by every surface that can own one.
// Zero value is legal (resume with the defaults); Normalize promotes it to
// explicit values. All fields are additive on their host schemas — old
// manifests and old rows load unchanged.
//
// Fields are named by FAILURE CLASS rather than as one boolean so a future
// class (network_transient, tool_failed_transient) becomes a sibling field
// instead of a schema break.
type Policy struct {
	// UsageWindow selects what happens when a run fails because the
	// provider's quota window is exhausted: "resume" (default) waits for
	// the reset and resumes, "off" leaves the run failed_resumable for the
	// operator.
	UsageWindow string `yaml:"usage_window,omitempty" json:"usage_window,omitempty" bson:"usage_window,omitempty"`
	// MaxAttempts bounds retries over the run's whole lifetime. 0 means
	// unset (inherit / default); use UsageWindow "off" to disable retries
	// rather than a zero count, so "disabled" is never ambiguous with
	// "not configured here".
	MaxAttempts int `yaml:"max_attempts,omitempty" json:"max_attempts,omitempty" bson:"max_attempts,omitempty"`
	// MaxWait caps how far ahead a retry may be scheduled (Go duration
	// string, default 8d). A reset further out than this is clamped, not
	// refused: waiting the cap and re-checking beats dropping the run.
	MaxWait string `yaml:"max_wait,omitempty" json:"max_wait,omitempty" bson:"max_wait,omitempty"`
	// Jitter is the maximum random delay added to a computed retry instant
	// (Go duration string, default 10m) so runs sharing a reset do not all
	// resume at once. "0s" disables it.
	Jitter string `yaml:"jitter,omitempty" json:"jitter,omitempty" bson:"jitter,omitempty"`
}

// Normalize returns p with defaults applied. Idempotent; never returns a
// Policy with an empty UsageWindow, MaxWait or Jitter.
func Normalize(p Policy) Policy {
	if p.UsageWindow == "" {
		p.UsageWindow = UsageWindowResume
	}
	if p.MaxAttempts == 0 {
		p.MaxAttempts = DefaultMaxAttempts
	}
	if p.MaxWait == "" {
		p.MaxWait = DefaultMaxWait.String()
	}
	if p.Jitter == "" {
		p.Jitter = DefaultJitter.String()
	}
	return p
}

// Validate reports whether p is coherent. Errors are user-facing and name
// the offending field.
func Validate(p Policy) error {
	switch p.UsageWindow {
	case "", UsageWindowResume, UsageWindowOff:
	default:
		return fmt.Errorf("retrypolicy: invalid usage_window %q (want %q or %q)", p.UsageWindow, UsageWindowResume, UsageWindowOff)
	}
	if p.MaxAttempts < 0 {
		return fmt.Errorf("retrypolicy: max_attempts must be >= 1 when set (got %d; use usage_window=%s to disable retries)", p.MaxAttempts, UsageWindowOff)
	}
	if err := validatePositiveDuration("max_wait", p.MaxWait); err != nil {
		return err
	}
	// Jitter alone may legitimately be zero ("0s" = never spread).
	if s := strings.TrimSpace(p.Jitter); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("retrypolicy: invalid jitter %q: %w", p.Jitter, err)
		}
		if d < 0 {
			return fmt.Errorf("retrypolicy: jitter must be >= 0 (got %q)", p.Jitter)
		}
	}
	return nil
}

func validatePositiveDuration(field, raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("retrypolicy: invalid %s %q: %w", field, raw, err)
	}
	if d <= 0 {
		return fmt.Errorf("retrypolicy: %s must be > 0 (got %q)", field, raw)
	}
	return nil
}

// Enabled reports whether a usage-window failure should be retried.
func (p Policy) Enabled() bool {
	p = Normalize(p)
	return p.UsageWindow == UsageWindowResume && p.MaxAttempts > 0
}

// MaxWaitDuration resolves the retry-horizon cap, falling back to the
// default on empty or unparseable values (Validate rejects the latter
// upstream; the fallback keeps runtime behavior total).
func (p Policy) MaxWaitDuration() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(p.MaxWait))
	if err != nil || d <= 0 {
		return DefaultMaxWait
	}
	return d
}

// JitterDuration resolves the retry spread. Unlike MaxWait, an explicit
// zero is meaningful (no spread) and is preserved; only an empty or
// unparseable value falls back to the default.
func (p Policy) JitterDuration() time.Duration {
	s := strings.TrimSpace(p.Jitter)
	if s == "" {
		return DefaultJitter
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return DefaultJitter
	}
	return d
}

// Layer is one contributor to a resolved Policy, tagged with a label used
// for provenance reporting.
type Layer struct {
	// Source names the layer for provenance ("run_override", "schedule",
	// "bot", "env", "default").
	Source string
	Policy Policy
}

// Provenance labels for the layers Resolve is normally called with.
const (
	SourceRunOverride = "run_override"
	SourceSchedule    = "schedule"
	SourceWebhook     = "webhook"
	SourceTrigger     = "trigger"
	SourceBot         = "bot"
	SourceEnv         = "env"
	SourceDefault     = "default"
	SourceCeiling     = "platform_ceiling"
)

// Resolve overlays layers FIELD BY FIELD, highest priority first, and
// reports which layer won each field.
//
// Per-field (rather than whole-struct) overlay is what makes the chain
// usable: a schedule that only pins max_wait must not silently discard the
// bot's usage_window choice. It mirrors how the permission gate resolves
// its mode with first-non-empty-wins while merging its rule lists.
//
// The returned map is keyed by the wire field name ("usage_window",
// "max_attempts", "max_wait", "jitter") so an API can show an operator why
// an effective value is what it is — a four-layer policy is undebuggable
// otherwise.
func Resolve(layers ...Layer) (Policy, map[string]string) {
	var out Policy
	src := make(map[string]string, 4)
	for _, l := range layers {
		if out.UsageWindow == "" && strings.TrimSpace(l.Policy.UsageWindow) != "" {
			out.UsageWindow = strings.TrimSpace(l.Policy.UsageWindow)
			src["usage_window"] = l.Source
		}
		if out.MaxAttempts == 0 && l.Policy.MaxAttempts != 0 {
			out.MaxAttempts = l.Policy.MaxAttempts
			src["max_attempts"] = l.Source
		}
		if out.MaxWait == "" && strings.TrimSpace(l.Policy.MaxWait) != "" {
			out.MaxWait = strings.TrimSpace(l.Policy.MaxWait)
			src["max_wait"] = l.Source
		}
		if out.Jitter == "" && strings.TrimSpace(l.Policy.Jitter) != "" {
			out.Jitter = strings.TrimSpace(l.Policy.Jitter)
			src["jitter"] = l.Source
		}
	}
	out = Normalize(out)
	for _, f := range []string{"usage_window", "max_attempts", "max_wait", "jitter"} {
		if _, ok := src[f]; !ok {
			src[f] = SourceDefault
		}
	}
	return out, src
}

// Ceiling is a platform-imposed bound that can only LOWER a resolved
// policy, never raise it. It is applied AFTER Resolve so a tenant cannot
// reserve a hundred attempts over thirty days by declaring them on their
// own schedule — the same ordering the cloud budget ceiling uses (tenant
// override first, platform clamp second).
type Ceiling struct {
	MaxAttempts int           // 0 = unbounded
	MaxWait     time.Duration // 0 = unbounded
}

// IsZero reports whether the ceiling constrains nothing.
func (c Ceiling) IsZero() bool { return c.MaxAttempts <= 0 && c.MaxWait <= 0 }

// Clamp lowers p to fit c, recording any field it changed in src (which may
// be nil). A ceiling never enables a retry that the policy disabled, and
// never raises a value the policy set lower.
func Clamp(p Policy, c Ceiling, src map[string]string) Policy {
	p = Normalize(p)
	if c.MaxAttempts > 0 && p.MaxAttempts > c.MaxAttempts {
		p.MaxAttempts = c.MaxAttempts
		if src != nil {
			src["max_attempts"] = SourceCeiling
		}
	}
	if c.MaxWait > 0 && p.MaxWaitDuration() > c.MaxWait {
		p.MaxWait = c.MaxWait.String()
		if src != nil {
			src["max_wait"] = SourceCeiling
		}
	}
	return p
}
