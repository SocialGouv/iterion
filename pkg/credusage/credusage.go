// Package credusage meters per-CREDENTIAL monthly LLM spend — the question
// the org bucket cannot answer.
//
// pkg/orgusage charges a run's totals to the org that launched it, whatever
// tier served it, so it answers "how much did this org consume" and never
// "what did this key cost". Asked in production on 2026-09-03, about one
// shared subscription, and unanswerable: the org counter mixed a team
// forfait, the platform fallback and a lent pool key into one number. Only
// the pool tier had per-credential accounting (the donor's ledger), which is
// the precedent this generalises.
//
// Two properties carry the whole design.
//
// **Split by backend.** One run can consume two credentials — a claude_code
// forfait for the implementer and a platform codex key for the plan review —
// while RunTotals() is a single number. A counter fed from that total would
// charge one credential for the other's calls, which is exactly the
// misattribution it exists to remove.
//
// **Nature, in the API and not only in the docs.** On a subscription the
// amount is NOT money: claude_code prints total_cost_usd on every call
// whatever pays for it, and on a forfait that figure is the price those calls
// WOULD have cost metered (a three-token "pong" reported $0.0402, measured
// 2026-09-03). A metered key's figure is a real charge on a real invoice.
// The two are not comparable and must never be summed as if they were, so
// every record carries its Nature and every response field does too. It is
// the same line credpool.CredentialSource.Metered() draws for a lent
// credential — kept as a value here so this package stays a leaf.
package credusage

import (
	"context"
	"math"
	"strings"
	"time"
)

// Nature says what an amount MEANS. Never a display detail: two amounts of
// different nature cannot be added.
type Nature string

const (
	// NatureMetered is real money, charged per token on someone's invoice
	// (a BYOK or lent API key).
	NatureMetered Nature = "metered"
	// NatureEstimate is what the calls WOULD have cost metered, on a
	// subscription that bills nothing per call (an OAuth forfait). The
	// provider's usage window is the real ceiling, not this figure.
	NatureEstimate Nature = "estimate"
)

func (n Nature) Valid() bool { return n == NatureMetered || n == NatureEstimate }

// Tier names which resolution tier supplied the credential, so an operator
// reading a number knows whose it is.
type Tier string

const (
	// TierTeam is a credential the run's own tenant resolved (BYOK key or
	// team/user forfait).
	TierTeam Tier = "team"
	// TierPool is a contributor's credential lent through the mutualised
	// pool: the spend is the DONOR's, not the borrower's.
	TierPool Tier = "pool"
	// TierPlatform is the deployment's own DB-backed fallback.
	TierPlatform Tier = "platform"
)

// Key identifies one credential's meter for one tenant.
//
// The fingerprint is the credential's audit identity, and the tenant is kept
// beside it deliberately: a platform or pool credential serves several
// tenants, and "what did this key cost" and "what did this key cost US" are
// both real questions. Summing across tenants answers the first.
type Key struct {
	// Fingerprint is the credential's audit identity
	// (secrets.FingerprintSHA256 for a key, the connect-time stamp for a
	// forfait). Required.
	Fingerprint string
	// Provider is the credential slot — a provider name ("anthropic") or an
	// OAuth kind ("claude_code").
	Provider string
	// Tier is which resolution tier supplied it.
	Tier Tier
	// TenantID is the team the run belonged to. Empty for a run with no
	// tenant (local/CLI), which still meters.
	TenantID string
}

// Valid reports whether the key names something meterable. A credential with
// no fingerprint names a SLOT, not an account — counting it would merge every
// unstamped credential of that provider into one bucket.
func (k Key) Valid() bool {
	return strings.TrimSpace(k.Fingerprint) != "" && strings.TrimSpace(k.Provider) != ""
}

// MonthlyUsage is the read view of one credential-month.
type MonthlyUsage struct {
	// Month is the UTC bucket key, e.g. "2026-09".
	Month string `json:"month"`
	// Fingerprint / Provider / Tier / TenantID echo the key.
	Fingerprint string `json:"fingerprint"`
	Provider    string `json:"provider"`
	Tier        Tier   `json:"tier"`
	TenantID    string `json:"tenant_id,omitempty"`
	// Nature qualifies CostUSD. Read it before comparing two rows: an
	// estimate is not an invoice.
	Nature Nature `json:"nature"`
	// CostUSD is the accumulated figure — real money when Nature is
	// metered, the metered-equivalent price when it is estimate.
	CostUSD      float64 `json:"cost_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	// Runs counts the attempts that spent anything on this credential.
	Runs int `json:"runs"`
	// Backends lists the backends that drew on it this month, so a mixed
	// figure can be read apart.
	Backends []string `json:"backends,omitempty"`
}

// Spend is one attempt's consumption of one credential through one backend.
type Spend struct {
	Key
	// Nature qualifies CostUSD (see the package doc). Required.
	Nature Nature
	// Backend is the delegate that spent it ("claude_code", "claw", …).
	Backend      string
	CostUSD      float64
	InputTokens  int64
	OutputTokens int64
}

// Counter is the per-credential monthly metering surface, mirroring
// orgusage.Counter. Implementations: MongoCounter (production, atomic $inc)
// and MemoryCounter (tests/local). Keep semantics in lock-step.
//
// There is deliberately no Allow/gate here: enforcement is the run's own
// budget and the provider's usage window. This counts.
type Counter interface {
	// AddSpend accumulates one attempt's consumption. A spend with an
	// invalid key or a zero amount is a no-op, never an error: metering
	// must not turn a finished run into a failed one.
	AddSpend(ctx context.Context, when time.Time, s Spend) error
	// Usage returns one credential-month, zero-valued (with Month and the
	// key echoed) when nothing was recorded.
	Usage(ctx context.Context, when time.Time, k Key) (MonthlyUsage, error)
	// List returns every credential-month recorded for a tenant, newest
	// spend first. An empty tenant lists the runs that had none.
	List(ctx context.Context, when time.Time, tenantID string) ([]MonthlyUsage, error)
	// ListByFingerprint returns one credential's month ACROSS tenants —
	// what a platform or lent credential actually cost, summed by the
	// caller. Empty fingerprint returns nothing.
	ListByFingerprint(ctx context.Context, when time.Time, fingerprint string) ([]MonthlyUsage, error)
	// ListByTier returns every credential-month of one tier, across
	// tenants. The platform tier's rows live under the TENANTS it served
	// (a run is metered where it ran), so "what did the deployment's own
	// credentials cost this month" cannot be asked by tenant.
	ListByTier(ctx context.Context, when time.Time, tier Tier) ([]MonthlyUsage, error)
}

// RetentionDays bounds how long credential-month documents are retained
// (Mongo TTL) — the same 400 days as orgusage, so a full billing year plus
// margin survives.
const RetentionDays = 400

// monthKey buckets a timestamp into its UTC month.
func monthKey(when time.Time) string { return when.UTC().Format("2006-01") }

// monthStart returns the first instant of the timestamp's UTC month —
// stored on each document so the TTL index can evict old months.
func monthStart(when time.Time) time.Time {
	u := when.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// docID is the document id for one (credential, tier, tenant, month).
//
// The tier is part of the identity, not a label: the same key lent through
// the pool and used directly by its owner is two different economic facts,
// and merging them would report the donor's loan as the borrower's spend.
func docID(k Key, when time.Time) string {
	return "cred|" + k.Fingerprint + "|" + k.Provider + "|" + string(k.Tier) + "|" + k.TenantID + "|" + monthKey(when)
}

// CostToMillis converts a USD amount to integer thousandths so the Mongo
// $inc stays integral (a float $inc accumulates drift) — the same unit
// orgusage counts in.
func CostToMillis(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	return int64(math.Round(usd * 1000))
}

// millisToCost converts back for the read view.
func millisToCost(m int64) float64 { return float64(m) / 1000 }

// empty reports whether a spend carries nothing worth recording.
func (s Spend) empty() bool {
	return s.CostUSD <= 0 && s.InputTokens <= 0 && s.OutputTokens <= 0
}

// recordable reports whether a spend can be counted at all.
func (s Spend) recordable() bool {
	return s.Key.Valid() && s.Nature.Valid() && !s.empty()
}
