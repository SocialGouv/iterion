- **Per-team aggregate** (the code's "org aggregate"):
  `DefaultOrgAggregateQuota = 1 GiB` per team. `PATCH /api/admin/orgs/{id}`
  with `memory_quota_bytes` writes, best effort, the org's ceiling onto the counter of
  each team the org has at that moment through the cloud Mongo memory store's `SetTenantQuota`
  capability
  ([pkg/server/admin_orgs_routes.go:tenantMemoryQuotaSetter](../pkg/server/admin_orgs_routes.go)),
  so a team created later starts at the 1 GiB default, and the field on the
  org alone is not enough.