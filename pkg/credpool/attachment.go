package credpool

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Pool attachments (ADR-164): the opt-in that makes a named pool serve a
// scope's runs. Zero attachments mean what has always been true — a pool
// answers only its audience walk — so an attachment is never implicit: it
// is written at a scope by someone who manages that scope AND owns (or is
// admitted by) the pool, and detaching simply restores the inheritance.

// DefaultMarginPct is the pre-exhaustion threshold a strategy carries when
// MarginPct is nil: an entry whose freshest usage reading reports this much
// utilization (or a rejected window) with a future reset instant is skipped
// at seal time, and the next entry serves instead. The pointer, not a zero
// default, is what keeps "refuse-only" expressible — a plain float64 with
// omitempty would read 0 as unset and 0 would mean "skip everything".
const DefaultMarginPct = 85.0

// StrategyMode picks how a pool orders its entries for one run.
type StrategyMode string

const (
	// StrategyPriority — the operator's explicit order wins: lower
	// Priority first; equal priorities keep the pool's fairness ranking
	// (forfaits before metered keys at equal priority, then least
	// consumed). The default mode.
	StrategyPriority StrategyMode = "priority"
	// StrategyLeastConsumed — the legacy fairness ranking alone.
	StrategyLeastConsumed StrategyMode = "least_consumed"
)

// Strategy is the fallback policy one attachment configures. It freezes
// onto the run doc with the pool id it belongs to; the entry choice
// re-resolves per attempt.
type Strategy struct {
	// Mode picks the ranking. Zero reads as StrategyPriority.
	Mode StrategyMode `bson:"mode,omitempty" json:"mode,omitempty"`
	// MarginPct is the pre-exhaustion threshold (nil = DefaultMarginPct).
	// An entry whose freshest usage reading reports utilization at or
	// above the margin — or a rejected window — with a future ResetsAt is
	// skipped at seal time and the next entry serves. The skip fires only
	// for shapes that carry proactive signal (OAuth subscriptions read
	// through the usagecap store); every other shape is honest about being
	// reactive (the preview says so) and gates on its provider cooldown
	// alone.
	MarginPct *float64 `bson:"margin_pct,omitempty" json:"margin_pct,omitempty"`
	// Triggers names which fallback triggers the strategy answers,
	// from the llmroute trigger vocabulary (the closed list the routing
	// policy validates against — the platform ceiling applies at the fold,
	// here only the shape is checked). Empty = [usage_window], the
	// exhaustion case the pool exists for. The margin skip fires only when
	// usage_window is among them; the entry rungs carry this list as their
	// On filter, so [unavailable] switches on unavailability without ever
	// pre-empting a warm entry.
	Triggers []string `bson:"triggers,omitempty" json:"triggers,omitempty"`
	// ResetHold keeps a switch held until a manual reset: when the strategy
	// switches AT SEAL TIME (a better-ranked entry of the kind did not
	// serve), the lease it opens on the next entry is marked Held, and a
	// resume re-acquires THAT entry instead of re-ranking — until the
	// operator lifts the hold through the entry reset route. A switch made
	// IN-RUN by the entry rungs does not hold: the in-run chain restarts
	// from its top on every attempt. Off (the default) lets the periodic
	// reset reopen the previous order on its own.
	ResetHold bool `bson:"reset_hold,omitempty" json:"reset_hold,omitempty"`
}

// Equal reports whether two strategies say the same thing. The triggers
// compare as a set — their order means nothing to the fold or the rungs.
// A dial left absent and the same value written are different answers:
// the absent one follows its default (a zero mode, a nil margin, an empty
// trigger list), the written one pins it, so mode "" and "priority", a nil
// margin and an explicit 85, or [] and [usage_window] are NOT equal.
func (s Strategy) Equal(o Strategy) bool {
	if s.Mode != o.Mode || s.ResetHold != o.ResetHold || !sameStringSet(s.Triggers, o.Triggers) {
		return false
	}
	if s.MarginPct == nil || o.MarginPct == nil {
		return s.MarginPct == nil && o.MarginPct == nil
	}
	return *s.MarginPct == *o.MarginPct
}

// sameStringSet reports whether two lists hold the same strings, in any
// order (Validate refuses a duplicate trigger).
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := slices.Clone(a), slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(as, bs)
}

// Margin returns the effective threshold.
func (s Strategy) Margin() float64 {
	if s.MarginPct == nil {
		return DefaultMarginPct
	}
	return *s.MarginPct
}

// WantUsageWindow reports whether the margin skip may fire: the
// pre-exhaustion skip IS the usage_window trigger, and a strategy that
// never listed it never skips a warm entry.
func (s Strategy) WantUsageWindow() bool {
	return s.hasTrigger("usage_window")
}

// hasTrigger reports whether the trigger list names t; an empty list means
// the default [usage_window].
func (s Strategy) hasTrigger(t string) bool {
	if len(s.Triggers) == 0 {
		return t == "usage_window"
	}
	for _, v := range s.Triggers {
		if v == t {
			return true
		}
	}
	return false
}

// Validate checks the strategy's shape. The trigger VALUES are validated
// against the llmroute vocabulary at the fold (the platform ceiling lives
// there too); this package deliberately does not know them.
func (s Strategy) Validate() error {
	switch s.Mode {
	case "", StrategyPriority, StrategyLeastConsumed:
	default:
		return fmt.Errorf("credpool: unknown strategy mode %q", s.Mode)
	}
	if s.MarginPct != nil && (*s.MarginPct <= 0 || *s.MarginPct > 100) {
		return fmt.Errorf("credpool: margin_pct must be in (0, 100], got %v", *s.MarginPct)
	}
	seen := map[string]bool{}
	for _, t := range s.Triggers {
		if strings.TrimSpace(t) == "" || t != strings.TrimSpace(t) {
			return fmt.Errorf("credpool: a trigger name is blank or padded: %q", t)
		}
		if seen[t] {
			return fmt.Errorf("credpool: trigger %q listed twice", t)
		}
		seen[t] = true
	}
	return nil
}

// DrawLimits are the borrower-side draw caps an attachment sets on a scope
// — how much of the pool that scope may consume. Two fields, two duties:
// the donor's own Limits cap SUPPLY on the pledge row; these cap DRAW on
// the attachment row. Both are enforced inside the ledger's atomic
// reserve, on two separate ledger rows.
type DrawLimits struct {
	MaxRunsPerDay int     `bson:"max_runs_per_day,omitempty" json:"max_runs_per_day,omitempty"`
	MaxUSDPerDay  float64 `bson:"max_usd_per_day,omitempty" json:"max_usd_per_day,omitempty"`
}

// AsLimits reads the draw caps as the ledger's Limits: the two fields the
// attachment carries are exactly the two the atomic reserve judges on the
// att: row (concurrency stays the donor's own dial, on their row).
func (d DrawLimits) AsLimits() Limits {
	return Limits{MaxRunsPerDay: d.MaxRunsPerDay, MaxUSDPerDay: d.MaxUSDPerDay}
}

// Validate refuses negative caps. A cap-less attachment is allowed (and the
// route warns at save — the C-less equivalent of the price feed's loud
// posture) because an org pool that trusts its scope sets no numbers.
func (d DrawLimits) Validate() error {
	if d.MaxRunsPerDay < 0 || d.MaxUSDPerDay < 0 {
		return fmt.Errorf("credpool: draw limits cannot be negative")
	}
	return nil
}

// Attachment is one scope's standing answer to "which pool serves this
// scope's runs, and how". Stored one per scope; the fold picks the nearest
// one. Enabled=false is a resolved-disabled answer: it STOPS the inheritance
// at that level (nearest-wins means nearest also blocks) — detaching is the
// delete.
type Attachment struct {
	ScopeID string `bson:"_id" json:"scope_id"`
	PoolID  string `bson:"pool_id" json:"pool_id"`
	// Enabled=false pauses the fallback policy without deleting its
	// configuration, and blocks the parent scope's pool from serving this
	// scope: the fold resolves a disabled attachment as "no pool here, do
	// not inherit".
	Enabled   bool       `bson:"enabled" json:"enabled"`
	Strategy  Strategy   `bson:"strategy" json:"strategy"`
	Limits    DrawLimits `bson:"limits" json:"limits"`
	CreatedAt time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time  `bson:"updated_at" json:"updated_at"`
}

// Scope ids. The shapes are the price feed's composite-id precedent; the
// binding level does not live in this collection — it rides the
// provisioning records (schedule row / trigger subscription / webhook
// config) as a pool: block, validated at record-write time.
const (
	// ScopePlatform is the instance-level attachment: the deployment's
	// default pool, the fold's floor.
	ScopePlatform = "platform"
)

// OrgScopeID, TeamScopeID and BotScopeID build the composite ids.
func OrgScopeID(orgID string) string       { return "org:" + orgID }
func TeamScopeID(teamID string) string     { return "team:" + teamID }
func BotScopeID(teamID, bot string) string { return "bot:" + teamID + "/" + bot }

// AttachmentLedgerID is the ledger pseudo-pledge id one attachment's draw
// caps book against. The "att:" prefix cannot collide with a real pledge id
// (PledgeID joins user|source|ref with the same separator, and a user id is
// never allowed to start with it) — ValidateDrawLedgerID is the guard the
// caller runs when the namespace meets a real pledge id.
func AttachmentLedgerID(scopeID string) string { return "att:" + scopeID }

// IsAttachmentLedgerID reports whether an id is an attachment ledger row,
// never a real pledge's.
func IsAttachmentLedgerID(id string) bool { return strings.HasPrefix(id, "att:") }

// ValidateScopeID checks a composite attachment id's shape. Note the
// deliberate split: the attachment scope's platform doc id is the bare
// word "platform" (the price feed's doc-id shape), while the LEND scope
// sentinel is secrets' "platform:" tenant — two namespaces, never mixed.
func ValidateScopeID(s string) error {
	if s == ScopePlatform {
		return nil
	}
	for _, prefix := range []string{"org:", "team:"} {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			if rest == "" {
				return fmt.Errorf("credpool: scope %q names no id", s)
			}
			return nil
		}
	}
	if team, rest, ok := strings.Cut(strings.TrimPrefix(s, "bot:"), "/"); strings.HasPrefix(s, "bot:") {
		if !ok || team == "" || rest == "" {
			return fmt.Errorf("credpool: bot scope %q must be bot:<teamID>/<botID>", s)
		}
		return nil
	}
	return fmt.Errorf("credpool: unknown attachment scope %q", s)
}

// Validate checks an attachment before it is stored.
func (a Attachment) Validate() error {
	if err := ValidateScopeID(a.ScopeID); err != nil {
		return err
	}
	if strings.TrimSpace(a.PoolID) == "" {
		return fmt.Errorf("credpool: attachment names no pool")
	}
	if err := a.Strategy.Validate(); err != nil {
		return err
	}
	return a.Limits.Validate()
}

// Binding is the pool: block a provisioning record carries — a schedule
// row, a forge integration and the webhook config it provisions (ADR-164):
// the named pool, an on/off (off = resolved-disabled, the fold stops here)
// and the fallback strategy. It is the machine lane's consent: the record
// is written by a team admin through a validated write surface, and
// writing the block is what grants the record's launches the named pool.
// ONE type for every record, so the shape check and the projection cannot
// drift between them.
type Binding struct {
	PoolID   string   `bson:"pool_id" json:"pool_id"`
	Enabled  bool     `bson:"enabled" json:"enabled"`
	Strategy Strategy `bson:"strategy" json:"strategy"`
}

// Validate checks the block's shape; the POOL's existence and the consent
// are the write surface's job (it owns the stores and the caller's
// identity).
func (b Binding) Validate() error {
	if strings.TrimSpace(b.PoolID) == "" {
		return fmt.Errorf("pool block names no pool")
	}
	return b.Strategy.Validate()
}

// Layer projects the block as the binding layer the launch fold consumes
// (route_resolve.go's `higher`), naming the record kind. ok=false when the
// record binds no pool — the fold then reads the stored scopes alone.
func (b *Binding) Layer(source string) (BindingLayer, bool) {
	if b == nil {
		return BindingLayer{}, false
	}
	return BindingLayer{Source: source, Enabled: b.Enabled, PoolID: b.PoolID, Strategy: b.Strategy}, true
}

// Equal reports whether two blocks say the same thing — nil equals nil
// only (an absent block and a present one never match).
func (b *Binding) Equal(o *Binding) bool {
	if b == nil || o == nil {
		return b == nil && o == nil
	}
	return b.PoolID == o.PoolID && b.Enabled == o.Enabled && b.Strategy.Equal(o.Strategy)
}

// BindingLayer is one binding-level answer the launch carries IN — the
// provisioning record's pool: block (schedule row, trigger subscription,
// webhook config), the routing `higher` layers' twin (route_resolve.go).
// The fold reads these BEFORE the stored scopes: the binding level is the
// most specific one. The block is validated at record-write time (slice:
// routes); at launch an unreadable block REFUSES — the fold never folds
// around garbage.
type BindingLayer struct {
	// Source names the record kind for the refusal and the provenance
	// ("schedule", "webhook", "trigger").
	Source string
	// Enabled=false is a resolved-disabled answer at the binding level:
	// it stops the inheritance exactly like a stored attachment's.
	Enabled  bool
	PoolID   string
	Strategy Strategy
}

// StatusNearLimit extends the display vocabulary: the entry carries
// proactive signal and its freshest reading sits at or past the strategy
// margin with a future reset — skipped at seal time, back after the reset,
// without any stored state (derived, like cooling).
const StatusNearLimit Status = "near_limit"
