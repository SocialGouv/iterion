package knowledge

// Default quota ceilings, in bytes. The org aggregate is the
// authoritative hard cap (the sum of all of an org's spaces); the
// per-visibility values are finer-grained sub-limits beneath it, so
// one runaway space cannot consume the whole org budget. All are
// overridable per-org (admin) and via environment at process start.
const (
	// DefaultOrgAggregateQuota is the per-org ceiling across every
	// space the org owns. Env override: ITERION_MEMORY_QUOTA_ORG_TOTAL.
	DefaultOrgAggregateQuota int64 = 1 << 30 // 1 GiB

	// Per-visibility space sub-caps.
	DefaultQuotaCrossProject int64 = 512 << 20 // 512 MiB
	DefaultQuotaOrgSpace     int64 = 1 << 30   // 1 GiB (a single org-wide space may use the whole budget)
	DefaultQuotaProject      int64 = 256 << 20 // 256 MiB
	DefaultQuotaBot          int64 = 256 << 20 // 256 MiB
	DefaultQuotaUser         int64 = 128 << 20 // 128 MiB
	DefaultQuotaPrivate      int64 = 64 << 20  // 64 MiB

	// DefaultMaxDocumentSize caps a single markdown document.
	DefaultMaxDocumentSize int64 = 2 << 20 // 2 MiB
)

// DefaultQuotaFor returns the default per-space sub-cap for a
// visibility. Zero means "no sub-cap, never deny" — the value both adapters
// act on (the FS one skips the per-space ceiling and charges the aggregate;
// the cloud one passes allowZeroQuota to its counter CAS) — not "not
// writable". Only VisibilityGlobal and an unknown visibility take it:
// the instance-wide catalogue is bounded by its own aggregate
// (InstanceUsageID), not by a per-space slice of one org's budget.
func DefaultQuotaFor(v Visibility) int64 {
	switch v {
	case VisibilityCrossProject:
		return DefaultQuotaCrossProject
	case VisibilityOrg:
		return DefaultQuotaOrgSpace
	case VisibilityProject:
		return DefaultQuotaProject
	case VisibilityBot:
		return DefaultQuotaBot
	case VisibilityUser:
		return DefaultQuotaUser
	case VisibilityPrivate:
		return DefaultQuotaPrivate
	default:
		return 0
	}
}
